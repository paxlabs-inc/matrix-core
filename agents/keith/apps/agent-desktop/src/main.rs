use std::path::PathBuf;
use std::process::ExitCode;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;
use std::time::Duration;

use keith_agent_desktop::{
    BrowserHandoff, DesktopBootstrap, DesktopLifecycle, DesktopProcessConfig, DesktopUpdateManager,
    UninstallChoice, backup_state, execute_uninstall, plan_uninstall, restore_state,
};
use keith_agent_types::UtcTimestamp;
use signal_hook::consts::{SIGINT, SIGTERM};

fn main() -> ExitCode {
    match run() {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("agent-desktop: {error}");
            ExitCode::FAILURE
        }
    }
}

#[allow(clippy::too_many_lines)]
fn run() -> Result<(), String> {
    let mut arguments = std::env::args_os();
    let _program = arguments.next();
    let Some(command) = arguments.next() else {
        return Err(usage().into());
    };
    if matches!(command.to_str(), Some("--version" | "-V")) {
        println!("{} {}", env!("CARGO_BIN_NAME"), env!("CARGO_PKG_VERSION"));
        return Ok(());
    }
    match command.to_str() {
        Some("setup-default") => {
            let origin = arguments
                .next()
                .and_then(|value| value.into_string().ok())
                .unwrap_or_else(|| "http://127.0.0.1:7341".into());
            DesktopBootstrap::initialize_default(&origin).map_err(|error| error.to_string())?;
            Ok(())
        }
        Some("setup") => {
            let state_root = arguments
                .next()
                .map(PathBuf::from)
                .ok_or_else(|| "setup requires STATE_ROOT".to_owned())?;
            let data_root = arguments
                .next()
                .map(PathBuf::from)
                .ok_or_else(|| "setup requires DATA_ROOT".to_owned())?;
            let origin = arguments
                .next()
                .and_then(|value| value.into_string().ok())
                .unwrap_or_else(|| "http://127.0.0.1:7341".into());
            DesktopBootstrap::initialize(&state_root, &data_root, &origin)
                .map_err(|error| error.to_string())?;
            Ok(())
        }
        Some("open") => {
            let origin = arguments
                .next()
                .and_then(|value| value.into_string().ok())
                .ok_or_else(|| "open requires ORIGIN".to_owned())?;
            let path = arguments
                .next()
                .and_then(|value| value.into_string().ok())
                .unwrap_or_else(|| "/".into());
            BrowserHandoff::new(&origin, &path)
                .and_then(|handoff| handoff.open())
                .map_err(|error| error.to_string())
        }
        Some("settings") => {
            let state_root = required_path(&mut arguments, "settings requires STATE_ROOT")?;
            let settings =
                DesktopBootstrap::load(&state_root).map_err(|error| error.to_string())?;
            println!(
                "{}",
                serde_json::to_string_pretty(&settings).map_err(|error| error.to_string())?
            );
            Ok(())
        }
        Some("backup") => {
            let state_root = required_path(&mut arguments, "backup requires STATE_ROOT")?;
            let settings =
                DesktopBootstrap::load(&state_root).map_err(|error| error.to_string())?;
            let backup = backup_state(&settings).map_err(|error| error.to_string())?;
            println!("{}", backup.display());
            Ok(())
        }
        Some("restore") => {
            let backup = required_path(&mut arguments, "restore requires BACKUP")?;
            let target = required_path(&mut arguments, "restore requires TARGET_DATA_ROOT")?;
            restore_state(&backup, &target).map_err(|error| error.to_string())
        }
        Some("digest-release") => {
            let release =
                required_path(&mut arguments, "digest-release requires RELEASE_DIRECTORY")?;
            let digest = DesktopUpdateManager::digest_release(&release)
                .map_err(|error| error.to_string())?;
            println!("{digest}");
            Ok(())
        }
        Some("verify-release") => {
            let release =
                required_path(&mut arguments, "verify-release requires RELEASE_DIRECTORY")?;
            let encoded_key = required_string(
                &mut arguments,
                "verify-release requires EXPECTED_PUBLIC_KEY_HEX",
            )?;
            let public_key = keith_release::decode_public_key(&encoded_key)
                .map_err(|error| error.to_string())?;
            let verified = keith_release::verify_release(&release, &public_key)
                .map_err(|error| error.to_string())?;
            if verified.manifest.target
                != format!("{}-{}", std::env::consts::ARCH, std::env::consts::OS)
            {
                return Err("release target does not match this host".into());
            }
            keith_release::verify_packaged_build_reports(&release, &verified.manifest)
                .map_err(|error| error.to_string())?;
            println!(
                "{}",
                serde_json::to_string_pretty(&verified).map_err(|error| error.to_string())?
            );
            Ok(())
        }
        Some("update") => {
            let state_root = required_path(&mut arguments, "update requires STATE_ROOT")?;
            let release = required_path(&mut arguments, "update requires RELEASE_DIRECTORY")?;
            let public_key =
                required_string(&mut arguments, "update requires EXPECTED_PUBLIC_KEY_HEX")?;
            DesktopBootstrap::load(&state_root).map_err(|error| error.to_string())?;
            let manager =
                DesktopUpdateManager::open(&state_root).map_err(|error| error.to_string())?;
            let active = manager
                .activate(
                    &release,
                    &public_key,
                    UtcTimestamp::now().map_err(|error| error.to_string())?,
                )
                .map_err(|error| error.to_string())?;
            println!(
                "{}",
                serde_json::to_string_pretty(&active).map_err(|error| error.to_string())?
            );
            Ok(())
        }
        Some("rollback") => {
            let state_root = required_path(&mut arguments, "rollback requires STATE_ROOT")?;
            DesktopBootstrap::load(&state_root).map_err(|error| error.to_string())?;
            let manager =
                DesktopUpdateManager::open(&state_root).map_err(|error| error.to_string())?;
            let active = manager
                .rollback(UtcTimestamp::now().map_err(|error| error.to_string())?)
                .map_err(|error| error.to_string())?;
            println!(
                "{}",
                serde_json::to_string_pretty(&active).map_err(|error| error.to_string())?
            );
            Ok(())
        }
        Some("serve") => {
            let state_root = required_path(&mut arguments, "serve requires STATE_ROOT")?;
            let workspace_root = required_path(&mut arguments, "serve requires WORKSPACE_ROOT")?;
            let web_bind = required_string(&mut arguments, "serve requires WEB_BIND")?;
            let login_secret_env = arguments
                .next()
                .and_then(|value| value.into_string().ok())
                .unwrap_or_else(|| "KEITH_WEB_LOGIN_SECRET".into());
            let credential_key_env = arguments
                .next()
                .and_then(|value| value.into_string().ok())
                .unwrap_or_else(|| "KEITH_CREDENTIAL_KEY".into());
            serve_active_release(
                &state_root,
                workspace_root,
                web_bind,
                login_secret_env,
                credential_key_env,
            )
        }
        Some("uninstall-plan") => {
            let state_root = required_path(&mut arguments, "uninstall-plan requires STATE_ROOT")?;
            let choice = uninstall_choice(&required_string(
                &mut arguments,
                "uninstall-plan requires DATA_CHOICE",
            )?)?;
            let settings =
                DesktopBootstrap::load(&state_root).map_err(|error| error.to_string())?;
            println!(
                "{}",
                serde_json::to_string_pretty(&plan_uninstall(&settings, choice))
                    .map_err(|error| error.to_string())?
            );
            Ok(())
        }
        Some("uninstall") => {
            let state_root = required_path(&mut arguments, "uninstall requires STATE_ROOT")?;
            let choice = uninstall_choice(&required_string(
                &mut arguments,
                "uninstall requires DATA_CHOICE",
            )?)?;
            let confirmation = required_string(&mut arguments, "uninstall requires CONFIRMATION")?;
            let settings =
                DesktopBootstrap::load(&state_root).map_err(|error| error.to_string())?;
            let plan = plan_uninstall(&settings, choice);
            execute_uninstall(&settings, &plan, &confirmation).map_err(|error| error.to_string())
        }
        _ => Err(usage().into()),
    }
}

fn required_path(
    arguments: &mut impl Iterator<Item = std::ffi::OsString>,
    message: &str,
) -> Result<PathBuf, String> {
    arguments
        .next()
        .map(PathBuf::from)
        .ok_or_else(|| message.into())
}

fn required_string(
    arguments: &mut impl Iterator<Item = std::ffi::OsString>,
    message: &str,
) -> Result<String, String> {
    arguments
        .next()
        .and_then(|value| value.into_string().ok())
        .ok_or_else(|| message.into())
}

fn uninstall_choice(value: &str) -> Result<UninstallChoice, String> {
    match value {
        "keep-user-data" => Ok(UninstallChoice::KeepUserData),
        "remove-runtime" => Ok(UninstallChoice::RemoveRuntime),
        "remove-everything" => Ok(UninstallChoice::RemoveEverything),
        _ => Err("DATA_CHOICE must be keep-user-data, remove-runtime, or remove-everything".into()),
    }
}

fn serve_active_release(
    state_root: &std::path::Path,
    workspace_root: PathBuf,
    web_bind: String,
    login_secret_env: String,
    credential_key_env: String,
) -> Result<(), String> {
    if std::env::var_os(&login_secret_env).is_none()
        || std::env::var_os(&credential_key_env).is_none()
    {
        return Err("the configured login and credential-key environments must both be set".into());
    }
    let settings = DesktopBootstrap::load(state_root).map_err(|error| error.to_string())?;
    let release = DesktopUpdateManager::open(state_root)
        .and_then(|manager| manager.active_release_root())
        .map_err(|error| error.to_string())?;
    let executable = |name: &str| {
        release
            .join("bin")
            .join(format!("{name}{}", std::env::consts::EXE_SUFFIX))
    };
    let mut lifecycle = DesktopLifecycle::new(DesktopProcessConfig {
        workspace_root,
        daemon_executable: executable("agentd"),
        worker_executable: executable("agent-worker"),
        web_executable: executable("agent-web"),
        web_bind,
        asset_root: release.join("web"),
        credential_root: settings.data_root.join("credentials"),
        login_secret_env,
        credential_key_env,
        reuse_existing_processes: false,
        startup_timeout: Duration::from_secs(30),
        shutdown_grace: Duration::from_secs(30),
        settings,
    })
    .map_err(|error| error.to_string())?;
    let shutdown = Arc::new(AtomicBool::new(false));
    signal_hook::flag::register(SIGTERM, Arc::clone(&shutdown))
        .map_err(|error| error.to_string())?;
    signal_hook::flag::register(SIGINT, Arc::clone(&shutdown))
        .map_err(|error| error.to_string())?;
    lifecycle
        .ensure_daemon()
        .map_err(|error| error.to_string())?;
    lifecycle.ensure_web().map_err(|error| error.to_string())?;
    while !shutdown.load(Ordering::Acquire) {
        let crashes = lifecycle
            .poll_crashes()
            .map_err(|error| error.to_string())?;
        if !crashes.is_empty() {
            return Err("a managed Keith process stopped; inspect the bounded crash report".into());
        }
        thread::sleep(Duration::from_millis(100));
    }
    lifecycle.stop_owned().map_err(|error| error.to_string())
}

fn usage() -> &'static str {
    "usage: agent-desktop <setup-default [ORIGIN]|setup STATE_ROOT DATA_ROOT [ORIGIN]|settings STATE_ROOT|backup STATE_ROOT|restore BACKUP TARGET_DATA_ROOT|digest-release RELEASE_DIRECTORY|verify-release RELEASE_DIRECTORY EXPECTED_PUBLIC_KEY_HEX|update STATE_ROOT RELEASE_DIRECTORY EXPECTED_PUBLIC_KEY_HEX|rollback STATE_ROOT|serve STATE_ROOT WORKSPACE_ROOT WEB_BIND [LOGIN_SECRET_ENV] [CREDENTIAL_KEY_ENV]|uninstall-plan STATE_ROOT DATA_CHOICE|uninstall STATE_ROOT DATA_CHOICE CONFIRMATION|open ORIGIN [PATH]>"
}
