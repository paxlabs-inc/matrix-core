#![forbid(unsafe_code)]

use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Component, Path, PathBuf};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, ProfileId, SchemaVersion, SessionId, UtcTimestamp,
    canonical_json_bytes,
};
use keith_retrieval::{RetrievalError, RetrievalService};
use keith_state_store::{EmbeddedStore, FileBackupHook, StoreError};
use keith_state_store_core::{
    AtomicStateRepository, Collection, RecordMutation, VersionedRecord, WritePrecondition,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

pub const PORTABLE_FORMAT: &str = "keith-portable-export";
pub const PORTABLE_SCHEMA_VERSION: u16 = 1;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DataDomain {
    Sessions,
    Workspaces,
    Memory,
    Knowledge,
    Skills,
    Artifacts,
    Schedules,
    Commitments,
    Routes,
    ChannelState,
    ToolExperience,
    Credentials,
}

impl DataDomain {
    pub const ALL: [Self; 12] = [
        Self::Sessions,
        Self::Workspaces,
        Self::Memory,
        Self::Knowledge,
        Self::Skills,
        Self::Artifacts,
        Self::Schedules,
        Self::Commitments,
        Self::Routes,
        Self::ChannelState,
        Self::ToolExperience,
        Self::Credentials,
    ];

    const fn collection(self) -> Option<Collection> {
        match self {
            Self::Schedules => Some(Collection::ScheduledJobs),
            Self::Commitments => Some(Collection::Commitments),
            Self::Routes => Some(Collection::RoutingRules),
            Self::ChannelState => Some(Collection::ChannelOffsets),
            Self::ToolExperience => Some(Collection::ToolExperience),
            Self::Sessions
            | Self::Workspaces
            | Self::Memory
            | Self::Knowledge
            | Self::Skills
            | Self::Artifacts
            | Self::Credentials => None,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DataScope {
    pub profile_id: ProfileId,
    pub session_id: Option<SessionId>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PortableFile {
    pub relative_path: String,
    pub sha256: String,
    pub content_hex: String,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PortableExport {
    pub format: String,
    pub schema_version: u16,
    pub product_schema: SchemaVersion,
    pub domain: DataDomain,
    pub scope: DataScope,
    pub exported_at: UtcTimestamp,
    pub files: Vec<PortableFile>,
    pub records: Vec<VersionedRecord>,
}

impl PortableExport {
    /// Serializes the documented standalone JSON representation.
    ///
    /// # Errors
    ///
    /// Returns an error when canonical JSON serialization fails.
    pub fn to_bytes(&self) -> Result<Vec<u8>, DataControlError> {
        Ok(canonical_json_bytes(self)?)
    }

    /// Parses and validates a standalone portable export.
    ///
    /// # Errors
    ///
    /// Returns an error for malformed, unsupported, or internally inconsistent exports.
    pub fn from_bytes(bytes: &[u8]) -> Result<Self, DataControlError> {
        let export: Self = serde_json::from_slice(bytes)?;
        export.validate()?;
        Ok(export)
    }

    fn validate(&self) -> Result<(), DataControlError> {
        if self.format != PORTABLE_FORMAT
            || self.schema_version != PORTABLE_SCHEMA_VERSION
            || self.product_schema.major != CURRENT_SCHEMA_VERSION.major
            || self.product_schema.minor > CURRENT_SCHEMA_VERSION.minor
        {
            return Err(DataControlError::UnsupportedExport);
        }
        if (self.domain.collection().is_some() && !self.files.is_empty())
            || (self.domain.collection().is_none() && !self.records.is_empty())
        {
            return Err(DataControlError::InvalidExport);
        }
        for file in &self.files {
            validate_relative(&file.relative_path)?;
            let content = decode_hex(&file.content_hex)?;
            if digest(&content) != file.sha256 {
                return Err(DataControlError::DigestMismatch(file.relative_path.clone()));
            }
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DataLimits {
    pub max_files: usize,
    pub max_file_bytes: u64,
    pub max_total_bytes: u64,
    pub max_records: usize,
}

impl Default for DataLimits {
    fn default() -> Self {
        Self {
            max_files: 100_000,
            max_file_bytes: 256 * 1_024 * 1_024,
            max_total_bytes: 4 * 1_024 * 1_024 * 1_024,
            max_records: 1_000_000,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DeletionTarget {
    pub domain: DataDomain,
    pub scope: DataScope,
    pub files: Vec<String>,
    pub records: Vec<EntityId>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DeletionPlan {
    pub target: DeletionTarget,
    pub confirmation: String,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct DeletionReport {
    pub deleted_files: usize,
    pub deleted_records: usize,
    pub remaining_files: Vec<String>,
    pub remaining_records: Vec<EntityId>,
}

impl DeletionReport {
    pub fn complete(&self) -> bool {
        self.remaining_files.is_empty() && self.remaining_records.is_empty()
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
#[allow(clippy::struct_excessive_bools)]
pub struct SourceDeletionReport {
    pub source_deleted: bool,
    pub lexical_removed: bool,
    pub trigram_removed: bool,
    pub vector_removed: bool,
    pub cache_removed: bool,
    pub summary_removed: bool,
    pub preview_removed: bool,
    pub remaining: Vec<String>,
}

pub struct DataControl {
    root: PathBuf,
    store: EmbeddedStore,
    limits: DataLimits,
}

impl DataControl {
    /// Opens the owned data root and migrates its transactional state before use.
    ///
    /// # Errors
    ///
    /// Returns an error for an invalid root, limits, or state database.
    pub fn open(root: impl AsRef<Path>, limits: DataLimits) -> Result<Self, DataControlError> {
        if limits.max_files == 0
            || limits.max_file_bytes == 0
            || limits.max_total_bytes == 0
            || limits.max_records == 0
        {
            return Err(DataControlError::InvalidLimits);
        }
        fs::create_dir_all(root.as_ref())?;
        let root = fs::canonicalize(root.as_ref())?;
        let store = EmbeddedStore::open(&root.join("state.sqlite"), Some(&FileBackupHook))?;
        Ok(Self {
            root,
            store,
            limits,
        })
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Exports exactly one domain and scope as standalone, versioned JSON data.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe paths, corrupt records, symlinks, or configured bounds.
    pub fn export(
        &self,
        domain: DataDomain,
        scope: DataScope,
        now: UtcTimestamp,
    ) -> Result<PortableExport, DataControlError> {
        let (files, records) = if let Some(collection) = domain.collection() {
            let records = self.scoped_records(collection, &scope)?;
            if records.len() > self.limits.max_records {
                return Err(DataControlError::LimitExceeded);
            }
            (Vec::new(), records)
        } else {
            (self.export_files(domain, &scope)?, Vec::new())
        };
        let export = PortableExport {
            format: PORTABLE_FORMAT.into(),
            schema_version: PORTABLE_SCHEMA_VERSION,
            product_schema: CURRENT_SCHEMA_VERSION,
            domain,
            scope,
            exported_at: now,
            files,
            records,
        };
        export.validate()?;
        Ok(export)
    }

    /// Restores missing items from one portable export without overwriting current data.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid data, conflicts, unsafe paths, or persistence failure.
    pub fn restore(&self, export: &PortableExport) -> Result<(), DataControlError> {
        export.validate()?;
        if let Some(collection) = export.domain.collection() {
            let mutations = export
                .records
                .iter()
                .cloned()
                .map(|record| RecordMutation::Put {
                    collection,
                    record,
                    precondition: WritePrecondition::Missing,
                })
                .collect::<Vec<_>>();
            if !mutations.is_empty() {
                self.store.transact(&mutations)?;
            }
            return Ok(());
        }
        let base = self.filesystem_scope(export.domain, &export.scope)?;
        fs::create_dir_all(&base)?;
        for file in &export.files {
            let relative = validate_relative(&file.relative_path)?;
            let destination = base.join(relative);
            if let Some(parent) = destination.parent() {
                fs::create_dir_all(parent)?;
            }
            let content = decode_hex(&file.content_hex)?;
            let mut output = OpenOptions::new()
                .create_new(true)
                .write(true)
                .open(&destination)?;
            output.write_all(&content)?;
            output.sync_all()?;
        }
        Ok(())
    }

    /// Builds an immutable exact-scope deletion plan and confirmation digest.
    ///
    /// # Errors
    ///
    /// Returns an error when the scope cannot be enumerated safely.
    pub fn plan_delete(
        &self,
        domain: DataDomain,
        scope: DataScope,
    ) -> Result<DeletionPlan, DataControlError> {
        let (files, records) = if let Some(collection) = domain.collection() {
            (
                Vec::new(),
                self.scoped_records(collection, &scope)?
                    .into_iter()
                    .map(|record| record.id)
                    .collect(),
            )
        } else {
            (
                self.export_files(domain, &scope)?
                    .into_iter()
                    .map(|file| file.relative_path)
                    .collect(),
                Vec::new(),
            )
        };
        let target = DeletionTarget {
            domain,
            scope,
            files,
            records,
        };
        let confirmation = digest(&canonical_json_bytes(&target)?);
        Ok(DeletionPlan {
            target,
            confirmation,
        })
    }

    /// Deletes only targets named by a plan after verifying its confirmation digest.
    ///
    /// Every failed target remains in the returned report so partial deletion is never hidden.
    ///
    /// # Errors
    ///
    /// Returns an error when confirmation is missing/wrong or the plan was altered.
    pub fn delete(
        &self,
        plan: &DeletionPlan,
        confirmation: &str,
    ) -> Result<DeletionReport, DataControlError> {
        let expected = digest(&canonical_json_bytes(&plan.target)?);
        if confirmation != expected || plan.confirmation != expected {
            return Err(DataControlError::ConfirmationRequired(expected));
        }
        let mut report = DeletionReport::default();
        if let Some(collection) = plan.target.domain.collection() {
            for id in &plan.target.records {
                match self.store.transact(&[RecordMutation::Delete {
                    collection,
                    id: id.clone(),
                    precondition: WritePrecondition::Any,
                }]) {
                    Ok(_) => report.deleted_records += 1,
                    Err(_) => report.remaining_records.push(id.clone()),
                }
            }
            return Ok(report);
        }
        let base = self.filesystem_scope(plan.target.domain, &plan.target.scope)?;
        for relative in &plan.target.files {
            let path = base.join(validate_relative(relative)?);
            match fs::remove_file(&path) {
                Ok(()) => report.deleted_files += 1,
                Err(_) => report.remaining_files.push(relative.clone()),
            }
        }
        remove_empty_directories(&base);
        Ok(report)
    }

    /// Deletes one source and every associated derived projection under the same profile.
    ///
    /// # Errors
    ///
    /// Returns an error for an unsafe source, wrong confirmation, or retrieval failure.
    pub fn delete_source(
        &self,
        profile_id: &ProfileId,
        source_root: &Path,
        source_path: &str,
        retrieval: &RetrievalService,
        confirmation: &str,
    ) -> Result<SourceDeletionReport, DataControlError> {
        let relative = validate_relative(source_path)?;
        let source_root = fs::canonicalize(source_root)?;
        let source = source_root.join(relative);
        let expected = source_confirmation(profile_id, source_path);
        if confirmation != expected {
            return Err(DataControlError::ConfirmationRequired(expected));
        }
        let mut report = SourceDeletionReport::default();
        if fs::remove_file(&source).is_ok() {
            report.source_deleted = true;
        } else if source.exists() {
            report.remaining.push("source".into());
        }
        retrieval.purge_source(profile_id, source_path)?;
        report.lexical_removed = true;
        report.trigram_removed = true;
        report.vector_removed = true;
        let key = digest(source_path.as_bytes());
        for (name, completed) in [
            ("cache", &mut report.cache_removed),
            ("summaries", &mut report.summary_removed),
            ("previews", &mut report.preview_removed),
        ] {
            let path = self
                .root
                .join("derived")
                .join(profile_id.to_string())
                .join(name)
                .join(&key);
            if !path.exists() || remove_any(&path).is_ok() {
                *completed = true;
            } else {
                report.remaining.push(name.into());
            }
        }
        Ok(report)
    }

    /// Returns the confirmation digest for one exact source path.
    pub fn source_confirmation(profile_id: &ProfileId, source_path: &str) -> String {
        source_confirmation(profile_id, source_path)
    }

    /// Scans every required domain and returns scopes that still contain data.
    ///
    /// # Errors
    ///
    /// Returns an error when any domain cannot be inspected safely.
    pub fn scan_remnants(&self, scope: &DataScope) -> Result<Vec<DataDomain>, DataControlError> {
        let mut remnants = Vec::new();
        for domain in DataDomain::ALL {
            let present = if let Some(collection) = domain.collection() {
                !self.scoped_records(collection, scope)?.is_empty()
            } else {
                !self.export_files(domain, scope)?.is_empty()
            };
            if present {
                remnants.push(domain);
            }
        }
        Ok(remnants)
    }

    fn scoped_records(
        &self,
        collection: Collection,
        scope: &DataScope,
    ) -> Result<Vec<VersionedRecord>, DataControlError> {
        let records = self
            .store
            .list_records(collection)?
            .into_iter()
            .filter(|record| record_matches_scope(record, scope))
            .collect::<Vec<_>>();
        Ok(records)
    }

    fn export_files(
        &self,
        domain: DataDomain,
        scope: &DataScope,
    ) -> Result<Vec<PortableFile>, DataControlError> {
        let base = self.filesystem_scope(domain, scope)?;
        if !base.exists() {
            return Ok(Vec::new());
        }
        let mut files = Vec::new();
        let mut total = 0_u64;
        collect_files(&base, &base, &mut files, &mut total, self.limits)?;
        files.sort_by(|left, right| left.relative_path.cmp(&right.relative_path));
        Ok(files)
    }

    fn filesystem_scope(
        &self,
        domain: DataDomain,
        scope: &DataScope,
    ) -> Result<PathBuf, DataControlError> {
        let profile = scope.profile_id.to_string();
        let session = || {
            scope
                .session_id
                .as_ref()
                .map(ToString::to_string)
                .ok_or(DataControlError::SessionScopeRequired(domain))
        };
        let relative = match domain {
            DataDomain::Sessions => PathBuf::from("sessions").join(session()?),
            DataDomain::Workspaces => PathBuf::from("workspaces").join(profile).join(session()?),
            DataDomain::Memory => PathBuf::from("profiles").join(profile).join("memory"),
            DataDomain::Knowledge => PathBuf::from("profiles").join(profile).join("knowledge"),
            DataDomain::Skills => PathBuf::from("profiles").join(profile).join("skills"),
            DataDomain::Artifacts => PathBuf::from("artifacts").join(profile).join(session()?),
            DataDomain::Credentials => PathBuf::from("credentials").join(profile),
            DataDomain::Schedules
            | DataDomain::Commitments
            | DataDomain::Routes
            | DataDomain::ChannelState
            | DataDomain::ToolExperience => {
                return Err(DataControlError::NotFilesystemDomain(domain));
            }
        };
        Ok(self.root.join(relative))
    }
}

fn collect_files(
    base: &Path,
    directory: &Path,
    files: &mut Vec<PortableFile>,
    total: &mut u64,
    limits: DataLimits,
) -> Result<(), DataControlError> {
    for entry in fs::read_dir(directory)? {
        let entry = entry?;
        let file_type = entry.file_type()?;
        if file_type.is_symlink() {
            return Err(DataControlError::Symlink(entry.path()));
        }
        if file_type.is_dir() {
            collect_files(base, &entry.path(), files, total, limits)?;
            continue;
        }
        if !file_type.is_file() || files.len() >= limits.max_files {
            return Err(DataControlError::LimitExceeded);
        }
        let metadata = entry.metadata()?;
        if metadata.len() > limits.max_file_bytes {
            return Err(DataControlError::LimitExceeded);
        }
        *total = total
            .checked_add(metadata.len())
            .ok_or(DataControlError::LimitExceeded)?;
        if *total > limits.max_total_bytes {
            return Err(DataControlError::LimitExceeded);
        }
        let content = fs::read(entry.path())?;
        let relative = entry
            .path()
            .strip_prefix(base)
            .map_err(|_| DataControlError::PathEscape)?
            .to_string_lossy()
            .replace('\\', "/");
        files.push(PortableFile {
            relative_path: relative,
            sha256: digest(&content),
            content_hex: encode_hex(&content),
        });
    }
    Ok(())
}

fn record_matches_scope(record: &VersionedRecord, scope: &DataScope) -> bool {
    let profile_id = scope.profile_id.to_string();
    let profile_matches = record
        .payload
        .get("profile_id")
        .and_then(serde_json::Value::as_str)
        .is_some_and(|value| value == profile_id);
    if !profile_matches {
        return false;
    }
    match &scope.session_id {
        Some(session_id) => {
            let session_id = session_id.to_string();
            record
                .payload
                .get("session_id")
                .and_then(serde_json::Value::as_str)
                .is_some_and(|value| value == session_id)
        }
        None => true,
    }
}

fn validate_relative(path: &str) -> Result<PathBuf, DataControlError> {
    let path = Path::new(path);
    if path.as_os_str().is_empty()
        || path.is_absolute()
        || path.components().any(|component| {
            !matches!(component, Component::Normal(_))
                || component.as_os_str().to_string_lossy().contains('\0')
        })
    {
        return Err(DataControlError::PathEscape);
    }
    Ok(path.to_path_buf())
}

fn source_confirmation(profile_id: &ProfileId, source_path: &str) -> String {
    digest(format!("{profile_id}\0{source_path}").as_bytes())
}

fn remove_empty_directories(base: &Path) {
    let mut directories = Vec::new();
    collect_directories(base, &mut directories);
    directories.sort_by_key(|path| std::cmp::Reverse(path.components().count()));
    for directory in directories {
        let _ = fs::remove_dir(directory);
    }
    let _ = fs::remove_dir(base);
}

fn collect_directories(directory: &Path, output: &mut Vec<PathBuf>) {
    let Ok(entries) = fs::read_dir(directory) else {
        return;
    };
    for entry in entries.flatten() {
        if entry.file_type().is_ok_and(|kind| kind.is_dir()) {
            output.push(entry.path());
            collect_directories(&entry.path(), output);
        }
    }
}

fn remove_any(path: &Path) -> std::io::Result<()> {
    if path.is_dir() {
        fs::remove_dir_all(path)
    } else {
        fs::remove_file(path)
    }
}

fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn encode_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut encoded = String::with_capacity(bytes.len().saturating_mul(2));
    for byte in bytes {
        encoded.push(char::from(HEX[usize::from(byte >> 4)]));
        encoded.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    encoded
}

fn decode_hex(value: &str) -> Result<Vec<u8>, DataControlError> {
    if !value.len().is_multiple_of(2) {
        return Err(DataControlError::InvalidExport);
    }
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            let high = hex_digit(pair[0])?;
            let low = hex_digit(pair[1])?;
            Ok((high << 4) | low)
        })
        .collect()
}

fn hex_digit(value: u8) -> Result<u8, DataControlError> {
    match value {
        b'0'..=b'9' => Ok(value - b'0'),
        b'a'..=b'f' => Ok(value - b'a' + 10),
        _ => Err(DataControlError::InvalidExport),
    }
}

#[derive(Debug, Error)]
pub enum DataControlError {
    #[error("data control I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error(transparent)]
    Retrieval(#[from] RetrievalError),
    #[error("portable export JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("portable export uses an unsupported format or schema")]
    UnsupportedExport,
    #[error("portable export is internally inconsistent")]
    InvalidExport,
    #[error("portable file digest does not match: {0}")]
    DigestMismatch(String),
    #[error("data path escaped its owned scope")]
    PathEscape,
    #[error("symlinks are not portable export inputs: {0}")]
    Symlink(PathBuf),
    #[error("data operation exceeded a configured bound")]
    LimitExceeded,
    #[error("data bounds must be non-zero")]
    InvalidLimits,
    #[error("domain {0:?} requires an exact session scope")]
    SessionScopeRequired(DataDomain),
    #[error("domain {0:?} is stored transactionally")]
    NotFilesystemDomain(DataDomain),
    #[error("destructive confirmation required; expected {0}")]
    ConfirmationRequired(String),
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;

    use keith_agent_types::Revision;
    use keith_retrieval::{
        LocalHashEmbedder, MemoryVectorIndex, RankWeights, RetrievalLimits, SearchSourceKind,
        SourceInput, VectorComponents,
    };
    use tempfile::tempdir;

    use super::*;

    fn scope() -> DataScope {
        DataScope {
            profile_id: ProfileId::new(),
            session_id: Some(SessionId::new()),
        }
    }

    fn seed_files(control: &DataControl, scope: &DataScope, marker: &str) {
        for domain in DataDomain::ALL
            .into_iter()
            .filter(|domain| domain.collection().is_none())
        {
            let base = control.filesystem_scope(domain, scope).unwrap();
            fs::create_dir_all(base.join("nested")).unwrap();
            fs::write(base.join("nested/data.bin"), marker.as_bytes()).unwrap();
        }
    }

    fn seed_records(control: &DataControl, scope: &DataScope, marker: &str) {
        let mutations = DataDomain::ALL
            .into_iter()
            .filter_map(|domain| domain.collection().map(|collection| (domain, collection)))
            .map(|(domain, collection)| RecordMutation::Put {
                collection,
                record: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: EntityId::new(),
                    revision: Revision::new(1),
                    updated_at: UtcTimestamp::UNIX_EPOCH,
                    payload: serde_json::json!({
                        "profile_id": scope.profile_id.to_string(),
                        "session_id": scope.session_id.as_ref().unwrap().to_string(),
                        "domain": domain,
                        "marker": marker,
                    }),
                },
                precondition: WritePrecondition::Missing,
            })
            .collect::<Vec<_>>();
        control.store.transact(&mutations).unwrap();
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn complete_lifecycle_exports_restores_deletes_rebuilds_and_isolates() {
        let directory = tempdir().unwrap();
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        let first = scope();
        let second = scope();
        seed_files(&control, &first, "first-profile-private");
        seed_files(&control, &second, "second-profile-private");
        seed_records(&control, &first, "first-record");
        seed_records(&control, &second, "second-record");

        let mut exports = Vec::new();
        for domain in DataDomain::ALL {
            let export = control
                .export(domain, first.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            assert_eq!(export.domain, domain);
            assert_eq!(export.schema_version, PORTABLE_SCHEMA_VERSION);
            let bytes = export.to_bytes().unwrap();
            let generic: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
            assert_eq!(generic["format"], PORTABLE_FORMAT);
            let decoded = PortableExport::from_bytes(&bytes).unwrap();
            assert_eq!(decoded, export);
            assert!(
                !String::from_utf8(bytes)
                    .unwrap()
                    .contains("first-profile-private")
            );
            exports.push(export);
        }

        let session_export = exports
            .iter()
            .find(|export| export.domain == DataDomain::Sessions)
            .unwrap();
        let plan = control
            .plan_delete(DataDomain::Sessions, first.clone())
            .unwrap();
        let report = control.delete(&plan, &plan.confirmation).unwrap();
        assert!(report.complete());
        control.restore(session_export).unwrap();
        assert_eq!(
            fs::read(
                control
                    .filesystem_scope(DataDomain::Sessions, &first)
                    .unwrap()
                    .join("nested/data.bin")
            )
            .unwrap(),
            b"first-profile-private"
        );

        let retrieval = RetrievalService::open(
            directory.path().join("retrieval"),
            RetrievalLimits::default(),
            RankWeights::default(),
            Some(VectorComponents {
                embedder: Arc::new(LocalHashEmbedder::new(32).unwrap()),
                index: Arc::new(MemoryVectorIndex::default()),
            }),
        )
        .unwrap();
        retrieval
            .index_sources(
                &first.profile_id,
                &[SourceInput {
                    source_path: "nested/data.bin".into(),
                    source_version: "1".into(),
                    source_kind: SearchSourceKind::DurableMemory,
                    modified_at: UtcTimestamp::UNIX_EPOCH,
                    text: "first-profile-private searchable".into(),
                }],
            )
            .unwrap();
        let memory_root = control
            .filesystem_scope(DataDomain::Memory, &first)
            .unwrap();
        let key = digest(b"nested/data.bin");
        for kind in ["cache", "summaries", "previews"] {
            let path = directory
                .path()
                .join("derived")
                .join(first.profile_id.to_string())
                .join(kind)
                .join(&key);
            fs::create_dir_all(path.parent().unwrap()).unwrap();
            fs::write(path, b"derived private content").unwrap();
        }
        let confirmation = DataControl::source_confirmation(&first.profile_id, "nested/data.bin");
        let source_report = control
            .delete_source(
                &first.profile_id,
                &memory_root,
                "nested/data.bin",
                &retrieval,
                &confirmation,
            )
            .unwrap();
        assert!(source_report.source_deleted);
        assert!(source_report.lexical_removed);
        assert!(source_report.trigram_removed);
        assert!(source_report.vector_removed);
        assert!(source_report.cache_removed);
        assert!(source_report.summary_removed);
        assert!(source_report.preview_removed);
        assert!(source_report.remaining.is_empty());
        assert!(
            retrieval
                .search(&first.profile_id, "searchable", 10)
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            retrieval
                .rebuild_workspace(
                    &first.profile_id,
                    &memory_root,
                    UtcTimestamp::from_unix_millis(2),
                )
                .unwrap(),
            0
        );
        assert!(
            !retrieval
                .search(&second.profile_id, "searchable", 10)
                .unwrap()
                .iter()
                .any(|result| result.source_path == "nested/data.bin")
        );

        for domain in DataDomain::ALL {
            let plan = control.plan_delete(domain, first.clone()).unwrap();
            let report = control.delete(&plan, &plan.confirmation).unwrap();
            assert!(report.complete(), "{domain:?}: {report:?}");
        }
        assert!(control.scan_remnants(&first).unwrap().is_empty());
        assert_eq!(control.scan_remnants(&second).unwrap(), DataDomain::ALL);
        for domain in DataDomain::ALL {
            let export = control
                .export(domain, second.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            if domain.collection().is_some() {
                assert_eq!(export.records.len(), 1);
            } else {
                assert_eq!(export.files.len(), 1);
                assert_eq!(
                    decode_hex(&export.files[0].content_hex).unwrap(),
                    b"second-profile-private"
                );
            }
        }
    }

    #[test]
    fn destructive_scope_requires_exact_confirmation_and_reports_partial_failure() {
        let directory = tempdir().unwrap();
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        let scope = scope();
        let base = control
            .filesystem_scope(DataDomain::Knowledge, &scope)
            .unwrap();
        fs::create_dir_all(&base).unwrap();
        fs::write(base.join("source.md"), b"knowledge").unwrap();
        let plan = control.plan_delete(DataDomain::Knowledge, scope).unwrap();
        assert!(matches!(
            control.delete(&plan, "wrong"),
            Err(DataControlError::ConfirmationRequired(_))
        ));
        fs::remove_file(base.join("source.md")).unwrap();
        fs::create_dir(base.join("source.md")).unwrap();
        fs::write(base.join("source.md/retained"), b"remaining").unwrap();
        let report = control.delete(&plan, &plan.confirmation).unwrap();
        assert!(!report.complete());
        assert_eq!(report.remaining_files, vec!["source.md"]);
        assert!(base.join("source.md/retained").exists());
    }

    #[test]
    fn export_rejects_symlinks_tampering_and_path_escape() {
        let directory = tempdir().unwrap();
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        let scope = scope();
        let base = control
            .filesystem_scope(DataDomain::Skills, &scope)
            .unwrap();
        fs::create_dir_all(&base).unwrap();
        #[cfg(unix)]
        {
            std::os::unix::fs::symlink(directory.path(), base.join("escape")).unwrap();
            assert!(matches!(
                control.export(DataDomain::Skills, scope.clone(), UtcTimestamp::UNIX_EPOCH),
                Err(DataControlError::Symlink(_))
            ));
            fs::remove_file(base.join("escape")).unwrap();
        }
        fs::write(base.join("skill.md"), b"safe").unwrap();
        let mut export = control
            .export(DataDomain::Skills, scope, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        export.files[0].content_hex = encode_hex(b"tampered");
        assert!(matches!(
            export.validate(),
            Err(DataControlError::DigestMismatch(_))
        ));
        export.files[0].relative_path = "../escape".into();
        assert!(matches!(
            export.validate(),
            Err(DataControlError::PathEscape)
        ));
    }

    #[test]
    fn compressed_archive_bombs_are_rejected_without_expansion() {
        let zip_bomb = [
            b'P', b'K', 3, 4, 20, 0, 0, 0, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff, 0xff, 0xff, 0x7f,
        ];
        let gzip_bomb = [0x1f, 0x8b, 8, 0, 0, 0, 0, 0, 0, 3, 3, 0];
        for bytes in [&zip_bomb[..], &gzip_bomb[..]] {
            assert!(matches!(
                PortableExport::from_bytes(bytes),
                Err(DataControlError::Json(_))
            ));
        }
    }
}
