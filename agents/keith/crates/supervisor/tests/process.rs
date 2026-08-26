use std::path::Path;
use std::time::Duration;

use keith_agent_types::{Generation, RootTreeId};
#[cfg(unix)]
use keith_supervisor::WorkerHealth;
use keith_supervisor::{ImageInstallRequest, SupervisorOptions, WorkerEvent, WorkerSupervisor};
use keith_worker_runtime::LeaseManager;
#[cfg(unix)]
use nix::sys::signal::{Signal, kill};
#[cfg(unix)]
use nix::sys::wait::waitpid;
#[cfg(unix)]
use nix::unistd::Pid;
use ring::rand::SystemRandom;
use ring::signature::{Ed25519KeyPair, KeyPair};
use serde_json::json;
use sha2::{Digest, Sha256};

fn options() -> SupervisorOptions {
    SupervisorOptions {
        startup_timeout: Duration::from_secs(3),
        drain_timeout: Duration::from_secs(1),
        stale_heartbeat: Duration::from_secs(1),
        heartbeat_interval: Duration::from_millis(20),
        lease_duration: Duration::from_millis(500),
    }
}

fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn signed_worker_image(executable: &[u8]) -> (Vec<u8>, Vec<u8>, [u8; 32]) {
    let output = "real exit status: success";
    let output_digest = digest(output.as_bytes());
    let gates = [
        "formatting",
        "strict_clippy",
        "workspace_tests",
        "dependency_policy",
        "security",
        "platform",
    ]
    .into_iter()
    .map(|gate| {
        json!({
            "gate": gate,
            "exit_code": 0,
            "elapsed_millis": 1,
            "output": output,
            "output_sha256": output_digest,
            "sandbox": {"backend":"real-process-test"}
        })
    })
    .collect::<Vec<_>>();
    let manifest = serde_json::to_vec(&json!({
        "format": "keith-worker-image-v1",
        "build_id": "generation-two",
        "base_revision": "a".repeat(40),
        "source_manifest_sha256": "b".repeat(64),
        "executable_sha256": digest(executable),
        "executable_bytes": executable.len(),
        "toolchain": {"rustc":"rustc 1.93", "cargo":"cargo 1.93", "target":"test-host"},
        "worker_report": {
            "component":"worker", "package_version":"0.1.0", "build_id":"generation-two",
            "protocol_version":"1.0", "storage_schema":"1.0", "enabled_features":["runtime"]
        },
        "gates": gates,
        "artifact_source_paths": ["crates/tools/src/lib.rs"],
        "change_class": "b"
    }))
    .unwrap();
    let key_bytes = Ed25519KeyPair::generate_pkcs8(&SystemRandom::new()).unwrap();
    let key = Ed25519KeyPair::from_pkcs8(key_bytes.as_ref()).unwrap();
    let public_key = key.public_key().as_ref().try_into().unwrap();
    let signature = key.sign(&manifest).as_ref().to_vec();
    (manifest, signature, public_key)
}

#[test]
#[cfg(unix)]
fn real_workers_are_adopted_isolated_restarted_and_evicted() {
    let directory = tempfile::tempdir().unwrap();
    let executable = env!("CARGO_BIN_EXE_keith-worker-process-host");
    let first = RootTreeId::new();
    let second = RootTreeId::new();

    let mut initial = WorkerSupervisor::open(directory.path(), executable, options()).unwrap();
    let first_status = initial.start(first.clone()).unwrap();
    let second_status = initial.start(second.clone()).unwrap();
    assert_eq!(first_status.health, WorkerHealth::Healthy);
    assert_eq!(second_status.health, WorkerHealth::Healthy);

    drop(initial);
    let mut restarted_daemon =
        WorkerSupervisor::open(directory.path(), executable, options()).unwrap();
    let adopted = restarted_daemon.adopt_existing().unwrap();
    assert_eq!(adopted.len(), 2, "adopted workers: {adopted:?}");

    kill(
        Pid::from_raw(i32::try_from(first_status.pid).unwrap()),
        Signal::SIGKILL,
    )
    .unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(2);
    let event = loop {
        if let Some(event) = restarted_daemon.monitor().unwrap().into_iter().next() {
            break event;
        }
        assert!(std::time::Instant::now() < deadline);
        std::thread::sleep(Duration::from_millis(10));
    };
    assert!(matches!(
        event,
        WorkerEvent::Exited { root_tree_id, .. } if root_tree_id == first
    ));
    assert_eq!(
        restarted_daemon.status(&second).unwrap().pid,
        second_status.pid
    );

    let replacement = restarted_daemon.restart(&first).unwrap();
    assert_eq!(replacement.generation, Generation::new(2));
    assert_ne!(replacement.pid, first_status.pid);
    assert!(
        restarted_daemon
            .validate_route(&first, Generation::new(1))
            .is_err()
    );
    restarted_daemon
        .validate_route(&first, Generation::new(2))
        .unwrap();

    std::thread::sleep(Duration::from_millis(5));
    let evicted = restarted_daemon.evict_idle(Duration::ZERO).unwrap();
    assert_eq!(evicted.len(), 2);
    assert!(restarted_daemon.statuses().is_empty());
}

#[test]
#[cfg(unix)]
fn restart_reclaims_a_dead_workers_unexpired_lease_before_activation() {
    let directory = tempfile::tempdir().unwrap();
    let executable = env!("CARGO_BIN_EXE_keith-worker-process-host");
    let root = RootTreeId::new();

    let mut initial = WorkerSupervisor::open(directory.path(), executable, options()).unwrap();
    let original = initial.start(root.clone()).unwrap();
    kill(
        Pid::from_raw(i32::try_from(original.pid).unwrap()),
        Signal::SIGKILL,
    )
    .unwrap();
    waitpid(Pid::from_raw(i32::try_from(original.pid).unwrap()), None).unwrap();
    drop(initial);

    let mut restarted = WorkerSupervisor::open(directory.path(), executable, options()).unwrap();
    assert!(restarted.adopt_existing().unwrap().is_empty());
    let replacement = restarted.start(root.clone()).unwrap();
    assert_eq!(replacement.generation, Generation::new(2));
    restarted
        .validate_route(&root, replacement.generation)
        .unwrap();
    restarted.drain(&root).unwrap();
}

#[test]
fn renewal_loss_stops_stale_worker_and_forced_replacement_advances_generation() {
    let directory = tempfile::tempdir().unwrap();
    let executable = env!("CARGO_BIN_EXE_keith-worker-process-host");
    let root = RootTreeId::new();
    let mut supervisor = WorkerSupervisor::open(directory.path(), executable, options()).unwrap();
    let original = supervisor.start(root.clone()).unwrap();
    let leases = LeaseManager::open(supervisor.lease_database_path()).unwrap();
    let deadline = std::time::Instant::now() + Duration::from_secs(2);
    loop {
        let grant = leases.current(&root).unwrap().unwrap();
        if leases.release(&grant).is_ok() {
            break;
        }
        assert!(std::time::Instant::now() < deadline);
    }

    let exit = loop {
        if let Some(event) = supervisor
            .monitor()
            .unwrap()
            .into_iter()
            .find(|event| matches!(event, WorkerEvent::Exited { .. }))
        {
            break event;
        }
        assert!(std::time::Instant::now() < deadline);
        std::thread::sleep(Duration::from_millis(10));
    };
    assert!(matches!(
        exit,
        WorkerEvent::Exited { root_tree_id, generation, .. }
            if root_tree_id == root && generation == original.generation
    ));

    let replacement = supervisor.start(root.clone()).unwrap();
    assert_eq!(replacement.generation, Generation::new(2));
    let forced = supervisor.force_replace(&root).unwrap();
    assert_eq!(forced.generation, Generation::new(3));
    assert_ne!(forced.pid, replacement.pid);
    assert!(
        supervisor
            .validate_route(&root, replacement.generation)
            .is_err()
    );
    supervisor.validate_route(&root, forced.generation).unwrap();
    supervisor.drain(&root).unwrap();
    assert!(supervisor.status(&root).is_none());
}

#[test]
fn image_resolution_is_bound_to_each_real_worker_generation() {
    let directory = tempfile::tempdir().unwrap();
    let executable = Path::new(env!("CARGO_BIN_EXE_keith-worker-process-host"));
    let executable_bytes = std::fs::read(executable).unwrap();
    let root = RootTreeId::new();
    let mut supervisor = WorkerSupervisor::open(directory.path(), executable, options()).unwrap();

    let original = supervisor.start(root.clone()).unwrap();
    assert!(original.image_id.starts_with("bootstrap-"));
    let (manifest, signature, key) = signed_worker_image(&executable_bytes);
    let installed = supervisor
        .image_registry_mut()
        .install_verified(&ImageInstallRequest {
            manifest: &manifest,
            signature: &signature,
            executable: &executable_bytes,
            trusted_public_key: &key,
        })
        .unwrap();
    supervisor
        .image_registry_mut()
        .promote_verified(&installed.image_id, &key)
        .unwrap();
    assert_eq!(
        supervisor.status(&root).unwrap().image_id,
        original.image_id
    );

    let replacement = supervisor.restart(&root).unwrap();
    assert_eq!(replacement.generation, Generation::new(2));
    assert_eq!(replacement.image_id, installed.image_id);
    assert_eq!(
        replacement.source_manifest_sha256,
        installed.source_manifest_sha256
    );
    supervisor.drain(&root).unwrap();
}

#[test]
#[cfg(unix)]
fn worker_control_endpoint_survives_a_long_state_path() {
    let directory = tempfile::tempdir().unwrap();
    let long_component = "state-directory-segment-that-forces-the-unix-socket-path-past-its-limit";
    let state_directory = directory.path().join(long_component).join(long_component);
    let executable = env!("CARGO_BIN_EXE_keith-worker-process-host");
    let root = RootTreeId::new();
    let mut supervisor = WorkerSupervisor::open(&state_directory, executable, options()).unwrap();

    let status = supervisor.start(root.clone()).unwrap();
    assert_eq!(status.health, WorkerHealth::Healthy);
    supervisor.drain(&root).unwrap();
}
