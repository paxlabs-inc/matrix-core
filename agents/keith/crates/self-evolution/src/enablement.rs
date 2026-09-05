use std::fs::{self, OpenOptions};
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use keith_agent_types::{EntityId, UtcTimestamp};
use keith_sandbox::{SandboxStatus, configure_owned_process, terminate_owned_process_tree};
use keith_state_store_core::EvolutionLedgerRepository;
use ring::hmac;
use tempfile::TempDir;
use thiserror::Error;

use crate::{
    DependencyConsent, EvolutionEvent, EvolutionLedger, LedgerError, LedgerText, ProtectedSurface,
    ShadowTree,
};

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EnablementDisclosure {
    pub editable_surface: &'static str,
    pub protected_surface: &'static [&'static str],
    pub autonomy: &'static str,
    pub reversal: &'static str,
}

impl EnablementDisclosure {
    #[must_use]
    pub const fn installation() -> Self {
        Self {
            editable_surface: "permitted Rust source outside the compiled protected surface",
            protected_surface: ProtectedSurface::PATHS,
            autonomy: "class A autonomous; class B watchdog-bounded; class C owner-approved; class D refused",
            reversal: "every promotion remains individually reversible after disable",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct InstallationAuthority {
    identity: String,
    binding: [u8; 32],
    _private: (),
}

/// Installation-bound authority produced while atomically cancelling build and canary work.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReversalAuthority {
    pub(crate) identity: String,
    pub(crate) installation_root: PathBuf,
    _private: (),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum EvolutionUnavailable {
    Toolchain(String),
    Sandbox(Vec<String>),
    ImageDirectory(String),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum EvolutionAvailability {
    Available { rustc: String, cargo: String },
    Unavailable(Vec<EvolutionUnavailable>),
}

impl EvolutionAvailability {
    #[must_use]
    pub const fn is_available(&self) -> bool {
        matches!(self, Self::Available { .. })
    }
}

pub struct ProcessArtifacts {
    child: Child,
    owned_root: PathBuf,
    staged_paths: Vec<PathBuf>,
}

pub struct EvolutionWorkRoot(PathBuf);

impl EvolutionWorkRoot {
    /// Opens an installation-owned directory for isolated evolution artifacts.
    ///
    /// # Errors
    ///
    /// Returns an error when the path cannot be created, is a symlink, or is not a directory.
    pub fn open(path: impl Into<PathBuf>) -> Result<Self, std::io::Error> {
        let path = path.into();
        fs::create_dir_all(&path)?;
        let metadata = fs::symlink_metadata(&path)?;
        if metadata.file_type().is_symlink() || !metadata.is_dir() {
            return Err(std::io::Error::other(
                "evolution work root must be a regular directory",
            ));
        }
        Ok(Self(fs::canonicalize(path)?))
    }

    #[must_use]
    pub(crate) fn path(&self) -> &Path {
        &self.0
    }

    #[cfg(test)]
    pub(crate) fn for_test(path: PathBuf) -> Self {
        Self(path)
    }
}

impl ProcessArtifacts {
    /// # Errors
    /// Returns an error for paths outside the owned root or process spawn failure.
    pub fn spawn(
        command: &mut Command,
        owned_root: &EvolutionWorkRoot,
        staged_paths: Vec<PathBuf>,
    ) -> Result<Self, String> {
        let owned_root = fs::canonicalize(&owned_root.0).map_err(|error| error.to_string())?;
        for path in &staged_paths {
            let parent = path
                .parent()
                .ok_or_else(|| "staged path has no parent".to_owned())?;
            let canonical_parent = fs::canonicalize(parent).map_err(|error| error.to_string())?;
            if !canonical_parent.starts_with(&owned_root) {
                return Err("staged path is outside the owned evolution root".into());
            }
        }
        configure_owned_process(command);
        let child = command.spawn().map_err(|error| error.to_string())?;
        Ok(Self {
            child,
            owned_root,
            staged_paths,
        })
    }
}

impl ProcessArtifacts {
    fn abort_and_cleanup(&mut self) -> Result<(), String> {
        terminate_owned_process_tree(&mut self.child).map_err(|error| error.to_string())?;
        for path in &self.staged_paths {
            let parent = path
                .parent()
                .ok_or_else(|| "staged path has no parent".to_owned())?;
            let canonical_parent = fs::canonicalize(parent).map_err(|error| error.to_string())?;
            if !canonical_parent.starts_with(&self.owned_root) {
                return Err("cleanup path escaped owned root".into());
            }
            match fs::symlink_metadata(path) {
                Ok(metadata) if metadata.is_dir() => {
                    fs::remove_dir_all(path).map_err(|e| e.to_string())?;
                }
                Ok(_) => fs::remove_file(path).map_err(|e| e.to_string())?,
                Err(error) if error.kind() == io::ErrorKind::NotFound => {}
                Err(error) => return Err(error.to_string()),
            }
        }
        Ok(())
    }
}

#[derive(Clone, Default)]
pub struct EvolutionCancellation(Arc<AtomicBool>);
impl EvolutionCancellation {
    fn cancel(&self) {
        self.0.store(true, Ordering::Release);
    }
    #[must_use]
    pub fn is_cancelled(&self) -> bool {
        self.0.load(Ordering::Acquire)
    }
}

#[derive(Debug, Error)]
pub enum EnablementError {
    #[error("installation-owner authentication is required")]
    Unauthenticated,
    #[error("self-evolution is disabled")]
    Disabled,
    #[error("self-evolution is unavailable: {0:?}")]
    Unavailable(Vec<EvolutionUnavailable>),
    #[error("enablement disclosure was not acknowledged exactly")]
    DisclosureNotAcknowledged,
    #[error("evolution ledger failed: {0}")]
    Ledger(#[from] LedgerError),
    #[error("disable cleanup remains unresolved: {0:?}")]
    Cleanup(Vec<String>),
}

pub struct SelfEvolutionEnablement<R> {
    installation_root: PathBuf,
    enabled: AtomicBool,
    cancellation: Mutex<EvolutionCancellation>,
    operations: Mutex<Vec<ProcessArtifacts>>,
    lifecycle: Mutex<()>,
    installation_credential: [u8; 32],
    installation_identity: String,
    ledger: Arc<EvolutionLedger<R>>,
}

impl<R: EvolutionLedgerRepository> SelfEvolutionEnablement<R> {
    /// Issues a one-proposal dependency consent after revalidating installation ownership.
    ///
    /// # Errors
    /// Returns [`EnablementError::Unauthenticated`] when authority belongs to another installation.
    pub fn approve_dependency_additions(
        &self,
        authority: &InstallationAuthority,
        shadow_id: &EntityId,
        base_revision: &str,
        dependencies: &[String],
        proposal_digest: [u8; 32],
    ) -> Result<DependencyConsent, EnablementError> {
        self.validate_authority(authority)?;
        Ok(DependencyConsent::issue(
            shadow_id,
            base_revision,
            dependencies,
            proposal_digest,
            authority.binding,
        ))
    }
    #[must_use]
    pub fn new(
        root: PathBuf,
        installation_credential: [u8; 32],
        installation_identity: String,
        ledger: Arc<EvolutionLedger<R>>,
    ) -> Self {
        Self::new_restored(
            root,
            installation_credential,
            installation_identity,
            ledger,
            false,
        )
    }

    /// Restores the enablement bit only after the caller has verified the signed ledger chain.
    #[must_use]
    pub fn new_restored(
        root: PathBuf,
        installation_credential: [u8; 32],
        installation_identity: String,
        ledger: Arc<EvolutionLedger<R>>,
        enabled: bool,
    ) -> Self {
        let root = fs::canonicalize(&root).unwrap_or(root);
        Self {
            installation_root: root,
            enabled: AtomicBool::new(enabled),
            cancellation: Mutex::new(EvolutionCancellation::default()),
            operations: Mutex::new(Vec::new()),
            lifecycle: Mutex::new(()),
            installation_credential,
            installation_identity,
            ledger,
        }
    }

    /// # Errors
    /// Returns [`EnablementError::Unauthenticated`] for invalid credentials.
    pub fn authenticate_installation(
        &self,
        credential: &[u8],
    ) -> Result<InstallationAuthority, EnablementError> {
        if self.installation_identity.trim().is_empty()
            || credential.len() != 32
            || !constant_time_equal(credential, &self.installation_credential)
        {
            return Err(EnablementError::Unauthenticated);
        }
        Ok(InstallationAuthority {
            identity: self.installation_identity.clone(),
            binding: self.authority_binding(),
            _private: (),
        })
    }

    #[must_use]
    pub fn enabled(&self) -> bool {
        self.enabled.load(Ordering::Acquire)
    }

    /// # Errors
    /// Returns an error while disabled or when operation startup fails.
    ///
    /// # Panics
    /// Panics only if an internal lifecycle lock was poisoned.
    pub fn begin_operation<F>(&self, start: F) -> Result<EvolutionCancellation, EnablementError>
    where
        F: FnOnce() -> Result<ProcessArtifacts, String>,
    {
        let mut operations = self.operations.lock().expect("operation lock");
        if !self.enabled() {
            return Err(EnablementError::Disabled);
        }
        let token = self.cancellation.lock().expect("cancellation lock").clone();
        operations.push(start().map_err(|error| EnablementError::Cleanup(vec![error]))?);
        Ok(token)
    }

    /// # Errors
    /// Returns an error for stale disclosure, unavailable verification, or ledger failure.
    ///
    /// # Panics
    /// Panics only if an internal lifecycle lock was poisoned.
    pub fn enable(
        &self,
        authority: &InstallationAuthority,
        disclosure: &EnablementDisclosure,
        now: UtcTimestamp,
    ) -> Result<EvolutionAvailability, EnablementError> {
        let _lifecycle = self.lifecycle.lock().expect("lifecycle lock");
        self.validate_authority(authority)?;
        if !self.operations.lock().expect("operation lock").is_empty() {
            return Err(EnablementError::Cleanup(vec![
                "cleanup remains unresolved".into(),
            ]));
        }
        if disclosure != &EnablementDisclosure::installation() {
            return Err(EnablementError::DisclosureNotAcknowledged);
        }
        let work_root = self
            .work_root()
            .map_err(|error| EnablementError::Cleanup(vec![error.to_string()]))?;
        ShadowTree::reclaim_abandoned(&work_root)
            .map_err(|error| EnablementError::Cleanup(vec![error.to_string()]))?;
        let availability = probe_availability(&self.installation_root);
        let EvolutionAvailability::Available { .. } = &availability else {
            let EvolutionAvailability::Unavailable(reasons) = availability else {
                unreachable!()
            };
            return Err(EnablementError::Unavailable(reasons));
        };
        self.ledger.append(
            EntityId::new(),
            now,
            EvolutionEvent::Enable {
                acting_identity: LedgerText::redacted(&authority.identity, 256, &[])?,
            },
        )?;
        *self.cancellation.lock().expect("cancellation lock") = EvolutionCancellation::default();
        self.enabled.store(true, Ordering::Release);
        Ok(availability)
    }

    /// # Errors
    /// Returns an error when cleanup remains unresolved or ledger recording fails.
    ///
    /// # Panics
    /// Panics only if an internal lifecycle lock was poisoned.
    pub fn disable(
        &self,
        authority: &InstallationAuthority,
        reason: &str,
        now: UtcTimestamp,
    ) -> Result<(), EnablementError> {
        let _lifecycle = self.lifecycle.lock().expect("lifecycle lock");
        self.validate_authority(authority)?;
        let safe_reason = LedgerText::redacted(reason, 1024, &[]).unwrap_or_else(|_| {
            LedgerText::redacted("installation owner requested disable", 1024, &[])
                .expect("fixed disable reason is safe")
        });
        self.enabled.store(false, Ordering::Release);
        self.cancellation
            .lock()
            .expect("cancellation lock")
            .cancel();
        let mut operations = self.operations.lock().expect("operation lock");
        let mut retained = Vec::new();
        let mut failures = Vec::new();
        for (index, mut operation) in operations.drain(..).enumerate() {
            if let Err(error) = operation.abort_and_cleanup() {
                let _ = error;
                failures.push(format!("cleanup operation {index} remains unresolved"));
                retained.push(operation);
            }
        }
        *operations = retained;
        match self.work_root().and_then(|root| {
            ShadowTree::reclaim_abandoned(&root)
                .map(|_| ())
                .map_err(io::Error::other)
        }) {
            Ok(()) => {}
            Err(error) => failures.push(format!("shadow cleanup remains unresolved: {error}")),
        }
        let unresolved_cleanup = failures
            .iter()
            .map(|failure| LedgerText::redacted(failure, 1024, &[]))
            .collect::<Result<Vec<_>, _>>()?;
        self.ledger.append(
            EntityId::new(),
            now,
            EvolutionEvent::Disable {
                acting_identity: LedgerText::redacted(&authority.identity, 256, &[])?,
                reason: safe_reason,
                unresolved_cleanup,
            },
        )?;
        if failures.is_empty() {
            Ok(())
        } else {
            Err(EnablementError::Cleanup(failures))
        }
    }

    /// Authenticates a reversal and aborts all in-flight evolution work without consulting the
    /// enabled flag. Already promoted state is deliberately left intact for the reversal journal.
    ///
    /// # Errors
    /// Returns an error for foreign installation authority or incomplete process cleanup.
    pub fn authorize_reversal(
        &self,
        authority: &InstallationAuthority,
    ) -> Result<ReversalAuthority, EnablementError> {
        let _lifecycle = self.lifecycle.lock().expect("lifecycle lock");
        self.validate_authority(authority)?;
        self.cancellation
            .lock()
            .expect("cancellation lock")
            .cancel();
        let mut operations = self.operations.lock().expect("operation lock");
        let mut retained = Vec::new();
        let mut failures = Vec::new();
        for (index, mut operation) in operations.drain(..).enumerate() {
            if operation.abort_and_cleanup().is_err() {
                failures.push(format!("cleanup operation {index} remains unresolved"));
                retained.push(operation);
            }
        }
        *operations = retained;
        if !failures.is_empty() {
            return Err(EnablementError::Cleanup(failures));
        }
        if self.enabled() {
            *self.cancellation.lock().expect("cancellation lock") =
                EvolutionCancellation::default();
        }
        Ok(ReversalAuthority {
            identity: authority.identity.clone(),
            installation_root: self.installation_root.clone(),
            _private: (),
        })
    }

    fn validate_authority(&self, authority: &InstallationAuthority) -> Result<(), EnablementError> {
        if authority.identity != self.installation_identity
            || !constant_time_equal(&authority.binding, &self.authority_binding())
        {
            return Err(EnablementError::Unauthenticated);
        }
        Ok(())
    }

    fn authority_binding(&self) -> [u8; 32] {
        let key = hmac::Key::new(hmac::HMAC_SHA256, &self.installation_credential);
        let mut material = self
            .installation_root
            .as_os_str()
            .as_encoded_bytes()
            .to_vec();
        material.extend_from_slice(self.installation_identity.as_bytes());
        hmac::sign(&key, &material)
            .as_ref()
            .try_into()
            .expect("HMAC-SHA256 length")
    }

    /// Resolves the only root from which evolution process cleanup may remove artifacts.
    ///
    /// # Errors
    /// Returns an error when the installation root is unusable or containment fails.
    pub fn work_root(&self) -> io::Result<EvolutionWorkRoot> {
        let installation = fs::canonicalize(&self.installation_root)?;
        let path = installation.join("evolution-work");
        fs::create_dir_all(&path)?;
        let path = fs::canonicalize(path)?;
        if !path.starts_with(&installation) || path == installation {
            return Err(io::Error::other("evolution work root escapes installation"));
        }
        Ok(EvolutionWorkRoot(path))
    }
}

fn constant_time_equal(left: &[u8], right: &[u8]) -> bool {
    left.iter()
        .zip(right)
        .fold(0_u8, |difference, (left, right)| {
            difference | (left ^ right)
        })
        == 0
}

#[must_use]
pub fn probe_availability(root: &Path) -> EvolutionAvailability {
    probe_availability_with(root, Path::new("rustc"), Path::new("cargo"))
}

fn probe_availability_with(root: &Path, rustc: &Path, cargo: &Path) -> EvolutionAvailability {
    let image_directory = match owned_image_directory(root) {
        Ok(path) => path,
        Err(error) => {
            return EvolutionAvailability::Unavailable(vec![EvolutionUnavailable::ImageDirectory(
                error.to_string(),
            )]);
        }
    };
    let mut failures = Vec::new();
    let sandbox = SandboxStatus::detect();
    if !sandbox.supports_untrusted()
        || !sandbox.network_isolation
        || !sandbox.cpu_limit
        || !sandbox.memory_limit
    {
        let mut reasons = sandbox.reduced_reasons;
        if !sandbox.cpu_limit {
            reasons.push("CPU limiting is unavailable".into());
        }
        if !sandbox.memory_limit {
            reasons.push("memory limiting is unavailable".into());
        }
        failures.push(EvolutionUnavailable::Sandbox(reasons));
    }
    let (rustc_version, cargo_version) = match probe_toolchain(&image_directory, rustc, cargo) {
        Ok(versions) => versions,
        Err(error) => {
            failures.push(EvolutionUnavailable::Toolchain(error.to_string()));
            (String::new(), String::new())
        }
    };
    if failures.is_empty() {
        EvolutionAvailability::Available {
            rustc: rustc_version,
            cargo: cargo_version,
        }
    } else {
        EvolutionAvailability::Unavailable(failures)
    }
}

fn owned_image_directory(root: &Path) -> io::Result<PathBuf> {
    let root = fs::canonicalize(root)?;
    let directory = root.join("evolution-images");
    fs::create_dir_all(&directory)?;
    let canonical = fs::canonicalize(&directory)?;
    if !canonical.starts_with(&root) || canonical == root {
        return Err(io::Error::other(
            "image directory escapes installation root",
        ));
    }
    let probe = canonical.join(format!(".probe-{}", EntityId::new()));
    let file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(&probe)?;
    file.sync_all()?;
    drop(file);
    fs::remove_file(probe)?;
    Ok(canonical)
}

fn probe_toolchain(directory: &Path, rustc: &Path, cargo: &Path) -> io::Result<(String, String)> {
    let rustc_version = command_output(rustc, &["--version"])?;
    let cargo_version = command_output(cargo, &["--version"])?;
    if rustc_version.is_empty() || cargo_version.is_empty() {
        return Err(io::Error::other("toolchain version output is empty"));
    }
    let project = TempDir::new_in(directory)?;
    fs::create_dir(project.path().join("src"))?;
    fs::write(
        project.path().join("Cargo.toml"),
        "[package]\nname='keith_probe'\nversion='0.0.0'\nedition='2024'\n",
    )?;
    fs::write(project.path().join("src/main.rs"), "fn main() {}\n")?;
    let cargo_status = Command::new(cargo)
        .args(["check", "--offline", "--quiet"])
        .current_dir(project.path())
        .env_clear()
        .env("PATH", toolchain_path())
        .env("RUSTUP_HOME", toolchain_home("RUSTUP_HOME", ".rustup"))
        .env("CARGO_HOME", toolchain_home("CARGO_HOME", ".cargo"))
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()?;
    if !cargo_status.success() {
        return Err(io::Error::other(
            "cargo could not compile an isolated crate",
        ));
    }
    let source = project.path().join("standalone.rs");
    let binary = project.path().join("standalone");
    fs::File::create(&source)?.write_all(b"fn main() {}\n")?;
    let rustc_status = Command::new(rustc)
        .arg(&source)
        .arg("-o")
        .arg(&binary)
        .env_clear()
        .env("PATH", toolchain_path())
        .env("RUSTUP_HOME", toolchain_home("RUSTUP_HOME", ".rustup"))
        .env("CARGO_HOME", toolchain_home("CARGO_HOME", ".cargo"))
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status()?;
    if !rustc_status.success() || !binary.is_file() {
        return Err(io::Error::other(
            "rustc could not link an isolated executable",
        ));
    }
    Ok((rustc_version, cargo_version))
}

fn command_output(program: &Path, args: &[&str]) -> io::Result<String> {
    let output = Command::new(program)
        .args(args)
        .env_clear()
        .env("PATH", toolchain_path())
        .env("RUSTUP_HOME", toolchain_home("RUSTUP_HOME", ".rustup"))
        .env("CARGO_HOME", toolchain_home("CARGO_HOME", ".cargo"))
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .output()?;
    if !output.status.success() {
        return Err(io::Error::other(format!(
            "{} exited with {}",
            program.display(),
            output.status
        )));
    }
    Ok(String::from_utf8_lossy(&output.stdout)
        .trim()
        .chars()
        .take(512)
        .collect())
}

fn toolchain_path() -> std::ffi::OsString {
    std::env::var_os("PATH").unwrap_or_else(|| "/usr/local/bin:/usr/bin:/bin".into())
}

fn toolchain_home(variable: &str, suffix: &str) -> PathBuf {
    std::env::var_os(variable).map_or_else(
        || PathBuf::from(std::env::var_os("HOME").unwrap_or_default()).join(suffix),
        PathBuf::from,
    )
}

#[cfg(test)]
mod tests {
    use super::*;
    use keith_state_store::EmbeddedStore;
    #[cfg(unix)]
    use std::os::unix::fs::{PermissionsExt, symlink};
    #[cfg(target_os = "linux")]
    use std::thread;
    #[cfg(target_os = "linux")]
    use std::time::Duration;

    fn service(root: &TempDir) -> SelfEvolutionEnablement<EmbeddedStore> {
        let ledger = Arc::new(
            EvolutionLedger::from_seed(
                Arc::new(EmbeddedStore::open_in_memory().unwrap()),
                &[9; 32],
            )
            .unwrap(),
        );
        SelfEvolutionEnablement::new(root.path().into(), [7; 32], "owner".into(), ledger)
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn successful_enable_and_disable_are_signed_with_the_acting_identity() {
        let root = TempDir::new().unwrap();
        let repository = Arc::new(EmbeddedStore::open_in_memory().unwrap());
        let ledger =
            Arc::new(EvolutionLedger::from_seed(Arc::clone(&repository), &[8; 32]).unwrap());
        let service = SelfEvolutionEnablement::new(
            root.path().into(),
            [7; 32],
            "installation-owner".into(),
            Arc::clone(&ledger),
        );
        let authority = service.authenticate_installation(&[7; 32]).unwrap();
        assert!(
            service
                .enable(
                    &authority,
                    &EnablementDisclosure::installation(),
                    UtcTimestamp::UNIX_EPOCH
                )
                .unwrap()
                .is_available()
        );
        assert!(service.enabled());
        service
            .disable(
                &authority,
                "private reasoning",
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        let records = ledger.records().unwrap();
        assert!(
            matches!(&records[0].event, EvolutionEvent::Enable { acting_identity } if acting_identity.as_str() == "installation-owner")
        );
        assert!(
            matches!(&records[1].event, EvolutionEvent::Disable { acting_identity, reason, unresolved_cleanup } if acting_identity.as_str() == "installation-owner" && reason.as_str() == "installation owner requested disable" && unresolved_cleanup.is_empty())
        );
    }

    #[test]
    fn authority_is_bound_to_one_installation_and_fixed_owner_identity() {
        let first_root = TempDir::new().unwrap();
        let second_root = TempDir::new().unwrap();
        let first = service(&first_root);
        let second = service(&second_root);
        let authority = first.authenticate_installation(&[7; 32]).unwrap();
        assert!(matches!(
            second.enable(
                &authority,
                &EnablementDisclosure::installation(),
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(EnablementError::Unauthenticated)
        ));
        let shadow_id = EntityId::new();
        let dependencies = vec!["crate:dependencies:serde=\"1\"".to_owned()];
        assert!(
            first
                .approve_dependency_additions(
                    &authority,
                    &shadow_id,
                    "0123456789012345678901234567890123456789",
                    &dependencies,
                    [3; 32],
                )
                .is_ok()
        );
        assert!(matches!(
            second.approve_dependency_additions(
                &authority,
                &shadow_id,
                "0123456789012345678901234567890123456789",
                &dependencies,
                [3; 32],
            ),
            Err(EnablementError::Unauthenticated)
        ));
    }

    #[test]
    fn default_off_rejects_work_without_writing_an_image_directory() {
        let root = TempDir::new().unwrap();
        let service = service(&root);
        let started = Arc::new(AtomicBool::new(false));
        let observed = Arc::clone(&started);
        assert!(matches!(
            service.begin_operation(move || {
                observed.store(true, Ordering::Release);
                Err("disabled admission invoked its process factory".into())
            }),
            Err(EnablementError::Disabled)
        ));
        assert!(!started.load(Ordering::Acquire));
        assert!(!root.path().join("shadow").exists());
        assert!(!root.path().join("evolution-images").exists());
    }
    #[test]
    fn authentication_and_missing_toolchain_fail_closed() {
        let root = TempDir::new().unwrap();
        let service = service(&root);
        assert!(service.authenticate_installation(&[6; 32]).is_err());
        assert!(matches!(
            probe_availability_with(
                root.path(),
                &root.path().join("missing-rustc"),
                &root.path().join("missing-cargo")
            ),
            EvolutionAvailability::Unavailable(_)
        ));
    }
    #[cfg(unix)]
    #[test]
    fn version_only_executables_are_not_a_usable_toolchain() {
        let root = TempDir::new().unwrap();
        let images = owned_image_directory(root.path()).unwrap();
        let fake = root.path().join("version-only");
        fs::write(&fake, "#!/bin/sh\necho version-only\n").unwrap();
        fs::set_permissions(&fake, fs::Permissions::from_mode(0o700)).unwrap();
        assert!(probe_toolchain(&images, &fake, &fake).is_err());
    }
    #[test]
    fn installed_toolchain_compiles_and_links_an_isolated_crate() {
        let root = TempDir::new().unwrap();
        let images = owned_image_directory(root.path()).unwrap();
        let (rustc, cargo) =
            probe_toolchain(&images, Path::new("rustc"), Path::new("cargo")).unwrap();
        assert!(rustc.starts_with("rustc "));
        assert!(cargo.starts_with("cargo "));
    }
    #[cfg(unix)]
    #[test]
    fn image_root_rejects_symlink_escape() {
        let root = TempDir::new().unwrap();
        let outside = TempDir::new().unwrap();
        symlink(outside.path(), root.path().join("evolution-images")).unwrap();
        assert!(owned_image_directory(root.path()).is_err());
    }
    #[cfg(target_os = "linux")]
    #[test]
    fn disable_kills_real_process_and_removes_real_artifacts() {
        let root = TempDir::new().unwrap();
        let service = service(&root);
        let authority = service.authenticate_installation(&[7; 32]).unwrap();
        service
            .enable(
                &authority,
                &EnablementDisclosure::installation(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let work_root = service.work_root().unwrap();
        let staged = work_root.0.join("shadow");
        fs::create_dir(&staged).unwrap();
        fs::write(staged.join("candidate"), b"bytes").unwrap();
        let descendant_file = root.path().join("descendant.pid");
        let work_root_for_operation = work_root;
        let staged_for_operation = staged.clone();
        let descendant_for_operation = descendant_file.clone();
        let cancellation = service
            .begin_operation(move || {
                let mut command = Command::new("/bin/sh");
                command.args([
                    "-c",
                    &format!(
                        "sleep 30 & echo $! > {}; wait",
                        descendant_for_operation.display()
                    ),
                ]);
                ProcessArtifacts::spawn(
                    &mut command,
                    &work_root_for_operation,
                    vec![staged_for_operation],
                )
            })
            .unwrap();
        for _ in 0..100 {
            if descendant_file.exists() {
                break;
            }
            thread::sleep(Duration::from_millis(10));
        }
        let descendant_pid = fs::read_to_string(&descendant_file).unwrap();
        service
            .disable(&authority, "operator disabled", UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        assert!(cancellation.is_cancelled());
        assert!(!staged.exists());
        thread::sleep(Duration::from_millis(20));
        assert!(!Path::new(&format!("/proc/{}", descendant_pid.trim())).exists());
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn unresolved_real_cleanup_blocks_enable_until_reconciled() {
        let root = TempDir::new().unwrap();
        let service = service(&root);
        let authority = service.authenticate_installation(&[7; 32]).unwrap();
        service
            .enable(
                &authority,
                &EnablementDisclosure::installation(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let work_root = service.work_root().unwrap();
        let holder = work_root.0.join("holder");
        let displaced = work_root.0.join("displaced");
        fs::create_dir(&holder).unwrap();
        let staged = holder.join("candidate");
        fs::write(&staged, b"candidate").unwrap();
        let work_root_for_operation = work_root;
        service
            .begin_operation(move || {
                let mut command = Command::new("/bin/sh");
                command.args(["-c", "exit 0"]);
                ProcessArtifacts::spawn(&mut command, &work_root_for_operation, vec![staged])
            })
            .unwrap();
        fs::rename(&holder, &displaced).unwrap();
        assert!(matches!(
            service.disable(&authority, "reconcile", UtcTimestamp::from_unix_millis(1)),
            Err(EnablementError::Cleanup(_))
        ));
        assert!(matches!(
            service.enable(
                &authority,
                &EnablementDisclosure::installation(),
                UtcTimestamp::from_unix_millis(2)
            ),
            Err(EnablementError::Cleanup(_))
        ));
        fs::rename(&displaced, &holder).unwrap();
        service
            .disable(
                &authority,
                "retry cleanup",
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        assert!(
            service
                .enable(
                    &authority,
                    &EnablementDisclosure::installation(),
                    UtcTimestamp::from_unix_millis(4)
                )
                .is_ok()
        );
    }
}
