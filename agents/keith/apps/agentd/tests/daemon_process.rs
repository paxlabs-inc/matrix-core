use std::collections::BTreeSet;
use std::fs;
use std::io::{ErrorKind, Read, Write};
use std::net::{SocketAddr, TcpListener, TcpStream};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Mutex, MutexGuard, OnceLock};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{
    CURRENT_PROTOCOL_VERSION, CURRENT_SCHEMA_VERSION, ClientId, CommandId, EntityId, ProfileId,
    RootTreeId, SessionId, StableKey, UtcTimestamp, WorkerId,
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
use keith_protocol::{
    AgentLifecycleCommand, AttachSession, ClientCommand, ClientHello, CommandEnvelope,
    CommandResult, ConversationCommand, ConversationMembershipAction,
    ConversationMembershipRequest, ConversationPageRequest, ConversationParticipantPrincipal,
    ConversationParticipantRole, CreateGoal, CreateGroupCommand, DaemonEvent, DeliveryPolicy,
    GoalLimits, GroupMentionModeCommand, ProfileRevisionCommand, ResponsePayload, SessionFilter,
    SessionState, SubmitPrompt, TeammatesCommand, WireFormat, WireMessage,
};
use keith_self_evolution::{
    DaemonRestartConsent, DaemonStaging, DaemonStagingPhase, EvolutionGuard, GateKind, GateResult,
    StagingRequest, ToolchainIdentity, WorkerImage, WorkerImageManifest,
};
use keith_worker_runtime::{WorkerRunState, read_registration, registration_path};

const DAEMON_COMMAND_TIMEOUT: Duration = Duration::from_secs(10);

fn process_test_guard() -> MutexGuard<'static, ()> {
    static PROCESS_TEST_LOCK: OnceLock<Mutex<()>> = OnceLock::new();
    PROCESS_TEST_LOCK
        .get_or_init(|| Mutex::new(()))
        .lock()
        .unwrap_or_else(|poisoned| poisoned.into_inner())
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
        session_aliases: Vec::new(),
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
    start_daemon_with_runtime(data_root, socket, None)
}

fn start_daemon_with_runtime(
    data_root: &Path,
    socket: &Path,
    runtime: Option<&LocalRuntimeLaunchConfig>,
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
        .arg("60");
    if let Some(runtime) = runtime {
        command
            .arg("--credential-root")
            .arg(&runtime.credential_root)
            .arg("--workspace-root")
            .arg(&runtime.workspace_root)
            .arg("--openai-base-url")
            .arg(&runtime.openai_base_url)
            .arg("--anthropic-base-url")
            .arg(&runtime.anthropic_base_url);
        for (provider, base_url) in &runtime.provider_base_urls {
            command
                .arg("--provider-base-url")
                .arg(format!("{provider}={base_url}"));
        }
    }
    command
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::inherit())
        .spawn()
        .unwrap()
}

fn connect_when_ready(socket: &Path) -> LocalStream {
    // Debug daemon and worker images are each hundreds of MiB. First launch durably publishes and
    // fsyncs both before exposing the socket; four process tests can concurrently write roughly
    // four GiB. Budget 120 seconds for that real publication without weakening command timeouts.
    let deadline = Instant::now() + Duration::from_secs(120);
    loop {
        if let Ok(stream) = connect_local(socket) {
            // Session attachment synchronously starts a worker (whose contract permits five
            // seconds), authenticates its control channel, and restores its snapshot.
            set_local_read_timeout(&stream, Some(DAEMON_COMMAND_TIMEOUT)).unwrap();
            set_local_write_timeout(&stream, Some(DAEMON_COMMAND_TIMEOUT)).unwrap();
            return stream;
        }
        assert!(
            Instant::now() < deadline,
            "daemon socket did not become ready"
        );
        thread::sleep(Duration::from_millis(10));
    }
}

fn open_connection(socket: &Path) -> (FramedTransport<LocalStream>, ClientId) {
    let deadline = Instant::now() + DAEMON_COMMAND_TIMEOUT;
    loop {
        let mut transport = FramedTransport::new(connect_when_ready(socket), WireFormat::Json);
        let client_id = ClientId::new();
        let hello = WireMessage::ClientHello(ClientHello {
            protocol: CURRENT_PROTOCOL_VERSION,
            client_id: client_id.clone(),
            client_name: "daemon-process-test".into(),
            client_version: "1.0.0".into(),
            supported_features: BTreeSet::new(),
            resume: None,
        });
        if transport.send(&hello).is_ok()
            && matches!(transport.receive(), Ok(WireMessage::ServerHello(_)))
        {
            return (transport, client_id);
        }
        assert!(
            Instant::now() < deadline,
            "daemon handshake did not become ready"
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

fn execute_on_connection(
    transport: &mut FramedTransport<LocalStream>,
    client_id: &ClientId,
    session_id: Option<SessionId>,
    command: ClientCommand,
) -> CommandResult {
    let command_id = CommandId::new();
    transport
        .send(&WireMessage::Command(CommandEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: command_id.clone(),
            client_id: client_id.clone(),
            sent_at: UtcTimestamp::UNIX_EPOCH,
            session_id,
            command,
        }))
        .unwrap();
    loop {
        if let WireMessage::CommandResult(result) = transport.receive().unwrap()
            && result.command_id == command_id
        {
            return result.result;
        }
    }
}

fn wait_for_worker(data_root: &Path, root: &RootTreeId) -> u32 {
    let path = registration_path(&data_root.join("runtime"), root);
    let deadline = Instant::now() + DAEMON_COMMAND_TIMEOUT;
    let mut last_registration = None;
    loop {
        if let Ok(registration) = read_registration(&path) {
            if registration.state == WorkerRunState::Ready {
                return registration.pid;
            }
            last_registration = Some(registration);
        }
        assert!(
            Instant::now() < deadline,
            "worker did not become ready; registration: {last_registration:?}"
        );
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
    // An adopted worker is reparented when the first daemon is killed. In minimal CI
    // containers PID 1 may retain the exited worker as a zombie indefinitely; signal 0 still
    // succeeds for that process-table entry even though no worker code can execute.
    if fs::read_to_string(format!("/proc/{pid}/stat"))
        .ok()
        .and_then(|stat| {
            stat.rsplit_once(") ")
                .map(|(_, suffix)| suffix.starts_with('Z'))
        })
        .unwrap_or(false)
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
    let _process_test_guard = process_test_guard();
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
    let second_start_identity = process_start_identity(second_pid);

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
    let deadline = Instant::now() + DAEMON_COMMAND_TIMEOUT;
    while process_matches(second_pid, second_start_identity.as_deref()) && Instant::now() < deadline
    {
        thread::sleep(Duration::from_millis(10));
    }
    assert!(!process_matches(
        second_pid,
        second_start_identity.as_deref()
    ));
}

#[cfg(unix)]
fn process_start_identity(pid: u32) -> Option<String> {
    let stat = fs::read_to_string(format!("/proc/{pid}/stat")).ok()?;
    let (_, fields) = stat.rsplit_once(") ")?;
    fields.split_whitespace().nth(19).map(str::to_owned)
}

#[cfg(windows)]
fn process_start_identity(_pid: u32) -> Option<String> {
    None
}

fn process_matches(pid: u32, identity: Option<&str>) -> bool {
    process_is_alive(pid) && process_start_identity(pid).as_deref() == identity
}

#[test]
fn real_workers_preserve_origin_and_quiesce_profile_close_across_roots() {
    let _process_test_guard = process_test_guard();
    let directory = tempfile::tempdir().unwrap();
    let data_root = directory.path().join("data");
    let socket = directory.path().join("agentd.sock");
    let provider = TcpListener::bind("127.0.0.1:0").unwrap();
    let provider_address = provider.local_addr().unwrap();
    let (provider_started_sender, provider_started_receiver) = std::sync::mpsc::channel();
    let (provider_release_sender, provider_release_receiver) = std::sync::mpsc::channel();
    let provider_thread = thread::spawn(move || {
        let (mut stream, _) = provider.accept().unwrap();
        stream
            .set_read_timeout(Some(Duration::from_secs(2)))
            .unwrap();
        let mut request = [0_u8; 16 * 1024];
        let _ = stream.read(&mut request);
        provider_started_sender.send(()).unwrap();
        provider_release_receiver.recv().unwrap();
        let _ = stream.write_all(
            b"HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\nConnection: close\r\n\r\n",
        );
    });

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
        openai_base_url: format!("http://{provider_address}"),
        anthropic_base_url: "http://127.0.0.1:1".into(),
        provider_base_urls: std::collections::BTreeMap::new(),
    };
    seed_provider_credential(&launch);
    let first_profile = seed_runtime_session(&launch, &first_root, &first_session);
    let second_profile = seed_runtime_session(&launch, &second_root, &second_session);
    assert_eq!(first_profile, second_profile);
    write_manifest(&data_root, &first_root, &first_session, &first_profile);
    write_manifest(&data_root, &second_root, &second_session, &second_profile);
    let (admin_root, prompt_root, prompt_session) = if first_root < second_root {
        (&first_root, &second_root, &second_session)
    } else {
        (&second_root, &first_root, &first_session)
    };
    let mut daemon = start_daemon_with_runtime(&data_root, &socket, Some(&launch));
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
    let admin_pid = wait_for_worker(&data_root, admin_root);
    let prompt_pid = wait_for_worker(&data_root, prompt_root);
    assert_ne!(admin_pid, prompt_pid);

    let roster = execute(
        &socket,
        ClientCommand::AgentLifecycle(AgentLifecycleCommand::List),
    );
    let CommandResult::Data(roster) = roster else {
        panic!("an unscoped authenticated owner must reach a worker");
    };
    let ResponsePayload::AgentRoster(roster) = *roster else {
        panic!("owner lifecycle list must return the authoritative roster");
    };
    let profile_revision = roster
        .iter()
        .find(|entry| entry.profile_id == first_profile)
        .unwrap()
        .revision;

    let group = execute(
        &socket,
        ClientCommand::Conversation(ConversationCommand::Teammates(
            TeammatesCommand::CreateGroup(CreateGroupCommand {
                request_id: EntityId::new(),
                operation_key: StableKey::parse("process-origin-group").unwrap(),
                title: "Process origin verification".into(),
                initial_profile_ids: vec![first_profile.clone()],
                mention_mode: GroupMentionModeCommand::ExplicitOnly,
                now: UtcTimestamp::now().unwrap(),
            }),
        )),
    );
    let CommandResult::Data(group) = group else {
        panic!("owner group creation must return data: {group:?}");
    };
    let ResponsePayload::TeammatesReceipt(group) = *group else {
        panic!("owner group creation must return a teammates receipt");
    };
    let conversation_id = group.conversation_id.unwrap();
    let conversation_revision = group.resulting_revision.unwrap();

    let joined_profile = ProfileId::new();
    let membership = execute(
        &socket,
        ClientCommand::Conversation(ConversationCommand::ChangeMembership(
            ConversationMembershipRequest {
                conversation_id: conversation_id.clone(),
                target: ConversationParticipantPrincipal::Agent(joined_profile),
                role: ConversationParticipantRole::Observer,
                action: ConversationMembershipAction::Join,
                expected_participant_revision: keith_agent_types::Revision::ZERO,
                expected_conversation_revision: conversation_revision,
                operation_key: "process-human-membership".into(),
            },
        )),
    );
    assert!(
        matches!(membership, CommandResult::Data(_)),
        "owner group membership change must return data: {membership:?}"
    );
    let page = execute(
        &socket,
        ClientCommand::Conversation(ConversationCommand::Page(ConversationPageRequest {
            conversation_id: conversation_id.clone(),
            after_sequence: 0,
            limit: 100,
        })),
    );
    let CommandResult::Data(page) = page else {
        panic!("owner conversation page must return data");
    };
    let ResponsePayload::Conversation(page) = *page else {
        panic!("owner conversation page must return a projection");
    };
    assert!(page.events.iter().any(|event| {
        event.provenance_source == "owner-authorized-runtime-command"
            && event.author == keith_protocol::ConversationPrincipalProjection::Human
    }));

    let (mut attached, attached_client) = open_connection(&socket);
    assert!(matches!(
        execute_on_connection(
            &mut attached,
            &attached_client,
            Some(prompt_session.clone()),
            ClientCommand::AttachSession(AttachSession {
                session_id: prompt_session.clone(),
                resume: None,
            }),
        ),
        CommandResult::Data(_)
    ));
    assert!(matches!(
        execute_on_connection(
            &mut attached,
            &attached_client,
            Some(prompt_session.clone()),
            ClientCommand::DetachSession {
                session_id: prompt_session.clone(),
            },
        ),
        CommandResult::Accepted { .. }
    ));
    assert!(matches!(
        execute_on_connection(
            &mut attached,
            &attached_client,
            None,
            ClientCommand::AgentLifecycle(AgentLifecycleCommand::List),
        ),
        CommandResult::Rejected(_)
    ));

    let prompt_socket = socket.clone();
    let prompt_session_for_turn = prompt_session.clone();
    let (prompt_result_sender, prompt_result_receiver) = std::sync::mpsc::channel();
    let prompt_thread = thread::spawn(move || {
        let (mut transport, client_id) = open_connection(&prompt_socket);
        assert!(matches!(
            execute_on_connection(
                &mut transport,
                &client_id,
                Some(prompt_session_for_turn.clone()),
                ClientCommand::AttachSession(AttachSession {
                    session_id: prompt_session_for_turn.clone(),
                    resume: None,
                }),
            ),
            CommandResult::Data(_)
        ));
        let result = execute_on_connection(
            &mut transport,
            &client_id,
            Some(prompt_session_for_turn.clone()),
            ClientCommand::SubmitPrompt(SubmitPrompt {
                session_id: prompt_session_for_turn,
                text: "remain active until the owner closes this profile".into(),
                artifacts: Vec::new(),
                delivery: DeliveryPolicy::Immediate,
                reply_route: None,
            }),
        );
        prompt_result_sender.send(result.clone()).unwrap();
        result
    });
    if let Err(error) = provider_started_receiver.recv_timeout(DAEMON_COMMAND_TIMEOUT) {
        panic!(
            "provider request did not start: {error:?}; prompt result: {:?}",
            prompt_result_receiver.try_recv()
        );
    }

    let close_socket = socket.clone();
    let close_profile = first_profile.clone();
    let (close_sender, close_receiver) = std::sync::mpsc::channel();
    let close_thread = thread::spawn(move || {
        let result = execute(
            &close_socket,
            ClientCommand::AgentLifecycle(AgentLifecycleCommand::Disable(ProfileRevisionCommand {
                profile_id: close_profile,
                expected_revision: profile_revision,
            })),
        );
        close_sender.send(result).unwrap();
    });
    assert!(
        close_receiver
            .recv_timeout(Duration::from_millis(150))
            .is_err(),
        "profile close must not complete while another worker owns an in-flight turn"
    );
    provider_release_sender.send(()).unwrap();
    let _prompt_result = prompt_thread.join().unwrap();
    assert!(matches!(
        close_receiver.recv_timeout(Duration::from_secs(5)).unwrap(),
        CommandResult::Data(_)
    ));
    close_thread.join().unwrap();
    provider_thread.join().unwrap();
    assert!(process_is_alive(admin_pid));
    assert!(process_is_alive(prompt_pid));

    #[cfg(unix)]
    {
        send_signal(&mut daemon, Signal::SIGTERM);
        assert!(daemon.wait().unwrap().success());
    }
    #[cfg(windows)]
    {
        send_signal(&mut daemon, true);
        let _ = daemon.wait().unwrap();
        terminate_pid(admin_pid);
        terminate_pid(prompt_pid);
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn native_platform_bridge_reaches_the_real_daemon_and_leased_worker() {
    let _process_test_guard = process_test_guard();
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
        daemon_timeout: DAEMON_COMMAND_TIMEOUT,
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
    let (mut transport, client_id) = open_connection(socket);
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
                    break;
                }
            }
            _ => {}
        }
    }
    assert!(result && warning);
}

#[cfg(unix)]
fn sha256(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

#[cfg(unix)]
#[test]
fn staged_daemon_real_process_success_failures_restore_and_restart_stably() {
    let _process_test_guard = process_test_guard();
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
    stop_launcher(&mut daemon);
    let staging_root = data_root.join("self-evolution/daemon-images");
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
