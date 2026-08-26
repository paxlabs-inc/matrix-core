use std::collections::{BTreeMap, VecDeque};
use std::fs;
use std::io::{Read, Write};
use std::path::PathBuf;
use std::process::{Child, Command, Stdio};
use std::sync::Mutex;
use std::thread;
use std::time::Duration;

use keith_agent_types::{AuditId, EntityId, ProfileId, Revision, StableKey, UtcTimestamp};
use keith_resource_governor::{ResourceScope, ScopePath};
pub use keith_supervisor::HeadedBrowserSupervisor;
use keith_supervisor::SupervisorError;
use keith_web::{BrowserControlBounds, HeadedBrowserLaunch};
use thiserror::Error;

use crate::model::{
    AuditActor, ComputerAudit, ComputerAuditKind, ComputerError, ComputerRecord,
    ComputerRepository, ComputerRepositoryBatch, ComputerState, ControlState, TakeoverState,
};
use crate::policy::{BoundaryDecision, ComputerAction, ComputerActor, ComputerBoundaryPolicy};
use crate::recovery::{ComputerCrashTracker, RecoveryDecision, reconcile_computer};
use crate::secrets::FocusedSecretWriter;
use crate::stream::{
    ComputerFrame, ComputerFrameEncoding, ComputerInputCommand, ComputerInputPayload,
    ComputerInputReceipt, ComputerObservation, ComputerStreamAuthorization,
    ComputerStreamController, ComputerStreamError, ComputerStreamOrigin, ComputerStreamSession,
    ComputerStreamSubject,
};
use crate::takeover::{RefreshedComputerObservation, TakeoverResolutionBoundary};

#[derive(Clone, Debug)]
pub struct ComputerHostConfig {
    pub browser_runner_binary: PathBuf,
    pub chromium_binary: PathBuf,
    pub xvfb_binary: PathBuf,
    pub systemd_run_binary: PathBuf,
    pub display_base: u16,
    pub control_port_base: u16,
    pub screen_width: u16,
    pub screen_height: u16,
    pub screen_depth: u8,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TaskConflictPolicy {
    Queue,
    Deny,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerTaskRequest {
    pub owner_profile_id: ProfileId,
    pub task_key: StableKey,
    pub actor: ComputerActor,
    pub conflict: TaskConflictPolicy,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerTaskLease {
    pub owner_profile_id: ProfileId,
    pub task_key: StableKey,
    pub fencing_token: u64,
    pub computer_revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TaskAdmission {
    Acquired(ComputerTaskLease),
    Queued { position: usize },
    Denied,
}

#[derive(Debug, Error)]
pub enum ComputerHostError {
    #[error(transparent)]
    Computer(#[from] ComputerError),
    #[error("computer host I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("computer process is not running for profile {0}")]
    NotRunning(ProfileId),
    #[error("computer actor is not authorized for profile {0}")]
    Unauthorized(ProfileId),
    #[error("computer boundary policy denied the action")]
    PolicyDenied,
    #[error("computer action requires owner approval")]
    ApprovalRequired,
    #[error("computer host lock was poisoned")]
    LockPoisoned,
    #[error("computer process exited during startup")]
    ProcessExited,
    #[error("computer auxiliary supervisor failed: {0}")]
    Supervisor(#[from] SupervisorError),
    #[error("computer headed browser launch is invalid")]
    InvalidBrowserLaunch,
    #[error(transparent)]
    Stream(#[from] ComputerStreamError),
    #[error("computer browser runner returned an invalid response")]
    RunnerProtocol,
    #[error("computer browser runner response exceeded its bound")]
    RunnerResponseTooLarge,
    #[error("credential references require the secure secret injection boundary")]
    SecretInjectionRequired,
    #[error("secret input exceeded the write-only browser boundary")]
    SecretInputOutOfBounds,
}

struct RunningComputer {
    display: u16,
    xvfb: Child,
    runner: Child,
    policy: ComputerBoundaryPolicy,
    last_activity: UtcTimestamp,
    network_requests: VecDeque<i64>,
    scope: ScopePath,
}

pub struct ComputerHost<R> {
    repository: R,
    config: ComputerHostConfig,
    running: Mutex<BTreeMap<ProfileId, RunningComputer>>,
    queued: Mutex<BTreeMap<ProfileId, VecDeque<ComputerTaskRequest>>>,
    crashes: ComputerCrashTracker,
}

pub struct ComputerHostSecretWriter<'a, R> {
    host: &'a ComputerHost<R>,
    subject: ComputerStreamSubject,
    controller: ComputerStreamController,
}

impl<R: ComputerRepository> ComputerHost<R> {
    pub const fn secret_writer(
        &self,
        subject: ComputerStreamSubject,
        controller: ComputerStreamController,
    ) -> ComputerHostSecretWriter<'_, R> {
        ComputerHostSecretWriter {
            host: self,
            subject,
            controller,
        }
    }

    fn dispatch_secret_input(
        &self,
        subject: &ComputerStreamSubject,
        controller: &ComputerStreamController,
        target: RunnerSecretTarget<'_>,
        secret: &[u8],
    ) -> Result<(), ComputerHostError> {
        if secret.is_empty() || secret.len() > 64 * 1024 {
            return Err(ComputerHostError::SecretInputOutOfBounds);
        }
        let now = UtcTimestamp::now().map_err(|_| ComputerHostError::RunnerProtocol)?;
        self.current_stream_record(subject, controller, now)?;
        let request_id = EntityId::new();
        let encoded = ZeroingString(encode_base64(secret));
        let request = RunnerSecretRequest {
            request_id: &request_id,
            profile_id: &subject.profile_id,
            command: RunnerSecretCommand {
                kind: "secret_input",
                target,
                bytes_base64: &encoded.0,
            },
        };
        let mut request = ZeroingBytes(
            serde_json::to_vec(&request).map_err(|_| ComputerHostError::RunnerProtocol)?,
        );
        if request.0.len() > 256 * 1024 {
            request.0.fill(0);
            return Err(ComputerHostError::SecretInputOutOfBounds);
        }
        let mut running = self
            .running
            .lock()
            .map_err(|_| ComputerHostError::LockPoisoned)?;
        let runtime = running
            .get_mut(&subject.profile_id)
            .ok_or_else(|| ComputerHostError::NotRunning(subject.profile_id.clone()))?;
        if runtime.runner.try_wait()?.is_some() {
            request.0.fill(0);
            return Err(ComputerHostError::ProcessExited);
        }
        let input = runtime
            .runner
            .stdin
            .as_mut()
            .ok_or(ComputerHostError::RunnerProtocol)?;
        let write = input
            .write_all(&request.0)
            .and_then(|()| input.write_all(b"\n"))
            .and_then(|()| input.flush());
        request.0.fill(0);
        write?;
        let output = runtime
            .runner
            .stdout
            .as_mut()
            .ok_or(ComputerHostError::RunnerProtocol)?;
        let response = read_bounded_line(output, 64 * 1024)?;
        let response: serde_json::Value =
            serde_json::from_slice(&response).map_err(|_| ComputerHostError::RunnerProtocol)?;
        let request_id = request_id.to_string();
        let profile_id = subject.profile_id.to_string();
        if response
            .get("request_id")
            .and_then(serde_json::Value::as_str)
            != Some(request_id.as_str())
            || response
                .get("profile_id")
                .and_then(serde_json::Value::as_str)
                != Some(profile_id.as_str())
            || runner_outcome(&response, "secret_applied")?
                .get("applied")
                .and_then(serde_json::Value::as_bool)
                != Some(true)
        {
            return Err(ComputerHostError::RunnerProtocol);
        }
        Ok(())
    }
}

impl<R: ComputerRepository> FocusedSecretWriter for ComputerHostSecretWriter<'_, R> {
    type Error = ComputerHostError;

    fn write_focused_field(
        &mut self,
        exact_origin: &str,
        frame_origin: &str,
        field_id: &str,
        secret: &[u8],
    ) -> Result<(), Self::Error> {
        self.host.dispatch_secret_input(
            &self.subject,
            &self.controller,
            RunnerSecretTarget::FocusedField {
                exact_origin,
                frame_origin,
                field_id,
            },
            secret,
        )
    }

    fn write_credential_broker(
        &mut self,
        exact_origin: &str,
        broker_id: &str,
        secret: &[u8],
    ) -> Result<(), Self::Error> {
        self.host.dispatch_secret_input(
            &self.subject,
            &self.controller,
            RunnerSecretTarget::CredentialBroker {
                exact_origin,
                broker_id,
            },
            secret,
        )
    }
}

impl<R: ComputerRepository> ComputerHost<R> {
    pub fn new(repository: R, config: ComputerHostConfig) -> Self {
        Self {
            repository,
            config,
            running: Mutex::new(BTreeMap::new()),
            queued: Mutex::new(BTreeMap::new()),
            crashes: ComputerCrashTracker::default(),
        }
    }

    pub const fn repository(&self) -> &R {
        &self.repository
    }

    pub fn authorize_stream(
        &self,
        subject: ComputerStreamSubject,
        origin: ComputerStreamOrigin,
        controller: ComputerStreamController,
        issued_at: UtcTimestamp,
        expires_at: UtcTimestamp,
    ) -> Result<ComputerStreamAuthorization, ComputerHostError> {
        let record = self.current_stream_record(&subject, &controller, issued_at)?;
        if !self
            .running
            .lock()
            .map_err(|_| ComputerHostError::LockPoisoned)?
            .contains_key(&subject.profile_id)
        {
            return Err(ComputerHostError::NotRunning(subject.profile_id));
        }
        Ok(ComputerStreamAuthorization::issue(
            subject,
            record.revision,
            origin,
            issued_at,
            expires_at,
            controller,
        )?)
    }

    pub fn capture_stream_frame(
        &self,
        session: &mut ComputerStreamSession,
        now: UtcTimestamp,
        next_liveness_deadline: UtcTimestamp,
    ) -> Result<ComputerFrame, ComputerHostError> {
        let descriptor = session.descriptor();
        self.current_stream_record(&descriptor.subject, &descriptor.controller, now)?;
        let response = self.runner_request(
            &descriptor.subject.profile_id,
            serde_json::json!({
                "kind": "capture",
                "max_width": descriptor.limits.max_width,
                "max_height": descriptor.limits.max_height,
                "encoding": "png"
            }),
            descriptor
                .limits
                .max_frame_bytes
                .saturating_mul(2)
                .saturating_add(16_384),
        )?;
        let value = runner_outcome(&response, "frame")?;
        let width = bounded_u32(value.get("width"), descriptor.limits.max_width)?;
        let height = bounded_u32(value.get("height"), descriptor.limits.max_height)?;
        let encoded = value
            .get("bytes_base64")
            .and_then(serde_json::Value::as_str)
            .ok_or(ComputerHostError::RunnerProtocol)?;
        let bytes = decode_base64_bounded(encoded, descriptor.limits.max_frame_bytes)?;
        let sequence = descriptor
            .cursor
            .sequence
            .checked_add(1)
            .ok_or(ComputerStreamError::SequenceExhausted)?;
        let frame = ComputerFrame {
            session_id: descriptor.session_id,
            subject: descriptor.subject,
            origin: descriptor.origin,
            sequence,
            captured_at: now,
            width,
            height,
            encoding: ComputerFrameEncoding::Png,
            key_frame: true,
            bytes,
        };
        session.accept_frame(&frame, now, next_liveness_deadline)?;
        Ok(frame)
    }

    pub fn observe_stream(
        &self,
        session: &mut ComputerStreamSession,
        now: UtcTimestamp,
        next_liveness_deadline: UtcTimestamp,
    ) -> Result<ComputerObservation, ComputerHostError> {
        const MAX_OBSERVATION_BYTES: usize = 1024 * 1024;
        let descriptor = session.descriptor();
        self.current_stream_record(&descriptor.subject, &descriptor.controller, now)?;
        let response = self.runner_request(
            &descriptor.subject.profile_id,
            serde_json::json!({ "kind": "observe" }),
            MAX_OBSERVATION_BYTES,
        )?;
        let value = runner_outcome(&response, "page")?;
        let url = bounded_runner_text(value.get("url"), 16 * 1024)?;
        let title = bounded_runner_text(value.get("title"), 16 * 1024)?;
        let text = bounded_runner_text(value.get("text"), MAX_OBSERVATION_BYTES - 32 * 1024)?;
        let observation = ComputerObservation {
            session_id: descriptor.session_id,
            subject: descriptor.subject,
            origin: descriptor.origin,
            url,
            title,
            text,
            observed_at: now,
        };
        session.accept_observation(&observation, now, next_liveness_deadline)?;
        Ok(observation)
    }

    pub fn dispatch_stream_input(
        &self,
        session: &mut ComputerStreamSession,
        input: ComputerInputCommand,
        now: UtcTimestamp,
        next_liveness_deadline: UtcTimestamp,
    ) -> Result<ComputerInputReceipt, ComputerHostError> {
        let descriptor = session.descriptor();
        let actor = self.current_stream_actor(&descriptor.subject, &descriptor.controller, now)?;
        self.authorize_action(
            &descriptor.subject.profile_id,
            &actor,
            &ComputerAction::Input,
            now,
        )?;
        if matches!(
            input.payload,
            ComputerInputPayload::CredentialReference { .. }
        ) {
            return Err(ComputerHostError::SecretInjectionRequired);
        }
        let (receipt, payload) = session.authorize_input(input, now, next_liveness_deadline)?;
        let response = self.runner_request(
            &descriptor.subject.profile_id,
            serde_json::json!({ "kind": "input", "payload": payload }),
            64 * 1024,
        )?;
        runner_outcome(&response, "input_applied")?;
        Ok(receipt)
    }

    pub fn refresh_takeover_observation(
        &self,
        boundary: &TakeoverResolutionBoundary,
        now: UtcTimestamp,
    ) -> Result<RefreshedComputerObservation, ComputerHostError> {
        let record = self
            .repository
            .computer(&boundary.owner_profile_id)?
            .ok_or_else(|| ComputerError::MissingComputer(boundary.owner_profile_id.clone()))?;
        let lease = self
            .repository
            .lease(&boundary.owner_profile_id)?
            .ok_or_else(|| ComputerError::MissingLease(boundary.owner_profile_id.clone()))?;
        if record.computer_id != boundary.computer_id
            || record.owner_profile_id != boundary.owner_profile_id
            || record.control_state != ControlState::Paused
            || record.current_task_key.as_ref() != Some(&boundary.task_key)
            || lease.takeover_lease_id != boundary.takeover_lease_id
            || lease.computer_id != boundary.computer_id
            || lease.owner_profile_id != boundary.owner_profile_id
            || lease.task_key != boundary.task_key
            || lease.fencing_token != boundary.fencing_token
            || !matches!(
                lease.state,
                TakeoverState::HandedBack | TakeoverState::Expired
            )
        {
            return Err(ComputerHostError::Unauthorized(
                boundary.owner_profile_id.clone(),
            ));
        }
        let response = self.runner_request(
            &boundary.owner_profile_id,
            serde_json::json!({ "kind": "observe" }),
            1024 * 1024,
        )?;
        let value = runner_outcome(&response, "page")?;
        bounded_runner_text(value.get("url"), 16 * 1024)?;
        bounded_runner_text(value.get("title"), 16 * 1024)?;
        bounded_runner_text(value.get("text"), 1024 * 1024 - 32 * 1024)?;
        Ok(RefreshedComputerObservation {
            observation_key: StableKey::parse(format!(
                "takeover/observation/{}/{}",
                boundary.takeover_lease_id, boundary.fencing_token,
            ))
            .map_err(|_| ComputerError::Malformed("takeover observation stable key"))?,
            observed_at: now,
        })
    }

    pub fn start_profile(
        &self,
        owner: &ProfileId,
        policy: ComputerBoundaryPolicy,
        now: UtcTimestamp,
    ) -> Result<(), ComputerHostError> {
        let record = self
            .repository
            .computer(owner)?
            .ok_or_else(|| ComputerError::MissingComputer(owner.clone()))?;
        self.start(&record, policy, now)
    }

    pub fn start(
        &self,
        record: &ComputerRecord,
        policy: ComputerBoundaryPolicy,
        now: UtcTimestamp,
    ) -> Result<(), ComputerHostError> {
        if record.state != ComputerState::Ready {
            return Err(ComputerError::Malformed("only ready computers may start").into());
        }
        policy.validate().map_err(ComputerError::Malformed)?;
        fs::create_dir_all(&record.browser_profile_root)?;
        let display = self.display_for(record);
        let control_port = self
            .config
            .control_port_base
            .saturating_add(display.wrapping_sub(self.config.display_base));
        let control_endpoint = format!("127.0.0.1:{control_port}")
            .parse()
            .map_err(|_| ComputerHostError::InvalidBrowserLaunch)?;
        HeadedBrowserLaunch::new(
            self.config.chromium_binary.clone(),
            record.owner_profile_id.clone(),
            PathBuf::from(&record.browser_profile_root),
            format!(":{display}"),
            control_endpoint,
            BrowserControlBounds::default(),
        )
        .map_err(|_| ComputerHostError::InvalidBrowserLaunch)?;
        let scope = ScopePath::new(vec![
            ResourceScope::Installation,
            ResourceScope::Profile(record.owner_profile_id.clone()),
            ResourceScope::Computer(record.owner_profile_id.clone()),
            ResourceScope::Display(record.owner_profile_id.clone()),
        ])
        .map_err(|_| ComputerError::Malformed("computer resource scope"))?;
        let mut xvfb = self.spawn_supervised(
            &self.config.xvfb_binary,
            &[
                format!(":{display}"),
                "-screen".into(),
                "0".into(),
                format!(
                    "{}x{}x{}",
                    self.config.screen_width, self.config.screen_height, self.config.screen_depth
                ),
                "-nolisten".into(),
                "tcp".into(),
            ],
            &policy,
            None,
        )?;
        if xvfb.try_wait()?.is_some() {
            return Err(ComputerHostError::ProcessExited);
        }
        let mut runner = self.spawn_supervised(
            &self.config.browser_runner_binary,
            &[
                "--chromium".into(),
                self.config.chromium_binary.display().to_string(),
                "--profile".into(),
                record.owner_profile_id.to_string(),
                "--user-data-dir".into(),
                record.browser_profile_root.clone(),
                "--display".into(),
                format!(":{display}"),
                "--control-endpoint".into(),
                format!("127.0.0.1:{control_port}"),
            ],
            &policy,
            Some(display),
        )?;
        if runner.try_wait()?.is_some() {
            let _ = xvfb.kill();
            return Err(ComputerHostError::ProcessExited);
        }
        let readiness = (|| {
            let output = runner
                .stdout
                .as_mut()
                .ok_or(ComputerHostError::RunnerProtocol)?;
            let response = read_bounded_line(output, 64 * 1024)?;
            let response: serde_json::Value =
                serde_json::from_slice(&response).map_err(|_| ComputerHostError::RunnerProtocol)?;
            validate_runner_readiness(
                &response,
                &record.owner_profile_id,
                &control_endpoint.to_string(),
                &format!(":{display}"),
            )
        })();
        if let Err(error) = readiness {
            let _ = runner.kill();
            let _ = runner.wait();
            let _ = xvfb.kill();
            let _ = xvfb.wait();
            return Err(error);
        }
        let mut running = self
            .running
            .lock()
            .map_err(|_| ComputerHostError::LockPoisoned)?;
        if let Some(mut prior) = running.insert(
            record.owner_profile_id.clone(),
            RunningComputer {
                display,
                xvfb,
                runner,
                policy,
                last_activity: now,
                network_requests: VecDeque::new(),
                scope,
            },
        ) {
            let _ = prior.runner.kill();
            let _ = prior.xvfb.kill();
        }
        Ok(())
    }

    pub fn authorize_action(
        &self,
        owner: &ProfileId,
        actor: &ComputerActor,
        action: &ComputerAction,
        now: UtcTimestamp,
    ) -> Result<(), ComputerHostError> {
        if !actor.is_authorized_for(owner) {
            return Err(ComputerHostError::Unauthorized(owner.clone()));
        }
        let record = self
            .repository
            .computer(owner)?
            .ok_or_else(|| ComputerError::MissingComputer(owner.clone()))?;
        let mut running = self
            .running
            .lock()
            .map_err(|_| ComputerHostError::LockPoisoned)?;
        let runtime = running
            .get_mut(owner)
            .ok_or_else(|| ComputerHostError::NotRunning(owner.clone()))?;
        let idle_ms = i64::try_from(
            runtime
                .policy
                .resources
                .idle_timeout_seconds
                .saturating_mul(1_000),
        )
        .unwrap_or(i64::MAX);
        if now
            .unix_millis()
            .saturating_sub(runtime.last_activity.unix_millis())
            > idle_ms
            || directory_size(PathBuf::from(&record.browser_profile_root))?
                > runtime.policy.resources.max_disk_bytes
        {
            return Err(ComputerHostError::PolicyDenied);
        }
        if matches!(action, ComputerAction::Navigate { .. }) {
            let cutoff = now.unix_millis().saturating_sub(60_000);
            while runtime
                .network_requests
                .front()
                .is_some_and(|time| *time < cutoff)
            {
                runtime.network_requests.pop_front();
            }
            if u32::try_from(runtime.network_requests.len()).unwrap_or(u32::MAX)
                >= runtime.policy.resources.max_network_requests_per_minute
            {
                return Err(ComputerHostError::PolicyDenied);
            }
            runtime.network_requests.push_back(now.unix_millis());
        }
        if let ComputerAction::Download { destination } = action {
            if destination.metadata().ok().is_some_and(|metadata| {
                metadata.len() > runtime.policy.resources.max_download_bytes
            }) {
                return Err(ComputerHostError::PolicyDenied);
            }
        }
        match runtime.policy.evaluate(action) {
            BoundaryDecision::Allow => {
                runtime.last_activity = now;
                Ok(())
            }
            BoundaryDecision::RequireApproval => Err(ComputerHostError::ApprovalRequired),
            BoundaryDecision::Deny => Err(ComputerHostError::PolicyDenied),
        }
    }

    pub fn acquire_task(
        &self,
        request: ComputerTaskRequest,
        now: UtcTimestamp,
    ) -> Result<TaskAdmission, ComputerHostError> {
        if !request.actor.is_authorized_for(&request.owner_profile_id) {
            return Err(ComputerHostError::Unauthorized(request.owner_profile_id));
        }
        let record = self
            .repository
            .computer(&request.owner_profile_id)?
            .ok_or_else(|| ComputerError::MissingComputer(request.owner_profile_id.clone()))?;
        if record.state != ComputerState::Ready {
            return Ok(TaskAdmission::Denied);
        }
        if record.current_task_key.is_some() {
            return match request.conflict {
                TaskConflictPolicy::Deny => Ok(TaskAdmission::Denied),
                TaskConflictPolicy::Queue => {
                    let mut queued = self
                        .queued
                        .lock()
                        .map_err(|_| ComputerHostError::LockPoisoned)?;
                    let queue = queued.entry(request.owner_profile_id.clone()).or_default();
                    queue.push_back(request);
                    Ok(TaskAdmission::Queued {
                        position: queue.len(),
                    })
                }
            };
        }
        let expected = record.revision;
        let next = expected
            .checked_next()
            .ok_or(ComputerError::RevisionOverflow)?;
        let mut updated = record;
        updated.current_task_key = Some(request.task_key.clone());
        updated.control_state = ControlState::Agent;
        updated.revision = next;
        updated.updated_at = now;
        let audit = self.audit(
            &updated,
            request.task_key.clone(),
            ComputerAuditKind::TaskChanged,
            "computer task acquired",
            now,
        )?;
        self.repository.transact(&[
            ComputerRepositoryBatch::ReplaceComputer {
                expected_revision: expected,
                record: updated,
            },
            ComputerRepositoryBatch::AppendAudit(audit),
        ])?;
        Ok(TaskAdmission::Acquired(ComputerTaskLease {
            owner_profile_id: request.owner_profile_id,
            task_key: request.task_key,
            fencing_token: next.get(),
            computer_revision: next,
        }))
    }

    pub fn release_task(
        &self,
        lease: &ComputerTaskLease,
        now: UtcTimestamp,
    ) -> Result<Option<ComputerTaskRequest>, ComputerHostError> {
        let record = self
            .repository
            .computer(&lease.owner_profile_id)?
            .ok_or_else(|| ComputerError::MissingComputer(lease.owner_profile_id.clone()))?;
        if record.revision != lease.computer_revision
            || record.current_task_key.as_ref() != Some(&lease.task_key)
        {
            return Err(ComputerError::RevisionConflict {
                expected: lease.computer_revision,
                actual: record.revision,
            }
            .into());
        }
        let next = record
            .revision
            .checked_next()
            .ok_or(ComputerError::RevisionOverflow)?;
        let mut updated = record;
        updated.current_task_key = None;
        updated.control_state = ControlState::Idle;
        updated.revision = next;
        updated.updated_at = now;
        let audit = self.audit(
            &updated,
            lease.task_key.clone(),
            ComputerAuditKind::TaskChanged,
            "computer task released",
            now,
        )?;
        self.repository.transact(&[
            ComputerRepositoryBatch::ReplaceComputer {
                expected_revision: lease.computer_revision,
                record: updated,
            },
            ComputerRepositoryBatch::AppendAudit(audit),
        ])?;
        Ok(self
            .queued
            .lock()
            .map_err(|_| ComputerHostError::LockPoisoned)?
            .get_mut(&lease.owner_profile_id)
            .and_then(VecDeque::pop_front))
    }

    pub fn shutdown(&self, owner: &ProfileId) -> Result<(), ComputerHostError> {
        if let Some(mut runtime) = self
            .running
            .lock()
            .map_err(|_| ComputerHostError::LockPoisoned)?
            .remove(owner)
        {
            let _ = runtime.runner.kill();
            let _ = runtime.runner.wait();
            let _ = runtime.xvfb.kill();
            let _ = runtime.xvfb.wait();
        }
        Ok(())
    }

    pub fn reconcile(
        &self,
        owner: &ProfileId,
        policy: ComputerBoundaryPolicy,
        now: UtcTimestamp,
    ) -> Result<RecoveryDecision, ComputerHostError> {
        let record = self
            .repository
            .computer(owner)?
            .ok_or_else(|| ComputerError::MissingComputer(owner.clone()))?;
        let (present, running) = {
            let mut processes = self
                .running
                .lock()
                .map_err(|_| ComputerHostError::LockPoisoned)?;
            if let Some(runtime) = processes.get_mut(owner) {
                (
                    true,
                    runtime.runner.try_wait()?.is_none() && runtime.xvfb.try_wait()?.is_none(),
                )
            } else {
                (false, false)
            }
        };
        let mut decision = reconcile_computer(&record, running);
        if decision == RecoveryDecision::Relaunch && running {
            decision = RecoveryDecision::Noop;
        }
        if decision == RecoveryDecision::Relaunch {
            let crash = if present {
                self.crashes.record_and_decide(
                    owner,
                    now,
                    policy.resources.crash_limit,
                    policy.resources.crash_window_seconds,
                )
            } else {
                RecoveryDecision::Relaunch
            };
            if crash == RecoveryDecision::Quarantine {
                decision = crash;
            } else {
                self.shutdown(owner)?;
                self.start(&record, policy, now)?;
            }
        }
        if decision == RecoveryDecision::Quarantine && record.state != ComputerState::Quarantined {
            self.shutdown(owner)?;
            let expected = record.revision;
            let mut quarantined = record;
            quarantined.state = ComputerState::Quarantined;
            quarantined.control_state = ControlState::Idle;
            quarantined.current_task_key = None;
            quarantined.revision = expected
                .checked_next()
                .ok_or(ComputerError::RevisionOverflow)?;
            quarantined.updated_at = now;
            let key = StableKey::parse(format!(
                "computer/quarantine/{owner}/{}",
                quarantined.revision.get()
            ))
            .map_err(|_| ComputerError::Malformed("quarantine stable key"))?;
            let audit = self.audit(
                &quarantined,
                key,
                ComputerAuditKind::Recovery,
                "computer quarantined after process failure",
                now,
            )?;
            self.repository.transact(&[
                ComputerRepositoryBatch::ReplaceComputer {
                    expected_revision: expected,
                    record: quarantined,
                },
                ComputerRepositoryBatch::AppendAudit(audit),
            ])?;
        }
        Ok(decision)
    }

    pub fn display(&self, owner: &ProfileId) -> Result<Option<u16>, ComputerHostError> {
        Ok(self
            .running
            .lock()
            .map_err(|_| ComputerHostError::LockPoisoned)?
            .get(owner)
            .map(|runtime| runtime.display))
    }

    pub fn resource_scope(
        &self,
        owner: &ProfileId,
    ) -> Result<Option<ScopePath>, ComputerHostError> {
        Ok(self
            .running
            .lock()
            .map_err(|_| ComputerHostError::LockPoisoned)?
            .get(owner)
            .map(|runtime| runtime.scope.clone()))
    }

    fn current_stream_record(
        &self,
        subject: &ComputerStreamSubject,
        controller: &ComputerStreamController,
        now: UtcTimestamp,
    ) -> Result<ComputerRecord, ComputerHostError> {
        let record = self
            .repository
            .computer(&subject.profile_id)?
            .ok_or_else(|| ComputerError::MissingComputer(subject.profile_id.clone()))?;
        if record.computer_id != subject.computer_id
            || record.owner_profile_id != subject.profile_id
            || record.state != ComputerState::Ready
            || !controller.is_for_subject(subject)
            || record.current_task_key.as_ref() != Some(controller.task_key())
        {
            return Err(ComputerHostError::Unauthorized(subject.profile_id.clone()));
        }
        match controller {
            ComputerStreamController::Agent { fencing_token, .. }
            | ComputerStreamController::Routine { fencing_token, .. }
            | ComputerStreamController::Child { fencing_token, .. } => {
                if record.control_state != ControlState::Agent
                    || *fencing_token != record.revision.get()
                {
                    return Err(ComputerHostError::Unauthorized(subject.profile_id.clone()));
                }
            }
            ComputerStreamController::UserTakeover {
                lease_id,
                fencing_token,
                lease_revision,
                ..
            } => {
                let lease = self
                    .repository
                    .lease(&subject.profile_id)?
                    .ok_or_else(|| ComputerError::MissingLease(subject.profile_id.clone()))?;
                if record.control_state != ControlState::UserTakeover
                    || lease.takeover_lease_id != *lease_id
                    || lease.computer_id != subject.computer_id
                    || lease.owner_profile_id != subject.profile_id
                    || lease.task_key != *controller.task_key()
                    || lease.fencing_token != *fencing_token
                    || lease.revision != *lease_revision
                    || lease.state != TakeoverState::Active
                    || lease.expires_at <= now
                {
                    return Err(ComputerHostError::Unauthorized(subject.profile_id.clone()));
                }
            }
        }
        Ok(record)
    }

    fn current_stream_actor(
        &self,
        subject: &ComputerStreamSubject,
        controller: &ComputerStreamController,
        now: UtcTimestamp,
    ) -> Result<ComputerActor, ComputerHostError> {
        self.current_stream_record(subject, controller, now)?;
        Ok(match controller {
            ComputerStreamController::Agent { profile_id, .. } => ComputerActor::Agent {
                profile_id: profile_id.clone(),
            },
            ComputerStreamController::Routine { profile_id, .. } => ComputerActor::Routine {
                profile_id: profile_id.clone(),
            },
            ComputerStreamController::Child {
                profile_id,
                child_id,
                ..
            } => ComputerActor::Child {
                parent_profile_id: profile_id.clone(),
                child_id: child_id.clone(),
            },
            ComputerStreamController::UserTakeover { .. } => ComputerActor::User,
        })
    }

    fn runner_request(
        &self,
        owner: &ProfileId,
        command: serde_json::Value,
        max_response_bytes: usize,
    ) -> Result<serde_json::Value, ComputerHostError> {
        let request_id = EntityId::new();
        let request = serde_json::to_vec(&serde_json::json!({
            "request_id": request_id,
            "profile_id": owner,
            "command": command,
        }))
        .map_err(|_| ComputerHostError::RunnerProtocol)?;
        if request.len() > 256 * 1024 {
            return Err(ComputerHostError::RunnerProtocol);
        }
        let mut running = self
            .running
            .lock()
            .map_err(|_| ComputerHostError::LockPoisoned)?;
        let runtime = running
            .get_mut(owner)
            .ok_or_else(|| ComputerHostError::NotRunning(owner.clone()))?;
        if runtime.runner.try_wait()?.is_some() {
            return Err(ComputerHostError::ProcessExited);
        }
        let input = runtime
            .runner
            .stdin
            .as_mut()
            .ok_or(ComputerHostError::RunnerProtocol)?;
        input.write_all(&request)?;
        input.write_all(b"\n")?;
        input.flush()?;
        let output = runtime
            .runner
            .stdout
            .as_mut()
            .ok_or(ComputerHostError::RunnerProtocol)?;
        let response = read_bounded_line(output, max_response_bytes)?;
        let response: serde_json::Value =
            serde_json::from_slice(&response).map_err(|_| ComputerHostError::RunnerProtocol)?;
        let request_id = request_id.to_string();
        let owner = owner.to_string();
        if response
            .get("request_id")
            .and_then(serde_json::Value::as_str)
            != Some(request_id.as_str())
            || response
                .get("profile_id")
                .and_then(serde_json::Value::as_str)
                != Some(owner.as_str())
        {
            return Err(ComputerHostError::RunnerProtocol);
        }
        Ok(response)
    }

    fn display_for(&self, record: &ComputerRecord) -> u16 {
        let folded = record
            .screen_key
            .as_str()
            .bytes()
            .fold(0_u16, |value, byte| {
                value.wrapping_mul(31).wrapping_add(u16::from(byte))
            });
        self.config.display_base.saturating_add(folded % 10_000)
    }

    fn spawn_supervised(
        &self,
        program: &PathBuf,
        args: &[String],
        policy: &ComputerBoundaryPolicy,
        display: Option<u16>,
    ) -> Result<Child, ComputerHostError> {
        let command = |user_scope: bool| {
            let mut command = Command::new(&self.config.systemd_run_binary);
            if user_scope {
                command.arg("--user");
            }
            command
                .args(["--scope", "--quiet", "--collect"])
                .arg("-p")
                .arg(format!("CPUQuota={}%", policy.resources.cpu_quota_percent))
                .arg("-p")
                .arg(format!("MemoryMax={}", policy.resources.max_memory_bytes))
                .arg("-p")
                .arg(format!("TasksMax={}", policy.resources.max_processes))
                .arg("-p")
                .arg(format!(
                    "RuntimeMaxSec={}",
                    policy.resources.idle_timeout_seconds
                ))
                .arg("--")
                .arg(program)
                .args(args)
                .stdin(Stdio::piped())
                .stdout(Stdio::piped())
                .stderr(Stdio::null());
            if let Some(display) = display {
                command.env("DISPLAY", format!(":{display}"));
            }
            command
        };

        let mut child = command(true).spawn()?;
        thread::sleep(Duration::from_millis(100));
        if child.try_wait()?.is_none() {
            return Ok(child);
        }

        let mut child = command(false).spawn()?;
        thread::sleep(Duration::from_millis(100));
        if child.try_wait()?.is_some() {
            return Err(ComputerHostError::ProcessExited);
        }
        Ok(child)
    }

    fn audit(
        &self,
        record: &ComputerRecord,
        key: StableKey,
        kind: ComputerAuditKind,
        summary: &str,
        now: UtcTimestamp,
    ) -> Result<ComputerAudit, ComputerHostError> {
        let sequence = u64::try_from(self.repository.audit(&record.owner_profile_id)?.len())
            .unwrap_or(u64::MAX)
            .saturating_add(1);
        Ok(ComputerAudit {
            version: record.version,
            audit_id: AuditId::new(),
            computer_id: record.computer_id.clone(),
            owner_profile_id: record.owner_profile_id.clone(),
            sequence,
            stable_key: StableKey::parse(format!("host/{key}/{sequence}"))
                .map_err(|_| ComputerError::Malformed("host audit stable key"))?,
            actor: AuditActor::System,
            kind,
            task_key: Some(key),
            navigation_origin: None,
            control_transition: None,
            policy_decision: None,
            side_effect_summary: None,
            transfer: None,
            safe_failure: None,
            recovery_correlation: None,
            safe_summary: summary.into(),
            occurred_at: now,
            computer_revision: record.revision,
        })
    }
}

fn read_bounded_line(output: &mut impl Read, maximum: usize) -> Result<Vec<u8>, ComputerHostError> {
    let mut line = Vec::with_capacity(maximum.min(64 * 1024));
    let mut byte = [0_u8; 1];
    loop {
        let read = output.read(&mut byte)?;
        if read == 0 {
            return Err(ComputerHostError::RunnerProtocol);
        }
        if byte[0] == b'\n' {
            break;
        }
        if line.len() >= maximum {
            return Err(ComputerHostError::RunnerResponseTooLarge);
        }
        line.push(byte[0]);
    }
    if line.last() == Some(&b'\r') {
        line.pop();
    }
    if line.is_empty() {
        return Err(ComputerHostError::RunnerProtocol);
    }
    Ok(line)
}

fn runner_outcome<'a>(
    response: &'a serde_json::Value,
    expected_status: &str,
) -> Result<&'a serde_json::Value, ComputerHostError> {
    let outcome = response
        .get("outcome")
        .filter(|value| value.is_object())
        .ok_or(ComputerHostError::RunnerProtocol)?;
    if outcome.get("status").and_then(serde_json::Value::as_str) != Some(expected_status) {
        return Err(ComputerHostError::RunnerProtocol);
    }
    outcome
        .get("value")
        .filter(|value| value.is_object())
        .ok_or(ComputerHostError::RunnerProtocol)
}

fn validate_runner_readiness(
    response: &serde_json::Value,
    owner: &ProfileId,
    control_endpoint: &str,
    display: &str,
) -> Result<(), ComputerHostError> {
    let owner = owner.to_string();
    if response
        .get("request_id")
        .is_none_or(|value| !value.is_null())
        || response
            .get("profile_id")
            .and_then(serde_json::Value::as_str)
            != Some(owner.as_str())
    {
        return Err(ComputerHostError::RunnerProtocol);
    }
    let readiness = runner_outcome(response, "ready")?;
    if readiness
        .get("control_endpoint")
        .and_then(serde_json::Value::as_str)
        != Some(control_endpoint)
        || readiness.get("display").and_then(serde_json::Value::as_str) != Some(display)
        || readiness
            .get("persistent_profile")
            .and_then(serde_json::Value::as_bool)
            != Some(true)
    {
        return Err(ComputerHostError::RunnerProtocol);
    }
    Ok(())
}

fn bounded_u32(value: Option<&serde_json::Value>, maximum: u32) -> Result<u32, ComputerHostError> {
    let value = value
        .and_then(serde_json::Value::as_u64)
        .and_then(|value| u32::try_from(value).ok())
        .filter(|value| *value > 0 && *value <= maximum)
        .ok_or(ComputerHostError::RunnerProtocol)?;
    Ok(value)
}

fn bounded_runner_text(
    value: Option<&serde_json::Value>,
    maximum: usize,
) -> Result<String, ComputerHostError> {
    let value = value
        .and_then(serde_json::Value::as_str)
        .filter(|value| {
            value.len() <= maximum
                && !value.chars().any(|character| {
                    character.is_control() && !matches!(character, '\n' | '\r' | '\t')
                })
        })
        .ok_or(ComputerHostError::RunnerProtocol)?;
    Ok(value.to_owned())
}

fn decode_base64_bounded(encoded: &str, maximum: usize) -> Result<Vec<u8>, ComputerHostError> {
    let bytes = encoded.as_bytes();
    if bytes.is_empty() || bytes.len() % 4 != 0 {
        return Err(ComputerHostError::RunnerProtocol);
    }
    let padding = usize::from(bytes.ends_with(b"=")) + usize::from(bytes.ends_with(b"=="));
    let decoded_len = bytes
        .len()
        .checked_div(4)
        .and_then(|groups| groups.checked_mul(3))
        .and_then(|length| length.checked_sub(padding))
        .ok_or(ComputerHostError::RunnerProtocol)?;
    if decoded_len == 0 || decoded_len > maximum {
        return Err(ComputerHostError::RunnerResponseTooLarge);
    }
    let mut decoded = Vec::with_capacity(decoded_len);
    for (index, chunk) in bytes.chunks_exact(4).enumerate() {
        let last = index + 1 == bytes.len() / 4;
        if (!last && (chunk[2] == b'=' || chunk[3] == b'='))
            || chunk[0] == b'='
            || chunk[1] == b'='
            || chunk[2] == b'=' && chunk[3] != b'='
        {
            return Err(ComputerHostError::RunnerProtocol);
        }
        let a = decode_base64_character(chunk[0])?;
        let b = decode_base64_character(chunk[1])?;
        let c = if chunk[2] == b'=' {
            0
        } else {
            decode_base64_character(chunk[2])?
        };
        let d = if chunk[3] == b'=' {
            0
        } else {
            decode_base64_character(chunk[3])?
        };
        decoded.push((a << 2) | (b >> 4));
        if chunk[2] != b'=' {
            decoded.push((b << 4) | (c >> 2));
        }
        if chunk[3] != b'=' {
            decoded.push((c << 6) | d);
        }
    }
    if decoded.len() != decoded_len {
        return Err(ComputerHostError::RunnerProtocol);
    }
    Ok(decoded)
}

fn decode_base64_character(character: u8) -> Result<u8, ComputerHostError> {
    match character {
        b'A'..=b'Z' => Ok(character - b'A'),
        b'a'..=b'z' => Ok(character - b'a' + 26),
        b'0'..=b'9' => Ok(character - b'0' + 52),
        b'+' => Ok(62),
        b'/' => Ok(63),
        _ => Err(ComputerHostError::RunnerProtocol),
    }
}

#[derive(serde::Serialize)]
struct RunnerSecretRequest<'a> {
    request_id: &'a EntityId,
    profile_id: &'a ProfileId,
    command: RunnerSecretCommand<'a>,
}

#[derive(serde::Serialize)]
struct RunnerSecretCommand<'a> {
    kind: &'static str,
    target: RunnerSecretTarget<'a>,
    bytes_base64: &'a str,
}

#[derive(serde::Serialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
enum RunnerSecretTarget<'a> {
    FocusedField {
        exact_origin: &'a str,
        frame_origin: &'a str,
        field_id: &'a str,
    },
    CredentialBroker {
        exact_origin: &'a str,
        broker_id: &'a str,
    },
}

struct ZeroingString(String);

impl Drop for ZeroingString {
    fn drop(&mut self) {
        let mut bytes = std::mem::take(&mut self.0).into_bytes();
        bytes.fill(0);
    }
}

struct ZeroingBytes(Vec<u8>);

impl Drop for ZeroingBytes {
    fn drop(&mut self) {
        self.0.fill(0);
    }
}

fn encode_base64(input: &[u8]) -> String {
    const TABLE: &[u8; 64] = b"ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    let mut output = Vec::with_capacity(input.len().div_ceil(3) * 4);
    for chunk in input.chunks(3) {
        let first = chunk[0];
        let second = chunk.get(1).copied().unwrap_or_default();
        let third = chunk.get(2).copied().unwrap_or_default();
        output.push(TABLE[usize::from(first >> 2)]);
        output.push(TABLE[usize::from((first & 0x03) << 4 | second >> 4)]);
        output.push(if chunk.len() > 1 {
            TABLE[usize::from((second & 0x0f) << 2 | third >> 6)]
        } else {
            b'='
        });
        output.push(if chunk.len() > 2 {
            TABLE[usize::from(third & 0x3f)]
        } else {
            b'='
        });
    }
    String::from_utf8(output).expect("base64 alphabet is valid UTF-8")
}

fn directory_size(root: PathBuf) -> Result<u64, std::io::Error> {
    if !root.exists() {
        return Ok(0);
    }
    let mut total = 0_u64;
    let mut pending = vec![root];
    while let Some(path) = pending.pop() {
        for entry in fs::read_dir(path)? {
            let entry = entry?;
            let metadata = entry.metadata()?;
            if metadata.is_dir() {
                pending.push(entry.path());
            } else {
                total = total.saturating_add(metadata.len());
            }
        }
    }
    Ok(total)
}

impl<R> Drop for ComputerHost<R> {
    fn drop(&mut self) {
        if let Ok(running) = self.running.get_mut() {
            for runtime in running.values_mut() {
                let _ = runtime.runner.kill();
                let _ = runtime.xvfb.kill();
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::io::{BufRead, BufReader, Write};

    use keith_agent_types::{CURRENT_SCHEMA_VERSION, ComputerId};
    use tempfile::TempDir;

    use super::*;
    use crate::{ComputerState, InMemoryComputerRepository};

    #[test]
    fn runner_readiness_is_bound_to_the_exact_profile_display_and_endpoint() {
        let owner = ProfileId::new();
        let response = serde_json::json!({
            "request_id": null,
            "profile_id": owner,
            "outcome": {
                "status": "ready",
                "value": {
                    "control_endpoint": "127.0.0.1:32001",
                    "display": ":201",
                    "persistent_profile": true
                }
            }
        });
        assert!(validate_runner_readiness(&response, &owner, "127.0.0.1:32001", ":201").is_ok());
        assert!(validate_runner_readiness(&response, &owner, "127.0.0.1:32002", ":201").is_err());
        assert!(
            validate_runner_readiness(&response, &ProfileId::new(), "127.0.0.1:32001", ":201")
                .is_err()
        );
    }

    #[test]
    #[ignore = "requires real systemd user manager, Xvfb, Chromium, and keith-browser-runner"]
    fn host_process_real_headed_navigation_script_restart_and_profile_fencing() {
        let browser_runner_binary = PathBuf::from(
            std::env::var_os("KEITH_BROWSER_RUNNER")
                .expect("KEITH_BROWSER_RUNNER must name a real packaged runner"),
        );
        let chromium_binary = PathBuf::from(
            std::env::var_os("KEITH_CHROMIUM").expect("KEITH_CHROMIUM must name real Chromium"),
        );
        let xvfb_binary =
            PathBuf::from(std::env::var_os("KEITH_XVFB").expect("KEITH_XVFB must name real Xvfb"));
        let root = TempDir::new().unwrap();
        let profile_id = ProfileId::new();
        let record = ComputerRecord {
            version: CURRENT_SCHEMA_VERSION,
            computer_id: ComputerId::new(),
            owner_profile_id: profile_id.clone(),
            browser_profile_root: root.path().join("browser").to_string_lossy().into_owned(),
            screen_key: StableKey::parse("screen/live-1").unwrap(),
            state: ComputerState::Ready,
            control_state: ControlState::Idle,
            current_task_key: None,
            created_at: UtcTimestamp::UNIX_EPOCH,
            updated_at: UtcTimestamp::UNIX_EPOCH,
            revision: Revision::ZERO,
        };
        let repository = InMemoryComputerRepository::default();
        repository
            .transact(&[ComputerRepositoryBatch::InsertComputer(record.clone())])
            .unwrap();
        let host = ComputerHost::new(
            repository,
            ComputerHostConfig {
                browser_runner_binary,
                chromium_binary,
                xvfb_binary,
                systemd_run_binary: PathBuf::from("/usr/bin/systemd-run"),
                display_base: 200,
                control_port_base: 32_000,
                screen_width: 1_280,
                screen_height: 720,
                screen_depth: 24,
            },
        );
        let policy = ComputerBoundaryPolicy {
            allowed_origins: vec!["https://example.com".into()],
            writable_roots: vec![root.path().to_path_buf()],
            download_root: root.path().join("downloads"),
            allow_credentials: false,
            resources: crate::ComputerResourcePolicy {
                cpu_quota_percent: 100,
                max_memory_bytes: 1_073_741_824,
                max_processes: 64,
                max_disk_bytes: 1_073_741_824,
                max_download_bytes: 16_777_216,
                max_network_requests_per_minute: 60,
                idle_timeout_seconds: 300,
                crash_limit: 3,
                crash_window_seconds: 60,
            },
        };
        fs::create_dir_all(&policy.download_root).unwrap();
        host.start(&record, policy, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let mut running = host.running.lock().unwrap();
        let runner = running.get_mut(&profile_id).unwrap();
        let mut stdin = runner.runner.stdin.take().unwrap();
        let stdout = runner.runner.stdout.take().unwrap();
        let mut output = BufReader::new(stdout);
        let mut line = String::new();
        for (request_id, command) in [
            ("1", serde_json::json!({"kind":"ping"})),
            (
                "2",
                serde_json::json!({"kind":"navigate","url":"https://example.com"}),
            ),
            (
                "3",
                serde_json::json!({"kind":"evaluate","script":"document.title"}),
            ),
            ("4", serde_json::json!({"kind":"observe"})),
        ] {
            writeln!(stdin, "{}", serde_json::json!({"request_id":request_id,"profile_id":profile_id,"command":command})).unwrap();
            stdin.flush().unwrap();
            line.clear();
            output.read_line(&mut line).unwrap();
            assert!(line.len() <= 1_048_576);
            let response: serde_json::Value = serde_json::from_str(&line).unwrap();
            assert!(
                response.get("ok").and_then(serde_json::Value::as_bool) == Some(true)
                    || response.get("status").and_then(serde_json::Value::as_str) == Some("ok")
            );
        }
        drop(running);
        assert!(matches!(
            host.acquire_task(
                ComputerTaskRequest {
                    owner_profile_id: profile_id.clone(),
                    task_key: StableKey::parse("task/one").unwrap(),
                    actor: ComputerActor::Agent {
                        profile_id: profile_id.clone()
                    },
                    conflict: TaskConflictPolicy::Deny
                },
                UtcTimestamp::from_unix_millis(1)
            )
            .unwrap(),
            TaskAdmission::Acquired(_)
        ));
        assert!(matches!(
            host.acquire_task(
                ComputerTaskRequest {
                    owner_profile_id: profile_id.clone(),
                    task_key: StableKey::parse("task/two").unwrap(),
                    actor: ComputerActor::Routine {
                        profile_id: profile_id.clone()
                    },
                    conflict: TaskConflictPolicy::Queue
                },
                UtcTimestamp::from_unix_millis(2)
            )
            .unwrap(),
            TaskAdmission::Queued { position: 1 }
        ));
        assert!(matches!(
            host.acquire_task(
                ComputerTaskRequest {
                    owner_profile_id: profile_id.clone(),
                    task_key: StableKey::parse("task/forged").unwrap(),
                    actor: ComputerActor::Agent {
                        profile_id: ProfileId::new()
                    },
                    conflict: TaskConflictPolicy::Deny
                },
                UtcTimestamp::from_unix_millis(3)
            ),
            Err(ComputerHostError::Unauthorized(_))
        ));
        host.shutdown(&profile_id).unwrap();
        assert!(PathBuf::from(&record.browser_profile_root).exists());
    }
}
