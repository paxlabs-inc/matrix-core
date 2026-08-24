use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Component, Path, PathBuf};

use fs2::FileExt;
use keith_agent_types::{EntityId, Generation, RootTreeId, UtcTimestamp};
use keith_state_store_core::EvolutionLedgerRepository;
use keith_supervisor::{ImageRegistryError, SupervisorError, WorkerHealth};
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{
    EvolutionEvent, EvolutionLedger, LedgerError, LedgerText, PromotionError, PromotionRecord,
    PromotionRuntime, ReversalAuthority,
};

const STATE_VERSION: u32 = 1;
const ACTIVE_FILE: &str = "reversal.json";
const CATALOG_FILE: &str = "reversal-state.json";
const MUTATION_LOCK_FILE: &str = "promotion.lock";
const MAX_STATE_BYTES: u64 = 16 * 1024 * 1024;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ReversalScope {
    Promotion(EntityId),
    HumanBaseline,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ReversalPhase {
    Prepared,
    ImagePinned,
    WorkersRolling,
    WorkersRolled,
    SourceRestoring,
    SourceRestored,
    LedgerRecorded,
    Complete,
    Unresolved,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReversalWorker {
    pub root_tree_id: RootTreeId,
    pub prior_generation: Generation,
    pub restored_generation: Option<Generation>,
    pub error: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct SourceRestoration {
    path: PathBuf,
    expected_bytes: Option<Vec<u8>>,
    desired_bytes: Option<Vec<u8>>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct ReversalJournal {
    version: u32,
    transaction_id: EntityId,
    ledger_event_id: EntityId,
    phase: ReversalPhase,
    scope: ReversalScope,
    promotion_ids: Vec<EntityId>,
    hypothesis_id: EntityId,
    target_image_id: String,
    trusted_public_key: [u8; 32],
    acting_identity: String,
    reason: String,
    workers: Vec<ReversalWorker>,
    source_changes: Vec<SourceRestoration>,
    removable_directories: Vec<PathBuf>,
    source_applied: usize,
    unresolved: Vec<String>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct ReversalCatalog {
    version: u32,
    reverted: BTreeSet<EntityId>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReversalOutcome {
    pub transaction_id: EntityId,
    pub promotion_ids: Vec<EntityId>,
    pub restored_image_id: String,
    pub restored_paths: Vec<PathBuf>,
    pub workers: Vec<ReversalWorker>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BaselineRestore {
    pub outcome: ReversalOutcome,
}

pub struct ReversalRequest<'a> {
    pub scope: ReversalScope,
    pub trusted_public_key: &'a [u8; 32],
    pub authority: &'a ReversalAuthority,
    pub reason: &'a str,
    pub occurred_at: UtcTimestamp,
}

#[derive(Debug, Error)]
pub enum ReversalError {
    #[error("reversal request is invalid: {0}")]
    Invalid(String),
    #[error("reversal filesystem failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("reversal state is invalid: {0}")]
    State(#[from] serde_json::Error),
    #[error("promotion history is invalid: {0}")]
    Promotion(#[from] PromotionError),
    #[error("pinned image verification failed: {0}")]
    Registry(#[from] ImageRegistryError),
    #[error("worker reversal failed: {0}")]
    Supervisor(#[from] SupervisorError),
    #[error("signed reversal ledger append failed: {0}")]
    Ledger(#[from] LedgerError),
    #[error("another source mutation owns this installation")]
    Locked,
    #[error("an interrupted reversal must be recovered first")]
    RecoveryRequired,
    #[error("promotion is already reversed")]
    AlreadyReversed,
    #[error("live source differs from the recorded promotion state: {0}")]
    SourceConflict(PathBuf),
    #[error("unsafe reversal source path: {0}")]
    UnsafeSource(PathBuf),
    #[error("reversal is unresolved on pinned image {image_id}: {reasons}")]
    Unresolved { image_id: String, reasons: String },
}

/// Durable one-action image, worker-generation, source, and ledger reversal authority.
pub struct ReversalTransaction {
    root: PathBuf,
    live_root: PathBuf,
    active_path: PathBuf,
    catalog_path: PathBuf,
    _lock: File,
}

impl ReversalTransaction {
    /// Opens the same installation mutation lock used by promotion.
    pub fn open(
        root: impl Into<PathBuf>,
        live_root: impl AsRef<Path>,
    ) -> Result<Self, ReversalError> {
        let root = root.into();
        reject_symlink_components(&root)?;
        fs::create_dir_all(&root)?;
        let root = fs::canonicalize(root)?;
        let live_root = fs::canonicalize(live_root)?;
        let lock = OpenOptions::new()
            .create(true)
            .read(true)
            .write(true)
            .truncate(false)
            .open(root.join(MUTATION_LOCK_FILE))?;
        lock.try_lock_exclusive()
            .map_err(|_| ReversalError::Locked)?;
        Ok(Self {
            active_path: root.join(ACTIVE_FILE),
            catalog_path: root.join(CATALOG_FILE),
            root,
            live_root,
            _lock: lock,
        })
    }

    /// Reverses one promotion without a build, gate, toolchain, or network dependency.
    pub fn reverse<R, L>(
        &self,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        request: ReversalRequest<'_>,
    ) -> Result<ReversalOutcome, ReversalError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        if self.active_path.exists() {
            let active = self.load_journal()?;
            if active.phase != ReversalPhase::Complete {
                return Err(ReversalError::RecoveryRequired);
            }
            fs::remove_file(&self.active_path)?;
            sync_directory(&self.root)?;
        }
        if request.authority.installation_root != self.live_root {
            return Err(ReversalError::Invalid(
                "reversal authority belongs to another installation".into(),
            ));
        }
        let reason = LedgerText::redacted(request.reason, 2 * 1024, &[])?;
        let records = crate::promotion::load_promotion_history(&self.root)?;
        verify_signed_history(ledger, &records)?;
        let mut catalog = self.load_catalog()?;
        let targets = select_targets(&records, &catalog, &request.scope)?;
        let target_ids = targets
            .iter()
            .map(|record| record.transaction_id.clone())
            .collect::<BTreeSet<_>>();
        let mut desired_reverted = catalog.reverted.clone();
        desired_reverted.extend(target_ids.iter().cloned());
        let current_tree = materialize_tree(&records, &catalog.reverted);
        let desired_tree = materialize_tree(&records, &desired_reverted);
        let source_changes = source_plan(&self.live_root, &current_tree, &desired_tree)?;
        let target_image_id = target_image(&records, &desired_reverted)?;
        runtime.registry().resolve(&target_image_id)?;
        let removable_directories = removable_directories(&records, &desired_reverted);
        let workers = runtime
            .active_workers()
            .into_iter()
            .map(|worker| ReversalWorker {
                root_tree_id: worker.root_tree_id,
                prior_generation: worker.generation,
                restored_generation: None,
                error: None,
            })
            .collect();
        let mut journal = ReversalJournal {
            version: STATE_VERSION,
            transaction_id: EntityId::new(),
            ledger_event_id: EntityId::new(),
            phase: ReversalPhase::Prepared,
            scope: request.scope,
            promotion_ids: target_ids.into_iter().collect(),
            hypothesis_id: targets
                .last()
                .expect("non-empty reversal targets")
                .hypothesis_id
                .clone(),
            target_image_id,
            trusted_public_key: *request.trusted_public_key,
            acting_identity: request.authority.identity.clone(),
            reason: reason.as_str().into(),
            workers,
            source_changes,
            removable_directories,
            source_applied: 0,
            unresolved: Vec::new(),
        };
        self.persist_journal(&journal)?;
        crash_boundary("prepared");
        let outcome = self.execute(
            runtime,
            ledger,
            &mut catalog,
            &mut journal,
            request.occurred_at,
        )?;
        Ok(outcome)
    }

    /// Reverts every still-active self-evolution promotion to the human installation baseline.
    pub fn restore_baseline<R, L>(
        &self,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        trusted_public_key: &[u8; 32],
        authority: &ReversalAuthority,
        reason: &str,
        occurred_at: UtcTimestamp,
    ) -> Result<BaselineRestore, ReversalError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        self.reverse(
            runtime,
            ledger,
            ReversalRequest {
                scope: ReversalScope::HumanBaseline,
                trusted_public_key,
                authority,
                reason,
                occurred_at,
            },
        )
        .map(|outcome| BaselineRestore { outcome })
    }

    /// Resumes an interrupted reversal from its last durable boundary without new confirmation.
    pub fn recover<R, L>(
        &self,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        occurred_at: UtcTimestamp,
    ) -> Result<Option<ReversalOutcome>, ReversalError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        if !self.active_path.exists() {
            return Ok(None);
        }
        let mut journal = self.load_journal()?;
        if journal.phase == ReversalPhase::Complete {
            return Ok(Some(outcome_for(&journal)));
        }
        journal.unresolved.clear();
        let mut catalog = self.load_catalog()?;
        self.execute(runtime, ledger, &mut catalog, &mut journal, occurred_at)
            .map(Some)
    }

    fn execute<R, L>(
        &self,
        runtime: &mut R,
        ledger: &EvolutionLedger<L>,
        catalog: &mut ReversalCatalog,
        journal: &mut ReversalJournal,
        occurred_at: UtcTimestamp,
    ) -> Result<ReversalOutcome, ReversalError>
    where
        R: PromotionRuntime,
        L: EvolutionLedgerRepository,
    {
        runtime
            .registry_mut()
            .restore_verified(&journal.target_image_id, &journal.trusted_public_key)?;
        journal.phase = ReversalPhase::ImagePinned;
        self.persist_journal(journal)?;
        crash_boundary("image_pinned");

        journal.phase = ReversalPhase::WorkersRolling;
        self.persist_journal(journal)?;
        for index in 0..journal.workers.len() {
            let root = journal.workers[index].root_tree_id.clone();
            if let Some(current) = runtime
                .active_workers()
                .into_iter()
                .find(|worker| worker.root_tree_id == root)
                && current.image_id == journal.target_image_id
                && current.health == WorkerHealth::Healthy
            {
                journal.workers[index].restored_generation = Some(current.generation);
                journal.workers[index].error = None;
                continue;
            }
            match runtime.roll_exact(&root, &journal.target_image_id) {
                Ok(proof)
                    if proof.image_id == journal.target_image_id
                        && proof.generation > journal.workers[index].prior_generation
                        && proof.health == WorkerHealth::Healthy =>
                {
                    journal.workers[index].restored_generation = Some(proof.generation);
                    journal.workers[index].error = None;
                }
                Ok(_) => {
                    journal.workers[index].error =
                        Some("worker returned invalid reversal proof".into());
                }
                Err(error) => journal.workers[index].error = Some(error.to_string()),
            }
            self.persist_journal(journal)?;
            crash_boundary("worker_rolled");
        }
        let worker_failures = journal
            .workers
            .iter()
            .filter_map(|worker| worker.error.clone())
            .collect::<Vec<_>>();
        if !worker_failures.is_empty() {
            return self.unresolved(journal, worker_failures);
        }
        journal.phase = ReversalPhase::WorkersRolled;
        self.persist_journal(journal)?;
        crash_boundary("workers_rolled");

        journal.phase = ReversalPhase::SourceRestoring;
        self.persist_journal(journal)?;
        for index in 0..journal.source_changes.len() {
            match apply_source_restoration(
                &self.live_root,
                &journal.transaction_id,
                index,
                &journal.source_changes[index],
            ) {
                Ok(()) => journal.source_applied = journal.source_applied.max(index + 1),
                Err(error) => return self.unresolved(journal, vec![error.to_string()]),
            }
            self.persist_journal(journal)?;
            crash_boundary("source_restored");
        }
        for directory in journal.removable_directories.iter().rev() {
            let path = resolve_safe(&self.live_root, directory)?;
            match fs::remove_dir(&path) {
                Ok(()) => sync_directory(path.parent().unwrap_or(&self.live_root))?,
                Err(error)
                    if matches!(
                        error.kind(),
                        std::io::ErrorKind::NotFound | std::io::ErrorKind::DirectoryNotEmpty
                    ) => {}
                Err(error) => return self.unresolved(journal, vec![error.to_string()]),
            }
        }
        journal.phase = ReversalPhase::SourceRestored;
        self.persist_journal(journal)?;
        crash_boundary("source_complete");

        let restored_paths = journal
            .source_changes
            .iter()
            .map(|change| change.path.clone())
            .collect::<Vec<_>>();
        ledger.append(
            journal.ledger_event_id.clone(),
            occurred_at,
            EvolutionEvent::Revert {
                hypothesis_id: journal.hypothesis_id.clone(),
                reason: LedgerText::redacted(&journal.reason, 2 * 1024, &[])?,
                acting_identity: Some(LedgerText::redacted(&journal.acting_identity, 256, &[])?),
                promotion_ids: journal.promotion_ids.clone(),
                restored_image_id: LedgerText::redacted(&journal.target_image_id, 256, &[])?,
                restored_paths: restored_paths
                    .iter()
                    .map(|path| LedgerText::redacted(&path.to_string_lossy(), 1024, &[]))
                    .collect::<Result<Vec<_>, _>>()?,
                unresolved: None,
            },
        )?;
        journal.phase = ReversalPhase::LedgerRecorded;
        self.persist_journal(journal)?;
        crash_boundary("ledger_recorded");
        catalog
            .reverted
            .extend(journal.promotion_ids.iter().cloned());
        self.persist_catalog(catalog)?;
        journal.phase = ReversalPhase::Complete;
        self.persist_journal(journal)?;
        crash_boundary("complete");
        Ok(outcome_for(journal))
    }

    fn unresolved<T>(
        &self,
        journal: &mut ReversalJournal,
        reasons: Vec<String>,
    ) -> Result<T, ReversalError> {
        journal.phase = ReversalPhase::Unresolved;
        journal.unresolved.extend(reasons);
        self.persist_journal(journal)?;
        Err(ReversalError::Unresolved {
            image_id: journal.target_image_id.clone(),
            reasons: journal.unresolved.join("; "),
        })
    }

    fn load_catalog(&self) -> Result<ReversalCatalog, ReversalError> {
        if !self.catalog_path.exists() {
            return Ok(ReversalCatalog {
                version: STATE_VERSION,
                reverted: BTreeSet::new(),
            });
        }
        let catalog: ReversalCatalog = self.load_bounded(&self.catalog_path)?;
        if catalog.version != STATE_VERSION {
            return Err(ReversalError::Invalid(
                "reversal catalog version is unsupported".into(),
            ));
        }
        Ok(catalog)
    }

    fn load_journal(&self) -> Result<ReversalJournal, ReversalError> {
        let journal: ReversalJournal = self.load_bounded(&self.active_path)?;
        if journal.version != STATE_VERSION
            || journal.target_image_id.is_empty()
            || journal.promotion_ids.is_empty()
            || journal.source_applied > journal.source_changes.len()
        {
            return Err(ReversalError::Invalid(
                "reversal journal fields are invalid".into(),
            ));
        }
        Ok(journal)
    }

    fn load_bounded<T: for<'de> Deserialize<'de>>(&self, path: &Path) -> Result<T, ReversalError> {
        let metadata = fs::symlink_metadata(path)?;
        if !metadata.is_file()
            || metadata.file_type().is_symlink()
            || metadata.len() > MAX_STATE_BYTES
        {
            return Err(ReversalError::Invalid(
                "reversal state is unsafe or oversized".into(),
            ));
        }
        Ok(serde_json::from_slice(&fs::read(path)?)?)
    }

    fn persist_journal(&self, journal: &ReversalJournal) -> Result<(), ReversalError> {
        persist(&self.root, &self.active_path, journal)
    }

    fn persist_catalog(&self, catalog: &ReversalCatalog) -> Result<(), ReversalError> {
        persist(&self.root, &self.catalog_path, catalog)
    }
}

fn verify_signed_history<L: EvolutionLedgerRepository>(
    ledger: &EvolutionLedger<L>,
    records: &[PromotionRecord],
) -> Result<(), ReversalError> {
    let mut bindings = BTreeMap::new();
    for record in ledger.records()? {
        if let EvolutionEvent::Promotion {
            hypothesis_id,
            promotion_id,
            artifact_id,
            artifact_digest,
        } = record.event
        {
            if bindings
                .insert(
                    promotion_id,
                    (
                        hypothesis_id,
                        artifact_id.as_str().to_owned(),
                        artifact_digest.as_str().to_owned(),
                    ),
                )
                .is_some()
            {
                return Err(ReversalError::Invalid(
                    "signed ledger contains duplicate promotion identities".into(),
                ));
            }
        }
    }
    for record in records {
        let digest = crate::promotion::promotion_record_digest(record)?;
        let Some((hypothesis_id, image_id, archive_digest)) = bindings.get(&record.transaction_id)
        else {
            return Err(ReversalError::Invalid(
                "promotion archive has no signed ledger binding".into(),
            ));
        };
        if hypothesis_id != &record.hypothesis_id
            || image_id != &record.candidate_image_id
            || archive_digest != &digest
        {
            return Err(ReversalError::Invalid(
                "promotion archive disagrees with its signed ledger binding".into(),
            ));
        }
    }
    if bindings.len() != records.len() {
        return Err(ReversalError::Invalid(
            "signed ledger promotion set disagrees with the archive set".into(),
        ));
    }
    Ok(())
}

fn select_targets<'a>(
    records: &'a [PromotionRecord],
    catalog: &ReversalCatalog,
    scope: &ReversalScope,
) -> Result<Vec<&'a PromotionRecord>, ReversalError> {
    let targets = match scope {
        ReversalScope::Promotion(id) => records
            .iter()
            .filter(|record| &record.transaction_id == id && !catalog.reverted.contains(id))
            .collect::<Vec<_>>(),
        ReversalScope::HumanBaseline => records
            .iter()
            .filter(|record| !catalog.reverted.contains(&record.transaction_id))
            .collect::<Vec<_>>(),
    };
    if targets.is_empty() {
        if matches!(scope, ReversalScope::Promotion(id) if catalog.reverted.contains(id)) {
            Err(ReversalError::AlreadyReversed)
        } else {
            Err(ReversalError::Invalid(
                "no committed promotion matches the reversal scope".into(),
            ))
        }
    } else {
        Ok(targets)
    }
}

fn materialize_tree(
    records: &[PromotionRecord],
    reverted: &BTreeSet<EntityId>,
) -> BTreeMap<PathBuf, Option<Vec<u8>>> {
    let mut tree = BTreeMap::new();
    for record in records {
        for change in &record.source_changes {
            tree.entry(change.path.clone())
                .or_insert_with(|| change.prior_bytes.clone());
        }
    }
    for record in records {
        if reverted.contains(&record.transaction_id) {
            continue;
        }
        for change in &record.source_changes {
            tree.insert(change.path.clone(), change.desired_bytes.clone());
        }
    }
    tree
}

fn source_plan(
    live_root: &Path,
    current: &BTreeMap<PathBuf, Option<Vec<u8>>>,
    desired: &BTreeMap<PathBuf, Option<Vec<u8>>>,
) -> Result<Vec<SourceRestoration>, ReversalError> {
    let paths = current
        .keys()
        .chain(desired.keys())
        .cloned()
        .collect::<BTreeSet<_>>();
    let mut changes = Vec::new();
    for path in paths {
        let expected = current.get(&path).cloned().flatten();
        let target = desired.get(&path).cloned().flatten();
        let actual = read_optional_regular(&resolve_safe(live_root, &path)?)?;
        if actual != expected {
            return Err(ReversalError::SourceConflict(path));
        }
        if expected != target {
            changes.push(SourceRestoration {
                path,
                expected_bytes: expected,
                desired_bytes: target,
            });
        }
    }
    Ok(changes)
}

fn target_image(
    records: &[PromotionRecord],
    reverted: &BTreeSet<EntityId>,
) -> Result<String, ReversalError> {
    records
        .iter()
        .rev()
        .find(|record| !reverted.contains(&record.transaction_id))
        .map(|record| record.candidate_image_id.clone())
        .or_else(|| records.first().map(|record| record.prior_image_id.clone()))
        .ok_or_else(|| ReversalError::Invalid("promotion history is empty".into()))
}

fn removable_directories(
    records: &[PromotionRecord],
    reverted: &BTreeSet<EntityId>,
) -> Vec<PathBuf> {
    let active = records
        .iter()
        .filter(|record| !reverted.contains(&record.transaction_id))
        .flat_map(|record| record.created_directories.iter().cloned())
        .collect::<BTreeSet<_>>();
    let mut removable = records
        .iter()
        .filter(|record| reverted.contains(&record.transaction_id))
        .flat_map(|record| record.created_directories.iter().cloned())
        .filter(|directory| !active.contains(directory))
        .collect::<BTreeSet<_>>()
        .into_iter()
        .collect::<Vec<_>>();
    removable.sort_by_key(|path| std::cmp::Reverse(path.components().count()));
    removable
}

fn apply_source_restoration(
    root: &Path,
    transaction_id: &EntityId,
    index: usize,
    change: &SourceRestoration,
) -> Result<(), ReversalError> {
    let path = resolve_safe(root, &change.path)?;
    let current = read_optional_regular(&path)?;
    if current == change.desired_bytes {
        return Ok(());
    }
    if current != change.expected_bytes {
        return Err(ReversalError::SourceConflict(change.path.clone()));
    }
    match &change.desired_bytes {
        Some(bytes) => {
            let parent = path
                .parent()
                .ok_or_else(|| ReversalError::UnsafeSource(change.path.clone()))?;
            fs::create_dir_all(parent)?;
            reject_symlink_components(parent)?;
            let temporary = parent.join(format!(".keith-reversal-{transaction_id}-{index}.tmp"));
            remove_if_present(&temporary)?;
            let mut file = OpenOptions::new()
                .create_new(true)
                .write(true)
                .open(&temporary)?;
            file.write_all(bytes)?;
            file.sync_all()?;
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

fn persist<T: Serialize>(root: &Path, path: &Path, value: &T) -> Result<(), ReversalError> {
    let bytes = serde_json::to_vec(value)?;
    if u64::try_from(bytes.len()).unwrap_or(u64::MAX) > MAX_STATE_BYTES {
        return Err(ReversalError::Invalid(
            "reversal state exceeds its storage bound".into(),
        ));
    }
    let temporary = root.join(format!(
        ".{}.tmp",
        path.file_name()
            .and_then(|name| name.to_str())
            .unwrap_or("state")
    ));
    remove_if_present(&temporary)?;
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&temporary)?;
    file.write_all(&bytes)?;
    file.sync_all()?;
    keith_platform::replace_file(&temporary, path)?;
    sync_directory(root)?;
    Ok(())
}

fn resolve_safe(root: &Path, relative: &Path) -> Result<PathBuf, ReversalError> {
    if relative.is_absolute()
        || relative.as_os_str().is_empty()
        || relative.components().any(|component| {
            matches!(
                component,
                Component::ParentDir | Component::RootDir | Component::Prefix(_)
            )
        })
    {
        return Err(ReversalError::UnsafeSource(relative.into()));
    }
    let path = root.join(relative);
    reject_symlink_components(&path)?;
    Ok(path)
}

fn reject_symlink_components(path: &Path) -> Result<(), ReversalError> {
    let mut current = PathBuf::new();
    for component in path.components() {
        current.push(component.as_os_str());
        match fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                return Err(ReversalError::UnsafeSource(path.into()));
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => break,
            Err(error) => return Err(error.into()),
        }
    }
    Ok(())
}

fn read_optional_regular(path: &Path) -> Result<Option<Vec<u8>>, ReversalError> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_file() => {
            Err(ReversalError::UnsafeSource(path.into()))
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

fn sync_directory(path: &Path) -> Result<(), std::io::Error> {
    File::open(path)?.sync_all()
}

fn outcome_for(journal: &ReversalJournal) -> ReversalOutcome {
    ReversalOutcome {
        transaction_id: journal.transaction_id.clone(),
        promotion_ids: journal.promotion_ids.clone(),
        restored_image_id: journal.target_image_id.clone(),
        restored_paths: journal
            .source_changes
            .iter()
            .map(|change| change.path.clone())
            .collect(),
        workers: journal.workers.clone(),
    }
}

#[cfg(debug_assertions)]
fn crash_boundary(boundary: &str) {
    if std::env::var("KEITH_REVERSAL_CRASH_AT").as_deref() == Ok(boundary) {
        std::process::exit(87);
    }
}

#[cfg(not(debug_assertions))]
fn crash_boundary(_boundary: &str) {}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::RecordedSourceChange;

    fn record(
        sequence: u64,
        transaction: u128,
        prior: Option<&[u8]>,
        desired: Option<&[u8]>,
    ) -> PromotionRecord {
        PromotionRecord {
            version: 1,
            sequence,
            transaction_id: EntityId::from_u128(transaction),
            hypothesis_id: EntityId::from_u128(transaction + 100),
            prior_image_id: format!("image-{}", sequence - 1),
            candidate_image_id: format!("image-{sequence}"),
            rolls: Vec::new(),
            source_changes: vec![RecordedSourceChange {
                path: PathBuf::from("crates/feature/src/lib.rs"),
                prior_bytes: prior.map(<[u8]>::to_vec),
                desired_bytes: desired.map(<[u8]>::to_vec),
            }],
            created_directories: Vec::new(),
        }
    }

    #[test]
    fn stacked_and_out_of_order_replay_preserves_later_full_bytes() {
        let records = vec![
            record(1, 1, Some(b"base"), Some(b"first")),
            record(2, 2, Some(b"first"), Some(b"second")),
            record(3, 3, Some(b"second"), Some(b"third")),
        ];
        let reverted = BTreeSet::from([EntityId::from_u128(2)]);
        assert_eq!(
            materialize_tree(&records, &reverted).get(Path::new("crates/feature/src/lib.rs")),
            Some(&Some(b"third".to_vec()))
        );
        let baseline = records
            .iter()
            .map(|record| record.transaction_id.clone())
            .collect();
        assert_eq!(
            materialize_tree(&records, &baseline).get(Path::new("crates/feature/src/lib.rs")),
            Some(&Some(b"base".to_vec()))
        );
    }

    #[test]
    fn reversal_source_plan_rejects_unrecorded_live_edits_before_mutation() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("crates/feature/src/lib.rs");
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(&path, b"human edit").unwrap();
        let current = BTreeMap::from([(
            PathBuf::from("crates/feature/src/lib.rs"),
            Some(b"candidate".to_vec()),
        )]);
        let desired = BTreeMap::from([(
            PathBuf::from("crates/feature/src/lib.rs"),
            Some(b"base".to_vec()),
        )]);
        assert!(matches!(
            source_plan(directory.path(), &current, &desired),
            Err(ReversalError::SourceConflict(_))
        ));
        assert_eq!(fs::read(path).unwrap(), b"human edit");
    }
}
