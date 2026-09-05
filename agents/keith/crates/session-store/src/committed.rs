//! Canonical source receipts. These attest stored bytes, not truth or finalization eligibility.

use std::collections::BTreeSet;
use std::fmt;
use std::fs::{self, File, OpenOptions};
use std::io::{Read, Seek, SeekFrom};
use std::path::Path;

use fs2::FileExt;
use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntryId, ProfileId, RootTreeId, SchemaVersion, SessionId, UtcTimestamp,
    WorkspaceId,
};
use serde::{Deserialize, Serialize};

use crate::{
    HISTORY_FILE, MANIFEST_FILE, QUARANTINE_FILE, SessionEntry, SessionEntryPayload,
    SessionManifest, SessionStore, SessionStoreError, SessionWriter, WRITER_LOCK_FILE,
    decode_manifest, parse_complete_history,
};

const MAX_ENTRIES: usize = 4096;
const MAX_BYTES: usize = 16 * 1024 * 1024;
const MAX_MANIFEST_BYTES: u64 = 1024 * 1024;

/// Immediate provenance of a faithfully copied history entry, not independent support.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CommittedSourceReference {
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub entry_id: EntryId,
    pub checksum: String,
}

impl CommittedSourceReference {
    pub(crate) fn validate(&self) -> Result<(), SessionStoreError> {
        if valid_digest(&self.checksum) {
            Ok(())
        } else {
            Err(SessionStoreError::InvalidSourceReference)
        }
    }
}

/// A persisted progress hint, revalidated against canonical history before every read.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CommittedSourceCursor {
    version: SchemaVersion,
    profile_id: ProfileId,
    session_id: SessionId,
    offset: u64,
    anchor_offset: u64,
    anchor_id: EntryId,
    anchor_checksum: String,
    first_checksum: String,
}

impl CommittedSourceCursor {
    pub fn profile_id(&self) -> &ProfileId {
        &self.profile_id
    }

    pub fn session_id(&self) -> &SessionId {
        &self.session_id
    }

    pub fn offset(&self) -> u64 {
        self.offset
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CommittedSourceLimits {
    pub max_entries: usize,
    pub max_bytes: usize,
}

impl Default for CommittedSourceLimits {
    fn default() -> Self {
        Self {
            max_entries: 128,
            max_bytes: 1024 * 1024,
        }
    }
}

impl CommittedSourceLimits {
    fn validate(self) -> Result<(), SessionStoreError> {
        if self.max_entries == 0
            || self.max_entries > MAX_ENTRIES
            || self.max_bytes == 0
            || self.max_bytes > MAX_BYTES
        {
            return Err(SessionStoreError::SourceReadLimit);
        }
        Ok(())
    }
}

#[derive(Clone, Debug)]
struct SourceScope {
    profile_id: ProfileId,
    session_id: SessionId,
    workspace_id: WorkspaceId,
    root_tree_id: RootTreeId,
    active_leaf: Option<EntryId>,
}

impl From<&SessionManifest> for SourceScope {
    fn from(manifest: &SessionManifest) -> Self {
        Self {
            profile_id: manifest.profile_id.clone(),
            session_id: manifest.session_id.clone(),
            workspace_id: manifest.workspace_id.clone(),
            root_tree_id: manifest.root_tree_id.clone(),
            active_leaf: manifest.active_leaf.clone(),
        }
    }
}

/// Only the session store can construct this receipt. It is deliberately not deserializable.
#[derive(Clone)]
pub struct CommittedSourceEntry {
    scope: SourceScope,
    entry: SessionEntry,
}

impl fmt::Debug for CommittedSourceEntry {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CommittedSourceEntry")
            .field("scope", &self.scope)
            .field("entry_id", &self.entry.id)
            .finish_non_exhaustive()
    }
}

impl CommittedSourceEntry {
    pub fn profile_id(&self) -> &ProfileId {
        &self.scope.profile_id
    }
    pub fn session_id(&self) -> &SessionId {
        &self.scope.session_id
    }
    pub fn workspace_id(&self) -> &WorkspaceId {
        &self.scope.workspace_id
    }
    pub fn root_tree_id(&self) -> &RootTreeId {
        &self.scope.root_tree_id
    }
    pub fn entry(&self) -> &SessionEntry {
        &self.entry
    }

    pub fn reference(&self) -> CommittedSourceReference {
        CommittedSourceReference {
            profile_id: self.scope.profile_id.clone(),
            session_id: self.scope.session_id.clone(),
            entry_id: self.entry.id.clone(),
            checksum: self.entry.checksum.clone(),
        }
    }
}

/// An append-order page, including historical branches. No active-branch claim is implied.
#[derive(Clone)]
pub struct CommittedSourcePage {
    scope: SourceScope,
    entries: Vec<SessionEntry>,
    input_cursor: Option<CommittedSourceCursor>,
    next_cursor: Option<CommittedSourceCursor>,
    caught_up: bool,
    bytes_read: usize,
}

impl fmt::Debug for CommittedSourcePage {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CommittedSourcePage")
            .field("scope", &self.scope)
            .field("entry_count", &self.entries.len())
            .field("caught_up", &self.caught_up)
            .field("bytes_read", &self.bytes_read)
            .finish_non_exhaustive()
    }
}

impl CommittedSourcePage {
    pub fn profile_id(&self) -> &ProfileId {
        &self.scope.profile_id
    }
    pub fn session_id(&self) -> &SessionId {
        &self.scope.session_id
    }
    pub fn workspace_id(&self) -> &WorkspaceId {
        &self.scope.workspace_id
    }
    pub fn root_tree_id(&self) -> &RootTreeId {
        &self.scope.root_tree_id
    }
    pub fn active_leaf(&self) -> Option<&EntryId> {
        self.scope.active_leaf.as_ref()
    }
    pub fn entries(&self) -> &[SessionEntry] {
        &self.entries
    }
    pub fn input_cursor(&self) -> Option<&CommittedSourceCursor> {
        self.input_cursor.as_ref()
    }
    pub fn next_cursor(&self) -> Option<&CommittedSourceCursor> {
        self.next_cursor.as_ref()
    }
    pub fn caught_up(&self) -> bool {
        self.caught_up
    }

    /// History bytes read, including at most two bounded checkpoint records on continuation.
    /// The separately bounded manifest (at most 1 MiB) is not included.
    pub fn bytes_read(&self) -> usize {
        self.bytes_read
    }
}

impl SessionStore {
    /// Reads bounded committed source bytes while excluding an active writer.
    ///
    /// # Errors
    /// Rejects wrong scope, busy/quarantined stores, stale cursors, corrupt records and bounds.
    pub fn committed_source_page(
        &self,
        profile_id: &ProfileId,
        session_id: &SessionId,
        cursor: Option<&CommittedSourceCursor>,
        limits: CommittedSourceLimits,
    ) -> Result<CommittedSourcePage, SessionStoreError> {
        let directory = self.session_directory(session_id)?;
        let _guard = source_read_lock(&directory, session_id)?;
        let manifest = source_manifest(&directory, profile_id, session_id)?;
        read_page(&directory, &manifest, cursor, limits)
    }

    /// Looks up an exact source in a bounded tail window. Absence outside that window is unknown.
    ///
    /// # Errors
    /// Returns scope/lock/integrity errors, or `SourceLookupLimit` when the window is insufficient.
    pub fn committed_source_entry(
        &self,
        profile_id: &ProfileId,
        session_id: &SessionId,
        entry_id: &EntryId,
        limits: CommittedSourceLimits,
    ) -> Result<CommittedSourceEntry, SessionStoreError> {
        let directory = self.session_directory(session_id)?;
        let _guard = source_read_lock(&directory, session_id)?;
        let manifest = source_manifest(&directory, profile_id, session_id)?;
        lookup_entry(&directory, &manifest, entry_id, limits)
    }

    /// Attests the existing full-history ancestry operation used for explicit session forks.
    /// This operation loads the existing full index; bounded maintenance must use pages instead.
    ///
    /// # Errors
    /// Rejects wrong scope, busy/quarantined or corrupt history, and an unknown leaf.
    pub fn committed_ancestry(
        &self,
        profile_id: &ProfileId,
        session_id: &SessionId,
        leaf: &EntryId,
    ) -> Result<Vec<CommittedSourceEntry>, SessionStoreError> {
        let directory = self.session_directory(session_id)?;
        let _guard = source_read_lock(&directory, session_id)?;
        let manifest = source_manifest(&directory, profile_id, session_id)?;
        checked_regular(&directory.join(HISTORY_FILE))?;
        parse_complete_history(&directory.join(HISTORY_FILE))?
            .ancestry(leaf)?
            .into_iter()
            .map(|entry| {
                validate_entry_scope(&entry, &manifest)?;
                Ok(CommittedSourceEntry {
                    scope: SourceScope::from(&manifest),
                    entry,
                })
            })
            .collect()
    }
}

impl SessionWriter {
    /// Reads a bounded page under this writer's already-held exclusive lock.
    ///
    /// # Errors
    /// Rejects wrong scope, stale cursors, corrupt records and configured bounds.
    pub fn committed_source_page(
        &self,
        profile_id: &ProfileId,
        cursor: Option<&CommittedSourceCursor>,
        limits: CommittedSourceLimits,
    ) -> Result<CommittedSourcePage, SessionStoreError> {
        let manifest = source_manifest(&self.directory, profile_id, &self.manifest.session_id)?;
        read_page(&self.directory, &manifest, cursor, limits)
    }

    /// Attests an existing entry without re-appending accepted ingress.
    ///
    /// # Errors
    /// Rejects wrong scope, invalid history, or a lookup outside the explicit tail budget.
    pub fn committed_source_entry(
        &self,
        profile_id: &ProfileId,
        entry_id: &EntryId,
        limits: CommittedSourceLimits,
    ) -> Result<CommittedSourceEntry, SessionStoreError> {
        let manifest = source_manifest(&self.directory, profile_id, &self.manifest.session_id)?;
        lookup_entry(&self.directory, &manifest, entry_id, limits)
    }

    /// Appends through the existing durable writer and returns an opaque source receipt.
    ///
    /// # Errors
    /// Returns the existing append validation or durability error.
    pub fn append_committed_source(
        &mut self,
        parent_id: Option<EntryId>,
        timestamp: UtcTimestamp,
        payload: SessionEntryPayload,
    ) -> Result<CommittedSourceEntry, SessionStoreError> {
        let entry = self.append(parent_id, timestamp, payload)?;
        Ok(CommittedSourceEntry {
            scope: SourceScope::from(&self.manifest),
            entry,
        })
    }

    /// Copies only supported context payloads, preserving immediate original attribution.
    /// Neither a caller-supplied replacement claim nor a foreign profile can acquire its origin.
    ///
    /// # Errors
    /// Rejects foreign profile/workspace receipts and any existing append failure.
    pub fn append_source_copy(
        &mut self,
        parent_id: Option<EntryId>,
        source: &CommittedSourceEntry,
    ) -> Result<Option<CommittedSourceEntry>, SessionStoreError> {
        if source.profile_id() != &self.manifest.profile_id
            || source.workspace_id() != &self.manifest.workspace_id
        {
            return Err(SessionStoreError::SourceScopeMismatch);
        }
        let Some(payload) = copied_payload(&source.entry.payload, parent_id.as_ref()) else {
            return Ok(None);
        };
        let entry = self.append_attributed(
            parent_id,
            source.entry.timestamp,
            payload,
            Some(source.reference()),
        )?;
        Ok(Some(CommittedSourceEntry {
            scope: SourceScope::from(&self.manifest),
            entry,
        }))
    }
}

fn copied_payload(
    payload: &SessionEntryPayload,
    parent: Option<&EntryId>,
) -> Option<SessionEntryPayload> {
    match payload {
        SessionEntryPayload::UserMessage { .. }
        | SessionEntryPayload::ToolCall { .. }
        | SessionEntryPayload::ToolResult { .. } => Some(payload.clone()),
        SessionEntryPayload::AssistantMessage { message }
        | SessionEntryPayload::AssistantActivity { message, .. }
        | SessionEntryPayload::AssistantFinal { message, .. } => {
            Some(SessionEntryPayload::AssistantMessage {
                message: message.clone(),
            })
        }
        SessionEntryPayload::Compaction { summary, .. }
        | SessionEntryPayload::CompactionCheckpoint { summary, .. } => {
            parent.map(|entry| SessionEntryPayload::Compaction {
                summary: summary.clone(),
                compacted_through: entry.clone(),
            })
        }
        _ => None,
    }
}

fn checked_regular(path: &Path) -> Result<(), SessionStoreError> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink() || !metadata.is_file() {
        return Err(SessionStoreError::PathEscape);
    }
    Ok(())
}

fn source_read_lock(directory: &Path, session_id: &SessionId) -> Result<File, SessionStoreError> {
    if directory.join(QUARANTINE_FILE).exists() {
        return Err(SessionStoreError::Quarantined(session_id.clone()));
    }
    let path = directory.join(WRITER_LOCK_FILE);
    if fs::symlink_metadata(&path).is_ok() {
        checked_regular(&path)?;
    }
    let file = OpenOptions::new()
        .create(true)
        .truncate(false)
        .read(true)
        .write(true)
        .open(path)?;
    FileExt::try_lock_shared(&file)
        .map_err(|_| SessionStoreError::WriterLocked(session_id.clone()))?;
    Ok(file)
}

fn source_manifest(
    directory: &Path,
    profile: &ProfileId,
    session: &SessionId,
) -> Result<SessionManifest, SessionStoreError> {
    if directory.join(QUARANTINE_FILE).exists() {
        return Err(SessionStoreError::Quarantined(session.clone()));
    }
    let path = directory.join(MANIFEST_FILE);
    checked_regular(&path)?;
    let mut bytes = Vec::new();
    File::open(path)?
        .take(MAX_MANIFEST_BYTES + 1)
        .read_to_end(&mut bytes)?;
    if bytes.len() as u64 > MAX_MANIFEST_BYTES {
        return Err(SessionStoreError::SourceReadLimit);
    }
    let manifest = decode_manifest(&bytes)?;
    if &manifest.profile_id != profile || &manifest.session_id != session {
        return Err(SessionStoreError::SourceScopeMismatch);
    }
    Ok(manifest)
}

fn validate_entry_scope(
    entry: &SessionEntry,
    manifest: &SessionManifest,
) -> Result<(), SessionStoreError> {
    entry.verify()?;
    if entry
        .copied_from
        .as_ref()
        .is_some_and(|source| source.profile_id != manifest.profile_id)
    {
        return Err(SessionStoreError::SourceScopeMismatch);
    }
    Ok(())
}

fn valid_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn read_window(file: &mut File, offset: u64, limit: usize) -> Result<Vec<u8>, SessionStoreError> {
    file.seek(SeekFrom::Start(offset))?;
    let mut bytes = Vec::new();
    file.take(u64::try_from(limit).map_err(|_| SessionStoreError::SourceReadLimit)?)
        .read_to_end(&mut bytes)?;
    Ok(bytes)
}

fn first_record(file: &mut File, limit: usize) -> Result<(SessionEntry, usize), SessionStoreError> {
    file.seek(SeekFrom::Start(0))?;
    let mut bytes = Vec::new();
    while bytes.len() <= limit {
        let mut chunk = [0; 4096];
        let count = file.read(&mut chunk[..4096.min(limit + 1 - bytes.len())])?;
        if count == 0 {
            break;
        }
        let newline = chunk[..count].iter().position(|byte| *byte == b'\n');
        let previous_length = bytes.len();
        bytes.extend_from_slice(&chunk[..count]);
        if let Some(newline) = newline {
            let end = previous_length + newline + 1;
            if end > limit {
                return Err(SessionStoreError::SourceReadLimit);
            }
            let entry: SessionEntry = serde_json::from_slice(&bytes[..end])?;
            entry.verify()?;
            return Ok((entry, bytes.len()));
        }
    }
    Err(SessionStoreError::InvalidSourceCursor)
}

fn validate_cursor(
    file: &mut File,
    manifest: &SessionManifest,
    cursor: &CommittedSourceCursor,
    limits: CommittedSourceLimits,
) -> Result<usize, SessionStoreError> {
    if cursor.version != CURRENT_SCHEMA_VERSION
        || cursor.profile_id != manifest.profile_id
        || cursor.session_id != manifest.session_id
        || cursor.offset <= cursor.anchor_offset
        || cursor.offset > file.metadata()?.len()
        || !valid_digest(&cursor.anchor_checksum)
        || !valid_digest(&cursor.first_checksum)
    {
        return Err(SessionStoreError::InvalidSourceCursor);
    }
    let length = usize::try_from(cursor.offset - cursor.anchor_offset)
        .map_err(|_| SessionStoreError::InvalidSourceCursor)?;
    if length > limits.max_bytes {
        return Err(SessionStoreError::SourceReadLimit);
    }
    let bytes = read_window(file, cursor.anchor_offset, length)?;
    if bytes.len() != length || bytes.last() != Some(&b'\n') {
        return Err(SessionStoreError::InvalidSourceCursor);
    }
    let anchor: SessionEntry =
        serde_json::from_slice(&bytes).map_err(|_| SessionStoreError::InvalidSourceCursor)?;
    anchor
        .verify()
        .map_err(|_| SessionStoreError::InvalidSourceCursor)?;
    if anchor.id != cursor.anchor_id || anchor.checksum != cursor.anchor_checksum {
        return Err(SessionStoreError::InvalidSourceCursor);
    }
    let (first, first_bytes) = first_record(file, limits.max_bytes)?;
    if first.checksum != cursor.first_checksum {
        return Err(SessionStoreError::InvalidSourceCursor);
    }
    Ok(length + first_bytes)
}

fn read_page(
    directory: &Path,
    manifest: &SessionManifest,
    cursor: Option<&CommittedSourceCursor>,
    limits: CommittedSourceLimits,
) -> Result<CommittedSourcePage, SessionStoreError> {
    limits.validate()?;
    checked_regular(&directory.join(HISTORY_FILE))?;
    let mut file = File::open(directory.join(HISTORY_FILE))?;
    let checkpoint_bytes = cursor
        .map(|value| validate_cursor(&mut file, manifest, value, limits))
        .transpose()?
        .unwrap_or(0);
    let offset = cursor.map_or(0, |value| value.offset);
    let bytes = read_window(&mut file, offset, limits.max_bytes + 1)?;
    let file_length = file.metadata()?.len();
    let mut page = CommittedSourcePage {
        scope: SourceScope::from(manifest),
        entries: Vec::new(),
        input_cursor: cursor.cloned(),
        next_cursor: cursor.cloned(),
        caught_up: offset == file_length,
        bytes_read: checkpoint_bytes + bytes.len(),
    };
    let mut position = 0;
    let mut seen = BTreeSet::new();
    let mut first_checksum = cursor.map(|value| value.first_checksum.clone());
    while position < bytes.len() && page.entries.len() < limits.max_entries {
        let remaining = &bytes[position..];
        let Some(newline) = remaining.iter().position(|byte| *byte == b'\n') else {
            if bytes.len() > limits.max_bytes && !page.entries.is_empty() {
                break;
            }
            return Err(SessionStoreError::SourceReadLimit);
        };
        let end = position + newline + 1;
        if end > limits.max_bytes {
            if page.entries.is_empty() {
                return Err(SessionStoreError::SourceReadLimit);
            }
            break;
        }
        let entry: SessionEntry = serde_json::from_slice(&bytes[position..end])?;
        validate_entry_scope(&entry, manifest)?;
        if !seen.insert(entry.id.clone()) {
            return Err(SessionStoreError::DuplicateEntry(entry.id));
        }
        let initial_checksum = first_checksum.get_or_insert_with(|| entry.checksum.clone());
        page.next_cursor = Some(CommittedSourceCursor {
            version: CURRENT_SCHEMA_VERSION,
            profile_id: manifest.profile_id.clone(),
            session_id: manifest.session_id.clone(),
            offset: offset + end as u64,
            anchor_offset: offset + position as u64,
            anchor_id: entry.id.clone(),
            anchor_checksum: entry.checksum.clone(),
            first_checksum: initial_checksum.clone(),
        });
        page.entries.push(entry);
        position = end;
    }
    page.caught_up = offset + position as u64 == file_length;
    Ok(page)
}

fn lookup_entry(
    directory: &Path,
    manifest: &SessionManifest,
    entry_id: &EntryId,
    limits: CommittedSourceLimits,
) -> Result<CommittedSourceEntry, SessionStoreError> {
    limits.validate()?;
    checked_regular(&directory.join(HISTORY_FILE))?;
    let mut file = File::open(directory.join(HISTORY_FILE))?;
    let length = file.metadata()?.len();
    let offset = length.saturating_sub(limits.max_bytes as u64);
    let bytes = read_window(&mut file, offset, limits.max_bytes)?;
    let start = if offset == 0 {
        0
    } else {
        let previous = read_window(&mut file, offset - 1, 1)?;
        if previous == b"\n" {
            0
        } else {
            bytes
                .iter()
                .position(|byte| *byte == b'\n')
                .map_or(bytes.len(), |index| index + 1)
        }
    };
    if !bytes.is_empty() && bytes.last() != Some(&b'\n') {
        return Err(SessionStoreError::SourceReadLimit);
    }
    let mut end = bytes.len();
    for _ in 0..limits.max_entries {
        if end <= start {
            break;
        }
        let position = bytes[start..end - 1]
            .iter()
            .rposition(|byte| *byte == b'\n')
            .map_or(start, |index| start + index + 1);
        let entry: SessionEntry = serde_json::from_slice(&bytes[position..end])?;
        validate_entry_scope(&entry, manifest)?;
        if &entry.id == entry_id {
            return Ok(CommittedSourceEntry {
                scope: SourceScope::from(manifest),
                entry,
            });
        }
        end = position;
    }
    if offset != 0 || end > start {
        Err(SessionStoreError::SourceLookupLimit)
    } else {
        Err(SessionStoreError::MissingEntry(entry_id.clone()))
    }
}
