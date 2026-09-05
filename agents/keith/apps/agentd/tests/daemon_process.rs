use std::collections::BTreeSet;
use std::fs;
use std::io::{ErrorKind, Read, Write};
use std::net::{SocketAddr, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Mutex, MutexGuard};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{
    CURRENT_PROTOCOL_VERSION, CURRENT_SCHEMA_VERSION, ClientId, CommandId, EntityId, ErrorCode,
    ProfileId, Revision, RootTreeId, SessionId, UtcTimestamp, WorkerId,
};
use keith_agent_web::{PlatformCompatibilityConfig, WebServer, WebServerConfig};
use keith_connection::{
    AgentTransport, FramedTransport, LocalStream, connect_local, set_local_read_timeout,
    set_local_write_timeout,
};
use keith_credentials::{
    CredentialOwner, CredentialRef, EncryptedCredentialStore, MasterKey, RestrictedMasterKeyStore,
    SecretValue,
};
use keith_daemon_core::RootManifest;
use keith_local_runtime::{LocalRuntimeLaunchConfig, RuntimeCredentialKeySource};
use keith_platform_contracts::{
    ActionRisk, ApprovalEnvelope, ApprovalId, ApprovalState, AuditCorrelationId, CancellationId,
    Capability, ExternalAction, ExternalEffect, ExternalPrincipalId, LifecycleState, RedactedText,
};
use keith_protocol::{
    AttachSession, ClientCommand, ClientHello, CommandEnvelope, CommandResult, CreateGoal,
    DaemonEvent, ForkSession, GoalLimits, IntegrationAvailabilityProjection, IntegrationCommand,
    IntegrationMutation, IntegrationOperation, IntegrationService, ResponsePayload, SessionFilter,
    SessionState, WireFormat, WireMessage,
};
use keith_runtime_api::{ActiveServiceOperation, ServiceControl, ServiceRegistration};
use keith_self_evolution::{
    DaemonRestartConsent, DaemonStaging, DaemonStagingPhase, EvolutionGuard, GateKind, GateResult,
    StagingRequest, ToolchainIdentity, WorkerImage, WorkerImageManifest,
};
use keith_state_store::{EmbeddedStore, FileBackupHook};
use keith_state_store_core::{
    AtomicStateRepository, Collection, RecordMutation, VersionedRecord, WritePrecondition,
};
use keith_worker_runtime::{WorkerRunState, read_registration, registration_path};

static DAEMON_PROCESS_TEST_LOCK: Mutex<()> = Mutex::new(());

fn serial_daemon_process_test() -> MutexGuard<'static, ()> {
    DAEMON_PROCESS_TEST_LOCK
        .lock()
        .unwrap_or_else(std::sync::PoisonError::into_inner)
}
#[cfg(unix)]
use nix::sys::signal::{Signal, kill};
#[cfg(unix)]
use nix::unistd::Pid;
use ring::signature::{Ed25519KeyPair, KeyPair};
use sha2::{Digest, Sha256};

fn write_manifest(
    data_root: &Path,
    root: &RootTreeId,
    session: &SessionId,
    profile_id: &ProfileId,
) {
    let directory = data_root.join("sessions").join(root.to_string());
    fs::create_dir_all(&directory).unwrap();
    let manifest = RootManifest {
        version: CURRENT_SCHEMA_VERSION,
        root_tree_id: root.clone(),
        root_session_id: session.clone(),
        profile_id: profile_id.clone(),
        title: Some(format!("root {root}")),
        state: SessionState::Dormant,
        updated_at: UtcTimestamp::UNIX_EPOCH,
    };
    fs::write(
        directory.join("manifest.json"),
        keith_agent_types::canonical_json_bytes(&manifest).unwrap(),
    )
    .unwrap();
    fs::write(
        directory.join("session.jsonl"),
        b"corrupt session state that lazy discovery must never load",
    )
    .unwrap();
}

fn seed_runtime_session(
    launch: &LocalRuntimeLaunchConfig,
    root: &RootTreeId,
    session: &SessionId,
) -> ProfileId {
    let runtime = launch
        .open_worker(root.clone(), WorkerId::new(), EntityId::new())
        .unwrap();
    let profile = runtime.registered_profiles().unwrap().remove(0);
    runtime
        .create_session_assigned(
            &profile.profile.id,
            &profile.profile.workspace_id,
            session.clone(),
            root.clone(),
            Some(format!("root {root}")),
        )
        .unwrap();
    profile.profile.id
}

fn seed_provider_credential(launch: &LocalRuntimeLaunchConfig) {
    let key = RestrictedMasterKeyStore::open(&launch.credential_root)
        .unwrap()
        .load_or_create()
        .unwrap();
    EncryptedCredentialStore::open(&launch.credential_root, key)
        .unwrap()
        .put(
            CredentialRef::new("default", CredentialOwner::Provider("openai".into())).unwrap(),
            SecretValue::new("unused-process-test-credential").unwrap(),
            UtcTimestamp::now().unwrap(),
        )
        .unwrap();
}

fn start_daemon(data_root: &Path, socket: &Path) -> Child {
    Command::new(env!("CARGO_BIN_EXE_agentd"))
        .arg("--data-root")
        .arg(data_root)
        .arg("--socket")
        .arg(socket)
        .arg("--worker-executable")
        .arg(env!("CARGO_BIN_EXE_keith-daemon-worker-host"))
        .arg("--idle-seconds")
        .arg("60")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::inherit())
        .spawn()
        .unwrap()
}

fn start_daemon_child(data_root: &Path, socket: &Path) -> Child {
    Command::new(env!("CARGO_BIN_EXE_agentd"))
        .arg("--data-root")
        .arg(data_root)
        .arg("--socket")
        .arg(socket)
        .arg("--worker-executable")
        .arg(env!("CARGO_BIN_EXE_keith-daemon-worker-host"))
        .arg("--idle-seconds")
        .arg("60")
        .env("KEITH_DAEMON_CHILD", "1")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::inherit())
        .spawn()
        .unwrap()
}

fn start_integration_daemon_child(data_root: &Path, socket: &Path, services: &[&str]) -> Child {
    let mut command = Command::new(env!("CARGO_BIN_EXE_agentd"));
    command
        .arg("--data-root")
        .arg(data_root)
        .arg("--socket")
        .arg(socket)
        .arg("--worker-executable")
        .arg(env!("CARGO_BIN_EXE_keith-daemon-worker-host"))
        .arg("--idle-seconds")
        .arg("60")
        .env("KEITH_DAEMON_CHILD", "1")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::inherit());
    for service in services {
        command.arg("--enable-service").arg(service);
    }
    command.spawn().unwrap()
}

fn connect_when_ready(socket: &Path) -> LocalStream {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Ok(stream) = connect_local(socket) {
            set_local_read_timeout(&stream, Some(Duration::from_secs(2))).unwrap();
            set_local_write_timeout(&stream, Some(Duration::from_secs(2))).unwrap();
            return stream;
        }
        assert!(
            Instant::now() < deadline,
            "daemon socket did not become ready"
        );
        thread::sleep(Duration::from_millis(10));
    }
}

fn connect_child_when_ready(socket: &Path, child: &mut Child) -> LocalStream {
    let deadline = Instant::now() + Duration::from_secs(120);
    loop {
        if let Some(status) = child.try_wait().unwrap() {
            panic!("daemon child exited before readiness with {status}");
        }
        if let Ok(stream) = connect_local(socket) {
            set_local_read_timeout(&stream, Some(Duration::from_secs(2))).unwrap();
            set_local_write_timeout(&stream, Some(Duration::from_secs(2))).unwrap();
            return stream;
        }
        assert!(
            Instant::now() < deadline,
            "daemon child remained alive without opening its socket"
        );
        thread::sleep(Duration::from_millis(10));
    }
}

fn open_connection(socket: &Path) -> (FramedTransport<LocalStream>, ClientId) {
    open_connection_with_timeout(socket, Duration::from_secs(2))
}

fn open_connection_with_timeout(
    socket: &Path,
    timeout: Duration,
) -> (FramedTransport<LocalStream>, ClientId) {
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Ok(stream) = connect_local(socket) {
            set_local_read_timeout(&stream, Some(Duration::from_millis(250))).unwrap();
            set_local_write_timeout(&stream, Some(Duration::from_millis(250))).unwrap();
            let mut transport = FramedTransport::new(stream, WireFormat::Json);
            let client_id = ClientId::new();
            let sent = transport.send(&WireMessage::ClientHello(ClientHello {
                protocol: CURRENT_PROTOCOL_VERSION,
                client_id: client_id.clone(),
                client_name: "daemon-process-test".into(),
                client_version: "1.0.0".into(),
                supported_features: BTreeSet::new(),
                resume: None,
            }));
            if sent.is_ok() && matches!(transport.receive(), Ok(WireMessage::ServerHello(_))) {
                let stream = transport.into_inner();
                set_local_read_timeout(&stream, Some(timeout)).unwrap();
                set_local_write_timeout(&stream, Some(timeout)).unwrap();
                return (FramedTransport::new(stream, WireFormat::Json), client_id);
            }
        }
        assert!(
            Instant::now() < deadline,
            "daemon protocol did not become ready"
        );
        thread::sleep(Duration::from_millis(10));
    }
}

fn execute(socket: &Path, command: ClientCommand) -> CommandResult {
    let (mut transport, client_id) = open_connection(socket);
    transport
        .send(&WireMessage::Command(CommandEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            client_id,
            sent_at: UtcTimestamp::UNIX_EPOCH,
            session_id: None,
            command,
        }))
        .unwrap();
    let WireMessage::CommandResult(result) = transport.receive().unwrap() else {
        panic!("daemon must return a command result");
    };
    result.result
}

fn execute_fork(socket: &Path, source_session_id: &SessionId) -> CommandResult {
    let (mut transport, client_id) = open_connection_with_timeout(socket, Duration::from_secs(120));
    transport
        .send(&WireMessage::Command(CommandEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            client_id,
            sent_at: UtcTimestamp::UNIX_EPOCH,
            session_id: Some(source_session_id.clone()),
            command: ClientCommand::ForkSession(ForkSession {
                source_session_id: source_session_id.clone(),
                title: Some("Independent process fork".into()),
            }),
        }))
        .unwrap();
    let WireMessage::CommandResult(result) = transport.receive().unwrap() else {
        panic!("daemon must return a fork command result");
    };
    result.result
}

fn integration_action(
    profile_id: &ProfileId,
    session_id: &SessionId,
    target: &str,
    capability: Capability,
    risk: ActionRisk,
    effect: ExternalEffect,
    cancellation_id: CancellationId,
) -> ExternalAction {
    let target = RedactedText::parse(target).unwrap();
    let target_digest = RedactedText::parse(format!("digest-{}", target.as_str())).unwrap();
    let now = UtcTimestamp::now().unwrap();
    let approval = if risk.is_consequential() {
        ApprovalState::Granted {
            approval_id: ApprovalId::new(),
            granted_by: ExternalPrincipalId::new(),
            exact_target_digest: target_digest.clone(),
            expires_at: UtcTimestamp::from_unix_millis(now.unix_millis().saturating_add(60_000)),
        }
    } else {
        ApprovalState::NotRequired
    };
    ExternalAction {
        profile_id: profile_id.clone(),
        session_id: session_id.clone(),
        acting_principal: ExternalPrincipalId::new(),
        requested_capability: capability,
        risk,
        approval: ApprovalEnvelope {
            risk,
            state: approval,
        },
        target,
        target_digest,
        cancellation_id,
        reply_route: None,
        audit_correlation: AuditCorrelationId::new(),
        external_effect: effect,
    }
}

fn integration_mutation(
    profile_id: &ProfileId,
    session_id: &SessionId,
    resource_id: Option<EntityId>,
    expected_revision: Option<Revision>,
    key: &str,
    idempotency_key: &str,
    operation: IntegrationOperation,
    cancellation_id: CancellationId,
) -> IntegrationMutation {
    let (capability, risk, effect) = match operation {
        IntegrationOperation::Connect => (
            Capability::AccountChange,
            ActionRisk::AccountChange,
            ExternalEffect::NonRepeatable,
        ),
        IntegrationOperation::Cancel => (
            Capability::LocalWrite,
            ActionRisk::ReversibleLocalWrite,
            ExternalEffect::Idempotent {
                delivery_key: RedactedText::parse(idempotency_key).unwrap(),
            },
        ),
        IntegrationOperation::Delete => (
            Capability::Delete,
            ActionRisk::Delete,
            ExternalEffect::NonRepeatable,
        ),
        _ => panic!("process helper supports connect, cancel, and delete"),
    };
    IntegrationMutation {
        profile_id: profile_id.clone(),
        service: IntegrationService::ChannelAccount,
        resource_id,
        native_resource_key: key.into(),
        display_label: format!("Channel {key}"),
        expected_revision,
        idempotency_key: idempotency_key.into(),
        operation,
        authority: integration_action(
            profile_id,
            session_id,
            key,
            capability,
            risk,
            effect,
            cancellation_id,
        ),
    }
}

fn mark_integration_active(data_root: &Path, resource_id: &EntityId) {
    let store =
        EmbeddedStore::open(&data_root.join("state.sqlite"), Some(&FileBackupHook)).unwrap();
    let record = store
        .get_record(Collection::ChannelAccounts, resource_id)
        .unwrap()
        .unwrap();
    let mut registration: ServiceRegistration =
        serde_json::from_value(record.payload.clone()).unwrap();
    let registration_revision = registration.revision.checked_next().unwrap();
    registration.lifecycle = LifecycleState::Active;
    registration.revision = registration_revision;
    registration.updated_at = UtcTimestamp::now().unwrap();
    registration.controls.insert(ServiceControl::Cancel);
    registration.controls.remove(&ServiceControl::Restart);

    let operation_record = store
        .list_records(Collection::IntegrationOperations)
        .unwrap()
        .into_iter()
        .find(|record| {
            serde_json::from_value::<ActiveServiceOperation>(record.payload.clone())
                .is_ok_and(|operation| operation.registration_id == *resource_id)
        })
        .unwrap();
    let mut operation: ActiveServiceOperation =
        serde_json::from_value(operation_record.payload.clone()).unwrap();
    let operation_revision = operation_record.revision.checked_next().unwrap();
    operation.lifecycle = LifecycleState::Active;
    operation.attempt = 1;
    operation.updated_at = UtcTimestamp::now().unwrap();

    store
        .transact(&[
            RecordMutation::Put {
                collection: Collection::ChannelAccounts,
                record: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: resource_id.clone(),
                    revision: registration_revision,
                    updated_at: registration.updated_at,
                    payload: serde_json::to_value(registration).unwrap(),
                },
                precondition: WritePrecondition::Exact(record.revision),
            },
            RecordMutation::Put {
                collection: Collection::IntegrationOperations,
                record: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: operation_record.id,
                    revision: operation_revision,
                    updated_at: operation.updated_at,
                    payload: serde_json::to_value(operation).unwrap(),
                },
                precondition: WritePrecondition::Exact(operation_record.revision),
            },
        ])
        .unwrap();
}

#[test]
fn daemon_process_forks_a_session_into_an_independent_leased_root() {
    let _serial = serial_daemon_process_test();
    let directory = tempfile::tempdir().unwrap();
    let data_root = directory.path().join("data");
    let socket = directory.path().join("agentd.sock");
    let workspace = directory.path().join("workspace");
    fs::create_dir_all(&workspace).unwrap();
    let source_root = RootTreeId::new();
    let source_session = SessionId::new();
    let launch = LocalRuntimeLaunchConfig {
        data_root: data_root.clone(),
        credential_root: data_root.join("credentials"),
        credential_key_source: RuntimeCredentialKeySource::Restricted(
            data_root.join("credentials"),
        ),
        workspace_root: workspace,
        openai_base_url: "http://127.0.0.1:1".into(),
        anthropic_base_url: "http://127.0.0.1:1".into(),
        provider_base_urls: std::collections::BTreeMap::new(),
    };
    seed_provider_credential(&launch);
    let source_profile = seed_runtime_session(&launch, &source_root, &source_session);
    write_manifest(&data_root, &source_root, &source_session, &source_profile);

    let mut daemon = start_daemon_child(&data_root, &socket);
    drop(connect_child_when_ready(&socket, &mut daemon));
    let CommandResult::Data(payload) = execute_fork(&socket, &source_session) else {
        panic!("real daemon and worker must create the fork");
    };
    let ResponsePayload::Snapshot(snapshot) = *payload else {
        panic!("fork must return the authoritative target snapshot");
    };
    assert_ne!(snapshot.session.session_id, source_session);
    assert_ne!(snapshot.session.root_tree_id, source_root);
    assert_eq!(snapshot.session.profile_id, source_profile);
    assert_eq!(snapshot.messages.len(), 0);
    let fork_worker = wait_for_worker(&data_root, &snapshot.session.root_tree_id);
    assert!(process_is_alive(fork_worker));

    let CommandResult::Data(payload) = execute(
        &socket,
        ClientCommand::ListSessions(SessionFilter::default()),
    ) else {
        panic!("forked catalog must remain listable");
    };
    let ResponsePayload::Sessions(sessions) = *payload else {
        panic!("catalog listing must return sessions");
    };
    assert_eq!(sessions.len(), 2);
    assert!(sessions.iter().any(|session| {
        session.session_id == snapshot.session.session_id
            && session.root_tree_id == snapshot.session.root_tree_id
    }));

    #[cfg(unix)]
    {
        send_signal(&mut daemon, Signal::SIGTERM);
        assert!(daemon.wait().unwrap().success());
    }
    #[cfg(windows)]
    {
        send_signal(&mut daemon, true);
        let _ = daemon.wait().unwrap();
        terminate_pid(fork_worker);
    }
}

#[cfg(unix)]
#[test]
#[allow(clippy::too_many_lines)]
fn daemon_process_integration_lifecycle_survives_crash_and_quarantines_corrupt_service() {
    let _serial = serial_daemon_process_test();
    let directory = tempfile::tempdir().unwrap();
    let data_root = directory.path().join("data");
    let socket = directory.path().join("agentd.sock");
    let workspace = directory.path().join("workspace");
    fs::create_dir_all(&workspace).unwrap();
    let root = RootTreeId::new();
    let session = SessionId::new();
    let launch = LocalRuntimeLaunchConfig {
        data_root: data_root.clone(),
        credential_root: data_root.join("credentials"),
        credential_key_source: RuntimeCredentialKeySource::Restricted(
            data_root.join("credentials"),
        ),
        workspace_root: workspace,
        openai_base_url: "http://127.0.0.1:1".into(),
        anthropic_base_url: "http://127.0.0.1:1".into(),
        provider_base_urls: std::collections::BTreeMap::new(),
    };
    seed_provider_credential(&launch);
    let profile = seed_runtime_session(&launch, &root, &session);
    write_manifest(&data_root, &root, &session, &profile);

    let mut daemon = start_integration_daemon_child(&data_root, &socket, &["channels"]);
    drop(connect_child_when_ready(&socket, &mut daemon));
    let primary_cancellation = CancellationId::new();
    let primary = integration_mutation(
        &profile,
        &session,
        None,
        None,
        "web/primary",
        "connect-primary",
        IntegrationOperation::Connect,
        primary_cancellation,
    );
    let CommandResult::Data(payload) = execute(
        &socket,
        ClientCommand::Integration(IntegrationCommand::Mutate(Box::new(primary.clone()))),
    ) else {
        panic!("enabled profile-scoped channel must be admitted");
    };
    let ResponsePayload::IntegrationResource(primary_resource) = *payload else {
        panic!("connect must return a durable integration resource");
    };
    assert_eq!(primary_resource.lifecycle, LifecycleState::Pending);
    let CommandResult::Data(replay_payload) = execute(
        &socket,
        ClientCommand::Integration(IntegrationCommand::Mutate(Box::new(primary))),
    ) else {
        panic!("idempotent replay must return the original resource");
    };
    let ResponsePayload::IntegrationResource(replayed) = *replay_payload else {
        panic!("idempotent replay must keep its response kind");
    };
    assert_eq!(replayed.id, primary_resource.id);

    let wrong_profile = ProfileId::new();
    let denied = integration_mutation(
        &wrong_profile,
        &session,
        None,
        None,
        "web/foreign",
        "connect-foreign",
        IntegrationOperation::Connect,
        CancellationId::new(),
    );
    let CommandResult::Rejected(error) = execute(
        &socket,
        ClientCommand::Integration(IntegrationCommand::Mutate(Box::new(denied))),
    ) else {
        panic!("a session must not select another profile");
    };
    assert_eq!(error.error.code, ErrorCode::Unauthorized);

    let uncertain_cancellation = CancellationId::new();
    let uncertain = integration_mutation(
        &profile,
        &session,
        None,
        None,
        "web/uncertain",
        "connect-uncertain",
        IntegrationOperation::Connect,
        uncertain_cancellation.clone(),
    );
    let CommandResult::Data(payload) = execute(
        &socket,
        ClientCommand::Integration(IntegrationCommand::Mutate(Box::new(uncertain))),
    ) else {
        panic!("second account must be admitted independently");
    };
    let ResponsePayload::IntegrationResource(uncertain_resource) = *payload else {
        panic!("connect must return the second durable resource");
    };

    send_signal(&mut daemon, Signal::SIGKILL);
    assert!(!daemon.wait().unwrap().success());
    mark_integration_active(&data_root, &uncertain_resource.id);
    let store =
        EmbeddedStore::open(&data_root.join("state.sqlite"), Some(&FileBackupHook)).unwrap();
    store
        .transact(&[RecordMutation::Put {
            collection: Collection::ConnectedApps,
            record: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: EntityId::new(),
                revision: Revision::ZERO,
                updated_at: UtcTimestamp::now().unwrap(),
                payload: serde_json::json!({"corrupt": true}),
            },
            precondition: WritePrecondition::Missing,
        }])
        .unwrap();
    drop(store);

    let mut restarted =
        start_integration_daemon_child(&data_root, &socket, &["channels", "connected_apps"]);
    drop(connect_child_when_ready(&socket, &mut restarted));
    let CommandResult::Data(payload) = execute(
        &socket,
        ClientCommand::Integration(IntegrationCommand::List {
            profile_id: profile.clone(),
            service: None,
        }),
    ) else {
        panic!("one corrupt service must not prevent the profile projection");
    };
    let ResponsePayload::ProfileIntegrations(projection) = *payload else {
        panic!("list must return the profile integration projection");
    };
    assert!(projection.services.iter().any(|service| {
        service.service == IntegrationService::ConnectedApp
            && matches!(
                service.availability,
                IntegrationAvailabilityProjection::Unavailable { .. }
            )
    }));
    let interrupted = projection
        .resources
        .iter()
        .find(|resource| resource.id == uncertain_resource.id)
        .unwrap();
    assert_eq!(interrupted.lifecycle, LifecycleState::Interrupted);
    assert!(interrupted.safe_error.is_some());
    assert!(matches!(
        execute(
            &socket,
            ClientCommand::ListSessions(SessionFilter::default())
        ),
        CommandResult::Data(_)
    ));

    let cancel = integration_mutation(
        &profile,
        &session,
        Some(interrupted.id.clone()),
        Some(interrupted.revision),
        &interrupted.native_resource_key,
        "cancel-uncertain",
        IntegrationOperation::Cancel,
        uncertain_cancellation,
    );
    let CommandResult::Data(payload) = execute(
        &socket,
        ClientCommand::Integration(IntegrationCommand::Mutate(Box::new(cancel))),
    ) else {
        panic!("exact cancellation identity must cancel the interrupted resource");
    };
    let ResponsePayload::IntegrationResource(cancelled) = *payload else {
        panic!("cancel must return the updated resource");
    };
    assert_eq!(cancelled.lifecycle, LifecycleState::Cancelled);
    let delete = integration_mutation(
        &profile,
        &session,
        Some(cancelled.id.clone()),
        Some(cancelled.revision),
        &cancelled.native_resource_key,
        "delete-uncertain",
        IntegrationOperation::Delete,
        CancellationId::new(),
    );
    let CommandResult::Data(payload) = execute(
        &socket,
        ClientCommand::Integration(IntegrationCommand::Mutate(Box::new(delete))),
    ) else {
        panic!("exact deletion must succeed");
    };
    let ResponsePayload::IntegrationDeletion(report) = *payload else {
        panic!("delete must return exact remnant reporting");
    };
    assert_eq!(report.remaining_records, 0);
    assert!(report.remaining_media_objects.is_none());
    assert!(report.retained_operation_records >= 3);
    assert!(report.retained_audit_records >= 3);
    assert!(report.retention_reason.is_some());

    send_signal(&mut restarted, Signal::SIGTERM);
    assert!(restarted.wait().unwrap().success());
}

fn wait_for_worker(data_root: &Path, root: &RootTreeId) -> u32 {
    let path = registration_path(&data_root.join("runtime"), root);
    let deadline = Instant::now() + Duration::from_secs(5);
    loop {
        if let Ok(registration) = read_registration(&path)
            && registration.state == WorkerRunState::Ready
        {
            return registration.pid;
        }
        assert!(Instant::now() < deadline, "worker did not become ready");
        thread::sleep(Duration::from_millis(10));
    }
}

async fn http_request(address: SocketAddr, request: Vec<u8>) -> String {
    tokio::task::spawn_blocking(move || {
        let mut stream = TcpStream::connect(address).unwrap();
        stream.write_all(&request).unwrap();
        let mut response = String::new();
        stream.read_to_string(&mut response).unwrap();
        response
    })
    .await
    .unwrap()
}

async fn http_stream_prefix(address: SocketAddr, request: Vec<u8>, marker: &str) -> String {
    let marker = marker.to_owned();
    tokio::task::spawn_blocking(move || {
        let mut stream = TcpStream::connect(address).unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(3)))
            .unwrap();
        stream.write_all(&request).unwrap();
        let mut response = Vec::new();
        let mut chunk = [0_u8; 4096];
        loop {
            match stream.read(&mut chunk) {
                Ok(0) => break,
                Ok(read) => {
                    response.extend_from_slice(&chunk[..read]);
                    if String::from_utf8_lossy(&response).contains(&marker) {
                        break;
                    }
                }
                Err(error)
                    if matches!(error.kind(), ErrorKind::WouldBlock | ErrorKind::TimedOut) =>
                {
                    break;
                }
                Err(error) => panic!("event stream read failed: {error}"),
            }
        }
        String::from_utf8(response).unwrap()
    })
    .await
    .unwrap()
}

#[cfg(unix)]
fn send_signal(process: &mut Child, signal: Signal) {
    kill(Pid::from_raw(i32::try_from(process.id()).unwrap()), signal).unwrap();
}

#[cfg(windows)]
fn send_signal(process: &mut Child, _force: bool) {
    process.kill().unwrap();
}

#[cfg(unix)]
fn process_is_alive(pid: u32) -> bool {
    let Ok(pid) = i32::try_from(pid) else {
        return false;
    };
    #[cfg(target_os = "linux")]
    if fs::read_to_string(Path::new("/proc").join(pid.to_string()).join("stat"))
        .ok()
        .and_then(|stat| stat.split_whitespace().nth(2).map(str::to_owned))
        .is_some_and(|state| state == "Z")
    {
        return false;
    }
    kill(Pid::from_raw(pid), None).is_ok()
}

#[cfg(windows)]
fn process_is_alive(pid: u32) -> bool {
    let output = Command::new("tasklist")
        .args(["/FI", &format!("PID eq {pid}"), "/FO", "CSV", "/NH"])
        .output()
        .unwrap();
    String::from_utf8_lossy(&output.stdout).lines().any(|line| {
        line.split(',')
            .nth(1)
            .is_some_and(|value| value.trim_matches('"') == pid.to_string())
    })
}

#[cfg(unix)]
fn terminate_pid(pid: u32) {
    kill(Pid::from_raw(i32::try_from(pid).unwrap()), Signal::SIGKILL).unwrap();
}

#[cfg(windows)]
fn terminate_pid(pid: u32) {
    assert!(
        Command::new("taskkill")
            .args(["/PID", &pid.to_string(), "/F"])
            .status()
            .unwrap()
            .success()
    );
}

#[test]
fn daemon_process_is_lazy_contains_crashes_and_adopts_after_restart() {
    let _serial = serial_daemon_process_test();
    let directory = tempfile::tempdir().unwrap();
    let data_root = directory.path().join("data");
    let socket = directory.path().join("agentd.sock");
    let first_root = RootTreeId::new();
    let first_session = SessionId::new();
    let second_root = RootTreeId::new();
    let second_session = SessionId::new();
    let launch = LocalRuntimeLaunchConfig {
        data_root: data_root.clone(),
        credential_root: data_root.join("credentials"),
        credential_key_source: RuntimeCredentialKeySource::Restricted(
            data_root.join("credentials"),
        ),
        workspace_root: directory.path().join("workspace"),
        openai_base_url: "http://127.0.0.1:1".into(),
        anthropic_base_url: "http://127.0.0.1:1".into(),
        provider_base_urls: std::collections::BTreeMap::new(),
    };
    seed_provider_credential(&launch);
    let first_profile = seed_runtime_session(&launch, &first_root, &first_session);
    let second_profile = seed_runtime_session(&launch, &second_root, &second_session);
    assert_eq!(first_profile, second_profile);
    write_manifest(&data_root, &first_root, &first_session, &first_profile);
    write_manifest(&data_root, &second_root, &second_session, &second_profile);

    let mut daemon = start_daemon(&data_root, &socket);
    let _idle_connection = open_connection(&socket);
    let listed = execute(
        &socket,
        ClientCommand::ListSessions(SessionFilter::default()),
    );
    let CommandResult::Data(payload) = listed else {
        panic!("catalog listing must return data");
    };
    let ResponsePayload::Sessions(sessions) = *payload else {
        panic!("catalog listing must return sessions");
    };
    assert_eq!(sessions.len(), 2);
    assert!(!data_root.join("runtime/workers").exists());

    for session in [&first_session, &second_session] {
        assert!(matches!(
            execute(
                &socket,
                ClientCommand::AttachSession(AttachSession {
                    session_id: session.clone(),
                    resume: None,
                })
            ),
            CommandResult::Data(_)
        ));
    }
    let first_pid = wait_for_worker(&data_root, &first_root);
    let second_pid = wait_for_worker(&data_root, &second_root);

    terminate_pid(first_pid);
    thread::sleep(Duration::from_millis(150));
    assert!(matches!(
        execute(
            &socket,
            ClientCommand::ListSessions(SessionFilter::default())
        ),
        CommandResult::Data(_)
    ));
    assert!(daemon.try_wait().unwrap().is_none());
    assert!(process_is_alive(second_pid));

    #[cfg(unix)]
    send_signal(&mut daemon, Signal::SIGKILL);
    #[cfg(windows)]
    send_signal(&mut daemon, true);
    assert!(!daemon.wait().unwrap().success());
    assert!(process_is_alive(second_pid));

    let mut restarted = start_daemon(&data_root, &socket);
    assert!(matches!(
        execute(
            &socket,
            ClientCommand::ListSessions(SessionFilter::default())
        ),
        CommandResult::Data(_)
    ));
    assert!(process_is_alive(second_pid));

    #[cfg(unix)]
    {
        send_signal(&mut restarted, Signal::SIGTERM);
        assert!(restarted.wait().unwrap().success());
    }
    #[cfg(windows)]
    {
        send_signal(&mut restarted, true);
        let _ = restarted.wait().unwrap();
        terminate_pid(second_pid);
    }
    let deadline = Instant::now() + Duration::from_secs(2);
    while process_is_alive(second_pid) && Instant::now() < deadline {
        thread::sleep(Duration::from_millis(10));
    }
    assert!(!process_is_alive(second_pid));
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[allow(clippy::too_many_lines)]
async fn native_platform_bridge_reaches_the_real_daemon_and_leased_worker() {
    let _serial = serial_daemon_process_test();
    let directory = tempfile::tempdir().unwrap();
    let data_root = directory.path().join("data");
    let socket = directory.path().join("agentd.sock");
    let workspace = directory.path().join("workspace");
    let root = RootTreeId::new();
    let requested_session = SessionId::new();
    let launch = LocalRuntimeLaunchConfig {
        data_root: data_root.clone(),
        credential_root: data_root.join("credentials"),
        credential_key_source: RuntimeCredentialKeySource::Restricted(
            data_root.join("credentials"),
        ),
        workspace_root: workspace,
        openai_base_url: "http://127.0.0.1:1".into(),
        anthropic_base_url: "http://127.0.0.1:1".into(),
        provider_base_urls: std::collections::BTreeMap::new(),
    };
    seed_provider_credential(&launch);
    let runtime = launch
        .open_worker(root.clone(), WorkerId::new(), EntityId::new())
        .unwrap();
    let registered = runtime.registered_profiles().unwrap().remove(0);
    let profile = registered.profile.id;
    let created = runtime
        .create_session_assigned(
            &profile,
            &registered.profile.workspace_id,
            requested_session,
            root.clone(),
            Some("Native bridge process proof".into()),
        )
        .unwrap();
    let _created_session = created.session_id;
    drop(runtime);
    let mut daemon = start_daemon(&data_root, &socket);
    let _connection = open_connection(&socket);

    let assets = directory.path().join("assets");
    fs::create_dir(&assets).unwrap();
    fs::write(assets.join("agent_web.js"), b"export default function(){}").unwrap();
    fs::write(assets.join("agent_web_bg.wasm"), b"\0asm\x01\0\0\0").unwrap();
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let platform_key = b"real-daemon-platform-bridge-key-0001";
    let web = WebServer::new(WebServerConfig {
        bind: address,
        exact_origin: format!("http://{address}"),
        daemon_socket: socket.clone(),
        asset_root: assets,
        credential_root: directory.path().join("web-credentials"),
        credential_key: MasterKey::from_bytes([0x42; 32]),
        login_secret: b"real-daemon-web-login-secret-0001".to_vec(),
        session_lifetime: Duration::from_secs(60),
        mutation_limit_per_second: 8,
        daemon_timeout: Duration::from_secs(2),
        openai_compatibility: None,
        platform_compatibility: Some(PlatformCompatibilityConfig {
            api_key: platform_key.to_vec(),
            allow_non_loopback: false,
            max_in_flight: 2,
        }),
    })
    .unwrap();
    let web_task = tokio::spawn(web.serve_listener(listener));

    let catalog = http_request(
        address,
        format!(
            "GET /platform/v1/catalog HTTP/1.1\r\nHost: {address}\r\nAuthorization: Bearer {}\r\nConnection: close\r\n\r\n",
            String::from_utf8_lossy(platform_key)
        )
        .into_bytes(),
    )
    .await;
    assert!(catalog.starts_with("HTTP/1.1 200 OK"), "{catalog}");
    assert!(catalog.contains(&profile.to_string()), "{catalog}");
    let (_, catalog_body) = catalog.split_once("\r\n\r\n").unwrap();
    let catalog_json: serde_json::Value = serde_json::from_str(catalog_body).unwrap();
    let session: SessionId = catalog_json["sessions"][0]["session_id"]
        .as_str()
        .unwrap()
        .parse()
        .unwrap();
    let session_root: RootTreeId = catalog_json["sessions"][0]["root_tree_id"]
        .as_str()
        .unwrap()
        .parse()
        .unwrap();

    let body = serde_json::to_vec(&serde_json::json!({
        "session_id": session.clone(),
        "command": ClientCommand::AttachSession(AttachSession {
            session_id: session.clone(),
            resume: None,
        }),
    }))
    .unwrap();
    let mut command = format!(
        "POST /platform/v1/profiles/{profile}/commands HTTP/1.1\r\nHost: {address}\r\nAuthorization: Bearer {}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        String::from_utf8_lossy(platform_key),
        body.len()
    )
    .into_bytes();
    command.extend_from_slice(&body);
    let attached = http_request(address, command).await;
    assert!(attached.starts_with("HTTP/1.1 200 OK"), "{attached}");
    let (_, attached_body) = attached.split_once("\r\n\r\n").unwrap();
    let attached_json: serde_json::Value = serde_json::from_str(attached_body).unwrap();
    let generation = attached_json["result"]["payload"]["value"]["generation"]
        .as_u64()
        .unwrap();
    let worker_pid = wait_for_worker(&data_root, &session_root);
    assert!(process_is_alive(worker_pid));

    let goal_body = serde_json::to_vec(&serde_json::json!({
        "session_id": session.clone(),
        "command": ClientCommand::CreateGoal(CreateGoal {
            session_id: session.clone(),
            objective: "Prove native replay".into(),
            limits: GoalLimits {
                max_turns: Some(1),
                max_tokens: Some(64),
                deadline: None,
            },
        }),
    }))
    .unwrap();
    let mut goal_command = format!(
        "POST /platform/v1/profiles/{profile}/commands HTTP/1.1\r\nHost: {address}\r\nAuthorization: Bearer {}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        String::from_utf8_lossy(platform_key),
        goal_body.len()
    )
    .into_bytes();
    goal_command.extend_from_slice(&goal_body);
    let goal = http_request(address, goal_command).await;
    assert!(goal.starts_with("HTTP/1.1 200 OK"), "{goal}");

    let wrong_profile = ProfileId::new();
    let mut denied = format!(
        "POST /platform/v1/profiles/{wrong_profile}/commands HTTP/1.1\r\nHost: {address}\r\nAuthorization: Bearer {}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        String::from_utf8_lossy(platform_key),
        body.len()
    )
    .into_bytes();
    denied.extend_from_slice(&body);
    let denied = http_request(address, denied).await;
    assert!(denied.starts_with("HTTP/1.1 403 Forbidden"), "{denied}");
    assert!(denied.contains("scope_denied"), "{denied}");

    let replay = http_stream_prefix(
        address,
        format!(
            "GET /platform/v1/events/{profile}/{session}?generation={generation}&sequence=0 HTTP/1.1\r\nHost: {address}\r\nAuthorization: Bearer {}\r\nAccept: text/event-stream\r\nConnection: close\r\n\r\n",
            String::from_utf8_lossy(platform_key)
        )
        .into_bytes(),
        "\"message\":\"snapshot\"",
    )
    .await;
    assert!(replay.starts_with("HTTP/1.1 200 OK"), "{replay}");
    assert!(replay.contains("text/event-stream"), "{replay}");
    assert!(replay.contains("\"message\":\"snapshot\""), "{replay}");
    assert!(replay.contains("Prove native replay"), "{replay}");

    web_task.abort();
    let _ = web_task.await;
    #[cfg(unix)]
    {
        send_signal(&mut daemon, Signal::SIGTERM);
        assert!(daemon.wait().unwrap().success());
    }
    #[cfg(windows)]
    {
        send_signal(&mut daemon, true);
        let _ = daemon.wait().unwrap();
        terminate_pid(worker_pid);
    }
}

#[cfg(unix)]
fn signed_daemon_image(executable: &[u8], build_id: &str) -> (WorkerImage, [u8; 32]) {
    let output = "real verification gate exited successfully";
    let gates = [
        GateKind::Formatting,
        GateKind::StrictClippy,
        GateKind::WorkspaceTests,
        GateKind::DependencyPolicy,
        GateKind::Security,
        GateKind::Platform,
    ]
    .into_iter()
    .map(|gate| GateResult {
        gate,
        exit_code: 0,
        elapsed_millis: 1,
        output: output.into(),
        output_sha256: sha256(output.as_bytes()),
        sandbox: keith_sandbox::SandboxStatus::detect(),
    })
    .collect();
    let manifest = WorkerImageManifest {
        format: "keith-worker-image-v1".into(),
        build_id: build_id.into(),
        base_revision: "a".repeat(40),
        source_manifest_sha256: sha256(build_id.as_bytes()),
        executable_sha256: sha256(executable),
        executable_bytes: u64::try_from(executable.len()).unwrap(),
        toolchain: ToolchainIdentity {
            rustc: "rustc process-test".into(),
            cargo: "cargo process-test".into(),
            target: std::env::consts::ARCH.into(),
        },
        worker_report: keith_build_info::BuildReport {
            component: "daemon".into(),
            package_version: env!("CARGO_PKG_VERSION").into(),
            build_id: build_id.into(),
            protocol_version: CURRENT_PROTOCOL_VERSION.to_string(),
            storage_schema: CURRENT_SCHEMA_VERSION.to_string(),
            enabled_features: BTreeSet::from(["supervised_restart".into()]),
        },
        gates,
        artifact_source_paths: vec![PathBuf::from("apps/agentd/src/main.rs")],
        change_class: "c".into(),
    };
    let key = Ed25519KeyPair::from_seed_unchecked(&[73; 32]).unwrap();
    let public_key = key.public_key().as_ref().try_into().unwrap();
    let signature = key.sign(&keith_agent_types::canonical_json_bytes(&manifest).unwrap());
    let image = WorkerImage::from_signed_parts(
        manifest,
        executable.to_vec(),
        signature.as_ref().to_vec(),
        public_key,
        &public_key,
    )
    .unwrap();
    (image, public_key)
}

#[cfg(unix)]
fn stage_daemon(
    data_root: &Path,
    executable: &Path,
    build_id: &str,
) -> keith_self_evolution::StagedDaemonImage {
    let bytes = fs::read(executable).unwrap();
    let (image, public_key) = signed_daemon_image(&bytes, build_id);
    let staging_root = data_root.join("self-evolution/daemon-images");
    let guard = EvolutionGuard::new(data_root).unwrap();
    let mut staging = DaemonStaging::open(&staging_root, executable).unwrap();
    staging
        .stage(StagingRequest {
            image: &image,
            trusted_public_key: &public_key,
            consent: &DaemonRestartConsent {
                owner_identity: "installation-owner-process-test".into(),
                restart_required: true,
                affected_scope: vec!["Keith daemon, catalog, and local endpoint".into()],
                reversal_path: "Restore the pinned known-good daemon image".into(),
            },
            guard: &guard,
        })
        .unwrap()
}

#[cfg(unix)]
fn start_daemon_with_fault(
    data_root: &Path,
    socket: &Path,
    boundary: Option<&str>,
    image_id: Option<&str>,
) -> Child {
    let mut command = Command::new(env!("CARGO_BIN_EXE_agentd"));
    command
        .arg("--data-root")
        .arg(data_root)
        .arg("--socket")
        .arg(socket)
        .arg("--worker-executable")
        .arg(env!("CARGO_BIN_EXE_keith-daemon-worker-host"))
        .arg("--idle-seconds")
        .arg("60")
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::inherit());
    if let (Some(boundary), Some(image_id)) = (boundary, image_id) {
        command
            .env("KEITH_DAEMON_STARTUP_FAIL_AT", boundary)
            .env("KEITH_DAEMON_STARTUP_FAIL_IMAGE", image_id);
    }
    command.spawn().unwrap()
}

#[cfg(unix)]
fn stop_launcher(process: &mut Child) {
    send_signal(process, Signal::SIGTERM);
    assert!(process.wait().unwrap().success());
}

#[cfg(unix)]
fn attach_and_observe_restoration(socket: &Path, session_id: &SessionId) {
    let (mut transport, client_id) = open_connection_with_timeout(socket, Duration::from_secs(120));
    transport
        .send(&WireMessage::Command(CommandEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            client_id,
            sent_at: UtcTimestamp::UNIX_EPOCH,
            session_id: Some(session_id.clone()),
            command: ClientCommand::AttachSession(AttachSession {
                session_id: session_id.clone(),
                resume: None,
            }),
        }))
        .unwrap();
    let mut result = false;
    let mut warning = false;
    for _ in 0..4 {
        match transport.receive().unwrap() {
            WireMessage::CommandResult(envelope) => {
                assert!(matches!(envelope.result, CommandResult::Data(_)));
                result = true;
            }
            WireMessage::Event(envelope) => {
                if let DaemonEvent::Warning(error) = envelope.event {
                    assert!(error.message.contains("restored the previous daemon"));
                    warning = true;
                }
            }
            _ => {}
        }
        if result && warning {
            break;
        }
    }
    assert!(result && warning);
}

#[cfg(unix)]
fn sha256(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

#[cfg(unix)]
fn wait_for_staging_phase(root: &Path, expected: DaemonStagingPhase) {
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        let phase = fs::read(root.join("staging.json"))
            .ok()
            .and_then(|bytes| serde_json::from_slice::<serde_json::Value>(&bytes).ok())
            .and_then(|state| state["staged"]["phase"].as_str().map(str::to_owned));
        if phase.as_deref()
            == Some(match expected {
                DaemonStagingPhase::Staged => "staged",
                DaemonStagingPhase::Launching => "launching",
                DaemonStagingPhase::Active => "active",
                DaemonStagingPhase::Restored => "restored",
            })
        {
            return;
        }
        assert!(
            Instant::now() < deadline,
            "staging phase did not reach {expected:?}; last phase was {phase:?}"
        );
        thread::sleep(Duration::from_millis(10));
    }
}

#[cfg(unix)]
#[test]
fn staged_daemon_real_process_success_failures_restore_and_restart_stably() {
    let _serial = serial_daemon_process_test();
    let directory = tempfile::tempdir().unwrap();
    let data_root = directory.path().join("data");
    let socket = directory.path().join("agentd.sock");
    let workspace = directory.path().join("workspace");
    fs::create_dir_all(&workspace).unwrap();
    let root = RootTreeId::new();
    let session = SessionId::new();
    let launch = LocalRuntimeLaunchConfig {
        data_root: data_root.clone(),
        credential_root: data_root.join("credentials"),
        credential_key_source: RuntimeCredentialKeySource::Restricted(
            data_root.join("credentials"),
        ),
        workspace_root: workspace,
        openai_base_url: "http://127.0.0.1:1".into(),
        anthropic_base_url: "http://127.0.0.1:1".into(),
        provider_base_urls: std::collections::BTreeMap::new(),
    };
    seed_provider_credential(&launch);
    let profile = seed_runtime_session(&launch, &root, &session);
    write_manifest(&data_root, &root, &session, &profile);

    let executable = Path::new(env!("CARGO_BIN_EXE_agentd"));
    let successful = stage_daemon(&data_root, executable, "daemon-success");
    let mut daemon = start_daemon_with_fault(&data_root, &socket, None, None);
    drop(connect_when_ready(&socket));
    let staging_root = data_root.join("self-evolution/daemon-images");
    wait_for_staging_phase(&staging_root, DaemonStagingPhase::Active);
    stop_launcher(&mut daemon);
    let staging = DaemonStaging::open(&staging_root, executable).unwrap();
    assert_eq!(
        staging.staged().unwrap().unwrap().phase,
        DaemonStagingPhase::Active
    );
    drop(staging);

    let mut repeated = start_daemon_with_fault(&data_root, &socket, None, None);
    drop(connect_when_ready(&socket));
    stop_launcher(&mut repeated);

    let migration = stage_daemon(&data_root, executable, "daemon-migration-failure");
    let mut restored = start_daemon_with_fault(
        &data_root,
        &socket,
        Some("migration"),
        Some(&migration.candidate.image_id),
    );
    drop(connect_when_ready(&socket));
    attach_and_observe_restoration(&socket, &session);
    stop_launcher(&mut restored);
    let staging = DaemonStaging::open(&staging_root, executable).unwrap();
    let migration_state = staging.staged().unwrap().unwrap();
    assert_eq!(migration_state.phase, DaemonStagingPhase::Restored);
    assert_eq!(
        migration_state.pinned.image_id,
        successful.candidate.image_id
    );
    assert!(migration_state.candidate.executable.is_file());
    drop(staging);

    let mut stable_restore = start_daemon_with_fault(&data_root, &socket, None, None);
    drop(connect_when_ready(&socket));
    stop_launcher(&mut stable_restore);

    let readiness = stage_daemon(&data_root, executable, "daemon-readiness-failure");
    let mut readiness_restored = start_daemon_with_fault(
        &data_root,
        &socket,
        Some("readiness"),
        Some(&readiness.candidate.image_id),
    );
    drop(connect_when_ready(&socket));
    stop_launcher(&mut readiness_restored);
    let staging = DaemonStaging::open(&staging_root, executable).unwrap();
    let readiness_state = staging.staged().unwrap().unwrap();
    assert_eq!(readiness_state.phase, DaemonStagingPhase::Restored);
    assert_eq!(
        readiness_state.pinned.image_id,
        successful.candidate.image_id
    );
    assert!(readiness_state.candidate.executable.is_file());
}
