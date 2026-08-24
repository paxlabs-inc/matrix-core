use std::fs;
use std::path::PathBuf;
use std::process::Command;
use std::thread;
use std::time::Duration;

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, ComputerId, EntityId, ProfileId, Revision, StableKey, UtcTimestamp,
};
use keith_computer::{
    BoundaryDecision, ComputerAction, ComputerActor, ComputerBoundaryPolicy, ComputerHost,
    ComputerHostConfig, ComputerHostError, ComputerInputCommand, ComputerInputPayload,
    ComputerRecord, ComputerRepository, ComputerRepositoryBatch, ComputerResourcePolicy,
    ComputerState, ComputerStreamController, ComputerStreamLimits, ComputerStreamOpenRequest,
    ComputerStreamOrigin, ComputerStreamSession, ComputerStreamSubject, ComputerTaskRequest,
    ControlState, InMemoryComputerRepository, RecoveryDecision, TaskAdmission, TaskConflictPolicy,
};

fn required_absolute_path(variable: &str) -> PathBuf {
    let path = PathBuf::from(
        std::env::var_os(variable)
            .unwrap_or_else(|| panic!("{variable} must name a real packaged executable")),
    );
    assert!(path.is_absolute(), "{variable} must be absolute");
    assert!(path.is_file(), "{variable} must exist");
    path
}

fn key(value: &str) -> StableKey {
    StableKey::parse(value).expect("test stable key must be canonical")
}

fn now(value: i64) -> UtcTimestamp {
    UtcTimestamp::from_unix_millis(value)
}

fn policy(root: &std::path::Path) -> ComputerBoundaryPolicy {
    ComputerBoundaryPolicy {
        allowed_origins: vec!["https://example.com".into()],
        writable_roots: vec![root.to_path_buf()],
        download_root: root.join("downloads"),
        allow_credentials: false,
        resources: ComputerResourcePolicy {
            cpu_quota_percent: 50,
            max_memory_bytes: 536_870_912,
            max_processes: 48,
            max_disk_bytes: 64 * 1024 * 1024,
            max_download_bytes: 8 * 1024 * 1024,
            max_network_requests_per_minute: 2,
            idle_timeout_seconds: 120,
            crash_limit: 2,
            crash_window_seconds: 60,
        },
    }
}

fn runner_pid(profile_id: &ProfileId) -> u32 {
    let output = Command::new("/usr/bin/pgrep")
        .args(["-f", &profile_id.to_string()])
        .output()
        .expect("pgrep must inspect the dedicated runner");
    assert!(
        output.status.success(),
        "profile runner must be discoverable"
    );
    String::from_utf8(output.stdout)
        .unwrap()
        .lines()
        .find_map(|line| line.trim().parse().ok())
        .expect("profile runner PID must be numeric")
}

fn cgroup_path(process_id: u32) -> PathBuf {
    let membership = fs::read_to_string(format!("/proc/{process_id}/cgroup"))
        .expect("runner cgroup membership must be readable");
    let relative = membership
        .lines()
        .find_map(|line| line.strip_prefix("0::"))
        .expect("unified cgroup v2 membership is required");
    PathBuf::from("/sys/fs/cgroup").join(relative.trim_start_matches('/'))
}

#[test]
#[ignore = "requires packaged browser-runner, Chromium, Xvfb, systemd user scopes, and cgroup v2"]
fn real_headed_stream_input_crash_and_resource_adversarial_matrix() {
    let root = tempfile::tempdir().unwrap();
    fs::create_dir_all(root.path().join("downloads")).unwrap();
    let profile_id = ProfileId::new();
    let computer_id = ComputerId::new();
    let record = ComputerRecord {
        version: CURRENT_SCHEMA_VERSION,
        computer_id: computer_id.clone(),
        owner_profile_id: profile_id.clone(),
        browser_profile_root: root.path().join("chromium").to_string_lossy().into_owned(),
        screen_key: key("screen/real-matrix"),
        state: ComputerState::Ready,
        control_state: ControlState::Idle,
        current_task_key: None,
        created_at: now(0),
        updated_at: now(0),
        revision: Revision::ZERO,
    };
    let repository = InMemoryComputerRepository::default();
    repository
        .transact(&[ComputerRepositoryBatch::InsertComputer(record.clone())])
        .unwrap();
    let host = ComputerHost::new(
        repository,
        ComputerHostConfig {
            browser_runner_binary: required_absolute_path("KEITH_BROWSER_RUNNER"),
            chromium_binary: required_absolute_path("KEITH_CHROMIUM"),
            xvfb_binary: required_absolute_path("KEITH_XVFB"),
            systemd_run_binary: required_absolute_path("KEITH_SYSTEMD_RUN"),
            display_base: 300,
            control_port_base: 36_000,
            screen_width: 1_280,
            screen_height: 720,
            screen_depth: 24,
        },
    );
    let boundary = policy(root.path());
    host.start(&record, boundary.clone(), now(1)).unwrap();
    let task = key("task/real-matrix");
    let lease = match host
        .acquire_task(
            ComputerTaskRequest {
                owner_profile_id: profile_id.clone(),
                task_key: task.clone(),
                actor: ComputerActor::Agent {
                    profile_id: profile_id.clone(),
                },
                conflict: TaskConflictPolicy::Deny,
            },
            now(2),
        )
        .unwrap()
    {
        TaskAdmission::Acquired(lease) => lease,
        admission => panic!("real task must acquire its screen, got {admission:?}"),
    };
    let subject = ComputerStreamSubject {
        profile_id: profile_id.clone(),
        computer_id: computer_id.clone(),
    };
    let origin = ComputerStreamOrigin {
        server_instance_id: EntityId::new(),
        stream_instance_id: EntityId::new(),
        authority_key: key("stream/real-matrix"),
        generation: 1,
    };
    let controller = ComputerStreamController::Agent {
        profile_id: profile_id.clone(),
        task_key: task,
        fencing_token: lease.fencing_token,
    };
    let authorization = host
        .authorize_stream(subject.clone(), origin, controller, now(3), now(30_000))
        .unwrap();
    let mut session = ComputerStreamSession::open(
        EntityId::new(),
        ComputerStreamOpenRequest {
            subject: subject.clone(),
            resume: None,
        },
        authorization,
        ComputerStreamLimits::STRICT,
        now(4),
        now(2_000),
    )
    .unwrap();
    let observation = host
        .observe_stream(&mut session, now(5), now(2_100))
        .unwrap();
    assert!(observation.url.starts_with("http") || observation.url == "about:blank");
    let frame = host
        .capture_stream_frame(&mut session, now(6), now(2_200))
        .unwrap();
    assert!(
        frame
            .bytes
            .starts_with(&[0x89, b'P', b'N', b'G', 0x0d, 0x0a, 0x1a, 0x0a])
    );
    let descriptor = session.descriptor();
    host.dispatch_stream_input(
        &mut session,
        ComputerInputCommand {
            session_id: descriptor.session_id,
            subject: descriptor.subject,
            origin: descriptor.origin,
            sequence: 0,
            expected_computer_revision: descriptor.computer_revision,
            controller: descriptor.controller,
            payload: ComputerInputPayload::Focus,
        },
        now(7),
        now(2_300),
    )
    .unwrap();

    let forged = ComputerStreamSubject {
        profile_id: ProfileId::new(),
        computer_id,
    };
    assert!(matches!(
        host.authorize_stream(
            forged,
            ComputerStreamOrigin {
                server_instance_id: EntityId::new(),
                stream_instance_id: EntityId::new(),
                authority_key: key("stream/forged-matrix"),
                generation: 1,
            },
            ComputerStreamController::Agent {
                profile_id: profile_id.clone(),
                task_key: key("task/real-matrix"),
                fencing_token: lease.fencing_token,
            },
            now(8),
            now(30_000),
        ),
        Err(ComputerHostError::Computer(_) | ComputerHostError::Unauthorized(_))
    ));

    let runner = runner_pid(&profile_id);
    let cgroup = cgroup_path(runner);
    assert_ne!(
        fs::read_to_string(cgroup.join("memory.max"))
            .unwrap()
            .trim(),
        "max"
    );
    assert_ne!(
        fs::read_to_string(cgroup.join("pids.max")).unwrap().trim(),
        "max"
    );
    let cpu = fs::read_to_string(cgroup.join("cpu.max")).unwrap();
    let mut cpu = cpu.split_whitespace();
    let quota: u64 = cpu.next().unwrap().parse().unwrap();
    let period: u64 = cpu.next().unwrap().parse().unwrap();
    assert!(quota < period);

    assert_eq!(
        boundary.evaluate(&ComputerAction::CredentialUse {
            credential_ref: "credential/private".into(),
            approved: true,
        }),
        BoundaryDecision::Deny,
    );
    host.authorize_action(
        &profile_id,
        &ComputerActor::Agent {
            profile_id: profile_id.clone(),
        },
        &ComputerAction::Navigate {
            origin: "https://example.com".into(),
        },
        now(10),
    )
    .unwrap();
    host.authorize_action(
        &profile_id,
        &ComputerActor::Agent {
            profile_id: profile_id.clone(),
        },
        &ComputerAction::Navigate {
            origin: "https://example.com".into(),
        },
        now(11),
    )
    .unwrap();
    assert!(matches!(
        host.authorize_action(
            &profile_id,
            &ComputerActor::Agent {
                profile_id: profile_id.clone(),
            },
            &ComputerAction::Navigate {
                origin: "https://example.com".into(),
            },
            now(12),
        ),
        Err(ComputerHostError::PolicyDenied)
    ));

    let disk_pressure = root.path().join("chromium").join("disk-bound.bin");
    fs::OpenOptions::new()
        .create(true)
        .truncate(true)
        .write(true)
        .open(&disk_pressure)
        .unwrap()
        .set_len(boundary.resources.max_disk_bytes + 1)
        .unwrap();
    assert!(matches!(
        host.authorize_action(
            &profile_id,
            &ComputerActor::Agent {
                profile_id: profile_id.clone(),
            },
            &ComputerAction::Input,
            now(13),
        ),
        Err(ComputerHostError::PolicyDenied)
    ));
    fs::remove_file(disk_pressure).unwrap();

    for attempt in 0..2 {
        let status = Command::new("/usr/bin/pkill")
            .args(["-f", &profile_id.to_string()])
            .status()
            .unwrap();
        assert!(status.success());
        thread::sleep(Duration::from_millis(250));
        let decision = host
            .reconcile(&profile_id, boundary.clone(), now(20 + attempt))
            .unwrap();
        if attempt == 0 {
            assert_eq!(decision, RecoveryDecision::Relaunch);
        } else {
            assert_eq!(decision, RecoveryDecision::Quarantine);
        }
    }
    assert_eq!(
        host.repository()
            .computer(&profile_id)
            .unwrap()
            .unwrap()
            .state,
        ComputerState::Quarantined,
    );
    assert!(
        host.repository()
            .audit(&profile_id)
            .unwrap()
            .iter()
            .any(|audit| {
                audit.kind == keith_computer::ComputerAuditKind::Recovery
                    && audit.safe_summary.contains("quarantined")
            })
    );
    host.shutdown(&profile_id).unwrap();
}
