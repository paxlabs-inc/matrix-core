use std::time::Duration;

use keith_agent_types::{Generation, RootTreeId};
#[cfg(unix)]
use keith_supervisor::WorkerHealth;
use keith_supervisor::{SupervisorOptions, WorkerEvent, WorkerSupervisor};
use keith_worker_runtime::LeaseManager;
#[cfg(unix)]
use nix::sys::signal::{Signal, kill};
#[cfg(unix)]
use nix::unistd::Pid;

fn options() -> SupervisorOptions {
    SupervisorOptions {
        startup_timeout: Duration::from_secs(3),
        drain_timeout: Duration::from_secs(1),
        stale_heartbeat: Duration::from_secs(1),
        heartbeat_interval: Duration::from_millis(20),
        lease_duration: Duration::from_millis(500),
    }
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
