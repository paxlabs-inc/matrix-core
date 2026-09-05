use std::collections::BTreeSet;
use std::io::{BufRead, BufReader, BufWriter, Read, Write};
use std::net::TcpListener;
use std::path::Path;
use std::process::{Child, ChildStdin, ChildStdout, Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{ProfileId, SessionId, UtcTimestamp};
use keith_cua::{
    ActionAttempt, ActionTarget, ComputerAction, ComputerActionRequest, ComputerController,
    ComputerLifecycle, ComputerMode, ComputerResourceLimits, CreateComputerRequest,
    IsolationRequirement, MouseButton, NamedCredentialGrant, NetworkPolicy, Point,
    ProgressExpectation, RunnerCommand, RunnerResponse, ScreenConnectionState, ScreenQuality,
    SemanticTarget, Viewport, action_target_digest,
};
use keith_cua_runner::LinuxComputerRuntime;
use keith_platform_contracts::{
    ActionRisk, ApprovalEnvelope, ApprovalId, ApprovalState, AuditCorrelationId, AuthorityBoundary,
    CancellationId, Capability, CapabilityGrant, ControlOwner, ExternalAction, ExternalEffect,
    ExternalPrincipalId, RedactedText,
};
use tempfile::TempDir;

struct RunnerProcess {
    child: Child,
    input: BufWriter<ChildStdin>,
    output: BufReader<ChildStdout>,
}

impl RunnerProcess {
    fn start(root: &Path) -> Self {
        let mut child = Command::new(env!("CARGO_BIN_EXE_keith-cua-runner"))
            .arg("--root")
            .arg(root)
            .arg("--allow-headless-process-test")
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        let input = BufWriter::new(child.stdin.take().unwrap());
        let output = BufReader::new(child.stdout.take().unwrap());
        Self {
            child,
            input,
            output,
        }
    }

    fn rpc(&mut self, command: &RunnerCommand) -> RunnerResponse {
        serde_json::to_writer(&mut self.input, command).unwrap();
        self.input.write_all(b"\n").unwrap();
        self.input.flush().unwrap();
        let mut line = String::new();
        self.output.read_line(&mut line).unwrap();
        assert!(!line.is_empty(), "runner closed before responding");
        serde_json::from_str(&line).unwrap()
    }

    fn crash(mut self) -> u32 {
        let pid = self.child.id();
        self.child.kill().unwrap();
        self.child.wait().unwrap();
        pid
    }

    fn shutdown(mut self) {
        assert_eq!(self.rpc(&RunnerCommand::Shutdown), RunnerResponse::Shutdown);
        assert!(self.child.wait().unwrap().success());
    }
}

fn create_request(profile_id: ProfileId, mode: ComputerMode) -> CreateComputerRequest {
    let limits = ComputerResourceLimits {
        memory_bytes: 16 * 1_024 * 1_024 * 1_024,
        idle_timeout_ms: 1_000,
        ..ComputerResourceLimits::default()
    };
    CreateComputerRequest {
        profile_id,
        mode,
        isolation: IsolationRequirement::ReducedExplicitlyAllowed,
        network: NetworkPolicy::Denied,
        viewport: Viewport {
            width: 800,
            height: 600,
            device_scale_milli: 1_000,
        },
        limits,
    }
}

#[test]
fn concrete_headed_linux_runtime_launches_xvfb_and_real_browser() {
    let temp = TempDir::new().unwrap();
    let profile = ProfileId::new();
    let now = UtcTimestamp::from_unix_millis(800_000);
    let runtime = LinuxComputerRuntime::discover().unwrap();
    let mut controller = ComputerController::open(temp.path(), runtime, now).unwrap();
    let mut request = create_request(profile.clone(), ComputerMode::Ephemeral);
    request.isolation = IsolationRequirement::Strong;
    let session = controller.create(request, now).unwrap();
    controller.start(&session.id, &profile, now).unwrap();
    let observation = controller.observe(&session.id, &profile, now).unwrap();
    assert_eq!(observation.focused_window.unwrap().application, "Chromium");
    assert!(!observation.screenshot.base64_data.is_empty());
    controller.terminate(&session.id, &profile, now).unwrap();
}

#[test]
fn concrete_headless_linux_runtime_launches_real_browser() {
    let temp = TempDir::new().unwrap();
    let profile = ProfileId::new();
    let now = UtcTimestamp::from_unix_millis(900_000);
    let runtime = LinuxComputerRuntime::discover_for_process_tests().unwrap();
    let mut controller = ComputerController::open(temp.path(), runtime, now).unwrap();
    let session = controller
        .create(
            create_request(profile.clone(), ComputerMode::Ephemeral),
            now,
        )
        .unwrap();
    controller.start(&session.id, &profile, now).unwrap();
    let observation = controller.observe(&session.id, &profile, now).unwrap();
    assert!(!observation.screenshot.base64_data.is_empty());
    controller.terminate(&session.id, &profile, now).unwrap();
}

#[test]
fn real_browser_wait_verifies_no_change_and_cancels_in_flight() {
    let temp = TempDir::new().unwrap();
    let profile = ProfileId::new();
    let now = UtcTimestamp::from_unix_millis(950_000);
    let runtime = LinuxComputerRuntime::discover_for_process_tests().unwrap();
    let mut controller = ComputerController::open(temp.path(), runtime, now).unwrap();
    let session = controller
        .create(
            create_request(profile.clone(), ComputerMode::Ephemeral),
            now,
        )
        .unwrap();
    controller.start(&session.id, &profile, now).unwrap();
    let observed = controller.observe(&session.id, &profile, now).unwrap();
    let wait = approved_attempt(
        &session,
        &observed,
        ComputerAction::Wait { duration_ms: 50 },
        now,
    );
    let wait_boundary = boundary(&profile, &wait.authority.target);
    let waited = controller
        .execute_action(
            &ComputerActionRequest {
                computer_session_id: session.id.clone(),
                profile_id: profile.clone(),
                primary: wait,
                alternates: Vec::new(),
                progress: ProgressExpectation::NoChangeBefore { duration_ms: 50 },
            },
            &wait_boundary,
            now,
        )
        .unwrap();
    assert_eq!(waited.disposition, keith_cua::ActionDisposition::Completed);

    let observed = controller.observe(&session.id, &profile, now).unwrap();
    let long_wait = approved_attempt(
        &session,
        &observed,
        ComputerAction::Wait { duration_ms: 1_000 },
        now,
    );
    let cancellation = controller.cancellation_handle(long_wait.authority.cancellation_id.clone());
    let wait_boundary = boundary(&profile, &long_wait.authority.target);
    let cancel_thread = thread::spawn(move || {
        thread::sleep(Duration::from_millis(250));
        cancellation.cancel();
    });
    let started = Instant::now();
    let result = controller
        .execute_action(
            &ComputerActionRequest {
                computer_session_id: session.id.clone(),
                profile_id: profile.clone(),
                primary: long_wait,
                alternates: Vec::new(),
                progress: ProgressExpectation::NoChangeBefore { duration_ms: 1_000 },
            },
            &wait_boundary,
            now,
        )
        .unwrap();
    cancel_thread.join().unwrap();
    assert_eq!(result.disposition, keith_cua::ActionDisposition::Cancelled);
    let cancellation_latency = started.elapsed();
    assert!(cancellation_latency < Duration::from_millis(900));
    eprintln!(
        "keith_cua_metric cancellation_latency_ms={}",
        cancellation_latency.as_millis()
    );
    controller.terminate(&session.id, &profile, now).unwrap();
}

#[test]
#[allow(clippy::too_many_lines)]
fn named_credential_is_origin_scoped_and_only_fills_a_protected_field() {
    let listener = TcpListener::bind("127.0.0.1:0").unwrap();
    let address = listener.local_addr().unwrap();
    let server = thread::spawn(move || {
        let (mut stream, _) = listener.accept().unwrap();
        let mut request = [0_u8; 4_096];
        let _ = stream.read(&mut request).unwrap();
        let body = b"<!doctype html><title>Credential proof</title><input id='password' type='password'><input id='plain' type='text'>";
        write!(
            stream,
            "HTTP/1.1 200 OK\r\nContent-Type: text/html\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            body.len()
        )
        .unwrap();
        stream.write_all(body).unwrap();
    });

    let temp = TempDir::new().unwrap();
    let profile = ProfileId::new();
    let now = UtcTimestamp::from_unix_millis(975_000);
    let runtime = LinuxComputerRuntime::discover_for_process_tests().unwrap();
    let mut controller = ComputerController::open(temp.path(), runtime, now).unwrap();
    controller
        .runtime_mut()
        .register_credential(&profile, "vault:password", b"super-secret")
        .unwrap();
    let mut create = create_request(profile.clone(), ComputerMode::Ephemeral);
    create.network = NetworkPolicy::LoopbackOnly;
    let session = controller.create(create, now).unwrap();
    controller.start(&session.id, &profile, now).unwrap();
    let blank = controller.observe(&session.id, &profile, now).unwrap();
    let url = format!("http://{address}/");
    let navigate = approved_attempt(
        &session,
        &blank,
        ComputerAction::Navigate { url: url.clone() },
        now,
    );
    let navigate_boundary = boundary(&profile, &navigate.authority.target);
    let navigated = controller
        .execute_action(
            &ComputerActionRequest {
                computer_session_id: session.id.clone(),
                profile_id: profile.clone(),
                primary: navigate,
                alternates: Vec::new(),
                progress: ProgressExpectation::DomContains {
                    text: "id=\"password\"".into(),
                },
            },
            &navigate_boundary,
            now,
        )
        .unwrap()
        .observation;
    server.join().unwrap();

    let grant = NamedCredentialGrant {
        grant_name: RedactedText::parse("primary-password").unwrap(),
        opaque_handle: RedactedText::parse("vault:password").unwrap(),
        profile_id: profile.clone(),
        allowed_origin: RedactedText::parse(url.clone()).unwrap(),
        expires_at: UtcTimestamp::from_unix_millis(now.unix_millis() + 60_000),
    };
    let fill = approved_attempt(
        &session,
        &navigated,
        ComputerAction::CredentialFill {
            grant: grant.clone(),
            target: SemanticTarget::Css {
                selector: "#password".into(),
            },
        },
        now,
    );
    let fill_boundary = boundary(&profile, &fill.authority.target);
    let filled = controller
        .execute_action(
            &ComputerActionRequest {
                computer_session_id: session.id.clone(),
                profile_id: profile.clone(),
                primary: fill,
                alternates: Vec::new(),
                progress: ProgressExpectation::FrameChanged,
            },
            &fill_boundary,
            now,
        )
        .unwrap();
    assert_eq!(filled.disposition, keith_cua::ActionDisposition::Completed);
    assert!(
        !serde_json::to_string(&filled.observation)
            .unwrap()
            .contains("super-secret")
    );

    let plain = approved_attempt(
        &session,
        &filled.observation,
        ComputerAction::CredentialFill {
            grant: grant.clone(),
            target: SemanticTarget::Css {
                selector: "#plain".into(),
            },
        },
        now,
    );
    let plain_boundary = boundary(&profile, &plain.authority.target);
    let refused = controller
        .execute_action(
            &ComputerActionRequest {
                computer_session_id: session.id.clone(),
                profile_id: profile.clone(),
                primary: plain,
                alternates: Vec::new(),
                progress: ProgressExpectation::FrameChanged,
            },
            &plain_boundary,
            now,
        )
        .unwrap();
    assert_eq!(
        refused.disposition,
        keith_cua::ActionDisposition::NoProgress
    );

    let current = controller.observe(&session.id, &profile, now).unwrap();
    let mut wrong_origin = grant;
    wrong_origin.allowed_origin =
        RedactedText::parse(format!("http://127.0.0.1.evil:{}/", address.port())).unwrap();
    let substituted = approved_attempt(
        &session,
        &current,
        ComputerAction::CredentialFill {
            grant: wrong_origin,
            target: SemanticTarget::Css {
                selector: "#password".into(),
            },
        },
        now,
    );
    let substituted_boundary = boundary(&profile, &substituted.authority.target);
    assert!(matches!(
        controller.execute_action(
            &ComputerActionRequest {
                computer_session_id: session.id.clone(),
                profile_id: profile.clone(),
                primary: substituted,
                alternates: Vec::new(),
                progress: ProgressExpectation::FrameChanged,
            },
            &substituted_boundary,
            now,
        ),
        Err(keith_cua::ComputerError::InvalidCredentialGrant)
    ));
    controller.terminate(&session.id, &profile, now).unwrap();
}

#[test]
#[allow(clippy::too_many_lines)]
fn real_runner_process_observes_and_acts() {
    let journey_started = Instant::now();
    let temp = TempDir::new().unwrap();
    let profile = ProfileId::new();
    let now = UtcTimestamp::from_unix_millis(1_000_000);
    let mut runner = RunnerProcess::start(temp.path());
    let session = match runner.rpc(&RunnerCommand::Create {
        request: create_request(profile.clone(), ComputerMode::Persistent),
        now,
    }) {
        RunnerResponse::Session { session } => session,
        response => panic!("unexpected create response: {response:?}"),
    };
    let workspace = temp
        .path()
        .join("profiles")
        .join(profile.to_string())
        .join(session.id.to_string())
        .join("workspace");
    let page = workspace.join("action.html");
    std::fs::write(
        &page,
        b"<!doctype html><title>Keith CUA</title><main><h1>Ready</h1><button id='go' onclick=\"this.textContent='Done';document.body.dataset.state='clicked'\">Go</button></main>",
    )
    .unwrap();
    let start_response = runner.rpc(&RunnerCommand::Start {
        session_id: session.id.clone(),
        profile_id: profile.clone(),
        now,
    });
    let running = match start_response {
        RunnerResponse::Session { session } => session,
        response => panic!("unexpected start response: {response:?}"),
    };
    assert_eq!(running.lifecycle, ComputerLifecycle::Running);
    let keith = ExternalPrincipalId::new();
    let screen = match runner.rpc(&RunnerCommand::CreateScreen {
        session_id: running.id.clone(),
        profile_id: profile.clone(),
        keith_principal: keith.clone(),
        now,
    }) {
        RunnerResponse::Screen { screen } => screen,
        response => panic!("unexpected screen response: {response:?}"),
    };
    assert!(matches!(
        runner.rpc(&RunnerCommand::UpdateScreen {
            screen_id: screen.id.clone(),
            profile_id: profile.clone(),
            connection: ScreenConnectionState::Connected,
            quality: ScreenQuality::High,
            frame_sequence: 1,
            active_action: None,
            intended_action: None,
            recording: false,
            safe_error: None,
            now,
        }),
        RunnerResponse::Screen { .. }
    ));
    let blank = observation(
        &mut runner,
        &running.id,
        &profile,
        UtcTimestamp::from_unix_millis(now.unix_millis() + 1),
    );
    assert_eq!(blank.url.as_deref(), Some("about:blank"));
    assert!(!blank.screenshot.base64_data.is_empty());

    let file_url = url::Url::from_file_path(&page).unwrap().to_string();
    let navigate = ComputerAction::Navigate {
        url: file_url.clone(),
    };
    let mut navigate_attempt = approved_attempt(&running, &blank, navigate, now);
    navigate_attempt.authority.acting_principal = keith.clone();
    let navigate_boundary = boundary(&profile, &navigate_attempt.authority.target);
    let navigated = controlled_action(
        &mut runner,
        &running,
        &screen,
        navigate_attempt,
        ProgressExpectation::DomContains {
            text: "Ready".into(),
        },
        navigate_boundary,
        now,
    );
    assert_eq!(navigated.url.as_deref(), Some(file_url.as_str()));

    let click = ComputerAction::Click {
        target: ActionTarget::Semantic {
            target: SemanticTarget::Css {
                selector: "#go".into(),
            },
        },
        button: MouseButton::Left,
    };
    let mut click_attempt = approved_attempt(&running, &navigated, click, now);
    click_attempt.authority.acting_principal = keith.clone();
    let click_boundary = boundary(&profile, &click_attempt.authority.target);
    let clicked = controlled_action(
        &mut runner,
        &running,
        &screen,
        click_attempt,
        ProgressExpectation::DomContains {
            text: "data-state=\"clicked\"".into(),
        },
        click_boundary,
        now,
    );
    assert!(clicked.dom.as_ref().unwrap().html.contains(">Done<"));

    let user = ExternalPrincipalId::new();
    let user_screen = match runner.rpc(&RunnerCommand::TakeUserControl {
        screen_id: screen.id.clone(),
        profile_id: profile.clone(),
        expected_revision: 0,
        user_principal: user.clone(),
        now,
    }) {
        RunnerResponse::Screen { screen } => screen,
        response => panic!("unexpected takeover response: {response:?}"),
    };
    let mut manual_wait = approved_attempt(
        &running,
        &clicked,
        ComputerAction::Wait { duration_ms: 20 },
        now,
    );
    manual_wait.authority.acting_principal = user;
    let manual_boundary = boundary(&profile, &manual_wait.authority.target);
    let manual = controlled_action(
        &mut runner,
        &running,
        &user_screen,
        manual_wait,
        ProgressExpectation::NoChangeBefore { duration_ms: 20 },
        manual_boundary,
        now,
    );
    let mut blocked_keith = approved_attempt(
        &running,
        &manual,
        ComputerAction::Wait { duration_ms: 20 },
        now,
    );
    blocked_keith.authority.acting_principal = keith.clone();
    let blocked_boundary = boundary(&profile, &blocked_keith.authority.target);
    assert!(matches!(
        runner.rpc(&RunnerCommand::ControlledAct {
            request: Box::new(ComputerActionRequest {
                computer_session_id: running.id.clone(),
                profile_id: profile.clone(),
                primary: blocked_keith,
                alternates: Vec::new(),
                progress: ProgressExpectation::NoChangeBefore { duration_ms: 20 },
            }),
            boundary: blocked_boundary,
            screen_id: screen.id.clone(),
            expected_revision: 0,
            principal: keith.clone(),
            focus_unambiguous: true,
            stream_synchronized: true,
            now,
        }),
        RunnerResponse::Error { ref code, .. } if code == "stale_lease"
    ));
    assert!(matches!(
        runner.rpc(&RunnerCommand::RequestKeithControl {
            screen_id: screen.id.clone(),
            profile_id: profile.clone(),
            keith_principal: keith,
        }),
        RunnerResponse::KeithControlRequested
    ));
    assert!(matches!(
        runner.rpc(&RunnerCommand::GrantKeithControl {
            screen_id: screen.id.clone(),
            profile_id: profile.clone(),
            expected_revision: 1,
            now,
        }),
        RunnerResponse::Screen { screen } if screen.control.owner == ControlOwner::KeithControl
            && screen.control.revision == 2
    ));

    let stale_action = ComputerAction::Click {
        target: ActionTarget::Coordinate {
            point: Point { x: 20, y: 20 },
            source_frame: navigated.screenshot.frame_id.clone(),
        },
        button: MouseButton::Left,
    };
    let stale_attempt = approved_attempt(&running, &clicked, stale_action, now);
    let stale_boundary = boundary(&profile, &stale_attempt.authority.target);
    let stale = runner.rpc(&RunnerCommand::Act {
        request: Box::new(ComputerActionRequest {
            computer_session_id: running.id.clone(),
            profile_id: profile.clone(),
            primary: stale_attempt,
            alternates: Vec::new(),
            progress: ProgressExpectation::FrameChanged,
        }),
        boundary: stale_boundary,
        now,
    });
    assert!(matches!(
        stale,
        RunnerResponse::Error { ref code, .. } if code == "stale_target"
    ));

    assert!(matches!(
        runner.rpc(&RunnerCommand::Suspend {
            session_id: running.id.clone(),
            profile_id: profile.clone(),
            now,
        }),
        RunnerResponse::Session { session } if session.lifecycle == ComputerLifecycle::Suspended
    ));
    std::fs::write(workspace.join("snapshot-proof.txt"), b"before").unwrap();
    let snapshot_id = match runner.rpc(&RunnerCommand::Snapshot {
        session_id: running.id.clone(),
        profile_id: profile.clone(),
        now,
    }) {
        RunnerResponse::Snapshot { snapshot_id } => snapshot_id,
        response => panic!("unexpected snapshot response: {response:?}"),
    };
    std::fs::write(workspace.join("snapshot-proof.txt"), b"after").unwrap();
    assert!(matches!(
        runner.rpc(&RunnerCommand::Restore {
            session_id: running.id.clone(),
            profile_id: profile.clone(),
            snapshot_id,
            now,
        }),
        RunnerResponse::Session { .. }
    ));
    assert_eq!(
        std::fs::read(workspace.join("snapshot-proof.txt")).unwrap(),
        b"before"
    );
    assert!(matches!(
        runner.rpc(&RunnerCommand::Resume {
            session_id: running.id.clone(),
            profile_id: profile.clone(),
            now,
        }),
        RunnerResponse::Session { session } if session.lifecycle == ComputerLifecycle::Running
    ));
    assert!(matches!(
        runner.rpc(&RunnerCommand::ReclaimIdle {
            now: UtcTimestamp::from_unix_millis(now.unix_millis() + 2_000),
        }),
        RunnerResponse::Reclaimed { sessions } if sessions == vec![running.id.clone()]
    ));
    assert!(matches!(
        runner.rpc(&RunnerCommand::Reset {
            session_id: running.id.clone(),
            profile_id: profile.clone(),
            now,
        }),
        RunnerResponse::Session { session } if session.lifecycle == ComputerLifecycle::Created
    ));
    assert!(!workspace.join("snapshot-proof.txt").exists());
    assert_eq!(
        runner.rpc(&RunnerCommand::DeleteProfile {
            profile_id: profile,
            now,
        }),
        RunnerResponse::Deleted { sessions: 1 }
    );
    let journey_latency = journey_started.elapsed();
    assert!(journey_latency < Duration::from_secs(120));
    eprintln!(
        "keith_cua_metric completed_actions=3 unsafe_refusals=2 journey_latency_ms={}",
        journey_latency.as_millis()
    );
    runner.shutdown();
}

fn authenticate_read_only_screen_stream(
    runner: &mut RunnerProcess,
    screen_id: &keith_agent_types::EntityId,
    profile: &ProfileId,
    observer: ExternalPrincipalId,
    now: UtcTimestamp,
) {
    let stream_started = Instant::now();
    let grant = match runner.rpc(&RunnerCommand::NegotiateScreenStream {
        screen_id: screen_id.clone(),
        profile_id: profile.clone(),
        observer_id: observer.clone(),
        origin: "https://keith.test".into(),
        now,
        ttl_ms: 10_000,
    }) {
        RunnerResponse::ScreenStream { grant } => grant,
        response => panic!("unexpected stream response: {response:?}"),
    };
    assert!(grant.read_only);
    assert!(!grant.stream_path.contains("vnc"));
    assert!(!grant.stream_path.contains("debug"));
    assert!(matches!(
        runner.rpc(&RunnerCommand::AuthenticateScreenStream {
            profile_id: profile.clone(),
            observer_id: observer,
            origin: "https://keith.test".into(),
            stream_ticket: grant.stream_ticket,
            now,
        }),
        RunnerResponse::ScreenStreamAuthenticated { screen_id: authenticated }
            if authenticated == *screen_id
    ));
    let stream_latency = stream_started.elapsed();
    assert!(stream_latency < Duration::from_secs(2));
    eprintln!(
        "keith_cua_metric stream_auth_latency_ms={}",
        stream_latency.as_millis()
    );
}

#[test]
fn real_runner_process_enforces_stream_and_exclusive_control_across_restart() {
    let temp = TempDir::new().unwrap();
    let profile = ProfileId::new();
    let stranger = ProfileId::new();
    let keith = ExternalPrincipalId::new();
    let user = ExternalPrincipalId::new();
    let observer = ExternalPrincipalId::new();
    let now = UtcTimestamp::from_unix_millis(1_500_000);
    let mut runner = RunnerProcess::start(temp.path());
    let session = match runner.rpc(&RunnerCommand::Create {
        request: create_request(profile.clone(), ComputerMode::Persistent),
        now,
    }) {
        RunnerResponse::Session { session } => session,
        response => panic!("unexpected create response: {response:?}"),
    };
    let screen = match runner.rpc(&RunnerCommand::CreateScreen {
        session_id: session.id.clone(),
        profile_id: profile.clone(),
        keith_principal: keith.clone(),
        now,
    }) {
        RunnerResponse::Screen { screen } => screen,
        response => panic!("unexpected screen response: {response:?}"),
    };
    assert!(matches!(
        runner.rpc(&RunnerCommand::GetScreen {
            screen_id: screen.id.clone(),
            profile_id: stranger,
        }),
        RunnerResponse::Error { ref code, .. } if code == "profile_isolation"
    ));
    assert!(matches!(
        runner.rpc(&RunnerCommand::UpdateScreen {
            screen_id: screen.id.clone(),
            profile_id: profile.clone(),
            connection: ScreenConnectionState::Connected,
            quality: ScreenQuality::High,
            frame_sequence: 1,
            active_action: Some(RedactedText::parse("opening account settings").unwrap()),
            intended_action: Some(RedactedText::parse("wait for user review").unwrap()),
            recording: false,
            safe_error: None,
            now,
        }),
        RunnerResponse::Screen { .. }
    ));
    authenticate_read_only_screen_stream(&mut runner, &screen.id, &profile, observer, now);
    let user_screen = match runner.rpc(&RunnerCommand::TakeUserControl {
        screen_id: screen.id.clone(),
        profile_id: profile.clone(),
        expected_revision: 0,
        user_principal: user,
        now,
    }) {
        RunnerResponse::Screen { screen } => screen,
        response => panic!("unexpected takeover response: {response:?}"),
    };
    assert_eq!(user_screen.control.owner, ControlOwner::UserControl);
    assert!(matches!(
        runner.rpc(&RunnerCommand::RequestKeithControl {
            screen_id: screen.id.clone(),
            profile_id: profile.clone(),
            keith_principal: keith,
        }),
        RunnerResponse::KeithControlRequested
    ));
    let returned = match runner.rpc(&RunnerCommand::GrantKeithControl {
        screen_id: screen.id.clone(),
        profile_id: profile.clone(),
        expected_revision: 1,
        now,
    }) {
        RunnerResponse::Screen { screen } => screen,
        response => panic!("unexpected return response: {response:?}"),
    };
    assert_eq!(returned.control.owner, ControlOwner::KeithControl);
    runner.shutdown();

    let mut restarted = RunnerProcess::start(temp.path());
    assert!(matches!(
        restarted.rpc(&RunnerCommand::GetScreen {
            screen_id: screen.id,
            profile_id: profile,
        }),
        RunnerResponse::Screen { screen } if screen.control.owner == ControlOwner::Paused
            && screen.connection == ScreenConnectionState::Reconnecting
    ));
    restarted.shutdown();
}

#[test]
fn real_runner_process_crash_reconciles_without_cross_profile_access() {
    let temp = TempDir::new().unwrap();
    let owner = ProfileId::new();
    let stranger = ProfileId::new();
    let now = UtcTimestamp::from_unix_millis(2_000_000);
    let mut runner = RunnerProcess::start(temp.path());
    let session = match runner.rpc(&RunnerCommand::Create {
        request: create_request(owner.clone(), ComputerMode::Ephemeral),
        now,
    }) {
        RunnerResponse::Session { session } => session,
        response => panic!("unexpected create response: {response:?}"),
    };
    assert!(matches!(
        runner.rpc(&RunnerCommand::Start {
            session_id: session.id.clone(),
            profile_id: stranger.clone(),
            now,
        }),
        RunnerResponse::Error { ref code, .. } if code == "profile_isolation"
    ));
    let start_response = runner.rpc(&RunnerCommand::Start {
        session_id: session.id.clone(),
        profile_id: owner.clone(),
        now,
    });
    assert!(matches!(
        start_response,
        RunnerResponse::Session { session } if session.lifecycle == ComputerLifecycle::Running
    ));
    let recovery_started = Instant::now();
    let crashed_pid = runner.crash();
    thread::sleep(Duration::from_millis(250));
    assert!(!resource_groups_owned_by(crashed_pid).is_empty());

    let mut restarted = RunnerProcess::start(temp.path());
    let health = restarted.rpc(&RunnerCommand::Health {
        session_id: session.id.clone(),
        profile_id: owner.clone(),
    });
    assert!(matches!(
        health,
        RunnerResponse::Health { health }
            if health.lifecycle == ComputerLifecycle::Interrupted
                && !health.process_running
                && health.restartable
    ));
    assert!(resource_groups_owned_by(crashed_pid).is_empty());
    let recovery_latency = recovery_started.elapsed();
    assert!(recovery_latency < Duration::from_secs(10));
    eprintln!(
        "keith_cua_metric crash_recovery_latency_ms={}",
        recovery_latency.as_millis()
    );
    assert_eq!(
        restarted.rpc(&RunnerCommand::DeleteProfile {
            profile_id: owner,
            now,
        }),
        RunnerResponse::Deleted { sessions: 1 }
    );
    restarted.shutdown();
}

fn resource_groups_owned_by(owner_pid: u32) -> Vec<std::path::PathBuf> {
    fn visit(path: &Path, prefix: &str, depth: u8, found: &mut Vec<std::path::PathBuf>) {
        if depth == 0 {
            return;
        }
        let Ok(entries) = std::fs::read_dir(path) else {
            return;
        };
        for entry in entries.flatten() {
            let Ok(file_type) = entry.file_type() else {
                continue;
            };
            if !file_type.is_dir() {
                continue;
            }
            if entry.file_name().to_string_lossy().starts_with(prefix) {
                found.push(entry.path());
            } else {
                visit(&entry.path(), prefix, depth - 1, found);
            }
        }
    }

    let mut found = Vec::new();
    visit(
        Path::new("/sys/fs/cgroup"),
        format!("keith-cua-{owner_pid}-").as_str(),
        5,
        &mut found,
    );
    found
}

fn observation(
    runner: &mut RunnerProcess,
    session_id: &keith_platform_contracts::ComputerSessionId,
    profile_id: &ProfileId,
    now: UtcTimestamp,
) -> keith_cua::ComputerObservation {
    match runner.rpc(&RunnerCommand::Observe {
        session_id: session_id.clone(),
        profile_id: profile_id.clone(),
        now,
    }) {
        RunnerResponse::Observation { observation } => *observation,
        response => panic!("unexpected observation response: {response:?}"),
    }
}

fn controlled_action(
    runner: &mut RunnerProcess,
    session: &keith_cua::ComputerSession,
    screen: &keith_cua::ScreenSession,
    attempt: ActionAttempt,
    progress: ProgressExpectation,
    boundary: AuthorityBoundary,
    now: UtcTimestamp,
) -> keith_cua::ComputerObservation {
    let principal = attempt.authority.acting_principal.clone();
    match runner.rpc(&RunnerCommand::ControlledAct {
        request: Box::new(ComputerActionRequest {
            computer_session_id: session.id.clone(),
            profile_id: session.profile_id.clone(),
            primary: attempt,
            alternates: Vec::new(),
            progress,
        }),
        boundary,
        screen_id: screen.id.clone(),
        expected_revision: screen.control.revision,
        principal,
        focus_unambiguous: true,
        stream_synchronized: true,
        now,
    }) {
        RunnerResponse::Action { action } => {
            assert_eq!(action.disposition, keith_cua::ActionDisposition::Completed);
            action.observation
        }
        response => panic!("unexpected controlled action response: {response:?}"),
    }
}

fn approved_attempt(
    session: &keith_cua::ComputerSession,
    observation: &keith_cua::ComputerObservation,
    action: ComputerAction,
    now: UtcTimestamp,
) -> ActionAttempt {
    let digest = action_target_digest(&session.id, &action, observation).unwrap();
    ActionAttempt {
        action,
        authority: ExternalAction {
            profile_id: session.profile_id.clone(),
            session_id: SessionId::new(),
            acting_principal: ExternalPrincipalId::new(),
            requested_capability: Capability::ComputerControl,
            risk: ActionRisk::IrreversibleComputerInput,
            approval: ApprovalEnvelope {
                risk: ActionRisk::IrreversibleComputerInput,
                state: ApprovalState::Granted {
                    approval_id: ApprovalId::new(),
                    granted_by: ExternalPrincipalId::new(),
                    exact_target_digest: RedactedText::parse(digest.clone()).unwrap(),
                    expires_at: UtcTimestamp::from_unix_millis(now.unix_millis() + 60_000),
                },
            },
            target: RedactedText::parse("computer-session").unwrap(),
            target_digest: RedactedText::parse(digest).unwrap(),
            cancellation_id: CancellationId::new(),
            reply_route: None,
            audit_correlation: AuditCorrelationId::new(),
            external_effect: ExternalEffect::NonRepeatable,
        },
    }
}

fn boundary(profile_id: &ProfileId, target: &RedactedText) -> AuthorityBoundary {
    AuthorityBoundary {
        profile_id: profile_id.clone(),
        allowed: BTreeSet::from([CapabilityGrant {
            capability: Capability::ComputerControl,
            resource: target.clone(),
            expires_at: None,
        }]),
        denied: BTreeSet::new(),
        max_automatic_risk: ActionRisk::IrreversibleComputerInput,
    }
}
