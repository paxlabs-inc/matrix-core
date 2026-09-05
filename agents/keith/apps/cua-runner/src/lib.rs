#![forbid(unsafe_code)]

use std::collections::{BTreeMap, VecDeque};
use std::fmt::Write as _;
use std::fs;
use std::io::{self, BufRead, BufReader, BufWriter, Write};
use std::os::fd::AsFd as _;
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, ChildStdout, Command, Stdio};
use std::sync::atomic::{AtomicU64, Ordering};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::UtcTimestamp;
use keith_cua::{
    AccessibilityNode, ActionTarget, ApplicationObservation, CancellationToken, ComputerAction,
    ComputerControlService, ComputerController, ComputerError, ComputerObservation,
    ComputerRuntime, ComputerSession, ComputerSessionLayout, ControlError, DialogObservation,
    DownloadObservation, DownloadState, FocusedWindow, FrameId, IsolationRequirement, MouseButton,
    NetworkPolicy, Point, RecentAction, RunnerCommand, RunnerResponse, RuntimeActionResult,
    Screenshot, SemanticTarget,
};
use keith_platform_contracts::ControlOwner;
use keith_sandbox::{SandboxStatus, configure_owned_process, terminate_owned_process_tree};
use nix::errno::Errno;
use nix::poll::{PollFd, PollFlags, PollTimeout, poll};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use thiserror::Error;
use url::Url;

const MAX_COMMAND_BYTES: usize = 2 * 1_024 * 1_024;
const MAX_CDP_MESSAGE_BYTES: usize = 32 * 1_024 * 1_024;
const BROWSER_BOOT_TIMEOUT: Duration = Duration::from_secs(15);
const PIPE_SCRIPT: &str = "exec 3<&0 4>&1; exec \"$@\"";
const RESOURCE_PIPE_SCRIPT: &str = r#"
printf '%s\n' "$$" > "$1/cgroup.procs" || exit 125
shift
exec 3<&0 4>&1
exec "$@"
"#;
const RESOURCE_EXEC_SCRIPT: &str = r#"
printf '%s\n' "$$" > "$1/cgroup.procs" || exit 125
shift
exec "$@"
"#;
const OWNED_XVFB_SCRIPT: &str = r#"
printf '%s\n' "$$" > "$1/cgroup.procs" || exit 125
shift
owner=$PPID
"$@" &
child=$!
while [ "$PPID" -eq "$owner" ] && kill -0 "$owner" 2>/dev/null; do
  if ! kill -0 "$child" 2>/dev/null; then
    wait "$child"
    exit $?
  fi
  sleep 0.1
done
kill "$child" 2>/dev/null || true
wait "$child" 2>/dev/null || true
"#;
const CGROUP_ROOT: &str = "/sys/fs/cgroup";
const CGROUP_PREFIX: &str = "keith-cua-";
static CGROUP_SEQUENCE: AtomicU64 = AtomicU64::new(1);

#[derive(Clone, Debug)]
pub struct LinuxRuntimeConfig {
    pub browser_executable: PathBuf,
    pub xvfb_executable: PathBuf,
    pub prlimit_executable: PathBuf,
    pub display_min: u16,
    pub display_max: u16,
    pub display_mode: BrowserDisplayMode,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BrowserDisplayMode {
    HeadedRequired,
    HeadlessProcessTest,
}

impl LinuxRuntimeConfig {
    /// Discovers concrete local executables without consulting a shell or ambient PATH during launch.
    ///
    /// # Errors
    ///
    /// Returns a typed error when a required executable is unavailable.
    pub fn discover() -> Result<Self, RunnerError> {
        Ok(Self {
            browser_executable: first_file(&[
                "/opt/chromium/chrome-linux64/chrome",
                "/usr/bin/chromium",
                "/usr/bin/chromium-browser",
                "/usr/bin/google-chrome",
                "/snap/bin/chromium",
            ])
            .ok_or(RunnerError::BrowserUnavailable)?,
            xvfb_executable: first_file(&["/usr/bin/Xvfb", "/bin/Xvfb"])
                .ok_or(RunnerError::XvfbUnavailable)?,
            prlimit_executable: first_file(&["/usr/bin/prlimit", "/bin/prlimit"])
                .ok_or(RunnerError::ResourceLimitUnavailable)?,
            display_min: 90,
            display_max: 190,
            display_mode: BrowserDisplayMode::HeadedRequired,
        })
    }
}

pub struct LinuxComputerRuntime {
    config: LinuxRuntimeConfig,
    sandbox: SandboxStatus,
    workstations: BTreeMap<keith_platform_contracts::ComputerSessionId, Workstation>,
    credentials: BTreeMap<(String, String), SecretBytes>,
}

impl LinuxComputerRuntime {
    /// Discovers the headed browser, virtual display, resource limiter, and isolation backend.
    ///
    /// # Errors
    ///
    /// Returns a typed error when the local runtime is incomplete.
    pub fn discover() -> Result<Self, RunnerError> {
        LinuxResourceGroup::reclaim_stale()?;
        Ok(Self::new(LinuxRuntimeConfig::discover()?))
    }

    /// Discovers the concrete browser in an explicitly headless process-test mode.
    /// This mode exists only for hosts that prohibit X11 socket creation and is never selected
    /// by the production runner unless the test-only CLI flag is supplied.
    ///
    /// # Errors
    ///
    /// Returns a typed error when the real browser runtime is incomplete.
    pub fn discover_for_process_tests() -> Result<Self, RunnerError> {
        LinuxResourceGroup::reclaim_stale()?;
        let mut config = LinuxRuntimeConfig::discover()?;
        if let Some(headless_shell) = first_file(&[
            "/opt/chromium/chrome-headless-shell-linux64/chrome-headless-shell",
            "/usr/bin/chrome-headless-shell",
        ]) {
            config.browser_executable = headless_shell;
        }
        config.display_mode = BrowserDisplayMode::HeadlessProcessTest;
        Ok(Self::new(config))
    }

    pub fn new(config: LinuxRuntimeConfig) -> Self {
        Self {
            config,
            sandbox: SandboxStatus::detect(),
            workstations: BTreeMap::new(),
            credentials: BTreeMap::new(),
        }
    }

    pub fn sandbox_status(&self) -> &SandboxStatus {
        &self.sandbox
    }

    /// Registers one in-process credential value behind an opaque, profile-scoped handle.
    /// The value is zeroed when removed or when the runtime exits and is never serialized.
    ///
    /// # Errors
    ///
    /// Returns a validation error for an empty or oversized value.
    pub fn register_credential(
        &mut self,
        profile_id: &keith_agent_types::ProfileId,
        opaque_handle: &str,
        secret: impl Into<Vec<u8>>,
    ) -> Result<(), RunnerError> {
        if opaque_handle.is_empty() || opaque_handle.len() > 256 {
            return Err(RunnerError::CredentialUnavailable);
        }
        self.credentials.insert(
            (profile_id.to_string(), opaque_handle.to_string()),
            SecretBytes::new(secret.into())?,
        );
        Ok(())
    }

    fn launch(
        &mut self,
        session: &ComputerSession,
        layout: &ComputerSessionLayout,
    ) -> Result<(), RunnerError> {
        if self.workstations.contains_key(&session.id) {
            return Err(RunnerError::AlreadyRunning);
        }
        if session.isolation == IsolationRequirement::Strong && !self.sandbox.supports_untrusted() {
            return Err(RunnerError::StrongIsolationUnavailable);
        }
        LinuxResourceGroup::reclaim_stale()?;
        let resources = LinuxResourceGroup::create(session)?;
        let (display, mut xvfb) = if self.config.display_mode == BrowserDisplayMode::HeadedRequired
        {
            let display = self.available_display(&session.id)?;
            let mut xvfb = Command::new("/bin/sh");
            xvfb.args(["-c", OWNED_XVFB_SCRIPT, "cua-xvfb"])
                .arg(resources.path())
                .arg(&self.config.xvfb_executable)
                .args([
                    format!(":{display}"),
                    "-screen".into(),
                    "0".into(),
                    format!("{}x{}x24", session.viewport.width, session.viewport.height),
                    "-nolisten".into(),
                    "tcp".into(),
                    "-noreset".into(),
                ])
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .env_clear();
            configure_owned_process(&mut xvfb);
            let mut xvfb = xvfb.spawn()?;
            wait_for_display(&mut xvfb, display)?;
            (Some(display), Some(xvfb))
        } else {
            (None, None)
        };

        let mut browser = self.browser_command(session, layout, display, &resources)?;
        let mut browser = match browser.spawn() {
            Ok(browser) => browser,
            Err(error) => {
                if let Some(xvfb) = xvfb.as_mut() {
                    let _ = terminate_owned_process_tree(xvfb);
                }
                return Err(error.into());
            }
        };
        let input = browser.stdin.take().ok_or(RunnerError::PipeUnavailable)?;
        let output = browser.stdout.take().ok_or(RunnerError::PipeUnavailable)?;
        let mut cdp = CdpPipe::new(input, output, session.limits.action_timeout());
        let (target_id, session_id) = match wait_for_browser(&mut browser, &mut cdp) {
            Ok(result) => result,
            Err(error) => {
                let _ = terminate_owned_process_tree(&mut browser);
                if let Some(xvfb) = xvfb.as_mut() {
                    let _ = terminate_owned_process_tree(xvfb);
                }
                return Err(error);
            }
        };
        cdp.session_id = Some(session_id);
        let mut workstation = Workstation {
            xvfb,
            browser,
            cdp,
            resources,
            display,
            target_id,
            cursor: Point::default(),
            layout: layout.clone(),
            recent_actions: VecDeque::new(),
            dialogs: Vec::new(),
            downloads: BTreeMap::new(),
        };
        for method in [
            "Page.enable",
            "Runtime.enable",
            "DOM.enable",
            "Accessibility.enable",
        ] {
            workstation.cdp.page(method, json!({}))?;
        }
        let download_path = runtime_visible_path(session, layout, &layout.downloads)?;
        workstation.cdp.browser(
            "Browser.setDownloadBehavior",
            json!({"behavior": "allow", "downloadPath": download_path}),
        )?;
        self.workstations.insert(session.id.clone(), workstation);
        Ok(())
    }

    fn stop(&mut self, session: &ComputerSession) -> Result<(), RunnerError> {
        let Some(mut workstation) = self.workstations.remove(&session.id) else {
            return Ok(());
        };
        workstation.shutdown()
    }

    #[allow(clippy::too_many_lines)]
    fn browser_command(
        &self,
        session: &ComputerSession,
        layout: &ComputerSessionLayout,
        display: Option<u16>,
        resources: &LinuxResourceGroup,
    ) -> Result<Command, RunnerError> {
        let strong = session.isolation == IsolationRequirement::Strong;
        let browser = if strong {
            runtime_browser_path(&self.config.browser_executable)?
        } else {
            self.config.browser_executable.clone()
        };
        let profile = runtime_visible_path(session, layout, &layout.profile)?;
        let downloads = runtime_visible_path(session, layout, &layout.downloads)?;
        let mut limited = vec![
            format!("--cpu={}", session.limits.cpu_seconds),
            format!("--nproc={}", session.limits.max_processes),
            "--nofile=1024".into(),
            format!("--fsize={}", session.limits.disk_bytes),
            "--".into(),
            browser.to_string_lossy().into_owned(),
            "--remote-debugging-pipe".into(),
            "--no-sandbox".into(),
            "--no-first-run".into(),
            "--no-default-browser-check".into(),
            "--disable-sync".into(),
            "--disable-background-networking".into(),
            "--disable-component-update".into(),
            "--disable-dev-shm-usage".into(),
            "--password-store=basic".into(),
            format!("--user-data-dir={}", profile.to_string_lossy()),
            format!(
                "--window-size={},{}",
                session.viewport.width, session.viewport.height
            ),
            format!(
                "--download-default-directory={}",
                downloads.to_string_lossy()
            ),
        ];
        if session.network != NetworkPolicy::Allowed && !strong {
            limited.push("--host-resolver-rules=MAP * 0.0.0.0, EXCLUDE localhost".into());
        }
        if self.config.display_mode == BrowserDisplayMode::HeadlessProcessTest {
            limited.push("--headless=new".into());
        }
        limited.push("about:blank".into());

        let mut command = if strong {
            let launcher = self
                .sandbox
                .launcher
                .as_ref()
                .ok_or(RunnerError::StrongIsolationUnavailable)?;
            let mut command = Command::new("/bin/sh");
            command
                .args(["-c", RESOURCE_EXEC_SCRIPT, "cua-resource"])
                .arg(resources.path())
                .arg(launcher)
                .args([
                    "--die-with-parent",
                    "--new-session",
                    "--unshare-all",
                    "--ro-bind",
                    "/usr",
                    "/usr",
                ]);
            mount_system_path(&mut command, Path::new("/lib"));
            mount_system_path(&mut command, Path::new("/lib64"));
            mount_system_path(&mut command, Path::new("/opt"));
            mount_system_path(&mut command, Path::new("/snap"));
            if Path::new("/bin").is_symlink() {
                command.args(["--symlink", "usr/bin", "/bin"]);
            } else {
                mount_system_path(&mut command, Path::new("/bin"));
            }
            command.args(["--tmpfs", "/tmp"]);
            if self.config.display_mode == BrowserDisplayMode::HeadedRequired {
                command
                    .args(["--dir", "/tmp/.X11-unix", "--ro-bind"])
                    .arg("/tmp/.X11-unix")
                    .arg("/tmp/.X11-unix");
            }
            command.args([
                "--dir",
                "/computer",
                "--dir",
                "/computer/profile",
                "--dir",
                "/computer/workspace",
                "--dir",
                "/computer/downloads",
                "--bind",
            ]);
            command
                .arg(&layout.profile)
                .arg("/computer/profile")
                .arg("--bind")
                .arg(&layout.workspace)
                .arg("/computer/workspace")
                .arg("--bind")
                .arg(&layout.downloads)
                .arg("/computer/downloads")
                .args([
                    "--chdir",
                    "/computer/workspace",
                    "--proc",
                    "/proc",
                    "--dev",
                    "/dev",
                ]);
            if session.network == NetworkPolicy::Allowed {
                command.arg("--share-net");
            }
            command
                .arg("--")
                .arg("/bin/sh")
                .args(["-c", PIPE_SCRIPT, "cua-browser"])
                .arg(&self.config.prlimit_executable)
                .args(limited);
            command
        } else {
            let mut command = Command::new("/bin/sh");
            command
                .args(["-c", RESOURCE_PIPE_SCRIPT, "cua-browser"])
                .arg(resources.path())
                .arg(&self.config.prlimit_executable)
                .args(limited)
                .current_dir(&layout.workspace);
            command
        };
        command
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .env_clear()
            .env(
                "HOME",
                if strong {
                    "/computer/profile"
                } else {
                    layout.profile.to_str().ok_or(RunnerError::InvalidPath)?
                },
            )
            .env("LANG", "C.UTF-8");
        if let Some(display) = display {
            command.env("DISPLAY", format!(":{display}"));
        }
        configure_owned_process(&mut command);
        Ok(command)
    }

    fn available_display(
        &self,
        session_id: &keith_platform_contracts::ComputerSessionId,
    ) -> Result<u16, RunnerError> {
        let span = self
            .config
            .display_max
            .checked_sub(self.config.display_min)
            .and_then(|value| value.checked_add(1))
            .ok_or(RunnerError::DisplayUnavailable)?;
        let seed = session_id
            .to_string()
            .bytes()
            .fold(0_u16, |accumulator, byte| {
                accumulator.wrapping_add(u16::from(byte))
            });
        for offset in 0..span {
            let display = self.config.display_min + (seed + offset) % span;
            let used_by_runtime = self
                .workstations
                .values()
                .any(|workstation| workstation.display == Some(display));
            if !used_by_runtime && !Path::new(&format!("/tmp/.X11-unix/X{display}")).exists() {
                return Ok(display);
            }
        }
        Err(RunnerError::DisplayUnavailable)
    }
}

impl Drop for LinuxComputerRuntime {
    fn drop(&mut self) {
        for workstation in self.workstations.values_mut() {
            let _ = workstation.shutdown();
        }
        self.workstations.clear();
    }
}

impl ComputerRuntime for LinuxComputerRuntime {
    type Error = RunnerError;

    fn start(
        &mut self,
        session: &ComputerSession,
        layout: &ComputerSessionLayout,
    ) -> Result<(), Self::Error> {
        self.launch(session, layout)
    }

    fn suspend(&mut self, session: &ComputerSession) -> Result<(), Self::Error> {
        self.stop(session)
    }

    fn resume(
        &mut self,
        session: &ComputerSession,
        layout: &ComputerSessionLayout,
    ) -> Result<(), Self::Error> {
        self.launch(session, layout)
    }

    fn terminate(&mut self, session: &ComputerSession) -> Result<(), Self::Error> {
        self.stop(session)
    }

    fn observe(
        &mut self,
        session: &ComputerSession,
        now: UtcTimestamp,
    ) -> Result<ComputerObservation, Self::Error> {
        let workstation = self
            .workstations
            .get_mut(&session.id)
            .ok_or(RunnerError::NotRunning)?;
        workstation.observe(session, now)
    }

    fn execute(
        &mut self,
        session: &ComputerSession,
        action: &ComputerAction,
        cancellation: &CancellationToken,
        now: UtcTimestamp,
    ) -> Result<RuntimeActionResult, Self::Error> {
        if cancellation.is_cancelled() {
            return Err(RunnerError::Cancelled);
        }
        let credential = if let ComputerAction::CredentialFill { grant, .. } = action {
            self.credentials.get(&(
                session.profile_id.to_string(),
                grant.opaque_handle.as_str().to_string(),
            ))
        } else {
            None
        };
        let workstation = self
            .workstations
            .get_mut(&session.id)
            .ok_or(RunnerError::NotRunning)?;
        workstation.execute(session, action, cancellation, now, credential)
    }

    fn is_running(
        &mut self,
        session_id: &keith_platform_contracts::ComputerSessionId,
    ) -> Result<bool, Self::Error> {
        let Some(workstation) = self.workstations.get_mut(session_id) else {
            return Ok(false);
        };
        let display_running = workstation
            .xvfb
            .as_mut()
            .is_none_or(|xvfb| xvfb.try_wait().is_ok_and(|status| status.is_none()));
        Ok(workstation.browser.try_wait()?.is_none() && display_running)
    }
}

struct LinuxResourceGroup {
    path: Option<PathBuf>,
}

impl LinuxResourceGroup {
    fn create(session: &ComputerSession) -> Result<Self, RunnerError> {
        Self::create_named(
            session.id.to_string().as_str(),
            session.limits.memory_bytes,
            session.limits.max_processes,
        )
    }

    fn probe() -> Result<Self, RunnerError> {
        Self::create_named("probe", 64 * 1_024 * 1_024, 8)
    }

    fn create_named(
        label: &str,
        memory_bytes: u64,
        max_processes: u32,
    ) -> Result<Self, RunnerError> {
        let sequence = CGROUP_SEQUENCE.fetch_add(1, Ordering::Relaxed);
        let label = safe_cgroup_label(label);
        let name = format!("{CGROUP_PREFIX}{}-{sequence}-{label}", std::process::id());
        for parent in resource_group_parents()? {
            let path = parent.join(&name);
            match fs::create_dir(&path) {
                Ok(()) => {}
                Err(error)
                    if matches!(
                        error.kind(),
                        io::ErrorKind::PermissionDenied | io::ErrorKind::ReadOnlyFilesystem
                    ) =>
                {
                    continue;
                }
                Err(error) => return Err(error.into()),
            }
            let configured = configure_resource_group(&path, memory_bytes, max_processes);
            if configured.is_ok() {
                return Ok(Self { path: Some(path) });
            }
            let _ = fs::remove_dir(&path);
        }
        Err(RunnerError::ResourceControlUnavailable)
    }

    fn path(&self) -> &Path {
        self.path
            .as_deref()
            .expect("resource group remains present while workstation runs")
    }

    fn cleanup(&mut self) -> Result<(), RunnerError> {
        let Some(path) = self.path.take() else {
            return Ok(());
        };
        cleanup_resource_group(&path)?;
        Ok(())
    }

    fn reclaim_stale() -> Result<(), RunnerError> {
        let current_pid = std::process::id().to_string();
        for parent in resource_group_parents()? {
            for entry in fs::read_dir(parent)? {
                let entry = entry?;
                let name = entry.file_name();
                let name = name.to_string_lossy();
                let Some(remainder) = name.strip_prefix(CGROUP_PREFIX) else {
                    continue;
                };
                let Some((owner_pid, _)) = remainder.split_once('-') else {
                    continue;
                };
                if owner_pid == current_pid || Path::new("/proc").join(owner_pid).exists() {
                    continue;
                }
                cleanup_resource_group(&entry.path())?;
            }
        }
        Ok(())
    }
}

impl Drop for LinuxResourceGroup {
    fn drop(&mut self) {
        let _ = self.cleanup();
    }
}

fn resource_group_parents() -> Result<Vec<PathBuf>, RunnerError> {
    let membership = fs::read_to_string("/proc/self/cgroup")?;
    let relative = membership
        .lines()
        .find_map(|line| line.strip_prefix("0::"))
        .ok_or(RunnerError::ResourceControlUnavailable)?;
    let root = Path::new(CGROUP_ROOT);
    let mut current = root.join(relative.strip_prefix('/').unwrap_or(relative));
    if !current.starts_with(root) {
        return Err(RunnerError::ResourceControlUnavailable);
    }
    let mut parents = Vec::new();
    loop {
        if child_resource_controllers_enabled(&current) {
            parents.push(current.clone());
        }
        if current == root || !current.pop() {
            break;
        }
    }
    if parents.is_empty() {
        return Err(RunnerError::ResourceControlUnavailable);
    }
    Ok(parents)
}

fn child_resource_controllers_enabled(path: &Path) -> bool {
    fs::read_to_string(path.join("cgroup.subtree_control")).is_ok_and(|controllers| {
        let controllers = controllers.split_ascii_whitespace().collect::<Vec<_>>();
        controllers.contains(&"memory") && controllers.contains(&"pids")
    })
}

fn configure_resource_group(path: &Path, memory_bytes: u64, max_processes: u32) -> io::Result<()> {
    fs::write(path.join("memory.max"), memory_bytes.to_string())?;
    let swap_limit = path.join("memory.swap.max");
    if swap_limit.exists() {
        fs::write(swap_limit, "0")?;
    }
    let oom_group = path.join("memory.oom.group");
    if oom_group.exists() {
        fs::write(oom_group, "1")?;
    }
    fs::write(path.join("pids.max"), max_processes.to_string())?;
    Ok(())
}

fn cleanup_resource_group(path: &Path) -> io::Result<()> {
    if !path.exists() {
        return Ok(());
    }
    let kill = path.join("cgroup.kill");
    if resource_group_populated(path)? && kill.exists() {
        fs::write(kill, "1")?;
    }
    let deadline = Instant::now() + Duration::from_secs(2);
    while resource_group_populated(path)? && Instant::now() < deadline {
        thread::sleep(Duration::from_millis(10));
    }
    if resource_group_populated(path)? {
        return Err(io::Error::other(
            "computer resource group did not become empty",
        ));
    }
    fs::remove_dir(path)
}

fn resource_group_populated(path: &Path) -> io::Result<bool> {
    let events = fs::read_to_string(path.join("cgroup.events"))?;
    Ok(events
        .lines()
        .any(|line| line.split_once(' ') == Some(("populated", "1"))))
}

fn safe_cgroup_label(value: &str) -> String {
    let label = value
        .chars()
        .filter(|character| character.is_ascii_alphanumeric() || matches!(character, '-' | '_'))
        .take(80)
        .collect::<String>();
    if label.is_empty() {
        "session".into()
    } else {
        label
    }
}

struct Workstation {
    xvfb: Option<Child>,
    browser: Child,
    cdp: CdpPipe,
    resources: LinuxResourceGroup,
    display: Option<u16>,
    target_id: String,
    cursor: Point,
    layout: ComputerSessionLayout,
    recent_actions: VecDeque<RecentAction>,
    dialogs: Vec<DialogObservation>,
    downloads: BTreeMap<String, DownloadObservation>,
}

impl Workstation {
    fn shutdown(&mut self) -> Result<(), RunnerError> {
        let browser_result = terminate_owned_process_tree(&mut self.browser);
        let xvfb_result = self
            .xvfb
            .as_mut()
            .map_or(Ok(()), terminate_owned_process_tree);
        let resource_result = self.resources.cleanup();
        browser_result?;
        xvfb_result?;
        resource_result?;
        Ok(())
    }

    fn observe(
        &mut self,
        session: &ComputerSession,
        now: UtcTimestamp,
    ) -> Result<ComputerObservation, RunnerError> {
        self.consume_events();
        let screenshot = self.cdp.page(
            "Page.captureScreenshot",
            json!({"format": "png", "fromSurface": true, "captureBeyondViewport": false}),
        )?;
        let base64_data = string_field(&screenshot, "data")?.to_string();
        let content_digest = hex_digest(base64_data.as_bytes());
        let frame_id = FrameId::new();
        let page = self.evaluate_value(
            r"(() => {
              const clone = document.documentElement ? document.documentElement.cloneNode(true) : null;
              if (clone) {
                for (const field of clone.querySelectorAll('input,textarea,select')) {
                  field.removeAttribute('value');
                  field.removeAttribute('data-token');
                  field.removeAttribute('data-secret');
                }
              }
              return {
                url: location.href,
                title: document.title,
                html: clone ? clone.outerHTML : '',
                width: innerWidth,
                height: innerHeight,
                scale: devicePixelRatio
              };
            })()",
        )?;
        let url = page
            .get("url")
            .and_then(Value::as_str)
            .unwrap_or("about:blank")
            .to_string();
        let title = page
            .get("title")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_string();
        let html = bounded_string(
            page.get("html").and_then(Value::as_str).unwrap_or_default(),
            keith_cua::MAX_DOM_BYTES,
        );
        let accessibility = self.accessibility()?;
        let window = self.cdp.browser(
            "Browser.getWindowForTarget",
            json!({"targetId": self.target_id}),
        )?;
        let window_id = window
            .get("windowId")
            .and_then(Value::as_i64)
            .map_or_else(|| self.target_id.clone(), |id| id.to_string());
        let downloads = scan_downloads(&self.layout.downloads, session.limits.max_file_bytes)?;
        for download in downloads {
            self.downloads.insert(download.file_name.clone(), download);
        }
        Ok(ComputerObservation {
            computer_session_id: session.id.clone(),
            profile_id: session.profile_id.clone(),
            captured_at: now,
            screenshot: Screenshot {
                frame_id: frame_id.clone(),
                content_digest,
                media_type: "image/png".into(),
                base64_data,
                width: session.viewport.width,
                height: session.viewport.height,
            },
            dom: Some(keith_cua::DomSnapshot {
                frame_id,
                url: url.clone(),
                title: title.clone(),
                html,
            }),
            accessibility,
            focused_window: Some(FocusedWindow {
                title,
                application: "Chromium".into(),
                window_id,
            }),
            url: Some(url),
            viewport: session.viewport,
            cursor: self.cursor,
            dialogs: self.dialogs.clone(),
            downloads: self.downloads.values().cloned().collect(),
            applications: vec![ApplicationObservation {
                name: "Chromium".into(),
                version: None,
                document_label: None,
            }],
            recent_actions: self.recent_actions.iter().cloned().collect(),
        })
    }

    #[allow(clippy::too_many_lines)]
    fn execute(
        &mut self,
        session: &ComputerSession,
        action: &ComputerAction,
        cancellation: &CancellationToken,
        now: UtcTimestamp,
        credential: Option<&SecretBytes>,
    ) -> Result<RuntimeActionResult, RunnerError> {
        action
            .validate(&session.limits)
            .map_err(RunnerError::Computer)?;
        if cancellation.is_cancelled() {
            return Err(RunnerError::Cancelled);
        }
        let description = match action {
            ComputerAction::Move { target } => {
                let point = self.target_point(target, session)?;
                self.mouse("mouseMoved", point, MouseButton::Left, 0, 0)?;
                self.cursor = point;
                "moved pointer"
            }
            ComputerAction::Click { target, button } => {
                let point = self.target_point(target, session)?;
                self.click(point, *button, 1)?;
                "clicked target"
            }
            ComputerAction::DoubleClick { target, button } => {
                let point = self.target_point(target, session)?;
                self.click(point, *button, 2)?;
                "double-clicked target"
            }
            ComputerAction::Drag {
                from,
                to,
                duration_ms,
            } => {
                let from = self.target_point(from, session)?;
                let to = self.target_point(to, session)?;
                self.mouse("mouseMoved", from, MouseButton::Left, 0, 0)?;
                self.mouse("mousePressed", from, MouseButton::Left, 1, 1)?;
                wait_cancellable(Duration::from_millis(*duration_ms / 2), cancellation)?;
                self.mouse("mouseMoved", to, MouseButton::Left, 1, 0)?;
                wait_cancellable(Duration::from_millis(*duration_ms / 2), cancellation)?;
                self.mouse("mouseReleased", to, MouseButton::Left, 0, 1)?;
                self.cursor = to;
                "dragged target"
            }
            ComputerAction::Scroll { delta_x, delta_y } => {
                self.cdp.page(
                    "Input.dispatchMouseEvent",
                    json!({"type": "mouseWheel", "x": self.cursor.x, "y": self.cursor.y, "deltaX": delta_x, "deltaY": delta_y}),
                )?;
                "scrolled"
            }
            ComputerAction::Key { key } => {
                self.key(key, 0)?;
                "pressed key"
            }
            ComputerAction::Text { text } => {
                self.cdp.page("Input.insertText", json!({"text": text}))?;
                "inserted text"
            }
            ComputerAction::Shortcut { keys } => {
                self.shortcut(keys)?;
                "pressed shortcut"
            }
            ComputerAction::ClipboardRead => {
                self.evaluate_value("navigator.clipboard.readText()")?;
                "read clipboard into protected runtime value"
            }
            ComputerAction::ClipboardWrite { text } => {
                let encoded = serde_json::to_string(text)?;
                self.evaluate_value(&format!("navigator.clipboard.writeText({encoded})"))?;
                "updated clipboard"
            }
            ComputerAction::FileUpload {
                target,
                relative_path,
            } => {
                self.file_upload(session, target, relative_path)?;
                "selected upload file"
            }
            ComputerAction::Download { target, .. } => {
                let point = self.semantic_point(target, session)?;
                self.click(point, MouseButton::Left, 1)?;
                "started download"
            }
            ComputerAction::NewTab { url } => {
                self.create_target(url.as_deref().unwrap_or("about:blank"), false, session)?;
                "opened tab"
            }
            ComputerAction::CloseTab | ComputerAction::CloseWindow => {
                self.cdp
                    .browser("Target.closeTarget", json!({"targetId": self.target_id}))?;
                self.select_target(0)?;
                "closed browser target"
            }
            ComputerAction::SwitchTab { index } => {
                self.select_target(*index)?;
                "switched tab"
            }
            ComputerAction::NewWindow { url } => {
                self.create_target(url.as_deref().unwrap_or("about:blank"), true, session)?;
                "opened window"
            }
            ComputerAction::FocusWindow { .. } => {
                self.cdp.page("Page.bringToFront", json!({}))?;
                "focused window"
            }
            ComputerAction::Navigate { url } => {
                validate_navigation(url, session, &self.layout)?;
                self.cdp.page("Page.navigate", json!({"url": url}))?;
                self.wait_ready(cancellation, session.limits.action_timeout())?;
                "navigated"
            }
            ComputerAction::Wait { duration_ms } => {
                wait_cancellable(Duration::from_millis(*duration_ms), cancellation)?;
                "waited"
            }
            ComputerAction::CredentialFill { target, .. } => {
                let credential = credential.ok_or(RunnerError::CredentialUnavailable)?;
                self.credential_fill(target, credential)?;
                "filled named credential"
            }
        };
        let digest = hex_digest(&serde_json::to_vec(action)?);
        self.recent_actions.push_back(RecentAction {
            action_digest: digest,
            description: description.into(),
            occurred_at: now,
            succeeded: true,
        });
        while self.recent_actions.len() > keith_cua::MAX_RECENT_ACTIONS {
            self.recent_actions.pop_front();
        }
        Ok(RuntimeActionResult {
            description: description.into(),
        })
    }

    fn target_point(
        &mut self,
        target: &ActionTarget,
        session: &ComputerSession,
    ) -> Result<Point, RunnerError> {
        match target {
            ActionTarget::Semantic { target } => self.semantic_point(target, session),
            ActionTarget::Coordinate { point, .. } if session.viewport.contains(*point) => {
                Ok(*point)
            }
            ActionTarget::Coordinate { .. } => Err(RunnerError::TargetUnavailable),
        }
    }

    fn semantic_point(
        &mut self,
        target: &SemanticTarget,
        session: &ComputerSession,
    ) -> Result<Point, RunnerError> {
        let selector = match target {
            SemanticTarget::Css { selector } => {
                let selector = serde_json::to_string(selector)?;
                format!("document.querySelector({selector})")
            }
            SemanticTarget::Text { text } => {
                let text = serde_json::to_string(text)?;
                format!(
                    "Array.from(document.querySelectorAll('button,a,input,textarea,select,[role],[tabindex]')).find(e => ((e.innerText || e.value || e.getAttribute('aria-label') || '')).trim().includes({text}))"
                )
            }
            SemanticTarget::Accessibility { role, name } => {
                let role = serde_json::to_string(role)?;
                let name = serde_json::to_string(name)?;
                format!(
                    "Array.from(document.querySelectorAll('[role],button,a,input,textarea,select')).find(e => ((e.getAttribute('role') || e.tagName.toLowerCase()) === {role}) && ((e.getAttribute('aria-label') || e.innerText || e.value || '')).trim().includes({name}))"
                )
            }
        };
        let expression = format!(
            "(() => {{ const e = {selector}; if (!e) return null; const r = e.getBoundingClientRect(); if (!r.width || !r.height) return null; return {{x: Math.round(r.left + r.width / 2), y: Math.round(r.top + r.height / 2)}}; }})()"
        );
        let value = self.evaluate_value(&expression)?;
        let x = value
            .get("x")
            .and_then(Value::as_i64)
            .ok_or(RunnerError::TargetUnavailable)?;
        let y = value
            .get("y")
            .and_then(Value::as_i64)
            .ok_or(RunnerError::TargetUnavailable)?;
        let point = Point {
            x: i32::try_from(x).map_err(|_| RunnerError::TargetUnavailable)?,
            y: i32::try_from(y).map_err(|_| RunnerError::TargetUnavailable)?,
        };
        if !session.viewport.contains(point) {
            return Err(RunnerError::TargetUnavailable);
        }
        Ok(point)
    }

    fn click(&mut self, point: Point, button: MouseButton, count: u8) -> Result<(), RunnerError> {
        self.mouse("mouseMoved", point, button, 0, 0)?;
        self.mouse("mousePressed", point, button, 1, count)?;
        self.mouse("mouseReleased", point, button, 0, count)?;
        self.cursor = point;
        Ok(())
    }

    fn mouse(
        &mut self,
        event_type: &str,
        point: Point,
        button: MouseButton,
        buttons: u8,
        click_count: u8,
    ) -> Result<(), RunnerError> {
        self.cdp.page(
            "Input.dispatchMouseEvent",
            json!({
                "type": event_type,
                "x": point.x,
                "y": point.y,
                "button": mouse_button(button),
                "buttons": buttons,
                "clickCount": click_count,
            }),
        )?;
        Ok(())
    }

    fn key(&mut self, key: &str, modifiers: u8) -> Result<(), RunnerError> {
        for event_type in ["keyDown", "keyUp"] {
            self.cdp.page(
                "Input.dispatchKeyEvent",
                json!({"type": event_type, "key": key, "modifiers": modifiers}),
            )?;
        }
        Ok(())
    }

    fn shortcut(&mut self, keys: &[String]) -> Result<(), RunnerError> {
        let mut modifiers = 0_u8;
        let mut primary = None;
        for key in keys {
            match key.to_ascii_lowercase().as_str() {
                "alt" => modifiers |= 1,
                "control" | "ctrl" => modifiers |= 2,
                "meta" | "command" => modifiers |= 4,
                "shift" => modifiers |= 8,
                _ if primary.is_none() => primary = Some(key.as_str()),
                _ => return Err(RunnerError::InvalidShortcut),
            }
        }
        self.key(primary.ok_or(RunnerError::InvalidShortcut)?, modifiers)
    }

    fn file_upload(
        &mut self,
        session: &ComputerSession,
        target: &SemanticTarget,
        relative_path: &str,
    ) -> Result<(), RunnerError> {
        let host_path = fs::canonicalize(self.layout.workspace.join(relative_path))?;
        let workspace = fs::canonicalize(&self.layout.workspace)?;
        if !host_path.starts_with(&workspace) || !host_path.is_file() {
            return Err(RunnerError::InvalidPath);
        }
        if host_path.metadata()?.len() > session.limits.max_file_bytes {
            return Err(RunnerError::FileLimit);
        }
        let SemanticTarget::Css { selector } = target else {
            return Err(RunnerError::UploadTargetMustBeCss);
        };
        let expression = format!(
            "document.querySelector({})",
            serde_json::to_string(selector)?
        );
        let evaluated = self.cdp.page(
            "Runtime.evaluate",
            json!({"expression": expression, "returnByValue": false}),
        )?;
        let object_id = evaluated
            .pointer("/result/objectId")
            .and_then(Value::as_str)
            .ok_or(RunnerError::TargetUnavailable)?;
        let requested = self
            .cdp
            .page("DOM.requestNode", json!({"objectId": object_id}))?;
        let node_id = requested
            .get("nodeId")
            .and_then(Value::as_i64)
            .ok_or(RunnerError::TargetUnavailable)?;
        let visible = runtime_visible_path(session, &self.layout, &host_path)?;
        self.cdp.page(
            "DOM.setFileInputFiles",
            json!({"nodeId": node_id, "files": [visible]}),
        )?;
        Ok(())
    }

    fn credential_fill(
        &mut self,
        target: &SemanticTarget,
        credential: &SecretBytes,
    ) -> Result<(), RunnerError> {
        let SemanticTarget::Css { selector } = target else {
            return Err(RunnerError::CredentialTargetMustBeCss);
        };
        let expression = format!(
            "document.querySelector({})",
            serde_json::to_string(selector)?
        );
        let evaluated = self.cdp.page(
            "Runtime.evaluate",
            json!({"expression": expression, "returnByValue": false}),
        )?;
        let object_id = evaluated
            .pointer("/result/objectId")
            .and_then(Value::as_str)
            .ok_or(RunnerError::TargetUnavailable)?
            .to_string();
        let protected = self.cdp.page(
            "Runtime.callFunctionOn",
            json!({
                "objectId": &object_id,
                "functionDeclaration": "function() { return this instanceof HTMLInputElement && this.type.toLowerCase() === 'password'; }",
                "returnByValue": true,
            }),
        )?;
        if protected.pointer("/result/value").and_then(Value::as_bool) != Some(true) {
            return Err(RunnerError::CredentialTargetMustBeProtected);
        }
        credential.with_utf8(|value| {
            self.cdp.page(
                "Runtime.callFunctionOn",
                json!({
                    "objectId": &object_id,
                    "functionDeclaration": "function(value) { this.focus(); this.value = value; this.dispatchEvent(new Event('input', {bubbles:true})); this.dispatchEvent(new Event('change', {bubbles:true})); }",
                    "arguments": [{"value": value}],
                    "returnByValue": true,
                }),
            )
        })??;
        Ok(())
    }

    fn create_target(
        &mut self,
        url: &str,
        new_window: bool,
        session: &ComputerSession,
    ) -> Result<(), RunnerError> {
        validate_navigation(url, session, &self.layout)?;
        let created = self.cdp.browser(
            "Target.createTarget",
            json!({"url": url, "newWindow": new_window}),
        )?;
        let target_id = string_field(&created, "targetId")?.to_string();
        self.attach_target(target_id)
    }

    fn select_target(&mut self, index: usize) -> Result<(), RunnerError> {
        let targets = self.cdp.browser("Target.getTargets", json!({}))?;
        let pages = targets
            .get("targetInfos")
            .and_then(Value::as_array)
            .ok_or(RunnerError::CdpProtocol)?
            .iter()
            .filter(|target| target.get("type").and_then(Value::as_str) == Some("page"))
            .collect::<Vec<_>>();
        let target_id = pages
            .get(index)
            .and_then(|target| target.get("targetId"))
            .and_then(Value::as_str)
            .ok_or(RunnerError::TargetUnavailable)?
            .to_string();
        self.attach_target(target_id)
    }

    fn attach_target(&mut self, target_id: String) -> Result<(), RunnerError> {
        let attached = self.cdp.browser(
            "Target.attachToTarget",
            json!({"targetId": target_id, "flatten": true}),
        )?;
        self.cdp.session_id = Some(string_field(&attached, "sessionId")?.to_string());
        self.target_id = target_id;
        for method in [
            "Page.enable",
            "Runtime.enable",
            "DOM.enable",
            "Accessibility.enable",
        ] {
            self.cdp.page(method, json!({}))?;
        }
        self.cdp.page("Page.bringToFront", json!({}))?;
        Ok(())
    }

    fn wait_ready(
        &mut self,
        cancellation: &CancellationToken,
        timeout: Duration,
    ) -> Result<(), RunnerError> {
        let deadline = Instant::now() + timeout;
        while Instant::now() < deadline {
            if cancellation.is_cancelled() {
                return Err(RunnerError::Cancelled);
            }
            let value = self.evaluate_value("document.readyState")?;
            if matches!(value.as_str(), Some("interactive" | "complete")) {
                return Ok(());
            }
            thread::sleep(Duration::from_millis(25));
        }
        Err(RunnerError::ActionTimeout)
    }

    fn evaluate_value(&mut self, expression: &str) -> Result<Value, RunnerError> {
        let evaluated = self.cdp.page(
            "Runtime.evaluate",
            json!({"expression": expression, "returnByValue": true, "awaitPromise": true}),
        )?;
        if let Some(exception) = evaluated.get("exceptionDetails") {
            return Err(RunnerError::BrowserEvaluation(
                exception
                    .get("text")
                    .and_then(Value::as_str)
                    .unwrap_or("browser evaluation failed")
                    .chars()
                    .take(256)
                    .collect(),
            ));
        }
        Ok(evaluated
            .pointer("/result/value")
            .cloned()
            .unwrap_or(Value::Null))
    }

    fn accessibility(&mut self) -> Result<Vec<AccessibilityNode>, RunnerError> {
        let tree = self.cdp.page("Accessibility.getFullAXTree", json!({}))?;
        let nodes = tree
            .get("nodes")
            .and_then(Value::as_array)
            .ok_or(RunnerError::CdpProtocol)?;
        Ok(nodes
            .iter()
            .take(keith_cua::MAX_ACCESSIBILITY_NODES)
            .map(|node| {
                let protected = node
                    .get("properties")
                    .and_then(Value::as_array)
                    .is_some_and(|properties| {
                        properties.iter().any(|property| {
                            property.get("name").and_then(Value::as_str) == Some("protected")
                                && property.pointer("/value/value").and_then(Value::as_bool)
                                    == Some(true)
                        })
                    });
                AccessibilityNode {
                    role: ax_value(node.get("role")),
                    name: ax_value(node.get("name")),
                    value: node.get("value").map(|value| {
                        if protected {
                            "[redacted]".into()
                        } else {
                            bounded_string(&ax_value(Some(value)), 1_024)
                        }
                    }),
                    disabled: ax_property(node, "disabled"),
                    focused: ax_property(node, "focused"),
                }
            })
            .collect())
    }

    fn consume_events(&mut self) {
        for event in self.cdp.take_events() {
            match event.get("method").and_then(Value::as_str) {
                Some("Page.javascriptDialogOpening") => {
                    let kind = event
                        .pointer("/params/type")
                        .and_then(Value::as_str)
                        .unwrap_or("dialog")
                        .to_string();
                    let message = event
                        .pointer("/params/message")
                        .and_then(Value::as_str)
                        .unwrap_or("browser dialog");
                    let safe = keith_platform_contracts::RedactedText::parse(bounded_string(
                        message, 1_024,
                    ))
                    .unwrap_or_else(|_| {
                        keith_platform_contracts::RedactedText::parse("browser dialog")
                            .expect("constant is safe")
                    });
                    self.dialogs.push(DialogObservation {
                        kind,
                        message: safe,
                    });
                }
                Some("Browser.downloadWillBegin") => {
                    let guid = event
                        .pointer("/params/guid")
                        .and_then(Value::as_str)
                        .unwrap_or("download")
                        .to_string();
                    let file_name = event
                        .pointer("/params/suggestedFilename")
                        .and_then(Value::as_str)
                        .unwrap_or("download")
                        .to_string();
                    self.downloads.insert(
                        guid,
                        DownloadObservation {
                            file_name,
                            received_bytes: 0,
                            total_bytes: None,
                            state: DownloadState::InProgress,
                        },
                    );
                }
                Some("Browser.downloadProgress") => {
                    let Some(guid) = event.pointer("/params/guid").and_then(Value::as_str) else {
                        continue;
                    };
                    if let Some(download) = self.downloads.get_mut(guid) {
                        download.received_bytes = event
                            .pointer("/params/receivedBytes")
                            .and_then(Value::as_u64)
                            .unwrap_or(download.received_bytes);
                        download.total_bytes =
                            event.pointer("/params/totalBytes").and_then(Value::as_u64);
                        download.state =
                            match event.pointer("/params/state").and_then(Value::as_str) {
                                Some("completed") => DownloadState::Completed,
                                Some("canceled") => DownloadState::Cancelled,
                                _ => DownloadState::InProgress,
                            };
                    }
                }
                _ => {}
            }
        }
        if self.dialogs.len() > 16 {
            self.dialogs.drain(..self.dialogs.len() - 16);
        }
    }
}

impl Drop for Workstation {
    fn drop(&mut self) {
        let _ = self.shutdown();
    }
}

struct CdpPipe {
    input: BufWriter<ChildStdin>,
    output: BufReader<ChildStdout>,
    next_id: u64,
    session_id: Option<String>,
    events: Vec<Value>,
    request_timeout: Duration,
}

impl CdpPipe {
    fn new(input: ChildStdin, output: ChildStdout, request_timeout: Duration) -> Self {
        Self {
            input: BufWriter::new(input),
            output: BufReader::new(output),
            next_id: 1,
            session_id: None,
            events: Vec::new(),
            request_timeout,
        }
    }

    #[allow(clippy::needless_pass_by_value)]
    fn browser(&mut self, method: &str, params: Value) -> Result<Value, RunnerError> {
        self.request(method, &params, None)
    }

    #[allow(clippy::needless_pass_by_value)]
    fn page(&mut self, method: &str, params: Value) -> Result<Value, RunnerError> {
        let session_id = self.session_id.clone().ok_or(RunnerError::CdpProtocol)?;
        self.request(method, &params, Some(session_id))
    }

    fn request(
        &mut self,
        method: &str,
        params: &Value,
        session_id: Option<String>,
    ) -> Result<Value, RunnerError> {
        let id = self.next_id;
        self.next_id = self.next_id.saturating_add(1);
        let mut request = json!({"id": id, "method": method, "params": params});
        if let Some(session_id) = session_id {
            request["sessionId"] = Value::String(session_id);
        }
        serde_json::to_writer(&mut self.input, &request)?;
        self.input.write_all(&[0])?;
        self.input.flush()?;
        let deadline = Instant::now() + self.request_timeout;
        loop {
            if self.output.buffer().is_empty() {
                let remaining = deadline.saturating_duration_since(Instant::now());
                if remaining.is_zero() {
                    return Err(RunnerError::ActionTimeout);
                }
                let timeout = PollTimeout::try_from(remaining).unwrap_or(PollTimeout::MAX);
                let mut descriptors = [PollFd::new(
                    self.output.get_ref().as_fd(),
                    PollFlags::POLLIN,
                )];
                match poll(&mut descriptors, timeout) {
                    Ok(0) => return Err(RunnerError::ActionTimeout),
                    Ok(_) => {}
                    Err(Errno::EINTR) => continue,
                    Err(error) => {
                        return Err(io::Error::from_raw_os_error(error as i32).into());
                    }
                }
            }
            let mut message = Vec::new();
            let bytes = self.output.read_until(0, &mut message)?;
            if bytes == 0 {
                return Err(RunnerError::BrowserExited);
            }
            if message.len() > MAX_CDP_MESSAGE_BYTES {
                return Err(RunnerError::CdpMessageLimit);
            }
            if message.last() == Some(&0) {
                message.pop();
            }
            let value: Value = serde_json::from_slice(&message)?;
            if value.get("id").and_then(Value::as_u64) == Some(id) {
                if value.get("error").is_some() {
                    return Err(RunnerError::CdpCommand);
                }
                return Ok(value.get("result").cloned().unwrap_or_else(|| json!({})));
            }
            self.events.push(value);
        }
    }

    fn take_events(&mut self) -> Vec<Value> {
        std::mem::take(&mut self.events)
    }
}

pub struct RunnerService {
    controller: ComputerController<LinuxComputerRuntime>,
    control: ComputerControlService,
}

impl RunnerService {
    /// Opens durable computer state and reconciles interrupted process ownership.
    ///
    /// # Errors
    ///
    /// Returns a storage error when state cannot be opened.
    pub fn open(
        root: impl Into<PathBuf>,
        runtime: LinuxComputerRuntime,
    ) -> Result<Self, RunnerError> {
        let root = root.into();
        let now = timestamp_now()?;
        Ok(Self {
            controller: ComputerController::open(root.clone(), runtime, now)?,
            control: ComputerControlService::open(root.join("control"), now)?,
        })
    }

    pub fn controller(&self) -> &ComputerController<LinuxComputerRuntime> {
        &self.controller
    }

    pub fn controller_mut(&mut self) -> &mut ComputerController<LinuxComputerRuntime> {
        &mut self.controller
    }

    /// Serves one bounded JSON command per line and emits one response per line.
    ///
    /// # Errors
    ///
    /// Returns only transport failures. Invalid commands receive safe error responses.
    pub fn run_stdio(&mut self) -> Result<(), RunnerError> {
        let stdin = io::stdin();
        let stdout = io::stdout();
        self.serve(BufReader::new(stdin.lock()), BufWriter::new(stdout.lock()))
    }

    /// Serves the bounded JSON-line protocol over caller-provided streams.
    ///
    /// # Errors
    ///
    /// Returns a transport or response-serialization error. Malformed commands are converted
    /// into safe protocol errors and do not terminate the service.
    pub fn serve(
        &mut self,
        mut input: impl BufRead,
        mut output: impl Write,
    ) -> Result<(), RunnerError> {
        loop {
            let mut line = String::new();
            let bytes = input.read_line(&mut line)?;
            if bytes == 0 {
                break;
            }
            let response = if line.len() > MAX_COMMAND_BYTES {
                RunnerResponse::Error {
                    code: "command_too_large".into(),
                    safe_message: "computer command exceeds the input limit".into(),
                }
            } else {
                match serde_json::from_str::<RunnerCommand>(&line) {
                    Ok(command) => {
                        let shutdown = matches!(command, RunnerCommand::Shutdown);
                        let response = self.handle(command);
                        write_response(&mut output, &response)?;
                        if shutdown {
                            break;
                        }
                        continue;
                    }
                    Err(_) => RunnerResponse::Error {
                        code: "invalid_command".into(),
                        safe_message: "computer command is malformed".into(),
                    },
                }
            };
            write_response(&mut output, &response)?;
        }
        Ok(())
    }

    #[allow(clippy::too_many_lines)]
    pub fn handle(&mut self, command: RunnerCommand) -> RunnerResponse {
        let result = match command {
            RunnerCommand::Create { request, now } => self
                .controller
                .create(request, now)
                .map(|session| RunnerResponse::Session { session }),
            RunnerCommand::Start {
                session_id,
                profile_id,
                now,
            } => self
                .controller
                .start(&session_id, &profile_id, now)
                .map(|session| RunnerResponse::Session { session }),
            RunnerCommand::Suspend {
                session_id,
                profile_id,
                now,
            } => self
                .controller
                .suspend(&session_id, &profile_id, now)
                .map(|session| RunnerResponse::Session { session }),
            RunnerCommand::Resume {
                session_id,
                profile_id,
                now,
            } => self
                .controller
                .resume(&session_id, &profile_id, now)
                .map(|session| RunnerResponse::Session { session }),
            RunnerCommand::Snapshot {
                session_id,
                profile_id,
                now,
            } => self
                .controller
                .snapshot(&session_id, &profile_id, now)
                .map(|snapshot_id| RunnerResponse::Snapshot { snapshot_id }),
            RunnerCommand::Restore {
                session_id,
                profile_id,
                snapshot_id,
                now,
            } => self
                .controller
                .restore(&session_id, &profile_id, &snapshot_id, now)
                .map(|session| RunnerResponse::Session { session }),
            RunnerCommand::Reset {
                session_id,
                profile_id,
                now,
            } => self
                .controller
                .reset(&session_id, &profile_id, now)
                .map(|session| RunnerResponse::Session { session }),
            RunnerCommand::Terminate {
                session_id,
                profile_id,
                now,
            } => self
                .controller
                .terminate(&session_id, &profile_id, now)
                .map(|session| RunnerResponse::Session { session }),
            RunnerCommand::DeleteProfile { profile_id, now } => {
                return self.delete_profile(&profile_id, now);
            }
            RunnerCommand::ReclaimIdle { now } => self
                .controller
                .reclaim_idle(now)
                .map(|sessions| RunnerResponse::Reclaimed { sessions }),
            RunnerCommand::Observe {
                session_id,
                profile_id,
                now,
            } => self
                .controller
                .observe(&session_id, &profile_id, now)
                .map(|observation| RunnerResponse::Observation {
                    observation: Box::new(observation),
                }),
            RunnerCommand::Act {
                request,
                boundary,
                now,
            } => self
                .controller
                .execute_action(&request, &boundary, now)
                .map(|action| RunnerResponse::Action {
                    action: Box::new(action),
                }),
            RunnerCommand::ControlledAct {
                request,
                boundary,
                screen_id,
                expected_revision,
                principal,
                focus_unambiguous,
                stream_synchronized,
                now,
            } => {
                return self.handle_controlled_action(
                    &request,
                    &boundary,
                    &screen_id,
                    expected_revision,
                    &principal,
                    focus_unambiguous,
                    stream_synchronized,
                    now,
                );
            }
            RunnerCommand::Cancel { cancellation_id } => Ok(RunnerResponse::Cancelled {
                accepted: self.controller.cancel(&cancellation_id),
            }),
            RunnerCommand::Health {
                session_id,
                profile_id,
            } => self
                .controller
                .health(&session_id, &profile_id)
                .map(|health| RunnerResponse::Health { health }),
            command @ (RunnerCommand::CreateScreen { .. }
            | RunnerCommand::GetScreen { .. }
            | RunnerCommand::NegotiateScreenStream { .. }
            | RunnerCommand::AuthenticateScreenStream { .. }
            | RunnerCommand::TakeUserControl { .. }
            | RunnerCommand::RequestKeithControl { .. }
            | RunnerCommand::GrantKeithControl { .. }
            | RunnerCommand::PauseControl { .. }
            | RunnerCommand::UpdateScreen { .. }) => return self.handle_control(command),
            RunnerCommand::Shutdown => Ok(RunnerResponse::Shutdown),
        };
        result.unwrap_or_else(|error| safe_response_error(&error))
    }

    fn delete_profile(
        &mut self,
        profile_id: &keith_agent_types::ProfileId,
        now: UtcTimestamp,
    ) -> RunnerResponse {
        let sessions = match self.controller.delete_profile(profile_id, now) {
            Ok(sessions) => sessions,
            Err(error) => return safe_response_error(&error),
        };
        match self.control.delete_profile(profile_id) {
            Ok(_) => RunnerResponse::Deleted { sessions },
            Err(error) => safe_control_response_error(&error),
        }
    }

    #[allow(clippy::too_many_arguments)]
    fn handle_controlled_action(
        &mut self,
        request: &keith_cua::ComputerActionRequest,
        boundary: &keith_platform_contracts::AuthorityBoundary,
        screen_id: &keith_agent_types::EntityId,
        expected_revision: u64,
        principal: &keith_platform_contracts::ExternalPrincipalId,
        focus_unambiguous: bool,
        stream_synchronized: bool,
        now: UtcTimestamp,
    ) -> RunnerResponse {
        if &request.primary.authority.acting_principal != principal {
            return safe_control_response_error(&ControlError::InputDenied);
        }
        let screen = match self.control.screen(&request.profile_id, screen_id) {
            Ok(screen) => screen.clone(),
            Err(error) => return safe_control_response_error(&error),
        };
        if screen.computer_session_id != request.computer_session_id {
            return safe_control_response_error(&ControlError::InputDenied);
        }
        if let Err(error) = self.control.authorize_input(
            &request.profile_id,
            screen_id,
            expected_revision,
            principal,
            focus_unambiguous,
            stream_synchronized,
            now,
        ) {
            return safe_control_response_error(&error);
        }
        let cancellation_id = request.primary.authority.cancellation_id.clone();
        let registered_keith = screen.control.owner == ControlOwner::KeithControl;
        if registered_keith {
            let token = self.controller.cancellation_handle(cancellation_id.clone());
            if let Err(error) = self.control.register_keith_input(
                &request.profile_id,
                screen_id,
                expected_revision,
                principal,
                token,
                now,
            ) {
                return safe_control_response_error(&error);
            }
        }
        let result = self.controller.execute_action(request, boundary, now);
        self.controller.release_cancellation(&cancellation_id);
        if registered_keith
            && let Err(error) =
                self.control
                    .finish_keith_input(&request.profile_id, screen_id, &cancellation_id)
        {
            return safe_control_response_error(&error);
        }
        match result {
            Ok(action) => RunnerResponse::Action {
                action: Box::new(action),
            },
            Err(error) => safe_response_error(&error),
        }
    }

    #[allow(clippy::too_many_lines)]
    fn handle_control(&mut self, command: RunnerCommand) -> RunnerResponse {
        let result = match command {
            RunnerCommand::CreateScreen {
                session_id,
                profile_id,
                keith_principal,
                now,
            } => {
                let Some(session) = self.controller.session(&session_id) else {
                    return safe_response_error(&ComputerError::NotFound);
                };
                if session.profile_id != profile_id {
                    return safe_response_error(&ComputerError::ProfileIsolation);
                }
                self.control
                    .create_screen(session_id, profile_id, keith_principal, now)
                    .map(|screen| RunnerResponse::Screen { screen })
            }
            RunnerCommand::GetScreen {
                screen_id,
                profile_id,
            } => self
                .control
                .screen(&profile_id, &screen_id)
                .cloned()
                .map(|screen| RunnerResponse::Screen { screen }),
            RunnerCommand::NegotiateScreenStream {
                screen_id,
                profile_id,
                observer_id,
                origin,
                now,
                ttl_ms,
            } => self
                .control
                .negotiate_stream(&profile_id, &screen_id, observer_id, &origin, now, ttl_ms)
                .map(|grant| RunnerResponse::ScreenStream { grant }),
            RunnerCommand::AuthenticateScreenStream {
                profile_id,
                observer_id,
                origin,
                stream_ticket,
                now,
            } => self
                .control
                .consume_stream_ticket(&profile_id, &observer_id, &origin, &stream_ticket, now)
                .map(|screen_id| RunnerResponse::ScreenStreamAuthenticated { screen_id }),
            RunnerCommand::TakeUserControl {
                screen_id,
                profile_id,
                expected_revision,
                user_principal,
                now,
            } => self
                .control
                .take_user_control(
                    &profile_id,
                    &screen_id,
                    expected_revision,
                    user_principal,
                    now,
                )
                .map(|screen| RunnerResponse::Screen { screen }),
            RunnerCommand::RequestKeithControl {
                screen_id,
                profile_id,
                keith_principal,
            } => self
                .control
                .request_keith_control(&profile_id, &screen_id, keith_principal)
                .map(|()| RunnerResponse::KeithControlRequested),
            RunnerCommand::GrantKeithControl {
                screen_id,
                profile_id,
                expected_revision,
                now,
            } => self
                .control
                .grant_keith_control(&profile_id, &screen_id, expected_revision, now)
                .map(|screen| RunnerResponse::Screen { screen }),
            RunnerCommand::PauseControl {
                screen_id,
                profile_id,
                expected_revision,
                now,
            } => self
                .control
                .pause(&profile_id, &screen_id, expected_revision, now)
                .map(|screen| RunnerResponse::Screen { screen }),
            RunnerCommand::UpdateScreen {
                screen_id,
                profile_id,
                connection,
                quality,
                frame_sequence,
                active_action,
                intended_action,
                recording,
                safe_error,
                now,
            } => self
                .control
                .update_screen(
                    &profile_id,
                    &screen_id,
                    connection,
                    quality,
                    frame_sequence,
                    active_action,
                    intended_action,
                    recording,
                    safe_error,
                    now,
                )
                .map(|screen| RunnerResponse::Screen { screen }),
            _ => unreachable!("non-control command routed to control service"),
        };
        result.unwrap_or_else(|error| safe_control_response_error(&error))
    }
}

fn wait_for_browser(child: &mut Child, cdp: &mut CdpPipe) -> Result<(String, String), RunnerError> {
    let deadline = Instant::now() + BROWSER_BOOT_TIMEOUT;
    while Instant::now() < deadline {
        if child.try_wait()?.is_some() {
            return Err(RunnerError::BrowserExited);
        }
        match cdp.browser("Target.getTargets", json!({})) {
            Ok(targets) => {
                let Some(target_id) = targets
                    .get("targetInfos")
                    .and_then(Value::as_array)
                    .and_then(|targets| {
                        targets.iter().find(|target| {
                            target.get("type").and_then(Value::as_str) == Some("page")
                        })
                    })
                    .and_then(|target| target.get("targetId"))
                    .and_then(Value::as_str)
                    .map(str::to_string)
                else {
                    thread::sleep(Duration::from_millis(25));
                    continue;
                };
                let attached = cdp.browser(
                    "Target.attachToTarget",
                    json!({"targetId": target_id, "flatten": true}),
                )?;
                let session_id = string_field(&attached, "sessionId")?.to_string();
                return Ok((target_id, session_id));
            }
            Err(RunnerError::Io(error))
                if matches!(
                    error.kind(),
                    io::ErrorKind::WouldBlock | io::ErrorKind::Interrupted
                ) => {}
            Err(error) => return Err(error),
        }
        thread::sleep(Duration::from_millis(25));
    }
    Err(RunnerError::BrowserBootTimeout)
}

fn wait_for_display(child: &mut Child, display: u16) -> Result<(), RunnerError> {
    let socket = PathBuf::from(format!("/tmp/.X11-unix/X{display}"));
    let deadline = Instant::now() + Duration::from_secs(5);
    while Instant::now() < deadline {
        if socket.exists() {
            return Ok(());
        }
        if child.try_wait()?.is_some() {
            return Err(RunnerError::XvfbExited);
        }
        thread::sleep(Duration::from_millis(20));
    }
    Err(RunnerError::DisplayUnavailable)
}

fn wait_cancellable(
    duration: Duration,
    cancellation: &CancellationToken,
) -> Result<(), RunnerError> {
    let deadline = Instant::now() + duration;
    while Instant::now() < deadline {
        if cancellation.is_cancelled() {
            return Err(RunnerError::Cancelled);
        }
        thread::sleep(
            Duration::from_millis(10).min(deadline.saturating_duration_since(Instant::now())),
        );
    }
    Ok(())
}

fn validate_navigation(
    value: &str,
    session: &ComputerSession,
    layout: &ComputerSessionLayout,
) -> Result<(), RunnerError> {
    let url = Url::parse(value).map_err(|_| RunnerError::NavigationDenied)?;
    match url.scheme() {
        "about" if value == "about:blank" => Ok(()),
        "file" => {
            let path = url
                .to_file_path()
                .map_err(|()| RunnerError::NavigationDenied)?;
            let path = fs::canonicalize(path)?;
            let workspace = fs::canonicalize(&layout.workspace)?;
            if path.starts_with(workspace) {
                Ok(())
            } else {
                Err(RunnerError::NavigationDenied)
            }
        }
        "http" | "https" if session.network == NetworkPolicy::Allowed => Ok(()),
        "http" | "https" if session.network == NetworkPolicy::LoopbackOnly => {
            if url
                .host_str()
                .is_some_and(|host| matches!(host, "localhost" | "127.0.0.1" | "[::1]" | "::1"))
            {
                Ok(())
            } else {
                Err(RunnerError::NavigationDenied)
            }
        }
        _ => Err(RunnerError::NavigationDenied),
    }
}

fn runtime_visible_path(
    session: &ComputerSession,
    layout: &ComputerSessionLayout,
    host_path: &Path,
) -> Result<PathBuf, RunnerError> {
    if session.isolation == IsolationRequirement::ReducedExplicitlyAllowed {
        return Ok(host_path.to_path_buf());
    }
    let relative = host_path
        .strip_prefix(&layout.root)
        .map_err(|_| RunnerError::InvalidPath)?;
    Ok(Path::new("/computer").join(relative))
}

fn runtime_browser_path(browser: &Path) -> Result<PathBuf, RunnerError> {
    if [
        Path::new("/usr"),
        Path::new("/bin"),
        Path::new("/opt"),
        Path::new("/snap"),
    ]
    .iter()
    .any(|root| browser.starts_with(root))
    {
        Ok(browser.to_path_buf())
    } else {
        Err(RunnerError::InvalidPath)
    }
}

fn mount_system_path(command: &mut Command, path: &Path) {
    if path.exists() {
        command.arg("--ro-bind").arg(path).arg(path);
    }
}

fn scan_downloads(path: &Path, file_limit: u64) -> Result<Vec<DownloadObservation>, RunnerError> {
    let mut downloads = Vec::new();
    for entry in fs::read_dir(path)? {
        let entry = entry?;
        let metadata = entry.metadata()?;
        if !metadata.is_file() {
            continue;
        }
        if metadata.len() > file_limit {
            return Err(RunnerError::FileLimit);
        }
        downloads.push(DownloadObservation {
            file_name: entry.file_name().to_string_lossy().into_owned(),
            received_bytes: metadata.len(),
            total_bytes: Some(metadata.len()),
            state: DownloadState::Completed,
        });
    }
    Ok(downloads)
}

fn ax_value(value: Option<&Value>) -> String {
    value
        .and_then(|value| value.get("value"))
        .and_then(Value::as_str)
        .map_or_else(String::new, |value| bounded_string(value, 1_024))
}

fn ax_property(node: &Value, name: &str) -> bool {
    node.get("properties")
        .and_then(Value::as_array)
        .is_some_and(|properties| {
            properties.iter().any(|property| {
                property.get("name").and_then(Value::as_str) == Some(name)
                    && property.pointer("/value/value").and_then(Value::as_bool) == Some(true)
            })
        })
}

fn string_field<'a>(value: &'a Value, name: &str) -> Result<&'a str, RunnerError> {
    value
        .get(name)
        .and_then(Value::as_str)
        .ok_or(RunnerError::CdpProtocol)
}

fn mouse_button(button: MouseButton) -> &'static str {
    match button {
        MouseButton::Left => "left",
        MouseButton::Middle => "middle",
        MouseButton::Right => "right",
    }
}

fn bounded_string(value: &str, max_bytes: usize) -> String {
    if value.len() <= max_bytes {
        return value.to_string();
    }
    let end = value
        .char_indices()
        .take_while(|(index, _)| *index <= max_bytes)
        .map(|(index, character)| index + character.len_utf8())
        .last()
        .unwrap_or(0)
        .min(max_bytes);
    value[..end].to_string()
}

fn hex_digest(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    let mut encoded = String::with_capacity(digest.len() * 2);
    for byte in digest {
        write!(encoded, "{byte:02x}").expect("writing to a string cannot fail");
    }
    encoded
}

fn first_file(candidates: &[&str]) -> Option<PathBuf> {
    candidates
        .iter()
        .map(PathBuf::from)
        .find(|candidate| candidate.is_file())
}

/// Reports whether the current syscall sandbox permits the local socket shutdown operation
/// Chromium requires during browser-process initialization.
///
/// # Errors
///
/// Returns an I/O error when a local socket pair cannot be created.
#[cfg(unix)]
pub fn browser_process_host_supported() -> io::Result<bool> {
    use std::net::Shutdown;
    use std::os::unix::net::UnixStream;

    let (left, _right) = UnixStream::pair()?;
    if let Err(error) = left.shutdown(Shutdown::Both) {
        return if error.kind() == io::ErrorKind::PermissionDenied {
            Ok(false)
        } else {
            Err(error)
        };
    }
    match LinuxResourceGroup::probe() {
        Ok(mut resources) => resources
            .cleanup()
            .map(|()| true)
            .map_err(|error| match error {
                RunnerError::Io(error) => error,
                other => io::Error::other(other),
            }),
        Err(RunnerError::ResourceControlUnavailable) => Ok(false),
        Err(RunnerError::Io(error)) => Err(error),
        Err(error) => Err(io::Error::other(error)),
    }
}

#[cfg(not(unix))]
pub fn browser_process_host_supported() -> io::Result<bool> {
    Ok(true)
}

fn timestamp_now() -> Result<UtcTimestamp, RunnerError> {
    UtcTimestamp::now().map_err(|_| RunnerError::Clock)
}

fn write_response(output: &mut impl Write, response: &RunnerResponse) -> Result<(), RunnerError> {
    serde_json::to_writer(&mut *output, response)?;
    output.write_all(b"\n")?;
    output.flush()?;
    Ok(())
}

fn safe_response_error(error: &ComputerError) -> RunnerResponse {
    let (code, safe_message) = match error {
        ComputerError::NotFound => ("not_found", "computer session was not found"),
        ComputerError::ProfileIsolation => ("profile_isolation", "computer session is unavailable"),
        ComputerError::InvalidLifecycle => (
            "invalid_lifecycle",
            "computer lifecycle operation is unavailable",
        ),
        ComputerError::InvalidLimits | ComputerError::InvalidViewport => {
            ("invalid_limits", "computer limits are invalid")
        }
        ComputerError::InvalidAction | ComputerError::InvalidObservation => (
            "invalid_action",
            "computer action or observation is invalid",
        ),
        ComputerError::InvalidCredentialGrant => {
            ("credential_grant", "named credential grant is unavailable")
        }
        ComputerError::Authority | ComputerError::ApprovalRequired => {
            ("approval_required", "computer action is not authorized")
        }
        ComputerError::ApprovalTargetChanged | ComputerError::StaleFrame { .. } => (
            "stale_target",
            "computer target changed and must be observed again",
        ),
        ComputerError::Cancelled => ("cancelled", "computer action was cancelled"),
        ComputerError::ActionRateLimit => ("rate_limit", "computer action rate limit was reached"),
        ComputerError::TooManyAlternates => (
            "alternate_limit",
            "too many alternate actions were supplied",
        ),
        ComputerError::ProcessUnavailable => {
            ("process_unavailable", "computer process is unavailable")
        }
        ComputerError::SnapshotNotFound | ComputerError::SnapshotOwnership => {
            ("snapshot_unavailable", "computer snapshot is unavailable")
        }
        ComputerError::InvalidStorage
        | ComputerError::UnsafeFilesystemEntry
        | ComputerError::DiskLimit => ("storage_safety", "computer storage failed a safety check"),
        ComputerError::Runtime(_) => ("runtime_failure", "computer runtime failed safely"),
        ComputerError::Contract(_) => (
            "contract_denied",
            "computer action contract denied the request",
        ),
        ComputerError::Io(_) | ComputerError::Json(_) => {
            ("storage_failure", "computer state could not be persisted")
        }
    };
    RunnerResponse::Error {
        code: code.into(),
        safe_message: safe_message.into(),
    }
}

fn safe_control_response_error(error: &ControlError) -> RunnerResponse {
    let (code, safe_message) = match error {
        ControlError::NotFound => ("not_found", "computer screen was not found"),
        ControlError::ProfileDenied => ("profile_isolation", "computer screen is unavailable"),
        ControlError::AlreadyExists => ("already_exists", "computer screen already exists"),
        ControlError::Capacity => ("capacity", "computer screen capacity was reached"),
        ControlError::StreamDenied | ControlError::OriginDenied => (
            "stream_denied",
            "computer screen stream authorization was denied",
        ),
        ControlError::StaleLease => ("stale_lease", "computer control changed; refresh and retry"),
        ControlError::InputDenied
        | ControlError::StreamDesynchronized
        | ControlError::FocusAmbiguous => {
            ("input_denied", "computer input is not currently authorized")
        }
        ControlError::StaleFrame => ("stale_frame", "computer screen frame is stale"),
        ControlError::NoPendingRequest => (
            "no_control_request",
            "Keith has not requested computer control",
        ),
        ControlError::InvalidRoot
        | ControlError::InvalidState
        | ControlError::InvalidTime
        | ControlError::Contract(_)
        | ControlError::Io(_)
        | ControlError::Json(_) => ("control_failure", "computer control state failed safely"),
    };
    RunnerResponse::Error {
        code: code.into(),
        safe_message: safe_message.into(),
    }
}

struct SecretBytes(Vec<u8>);

impl SecretBytes {
    fn new(bytes: Vec<u8>) -> Result<Self, RunnerError> {
        if bytes.is_empty()
            || bytes.len() > 64 * 1_024
            || bytes.contains(&b'\n')
            || bytes.contains(&b'\r')
        {
            return Err(RunnerError::CredentialUnavailable);
        }
        Ok(Self(bytes))
    }

    fn with_utf8<T>(&self, use_secret: impl FnOnce(&str) -> T) -> Result<T, RunnerError> {
        let value = std::str::from_utf8(&self.0).map_err(|_| RunnerError::CredentialUnavailable)?;
        Ok(use_secret(value))
    }
}

impl Drop for SecretBytes {
    fn drop(&mut self) {
        self.0.fill(0);
    }
}

#[derive(Debug, Error)]
pub enum RunnerError {
    #[error("headed Chromium is unavailable")]
    BrowserUnavailable,
    #[error("Xvfb is unavailable")]
    XvfbUnavailable,
    #[error("process resource limiter is unavailable")]
    ResourceLimitUnavailable,
    #[error("cgroup v2 memory and process controls are unavailable")]
    ResourceControlUnavailable,
    #[error("strong workstation isolation is unavailable")]
    StrongIsolationUnavailable,
    #[error("computer workstation is already running")]
    AlreadyRunning,
    #[error("computer workstation is not running")]
    NotRunning,
    #[error("virtual display is unavailable")]
    DisplayUnavailable,
    #[error("virtual display process exited")]
    XvfbExited,
    #[error("browser process exited")]
    BrowserExited,
    #[error("browser did not start before its deadline")]
    BrowserBootTimeout,
    #[error("browser debugging pipe is unavailable")]
    PipeUnavailable,
    #[error("browser protocol response is invalid")]
    CdpProtocol,
    #[error("browser protocol command failed")]
    CdpCommand,
    #[error("browser protocol message exceeded its limit")]
    CdpMessageLimit,
    #[error("browser target is unavailable")]
    TargetUnavailable,
    #[error("browser evaluation failed: {0}")]
    BrowserEvaluation(String),
    #[error("computer action was cancelled")]
    Cancelled,
    #[error("computer action timed out")]
    ActionTimeout,
    #[error("browser navigation is denied by network policy")]
    NavigationDenied,
    #[error("computer path is invalid or outside the workspace")]
    InvalidPath,
    #[error("computer file exceeds its resource limit")]
    FileLimit,
    #[error("file upload requires an exact CSS target")]
    UploadTargetMustBeCss,
    #[error("credential injection requires an exact CSS target")]
    CredentialTargetMustBeCss,
    #[error("credential injection requires a protected password field")]
    CredentialTargetMustBeProtected,
    #[error("named credential is unavailable")]
    CredentialUnavailable,
    #[error("keyboard shortcut is invalid")]
    InvalidShortcut,
    #[error("system clock is unavailable")]
    Clock,
    #[error(transparent)]
    Computer(#[from] ComputerError),
    #[error(transparent)]
    Control(#[from] ControlError),
    #[error(transparent)]
    Io(#[from] io::Error),
    #[error(transparent)]
    Json(#[from] serde_json::Error),
}

#[cfg(all(test, target_os = "linux"))]
mod tests {
    use super::*;

    #[test]
    fn real_cgroup_enforces_memory_and_process_limits_and_is_reclaimed() {
        let memory_bytes = 128 * 1_024 * 1_024;
        let max_processes = 4;
        let mut resources =
            LinuxResourceGroup::create_named("resource-test", memory_bytes, max_processes).unwrap();
        let path = resources.path().to_path_buf();
        assert_eq!(
            fs::read_to_string(path.join("memory.max")).unwrap().trim(),
            memory_bytes.to_string()
        );
        assert_eq!(
            fs::read_to_string(path.join("pids.max")).unwrap().trim(),
            max_processes.to_string()
        );
        let mut child = Command::new("/bin/sh");
        child
            .args(["-c", RESOURCE_EXEC_SCRIPT, "cua-resource-test"])
            .arg(&path)
            .args(["/bin/sh", "-c", "sleep 0.2"])
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null());
        let mut child = child.spawn().unwrap();
        let deadline = Instant::now() + Duration::from_secs(1);
        while !fs::read_to_string(path.join("cgroup.procs"))
            .unwrap()
            .lines()
            .any(|pid| pid == child.id().to_string())
            && Instant::now() < deadline
        {
            thread::sleep(Duration::from_millis(5));
        }
        assert!(
            fs::read_to_string(path.join("cgroup.procs"))
                .unwrap()
                .lines()
                .any(|pid| pid == child.id().to_string())
        );
        assert!(child.wait().unwrap().success());
        resources.cleanup().unwrap();
        assert!(!path.exists());
    }
}
