#![forbid(unsafe_code)]

#[cfg(any(target_os = "linux", target_os = "macos"))]
use std::path::Path;
use std::path::PathBuf;

use serde::{Deserialize, Serialize};

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
    if launcher.is_some() {
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
        SandboxStatus {
            backend: SandboxBackend::LinuxProcessGroup,
            level: IsolationLevel::Reduced,
            launcher: None,
            filesystem_containment: false,
            process_tree_control: true,
            network_isolation: false,
            cpu_limit: first_executable(&["/usr/bin/prlimit", "/bin/prlimit"]).is_some(),
            memory_limit: first_executable(&["/usr/bin/prlimit", "/bin/prlimit"]).is_some(),
            reduced_reasons: vec![
                "bubblewrap is unavailable; filesystem and network isolation are reduced".into(),
            ],
        }
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
