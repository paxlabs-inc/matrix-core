use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

use fs2::FileExt;
use keith_agent_types::{EntityId, UtcTimestamp};
use keith_platform::replace_file;
use keith_supervisor::{
    ImageInstallRequest, ImageRegistryError, InstalledImage, WorkerImageRegistry,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{
    ChangeClass, ConsentAuthority, ConsentPolicy, EvolutionGuard, GuardError, ImageError,
    WorkerImage,
};

const STATE_VERSION: u32 = 1;
const STATE_FILE: &str = "staging.json";
const NOTICE_FILE: &str = "restoration-notice.json";
const LOCK_FILE: &str = "staging.lock";
const MAX_STATE_BYTES: u64 = 1024 * 1024;
const MAX_DISCLOSURE_ITEMS: usize = 32;
const MAX_DISCLOSURE_BYTES: usize = 2 * 1024;
const MAX_REASON_BYTES: usize = 2 * 1024;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DaemonStagingPhase {
    Staged,
    Launching,
    Active,
    Restored,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DaemonRestartConsent {
    pub owner_identity: String,
    pub restart_required: bool,
    pub affected_scope: Vec<String>,
    pub reversal_path: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct StagedDaemonImage {
    pub transaction_id: EntityId,
    pub phase: DaemonStagingPhase,
    pub candidate: InstalledImage,
    pub pinned: InstalledImage,
    pub consent: DaemonRestartConsent,
    pub staged_at: UtcTimestamp,
    pub launch_attempts: u32,
    pub last_failure: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DaemonLaunchSelection {
    pub image: InstalledImage,
    pub candidate: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DaemonRestorationNotice {
    pub notice_id: EntityId,
    pub transaction_id: EntityId,
    pub failed_image_id: String,
    pub restored_image_id: String,
    pub reason: String,
    pub occurred_at: UtcTimestamp,
}

pub struct StagingRequest<'a> {
    pub image: &'a WorkerImage,
    pub trusted_public_key: &'a [u8; 32],
    pub consent: &'a DaemonRestartConsent,
    pub guard: &'a EvolutionGuard,
}

#[derive(Debug, Error)]
pub enum StagingError {
    #[error("daemon staging I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("daemon staging state is invalid: {0}")]
    State(#[from] serde_json::Error),
    #[error("daemon image registry failed: {0}")]
    Registry(#[from] ImageRegistryError),
    #[error("daemon candidate image failed authentication: {0}")]
    Image(#[from] ImageError),
    #[error("daemon staging guard rejected the request: {0}")]
    Guard(#[from] GuardError),
    #[error("daemon replacement requires an explicit Class C owner consent disclosure")]
    ConsentRequired,
    #[error("daemon image is not Class C daemon-resident code")]
    NotDaemonImage,
    #[error("another daemon staging transaction owns this installation")]
    Locked,
    #[error("a daemon replacement is already staged or launching")]
    AlreadyPending,
    #[error("no staged daemon replacement exists")]
    NotStaged,
    #[error("daemon launch identity does not match the durable transaction")]
    IdentityMismatch,
    #[error("daemon restoration reason is unsafe or unbounded")]
    UnsafeReason,
    #[error("daemon staging state exceeds its storage bound")]
    StateTooLarge,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct DurableState {
    version: u32,
    trusted_public_key: [u8; 32],
    staged: StagedDaemonImage,
}

/// Durable authority for staging and recovering one daemon replacement at a time.
pub struct DaemonStaging {
    root: PathBuf,
    state_path: PathBuf,
    registry: WorkerImageRegistry,
    _lock: File,
}

impl DaemonStaging {
    /// Opens the installation-owned daemon image registry and reconciles an interrupted launch.
    ///
    /// # Errors
    /// Returns an error for an unsafe bootstrap, corrupt durable state, or competing launcher.
    pub fn open(
        root: impl Into<PathBuf>,
        bootstrap_executable: impl AsRef<Path>,
    ) -> Result<Self, StagingError> {
        let root = root.into();
        fs::create_dir_all(&root)?;
        reject_symlink(&root)?;
        let root = fs::canonicalize(root)?;
        let lock = OpenOptions::new()
            .create(true)
            .read(true)
            .write(true)
            .truncate(false)
            .open(root.join(LOCK_FILE))?;
        lock.try_lock_exclusive()
            .map_err(|_| StagingError::Locked)?;
        let registry =
            WorkerImageRegistry::open(root.join("registry"), bootstrap_executable.as_ref())?;
        let staging = Self {
            state_path: root.join(STATE_FILE),
            root,
            registry,
            _lock: lock,
        };
        if staging.state_path.exists() {
            let state = staging.load()?;
            staging.validate_state(&state)?;
        }
        Ok(staging)
    }

    /// Authenticates and pins a Class C daemon candidate without changing the running image.
    ///
    /// # Errors
    /// Fails before publication when signature, class, consent, or disclosure is invalid.
    pub fn stage(
        &mut self,
        request: StagingRequest<'_>,
    ) -> Result<StagedDaemonImage, StagingError> {
        if self.state_path.exists()
            && matches!(
                self.load()?.staged.phase,
                DaemonStagingPhase::Staged | DaemonStagingPhase::Launching
            )
        {
            return Err(StagingError::AlreadyPending);
        }
        validate_consent(request.guard, request.consent)?;
        let manifest = request.image.manifest();
        let daemon_resident = manifest
            .artifact_source_paths
            .iter()
            .any(|path| path.starts_with("crates/daemon-core") || path.starts_with("apps/agentd"));
        if manifest.change_class != "c"
            || manifest.worker_report.component != "daemon"
            || !daemon_resident
        {
            return Err(StagingError::NotDaemonImage);
        }
        request.image.verify(request.trusted_public_key)?;
        let pinned = self.registry.resolve_current()?;
        let manifest_bytes = request.image.manifest_bytes()?;
        let candidate = self.registry.install_verified(&ImageInstallRequest {
            manifest: &manifest_bytes,
            signature: request.image.signature(),
            executable: request.image.executable(),
            trusted_public_key: request.trusted_public_key,
        })?;
        if candidate.image_id == pinned.image_id {
            return Err(StagingError::AlreadyPending);
        }
        let staged = StagedDaemonImage {
            transaction_id: EntityId::new(),
            phase: DaemonStagingPhase::Staged,
            candidate,
            pinned,
            consent: request.consent.clone(),
            staged_at: UtcTimestamp::now()
                .map_err(|error| StagingError::Io(std::io::Error::other(error)))?,
            launch_attempts: 0,
            last_failure: None,
        };
        self.persist(&DurableState {
            version: STATE_VERSION,
            trusted_public_key: *request.trusted_public_key,
            staged: staged.clone(),
        })?;
        Ok(staged)
    }

    /// Selects the exact candidate for one supervised launch attempt.
    ///
    /// An interrupted prior attempt is restored to the pinned image before returning.
    pub fn launch_selection(&mut self) -> Result<DaemonLaunchSelection, StagingError> {
        if !self.state_path.exists() {
            return Ok(DaemonLaunchSelection {
                image: self.registry.resolve_current()?,
                candidate: false,
            });
        }
        let mut state = self.load()?;
        self.validate_state(&state)?;
        match state.staged.phase {
            DaemonStagingPhase::Staged => {
                let image = self.registry.resolve(&state.staged.candidate.image_id)?;
                state.staged.phase = DaemonStagingPhase::Launching;
                state.staged.launch_attempts = state.staged.launch_attempts.saturating_add(1);
                self.persist(&state)?;
                Ok(DaemonLaunchSelection {
                    image,
                    candidate: true,
                })
            }
            DaemonStagingPhase::Launching => {
                let reason = "the prior supervised launch ended before readiness";
                let restored = self.restore(&mut state, reason)?;
                Ok(DaemonLaunchSelection {
                    image: restored,
                    candidate: false,
                })
            }
            DaemonStagingPhase::Active => Ok(DaemonLaunchSelection {
                image: self.registry.resolve_current()?,
                candidate: true,
            }),
            DaemonStagingPhase::Restored => Ok(DaemonLaunchSelection {
                image: self.registry.resolve(&state.staged.pinned.image_id)?,
                candidate: false,
            }),
        }
    }

    /// Commits a candidate pointer only after the child positively reports endpoint readiness.
    pub fn mark_ready(&mut self, image_id: &str) -> Result<InstalledImage, StagingError> {
        let mut state = self.load()?;
        if !matches!(
            state.staged.phase,
            DaemonStagingPhase::Launching | DaemonStagingPhase::Active
        ) || state.staged.candidate.image_id != image_id
        {
            return Err(StagingError::IdentityMismatch);
        }
        let active = self
            .registry
            .promote_verified(image_id, &state.trusted_public_key)?;
        state.staged.phase = DaemonStagingPhase::Active;
        state.staged.last_failure = None;
        self.persist(&state)?;
        Ok(active)
    }

    /// Restores the pinned image and records a durable next-attach notice.
    pub fn fail_and_restore(
        &mut self,
        image_id: &str,
        reason: &str,
    ) -> Result<InstalledImage, StagingError> {
        let mut state = self.load()?;
        if !matches!(
            state.staged.phase,
            DaemonStagingPhase::Launching | DaemonStagingPhase::Active
        ) || state.staged.candidate.image_id != image_id
        {
            return Err(StagingError::IdentityMismatch);
        }
        self.restore(&mut state, reason)
    }

    #[must_use]
    pub fn staged(&self) -> Result<Option<StagedDaemonImage>, StagingError> {
        if !self.state_path.exists() {
            return Ok(None);
        }
        Ok(Some(self.load()?.staged))
    }

    pub fn pending_restoration_notice(
        &self,
    ) -> Result<Option<DaemonRestorationNotice>, StagingError> {
        read_pending_restoration_notice(&self.root)
    }

    fn restore(
        &mut self,
        state: &mut DurableState,
        reason: &str,
    ) -> Result<InstalledImage, StagingError> {
        let reason = safe_text(reason, MAX_REASON_BYTES).ok_or(StagingError::UnsafeReason)?;
        let restored = self
            .registry
            .restore_current(&state.staged.pinned.image_id)?;
        state.staged.phase = DaemonStagingPhase::Restored;
        state.staged.last_failure = Some(reason.clone());
        self.persist(state)?;
        let notice = DaemonRestorationNotice {
            notice_id: EntityId::new(),
            transaction_id: state.staged.transaction_id.clone(),
            failed_image_id: state.staged.candidate.image_id.clone(),
            restored_image_id: restored.image_id.clone(),
            reason,
            occurred_at: UtcTimestamp::now()
                .map_err(|error| StagingError::Io(std::io::Error::other(error)))?,
        };
        write_json_atomic(&self.root, NOTICE_FILE, &notice)?;
        Ok(restored)
    }

    fn load(&self) -> Result<DurableState, StagingError> {
        let metadata = fs::symlink_metadata(&self.state_path)?;
        if !metadata.is_file() || metadata.file_type().is_symlink() {
            return Err(StagingError::StateTooLarge);
        }
        if metadata.len() > MAX_STATE_BYTES {
            return Err(StagingError::StateTooLarge);
        }
        Ok(serde_json::from_slice(&fs::read(&self.state_path)?)?)
    }

    fn validate_state(&self, state: &DurableState) -> Result<(), StagingError> {
        if state.version != STATE_VERSION
            || state.staged.candidate.image_id == state.staged.pinned.image_id
            || !state.staged.consent.restart_required
        {
            return Err(StagingError::NotStaged);
        }
        self.registry.resolve(&state.staged.candidate.image_id)?;
        self.registry.resolve(&state.staged.pinned.image_id)?;
        Ok(())
    }

    fn persist(&self, state: &DurableState) -> Result<(), StagingError> {
        let bytes = serde_json::to_vec(state)?;
        if u64::try_from(bytes.len()).unwrap_or(u64::MAX) > MAX_STATE_BYTES {
            return Err(StagingError::StateTooLarge);
        }
        write_atomic(&self.root, STATE_FILE, &bytes)?;
        Ok(())
    }
}

pub fn read_pending_restoration_notice(
    root: impl AsRef<Path>,
) -> Result<Option<DaemonRestorationNotice>, StagingError> {
    let path = root.as_ref().join(NOTICE_FILE);
    let metadata = match fs::symlink_metadata(&path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(None),
        Err(error) => return Err(error.into()),
    };
    if !metadata.is_file() || metadata.file_type().is_symlink() || metadata.len() > MAX_STATE_BYTES
    {
        return Err(StagingError::StateTooLarge);
    }
    let notice: DaemonRestorationNotice = serde_json::from_slice(&fs::read(path)?)?;
    if safe_text(&notice.reason, MAX_REASON_BYTES).as_deref() != Some(notice.reason.as_str()) {
        return Err(StagingError::UnsafeReason);
    }
    Ok(Some(notice))
}

pub fn acknowledge_restoration_notice(
    root: impl AsRef<Path>,
    notice_id: &EntityId,
) -> Result<bool, StagingError> {
    let root = root.as_ref();
    let Some(notice) = read_pending_restoration_notice(root)? else {
        return Ok(false);
    };
    if &notice.notice_id != notice_id {
        return Ok(false);
    }
    fs::remove_file(root.join(NOTICE_FILE))?;
    sync_directory(root)?;
    Ok(true)
}

fn validate_consent(
    guard: &EvolutionGuard,
    consent: &DaemonRestartConsent,
) -> Result<(), StagingError> {
    if !consent.restart_required
        || consent.affected_scope.is_empty()
        || consent.affected_scope.len() > MAX_DISCLOSURE_ITEMS
        || consent
            .affected_scope
            .iter()
            .any(|item| safe_text(item, MAX_DISCLOSURE_BYTES).as_deref() != Some(item.as_str()))
        || safe_text(&consent.reversal_path, MAX_DISCLOSURE_BYTES).as_deref()
            != Some(consent.reversal_path.as_str())
    {
        return Err(StagingError::ConsentRequired);
    }
    let authority = ConsentAuthority::InstallationOwner {
        identity: consent.owner_identity.clone(),
    };
    if guard.validate_consent(ChangeClass::C, &authority)? != ConsentPolicy::HumanApproval {
        return Err(StagingError::ConsentRequired);
    }
    Ok(())
}

fn safe_text(value: &str, maximum: usize) -> Option<String> {
    let value = value.trim();
    (!value.is_empty()
        && value.len() <= maximum
        && value.chars().all(|character| !character.is_control()))
    .then(|| value.to_owned())
}

fn reject_symlink(path: &Path) -> Result<(), StagingError> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err(StagingError::StateTooLarge);
    }
    Ok(())
}

fn write_json_atomic<T: Serialize>(
    root: &Path,
    file_name: &str,
    value: &T,
) -> Result<(), StagingError> {
    write_atomic(root, file_name, &serde_json::to_vec(value)?)?;
    Ok(())
}

fn write_atomic(root: &Path, file_name: &str, bytes: &[u8]) -> Result<(), std::io::Error> {
    let temporary = root.join(format!(".{file_name}.{}.tmp", std::process::id()));
    match fs::remove_file(&temporary) {
        Ok(()) => {}
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => return Err(error),
    }
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&temporary)?;
    file.write_all(bytes)?;
    file.sync_all()?;
    replace_file(&temporary, &root.join(file_name))?;
    sync_directory(root)
}

fn sync_directory(path: &Path) -> Result<(), std::io::Error> {
    File::open(path)?.sync_all()
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use keith_agent_types::canonical_json_bytes;
    use keith_release::BuildReport;
    use ring::signature::{Ed25519KeyPair, KeyPair};
    use sha2::{Digest, Sha256};
    use tempfile::tempdir;

    use crate::{GateKind, GateResult, ToolchainIdentity, WorkerImageManifest};

    use super::*;

    fn candidate(executable: &[u8]) -> (WorkerImage, [u8; 32]) {
        let output = "real verification gate exited successfully";
        let gates = [
            GateKind::Formatting,
            GateKind::StrictClippy,
            GateKind::WorkspaceTests,
            GateKind::DependencyPolicy,
            GateKind::Security,
            GateKind::Platform,
        ]
        .into_iter()
        .map(|gate| GateResult {
            gate,
            exit_code: 0,
            elapsed_millis: 1,
            output: output.into(),
            output_sha256: digest(output.as_bytes()),
            sandbox: keith_sandbox::SandboxStatus::detect(),
        })
        .collect();
        let manifest = WorkerImageManifest {
            format: "keith-worker-image-v1".into(),
            build_id: "daemon-staging-process-build".into(),
            base_revision: "a".repeat(40),
            source_manifest_sha256: "b".repeat(64),
            executable_sha256: digest(executable),
            executable_bytes: u64::try_from(executable.len()).unwrap(),
            toolchain: ToolchainIdentity {
                rustc: "rustc 1.93.0".into(),
                cargo: "cargo 1.93.0".into(),
                target: "test-target".into(),
            },
            worker_report: BuildReport {
                component: "daemon".into(),
                package_version: "0.1.0".into(),
                build_id: "daemon-staging-process-build".into(),
                protocol_version: "1".into(),
                storage_schema: "1".into(),
                enabled_features: BTreeSet::from(["runtime".into()]),
            },
            gates,
            artifact_source_paths: vec![PathBuf::from("apps/agentd/src/main.rs")],
            change_class: "c".into(),
        };
        let key = Ed25519KeyPair::from_seed_unchecked(&[41; 32]).unwrap();
        let public_key: [u8; 32] = key.public_key().as_ref().try_into().unwrap();
        let signature = key.sign(&canonical_json_bytes(&manifest).unwrap());
        let image = WorkerImage::from_signed_parts(
            manifest,
            executable.to_vec(),
            signature.as_ref().to_vec(),
            public_key,
            &public_key,
        )
        .unwrap();
        (image, public_key)
    }

    fn consent() -> DaemonRestartConsent {
        DaemonRestartConsent {
            owner_identity: "installation-owner".into(),
            restart_required: true,
            affected_scope: vec!["Keith daemon and local endpoint".into()],
            reversal_path: "Restore the pinned known-good daemon image".into(),
        }
    }

    fn digest(bytes: &[u8]) -> String {
        format!("{:x}", Sha256::digest(bytes))
    }

    #[test]
    fn signed_candidate_is_staged_activated_restored_and_remains_inspectable() {
        let directory = tempdir().unwrap();
        let live = tempdir().unwrap();
        let bootstrap = std::env::current_exe().unwrap();
        let bytes = fs::read(&bootstrap).unwrap();
        let (image, public_key) = candidate(&bytes);
        let guard = EvolutionGuard::new(live.path()).unwrap();
        let mut staging = DaemonStaging::open(directory.path(), &bootstrap).unwrap();
        let staged = staging
            .stage(StagingRequest {
                image: &image,
                trusted_public_key: &public_key,
                consent: &consent(),
                guard: &guard,
            })
            .unwrap();
        assert_eq!(staged.phase, DaemonStagingPhase::Staged);
        assert_ne!(staged.candidate.image_id, staged.pinned.image_id);

        let selected = staging.launch_selection().unwrap();
        assert!(selected.candidate);
        assert_eq!(selected.image.image_id, staged.candidate.image_id);
        staging.mark_ready(&selected.image.image_id).unwrap();
        drop(staging);

        let mut restarted = DaemonStaging::open(directory.path(), &bootstrap).unwrap();
        let active = restarted.launch_selection().unwrap();
        assert!(active.candidate);
        restarted
            .fail_and_restore(&active.image.image_id, "candidate exited after readiness")
            .unwrap();
        let notice = restarted.pending_restoration_notice().unwrap().unwrap();
        assert_eq!(notice.failed_image_id, staged.candidate.image_id);
        assert_eq!(notice.restored_image_id, staged.pinned.image_id);
        assert!(
            restarted
                .staged()
                .unwrap()
                .unwrap()
                .candidate
                .executable
                .is_file()
        );
        assert!(acknowledge_restoration_notice(directory.path(), &notice.notice_id).unwrap());
        assert!(restarted.pending_restoration_notice().unwrap().is_none());
    }

    #[test]
    fn interrupted_launch_restores_once_and_repeated_restarts_are_stable() {
        let directory = tempdir().unwrap();
        let live = tempdir().unwrap();
        let bootstrap = std::env::current_exe().unwrap();
        let bytes = fs::read(&bootstrap).unwrap();
        let (image, public_key) = candidate(&bytes);
        let guard = EvolutionGuard::new(live.path()).unwrap();
        let mut staging = DaemonStaging::open(directory.path(), &bootstrap).unwrap();
        let staged = staging
            .stage(StagingRequest {
                image: &image,
                trusted_public_key: &public_key,
                consent: &consent(),
                guard: &guard,
            })
            .unwrap();
        assert!(staging.launch_selection().unwrap().candidate);
        drop(staging);

        for attempt in 0..3 {
            let mut restarted = DaemonStaging::open(directory.path(), &bootstrap).unwrap();
            let selected = restarted.launch_selection().unwrap();
            assert!(!selected.candidate, "restart {attempt}");
            assert_eq!(selected.image.image_id, staged.pinned.image_id);
        }
    }

    #[test]
    fn daemon_stage_refuses_incomplete_owner_disclosure() {
        let directory = tempdir().unwrap();
        let live = tempdir().unwrap();
        let bootstrap = std::env::current_exe().unwrap();
        let bytes = fs::read(&bootstrap).unwrap();
        let (image, public_key) = candidate(&bytes);
        let guard = EvolutionGuard::new(live.path()).unwrap();
        let mut staging = DaemonStaging::open(directory.path(), &bootstrap).unwrap();
        let mut disclosure = consent();
        disclosure.restart_required = false;
        assert!(matches!(
            staging.stage(StagingRequest {
                image: &image,
                trusted_public_key: &public_key,
                consent: &disclosure,
                guard: &guard,
            }),
            Err(StagingError::ConsentRequired)
        ));
    }
}
