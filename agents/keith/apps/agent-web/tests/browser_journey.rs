#![cfg(unix)]

use std::path::PathBuf;
use std::process::Command;

#[test]
#[ignore = "requires a running real Keith stack plus KEITH_PLAYWRIGHT_MODULE, KEITH_WEB_ORIGIN, and KEITH_WEB_LOGIN_SECRET"]
fn chromium_real_daemon_login_send_response_and_security_journey() {
    for name in [
        "KEITH_PLAYWRIGHT_MODULE",
        "KEITH_WEB_ORIGIN",
        "KEITH_WEB_LOGIN_SECRET",
    ] {
        assert!(
            std::env::var_os(name).is_some(),
            "{name} must be configured"
        );
    }
    let status = Command::new("node")
        .arg(
            PathBuf::from(env!("CARGO_MANIFEST_DIR"))
                .join("tests")
                .join("browser_journey.cjs"),
        )
        .status()
        .unwrap();
    assert!(status.success());
}
