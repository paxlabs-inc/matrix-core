#![forbid(unsafe_code)]

use std::ffi::{OsStr, OsString};
use std::path::{Component, Path, PathBuf};

use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HostPlatform {
    Linux,
    MacOs,
    Windows,
    Unsupported,
}

impl HostPlatform {
    pub const fn current() -> Self {
        if cfg!(target_os = "linux") {
            Self::Linux
        } else if cfg!(target_os = "macos") {
            Self::MacOs
        } else if cfg!(target_os = "windows") {
            Self::Windows
        } else {
            Self::Unsupported
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LocalTransportBackend {
    UnixDomainSocket,
    WindowsNamedPipe,
    Unsupported,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ProcessControlBackend {
    PosixProcessGroup,
    WindowsProcessTree,
    Unsupported,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CredentialBackend {
    SecretService,
    AppleKeychain,
    WindowsCredentialManager,
    Unsupported,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PlatformCapabilities {
    pub platform: HostPlatform,
    pub local_transport: LocalTransportBackend,
    pub process_control: ProcessControlBackend,
    pub credentials: CredentialBackend,
    pub tui_restoration: bool,
    pub web_startup: bool,
    pub desktop_startup: bool,
}

impl PlatformCapabilities {
    pub const fn for_platform(platform: HostPlatform) -> Self {
        match platform {
            HostPlatform::Linux => Self {
                platform,
                local_transport: LocalTransportBackend::UnixDomainSocket,
                process_control: ProcessControlBackend::PosixProcessGroup,
                credentials: CredentialBackend::SecretService,
                tui_restoration: true,
                web_startup: true,
                desktop_startup: true,
            },
            HostPlatform::MacOs => Self {
                platform,
                local_transport: LocalTransportBackend::UnixDomainSocket,
                process_control: ProcessControlBackend::PosixProcessGroup,
                credentials: CredentialBackend::AppleKeychain,
                tui_restoration: true,
                web_startup: true,
                desktop_startup: true,
            },
            HostPlatform::Windows => Self {
                platform,
                local_transport: LocalTransportBackend::WindowsNamedPipe,
                process_control: ProcessControlBackend::WindowsProcessTree,
                credentials: CredentialBackend::WindowsCredentialManager,
                tui_restoration: true,
                web_startup: true,
                desktop_startup: true,
            },
            HostPlatform::Unsupported => Self {
                platform,
                local_transport: LocalTransportBackend::Unsupported,
                process_control: ProcessControlBackend::Unsupported,
                credentials: CredentialBackend::Unsupported,
                tui_restoration: false,
                web_startup: false,
                desktop_startup: false,
            },
        }
    }

    pub const fn current() -> Self {
        Self::for_platform(HostPlatform::current())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PlatformPaths {
    pub state_root: PathBuf,
    pub data_root: PathBuf,
    pub config_root: PathBuf,
    pub runtime_root: PathBuf,
    pub credential_root: PathBuf,
    pub daemon_endpoint: PathBuf,
}

impl PlatformPaths {
    /// Discovers application roots from the current operating-system environment.
    ///
    /// # Errors
    ///
    /// Returns an error when required native directories are absent or unsafe.
    pub fn discover() -> Result<Self, PlatformError> {
        Self::discover_for(HostPlatform::current(), |name| std::env::var_os(name))
    }

    /// Discovers application roots for an explicit platform and environment.
    ///
    /// # Errors
    ///
    /// Returns an error when required native directories are absent or unsafe.
    pub fn discover_for(
        platform: HostPlatform,
        environment: impl Fn(&str) -> Option<OsString>,
    ) -> Result<Self, PlatformError> {
        let home = || environment(home_variable(platform)).map(PathBuf::from);
        let paths = match platform {
            HostPlatform::Linux => {
                let home = home().ok_or(PlatformError::MissingHome)?;
                let state = environment("XDG_STATE_HOME")
                    .map_or_else(|| home.join(".local/state"), PathBuf::from)
                    .join("keith-agent");
                let data = environment("XDG_DATA_HOME")
                    .map_or_else(|| home.join(".local/share"), PathBuf::from)
                    .join("keith-agent");
                let config = environment("XDG_CONFIG_HOME")
                    .map_or_else(|| home.join(".config"), PathBuf::from)
                    .join("keith-agent");
                let runtime = environment("XDG_RUNTIME_DIR")
                    .map_or_else(|| state.join("run"), PathBuf::from)
                    .join("keith-agent");
                Self::from_roots(state, data, config, runtime)
            }
            HostPlatform::MacOs => {
                let home = home().ok_or(PlatformError::MissingHome)?;
                let support = home.join("Library/Application Support/Keith Agent");
                Self::from_roots(
                    support.join("state"),
                    support.join("data"),
                    support.join("config"),
                    home.join("Library/Caches/Keith Agent"),
                )
            }
            HostPlatform::Windows => {
                let local = environment("LOCALAPPDATA")
                    .map(PathBuf::from)
                    .ok_or(PlatformError::MissingHome)?
                    .join("Keith Agent");
                let roaming = environment("APPDATA").map_or_else(
                    || local.clone(),
                    |path| PathBuf::from(path).join("Keith Agent"),
                );
                Self::from_roots(
                    local.join("state"),
                    local.join("data"),
                    roaming.join("config"),
                    local.join("run"),
                )
            }
            HostPlatform::Unsupported => return Err(PlatformError::Unsupported),
        };
        paths.validate(platform)?;
        Ok(paths)
    }

    fn from_roots(
        state_root: PathBuf,
        data_root: PathBuf,
        config_root: PathBuf,
        runtime_root: PathBuf,
    ) -> Self {
        Self {
            credential_root: data_root.join("credentials"),
            daemon_endpoint: runtime_root.join("agentd.sock"),
            state_root,
            data_root,
            config_root,
            runtime_root,
        }
    }

    fn validate(&self, platform: HostPlatform) -> Result<(), PlatformError> {
        for path in [
            &self.state_root,
            &self.data_root,
            &self.config_root,
            &self.runtime_root,
            &self.credential_root,
            &self.daemon_endpoint,
        ] {
            if !absolute_for(platform, path)
                || path
                    .components()
                    .any(|component| matches!(component, Component::ParentDir))
            {
                return Err(PlatformError::UnsafePath);
            }
        }
        Ok(())
    }
}

fn absolute_for(platform: HostPlatform, path: &Path) -> bool {
    if platform == HostPlatform::Windows {
        let value = path.as_os_str().to_string_lossy();
        (value.as_bytes().get(1) == Some(&b':')
            && value
                .as_bytes()
                .get(2)
                .is_some_and(|byte| matches!(byte, b'/' | b'\\')))
            || value.starts_with("//")
            || value.starts_with("\\\\")
    } else {
        path.is_absolute()
    }
}

const fn home_variable(platform: HostPlatform) -> &'static str {
    match platform {
        HostPlatform::Windows => "USERPROFILE",
        HostPlatform::Linux | HostPlatform::MacOs | HostPlatform::Unsupported => "HOME",
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ReducedIsolationPolicy {
    Deny,
    RequireAcknowledgement,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct IsolationDecision {
    pub allowed: bool,
    pub reduced: bool,
    pub message: Option<String>,
}

/// Selects whether execution can proceed for the available isolation backend.
///
/// # Errors
///
/// Returns an error when strong isolation is required, acknowledgement is absent, or the
/// reduced-isolation reason is missing.
pub fn decide_isolation(
    strong_available: bool,
    policy: ReducedIsolationPolicy,
    acknowledged: bool,
    reason: &str,
) -> Result<IsolationDecision, PlatformError> {
    if strong_available {
        return Ok(IsolationDecision {
            allowed: true,
            reduced: false,
            message: None,
        });
    }
    if reason.is_empty() {
        return Err(PlatformError::MissingIsolationReason);
    }
    match (policy, acknowledged) {
        (ReducedIsolationPolicy::Deny, _) => Err(PlatformError::IsolationDenied),
        (ReducedIsolationPolicy::RequireAcknowledgement, false) => {
            Err(PlatformError::AcknowledgementRequired)
        }
        (ReducedIsolationPolicy::RequireAcknowledgement, true) => Ok(IsolationDecision {
            allowed: true,
            reduced: true,
            message: Some(reason.to_owned()),
        }),
    }
}

/// Converts a slash-separated protocol path into a portable relative filesystem path.
///
/// # Errors
///
/// Returns an error for absolute, escaping, empty, or host-specific path forms.
pub fn portable_relative_path(value: &str) -> Result<PathBuf, PlatformError> {
    if value.is_empty()
        || value.contains('\\')
        || value.as_bytes().get(1) == Some(&b':')
        || Path::new(value).is_absolute()
    {
        return Err(PlatformError::UnsafePath);
    }
    let path = Path::new(value);
    if path.components().any(|component| {
        !matches!(component, Component::Normal(_))
            || matches!(component, Component::Normal(value) if value == OsStr::new(""))
    }) {
        return Err(PlatformError::UnsafePath);
    }
    Ok(path.to_path_buf())
}

/// Atomically replaces a destination with a fully written temporary file on the same filesystem.
///
/// # Errors
///
/// Returns an error when the temporary path is invalid or the operating-system replacement fails.
pub fn replace_file(temporary: &Path, destination: &Path) -> std::io::Result<()> {
    tempfile::TempPath::try_from_path(temporary.to_path_buf())?
        .persist(destination)
        .map_err(|error| error.error)
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum PlatformError {
    #[error("platform home directory is unavailable")]
    MissingHome,
    #[error("platform path is unsafe")]
    UnsafePath,
    #[error("platform backend is unsupported")]
    Unsupported,
    #[error("strong isolation is required by policy")]
    IsolationDenied,
    #[error("reduced isolation requires explicit acknowledgement")]
    AcknowledgementRequired,
    #[error("reduced isolation must include a user-visible reason")]
    MissingIsolationReason,
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use super::*;

    fn environment<'a>(
        entries: &'a [(&'a str, &'a str)],
    ) -> impl Fn(&str) -> Option<OsString> + 'a {
        let values = entries
            .iter()
            .map(|(key, value)| ((*key).to_owned(), OsString::from(value)))
            .collect::<BTreeMap<_, _>>();
        move |name| values.get(name).cloned()
    }

    #[test]
    fn platform_paths_are_absolute_scoped_and_stable() {
        let linux = PlatformPaths::discover_for(
            HostPlatform::Linux,
            environment(&[("HOME", "/home/keith"), ("XDG_RUNTIME_DIR", "/run/user/42")]),
        )
        .unwrap();
        assert_eq!(
            linux.daemon_endpoint,
            Path::new("/run/user/42/keith-agent/agentd.sock")
        );
        let mac = PlatformPaths::discover_for(
            HostPlatform::MacOs,
            environment(&[("HOME", "/Users/keith")]),
        )
        .unwrap();
        assert!(
            mac.data_root
                .ends_with("Library/Application Support/Keith Agent/data")
        );
        let windows = PlatformPaths::discover_for(
            HostPlatform::Windows,
            environment(&[
                ("LOCALAPPDATA", "C:/Users/keith/AppData/Local"),
                ("APPDATA", "C:/Users/keith/AppData/Roaming"),
            ]),
        )
        .unwrap();
        assert!(
            windows
                .daemon_endpoint
                .ends_with("Keith Agent/run/agentd.sock")
        );
    }

    #[test]
    fn capabilities_name_every_supported_native_backend() {
        for platform in [
            HostPlatform::Linux,
            HostPlatform::MacOs,
            HostPlatform::Windows,
        ] {
            let capabilities = PlatformCapabilities::for_platform(platform);
            assert_ne!(
                capabilities.local_transport,
                LocalTransportBackend::Unsupported
            );
            assert_ne!(
                capabilities.process_control,
                ProcessControlBackend::Unsupported
            );
            assert_ne!(capabilities.credentials, CredentialBackend::Unsupported);
            assert!(capabilities.tui_restoration);
            assert!(capabilities.web_startup);
            assert!(capabilities.desktop_startup);
        }
    }

    #[test]
    fn reduced_isolation_is_denied_or_explicitly_acknowledged() {
        assert_eq!(
            decide_isolation(false, ReducedIsolationPolicy::Deny, true, "reduced"),
            Err(PlatformError::IsolationDenied)
        );
        assert_eq!(
            decide_isolation(
                false,
                ReducedIsolationPolicy::RequireAcknowledgement,
                false,
                "reduced"
            ),
            Err(PlatformError::AcknowledgementRequired)
        );
        assert!(
            decide_isolation(
                false,
                ReducedIsolationPolicy::RequireAcknowledgement,
                true,
                "network isolation unavailable"
            )
            .unwrap()
            .reduced
        );
    }

    #[test]
    fn portable_paths_reject_host_specific_or_escaping_forms() {
        assert_eq!(
            portable_relative_path("sessions/root/events.jsonl").unwrap(),
            Path::new("sessions/root/events.jsonl")
        );
        for invalid in ["../secret", "/absolute", "C:/windows", "windows\\path"] {
            assert_eq!(
                portable_relative_path(invalid),
                Err(PlatformError::UnsafePath)
            );
        }
    }

    #[test]
    fn atomic_replacement_overwrites_an_existing_file_portably() {
        let directory = tempfile::tempdir().unwrap();
        let destination = directory.path().join("state.json");
        let temporary = directory.path().join(".state.tmp");
        std::fs::write(&destination, b"old").unwrap();
        std::fs::write(&temporary, b"new").unwrap();
        replace_file(&temporary, &destination).unwrap();
        assert_eq!(std::fs::read(destination).unwrap(), b"new");
        assert!(!temporary.exists());
    }
}
