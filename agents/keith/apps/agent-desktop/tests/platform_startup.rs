use std::fs;
use std::path::PathBuf;
use std::time::Duration;

use keith_agent_desktop::{
    DesktopBootstrap, DesktopLifecycle, DesktopProcessConfig, ProcessOwnership,
};

#[test]
fn platform_desktop_startup_connects_and_stops_owned_daemon() {
    let directory = tempfile::tempdir().unwrap();
    let state = directory.path().join("state");
    let data = directory.path().join("data");
    let settings = DesktopBootstrap::initialize(&state, &data, "http://127.0.0.1:37432").unwrap();
    let assets = directory.path().join("assets");
    fs::create_dir(&assets).unwrap();
    fs::write(assets.join("agent_web.js"), b"packaged javascript").unwrap();
    fs::write(assets.join("agent_web_bg.wasm"), b"packaged wasm").unwrap();
    fs::create_dir_all(assets.join("ui/.vite")).unwrap();
    fs::create_dir_all(assets.join("ui/assets")).unwrap();
    fs::write(assets.join("ui/assets/keith.js"), b"packaged application").unwrap();
    fs::write(assets.join("ui/assets/keith.css"), b"packaged tokens").unwrap();
    fs::write(
        assets.join("ui/.vite/manifest.json"),
        br#"{"src/index.tsx":{"file":"assets/keith.js","isEntry":true,"css":["assets/keith.css"]}}"#,
    )
    .unwrap();
    let config = DesktopProcessConfig {
        settings,
        workspace_root: directory.path().to_path_buf(),
        daemon_executable: PathBuf::from(env!("CARGO_BIN_EXE_keith-desktop-daemon-host")),
        worker_executable: PathBuf::from(env!("CARGO_BIN_EXE_agent-desktop")),
        web_executable: PathBuf::from(env!("CARGO_BIN_EXE_agent-desktop")),
        web_bind: "127.0.0.1:37432".into(),
        asset_root: assets,
        credential_root: directory.path().join("credentials"),
        login_secret_env: "KEITH_TEST_DESKTOP_LOGIN".into(),
        credential_key_env: "KEITH_TEST_DESKTOP_KEY".into(),
        reuse_existing_processes: false,
        startup_timeout: Duration::from_secs(30),
        shutdown_grace: Duration::from_secs(2),
    };
    let mut lifecycle = DesktopLifecycle::new(config).unwrap();
    assert_eq!(lifecycle.ensure_daemon().unwrap(), ProcessOwnership::Owned);
    assert!(lifecycle.connection().list_sessions().unwrap().is_empty());
    lifecycle.stop_owned().unwrap();
}
