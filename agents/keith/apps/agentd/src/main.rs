#![forbid(unsafe_code)]

use std::collections::BTreeMap;
use std::ffi::OsString;
use std::fs::{self, File};
use std::io::Write;
use std::path::PathBuf;
use std::process::{Child, Command, ExitStatus};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;
use std::time::Duration;

use keith_configuration::{PlatformServiceGroup, ServiceEnablementConfig};
use keith_daemon_core::{DaemonCore, DaemonOptions};
use keith_local_runtime::{LocalRuntimeLaunchConfig, RuntimeCredentialKeySource};
use keith_platform::PlatformPaths;
use keith_self_evolution::DaemonStaging;
use signal_hook::consts::{SIGINT, SIGTERM};

struct Arguments {
    data_root: PathBuf,
    socket: PathBuf,
    bootstrap_worker_executable: PathBuf,
    idle_seconds: u64,
    credential_root: PathBuf,
    credential_key_source: CredentialKeySource,
    workspace_root: PathBuf,
    openai_base_url: String,
    anthropic_base_url: String,
    provider_base_urls: BTreeMap<String, String>,
    services: ServiceEnablementConfig,
}

enum CredentialKeySource {
    Environment(String),
    Native(String),
    Restricted(PathBuf),
}

const CHILD_ENV: &str = "KEITH_DAEMON_CHILD";
const LAUNCHER_PID_ENV: &str = "KEITH_DAEMON_LAUNCHER_PID";
const READY_PATH_ENV: &str = "KEITH_DAEMON_READY_PATH";
const READY_IMAGE_ENV: &str = "KEITH_DAEMON_READY_IMAGE";
const READY_TIMEOUT: Duration = Duration::from_secs(30);
const READY_POLL: Duration = Duration::from_millis(20);
const READY_RETRY_INITIAL: Duration = Duration::from_millis(250);
const READY_RETRY_MAX: Duration = Duration::from_secs(2);
const GRACEFUL_SHUTDOWN_TIMEOUT: Duration = Duration::from_secs(10);

impl Arguments {
    #[allow(clippy::too_many_lines)]
    fn parse<I, S>(arguments: I) -> Result<Option<Self>, String>
    where
        I: IntoIterator<Item = S>,
        S: Into<OsString>,
    {
        let mut arguments = arguments.into_iter().map(Into::into);
        let program = arguments.next().unwrap_or_else(|| OsString::from("agentd"));
        let mut data_root = None;
        let mut socket = None;
        let mut worker_executable = None;
        let mut idle_seconds = 15 * 60;
        let mut credential_root = None;
        let mut credential_key_source = None;
        let mut workspace_root = None;
        let mut openai_base_url = "https://api.openai.com".to_owned();
        let mut anthropic_base_url = "https://api.anthropic.com".to_owned();
        let mut provider_base_urls = BTreeMap::new();
        let mut services = ServiceEnablementConfig::default();
        while let Some(argument) = arguments.next() {
            let argument = argument
                .into_string()
                .map_err(|_| "arguments must be UTF-8".to_owned())?;
            if matches!(argument.as_str(), "--version" | "-V") {
                println!("{} {}", env!("CARGO_BIN_NAME"), env!("CARGO_PKG_VERSION"));
                return Ok(None);
            }
            if argument == "--build-info" {
                let report = keith_build_info::daemon_report();
                println!(
                    "{}",
                    serde_json::to_string_pretty(&report).map_err(|error| error.to_string())?
                );
                return Ok(None);
            }
            let value = arguments
                .next()
                .ok_or_else(|| format!("missing value for {argument}"))?;
            match argument.as_str() {
                "--data-root" => data_root = Some(PathBuf::from(value)),
                "--socket" => socket = Some(PathBuf::from(value)),
                "--worker-executable" => worker_executable = Some(PathBuf::from(value)),
                "--credential-root" => credential_root = Some(PathBuf::from(value)),
                "--credential-key-env" => {
                    credential_key_source = Some(CredentialKeySource::Environment(
                        value
                            .into_string()
                            .map_err(|_| "credential key environment must be UTF-8".to_owned())?,
                    ));
                }
                "--credential-key-native-account" => {
                    credential_key_source =
                        Some(CredentialKeySource::Native(value.into_string().map_err(
                            |_| "native key account must be UTF-8".to_owned(),
                        )?));
                }
                "--workspace-root" => workspace_root = Some(PathBuf::from(value)),
                "--openai-base-url" => {
                    openai_base_url = value
                        .into_string()
                        .map_err(|_| "OpenAI base URL must be UTF-8".to_owned())?;
                }
                "--anthropic-base-url" => {
                    anthropic_base_url = value
                        .into_string()
                        .map_err(|_| "Anthropic base URL must be UTF-8".to_owned())?;
                }
                "--provider-base-url" => {
                    let value = value
                        .into_string()
                        .map_err(|_| "provider base URL must be UTF-8".to_owned())?;
                    let (provider, base_url) = value.split_once('=').ok_or_else(|| {
                        "provider base URL must use the form PROVIDER=https://endpoint".to_owned()
                    })?;
                    if provider.trim().is_empty() || base_url.trim().is_empty() {
                        return Err(
                            "provider base URL must use the form PROVIDER=https://endpoint".into(),
                        );
                    }
                    if provider_base_urls
                        .insert(provider.to_owned(), base_url.to_owned())
                        .is_some()
                    {
                        return Err(format!("provider base URL for {provider} was repeated"));
                    }
                }
                "--enable-service" => {
                    let service = value
                        .into_string()
                        .map_err(|_| "service name must be UTF-8".to_owned())?;
                    let service = match service.as_str() {
                        "channels" => PlatformServiceGroup::Channels,
                        "acp" => PlatformServiceGroup::Acp,
                        "plugins" => PlatformServiceGroup::Plugins,
                        "connected_apps" => PlatformServiceGroup::ConnectedApps,
                        "computers" => PlatformServiceGroup::Computers,
                        "teaching" => PlatformServiceGroup::Teaching,
                        _ => return Err(format!("unknown service {service}")),
                    };
                    services.set_enabled(service, true);
                }
                "--idle-seconds" => {
                    idle_seconds = value
                        .into_string()
                        .map_err(|_| "idle seconds must be UTF-8".to_owned())?
                        .parse()
                        .map_err(|_| "idle seconds must be an integer".to_owned())?;
                }
                _ => return Err(format!("unknown argument {argument}")),
            }
        }
        let platform_paths = if data_root.is_none() {
            Some(PlatformPaths::discover().map_err(|error| error.to_string())?)
        } else {
            None
        };
        let data_root = data_root
            .or_else(|| platform_paths.as_ref().map(|paths| paths.data_root.clone()))
            .ok_or_else(|| "native data root is unavailable".to_owned())?;
        let socket = socket.unwrap_or_else(|| {
            platform_paths.as_ref().map_or_else(
                || data_root.join("agentd.sock"),
                |paths| paths.daemon_endpoint.clone(),
            )
        });
        let worker_executable = worker_executable.unwrap_or_else(|| {
            let mut sibling = PathBuf::from(program);
            sibling.set_file_name("agent-worker");
            sibling
        });
        let credential_root = credential_root.unwrap_or_else(|| data_root.join("credentials"));
        let credential_key_source = credential_key_source
            .unwrap_or_else(|| CredentialKeySource::Restricted(credential_root.clone()));
        let workspace_root = workspace_root
            .map_or_else(std::env::current_dir, Ok)
            .map_err(|error| error.to_string())?;
        Ok(Some(Self {
            data_root,
            socket,
            bootstrap_worker_executable: worker_executable,
            idle_seconds,
            credential_root,
            credential_key_source,
            workspace_root,
            openai_base_url,
            anthropic_base_url,
            provider_base_urls,
            services,
        }))
    }
}

fn run_child() -> Result<(), String> {
    let Some(arguments) = Arguments::parse(std::env::args_os())? else {
        return Ok(());
    };
    let shutdown = Arc::new(AtomicBool::new(false));
    signal_hook::flag::register(SIGTERM, Arc::clone(&shutdown))
        .map_err(|error| format!("failed to register SIGTERM: {error}"))?;
    signal_hook::flag::register(SIGINT, Arc::clone(&shutdown))
        .map_err(|error| format!("failed to register SIGINT: {error}"))?;
    let _launcher_watch = spawn_launcher_watch(Arc::clone(&shutdown), arguments.socket.clone())?;
    let options = DaemonOptions {
        idle_evict_after: Duration::from_secs(arguments.idle_seconds),
        evolution_source_root: Some(arguments.workspace_root.clone()),
        services: arguments.services.clone(),
        ..DaemonOptions::default()
    };
    let runtime = LocalRuntimeLaunchConfig {
        data_root: arguments.data_root.clone(),
        credential_root: arguments.credential_root,
        credential_key_source: match arguments.credential_key_source {
            CredentialKeySource::Environment(environment) => {
                RuntimeCredentialKeySource::Environment(environment)
            }
            CredentialKeySource::Native(account) => RuntimeCredentialKeySource::Native(account),
            CredentialKeySource::Restricted(root) => RuntimeCredentialKeySource::Restricted(root),
        },
        workspace_root: arguments.workspace_root,
        openai_base_url: arguments.openai_base_url,
        anthropic_base_url: arguments.anthropic_base_url,
        provider_base_urls: arguments.provider_base_urls,
    };
    let runtime_config = write_runtime_config(&arguments.data_root, &runtime)?;
    let mut daemon = DaemonCore::open_with_worker_runtime(
        &arguments.data_root,
        arguments.bootstrap_worker_executable,
        options,
        runtime_config,
    )
    .map_err(|error| error.to_string())?;
    let ready_path = std::env::var_os(READY_PATH_ENV).map(PathBuf::from);
    let ready_image = std::env::var(READY_IMAGE_ENV).ok();
    let result = daemon
        .serve_local_with_ready(&arguments.socket, &shutdown, || {
            if let (Some(path), Some(image_id)) = (&ready_path, &ready_image) {
                write_ready(path, image_id)?;
            }
            Ok(())
        })
        .map_err(|error| error.to_string());
    shutdown.store(true, Ordering::Release);
    result
}

#[allow(clippy::too_many_lines)]
fn run_launcher() -> Result<(), String> {
    let original_arguments = std::env::args_os().skip(1).collect::<Vec<_>>();
    if original_arguments
        .iter()
        .any(|argument| argument == "--version" || argument == "-V" || argument == "--build-info")
    {
        return run_child();
    }
    let data_root = launcher_data_root(&original_arguments)?;
    let socket = launcher_socket(&original_arguments, &data_root)?;
    let staging_root = data_root.join("self-evolution").join("daemon-images");
    let bootstrap = std::env::current_exe().map_err(|error| error.to_string())?;
    let mut staging =
        DaemonStaging::open(&staging_root, &bootstrap).map_err(|error| error.to_string())?;
    withdraw_owned_socket(&socket);
    let shutdown = Arc::new(AtomicBool::new(false));
    signal_hook::flag::register(SIGTERM, Arc::clone(&shutdown))
        .map_err(|error| format!("failed to register launcher SIGTERM: {error}"))?;
    signal_hook::flag::register(SIGINT, Arc::clone(&shutdown))
        .map_err(|error| format!("failed to register launcher SIGINT: {error}"))?;
    let readiness_retry_deadline = std::time::Instant::now() + READY_TIMEOUT;
    let mut readiness_retry_delay = READY_RETRY_INITIAL;

    loop {
        let selection = staging
            .launch_selection()
            .map_err(|error| error.to_string())?;
        let ready_path = staging_root.join(format!(
            ".ready-{}-{}",
            std::process::id(),
            selection.image.image_id
        ));
        remove_ready(&ready_path)?;
        let spawned = Command::new(&selection.image.executable)
            .args(&original_arguments)
            .env(CHILD_ENV, "1")
            .env(LAUNCHER_PID_ENV, std::process::id().to_string())
            .env(READY_PATH_ENV, &ready_path)
            .env(READY_IMAGE_ENV, &selection.image.image_id)
            .spawn();
        let mut child = match spawned {
            Ok(child) => child,
            Err(error) if selection.candidate => {
                let reason = format!(
                    "failed to launch daemon image {}: {error}",
                    selection.image.image_id
                );
                staging
                    .fail_and_restore(&selection.image.image_id, &reason)
                    .map_err(|restore| format!("{reason}; pinned restore failed: {restore}"))?;
                continue;
            }
            Err(error) => {
                return Err(format!(
                    "failed to launch daemon image {}: {error}",
                    selection.image.image_id
                ));
            }
        };

        match await_ready(
            &mut child,
            &ready_path,
            &selection.image.image_id,
            &shutdown,
        ) {
            Ok(()) => {
                if selection.candidate
                    && let Err(error) = staging.mark_ready(&selection.image.image_id)
                {
                    let _ = child.kill();
                    let _ = child.wait();
                    remove_ready(&ready_path)?;
                    let reason = format!("candidate readiness could not be committed: {error}");
                    staging
                        .fail_and_restore(&selection.image.image_id, &reason)
                        .map_err(|restore| format!("{reason}; pinned restore failed: {restore}"))?;
                    continue;
                }
                remove_ready(&ready_path)?;
                let status = wait_for_exit(&mut child, &shutdown)?;
                if shutdown.load(std::sync::atomic::Ordering::Acquire) {
                    return Ok(());
                }
                if !selection.candidate {
                    return exit_status(status);
                }
                staging
                    .fail_and_restore(
                        &selection.image.image_id,
                        &format!("candidate daemon exited after readiness with {status}"),
                    )
                    .map_err(|error| error.to_string())?;
            }
            Err(reason) if selection.candidate => {
                let _ = child.kill();
                let _ = child.wait();
                remove_ready(&ready_path)?;
                if shutdown.load(std::sync::atomic::Ordering::Acquire) {
                    return Ok(());
                }
                staging
                    .fail_and_restore(&selection.image.image_id, &reason)
                    .map_err(|error| error.to_string())?;
            }
            Err(reason) => {
                let _ = child.kill();
                let _ = child.wait();
                remove_ready(&ready_path)?;
                if shutdown.load(std::sync::atomic::Ordering::Acquire) {
                    return Ok(());
                }
                if std::time::Instant::now() < readiness_retry_deadline {
                    let remaining = readiness_retry_deadline
                        .saturating_duration_since(std::time::Instant::now());
                    thread::sleep(readiness_retry_delay.min(remaining));
                    readiness_retry_delay =
                        readiness_retry_delay.saturating_mul(2).min(READY_RETRY_MAX);
                    continue;
                }
                return Err(reason);
            }
        }
    }
}

fn spawn_launcher_watch(
    shutdown: Arc<AtomicBool>,
    socket: PathBuf,
) -> Result<Option<thread::JoinHandle<()>>, String> {
    let Some(encoded) = std::env::var_os(LAUNCHER_PID_ENV) else {
        return Ok(None);
    };
    let launcher_pid = encoded
        .to_string_lossy()
        .parse::<u32>()
        .map_err(|_| "daemon launcher PID is invalid".to_owned())?;
    if launcher_pid == 0 || launcher_pid == std::process::id() {
        return Err("daemon launcher PID is invalid".into());
    }
    Ok(Some(thread::spawn(move || {
        while !shutdown.load(Ordering::Acquire) {
            if !launcher_process_is_alive(launcher_pid) {
                terminate_orphaned_daemon(&socket);
            }
            thread::sleep(READY_POLL);
        }
    })))
}

fn terminate_orphaned_daemon(socket: &std::path::Path) -> ! {
    withdraw_owned_socket(socket);
    std::process::exit(70);
}

fn withdraw_owned_socket(socket: &std::path::Path) {
    match fs::remove_file(socket) {
        Ok(()) => {
            if let Some(parent) = socket.parent()
                && let Ok(directory) = File::open(parent)
            {
                let _ = directory.sync_all();
            }
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(_) => {}
    }
}

#[cfg(target_os = "linux")]
fn launcher_process_is_alive(pid: u32) -> bool {
    fs::read_to_string(PathBuf::from("/proc").join(pid.to_string()).join("stat"))
        .ok()
        .and_then(|stat| stat.split_whitespace().nth(2).map(str::to_owned))
        .is_some_and(|state| state != "Z")
}

#[cfg(all(unix, not(target_os = "linux")))]
fn launcher_process_is_alive(pid: u32) -> bool {
    Command::new("kill")
        .args(["-0", &pid.to_string()])
        .status()
        .is_ok_and(|status| status.success())
}

#[cfg(windows)]
fn launcher_process_is_alive(pid: u32) -> bool {
    Command::new("tasklist")
        .args(["/FI", &format!("PID eq {pid}"), "/FO", "CSV", "/NH"])
        .output()
        .is_ok_and(|output| {
            String::from_utf8_lossy(&output.stdout).lines().any(|line| {
                line.split(',')
                    .nth(1)
                    .is_some_and(|value| value.trim_matches('"') == pid.to_string())
            })
        })
}

#[cfg(not(any(unix, windows)))]
const fn launcher_process_is_alive(_pid: u32) -> bool {
    false
}

fn launcher_data_root(arguments: &[OsString]) -> Result<PathBuf, String> {
    let mut index = 0;
    while index < arguments.len() {
        if arguments[index] == "--data-root" {
            return arguments
                .get(index + 1)
                .map(PathBuf::from)
                .ok_or_else(|| "missing value for --data-root".to_owned());
        }
        index = index.saturating_add(2);
    }
    PlatformPaths::discover()
        .map(|paths| paths.data_root)
        .map_err(|error| error.to_string())
}

fn launcher_socket(arguments: &[OsString], data_root: &std::path::Path) -> Result<PathBuf, String> {
    let mut index = 0;
    while index < arguments.len() {
        if arguments[index] == "--socket" {
            return arguments
                .get(index + 1)
                .map(PathBuf::from)
                .ok_or_else(|| "missing value for --socket".to_owned());
        }
        index = index.saturating_add(2);
    }
    Ok(data_root.join("agentd.sock"))
}

fn await_ready(
    child: &mut std::process::Child,
    ready_path: &std::path::Path,
    image_id: &str,
    shutdown: &AtomicBool,
) -> Result<(), String> {
    let deadline = std::time::Instant::now() + READY_TIMEOUT;
    loop {
        if let Some(status) = child.try_wait().map_err(|error| error.to_string())? {
            return Err(format!(
                "candidate daemon exited before readiness with {status}"
            ));
        }
        if shutdown.load(std::sync::atomic::Ordering::Acquire) {
            let _ = stop_child(child);
            return Err("daemon launcher was asked to shut down".into());
        }
        match fs::read_to_string(ready_path) {
            Ok(value) if value == image_id => return Ok(()),
            Ok(_) => return Err("daemon readiness identity did not match its image".into()),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(format!("failed to read daemon readiness: {error}")),
        }
        if std::time::Instant::now() >= deadline {
            return Err("daemon readiness timed out".into());
        }
        thread::sleep(READY_POLL);
    }
}

fn wait_for_exit(
    child: &mut std::process::Child,
    shutdown: &AtomicBool,
) -> Result<ExitStatus, String> {
    loop {
        if let Some(status) = child.try_wait().map_err(|error| error.to_string())? {
            return Ok(status);
        }
        if shutdown.load(std::sync::atomic::Ordering::Acquire) {
            return stop_child(child);
        }
        thread::sleep(READY_POLL);
    }
}

fn stop_child(child: &mut Child) -> Result<ExitStatus, String> {
    request_child_shutdown(child)?;
    let deadline = std::time::Instant::now() + GRACEFUL_SHUTDOWN_TIMEOUT;
    loop {
        if let Some(status) = child.try_wait().map_err(|error| error.to_string())? {
            return Ok(status);
        }
        if std::time::Instant::now() >= deadline {
            child.kill().map_err(|error| error.to_string())?;
            return child.wait().map_err(|error| error.to_string());
        }
        thread::sleep(READY_POLL);
    }
}

#[cfg(unix)]
fn request_child_shutdown(child: &Child) -> Result<(), String> {
    let pid = i32::try_from(child.id()).map_err(|_| "daemon child PID is invalid".to_owned())?;
    nix::sys::signal::kill(
        nix::unistd::Pid::from_raw(pid),
        nix::sys::signal::Signal::SIGTERM,
    )
    .map_err(|error| error.to_string())
}

#[cfg(not(unix))]
fn request_child_shutdown(child: &mut Child) -> Result<(), String> {
    child.kill().map_err(|error| error.to_string())
}

fn write_ready(path: &std::path::Path, image_id: &str) -> Result<(), std::io::Error> {
    let mut file = File::options().create_new(true).write(true).open(path)?;
    file.write_all(image_id.as_bytes())?;
    file.sync_all()?;
    File::open(path.parent().unwrap_or_else(|| std::path::Path::new(".")))?.sync_all()
}

fn remove_ready(path: &std::path::Path) -> Result<(), String> {
    match fs::remove_file(path) {
        Ok(()) => File::open(path.parent().unwrap_or_else(|| std::path::Path::new(".")))
            .and_then(|directory| directory.sync_all())
            .map_err(|error| error.to_string()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error.to_string()),
    }
}

fn exit_status(status: ExitStatus) -> Result<(), String> {
    if status.success() {
        Ok(())
    } else {
        Err(format!("daemon exited with {status}"))
    }
}

fn run() -> Result<(), String> {
    if std::env::var_os(CHILD_ENV).is_some() {
        run_child()
    } else {
        run_launcher()
    }
}

fn write_runtime_config(
    data_root: &std::path::Path,
    runtime: &LocalRuntimeLaunchConfig,
) -> Result<PathBuf, String> {
    let directory = data_root.join("runtime");
    fs::create_dir_all(&directory).map_err(|error| error.to_string())?;
    let path = directory.join("worker-runtime.json");
    let temporary = directory.join(format!(".worker-runtime.{}.tmp", std::process::id()));
    let bytes = serde_json::to_vec(runtime).map_err(|error| error.to_string())?;
    let mut file = File::create(&temporary).map_err(|error| error.to_string())?;
    file.write_all(&bytes).map_err(|error| error.to_string())?;
    file.sync_all().map_err(|error| error.to_string())?;
    keith_platform::replace_file(&temporary, &path).map_err(|error| error.to_string())?;
    File::open(&directory)
        .and_then(|directory| directory.sync_all())
        .map_err(|error| error.to_string())?;
    Ok(path)
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
