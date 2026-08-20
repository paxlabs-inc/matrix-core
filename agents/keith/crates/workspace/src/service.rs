use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::fmt::Write as _;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Component, Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{self, Receiver, RecvTimeoutError};
use std::sync::{Arc, Mutex, MutexGuard};
use std::thread::{self, JoinHandle};
use std::time::Duration;

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, Revision, SchemaVersion, UtcTimestamp, canonical_json_bytes,
};
use keith_provider_core::CancellationToken;
use keith_tool_runner_core::{ExpectedPreimage, WorkspaceFs, WorkspaceLimits};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{
    EditOutcome, FileToken, FileTokenSnapshot, FileVersion, MergeProposal, PersonalWorkspaceError,
    PersonalWorkspaceLimits, SnapshotManifest, VersionOrigin, WorkspaceActor, WorkspaceEvent,
    WorkspaceLayout,
};

const INDEX_PATH: &str = ".keith/index.json";
const CORE_FILES: &[&str] = &["AGENT.md", "USER.md", "RULE.md", "MEMORY.md"];
const EDITABLE_DIRECTORIES: &[&str] =
    &["memory/daily", "state", "knowledge", "skills", "artifacts"];
const PROTECTED_TOP_LEVEL: &[&str] = &[".keith", "backups"];

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct CurrentFile {
    revision: Revision,
    digest: String,
    bytes: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct WorkspaceIndex {
    version: SchemaVersion,
    next_revision: Revision,
    context_revision: Revision,
    current: BTreeMap<PathBuf, CurrentFile>,
    versions: BTreeMap<PathBuf, Vec<FileVersion>>,
}

impl Default for WorkspaceIndex {
    fn default() -> Self {
        Self {
            version: CURRENT_SCHEMA_VERSION,
            next_revision: Revision::ZERO,
            context_revision: Revision::ZERO,
            current: BTreeMap::new(),
            versions: BTreeMap::new(),
        }
    }
}

struct Inner {
    root: PathBuf,
    filesystem: WorkspaceFs,
    limits: PersonalWorkspaceLimits,
    state: Mutex<WorkspaceIndex>,
    mutation: Mutex<()>,
}

#[derive(Clone)]
pub struct PersonalWorkspace {
    inner: Arc<Inner>,
}

impl PersonalWorkspace {
    /// Creates or opens the complete human-readable profile layout and durable version ledger.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe existing entries, invalid metadata, or persistence failure.
    pub fn open(
        root: impl AsRef<Path>,
        limits: PersonalWorkspaceLimits,
        now: UtcTimestamp,
    ) -> Result<Self, PersonalWorkspaceError> {
        validate_limits(limits)?;
        fs::create_dir_all(root.as_ref())?;
        let root = fs::canonicalize(root.as_ref())?;
        create_layout(&root)?;
        recover_temporary_files(&root)?;
        let filesystem = WorkspaceFs::open(
            &root,
            WorkspaceLimits {
                max_file_bytes: limits.max_file_bytes,
                max_directory_entries: limits.max_files,
                max_search_files: limits.max_files,
                max_search_bytes: usize::try_from(limits.max_total_bytes).unwrap_or(usize::MAX),
                max_search_results: limits.max_files,
            },
        )?;
        let index_path = root.join(INDEX_PATH);
        let index = if index_path.exists() {
            let index: WorkspaceIndex = serde_json::from_slice(&fs::read(index_path)?)?;
            validate_index(&index)?;
            index
        } else {
            WorkspaceIndex::default()
        };
        let workspace = Self {
            inner: Arc::new(Inner {
                root,
                filesystem,
                limits,
                state: Mutex::new(index),
                mutation: Mutex::new(()),
            }),
        };
        if workspace
            .inner
            .state
            .lock()
            .map_err(|_| PersonalWorkspaceError::LockPoisoned)?
            .current
            .is_empty()
        {
            workspace.initialize_index(now)?;
        } else {
            workspace.verify_backup_ledger()?;
        }
        Ok(workspace)
    }

    pub fn layout(&self) -> WorkspaceLayout {
        let root = self.inner.root.clone();
        WorkspaceLayout {
            agent: root.join("AGENT.md"),
            user: root.join("USER.md"),
            rules: root.join("RULE.md"),
            memory: root.join("MEMORY.md"),
            daily_memory: root.join("memory/daily"),
            state: root.join("state"),
            knowledge: root.join("knowledge"),
            skills: root.join("skills"),
            artifacts: root.join("artifacts"),
            backups: root.join("backups"),
            metadata: root.join(".keith"),
            root,
        }
    }

    /// Returns the authoritative context invalidation revision.
    ///
    /// # Errors
    ///
    /// Returns an error when the workspace state lock is poisoned.
    pub fn context_revision(&self) -> Result<Revision, PersonalWorkspaceError> {
        Ok(self.state()?.context_revision)
    }

    /// Returns the current optimistic token for an editable path.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe/protected paths or poisoned state.
    pub fn token(&self, path: impl AsRef<Path>) -> Result<FileToken, PersonalWorkspaceError> {
        let path = editable_path(path.as_ref())?;
        let state = self.state()?;
        Ok(state.current.get(&path).map_or(
            FileToken {
                revision: None,
                digest: None,
            },
            |current| FileToken {
                revision: Some(current.revision),
                digest: Some(current.digest.clone()),
            },
        ))
    }

    /// Returns the durable history for an editable file.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe/protected paths or poisoned state.
    pub fn versions(
        &self,
        path: impl AsRef<Path>,
    ) -> Result<Vec<FileVersion>, PersonalWorkspaceError> {
        let path = editable_path(path.as_ref())?;
        Ok(self
            .state()?
            .versions
            .get(&path)
            .cloned()
            .unwrap_or_default())
    }

    /// Detects, validates, versions, and invalidates context for external edits.
    ///
    /// # Errors
    ///
    /// Returns an error when scanning or durable metadata persistence fails.
    pub fn scan_external_changes(
        &self,
        now: UtcTimestamp,
    ) -> Result<Vec<WorkspaceEvent>, PersonalWorkspaceError> {
        let _mutation = self.mutation()?;
        let scan = scan_editable(&self.inner.root, self.inner.limits)?;
        let mut state = self.state()?;
        let rejected_paths = scan
            .rejected
            .iter()
            .filter_map(|event| match event {
                WorkspaceEvent::Rejected { path, .. } => Some(path.clone()),
                _ => None,
            })
            .collect::<BTreeSet<_>>();
        let mut events = scan.rejected;
        let old_paths = state.current.keys().cloned().collect::<BTreeSet<_>>();
        let new_paths = scan.files.keys().cloned().collect::<BTreeSet<_>>();
        for removed in old_paths.difference(&new_paths) {
            if rejected_paths.contains(removed) {
                continue;
            }
            let version = record_version(
                &self.inner.root,
                &mut state,
                removed.clone(),
                None,
                VersionOrigin::External,
                now,
            )?;
            events.push(WorkspaceEvent::Changed {
                version,
                context_revision: state.context_revision,
            });
        }
        for (path, file) in scan.files {
            if state
                .current
                .get(&path)
                .is_some_and(|current| current.digest == file.digest)
            {
                continue;
            }
            let version = record_version(
                &self.inner.root,
                &mut state,
                path,
                Some(&file.bytes),
                VersionOrigin::External,
                now,
            )?;
            events.push(WorkspaceEvent::Changed {
                version,
                context_revision: state.context_revision,
            });
        }
        if events
            .iter()
            .any(|event| matches!(event, WorkspaceEvent::Changed { .. }))
        {
            persist_index(&self.inner.root, &state)?;
        }
        Ok(events)
    }

    /// Atomically writes an editable file or returns a human/agent conflict proposal.
    ///
    /// # Errors
    ///
    /// Returns an error for unauthorized paths, invalid content, or persistence failure.
    pub fn edit(
        &self,
        actor: WorkspaceActor,
        path: impl AsRef<Path>,
        expected: &FileToken,
        replacement: &[u8],
        now: UtcTimestamp,
    ) -> Result<EditOutcome, PersonalWorkspaceError> {
        self.edit_with_origin(
            actor,
            path.as_ref(),
            expected,
            replacement,
            VersionOrigin::Agent,
            now,
        )
    }

    /// Atomically deletes an editable file or returns a human/agent conflict proposal.
    ///
    /// # Errors
    ///
    /// Returns an error for unauthorized paths, missing files, or persistence failure.
    pub fn delete(
        &self,
        actor: WorkspaceActor,
        path: impl AsRef<Path>,
        expected: &FileToken,
        now: UtcTimestamp,
    ) -> Result<EditOutcome, PersonalWorkspaceError> {
        let path = editable_path(path.as_ref())?;
        authorize(actor, &path)?;
        let _mutation = self.mutation()?;
        self.scan_locked(now)?;
        let mut state = self.state()?;
        let current_token = token_from_state(&state, &path);
        if &current_token != expected {
            return Ok(EditOutcome::Conflict(self.merge_proposal(
                &state,
                &path,
                expected,
                &[],
            )?));
        }
        let digest = expected
            .digest
            .as_ref()
            .ok_or(PersonalWorkspaceError::MissingVersion)?;
        self.inner
            .filesystem
            .delete(&path, &ExpectedPreimage::Sha256(digest.clone()))?;
        let version = record_version(
            &self.inner.root,
            &mut state,
            path,
            None,
            VersionOrigin::Agent,
            now,
        )?;
        persist_index(&self.inner.root, &state)?;
        Ok(EditOutcome::Written(version))
    }

    fn edit_with_origin(
        &self,
        actor: WorkspaceActor,
        path: &Path,
        expected: &FileToken,
        replacement: &[u8],
        origin: VersionOrigin,
        now: UtcTimestamp,
    ) -> Result<EditOutcome, PersonalWorkspaceError> {
        let path = editable_path(path)?;
        authorize(actor, &path)?;
        validate_content(&path, replacement, self.inner.limits.max_file_bytes)?;
        let _mutation = self.mutation()?;
        self.scan_locked(now)?;
        let mut state = self.state()?;
        let current_token = token_from_state(&state, &path);
        if &current_token != expected {
            return Ok(EditOutcome::Conflict(self.merge_proposal(
                &state,
                &path,
                expected,
                replacement,
            )?));
        }
        let preimage = expected
            .digest
            .as_ref()
            .map_or(ExpectedPreimage::Missing, |digest| {
                ExpectedPreimage::Sha256(digest.clone())
            });
        let cancellation = CancellationToken::default();
        match self
            .inner
            .filesystem
            .write_atomic(&path, replacement, &preimage, &cancellation)
        {
            Ok(_) => {}
            Err(keith_tool_runner_core::WorkspaceError::PreimageMismatch) => {
                drop(state);
                self.scan_locked(now)?;
                let state = self.state()?;
                return Ok(EditOutcome::Conflict(self.merge_proposal(
                    &state,
                    &path,
                    expected,
                    replacement,
                )?));
            }
            Err(error) => return Err(error.into()),
        }
        let version = record_version(
            &self.inner.root,
            &mut state,
            path,
            Some(replacement),
            origin,
            now,
        )?;
        persist_index(&self.inner.root, &state)?;
        Ok(EditOutcome::Written(version))
    }

    /// Starts a real polling watcher that publishes validated external-change events.
    ///
    /// # Errors
    ///
    /// Returns an error when the configured interval is zero.
    pub fn watch(&self) -> Result<WorkspaceWatcher, PersonalWorkspaceError> {
        let interval = Duration::from_millis(self.inner.limits.watcher_interval_ms);
        if interval.is_zero() {
            return Err(PersonalWorkspaceError::InvalidWatcherInterval);
        }
        let workspace = self.clone();
        let stop = Arc::new(AtomicBool::new(false));
        let worker_stop = Arc::clone(&stop);
        let (sender, receiver) = mpsc::channel();
        let handle = thread::spawn(move || {
            while !worker_stop.load(Ordering::Acquire) {
                thread::sleep(interval);
                let events = UtcTimestamp::now()
                    .map_err(|error| error.to_string())
                    .and_then(|now| {
                        workspace
                            .scan_external_changes(now)
                            .map_err(|error| error.to_string())
                    });
                match events {
                    Ok(events) => {
                        for event in events {
                            if sender.send(event).is_err() {
                                return;
                            }
                        }
                    }
                    Err(reason) => {
                        if sender
                            .send(WorkspaceEvent::WatcherError { reason })
                            .is_err()
                        {
                            return;
                        }
                    }
                }
            }
        });
        Ok(WorkspaceWatcher {
            receiver,
            stop,
            handle: Some(handle),
        })
    }

    /// Captures every accepted editable file into immutable content-addressed backups.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid labels, rejected external edits, or persistence failure.
    pub fn create_snapshot(
        &self,
        label: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<SnapshotManifest, PersonalWorkspaceError> {
        let label = label.into();
        if label.trim().is_empty() || label.len() > 256 {
            return Err(PersonalWorkspaceError::Corrupt(
                "snapshot label must be bounded and non-empty".into(),
            ));
        }
        let events = self.scan_external_changes(now)?;
        if events
            .iter()
            .any(|event| matches!(event, WorkspaceEvent::Rejected { .. }))
        {
            return Err(PersonalWorkspaceError::Corrupt(
                "invalid external files must be resolved before snapshot".into(),
            ));
        }
        let state = self.state()?;
        let manifest = SnapshotManifest {
            version: CURRENT_SCHEMA_VERSION,
            id: EntityId::new(),
            label,
            created_at: now,
            files: state
                .current
                .iter()
                .map(|(path, current)| {
                    (
                        path.clone(),
                        FileTokenSnapshot {
                            revision: current.revision,
                            digest: current.digest.clone(),
                            bytes: current.bytes,
                        },
                    )
                })
                .collect(),
        };
        write_immutable(
            &self
                .inner
                .root
                .join("backups/snapshots")
                .join(format!("{}.json", manifest.id)),
            &canonical_json_bytes(&manifest)?,
        )?;
        Ok(manifest)
    }

    /// Restores a complete versioned snapshot using atomic per-file replacements.
    ///
    /// # Errors
    ///
    /// Returns an error for unauthorized actors, missing snapshots/blobs, or persistence failure.
    pub fn restore_snapshot(
        &self,
        actor: WorkspaceActor,
        snapshot_id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<(), PersonalWorkspaceError> {
        if !matches!(actor, WorkspaceActor::Human | WorkspaceActor::System) {
            return Err(PersonalWorkspaceError::Protected);
        }
        let path = self
            .inner
            .root
            .join("backups/snapshots")
            .join(format!("{snapshot_id}.json"));
        let manifest: SnapshotManifest =
            serde_json::from_slice(&fs::read(path).map_err(|error| {
                if error.kind() == std::io::ErrorKind::NotFound {
                    PersonalWorkspaceError::MissingVersion
                } else {
                    error.into()
                }
            })?)?;
        validate_snapshot(&manifest, snapshot_id)?;
        let mut contents = BTreeMap::new();
        for (file, desired) in &manifest.files {
            let bytes = read_blob(&self.inner.root, &desired.digest)?;
            if u64::try_from(bytes.len()).unwrap_or(u64::MAX) != desired.bytes {
                return Err(PersonalWorkspaceError::Corrupt(
                    "snapshot blob length mismatch".into(),
                ));
            }
            contents.insert(file.clone(), bytes);
        }
        let _mutation = self.mutation()?;
        self.scan_locked(now)?;
        let mut state = self.state()?;
        let existing = state.current.keys().cloned().collect::<BTreeSet<_>>();
        let desired = manifest.files.keys().cloned().collect::<BTreeSet<_>>();
        let cancellation = CancellationToken::default();
        for removed in existing.difference(&desired) {
            let token = token_from_state(&state, removed);
            self.inner.filesystem.delete(
                removed,
                &token
                    .digest
                    .map_or(ExpectedPreimage::Missing, ExpectedPreimage::Sha256),
            )?;
            record_version(
                &self.inner.root,
                &mut state,
                removed.clone(),
                None,
                VersionOrigin::Restore,
                now,
            )?;
        }
        for (file, bytes) in contents {
            let token = token_from_state(&state, &file);
            let preimage = token
                .digest
                .map_or(ExpectedPreimage::Missing, ExpectedPreimage::Sha256);
            self.inner
                .filesystem
                .write_atomic(&file, &bytes, &preimage, &cancellation)?;
            record_version(
                &self.inner.root,
                &mut state,
                file,
                Some(&bytes),
                VersionOrigin::Restore,
                now,
            )?;
        }
        persist_index(&self.inner.root, &state)
    }

    /// Restores one historical file version through the same conflict-safe write path.
    ///
    /// # Errors
    ///
    /// Returns an error for unauthorized actors, missing/deleted versions, or write failure.
    pub fn restore_version(
        &self,
        actor: WorkspaceActor,
        path: impl AsRef<Path>,
        revision: Revision,
        now: UtcTimestamp,
    ) -> Result<EditOutcome, PersonalWorkspaceError> {
        if !matches!(actor, WorkspaceActor::Human | WorkspaceActor::System) {
            return Err(PersonalWorkspaceError::Protected);
        }
        let path = editable_path(path.as_ref())?;
        let state = self.state()?;
        let version = state
            .versions
            .get(&path)
            .and_then(|versions| versions.iter().find(|version| version.revision == revision))
            .ok_or(PersonalWorkspaceError::MissingVersion)?;
        let digest = version
            .digest
            .as_ref()
            .ok_or(PersonalWorkspaceError::MissingVersion)?;
        let bytes = read_blob(&self.inner.root, digest)?;
        let token = token_from_state(&state, &path);
        drop(state);
        self.edit_with_origin(
            WorkspaceActor::System,
            &path,
            &token,
            &bytes,
            VersionOrigin::Restore,
            now,
        )
    }

    fn initialize_index(&self, now: UtcTimestamp) -> Result<(), PersonalWorkspaceError> {
        let scan = scan_editable(&self.inner.root, self.inner.limits)?;
        if !scan.rejected.is_empty() {
            return Err(PersonalWorkspaceError::Corrupt(
                "initial workspace contains invalid editable entries".into(),
            ));
        }
        let mut state = self.state()?;
        for (path, file) in scan.files {
            record_version(
                &self.inner.root,
                &mut state,
                path,
                Some(&file.bytes),
                VersionOrigin::Initial,
                now,
            )?;
        }
        persist_index(&self.inner.root, &state)
    }

    fn scan_locked(&self, now: UtcTimestamp) -> Result<(), PersonalWorkspaceError> {
        let scan = scan_editable(&self.inner.root, self.inner.limits)?;
        if !scan.rejected.is_empty() {
            return Err(PersonalWorkspaceError::Corrupt(
                "workspace contains invalid external edits".into(),
            ));
        }
        let mut state = self.state()?;
        let old_paths = state.current.keys().cloned().collect::<BTreeSet<_>>();
        let new_paths = scan.files.keys().cloned().collect::<BTreeSet<_>>();
        let mut changed = false;
        for removed in old_paths.difference(&new_paths) {
            record_version(
                &self.inner.root,
                &mut state,
                removed.clone(),
                None,
                VersionOrigin::External,
                now,
            )?;
            changed = true;
        }
        for (path, file) in scan.files {
            if state
                .current
                .get(&path)
                .is_some_and(|current| current.digest == file.digest)
            {
                continue;
            }
            record_version(
                &self.inner.root,
                &mut state,
                path,
                Some(&file.bytes),
                VersionOrigin::External,
                now,
            )?;
            changed = true;
        }
        if changed {
            persist_index(&self.inner.root, &state)?;
        }
        Ok(())
    }

    fn merge_proposal(
        &self,
        state: &WorkspaceIndex,
        path: &Path,
        expected: &FileToken,
        proposed: &[u8],
    ) -> Result<MergeProposal, PersonalWorkspaceError> {
        let current = match state.current.get(path) {
            Some(current) => read_blob(&self.inner.root, &current.digest)?,
            None => Vec::new(),
        };
        let base = expected
            .digest
            .as_ref()
            .map(|digest| read_blob(&self.inner.root, digest))
            .transpose()?
            .unwrap_or_default();
        Ok(MergeProposal {
            path: path.to_path_buf(),
            expected: expected.clone(),
            merged: proposed_merge(&base, &current, proposed),
            current,
            proposed: proposed.to_vec(),
        })
    }

    fn verify_backup_ledger(&self) -> Result<(), PersonalWorkspaceError> {
        let state = self.state()?;
        for version in state.versions.values().flatten() {
            if let Some(digest) = &version.digest {
                let bytes = read_blob(&self.inner.root, digest)?;
                if hex_digest(&bytes) != *digest {
                    return Err(PersonalWorkspaceError::Corrupt(
                        "version backup digest mismatch".into(),
                    ));
                }
            }
        }
        Ok(())
    }

    fn state(&self) -> Result<MutexGuard<'_, WorkspaceIndex>, PersonalWorkspaceError> {
        self.inner
            .state
            .lock()
            .map_err(|_| PersonalWorkspaceError::LockPoisoned)
    }

    fn mutation(&self) -> Result<MutexGuard<'_, ()>, PersonalWorkspaceError> {
        self.inner
            .mutation
            .lock()
            .map_err(|_| PersonalWorkspaceError::LockPoisoned)
    }
}

pub struct WorkspaceWatcher {
    receiver: Receiver<WorkspaceEvent>,
    stop: Arc<AtomicBool>,
    handle: Option<JoinHandle<()>>,
}

impl WorkspaceWatcher {
    /// # Errors
    ///
    /// Returns an error when the watcher disconnects before an event arrives.
    pub fn recv_timeout(
        &self,
        timeout: Duration,
    ) -> Result<Option<WorkspaceEvent>, PersonalWorkspaceError> {
        match self.receiver.recv_timeout(timeout) {
            Ok(event) => Ok(Some(event)),
            Err(RecvTimeoutError::Timeout) => Ok(None),
            Err(RecvTimeoutError::Disconnected) => Err(PersonalWorkspaceError::WatcherDisconnected),
        }
    }

    /// Stops the watcher and waits for its worker thread.
    ///
    /// # Errors
    ///
    /// Returns an error when the worker thread panicked.
    pub fn stop(mut self) -> Result<(), PersonalWorkspaceError> {
        self.stop.store(true, Ordering::Release);
        if self
            .handle
            .take()
            .is_some_and(|handle| handle.join().is_err())
        {
            return Err(PersonalWorkspaceError::WatcherPanicked);
        }
        Ok(())
    }
}

impl Drop for WorkspaceWatcher {
    fn drop(&mut self) {
        self.stop.store(true, Ordering::Release);
        if let Some(handle) = self.handle.take() {
            let _ = handle.join();
        }
    }
}

struct ScannedFile {
    digest: String,
    bytes: Vec<u8>,
}

struct ScanResult {
    files: BTreeMap<PathBuf, ScannedFile>,
    rejected: Vec<WorkspaceEvent>,
}

fn create_layout(root: &Path) -> Result<(), PersonalWorkspaceError> {
    for directory in EDITABLE_DIRECTORIES.iter().copied().chain([
        "backups/versions",
        "backups/snapshots",
        ".keith/runtime",
        ".keith/builtins",
        ".keith/credentials",
    ]) {
        let mut path = root.to_path_buf();
        for component in Path::new(directory).components() {
            let Component::Normal(name) = component else {
                return Err(PersonalWorkspaceError::UnsafePath);
            };
            path.push(name);
            match fs::symlink_metadata(&path) {
                Ok(metadata) if metadata.file_type().is_symlink() => {
                    return Err(PersonalWorkspaceError::Symlink);
                }
                Ok(metadata) if !metadata.is_dir() => {
                    return Err(PersonalWorkspaceError::UnsafePath);
                }
                Ok(_) => {}
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                    fs::create_dir(&path)?;
                }
                Err(error) => return Err(error.into()),
            }
        }
    }
    for file in CORE_FILES {
        let path = root.join(file);
        if path.exists() {
            if path.symlink_metadata()?.file_type().is_symlink() || !path.is_file() {
                return Err(PersonalWorkspaceError::Symlink);
            }
        } else {
            let mut handle = OpenOptions::new().create_new(true).write(true).open(path)?;
            handle.write_all(format!("# {}\n", file.trim_end_matches(".md")).as_bytes())?;
            handle.sync_all()?;
        }
    }
    sync_directory(root)
}

fn recover_temporary_files(root: &Path) -> Result<(), PersonalWorkspaceError> {
    let mut pending = VecDeque::from([root.to_path_buf()]);
    while let Some(directory) = pending.pop_front() {
        for entry in fs::read_dir(&directory)? {
            let entry = entry?;
            let file_type = entry.file_type()?;
            if file_type.is_symlink() {
                continue;
            }
            if file_type.is_dir() {
                pending.push_back(entry.path());
            } else if entry.file_name().to_str().is_some_and(|name| {
                name.starts_with(".keith-write-")
                    && Path::new(name)
                        .extension()
                        .is_some_and(|extension| extension.eq_ignore_ascii_case("tmp"))
            }) {
                fs::remove_file(entry.path())?;
            }
        }
    }
    Ok(())
}

fn scan_editable(
    root: &Path,
    limits: PersonalWorkspaceLimits,
) -> Result<ScanResult, PersonalWorkspaceError> {
    let mut result = ScanResult {
        files: BTreeMap::new(),
        rejected: Vec::new(),
    };
    let mut pending = VecDeque::from([(root.to_path_buf(), PathBuf::new())]);
    let mut total = 0_u64;
    let mut entries = 0_usize;
    while let Some((directory, relative_directory)) = pending.pop_front() {
        for entry in fs::read_dir(directory)? {
            entries = entries.saturating_add(1);
            if entries > limits.max_files {
                return Err(PersonalWorkspaceError::LimitExceeded);
            }
            let entry = entry?;
            let relative = relative_directory.join(entry.file_name());
            if relative.to_str().is_none() {
                result.rejected.push(WorkspaceEvent::Rejected {
                    path: relative,
                    reason: "workspace path is not valid UTF-8".into(),
                });
                continue;
            }
            if relative.components().next().is_some_and(|component| {
                let Component::Normal(name) = component else {
                    return true;
                };
                PROTECTED_TOP_LEVEL
                    .iter()
                    .any(|protected| name == std::ffi::OsStr::new(protected))
            }) {
                continue;
            }
            let file_type = entry.file_type()?;
            if file_type.is_symlink() {
                result.rejected.push(WorkspaceEvent::Rejected {
                    path: relative,
                    reason: "symbolic links are not accepted".into(),
                });
                continue;
            }
            if file_type.is_dir() {
                pending.push_back((entry.path(), relative));
                continue;
            }
            if !file_type.is_file() {
                result.rejected.push(WorkspaceEvent::Rejected {
                    path: relative,
                    reason: "entry is not a regular file".into(),
                });
                continue;
            }
            let length = entry.metadata()?.len();
            if usize::try_from(length).map_or(true, |length| length > limits.max_file_bytes) {
                result.rejected.push(WorkspaceEvent::Rejected {
                    path: relative,
                    reason: "file exceeds the configured limit".into(),
                });
                continue;
            }
            total = total.saturating_add(length);
            if total > limits.max_total_bytes {
                return Err(PersonalWorkspaceError::LimitExceeded);
            }
            let bytes = fs::read(entry.path())?;
            if requires_utf8(&relative) && std::str::from_utf8(&bytes).is_err() {
                result.rejected.push(WorkspaceEvent::Rejected {
                    path: relative,
                    reason: "text workspace file is not valid UTF-8".into(),
                });
                continue;
            }
            result.files.insert(
                relative,
                ScannedFile {
                    digest: hex_digest(&bytes),
                    bytes,
                },
            );
        }
    }
    Ok(result)
}

fn record_version(
    root: &Path,
    state: &mut WorkspaceIndex,
    path: PathBuf,
    bytes: Option<&[u8]>,
    origin: VersionOrigin,
    now: UtcTimestamp,
) -> Result<FileVersion, PersonalWorkspaceError> {
    let revision = state
        .next_revision
        .checked_next()
        .ok_or_else(|| PersonalWorkspaceError::Corrupt("version revision exhausted".into()))?;
    let context_revision = state
        .context_revision
        .checked_next()
        .ok_or_else(|| PersonalWorkspaceError::Corrupt("context revision exhausted".into()))?;
    let (digest, length) = if let Some(bytes) = bytes {
        let digest = hex_digest(bytes);
        ensure_blob(root, &digest, bytes)?;
        (Some(digest), u64::try_from(bytes.len()).unwrap_or(u64::MAX))
    } else {
        (None, 0)
    };
    let version = FileVersion {
        revision,
        path: path.clone(),
        digest: digest.clone(),
        bytes: length,
        origin,
        recorded_at: now,
    };
    if let Some(digest) = digest {
        state.current.insert(
            path.clone(),
            CurrentFile {
                revision,
                digest,
                bytes: length,
            },
        );
    } else {
        state.current.remove(&path);
    }
    state
        .versions
        .entry(path)
        .or_default()
        .push(version.clone());
    state.next_revision = revision;
    state.context_revision = context_revision;
    Ok(version)
}

fn persist_index(root: &Path, index: &WorkspaceIndex) -> Result<(), PersonalWorkspaceError> {
    atomic_write(&root.join(INDEX_PATH), &canonical_json_bytes(index)?)
}

fn ensure_blob(root: &Path, digest: &str, bytes: &[u8]) -> Result<(), PersonalWorkspaceError> {
    let path = root.join("backups/versions").join(format!("{digest}.blob"));
    if path.exists() {
        if fs::read(&path)? != bytes {
            return Err(PersonalWorkspaceError::Corrupt(
                "content-addressed backup collision".into(),
            ));
        }
        return Ok(());
    }
    write_immutable(&path, bytes)
}

fn read_blob(root: &Path, digest: &str) -> Result<Vec<u8>, PersonalWorkspaceError> {
    let bytes = fs::read(root.join("backups/versions").join(format!("{digest}.blob"))).map_err(
        |error| {
            if error.kind() == std::io::ErrorKind::NotFound {
                PersonalWorkspaceError::MissingVersion
            } else {
                error.into()
            }
        },
    )?;
    if hex_digest(&bytes) != digest {
        return Err(PersonalWorkspaceError::Corrupt(
            "backup blob digest mismatch".into(),
        ));
    }
    Ok(bytes)
}

fn write_immutable(path: &Path, bytes: &[u8]) -> Result<(), PersonalWorkspaceError> {
    atomic_write(path, bytes)?;
    let mut permissions = fs::metadata(path)?.permissions();
    permissions.set_readonly(true);
    fs::set_permissions(path, permissions)?;
    Ok(())
}

fn atomic_write(path: &Path, bytes: &[u8]) -> Result<(), PersonalWorkspaceError> {
    let parent = path.parent().ok_or(PersonalWorkspaceError::UnsafePath)?;
    fs::create_dir_all(parent)?;
    let temporary = parent.join(format!(".keith-write-{}.tmp", EntityId::new()));
    let result = (|| {
        let mut file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)?;
        file.write_all(bytes)?;
        file.sync_all()?;
        keith_platform::replace_file(&temporary, path)?;
        sync_directory(parent)
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
}

fn sync_directory(path: &Path) -> Result<(), PersonalWorkspaceError> {
    fs::File::open(path)?.sync_all()?;
    Ok(())
}

fn editable_path(path: &Path) -> Result<PathBuf, PersonalWorkspaceError> {
    if path.as_os_str().is_empty() || path.is_absolute() {
        return Err(PersonalWorkspaceError::UnsafePath);
    }
    let mut clean = PathBuf::new();
    for component in path.components() {
        let Component::Normal(name) = component else {
            return Err(PersonalWorkspaceError::UnsafePath);
        };
        clean.push(name);
    }
    if clean.components().next().is_some_and(|component| {
        let Component::Normal(name) = component else {
            return true;
        };
        PROTECTED_TOP_LEVEL
            .iter()
            .any(|protected| name == std::ffi::OsStr::new(protected))
    }) {
        return Err(PersonalWorkspaceError::Protected);
    }
    Ok(clean)
}

fn authorize(actor: WorkspaceActor, path: &Path) -> Result<(), PersonalWorkspaceError> {
    let allowed = match actor {
        WorkspaceActor::Human | WorkspaceActor::Agent | WorkspaceActor::System => true,
        WorkspaceActor::MemoryTool => {
            path == Path::new("MEMORY.md")
                || path.starts_with("memory")
                || path.starts_with("state")
        }
        WorkspaceActor::SkillTool => path.starts_with("skills"),
        WorkspaceActor::RefinementTool => {
            matches!(
                path.to_str(),
                Some("AGENT.md" | "USER.md" | "RULE.md" | "MEMORY.md")
            ) || path.starts_with("memory")
                || path.starts_with("knowledge")
                || path.starts_with("skills")
        }
    };
    if allowed {
        Ok(())
    } else {
        Err(PersonalWorkspaceError::Protected)
    }
}

fn validate_content(
    path: &Path,
    bytes: &[u8],
    max_file_bytes: usize,
) -> Result<(), PersonalWorkspaceError> {
    if bytes.len() > max_file_bytes {
        return Err(PersonalWorkspaceError::LimitExceeded);
    }
    if requires_utf8(path) && std::str::from_utf8(bytes).is_err() {
        return Err(PersonalWorkspaceError::NonUtf8);
    }
    Ok(())
}

fn requires_utf8(path: &Path) -> bool {
    !path.starts_with("artifacts")
}

fn token_from_state(state: &WorkspaceIndex, path: &Path) -> FileToken {
    state.current.get(path).map_or(
        FileToken {
            revision: None,
            digest: None,
        },
        |current| FileToken {
            revision: Some(current.revision),
            digest: Some(current.digest.clone()),
        },
    )
}

fn proposed_merge(base: &[u8], current: &[u8], proposed: &[u8]) -> Option<Vec<u8>> {
    if current == base {
        return Some(proposed.to_vec());
    }
    if proposed == base {
        return Some(current.to_vec());
    }
    if current.starts_with(base) && proposed.starts_with(base) {
        let mut merged = base.to_vec();
        merged.extend_from_slice(&current[base.len()..]);
        merged.extend_from_slice(&proposed[base.len()..]);
        return Some(merged);
    }
    None
}

fn validate_limits(limits: PersonalWorkspaceLimits) -> Result<(), PersonalWorkspaceError> {
    if limits.max_file_bytes == 0
        || limits.max_files == 0
        || limits.max_total_bytes == 0
        || limits.watcher_interval_ms == 0
    {
        Err(PersonalWorkspaceError::LimitExceeded)
    } else {
        Ok(())
    }
}

fn validate_index(index: &WorkspaceIndex) -> Result<(), PersonalWorkspaceError> {
    let mut revisions = BTreeSet::new();
    let versions_valid = index.versions.iter().all(|(path, versions)| {
        editable_path(path).is_ok()
            && !versions.is_empty()
            && versions
                .windows(2)
                .all(|pair| pair[0].revision < pair[1].revision)
            && versions.iter().all(|version| {
                &version.path == path
                    && version.revision <= index.next_revision
                    && revisions.insert(version.revision)
                    && version
                        .digest
                        .as_ref()
                        .is_none_or(|digest| digest.len() == 64)
            })
    });
    let current_valid = index.current.iter().all(|(path, current)| {
        editable_path(path).is_ok()
            && current.digest.len() == 64
            && index
                .versions
                .get(path)
                .and_then(|versions| versions.last())
                .is_some_and(|latest| {
                    latest.revision == current.revision
                        && latest.digest.as_ref() == Some(&current.digest)
                        && latest.bytes == current.bytes
                })
    });
    let latest_valid = index.versions.iter().all(|(path, versions)| {
        versions.last().is_some_and(|latest| match &latest.digest {
            Some(digest) => index.current.get(path).is_some_and(|current| {
                current.revision == latest.revision
                    && &current.digest == digest
                    && current.bytes == latest.bytes
            }),
            None => !index.current.contains_key(path),
        })
    });
    if index.version.major != CURRENT_SCHEMA_VERSION.major
        || index.version.minor > CURRENT_SCHEMA_VERSION.minor
        || !versions_valid
        || !current_valid
        || !latest_valid
    {
        return Err(PersonalWorkspaceError::Corrupt(
            "workspace index schema or paths are invalid".into(),
        ));
    }
    Ok(())
}

fn validate_snapshot(
    snapshot: &SnapshotManifest,
    expected_id: &EntityId,
) -> Result<(), PersonalWorkspaceError> {
    if &snapshot.id != expected_id
        || snapshot.version.major != CURRENT_SCHEMA_VERSION.major
        || snapshot.version.minor > CURRENT_SCHEMA_VERSION.minor
        || snapshot.label.trim().is_empty()
        || snapshot
            .files
            .keys()
            .any(|path| editable_path(path).is_err())
    {
        return Err(PersonalWorkspaceError::Corrupt(
            "snapshot metadata is invalid".into(),
        ));
    }
    Ok(())
}

fn hex_digest(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    let mut encoded = String::with_capacity(64);
    for byte in digest {
        write!(&mut encoded, "{byte:02x}").expect("writing to a string cannot fail");
    }
    encoded
}

#[cfg(test)]
mod tests {
    use std::fs;
    use std::time::Duration;

    use tempfile::TempDir;

    use super::*;

    fn limits() -> PersonalWorkspaceLimits {
        PersonalWorkspaceLimits {
            max_file_bytes: 64,
            max_files: 1_000,
            max_total_bytes: 1_024 * 1_024,
            watcher_interval_ms: 10,
        }
    }

    fn workspace(root: &TempDir) -> PersonalWorkspace {
        PersonalWorkspace::open(root.path(), limits(), UtcTimestamp::UNIX_EPOCH).unwrap()
    }

    #[test]
    fn layout_external_watcher_versions_and_context_reload_without_restart() {
        let root = TempDir::new().unwrap();
        let workspace = workspace(&root);
        let layout = workspace.layout();
        for file in [&layout.agent, &layout.user, &layout.rules, &layout.memory] {
            assert!(file.is_file());
        }
        for directory in [
            &layout.daily_memory,
            &layout.state,
            &layout.knowledge,
            &layout.skills,
            &layout.artifacts,
            &layout.backups,
            &layout.metadata,
        ] {
            assert!(directory.is_dir());
        }
        let before = workspace.context_revision().unwrap();
        let watcher = workspace.watch().unwrap();
        fs::write(&layout.memory, "# MEMORY\nhuman edit\n").unwrap();
        let event = (0..20)
            .find_map(|_| watcher.recv_timeout(Duration::from_millis(50)).unwrap())
            .unwrap();
        let WorkspaceEvent::Changed {
            version,
            context_revision,
        } = event
        else {
            panic!("expected a validated change event");
        };
        assert_eq!(version.path, Path::new("MEMORY.md"));
        assert_eq!(version.origin, VersionOrigin::External);
        assert!(context_revision > before);
        watcher.stop().unwrap();
        assert_eq!(workspace.versions("MEMORY.md").unwrap().len(), 2);
        drop(workspace);
        let reopened =
            PersonalWorkspace::open(root.path(), limits(), UtcTimestamp::from_unix_millis(2))
                .unwrap();
        assert_eq!(reopened.context_revision().unwrap(), context_revision);
        assert_eq!(reopened.versions("MEMORY.md").unwrap().len(), 2);
    }

    #[test]
    fn concurrent_human_edit_returns_merge_proposal_without_overwrite() {
        let root = TempDir::new().unwrap();
        let workspace = workspace(&root);
        let memory = workspace.layout().memory;
        let base = fs::read(&memory).unwrap();
        let token = workspace.token("MEMORY.md").unwrap();
        let mut human = base.clone();
        human.extend_from_slice(b"human\n");
        fs::write(&memory, &human).unwrap();
        let mut agent = base;
        agent.extend_from_slice(b"agent\n");
        let outcome = workspace
            .edit(
                WorkspaceActor::Agent,
                "MEMORY.md",
                &token,
                &agent,
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        let EditOutcome::Conflict(proposal) = outcome else {
            panic!("expected a conflict");
        };
        assert_eq!(proposal.current, human);
        assert_eq!(proposal.proposed, agent);
        assert!(proposal.merged.is_some());
        assert_eq!(fs::read(memory).unwrap(), proposal.current);
        assert_eq!(workspace.versions("MEMORY.md").unwrap().len(), 2);
    }

    #[test]
    fn crash_residue_is_removed_without_changing_the_committed_file() {
        let root = TempDir::new().unwrap();
        let workspace = workspace(&root);
        let memory = workspace.layout().memory;
        let committed = fs::read(&memory).unwrap();
        drop(workspace);
        let orphan = root.path().join(".keith-write-crashed.tmp");
        fs::write(&orphan, "uncommitted replacement").unwrap();
        let reopened =
            PersonalWorkspace::open(root.path(), limits(), UtcTimestamp::from_unix_millis(1))
                .unwrap();
        assert!(!orphan.exists());
        assert_eq!(fs::read(reopened.layout().memory).unwrap(), committed);
    }

    #[test]
    #[cfg(unix)]
    fn symlinked_layout_component_is_rejected_before_any_outside_creation() {
        use std::os::unix::fs::symlink;

        let root = TempDir::new().unwrap();
        let outside = TempDir::new().unwrap();
        symlink(outside.path(), root.path().join("memory")).unwrap();
        assert!(matches!(
            PersonalWorkspace::open(root.path(), limits(), UtcTimestamp::UNIX_EPOCH),
            Err(PersonalWorkspaceError::Symlink)
        ));
        assert!(!outside.path().join("daily").exists());
    }

    #[test]
    fn invalid_encoding_large_files_symlinks_and_protected_paths_fail_closed() {
        let root = TempDir::new().unwrap();
        let workspace = workspace(&root);
        let layout = workspace.layout();
        fs::write(&layout.memory, [0xff, 0xfe]).unwrap();
        fs::write(&layout.rules, vec![b'x'; 65]).unwrap();
        #[cfg(unix)]
        {
            use std::os::unix::fs::symlink;
            symlink(&layout.user, layout.knowledge.join("link.md")).unwrap();
        }
        let events = workspace
            .scan_external_changes(UtcTimestamp::from_unix_millis(1))
            .unwrap();
        assert!(events.iter().any(|event| matches!(
            event,
            WorkspaceEvent::Rejected { path, reason }
                if path == Path::new("MEMORY.md") && reason.contains("UTF-8")
        )));
        assert!(events.iter().any(|event| matches!(
            event,
            WorkspaceEvent::Rejected { path, reason }
                if path == Path::new("RULE.md") && reason.contains("limit")
        )));
        #[cfg(unix)]
        assert!(events.iter().any(|event| matches!(
            event,
            WorkspaceEvent::Rejected { path, reason }
                if path == Path::new("knowledge/link.md") && reason.contains("symbolic")
        )));
        assert_eq!(workspace.versions("MEMORY.md").unwrap().len(), 1);
        let token = workspace.token("AGENT.md").unwrap();
        assert!(matches!(
            workspace.edit(
                WorkspaceActor::MemoryTool,
                "AGENT.md",
                &token,
                b"not allowed",
                UtcTimestamp::from_unix_millis(2)
            ),
            Err(PersonalWorkspaceError::Protected)
        ));
        assert!(matches!(
            workspace.edit(
                WorkspaceActor::Agent,
                "backups/overwrite",
                &FileToken {
                    revision: None,
                    digest: None,
                },
                b"not allowed",
                UtcTimestamp::from_unix_millis(2)
            ),
            Err(PersonalWorkspaceError::Protected)
        ));
        assert!(matches!(
            workspace.edit(
                WorkspaceActor::SkillTool,
                ".keith/builtins/core.md",
                &FileToken {
                    revision: None,
                    digest: None,
                },
                b"not allowed",
                UtcTimestamp::from_unix_millis(2)
            ),
            Err(PersonalWorkspaceError::Protected)
        ));
        assert!(matches!(
            workspace.token(".keith/credentials/provider"),
            Err(PersonalWorkspaceError::Protected)
        ));
        assert!(matches!(
            workspace.token("../outside"),
            Err(PersonalWorkspaceError::UnsafePath)
        ));
    }

    #[test]
    fn immutable_backups_snapshot_and_version_restore_round_trip_real_files() {
        let root = TempDir::new().unwrap();
        let workspace = workspace(&root);
        let token = workspace.token("MEMORY.md").unwrap();
        let first = workspace
            .edit(
                WorkspaceActor::Agent,
                "MEMORY.md",
                &token,
                b"# MEMORY\nversion one\n",
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        let EditOutcome::Written(first) = first else {
            panic!("expected a write");
        };
        let snapshot = workspace
            .create_snapshot("known good", UtcTimestamp::from_unix_millis(2))
            .unwrap();
        let snapshot_path = workspace
            .layout()
            .backups
            .join("snapshots")
            .join(format!("{}.json", snapshot.id));
        assert!(
            fs::metadata(&snapshot_path)
                .unwrap()
                .permissions()
                .readonly()
        );
        let token = workspace.token("MEMORY.md").unwrap();
        workspace
            .edit(
                WorkspaceActor::Agent,
                "MEMORY.md",
                &token,
                b"# MEMORY\nversion two\n",
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        let token = workspace.token("knowledge/new.md").unwrap();
        workspace
            .edit(
                WorkspaceActor::Agent,
                "knowledge/new.md",
                &token,
                b"new knowledge",
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        workspace
            .restore_version(
                WorkspaceActor::Human,
                "MEMORY.md",
                first.revision,
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        assert_eq!(
            fs::read(workspace.layout().memory).unwrap(),
            b"# MEMORY\nversion one\n"
        );
        let token = workspace.token("MEMORY.md").unwrap();
        workspace
            .edit(
                WorkspaceActor::Agent,
                "MEMORY.md",
                &token,
                b"# MEMORY\nversion three\n",
                UtcTimestamp::from_unix_millis(5),
            )
            .unwrap();
        workspace
            .restore_snapshot(
                WorkspaceActor::Human,
                &snapshot.id,
                UtcTimestamp::from_unix_millis(6),
            )
            .unwrap();
        assert_eq!(
            fs::read(workspace.layout().memory).unwrap(),
            b"# MEMORY\nversion one\n"
        );
        assert!(!workspace.layout().knowledge.join("new.md").exists());
        assert!(matches!(
            workspace.restore_snapshot(
                WorkspaceActor::MemoryTool,
                &snapshot.id,
                UtcTimestamp::from_unix_millis(7)
            ),
            Err(PersonalWorkspaceError::Protected)
        ));
        assert!(
            workspace
                .versions("MEMORY.md")
                .unwrap()
                .iter()
                .any(|version| version.origin == VersionOrigin::Restore)
        );
    }

    #[test]
    fn artifacts_allow_bounded_binary_content_while_text_files_do_not() {
        let root = TempDir::new().unwrap();
        let workspace = workspace(&root);
        let token = workspace.token("artifacts/image.bin").unwrap();
        assert!(matches!(
            workspace
                .edit(
                    WorkspaceActor::Agent,
                    "artifacts/image.bin",
                    &token,
                    &[0xff, 0x00, 0xfe],
                    UtcTimestamp::from_unix_millis(1)
                )
                .unwrap(),
            EditOutcome::Written(_)
        ));
        let token = workspace.token("state/text.md").unwrap();
        assert!(matches!(
            workspace.edit(
                WorkspaceActor::Agent,
                "state/text.md",
                &token,
                &[0xff, 0xfe],
                UtcTimestamp::from_unix_millis(2)
            ),
            Err(PersonalWorkspaceError::NonUtf8)
        ));
    }
}
