use std::io::Write;
use std::process::{Child, Command, Stdio};
use std::sync::{Arc, Mutex};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, ComputerId, EntityId, ProfileId, Revision, StableKey, UtcTimestamp,
};
use keith_computer::{
    ComputerError, ComputerFrame, ComputerFrameEncoding, ComputerInputCommand,
    ComputerInputPayload, ComputerRecord, ComputerRepository, ComputerRepositoryBatch,
    ComputerState, ComputerStreamAuthorization, ComputerStreamController, ComputerStreamError,
    ComputerStreamLimits, ComputerStreamOpenRequest, ComputerStreamOrigin, ComputerStreamSession,
    ComputerStreamSubject, ControlState, FocusedSecretWriter, InMemoryComputerRepository,
    RefreshedComputerObservation, SecretInjectionAuthority, SecretInjectionAuthorityResolver,
    SecretInjectionRequest, SecretInjectionTarget, SecureSecretInjection, TakeoverAcquireRequest,
    TakeoverBoundaryError, TakeoverHandbackRequest, TakeoverLeaseService, TakeoverPauseBoundary,
    TakeoverRenewRequest, TakeoverResolutionBoundary, TakeoverServiceError, TakeoverTaskBoundary,
};
use keith_credentials::{
    CredentialOwner, CredentialRef, EncryptedCredentialStore, MasterKey, SecretValue,
};

fn timestamp(value: i64) -> UtcTimestamp {
    UtcTimestamp::from_unix_millis(value)
}

fn task_key(value: &str) -> StableKey {
    StableKey::parse(value).expect("test task key must be canonical")
}

fn origin(generation: u64) -> ComputerStreamOrigin {
    ComputerStreamOrigin {
        server_instance_id: EntityId::new(),
        stream_instance_id: EntityId::new(),
        authority_key: task_key(&format!("stream/origin/{generation}")),
        generation,
    }
}

fn agent_controller(profile_id: &ProfileId, fence: u64) -> ComputerStreamController {
    ComputerStreamController::Agent {
        profile_id: profile_id.clone(),
        task_key: task_key("task/stream"),
        fencing_token: fence,
    }
}

#[test]
fn stream_rejects_cross_profile_subject_origin_replay_and_stale_liveness() {
    let profile_id = ProfileId::new();
    let subject = ComputerStreamSubject {
        profile_id: profile_id.clone(),
        computer_id: ComputerId::new(),
    };
    let authorized_origin = origin(7);
    let authorization = ComputerStreamAuthorization::issue(
        subject.clone(),
        Revision::new(4),
        authorized_origin.clone(),
        timestamp(10),
        timestamp(1_000),
        agent_controller(&profile_id, 4),
    )
    .expect("matching stream authority must be issued");
    let forged_request = ComputerStreamOpenRequest {
        subject: ComputerStreamSubject {
            profile_id: ProfileId::new(),
            computer_id: subject.computer_id.clone(),
        },
        resume: None,
    };
    assert_eq!(
        ComputerStreamSession::open(
            EntityId::new(),
            forged_request,
            authorization.clone(),
            ComputerStreamLimits::STRICT,
            timestamp(20),
            timestamp(100),
        ),
        Err(ComputerStreamError::UnauthorizedSubject),
    );

    let mut session = ComputerStreamSession::open(
        EntityId::new(),
        ComputerStreamOpenRequest {
            subject: subject.clone(),
            resume: None,
        },
        authorization,
        ComputerStreamLimits::STRICT,
        timestamp(20),
        timestamp(100),
    )
    .expect("authorized stream must open");
    let descriptor = session.descriptor();
    let forged_frame = ComputerFrame {
        session_id: descriptor.session_id.clone(),
        subject: ComputerStreamSubject {
            profile_id: ProfileId::new(),
            computer_id: subject.computer_id.clone(),
        },
        origin: authorized_origin.clone(),
        sequence: 1,
        captured_at: timestamp(30),
        width: 1,
        height: 1,
        encoding: ComputerFrameEncoding::Png,
        key_frame: true,
        bytes: Vec::new(),
    };
    assert_eq!(
        session.accept_frame(&forged_frame, timestamp(30), timestamp(120)),
        Err(ComputerStreamError::ForgedSubject),
    );
    let input = ComputerInputCommand {
        session_id: descriptor.session_id,
        subject,
        origin: authorized_origin,
        sequence: 0,
        expected_computer_revision: Revision::new(4),
        controller: agent_controller(&profile_id, 4),
        payload: ComputerInputPayload::Focus,
    };
    assert_eq!(
        session.authorize_input(input, timestamp(101), timestamp(200)),
        Err(ComputerStreamError::Expired),
    );
}

#[test]
fn stream_restart_requires_a_new_generation_and_never_accepts_an_old_cursor() {
    let profile_id = ProfileId::new();
    let subject = ComputerStreamSubject {
        profile_id: profile_id.clone(),
        computer_id: ComputerId::new(),
    };
    let initial_origin = origin(2);
    let authorization = ComputerStreamAuthorization::issue(
        subject.clone(),
        Revision::new(3),
        initial_origin.clone(),
        timestamp(1),
        timestamp(1_000),
        agent_controller(&profile_id, 3),
    )
    .unwrap();
    let mut session = ComputerStreamSession::open(
        EntityId::new(),
        ComputerStreamOpenRequest {
            subject: subject.clone(),
            resume: None,
        },
        authorization,
        ComputerStreamLimits::STRICT,
        timestamp(2),
        timestamp(100),
    )
    .unwrap();
    let unchanged = ComputerStreamAuthorization::issue(
        subject.clone(),
        Revision::new(3),
        initial_origin,
        timestamp(3),
        timestamp(1_000),
        agent_controller(&profile_id, 3),
    )
    .unwrap();
    assert_eq!(
        session.reconcile_origin(unchanged, timestamp(3), timestamp(100)),
        Err(ComputerStreamError::GenerationChanged),
    );
    let replacement = origin(3);
    let replacement_authority = ComputerStreamAuthorization::issue(
        subject,
        Revision::new(3),
        replacement.clone(),
        timestamp(4),
        timestamp(1_000),
        agent_controller(&profile_id, 3),
    )
    .unwrap();
    let descriptor = session
        .reconcile_origin(replacement_authority, timestamp(4), timestamp(120))
        .expect("server restart must install a strictly newer stream generation");
    assert_eq!(descriptor.cursor.generation, 3);
    assert_eq!(descriptor.cursor.sequence, 0);
    let stale = ComputerStreamAuthorization::issue(
        descriptor.subject.clone(),
        descriptor.computer_revision,
        replacement,
        timestamp(4),
        timestamp(1_000),
        descriptor.controller.clone(),
    )
    .unwrap();
    assert_eq!(
        session.reconnect(
            stale,
            keith_computer::ComputerStreamCursor {
                generation: 3,
                sequence: 1
            },
            timestamp(5),
            timestamp(130),
        ),
        Err(ComputerStreamError::InvalidCursor),
    );
}

struct ProcessTaskBoundary {
    child: Mutex<Child>,
}

impl ProcessTaskBoundary {
    fn new() -> Self {
        Self {
            child: Mutex::new(
                Command::new("/bin/sleep")
                    .arg("30")
                    .spawn()
                    .expect("real pause-boundary process must start"),
            ),
        }
    }

    fn signal(&self, signal: &str) -> Result<(), TakeoverBoundaryError> {
        let process_id = self
            .child
            .lock()
            .map_err(|_| TakeoverBoundaryError::new("process boundary lock poisoned").unwrap())?
            .id();
        let status = Command::new("/bin/kill")
            .args([signal, &process_id.to_string()])
            .status()
            .map_err(|_| TakeoverBoundaryError::new("process signal failed").unwrap())?;
        if status.success() {
            Ok(())
        } else {
            Err(TakeoverBoundaryError::new("process rejected signal").unwrap())
        }
    }
}

impl TakeoverTaskBoundary for ProcessTaskBoundary {
    fn pause_agent_input(&self, _: &TakeoverPauseBoundary) -> Result<(), TakeoverBoundaryError> {
        self.signal("-STOP")
    }

    fn release_uncommitted_pause(
        &self,
        _: &TakeoverPauseBoundary,
    ) -> Result<(), TakeoverBoundaryError> {
        self.signal("-CONT")
    }

    fn refresh_observation(
        &self,
        _: &TakeoverResolutionBoundary,
    ) -> Result<RefreshedComputerObservation, TakeoverBoundaryError> {
        let process_id = self
            .child
            .lock()
            .map_err(|_| TakeoverBoundaryError::new("process boundary lock poisoned").unwrap())?
            .id();
        std::fs::read_to_string(format!("/proc/{process_id}/status"))
            .map_err(|_| TakeoverBoundaryError::new("process observation failed").unwrap())?;
        Ok(RefreshedComputerObservation {
            observation_key: task_key(&format!("process/{process_id}/observation")),
            observed_at: timestamp(300),
        })
    }

    fn resume_owning_task(
        &self,
        _: &TakeoverResolutionBoundary,
        _: &RefreshedComputerObservation,
    ) -> Result<(), TakeoverBoundaryError> {
        self.signal("-CONT")
    }

    fn fail_owning_task(
        &self,
        _: &TakeoverResolutionBoundary,
        _: &str,
    ) -> Result<(), TakeoverBoundaryError> {
        self.signal("-KILL")
    }
}

impl Drop for ProcessTaskBoundary {
    fn drop(&mut self) {
        if let Ok(child) = self.child.get_mut() {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

struct RepositorySecretAuthority<R> {
    repository: R,
    credential_ref: CredentialRef,
    target: SecretInjectionTarget,
    policy_revision: Revision,
}

impl<R: ComputerRepository> SecretInjectionAuthorityResolver for RepositorySecretAuthority<R> {
    type Error = ComputerError;

    fn resolve_current(
        &self,
        profile_id: &ProfileId,
        computer_id: &ComputerId,
        task_key: &StableKey,
    ) -> Result<SecretInjectionAuthority, Self::Error> {
        let record = self
            .repository
            .computer(profile_id)?
            .ok_or_else(|| ComputerError::MissingComputer(profile_id.clone()))?;
        if record.computer_id != *computer_id
            || record.owner_profile_id != *profile_id
            || record.state != ComputerState::Ready
            || record.control_state != ControlState::Agent
            || record.current_task_key.as_ref() != Some(task_key)
        {
            return Err(ComputerError::Malformed("secret authority is not current"));
        }
        Ok(SecretInjectionAuthority {
            profile_id: profile_id.clone(),
            computer_id: computer_id.clone(),
            task_key: task_key.clone(),
            task_fencing_token: record.revision.get(),
            computer_revision: record.revision,
            policy_revision: self.policy_revision,
            credential_ref: self.credential_ref.clone(),
            credential_owner: self.credential_ref.owner.clone(),
            target: self.target.clone(),
            enabled: true,
            allow_secret_injection: true,
            requires_owner_approval: true,
            recording_active: false,
            max_secret_bytes: 4_096,
        })
    }
}

struct ProcessSecretWriter {
    digest: Arc<Mutex<Option<String>>>,
}

impl ProcessSecretWriter {
    fn write_to_process(&self, secret: &[u8]) -> Result<(), std::io::Error> {
        let mut child = Command::new("/usr/bin/sha256sum")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .spawn()?;
        let mut input = child.stdin.take().ok_or_else(|| {
            std::io::Error::new(
                std::io::ErrorKind::BrokenPipe,
                "sha256sum stdin unavailable",
            )
        })?;
        input.write_all(secret)?;
        input.flush()?;
        drop(input);
        let output = child.wait_with_output()?;
        if !output.status.success() {
            return Err(std::io::Error::other("sha256sum rejected focused bytes"));
        }
        let digest = String::from_utf8(output.stdout)
            .map_err(|_| std::io::Error::other("sha256sum output was invalid"))?;
        let digest = digest
            .split_whitespace()
            .next()
            .ok_or_else(|| std::io::Error::other("sha256sum output omitted digest"))?;
        *self
            .digest
            .lock()
            .map_err(|_| std::io::Error::other("digest lock poisoned"))? = Some(digest.to_owned());
        Ok(())
    }
}

impl FocusedSecretWriter for ProcessSecretWriter {
    type Error = std::io::Error;

    fn write_focused_field(
        &mut self,
        _: &str,
        _: &str,
        _: &str,
        secret: &[u8],
    ) -> Result<(), Self::Error> {
        self.write_to_process(secret)
    }

    fn write_credential_broker(
        &mut self,
        _: &str,
        _: &str,
        secret: &[u8],
    ) -> Result<(), Self::Error> {
        self.write_to_process(secret)
    }
}

fn assert_no_plaintext_files(root: &std::path::Path, plaintext: &[u8]) {
    let mut pending = vec![root.to_path_buf()];
    while let Some(path) = pending.pop() {
        for entry in std::fs::read_dir(path).unwrap() {
            let entry = entry.unwrap();
            if entry.file_type().unwrap().is_dir() {
                pending.push(entry.path());
            } else {
                let bytes = std::fs::read(entry.path()).unwrap();
                assert!(
                    !bytes
                        .windows(plaintext.len())
                        .any(|window| window == plaintext),
                    "credential plaintext reached a durable file",
                );
            }
        }
    }
}

#[test]
fn encrypted_secret_injection_uses_a_real_write_only_process_and_persists_no_plaintext() {
    let directory = tempfile::tempdir().unwrap();
    let credentials = EncryptedCredentialStore::open(
        &directory.path().join("credentials"),
        MasterKey::from_bytes([0x5a; 32]),
    )
    .unwrap();
    let owner = CredentialOwner::Tool("browser-login".into());
    let credential_ref = CredentialRef::new("account-password", owner.clone()).unwrap();
    let plaintext = b"keith-real-secret-process-boundary";
    credentials
        .put(
            credential_ref.clone(),
            SecretValue::new(String::from_utf8(plaintext.to_vec()).unwrap()).unwrap(),
            timestamp(0),
        )
        .unwrap();
    let profile_id = ProfileId::new();
    let computer_id = ComputerId::new();
    let current_task = task_key("task/secret-process");
    let repository = InMemoryComputerRepository::default();
    let initial_computer = ComputerRecord {
        version: CURRENT_SCHEMA_VERSION,
        computer_id: computer_id.clone(),
        owner_profile_id: profile_id.clone(),
        browser_profile_root: directory
            .path()
            .join("browser")
            .to_string_lossy()
            .into_owned(),
        screen_key: task_key("screen/secret-process"),
        state: ComputerState::Provisioning,
        control_state: ControlState::Idle,
        current_task_key: None,
        created_at: timestamp(0),
        updated_at: timestamp(0),
        revision: Revision::ZERO,
    };
    repository
        .transact(&[ComputerRepositoryBatch::InsertComputer(
            initial_computer.clone(),
        )])
        .unwrap();
    repository
        .transact(&[ComputerRepositoryBatch::ReplaceComputer {
            expected_revision: Revision::ZERO,
            record: ComputerRecord {
                state: ComputerState::Ready,
                control_state: ControlState::Agent,
                current_task_key: Some(current_task.clone()),
                updated_at: timestamp(1),
                revision: Revision::new(1),
                ..initial_computer
            },
        }])
        .unwrap();
    let target = SecretInjectionTarget::FocusedField {
        exact_origin: "https://accounts.example.com".into(),
        frame_origin: "https://accounts.example.com".into(),
        field_id: "password".into(),
        focus_revision: Revision::new(9),
    };
    let authority = RepositorySecretAuthority {
        repository,
        credential_ref: credential_ref.clone(),
        target: target.clone(),
        policy_revision: Revision::new(3),
    };
    let digest = Arc::new(Mutex::new(None));
    let writer = ProcessSecretWriter {
        digest: Arc::clone(&digest),
    };
    let mut injection = SecureSecretInjection::new(&credentials, authority, writer);
    let request = SecretInjectionRequest {
        operation_key: task_key("secret/inject/real-process"),
        claimed_profile_id: profile_id.clone(),
        computer_id,
        task_key: current_task,
        task_fencing_token: 1,
        computer_revision: Revision::new(1),
        policy_revision: Revision::new(3),
        credential_ref,
        target,
        owner_approved: true,
    };
    assert!(matches!(
        injection.inject(&ProfileId::new(), request.clone(), timestamp(2)),
        Err(keith_computer::SecretInjectionError::Unauthorized)
    ));
    let receipt = injection
        .inject(&profile_id, request, timestamp(3))
        .unwrap();
    let receipt = serde_json::to_vec(&receipt).unwrap();
    assert!(
        !receipt
            .windows(plaintext.len())
            .any(|window| window == plaintext)
    );
    let digest = digest.lock().unwrap().clone().unwrap();
    assert_eq!(digest.len(), 64);
    assert!(!digest.contains("keith-real-secret"));
    assert_no_plaintext_files(directory.path(), plaintext);
}

#[test]
fn takeover_forged_token_is_denied_and_restart_preserves_audited_handback() {
    let repository = InMemoryComputerRepository::default();
    let profile_id = ProfileId::new();
    let key = task_key("task/real-process");
    let record = ComputerRecord {
        version: CURRENT_SCHEMA_VERSION,
        computer_id: ComputerId::new(),
        owner_profile_id: profile_id.clone(),
        browser_profile_root: "/tmp/keith-computer-test-profile".into(),
        screen_key: task_key("screen/real-process"),
        state: ComputerState::Ready,
        control_state: ControlState::Agent,
        current_task_key: Some(key.clone()),
        created_at: timestamp(0),
        updated_at: timestamp(0),
        revision: Revision::ZERO,
    };
    repository
        .transact(&[ComputerRepositoryBatch::InsertComputer(record)])
        .unwrap();
    let boundary = ProcessTaskBoundary::new();
    let service = TakeoverLeaseService::new(repository);
    let claim = service
        .acquire(
            TakeoverAcquireRequest {
                owner_profile_id: profile_id.clone(),
                expected_computer_revision: Revision::ZERO,
                task_key: key,
                token_digest_hex: "a".repeat(64),
                lease_millis: 10_000,
                operation_key: task_key("takeover/acquire/real-process"),
                now: timestamp(10),
            },
            &boundary,
        )
        .unwrap();
    assert!(matches!(
        service.handback(
            TakeoverHandbackRequest {
                claim: claim.clone(),
                presented_token_digest_hex: "f".repeat(64),
                operation_key: task_key("takeover/forged/real-process"),
                now: timestamp(20),
            },
            &boundary,
        ),
        Err(TakeoverServiceError::StaleController)
    ));
    let renewed = service
        .renew(TakeoverRenewRequest {
            claim,
            presented_token_digest_hex: "a".repeat(64),
            replacement_token_digest_hex: "b".repeat(64),
            lease_millis: 10_000,
            operation_key: task_key("takeover/renew/real-process"),
            now: timestamp(30),
        })
        .unwrap();
    let repository = service.into_repository();
    let restarted_service = TakeoverLeaseService::new(repository);
    restarted_service
        .handback(
            TakeoverHandbackRequest {
                claim: renewed,
                presented_token_digest_hex: "b".repeat(64),
                operation_key: task_key("takeover/handback/real-process"),
                now: timestamp(40),
            },
            &boundary,
        )
        .unwrap();
    assert_eq!(
        restarted_service
            .repository()
            .computer(&profile_id)
            .unwrap()
            .unwrap()
            .control_state,
        ControlState::Agent,
    );
    assert!(
        restarted_service
            .repository()
            .audit(&profile_id)
            .unwrap()
            .len()
            >= 3
    );
    assert!(matches!(
        restarted_service
            .repository()
            .lease(&profile_id)
            .unwrap()
            .unwrap()
            .state,
        keith_computer::TakeoverState::HandedBack
    ));
}

#[test]
fn stale_computer_revision_cannot_be_replaced_during_stream_or_takeover_work() {
    let repository = InMemoryComputerRepository::default();
    let profile_id = ProfileId::new();
    let record = ComputerRecord {
        version: CURRENT_SCHEMA_VERSION,
        computer_id: ComputerId::new(),
        owner_profile_id: profile_id.clone(),
        browser_profile_root: "/tmp/keith-stale-revision".into(),
        screen_key: task_key("screen/stale-revision"),
        state: ComputerState::Ready,
        control_state: ControlState::Idle,
        current_task_key: None,
        created_at: timestamp(0),
        updated_at: timestamp(0),
        revision: Revision::ZERO,
    };
    repository
        .transact(&[ComputerRepositoryBatch::InsertComputer(record.clone())])
        .unwrap();
    let mut replacement = record;
    replacement.revision = Revision::new(2);
    assert!(matches!(
        repository.transact(&[ComputerRepositoryBatch::ReplaceComputer {
            expected_revision: Revision::new(1),
            record: replacement,
        }]),
        Err(ComputerError::RevisionConflict { .. })
    ));
}
