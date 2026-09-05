use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Component, Path, PathBuf};

use fs2::FileExt;
use keith_agent_types::{EntityId, Generation, RootTreeId, UtcTimestamp, canonical_json_bytes};
use keith_state_store_core::EvolutionLedgerRepository;
use keith_supervisor::{
    ImageInstallRequest, ImageRegistryError, InstalledImage, SupervisorError, WorkerHealth,
    WorkerImageRegistry, WorkerRollProof, WorkerStatus, WorkerSupervisor,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    ArtifactManifest, CanaryEvaluation, CanaryVerdict, ChangeClass, EvolutionEvent, EvolutionGuard,
    EvolutionLedger, EvolutionProposal, GuardError, ImageError, LedgerError, LedgerText,
    WorkerImage,
};

const JOURNAL_FILE: &str = "promotion.json";
const LOCK_FILE: &str = "promotion.lock";
const HISTORY_DIRECTORY: &str = "promotion-history";
const JOURNAL_VERSION: u32 = 1;
const MAX_JOURNAL_BYTES: u64 = 8 * 1024 * 1024;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PromotionPhase {
    Prepared,
    Installed,
    ImageSelected,
    Rolling,
    WorkersRolled,
    SourceWriting,
    Committed,
    Restoring,
    Restored,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WorkerRoll {
    pub root_tree_id: RootTreeId,
    pub prior_generation: Generation,
    pub prior_image_id: String,
    pub candidate_generation: Option<Generation>,
    pub restored_generation: Option<Generation>,
    pub error: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecordedSourceChange {
    pub path: PathBuf,
    pub prior_bytes: Option<Vec<u8>>,
    pub desired_bytes: Option<Vec<u8>>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PromotionRecord {
    pub version: u32,
    pub sequence: u64,
    pub transaction_id: EntityId,
    pub hypothesis_id: EntityId,
    pub prior_image_id: String,
    pub candidate_image_id: String,
    pub rolls: Vec<WorkerRoll>,
    pub source_changes: Vec<RecordedSourceChange>,
    pub created_directories: Vec<PathBuf>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct PromotionJournal {
    version: u32,
    sequence: u64,
    transaction_id: EntityId,
    hypothesis_id: EntityId,
    ledger_event_id: EntityId,
    occurred_at: UtcTimestamp,
    phase: PromotionPhase,
    prior_image_id: String,
    candidate_image_id: String,
    failure_threshold: usize,
    failures: usize,
    rolls: Vec<WorkerRoll>,
    source_changes: Vec<RecordedSourceChange>,
    created_directories: Vec<PathBuf>,
    source_applied: usize,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PromotionOutcome {
    pub transaction_id: EntityId,
    pub image: InstalledImage,
    pub rolls: Vec<WorkerRoll>,
}

#[derive(Debug, Error)]
pub enum PromotionError {
    #[error("promotion configuration is invalid: {0}")]
    Invalid(String),
    #[error("candidate image failed authentication: {0}")]
    Image(#[from] ImageError),
    #[error("worker image registry failed: {0}")]
    Registry(#[from] ImageRegistryError),
    #[error("worker roll failed: {0}")]
    Supervisor(#[from] SupervisorError),
    #[error("promotion guard rejected the artifact: {0}")]
    Guard(#[from] GuardError),
    #[error("promotion filesystem failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("promotion journal is invalid: {0}")]
    Journal(#[from] serde_json::Error),
    #[error("promotion ledger append failed: {0}")]
    Ledger(#[from] LedgerError),
    #[error("the canary did not pass for the exact candidate image")]
    CanaryRejected,
    #[error("live source changed after the proposal captured its preimage: {0}")]
    SourceConflict(PathBuf),
    #[error("promotion source path is unsafe: {0}")]
    UnsafeSource(PathBuf),
    #[error("one or more worker roots rejected the candidate and the prior image was restored")]
    RollAborted,
    #[error("an interrupted promotion requires recovery before another can begin")]
    RecoveryRequired,
    #[error("promotion restoration was incomplete: {0}")]
    RestoreIncomplete(String),
}

/// Exact live-worker authority consumed by the durable promotion state machine.
pub trait PromotionRuntime {
    fn registry(&self) -> &WorkerImageRegistry;
    fn registry_mut(&mut self) -> &mut WorkerImageRegistry;
    fn active_workers(&self) -> Vec<WorkerStatus>;
    fn roll_exact(
        &mut self,
        root: &RootTreeId,
        image_id: &str,
    ) -> Result<WorkerRollProof, SupervisorError>;
}

impl PromotionRuntime for WorkerSupervisor {
    fn registry(&self) -> &WorkerImageRegistry {
        self.image_registry()
    }

    fn registry_mut(&mut self) -> &mut WorkerImageRegistry {
        self.image_registry_mut()
    }

    fn active_workers(&self) -> Vec<WorkerStatus> {
        self.statuses()
    }

    fn roll_exact(
        &mut self,
        root: &RootTreeId,
        image_id: &str,
    ) -> Result<WorkerRollProof, SupervisorError> {
        self.roll_to_image(root, image_id)
    }
}

pub struct PromotionRequest<'a> {
    pub hypothesis_id: EntityId,
    pub occurred_at: UtcTimestamp,
    pub image: &'a WorkerImage,
    pub trusted_public_key: &'a [u8; 32],
    pub canary: &'a CanaryEvaluation,
    pub proposal: &'a EvolutionProposal,
    pub shadow_root: &'a Path,
    pub failure_threshold: usize,
}

/// Durable authority for one live worker promotion at a time.
pub struct PromotionTransaction {
    root: PathBuf,
    live_root: PathBuf,
    journal_path: PathBuf,
    _lock: File,
}

impl PromotionTransaction {
    /// Opens the owned transaction journal and locks out competing promotions.
    pub fn open(
        root: impl Into<PathBuf>,
        live_root: impl AsRef<Path>,
    ) -> Result<Self, PromotionError> {
        let root = root.into();
        reject_symlink_components(&root)?;
        fs::create_dir_all(&root)?;
        let root = fs::canonicalize(root)?;
        crate::RevertWatchdog::assert_promotion_allowed(&root)
            .map_err(|error| PromotionError::Invalid(error.to_string()))?;
        let live_root = fs::canonicalize(live_root)?;
        if !live_root.is_dir() {
            return Err(PromotionError::Invalid(
                "live root is not a directory".into(),
            ));
        }
        let lock = OpenOptions::new()
            .create(true)
            .read(true)
            .write(true)
            .truncate(false)
            .open(root.join(LOCK_FILE))?;
        lock.try_lock_exclusive()
            .map_err(|_| PromotionError::Invalid("another promotion owns the journal".into()))?;
        Ok(Self {
            journal_path: root.join(JOURNAL_FILE),
            root,
            live_root,
            _lock: lock,
        })
    }

    /// Installs, rolls, and writes back one verified candidate.
    ///
    /// Live source is not mutated until every active root is healthy on the exact candidate.
    pub fn promote<R, L>(
        &self,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        request: PromotionRequest<'_>,
    ) -> Result<PromotionOutcome, PromotionError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        self.clear_finished_journal()?;
        if request.failure_threshold == 0 {
            return Err(PromotionError::Invalid(
                "failure threshold must be at least one".into(),
            ));
        }
        request.image.verify(request.trusted_public_key)?;
        let manifest_bytes = request.image.manifest_bytes()?;
        let candidate_image_id = sha256(&manifest_bytes);
        if request.canary.image_id != candidate_image_id
            || !matches!(request.canary.verdict, CanaryVerdict::Passed)
        {
            return Err(PromotionError::CanaryRejected);
        }
        let guard = EvolutionGuard::new(&self.live_root)?;
        let artifact = ArtifactManifest {
            source_paths: request.image.manifest().artifact_source_paths.clone(),
            ..ArtifactManifest::default()
        };
        let classification = guard.recompute(&request.proposal.changes, &artifact)?;
        if class_name(classification.proposal.max(classification.artifact))
            != request.image.manifest().change_class
        {
            return Err(PromotionError::Invalid(
                "signed artifact class does not match the live guard".into(),
            ));
        }
        let (source_changes, created_directories) =
            prepare_source_changes(&self.live_root, request.shadow_root, request.proposal)?;
        let prior_image_id = runtime.registry().current().image_id.clone();
        if prior_image_id == candidate_image_id {
            return Err(PromotionError::Invalid(
                "candidate image is already current".into(),
            ));
        }
        let mut journal = PromotionJournal {
            version: JOURNAL_VERSION,
            sequence: self.next_history_sequence()?,
            transaction_id: EntityId::new(),
            hypothesis_id: request.hypothesis_id,
            ledger_event_id: EntityId::new(),
            occurred_at: request.occurred_at,
            phase: PromotionPhase::Prepared,
            prior_image_id,
            candidate_image_id: candidate_image_id.clone(),
            failure_threshold: request.failure_threshold,
            failures: 0,
            rolls: runtime
                .active_workers()
                .into_iter()
                .map(|status| WorkerRoll {
                    root_tree_id: status.root_tree_id,
                    prior_generation: status.generation,
                    prior_image_id: status.image_id,
                    candidate_generation: None,
                    restored_generation: None,
                    error: None,
                })
                .collect(),
            source_changes,
            created_directories,
            source_applied: 0,
        };
        self.persist(&journal)?;
        crash_boundary("prepared");
        let installed = runtime
            .registry_mut()
            .install_verified(&ImageInstallRequest {
                manifest: &manifest_bytes,
                signature: request.image.signature(),
                executable: request.image.executable(),
                trusted_public_key: request.trusted_public_key,
            })?;
        if installed.image_id != candidate_image_id {
            return self.fail_and_restore(
                runtime,
                &mut journal,
                PromotionError::Invalid("installed candidate identity changed".into()),
            );
        }
        journal.phase = PromotionPhase::Installed;
        self.persist(&journal)?;
        crash_boundary("installed");
        runtime
            .registry_mut()
            .promote_verified(&candidate_image_id, request.trusted_public_key)?;
        journal.phase = PromotionPhase::ImageSelected;
        self.persist(&journal)?;
        crash_boundary("image_selected");

        journal.phase = PromotionPhase::Rolling;
        self.persist(&journal)?;
        crash_boundary("rolling");
        for index in 0..journal.rolls.len() {
            let root = journal.rolls[index].root_tree_id.clone();
            match runtime.roll_exact(&root, &candidate_image_id) {
                Ok(proof)
                    if proof.image_id == candidate_image_id
                        && proof.generation > proof.previous_generation
                        && proof.health == WorkerHealth::Healthy =>
                {
                    journal.rolls[index].candidate_generation = Some(proof.generation);
                }
                Ok(_) => {
                    journal.failures = journal.failures.saturating_add(1);
                    journal.rolls[index].error = Some("candidate roll proof was invalid".into());
                }
                Err(error) => {
                    journal.failures = journal.failures.saturating_add(1);
                    journal.rolls[index].error = Some(error.to_string());
                    journal.rolls[index].restored_generation = runtime
                        .active_workers()
                        .into_iter()
                        .find(|status| {
                            status.root_tree_id == root
                                && status.image_id == journal.rolls[index].prior_image_id
                        })
                        .map(|status| status.generation);
                }
            }
            self.persist(&journal)?;
            crash_boundary("rolled_root");
            if journal.failures >= journal.failure_threshold {
                return self.fail_and_restore(runtime, &mut journal, PromotionError::RollAborted);
            }
        }
        if journal.failures != 0 {
            return self.fail_and_restore(runtime, &mut journal, PromotionError::RollAborted);
        }
        journal.phase = PromotionPhase::WorkersRolled;
        self.persist(&journal)?;
        crash_boundary("workers_rolled");
        journal.phase = PromotionPhase::SourceWriting;
        self.persist(&journal)?;
        crash_boundary("source_writing");
        for index in 0..journal.source_changes.len() {
            let change = journal.source_changes[index].clone();
            if let Err(error) = apply_source_change(
                &self.live_root,
                &journal.transaction_id,
                index,
                &change,
                true,
            ) {
                return self.fail_and_restore(runtime, &mut journal, error);
            }
            journal.source_applied = index.saturating_add(1);
            self.persist(&journal)?;
            crash_boundary(&format!("source_{index}"));
        }
        let archive_digest = match self.archive(&journal) {
            Ok(digest) => digest,
            Err(error) => return self.fail_and_restore(runtime, &mut journal, error),
        };
        if let Err(error) = ledger.append(
            journal.ledger_event_id.clone(),
            journal.occurred_at,
            EvolutionEvent::Promotion {
                hypothesis_id: journal.hypothesis_id.clone(),
                promotion_id: journal.transaction_id.clone(),
                artifact_id: LedgerText::redacted(&journal.candidate_image_id, 256, &[])?,
                artifact_digest: LedgerText::redacted(&archive_digest, 64, &[])?,
            },
        ) {
            return self.fail_and_restore(runtime, &mut journal, error.into());
        }
        journal.phase = PromotionPhase::Committed;
        self.persist(&journal)?;
        crash_boundary("committed");
        Ok(PromotionOutcome {
            transaction_id: journal.transaction_id,
            image: installed,
            rolls: journal.rolls,
        })
    }

    /// Restores an interrupted transaction to its pinned image and exact source preimages.
    pub fn recover<R, L>(
        &self,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
    ) -> Result<bool, PromotionError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        if !self.journal_path.exists() {
            return Ok(false);
        }
        let mut journal = self.load()?;
        if journal.phase == PromotionPhase::Committed {
            self.ensure_signed_archive(ledger, &journal)?;
            return Ok(false);
        }
        if self.signed_archive_digest(ledger, &journal)?.is_some() {
            self.ensure_signed_archive(ledger, &journal)?;
            journal.phase = PromotionPhase::Committed;
            self.persist(&journal)?;
            return Ok(false);
        }
        if journal.phase == PromotionPhase::Restored {
            return Ok(false);
        }
        self.restore(runtime, &mut journal)?;
        Ok(true)
    }

    fn clear_finished_journal(&self) -> Result<(), PromotionError> {
        if !self.journal_path.exists() {
            return Ok(());
        }
        let journal = self.load()?;
        if !matches!(
            journal.phase,
            PromotionPhase::Committed | PromotionPhase::Restored
        ) {
            return Err(PromotionError::RecoveryRequired);
        }
        fs::remove_file(&self.journal_path)?;
        sync_directory(&self.root)?;
        Ok(())
    }

    fn fail_and_restore<R: PromotionRuntime, T>(
        &self,
        runtime: &mut R,
        journal: &mut PromotionJournal,
        original: PromotionError,
    ) -> Result<T, PromotionError> {
        self.restore(runtime, journal)?;
        Err(original)
    }

    fn restore<R: PromotionRuntime>(
        &self,
        runtime: &mut R,
        journal: &mut PromotionJournal,
    ) -> Result<(), PromotionError> {
        journal.phase = PromotionPhase::Restoring;
        self.persist(journal)?;
        runtime
            .registry_mut()
            .restore_current(&journal.prior_image_id)?;
        let mut failures = Vec::new();
        for index in 0..journal.rolls.len() {
            let roll = journal.rolls[index].clone();
            let current = runtime
                .active_workers()
                .into_iter()
                .find(|status| status.root_tree_id == roll.root_tree_id);
            if current
                .as_ref()
                .is_some_and(|status| status.image_id == roll.prior_image_id)
            {
                journal.rolls[index].restored_generation = current.map(|status| status.generation);
                continue;
            }
            match runtime.roll_exact(&roll.root_tree_id, &roll.prior_image_id) {
                Ok(proof)
                    if proof.image_id == roll.prior_image_id
                        && proof.health == WorkerHealth::Healthy =>
                {
                    journal.rolls[index].restored_generation = Some(proof.generation);
                }
                Ok(_) => failures.push(format!(
                    "{} returned invalid restore proof",
                    roll.root_tree_id
                )),
                Err(error) => failures.push(format!("{}: {error}", roll.root_tree_id)),
            }
            self.persist(journal)?;
        }
        for (index, change) in journal.source_changes.iter().enumerate().rev() {
            let inverse = RecordedSourceChange {
                path: change.path.clone(),
                prior_bytes: change.desired_bytes.clone(),
                desired_bytes: change.prior_bytes.clone(),
            };
            if let Err(error) = apply_source_change(
                &self.live_root,
                &journal.transaction_id,
                index,
                &inverse,
                false,
            ) {
                failures.push(error.to_string());
            }
        }
        for directory in journal.created_directories.iter().rev() {
            let path = resolve_safe(&self.live_root, directory)?;
            match fs::remove_dir(&path) {
                Ok(()) => sync_directory(path.parent().unwrap_or(&self.live_root))?,
                Err(error)
                    if matches!(
                        error.kind(),
                        std::io::ErrorKind::NotFound | std::io::ErrorKind::DirectoryNotEmpty
                    ) => {}
                Err(error) => failures.push(error.to_string()),
            }
        }
        if !failures.is_empty() {
            self.persist(journal)?;
            return Err(PromotionError::RestoreIncomplete(failures.join("; ")));
        }
        journal.phase = PromotionPhase::Restored;
        journal.source_applied = 0;
        self.persist(journal)?;
        self.remove_archive(journal)?;
        Ok(())
    }

    fn load(&self) -> Result<PromotionJournal, PromotionError> {
        let metadata = fs::symlink_metadata(&self.journal_path)?;
        if !metadata.is_file()
            || metadata.file_type().is_symlink()
            || metadata.len() > MAX_JOURNAL_BYTES
        {
            return Err(PromotionError::Invalid(
                "promotion journal is unsafe or oversized".into(),
            ));
        }
        let journal: PromotionJournal = serde_json::from_slice(&fs::read(&self.journal_path)?)?;
        if journal.version != JOURNAL_VERSION
            || journal.sequence == 0
            || journal.prior_image_id.is_empty()
            || journal.candidate_image_id.is_empty()
            || journal.failure_threshold == 0
            || journal.source_applied > journal.source_changes.len()
        {
            return Err(PromotionError::Invalid(
                "promotion journal fields are invalid".into(),
            ));
        }
        Ok(journal)
    }

    /// Returns immutable committed promotions in their installation order.
    pub fn history(&self) -> Result<Vec<PromotionRecord>, PromotionError> {
        load_promotion_history(&self.root)
    }

    fn next_history_sequence(&self) -> Result<u64, PromotionError> {
        load_promotion_history(&self.root)?
            .last()
            .map_or(Ok(1), |record| {
                record.sequence.checked_add(1).ok_or_else(|| {
                    PromotionError::Invalid("promotion history sequence overflow".into())
                })
            })
    }

    fn archive(&self, journal: &PromotionJournal) -> Result<String, PromotionError> {
        let history = self.root.join(HISTORY_DIRECTORY);
        fs::create_dir_all(&history)?;
        reject_symlink_components(&history)?;
        let record = PromotionRecord {
            version: JOURNAL_VERSION,
            sequence: journal.sequence,
            transaction_id: journal.transaction_id.clone(),
            hypothesis_id: journal.hypothesis_id.clone(),
            prior_image_id: journal.prior_image_id.clone(),
            candidate_image_id: journal.candidate_image_id.clone(),
            rolls: journal.rolls.clone(),
            source_changes: journal.source_changes.clone(),
            created_directories: journal.created_directories.clone(),
        };
        let destination = history.join(format!(
            "{:020}-{}.json",
            record.sequence, record.transaction_id
        ));
        if destination.exists() {
            let bytes = fs::read(&destination)?;
            let existing: PromotionRecord = serde_json::from_slice(&bytes)?;
            if existing == record && bytes == canonical_json_bytes(&existing)? {
                return promotion_record_digest(&record);
            }
            return Err(PromotionError::Invalid(
                "promotion history identity was reused with different content".into(),
            ));
        }
        write_new_synced(&destination, &canonical_json_bytes(&record)?)?;
        sync_directory(&history)?;
        promotion_record_digest(&record)
    }

    fn remove_archive(&self, journal: &PromotionJournal) -> Result<(), PromotionError> {
        let path = self.root.join(HISTORY_DIRECTORY).join(format!(
            "{:020}-{}.json",
            journal.sequence, journal.transaction_id
        ));
        remove_if_present(&path)?;
        if let Some(parent) = path.parent()
            && parent.exists()
        {
            sync_directory(parent)?;
        }
        Ok(())
    }

    fn ensure_signed_archive<L: EvolutionLedgerRepository>(
        &self,
        ledger: &EvolutionLedger<L>,
        journal: &PromotionJournal,
    ) -> Result<(), PromotionError> {
        let digest = self.archive(journal)?;
        match self.signed_archive_digest(ledger, journal)? {
            Some(bound) if bound == digest => Ok(()),
            Some(_) => Err(PromotionError::Invalid(
                "signed promotion archive digest does not match durable bytes".into(),
            )),
            None => Err(PromotionError::Invalid(
                "committed promotion has no signed ledger binding".into(),
            )),
        }
    }

    fn signed_archive_digest<L: EvolutionLedgerRepository>(
        &self,
        ledger: &EvolutionLedger<L>,
        journal: &PromotionJournal,
    ) -> Result<Option<String>, PromotionError> {
        Ok(ledger
            .records()?
            .into_iter()
            .find_map(|record| match record.event {
                EvolutionEvent::Promotion {
                    hypothesis_id,
                    promotion_id,
                    artifact_id,
                    artifact_digest,
                } if hypothesis_id == journal.hypothesis_id
                    && promotion_id == journal.transaction_id
                    && artifact_id.as_str() == journal.candidate_image_id =>
                {
                    Some(artifact_digest.as_str().to_owned())
                }
                _ => None,
            }))
    }

    fn persist(&self, journal: &PromotionJournal) -> Result<(), PromotionError> {
        let bytes = serde_json::to_vec(journal)?;
        if u64::try_from(bytes.len()).unwrap_or(u64::MAX) > MAX_JOURNAL_BYTES {
            return Err(PromotionError::Invalid(
                "promotion journal exceeds its bound".into(),
            ));
        }
        let temporary = self.root.join(format!(".{JOURNAL_FILE}.tmp"));
        remove_if_present(&temporary)?;
        write_new_synced(&temporary, &bytes)?;
        keith_platform::replace_file(&temporary, &self.journal_path)?;
        sync_directory(&self.root)?;
        Ok(())
    }
}

fn prepare_source_changes(
    live_root: &Path,
    shadow_root: &Path,
    proposal: &EvolutionProposal,
) -> Result<(Vec<RecordedSourceChange>, Vec<PathBuf>), PromotionError> {
    let shadow_root = fs::canonicalize(shadow_root)?;
    let mut paths = BTreeSet::new();
    for change in &proposal.changes {
        match change {
            crate::ChangedPath::Write(path) | crate::ChangedPath::Delete(path) => {
                paths.insert(path.clone());
            }
            crate::ChangedPath::Rename { from, to } => {
                paths.insert(from.clone());
                paths.insert(to.clone());
            }
        }
    }
    let preimages = proposal
        .preimages
        .iter()
        .map(|preimage| (preimage.path.clone(), preimage.prior_bytes.clone()))
        .collect::<BTreeMap<_, _>>();
    if paths.len() != preimages.len() || paths.iter().any(|path| !preimages.contains_key(path)) {
        return Err(PromotionError::Invalid(
            "proposal preimages do not exactly cover changed paths".into(),
        ));
    }
    let mut changes = Vec::with_capacity(paths.len());
    let mut created = BTreeSet::new();
    for path in paths {
        let live = resolve_safe(live_root, &path)?;
        let prior_bytes = read_optional_regular(&live)?;
        let expected = preimages.get(&path).cloned().flatten();
        if prior_bytes != expected {
            return Err(PromotionError::SourceConflict(path));
        }
        let shadow = resolve_safe(&shadow_root, &path)?;
        let desired_bytes = read_optional_regular(&shadow)?;
        if desired_bytes.is_some() {
            let mut parent = path.parent();
            while let Some(relative) = parent {
                if relative.as_os_str().is_empty() {
                    break;
                }
                let candidate = resolve_safe(live_root, relative)?;
                if candidate.exists() {
                    break;
                }
                created.insert(relative.to_path_buf());
                parent = relative.parent();
            }
        }
        changes.push(RecordedSourceChange {
            path,
            prior_bytes,
            desired_bytes,
        });
    }
    let mut created_directories = created.into_iter().collect::<Vec<_>>();
    created_directories.sort_by_key(|path| path.components().count());
    Ok((changes, created_directories))
}

pub(crate) fn load_promotion_history(root: &Path) -> Result<Vec<PromotionRecord>, PromotionError> {
    let history = root.join(HISTORY_DIRECTORY);
    let entries = match fs::read_dir(&history) {
        Ok(entries) => entries,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
        Err(error) => return Err(error.into()),
    };
    reject_symlink_components(&history)?;
    let mut records = Vec::new();
    for entry in entries {
        let entry = entry?;
        let metadata = entry.metadata()?;
        if !metadata.is_file()
            || metadata.file_type().is_symlink()
            || metadata.len() > MAX_JOURNAL_BYTES
            || entry.path().extension().and_then(|value| value.to_str()) != Some("json")
        {
            return Err(PromotionError::Invalid(
                "promotion history contains an unsafe entry".into(),
            ));
        }
        let bytes = fs::read(entry.path())?;
        let record: PromotionRecord = serde_json::from_slice(&bytes)?;
        if bytes != canonical_json_bytes(&record)? {
            return Err(PromotionError::Invalid(
                "promotion history bytes are not canonical".into(),
            ));
        }
        validate_promotion_record(&record)?;
        records.push(record);
    }
    records.sort_by_key(|record| record.sequence);
    let mut sequences = BTreeSet::new();
    let mut transactions = BTreeSet::new();
    if records.iter().any(|record| {
        !sequences.insert(record.sequence) || !transactions.insert(record.transaction_id.clone())
    }) {
        return Err(PromotionError::Invalid(
            "promotion history contains duplicate identities".into(),
        ));
    }
    Ok(records)
}

fn validate_promotion_record(record: &PromotionRecord) -> Result<(), PromotionError> {
    if record.version != JOURNAL_VERSION
        || record.sequence == 0
        || record.prior_image_id.is_empty()
        || record.candidate_image_id.is_empty()
        || record.prior_image_id == record.candidate_image_id
    {
        return Err(PromotionError::Invalid(
            "promotion history record fields are invalid".into(),
        ));
    }
    let mut paths = BTreeSet::new();
    for change in &record.source_changes {
        resolve_safe(Path::new("."), &change.path)?;
        if !paths.insert(change.path.clone()) || change.prior_bytes == change.desired_bytes {
            return Err(PromotionError::Invalid(
                "promotion history source changes are invalid".into(),
            ));
        }
    }
    Ok(())
}

pub(crate) fn promotion_record_digest(record: &PromotionRecord) -> Result<String, PromotionError> {
    Ok(sha256(&canonical_json_bytes(record)?))
}

fn apply_source_change(
    root: &Path,
    transaction_id: &EntityId,
    index: usize,
    change: &RecordedSourceChange,
    require_prior: bool,
) -> Result<(), PromotionError> {
    let path = resolve_safe(root, &change.path)?;
    let current = read_optional_regular(&path)?;
    if require_prior && current != change.prior_bytes {
        return Err(PromotionError::SourceConflict(change.path.clone()));
    }
    if !require_prior && current != change.prior_bytes && current != change.desired_bytes {
        return Err(PromotionError::SourceConflict(change.path.clone()));
    }
    match &change.desired_bytes {
        Some(bytes) => {
            let parent = path
                .parent()
                .ok_or_else(|| PromotionError::UnsafeSource(change.path.clone()))?;
            fs::create_dir_all(parent)?;
            reject_symlink_components(parent)?;
            let temporary = parent.join(format!(".keith-promotion-{transaction_id}-{index}.tmp"));
            remove_if_present(&temporary)?;
            write_new_synced(&temporary, bytes)?;
            keith_platform::replace_file(&temporary, &path)?;
            sync_directory(parent)?;
        }
        None => match fs::remove_file(&path) {
            Ok(()) => sync_directory(path.parent().unwrap_or(root))?,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        },
    }
    Ok(())
}

fn resolve_safe(root: &Path, relative: &Path) -> Result<PathBuf, PromotionError> {
    if relative.is_absolute()
        || relative.as_os_str().is_empty()
        || relative.components().any(|component| {
            matches!(
                component,
                Component::ParentDir | Component::RootDir | Component::Prefix(_)
            )
        })
    {
        return Err(PromotionError::UnsafeSource(relative.to_path_buf()));
    }
    let candidate = root.join(relative);
    reject_symlink_components(&candidate)?;
    Ok(candidate)
}

fn reject_symlink_components(path: &Path) -> Result<(), PromotionError> {
    let mut current = PathBuf::new();
    for component in path.components() {
        current.push(component.as_os_str());
        match fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                return Err(PromotionError::UnsafeSource(path.to_path_buf()));
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => break,
            Err(error) => return Err(error.into()),
        }
    }
    Ok(())
}

fn read_optional_regular(path: &Path) -> Result<Option<Vec<u8>>, PromotionError> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_file() => {
            Err(PromotionError::UnsafeSource(path.to_path_buf()))
        }
        Ok(_) => Ok(Some(fs::read(path)?)),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(error.into()),
    }
}

fn remove_if_present(path: &Path) -> Result<(), std::io::Error> {
    match fs::remove_file(path) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

fn write_new_synced(path: &Path, bytes: &[u8]) -> Result<(), std::io::Error> {
    let mut file = OpenOptions::new().create_new(true).write(true).open(path)?;
    file.write_all(bytes)?;
    file.sync_all()
}

fn sync_directory(path: &Path) -> Result<(), std::io::Error> {
    File::open(path)?.sync_all()
}

fn sha256(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn class_name(class: ChangeClass) -> &'static str {
    match class {
        ChangeClass::A => "a",
        ChangeClass::B => "b",
        ChangeClass::C => "c",
        ChangeClass::D => "d",
    }
}

#[cfg(debug_assertions)]
fn crash_boundary(boundary: &str) {
    if std::env::var("KEITH_PROMOTION_CRASH_AT").as_deref() == Ok(boundary) {
        std::process::exit(86);
    }
}

#[cfg(not(debug_assertions))]
fn crash_boundary(_boundary: &str) {}
