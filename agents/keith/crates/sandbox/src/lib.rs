#![forbid(unsafe_code)]

#[cfg(any(target_os = "linux", target_os = "macos"))]
use std::path::Path;
use std::path::PathBuf;
#[cfg(target_os = "linux")]
use std::process::{Command, Stdio};

use serde::{Deserialize, Serialize};

pub fn configure_owned_process(command: &mut std::process::Command) {
    configure_process_group(command);
}

/// Terminates and reaps the complete process tree previously configured by
/// [`configure_owned_process`].
///
/// # Errors
///
/// Returns an I/O error when the process tree cannot be terminated or reaped.
pub fn terminate_owned_process_tree(child: &mut std::process::Child) -> std::io::Result<()> {
    terminate_process_tree(child)
}

#[cfg(unix)]
fn configure_process_group(command: &mut std::process::Command) {
    use std::os::unix::process::CommandExt as _;
    command.process_group(0);
}

#[cfg(windows)]
fn configure_process_group(command: &mut std::process::Command) {
    use std::os::windows::process::CommandExt as _;
    command.creation_flags(0x0000_0200);
}

#[cfg(not(any(unix, windows)))]
fn configure_process_group(_command: &mut std::process::Command) {}

#[cfg(unix)]
fn terminate_process_tree(child: &mut std::process::Child) -> std::io::Result<()> {
    use nix::sys::signal::{Signal, killpg};
    use nix::unistd::Pid;
    if let Ok(pid) = i32::try_from(child.id()) {
        let group = Pid::from_raw(pid);
        if let Err(error) = killpg(group, Signal::SIGTERM)
            && error != nix::errno::Errno::ESRCH
        {
            return Err(std::io::Error::other(error));
        }
        std::thread::sleep(std::time::Duration::from_millis(50));
        if let Err(error) = killpg(group, Signal::SIGKILL)
            && error != nix::errno::Errno::ESRCH
        {
            return Err(std::io::Error::other(error));
        }
    }
    match child.kill() {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::InvalidInput => {}
        Err(error) => return Err(error),
    }
    child.wait().map(|_| ())
}

#[cfg(windows)]
fn terminate_process_tree(child: &mut std::process::Child) -> std::io::Result<()> {
    let status = std::process::Command::new("taskkill")
        .args(["/PID", &child.id().to_string(), "/T", "/F"])
        .status()?;
    if !status.success() && child.try_wait()?.is_none() {
        return Err(std::io::Error::other("taskkill failed"));
    }
    child.wait().map(|_| ())
}

#[cfg(not(any(unix, windows)))]
fn terminate_process_tree(child: &mut std::process::Child) -> std::io::Result<()> {
    if child.try_wait()?.is_none() {
        child.kill()?;
    }
    child.wait().map(|_| ())
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SandboxBackend {
    LinuxBubblewrap,
    LinuxProcessGroup,
    MacOsSandboxExec,
    MacOsProcessGroup,
    WindowsRestrictedProcess,
    Unsupported,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum IsolationLevel {
    Strong,
    Reduced,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)]
pub struct SandboxStatus {
    pub backend: SandboxBackend,
    pub level: IsolationLevel,
    pub launcher: Option<PathBuf>,
    pub filesystem_containment: bool,
    pub process_tree_control: bool,
    pub network_isolation: bool,
    pub cpu_limit: bool,
    pub memory_limit: bool,
    pub reduced_reasons: Vec<String>,
}

impl SandboxStatus {
    pub fn detect() -> Self {
        detect_platform()
    }

    pub fn supports_untrusted(&self) -> bool {
        self.level == IsolationLevel::Strong
            && self.filesystem_containment
            && self.process_tree_control
    }
}

#[cfg(target_os = "linux")]
fn detect_platform() -> SandboxStatus {
    let launcher = first_executable(&["/usr/bin/bwrap", "/bin/bwrap"]);
    let launcher_failure = launcher
        .as_deref()
        .and_then(|candidate| bubblewrap_probe(candidate).err());
    if launcher.is_some() && launcher_failure.is_none() {
        SandboxStatus {
            backend: SandboxBackend::LinuxBubblewrap,
            level: IsolationLevel::Strong,
            launcher,
            filesystem_containment: true,
            process_tree_control: true,
            network_isolation: true,
            cpu_limit: first_executable(&["/usr/bin/prlimit", "/bin/prlimit"]).is_some(),
            memory_limit: first_executable(&["/usr/bin/prlimit", "/bin/prlimit"]).is_some(),
            reduced_reasons: Vec::new(),
        }
    } else {
        let reason = launcher_failure.map_or_else(
            || "bubblewrap is unavailable; filesystem and network isolation are reduced".into(),
            |detail| format!("bubblewrap isolation probe failed: {detail}"),
        );
        SandboxStatus {
            backend: SandboxBackend::LinuxProcessGroup,
            level: IsolationLevel::Reduced,
            launcher: None,
            filesystem_containment: false,
            process_tree_control: true,
            network_isolation: false,
            cpu_limit: first_executable(&["/usr/bin/prlimit", "/bin/prlimit"]).is_some(),
            memory_limit: first_executable(&["/usr/bin/prlimit", "/bin/prlimit"]).is_some(),
            reduced_reasons: vec![reason],
        }
    }
}

#[cfg(target_os = "linux")]
fn bubblewrap_probe(launcher: &Path) -> Result<(), String> {
    let output = Command::new(launcher)
        .args([
            "--die-with-parent",
            "--new-session",
            "--unshare-all",
            "--ro-bind",
            "/usr",
            "/usr",
            "--symlink",
            "usr/bin",
            "/bin",
            "--symlink",
            "usr/lib",
            "/lib",
            "--symlink",
            "usr/lib64",
            "/lib64",
            "--dev",
            "/dev",
            "--proc",
            "/proc",
            "--",
            "/usr/bin/true",
        ])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::piped())
        .output()
        .map_err(|error| error.to_string())?;
    if output.status.success() {
        return Ok(());
    }
    let detail = String::from_utf8_lossy(&output.stderr);
    let detail = detail.trim();
    if detail.is_empty() {
        Err(format!("launcher exited with {}", output.status))
    } else {
        Err(detail.chars().take(512).collect())
    }
}

#[cfg(target_os = "macos")]
fn detect_platform() -> SandboxStatus {
    let launcher = first_executable(&["/usr/bin/sandbox-exec"]);
    let strong = launcher.is_some();
    SandboxStatus {
        backend: if strong {
            SandboxBackend::MacOsSandboxExec
        } else {
            SandboxBackend::MacOsProcessGroup
        },
        level: if strong {
            IsolationLevel::Strong
        } else {
            IsolationLevel::Reduced
        },
        launcher,
        filesystem_containment: strong,
        process_tree_control: true,
        network_isolation: strong,
        cpu_limit: false,
        memory_limit: false,
        reduced_reasons: if strong {
            vec!["native CPU and address-space limits require an external supervisor".into()]
        } else {
            vec!["sandbox-exec is unavailable; execution uses process-group containment".into()]
        },
    }
}

#[cfg(target_os = "windows")]
fn detect_platform() -> SandboxStatus {
    SandboxStatus {
        backend: SandboxBackend::WindowsRestrictedProcess,
        level: IsolationLevel::Reduced,
        launcher: None,
        filesystem_containment: false,
        process_tree_control: true,
        network_isolation: false,
        cpu_limit: false,
        memory_limit: false,
        reduced_reasons: vec![
            "restricted process and task-tree cleanup are available; AppContainer and Job Object resource limits are not enabled".into(),
        ],
    }
}

#[cfg(not(any(target_os = "linux", target_os = "macos", target_os = "windows")))]
fn detect_platform() -> SandboxStatus {
    SandboxStatus {
        backend: SandboxBackend::Unsupported,
        level: IsolationLevel::Reduced,
        launcher: None,
        filesystem_containment: false,
        process_tree_control: false,
        network_isolation: false,
        cpu_limit: false,
        memory_limit: false,
        reduced_reasons: vec!["this platform has no configured sandbox backend".into()],
    }
}

#[cfg(any(target_os = "linux", target_os = "macos"))]
fn first_executable(candidates: &[&str]) -> Option<PathBuf> {
    candidates
        .iter()
        .map(Path::new)
        .find(|path| path.is_file())
        .map(Path::to_path_buf)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn platform_status_never_claims_an_unimplemented_guarantee() {
        let status = SandboxStatus::detect();
        if status.level == IsolationLevel::Strong {
            assert!(status.launcher.is_some());
            assert!(status.filesystem_containment);
            assert!(status.process_tree_control);
        } else {
            assert!(!status.reduced_reasons.is_empty());
        }
    }
}
