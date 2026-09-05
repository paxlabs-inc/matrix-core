#![cfg(unix)]

use std::fs;
use std::path::PathBuf;
use std::process::Command;
use std::thread;
use std::time::Duration;

use keith_agent_desktop::{
    DesktopBootstrap, DesktopLifecycle, DesktopProcessConfig, ManagedProcessKind, ProcessOwnership,
};
use nix::sys::signal::{Signal, kill};
use nix::unistd::Pid;

fn config(directory: &tempfile::TempDir) -> DesktopProcessConfig {
    let state = directory.path().join("state");
    let data = directory.path().join("data");
    let settings = DesktopBootstrap::initialize(&state, &data, "http://127.0.0.1:37431").unwrap();
    let assets = directory.path().join("assets");
    fs::create_dir(&assets).unwrap();
    fs::create_dir_all(assets.join("ui/_next/static/chunks")).unwrap();
    fs::write(
        assets.join("ui/_next/static/chunks/keith.js"),
        b"packaged application",
    )
    .unwrap();
    fs::write(
        assets.join("ui/index.html"),
        br#"<!DOCTYPE html><main>Opening Keith</main><script src="/assets/ui/_next/static/chunks/keith.js"></script>"#,
    )
    .unwrap();
    DesktopProcessConfig {
        settings,
        workspace_root: directory.path().to_path_buf(),
        daemon_executable: PathBuf::from(env!("CARGO_BIN_EXE_keith-desktop-daemon-host")),
        worker_executable: PathBuf::from(env!("CARGO_BIN_EXE_agent-desktop")),
        web_executable: PathBuf::from(env!("CARGO_BIN_EXE_agent-desktop")),
        web_bind: "127.0.0.1:37431".into(),
        asset_root: assets,
        credential_root: directory.path().join("credentials"),
        login_secret_env: "KEITH_TEST_DESKTOP_LOGIN".into(),
        credential_key_env: "KEITH_TEST_DESKTOP_KEY".into(),
        reuse_existing_processes: true,
        startup_timeout: Duration::from_secs(5),
        shutdown_grace: Duration::from_secs(2),
    }
}

#[test]
fn startup_existing_daemon_crash_report_restart_and_graceful_stop_use_real_processes() {
    let directory = tempfile::tempdir().unwrap();
    let config = config(&directory);
    let mut owner = DesktopLifecycle::new(config.clone()).unwrap();
    assert_eq!(owner.ensure_daemon().unwrap(), ProcessOwnership::Owned);
    assert!(owner.connection().list_sessions().unwrap().is_empty());

    let mut attaching = DesktopLifecycle::new(config.clone()).unwrap();
    assert_eq!(
        attaching.ensure_daemon().unwrap(),
        ProcessOwnership::Existing
    );

    let socket = config.settings.daemon_socket.clone();
    let pid_output = Command::new("fuser").arg(&socket).output().unwrap();
    let pid = String::from_utf8_lossy(&pid_output.stdout)
        .split_whitespace()
        .find_map(|value| value.parse::<i32>().ok())
        .expect("real daemon pid");
    kill(Pid::from_raw(pid), Signal::SIGKILL).unwrap();
    let mut reported = false;
    for _ in 0..100 {
        let reports = owner.poll_crashes().unwrap();
        if !reports.is_empty() {
            assert_eq!(reports[0].process, ManagedProcessKind::Daemon);
            reported = true;
            break;
        }
        thread::sleep(Duration::from_millis(10));
    }
    assert!(reported);
    assert_eq!(owner.ensure_daemon().unwrap(), ProcessOwnership::Owned);
    assert!(owner.connection().list_sessions().unwrap().is_empty());
    owner.stop_owned().unwrap();
    assert!(!socket.exists());
}
