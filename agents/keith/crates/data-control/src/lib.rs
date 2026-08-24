#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::{Component, Path, PathBuf};

use keith_agent_types::{
    AssignmentId, CURRENT_SCHEMA_VERSION, ComputerId, ConversationId, EntityId, JobId, ProfileId,
    Revision, SchemaVersion, SessionId, StableKey, UtcTimestamp, canonical_json_bytes,
};
use keith_knowledge::SharedKnowledgeSpace;
use keith_retrieval::{RetrievalError, RetrievalService};
use keith_self_evolution::{EvolutionLedgerArchive, corpus_data_inventory, shadow_data_inventory};
use keith_state_store::{EmbeddedStore, FileBackupHook, StoreError};
use keith_state_store_core::{
    AtomicStateRepository, Collection, EvolutionLedgerDataControlRepository, RecordMutation,
    VersionedRecord, WritePrecondition,
};
use keith_supervisor::worker_image_data_inventory;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

pub const PORTABLE_FORMAT: &str = "keith-portable-export";
pub const PORTABLE_SCHEMA_VERSION: u16 = 1;
pub const EVOLUTION_PORTABLE_FORMAT: &str = "keith-evolution-data-export";
pub const EVOLUTION_PORTABLE_SCHEMA_VERSION: u16 = 1;
pub const TEAMMATES_PORTABLE_FORMAT: &str = "keith-teammates-export";
pub const TEAMMATES_PORTABLE_SCHEMA_VERSION: u16 = 1;
const MAX_TEAMMATES_SCOPE_ITEMS: usize = 1_000_000;

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "scope", content = "identity")]
pub enum TeammatesDataScope {
    OwnerWide,
    Profile {
        profile_id: ProfileId,
    },
    Conversation {
        conversation_id: ConversationId,
    },
    Assignment {
        assignment_id: AssignmentId,
    },
    Routine {
        routine_id: JobId,
    },
    SharedKnowledge {
        space_id: EntityId,
    },
    ComputerAudit {
        computer_id: ComputerId,
    },
    BrowserMetadata {
        profile_id: ProfileId,
        computer_id: ComputerId,
    },
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesExportRecord {
    pub collection: Collection,
    pub record: VersionedRecord,
    pub sha256: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesExportFile {
    pub relative_path: String,
    pub byte_length: u64,
    pub sha256: String,
    pub content_hex: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesRetentionNotice {
    pub stable_key: StableKey,
    pub classification: AgentDeleteRecordClassification,
    pub safe_consequence: String,
}

/// Readable standalone export of the canonical teammate state selected by one explicit scope.
/// Credential secret bytes and credential-bearing Chromium files are deliberately excluded;
/// non-secret persistent browser state, browser metadata, and external remnants remain explicit.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesExport {
    pub format: String,
    pub schema_version: u16,
    pub product_schema: SchemaVersion,
    pub scope: TeammatesDataScope,
    pub exported_at: UtcTimestamp,
    pub records: Vec<TeammatesExportRecord>,
    pub files: Vec<TeammatesExportFile>,
    pub retained: Vec<TeammatesRetentionNotice>,
    pub external_remnants: Vec<ExternalRemnant>,
    pub credentials_excluded: bool,
    pub browser_secrets_excluded: bool,
    pub sha256: String,
}

impl TeammatesExport {
    pub fn to_bytes(&self) -> Result<Vec<u8>, DataControlError> {
        self.validate()?;
        Ok(canonical_json_bytes(self)?)
    }

    pub fn from_bytes(bytes: &[u8]) -> Result<Self, DataControlError> {
        let export: Self = serde_json::from_slice(bytes)?;
        export.validate()?;
        Ok(export)
    }

    fn validate(&self) -> Result<(), DataControlError> {
        if self.format != TEAMMATES_PORTABLE_FORMAT
            || self.schema_version != TEAMMATES_PORTABLE_SCHEMA_VERSION
            || self.product_schema.major != CURRENT_SCHEMA_VERSION.major
            || self.product_schema.minor > CURRENT_SCHEMA_VERSION.minor
            || !self.credentials_excluded
            || !self.browser_secrets_excluded
            || self.records.len().saturating_add(self.files.len()) > MAX_TEAMMATES_SCOPE_ITEMS
        {
            return Err(DataControlError::InvalidTeammatesExport);
        }
        let mut record_keys = BTreeSet::new();
        for exported in &self.records {
            if !record_keys.insert((exported.collection, exported.record.id.clone()))
                || exported.record.version != self.product_schema
                || !record_matches_teammates_scope(
                    exported.collection,
                    &exported.record,
                    &self.scope,
                )
                || digest(&canonical_json_bytes(&exported.record)?) != exported.sha256
            {
                return Err(DataControlError::InvalidTeammatesExport);
            }
        }
        let mut paths = BTreeSet::new();
        for file in &self.files {
            validate_relative(&file.relative_path)?;
            let content = decode_hex(&file.content_hex)?;
            if !paths.insert(file.relative_path.clone())
                || u64::try_from(content.len()).ok() != Some(file.byte_length)
                || digest(&content) != file.sha256
                || forbidden_teammates_export_path(&file.relative_path)
                || !export_file_matches_scope(file, &self.files, &self.scope)
            {
                return Err(DataControlError::InvalidTeammatesExport);
            }
        }
        if teammates_export_digest(self)? != self.sha256 {
            return Err(DataControlError::TeammatesExportDigestMismatch);
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesRestore {
    pub scope: TeammatesDataScope,
    pub export_sha256: String,
    pub fresh_root: String,
    pub restored_records: usize,
    pub restored_files: usize,
    pub verified_records: usize,
    pub verified_files: usize,
    pub verified_at: UtcTimestamp,
    pub verification_sha256: String,
}

impl TeammatesRestore {
    pub const fn verified(&self) -> bool {
        self.restored_records == self.verified_records && self.restored_files == self.verified_files
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TeammatesEraseOperation {
    Archive,
    Hide,
    Disable,
    DeleteProfile,
    RemoveCredentials,
    EraseBrowserData,
    FullInstallationErase,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesEraseRecord {
    pub collection: Collection,
    pub id: EntityId,
    pub expected_revision: Revision,
    pub sha256: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesEraseFile {
    pub relative_path: String,
    pub sha256: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesErasePlan {
    pub version: SchemaVersion,
    pub scope: TeammatesDataScope,
    pub operation: TeammatesEraseOperation,
    pub expected_profile_revision: Option<Revision>,
    pub replay_key: StableKey,
    pub records: Vec<TeammatesEraseRecord>,
    pub files: Vec<TeammatesEraseFile>,
    pub retained: Vec<TeammatesRetentionNotice>,
    pub external_remnants: Vec<ExternalRemnant>,
    pub confirmation: String,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesEraseReport {
    pub deleted_records: usize,
    pub deleted_files: usize,
    pub retained: Vec<TeammatesRetentionNotice>,
    pub external_remnants: Vec<ExternalRemnant>,
    pub record_remnants: Vec<TeammatesEraseRecord>,
    pub file_remnants: Vec<TeammatesEraseFile>,
}

impl TeammatesEraseReport {
    pub fn complete(&self) -> bool {
        self.record_remnants.is_empty() && self.file_remnants.is_empty()
    }
}

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

/// Evolution data is owned by the installation, never by a profile or session.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EvolutionDataScope {
    InstallationGlobal,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EvolutionDataDomain {
    Ledger,
    WorkerImages,
    ShadowTrees,
    EvaluationCorpus,
}

impl EvolutionDataDomain {
    pub const ALL: [Self; 4] = [
        Self::Ledger,
        Self::WorkerImages,
        Self::ShadowTrees,
        Self::EvaluationCorpus,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::Ledger => "ledger",
            Self::WorkerImages => "worker-images",
            Self::ShadowTrees => "shadow-trees",
            Self::EvaluationCorpus => "evaluation-corpus",
        }
    }
}

/// Human-readable exact scope carried in every export and deletion plan.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvolutionScopeStatement {
    pub installation_global: bool,
    pub includes: Vec<String>,
    pub excludes: Vec<String>,
    pub deletion_effect: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmbeddedCorpusStatement {
    pub sha256: String,
    pub immutable: bool,
    pub deletable: bool,
    pub reason: String,
}

/// Standalone, versioned, readable evolution-data export.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvolutionPortableExport {
    pub format: String,
    pub schema_version: u16,
    pub product_schema: SchemaVersion,
    pub domain: EvolutionDataDomain,
    pub scope: EvolutionDataScope,
    pub scope_statement: EvolutionScopeStatement,
    pub exported_at: UtcTimestamp,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub ledger: Option<EvolutionLedgerArchive>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub files: Vec<PortableFile>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub embedded_corpus: Option<EmbeddedCorpusStatement>,
}

impl EvolutionPortableExport {
    /// Serializes the documented JSON document without installation-specific tooling.
    ///
    /// # Errors
    /// Returns an error when canonical serialization fails.
    pub fn to_bytes(&self) -> Result<Vec<u8>, DataControlError> {
        Ok(canonical_json_bytes(self)?)
    }

    /// Parses and fully verifies one standalone evolution export.
    ///
    /// # Errors
    /// Returns an error for malformed, unsupported, or internally inconsistent input.
    pub fn from_bytes(bytes: &[u8]) -> Result<Self, DataControlError> {
        let export: Self = serde_json::from_slice(bytes)?;
        export.validate()?;
        Ok(export)
    }

    fn validate(&self) -> Result<(), DataControlError> {
        if self.format != EVOLUTION_PORTABLE_FORMAT
            || self.schema_version != EVOLUTION_PORTABLE_SCHEMA_VERSION
            || self.product_schema.major != CURRENT_SCHEMA_VERSION.major
            || self.product_schema.minor > CURRENT_SCHEMA_VERSION.minor
            || self.scope != EvolutionDataScope::InstallationGlobal
            || !self.scope_statement.installation_global
        {
            return Err(DataControlError::UnsupportedExport);
        }
        let shape_matches = match self.domain {
            EvolutionDataDomain::Ledger => {
                self.ledger.is_some() && self.files.is_empty() && self.embedded_corpus.is_none()
            }
            EvolutionDataDomain::EvaluationCorpus => {
                self.ledger.is_none() && self.embedded_corpus.is_some()
            }
            EvolutionDataDomain::WorkerImages | EvolutionDataDomain::ShadowTrees => {
                self.ledger.is_none() && self.embedded_corpus.is_none()
            }
        };
        if !shape_matches {
            return Err(DataControlError::InvalidExport);
        }
        if let Some(ledger) = &self.ledger {
            ledger
                .verify()
                .map_err(|error| DataControlError::Evolution(error.to_string()))?;
        }
        let mut prior = None;
        for file in &self.files {
            validate_relative(&file.relative_path)?;
            if prior.is_some_and(|value: &str| value >= file.relative_path.as_str()) {
                return Err(DataControlError::InvalidExport);
            }
            let content = decode_hex(&file.content_hex)?;
            if digest(&content) != file.sha256 {
                return Err(DataControlError::DigestMismatch(file.relative_path.clone()));
            }
            prior = Some(file.relative_path.as_str());
        }
        if self.embedded_corpus.as_ref().is_some_and(|embedded| {
            !embedded.immutable || embedded.deletable || embedded.sha256.len() != 64
        }) {
            return Err(DataControlError::InvalidExport);
        }
        Ok(())
    }
}

/// Installation-specific authoritative roots. None of these contain profile/session identity.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EvolutionDataRoots {
    pub worker_image_registry: PathBuf,
    pub shadow_work_root: PathBuf,
    pub runtime_corpus_registry: PathBuf,
    pub derived_root: PathBuf,
}

impl EvolutionDataRoots {
    #[must_use]
    pub fn under(data_root: &Path) -> Self {
        Self {
            worker_image_registry: data_root.join("runtime/worker-images"),
            shadow_work_root: data_root.join("evolution-work"),
            runtime_corpus_registry: data_root.join("self-evolution/corpus"),
            derived_root: data_root.join("derived/evolution"),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvolutionDeletionTarget {
    pub domain: EvolutionDataDomain,
    pub scope: EvolutionDataScope,
    pub scope_statement: EvolutionScopeStatement,
    pub files: Vec<String>,
    pub derived_files: Vec<String>,
    pub ledger_records: usize,
    pub ledger_heads: usize,
    pub embedded_corpus_retained: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EvolutionDeletionPlan {
    pub target: EvolutionDeletionTarget,
    pub confirmation: String,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct EvolutionDeletionReport {
    pub deleted_files: usize,
    pub deleted_derived_files: usize,
    pub deleted_ledger_records: usize,
    pub deleted_ledger_heads: usize,
    pub remaining_paths: Vec<String>,
    pub remaining_ledger_records: usize,
    pub remaining_ledger_heads: usize,
    pub embedded_corpus_retained: bool,
}

impl EvolutionDeletionReport {
    #[must_use]
    pub fn complete(&self) -> bool {
        self.remaining_paths.is_empty()
            && self.remaining_ledger_records == 0
            && self.remaining_ledger_heads == 0
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

const MAX_AGENT_DELETE_ITEMS: usize = 4_096;
const MAX_AGENT_DELETE_DETAIL_BYTES: usize = 2_048;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AgentDeleteOperation {
    Archive,
    Hide,
    Disable,
    DeleteProfile,
    FullInstallationErase,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "disposition")]
pub enum OwnedWorkDisposition {
    Cancel,
    Transfer { to_profile_id: ProfileId },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OwnedWorkRecord {
    pub stable_key: StableKey,
    pub revision: Revision,
    pub disposition: OwnedWorkDisposition,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SharedDataClassification {
    ProfilePrivateDelete,
    ExplicitlySharedRetain,
    ExternallyControlledRetain,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SharedDataRecord {
    pub stable_key: StableKey,
    pub classification: SharedDataClassification,
    pub owner_readable_consequence: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LeaseResourceKind {
    Delivery,
    Assignment,
    Routine,
    ComputerTask,
    ComputerTakeover,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LeaseRevocation {
    pub stable_key: StableKey,
    pub kind: LeaseResourceKind,
    pub expected_revision: Revision,
    pub fencing_token: Option<u64>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ImmutableAuditRetention {
    pub stable_key: StableKey,
    pub policy_reason: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExternalRemnant {
    pub stable_key: StableKey,
    pub controller: String,
    pub owner_action: String,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AgentDeleteDomain {
    SessionsPrivateContext,
    Artifacts,
    RetrievalSourceAccess,
    KnowledgeProjectionsCaches,
    Attention,
    Awareness,
    Refinement,
    Credentials,
    PendingActions,
    Schedules,
    Computer,
    Workspace,
    Memory,
    SharedKnowledgeGrants,
    SharedKnowledgeSpaces,
}

impl AgentDeleteDomain {
    pub const MANDATORY: [Self; 15] = [
        Self::SessionsPrivateContext,
        Self::Artifacts,
        Self::RetrievalSourceAccess,
        Self::KnowledgeProjectionsCaches,
        Self::Attention,
        Self::Awareness,
        Self::Refinement,
        Self::Credentials,
        Self::PendingActions,
        Self::Schedules,
        Self::Computer,
        Self::Workspace,
        Self::Memory,
        Self::SharedKnowledgeGrants,
        Self::SharedKnowledgeSpaces,
    ];
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AgentDeleteRecordClassification {
    DeletePrivate,
    RetainShared,
    RetainImmutableAudit,
    ExternalRemnant,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteDiscoveredRecord {
    pub stable_key: StableKey,
    pub classification: AgentDeleteRecordClassification,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "support")]
pub enum AgentDeleteDomainSupport {
    Supported {
        records: Vec<AgentDeleteDiscoveredRecord>,
    },
    Unsupported {
        safe_reason: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteDomainDiscovery {
    pub domain: AgentDeleteDomain,
    pub support: AgentDeleteDomainSupport,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SharedKnowledgeSpaceDeleteDisposition {
    TombstoneOwnedPrivate,
    RetainOwnedShared,
    RemoveMembership,
    RetainTombstone,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SharedKnowledgeSpaceDeleteRecord {
    pub stable_key: StableKey,
    pub space_id: EntityId,
    pub expected_permission_revision: Revision,
    pub disposition: SharedKnowledgeSpaceDeleteDisposition,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SharedKnowledgeSpaceDeleteInventory {
    pub profile_id: ProfileId,
    pub records: Vec<SharedKnowledgeSpaceDeleteRecord>,
    pub discovery: AgentDeleteDomainDiscovery,
    pub shared_data: Vec<SharedDataRecord>,
    pub retained_audit: Vec<ImmutableAuditRetention>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SharedKnowledgeSpaceDeleteOutcome {
    Applied,
    Replay,
    Retained,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteInventory {
    pub profile_id: ProfileId,
    pub expected_revision: Revision,
    pub operation: AgentDeleteOperation,
    pub private_resources: Vec<StableKey>,
    pub owned_work: Vec<OwnedWorkRecord>,
    pub shared_data: Vec<SharedDataRecord>,
    pub lease_revocations: Vec<LeaseRevocation>,
    pub retained_audit: Vec<ImmutableAuditRetention>,
    pub external_remnants: Vec<ExternalRemnant>,
    pub domain_discovery: Vec<AgentDeleteDomainDiscovery>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeletePlan {
    pub version: SchemaVersion,
    pub profile_id: ProfileId,
    pub expected_revision: Revision,
    pub operation: AgentDeleteOperation,
    pub replay_key: StableKey,
    pub private_resources: Vec<StableKey>,
    pub owned_work: Vec<OwnedWorkRecord>,
    pub shared_data: Vec<SharedDataRecord>,
    pub lease_revocations: Vec<LeaseRevocation>,
    pub retained_audit: Vec<ImmutableAuditRetention>,
    pub external_remnants: Vec<ExternalRemnant>,
    pub domain_discovery: Vec<AgentDeleteDomainDiscovery>,
    pub confirmation: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct AgentDeleteTarget {
    version: SchemaVersion,
    profile_id: ProfileId,
    expected_revision: Revision,
    operation: AgentDeleteOperation,
    replay_key: StableKey,
    private_resources: Vec<StableKey>,
    owned_work: Vec<OwnedWorkRecord>,
    shared_data: Vec<SharedDataRecord>,
    lease_revocations: Vec<LeaseRevocation>,
    retained_audit: Vec<ImmutableAuditRetention>,
    external_remnants: Vec<ExternalRemnant>,
    domain_discovery: Vec<AgentDeleteDomainDiscovery>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteReceipt {
    pub version: SchemaVersion,
    pub profile_id: ProfileId,
    pub expected_revision: Revision,
    pub replay_key: StableKey,
    pub plan_digest: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AgentDeleteExecutionContext {
    pub current_revision: Revision,
    pub prior_receipt: Option<AgentDeleteReceipt>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AgentDeleteExecutionStatus {
    Executed,
    Duplicate,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AgentDeleteReport {
    pub status: AgentDeleteExecutionStatus,
    pub receipt: AgentDeleteReceipt,
    pub private_resources_to_delete: Vec<StableKey>,
    pub owned_work: Vec<OwnedWorkRecord>,
    pub lease_revocations: Vec<LeaseRevocation>,
    pub private_shared_data_to_delete: Vec<SharedDataRecord>,
    pub retained_shared_data: Vec<SharedDataRecord>,
    pub retained_audit: Vec<ImmutableAuditRetention>,
    pub externally_controlled_remnants: Vec<ExternalRemnant>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AgentDeleteSagaState {
    Planned,
    Executing,
    TerminalTombstoned,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AgentDeleteStepKind {
    PrivateResourceDelete,
    OwnedWorkDisposition,
    LeaseRevocation,
    PrivateSharedDataDelete,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "state")]
pub enum AgentDeleteStepState {
    Pending,
    Applied,
    Remnant { safe_detail: String },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteResourceStep {
    pub stable_key: StableKey,
    pub kind: AgentDeleteStepKind,
    pub state: AgentDeleteStepState,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteDirectives {
    pub private_resources: Vec<StableKey>,
    pub owned_work: Vec<OwnedWorkRecord>,
    pub lease_revocations: Vec<LeaseRevocation>,
    pub private_shared_data: Vec<SharedDataRecord>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "result")]
pub enum AgentDeleteDomainLeakResult {
    Clean,
    Retained {
        records: Vec<AgentDeleteDiscoveredRecord>,
    },
    Leak {
        stable_keys: Vec<StableKey>,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteDomainLeakScan {
    pub domain: AgentDeleteDomain,
    pub result: AgentDeleteDomainLeakResult,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteLeakScan {
    pub version: SchemaVersion,
    pub replay_key: StableKey,
    pub domains: Vec<AgentDeleteDomainLeakScan>,
    pub scanned_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DurableAgentDeleteOperation {
    pub version: SchemaVersion,
    pub profile_id: ProfileId,
    pub replay_key: StableKey,
    pub plan_digest: String,
    pub expected_profile_revision: Revision,
    pub state: AgentDeleteSagaState,
    pub steps: Vec<AgentDeleteResourceStep>,
    pub directives: AgentDeleteDirectives,
    pub domain_discovery: Vec<AgentDeleteDomainDiscovery>,
    pub retained_shared_data: Vec<SharedDataRecord>,
    pub retained_audit: Vec<ImmutableAuditRetention>,
    pub external_remnants: Vec<ExternalRemnant>,
    pub leak_scan: Option<AgentDeleteLeakScan>,
    pub receipt: Option<AgentDeleteReceipt>,
    pub revision: Revision,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
#[allow(clippy::struct_excessive_bools)]
pub struct AgentDeleteTombstoneProof {
    pub profile_id: ProfileId,
    pub replay_key: StableKey,
    pub operation_revision: Revision,
    pub private_resources_terminal: bool,
    pub owned_work_terminal: bool,
    pub leases_terminal: bool,
    pub private_shared_data_terminal: bool,
    pub immutable_receipt_present: bool,
}

impl DurableAgentDeleteOperation {
    pub const fn profile_removal_authorized(&self) -> bool {
        matches!(self.state, AgentDeleteSagaState::TerminalTombstoned)
    }

    pub fn remaining_steps(&self) -> usize {
        self.steps
            .iter()
            .filter(|step| matches!(step.state, AgentDeleteStepState::Pending))
            .count()
    }

    pub fn tombstone_proof(&self) -> Option<AgentDeleteTombstoneProof> {
        if !self.profile_removal_authorized() {
            return None;
        }
        let terminal = |kind| {
            self.steps
                .iter()
                .filter(|step| step.kind == kind)
                .all(|step| !matches!(step.state, AgentDeleteStepState::Pending))
        };
        Some(AgentDeleteTombstoneProof {
            profile_id: self.profile_id.clone(),
            replay_key: self.replay_key.clone(),
            operation_revision: self.revision,
            private_resources_terminal: terminal(AgentDeleteStepKind::PrivateResourceDelete),
            owned_work_terminal: terminal(AgentDeleteStepKind::OwnedWorkDisposition),
            leases_terminal: terminal(AgentDeleteStepKind::LeaseRevocation),
            private_shared_data_terminal: terminal(AgentDeleteStepKind::PrivateSharedDataDelete),
            immutable_receipt_present: self.receipt.is_some(),
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteAuditRecord {
    pub version: SchemaVersion,
    pub profile_id: ProfileId,
    pub replay_key: StableKey,
    pub plan_digest: String,
    pub terminal_revision: Revision,
    pub applied_steps: usize,
    pub remnant_steps: usize,
    pub retained_shared_data: usize,
    pub retained_audit: usize,
    pub external_remnants: usize,
    pub occurred_at: UtcTimestamp,
}

impl AgentDeletePlan {
    /// Creates an owner-confirmable, replay-stable profile deletion plan.
    ///
    /// # Errors
    /// Returns an error for non-delete scopes, unsafe transfers, duplicate keys, or bounds.
    pub fn build(mut inventory: AgentDeleteInventory) -> Result<Self, DataControlError> {
        validate_agent_delete_inventory(&inventory)?;
        inventory.private_resources.sort();
        inventory
            .owned_work
            .sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        inventory
            .shared_data
            .sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        inventory
            .lease_revocations
            .sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        inventory
            .retained_audit
            .sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        inventory
            .external_remnants
            .sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        inventory
            .domain_discovery
            .sort_by_key(|discovery| discovery.domain);
        let replay_key = StableKey::parse(format!(
            "agent-delete:{}/{}",
            inventory.profile_id,
            inventory.expected_revision.get()
        ))
        .map_err(|_| DataControlError::InvalidAgentDeletePlan)?;
        let target = AgentDeleteTarget {
            version: CURRENT_SCHEMA_VERSION,
            profile_id: inventory.profile_id,
            expected_revision: inventory.expected_revision,
            operation: inventory.operation,
            replay_key,
            private_resources: inventory.private_resources,
            owned_work: inventory.owned_work,
            shared_data: inventory.shared_data,
            lease_revocations: inventory.lease_revocations,
            retained_audit: inventory.retained_audit,
            external_remnants: inventory.external_remnants,
            domain_discovery: inventory.domain_discovery,
        };
        let confirmation = digest(&canonical_json_bytes(&target)?);
        Ok(Self {
            version: target.version,
            profile_id: target.profile_id,
            expected_revision: target.expected_revision,
            operation: target.operation,
            replay_key: target.replay_key,
            private_resources: target.private_resources,
            owned_work: target.owned_work,
            shared_data: target.shared_data,
            lease_revocations: target.lease_revocations,
            retained_audit: target.retained_audit,
            external_remnants: target.external_remnants,
            domain_discovery: target.domain_discovery,
            confirmation,
        })
    }
}

/// Validates owner confirmation and revision, then returns exact directives and remnant reporting.
///
/// The owning runtime must execute the returned resource, work, and lease directives atomically
/// with profile removal and persist the receipt plus immutable delete audit.
///
/// # Errors
/// Returns an error for plan tampering, stale revisions, unsafe scope, or mismatched replay.
pub fn execute_agent_delete(
    plan: &AgentDeletePlan,
    confirmation: &str,
    context: AgentDeleteExecutionContext,
) -> Result<AgentDeleteReport, DataControlError> {
    let target = agent_delete_target(plan);
    validate_agent_delete_target(&target)?;
    let plan_digest = digest(&canonical_json_bytes(&target)?);
    if confirmation != plan_digest || plan.confirmation != plan_digest {
        return Err(DataControlError::ConfirmationRequired(plan_digest));
    }
    let receipt = AgentDeleteReceipt {
        version: CURRENT_SCHEMA_VERSION,
        profile_id: plan.profile_id.clone(),
        expected_revision: plan.expected_revision,
        replay_key: plan.replay_key.clone(),
        plan_digest,
    };
    let status = if let Some(prior) = context.prior_receipt {
        if prior != receipt {
            return Err(DataControlError::AgentDeleteReplayConflict);
        }
        AgentDeleteExecutionStatus::Duplicate
    } else {
        if context.current_revision != plan.expected_revision {
            return Err(DataControlError::AgentDeleteRevisionConflict {
                expected: plan.expected_revision,
                actual: context.current_revision,
            });
        }
        AgentDeleteExecutionStatus::Executed
    };
    Ok(AgentDeleteReport {
        status,
        receipt,
        private_resources_to_delete: plan.private_resources.clone(),
        owned_work: plan.owned_work.clone(),
        lease_revocations: plan.lease_revocations.clone(),
        private_shared_data_to_delete: plan
            .shared_data
            .iter()
            .filter(|record| {
                record.classification == SharedDataClassification::ProfilePrivateDelete
            })
            .cloned()
            .collect(),
        retained_shared_data: plan
            .shared_data
            .iter()
            .filter(|record| {
                record.classification != SharedDataClassification::ProfilePrivateDelete
            })
            .cloned()
            .collect(),
        retained_audit: plan.retained_audit.clone(),
        externally_controlled_remnants: plan.external_remnants.clone(),
    })
}

fn agent_delete_target(plan: &AgentDeletePlan) -> AgentDeleteTarget {
    AgentDeleteTarget {
        version: plan.version,
        profile_id: plan.profile_id.clone(),
        expected_revision: plan.expected_revision,
        operation: plan.operation,
        replay_key: plan.replay_key.clone(),
        private_resources: plan.private_resources.clone(),
        owned_work: plan.owned_work.clone(),
        shared_data: plan.shared_data.clone(),
        lease_revocations: plan.lease_revocations.clone(),
        retained_audit: plan.retained_audit.clone(),
        external_remnants: plan.external_remnants.clone(),
        domain_discovery: plan.domain_discovery.clone(),
    }
}

fn validate_agent_delete_inventory(
    inventory: &AgentDeleteInventory,
) -> Result<(), DataControlError> {
    let target = AgentDeleteTarget {
        version: CURRENT_SCHEMA_VERSION,
        profile_id: inventory.profile_id.clone(),
        expected_revision: inventory.expected_revision,
        operation: inventory.operation,
        replay_key: StableKey::parse("agent-delete:validation")
            .map_err(|_| DataControlError::InvalidAgentDeletePlan)?,
        private_resources: inventory.private_resources.clone(),
        owned_work: inventory.owned_work.clone(),
        shared_data: inventory.shared_data.clone(),
        lease_revocations: inventory.lease_revocations.clone(),
        retained_audit: inventory.retained_audit.clone(),
        external_remnants: inventory.external_remnants.clone(),
        domain_discovery: inventory.domain_discovery.clone(),
    };
    validate_agent_delete_target(&target)
}

fn validate_agent_delete_target(target: &AgentDeleteTarget) -> Result<(), DataControlError> {
    if target.version != CURRENT_SCHEMA_VERSION
        || target.operation != AgentDeleteOperation::DeleteProfile
    {
        return Err(DataControlError::InvalidAgentDeleteScope);
    }
    validate_agent_delete_discovery(&target.domain_discovery)?;
    for length in [
        target.private_resources.len(),
        target.owned_work.len(),
        target.shared_data.len(),
        target.lease_revocations.len(),
        target.retained_audit.len(),
        target.external_remnants.len(),
    ] {
        if length > MAX_AGENT_DELETE_ITEMS {
            return Err(DataControlError::LimitExceeded);
        }
    }
    for work in &target.owned_work {
        if matches!(
            &work.disposition,
            OwnedWorkDisposition::Transfer { to_profile_id }
                if to_profile_id == &target.profile_id
        ) {
            return Err(DataControlError::InvalidAgentDeletePlan);
        }
    }
    for lease in &target.lease_revocations {
        if lease.fencing_token.is_some_and(|fence| fence == 0) {
            return Err(DataControlError::InvalidAgentDeletePlan);
        }
    }
    let valid_detail = |value: &str| {
        !value.trim().is_empty()
            && value.len() <= MAX_AGENT_DELETE_DETAIL_BYTES
            && !value.contains('\0')
    };
    if target
        .shared_data
        .iter()
        .any(|record| !valid_detail(&record.owner_readable_consequence))
        || target
            .retained_audit
            .iter()
            .any(|record| !valid_detail(&record.policy_reason))
        || target
            .external_remnants
            .iter()
            .any(|record| !valid_detail(&record.controller) || !valid_detail(&record.owner_action))
    {
        return Err(DataControlError::InvalidAgentDeletePlan);
    }
    let mut keys = BTreeSet::new();
    let all_keys = target
        .private_resources
        .iter()
        .chain(target.owned_work.iter().map(|record| &record.stable_key))
        .chain(target.shared_data.iter().map(|record| &record.stable_key))
        .chain(
            target
                .lease_revocations
                .iter()
                .map(|record| &record.stable_key),
        )
        .chain(
            target
                .retained_audit
                .iter()
                .map(|record| &record.stable_key),
        )
        .chain(
            target
                .external_remnants
                .iter()
                .map(|record| &record.stable_key),
        );
    if !all_keys.into_iter().all(|key| keys.insert(key)) {
        return Err(DataControlError::InvalidAgentDeletePlan);
    }
    Ok(())
}

fn validate_agent_delete_discovery(
    discoveries: &[AgentDeleteDomainDiscovery],
) -> Result<(), DataControlError> {
    if discoveries.len() != AgentDeleteDomain::MANDATORY.len() {
        return Err(DataControlError::AgentDeleteInventoryIncomplete);
    }
    let mut domains = BTreeSet::new();
    let mut keys = BTreeSet::new();
    for discovery in discoveries {
        if !domains.insert(discovery.domain) {
            return Err(DataControlError::AgentDeleteInventoryIncomplete);
        }
        match &discovery.support {
            AgentDeleteDomainSupport::Supported { records } => {
                if records.len() > MAX_AGENT_DELETE_ITEMS
                    || records
                        .iter()
                        .any(|record| !keys.insert(&record.stable_key))
                {
                    return Err(DataControlError::InvalidAgentDeletePlan);
                }
            }
            AgentDeleteDomainSupport::Unsupported { safe_reason } => {
                if safe_reason.trim().is_empty()
                    || safe_reason.len() > MAX_AGENT_DELETE_DETAIL_BYTES
                    || safe_reason.contains('\0')
                {
                    return Err(DataControlError::InvalidAgentDeletePlan);
                }
            }
        }
    }
    if AgentDeleteDomain::MANDATORY
        .iter()
        .any(|domain| !domains.contains(domain))
    {
        return Err(DataControlError::AgentDeleteInventoryIncomplete);
    }
    Ok(())
}

fn agent_delete_inventory_supported(discoveries: &[AgentDeleteDomainDiscovery]) -> bool {
    discoveries.iter().all(|discovery| {
        matches!(
            discovery.support,
            AgentDeleteDomainSupport::Supported { .. }
        )
    })
}

fn replace_agent_delete_stable_key(
    operation: &mut DurableAgentDeleteOperation,
    old: &StableKey,
    new: &StableKey,
) {
    for step in &mut operation.steps {
        if &step.stable_key == old {
            step.stable_key = new.clone();
        }
    }
    for key in &mut operation.directives.private_resources {
        if key == old {
            *key = new.clone();
        }
    }
    for record in &mut operation.directives.owned_work {
        if &record.stable_key == old {
            record.stable_key = new.clone();
        }
    }
    for record in &mut operation.directives.lease_revocations {
        if &record.stable_key == old {
            record.stable_key = new.clone();
        }
    }
    for record in &mut operation.directives.private_shared_data {
        if &record.stable_key == old {
            record.stable_key = new.clone();
        }
    }
    for record in &mut operation.retained_shared_data {
        if &record.stable_key == old {
            record.stable_key = new.clone();
        }
    }
    for record in &mut operation.retained_audit {
        if &record.stable_key == old {
            record.stable_key = new.clone();
        }
    }
    for record in &mut operation.external_remnants {
        if &record.stable_key == old {
            record.stable_key = new.clone();
        }
    }
}

fn agent_delete_steps(
    report: &AgentDeleteReport,
) -> Result<Vec<AgentDeleteResourceStep>, DataControlError> {
    let mut steps = Vec::new();
    steps.extend(
        report
            .private_resources_to_delete
            .iter()
            .cloned()
            .map(|stable_key| AgentDeleteResourceStep {
                stable_key,
                kind: AgentDeleteStepKind::PrivateResourceDelete,
                state: AgentDeleteStepState::Pending,
            }),
    );
    steps.extend(
        report
            .owned_work
            .iter()
            .map(|record| AgentDeleteResourceStep {
                stable_key: record.stable_key.clone(),
                kind: AgentDeleteStepKind::OwnedWorkDisposition,
                state: AgentDeleteStepState::Pending,
            }),
    );
    steps.extend(
        report
            .lease_revocations
            .iter()
            .map(|record| AgentDeleteResourceStep {
                stable_key: record.stable_key.clone(),
                kind: AgentDeleteStepKind::LeaseRevocation,
                state: AgentDeleteStepState::Pending,
            }),
    );
    steps.extend(report.private_shared_data_to_delete.iter().map(|record| {
        AgentDeleteResourceStep {
            stable_key: record.stable_key.clone(),
            kind: AgentDeleteStepKind::PrivateSharedDataDelete,
            state: AgentDeleteStepState::Pending,
        }
    }));
    steps.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
    if steps.len() > MAX_AGENT_DELETE_ITEMS
        || steps
            .windows(2)
            .any(|pair| pair[0].stable_key == pair[1].stable_key)
    {
        return Err(DataControlError::InvalidAgentDeletePlan);
    }
    Ok(steps)
}

fn validate_agent_delete_step_state(state: &AgentDeleteStepState) -> Result<(), DataControlError> {
    if let AgentDeleteStepState::Remnant { safe_detail } = state
        && (safe_detail.trim().is_empty()
            || safe_detail.len() > MAX_AGENT_DELETE_DETAIL_BYTES
            || safe_detail.contains('\0'))
    {
        return Err(DataControlError::InvalidAgentDeletePlan);
    }
    Ok(())
}

fn validate_durable_agent_delete(
    operation: &DurableAgentDeleteOperation,
) -> Result<(), DataControlError> {
    if operation.version != CURRENT_SCHEMA_VERSION
        || operation.steps.len() > MAX_AGENT_DELETE_ITEMS
        || operation.updated_at < operation.created_at
    {
        return Err(DataControlError::InvalidAgentDeletePlan);
    }
    validate_agent_delete_discovery(&operation.domain_discovery)?;
    if let Some(scan) = &operation.leak_scan {
        validate_agent_delete_leak_scan(scan, &operation.replay_key)?;
    }
    let mut keys = BTreeSet::new();
    for step in &operation.steps {
        validate_agent_delete_step_state(&step.state)?;
        if !keys.insert(&step.stable_key) {
            return Err(DataControlError::InvalidAgentDeletePlan);
        }
    }
    let mut directive_keys = Vec::new();
    directive_keys.extend(
        operation
            .directives
            .private_resources
            .iter()
            .cloned()
            .map(|key| (key, AgentDeleteStepKind::PrivateResourceDelete)),
    );
    directive_keys.extend(operation.directives.owned_work.iter().map(|record| {
        (
            record.stable_key.clone(),
            AgentDeleteStepKind::OwnedWorkDisposition,
        )
    }));
    directive_keys.extend(operation.directives.lease_revocations.iter().map(|record| {
        (
            record.stable_key.clone(),
            AgentDeleteStepKind::LeaseRevocation,
        )
    }));
    directive_keys.extend(
        operation
            .directives
            .private_shared_data
            .iter()
            .map(|record| {
                (
                    record.stable_key.clone(),
                    AgentDeleteStepKind::PrivateSharedDataDelete,
                )
            }),
    );
    directive_keys.sort();
    let mut step_keys = operation
        .steps
        .iter()
        .map(|step| (step.stable_key.clone(), step.kind))
        .collect::<Vec<_>>();
    step_keys.sort();
    if directive_keys != step_keys {
        return Err(DataControlError::InvalidAgentDeletePlan);
    }
    let terminal = operation.state == AgentDeleteSagaState::TerminalTombstoned;
    if terminal != operation.receipt.is_some()
        || terminal && operation.leak_scan.is_none()
        || terminal && operation.remaining_steps() != 0
        || operation.receipt.as_ref().is_some_and(|receipt| {
            receipt.version != operation.version
                || receipt.profile_id != operation.profile_id
                || receipt.replay_key != operation.replay_key
                || receipt.plan_digest != operation.plan_digest
                || receipt.expected_revision != operation.expected_profile_revision
        })
    {
        return Err(DataControlError::InvalidAgentDeletePlan);
    }
    Ok(())
}

fn validate_agent_delete_leak_scan(
    scan: &AgentDeleteLeakScan,
    replay_key: &StableKey,
) -> Result<(), DataControlError> {
    if scan.version != CURRENT_SCHEMA_VERSION
        || &scan.replay_key != replay_key
        || scan.domains.len() != AgentDeleteDomain::MANDATORY.len()
    {
        return Err(DataControlError::AgentDeleteInventoryIncomplete);
    }
    let mut domains = BTreeSet::new();
    for result in &scan.domains {
        if !domains.insert(result.domain) {
            return Err(DataControlError::AgentDeleteInventoryIncomplete);
        }
        match &result.result {
            AgentDeleteDomainLeakResult::Clean => {}
            AgentDeleteDomainLeakResult::Retained { records } => {
                if records.is_empty()
                    || records.len() > MAX_AGENT_DELETE_ITEMS
                    || records.iter().any(|record| {
                        record.classification == AgentDeleteRecordClassification::DeletePrivate
                    })
                {
                    return Err(DataControlError::InvalidAgentDeletePlan);
                }
            }
            AgentDeleteDomainLeakResult::Leak { stable_keys } => {
                if stable_keys.is_empty() || stable_keys.len() > MAX_AGENT_DELETE_ITEMS {
                    return Err(DataControlError::InvalidAgentDeletePlan);
                }
            }
        }
    }
    if AgentDeleteDomain::MANDATORY
        .iter()
        .any(|domain| !domains.contains(domain))
    {
        return Err(DataControlError::AgentDeleteInventoryIncomplete);
    }
    Ok(())
}

fn check_agent_delete_revision(
    expected: Revision,
    actual: Revision,
) -> Result<(), DataControlError> {
    if expected == actual {
        Ok(())
    } else {
        Err(DataControlError::AgentDeleteRevisionConflict { expected, actual })
    }
}

fn agent_delete_record_id(kind: &str, replay_key: &StableKey) -> EntityId {
    let bytes = Sha256::digest(format!("agent-delete:{kind}:{replay_key}").as_bytes());
    let mut raw = [0_u8; 16];
    raw.copy_from_slice(&bytes[..16]);
    EntityId::from_u128(u128::from_be_bytes(raw))
}

fn agent_delete_versioned_record<T: Serialize>(
    id: EntityId,
    revision: Revision,
    updated_at: UtcTimestamp,
    payload: &T,
) -> Result<VersionedRecord, DataControlError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id,
        revision,
        updated_at,
        payload: serde_json::to_value(payload)?,
    })
}

fn strict_payload<T>(record: &VersionedRecord) -> Result<T, DataControlError>
where
    T: for<'de> Deserialize<'de> + Serialize,
{
    if record.version != CURRENT_SCHEMA_VERSION {
        return Err(DataControlError::InvalidAgentDeletePlan);
    }
    let bytes = canonical_json_bytes(&record.payload)?;
    serde_json::from_slice(&bytes).map_err(DataControlError::Json)
}

fn shared_knowledge_space_stable_key(
    prefix: &str,
    space_id: &EntityId,
    profile_id: &ProfileId,
    revision: Revision,
    disposition: SharedKnowledgeSpaceDeleteDisposition,
) -> Result<StableKey, DataControlError> {
    let suffix = match disposition {
        SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate => "tombstone-owned-private",
        SharedKnowledgeSpaceDeleteDisposition::RetainOwnedShared => "retain-owned-shared",
        SharedKnowledgeSpaceDeleteDisposition::RemoveMembership => "remove-membership",
        SharedKnowledgeSpaceDeleteDisposition::RetainTombstone => "retain-tombstone",
    };
    StableKey::parse(format!(
        "shared-space-{prefix}:{space_id}:{profile_id}:{}:{suffix}",
        revision.get()
    ))
    .map_err(|_| DataControlError::InvalidAgentDeletePlan)
}

fn parse_shared_knowledge_space_stable_key(
    stable_key: &StableKey,
) -> Result<(ProfileId, SharedKnowledgeSpaceDeleteRecord), DataControlError> {
    let mut parts = stable_key.as_str().split(':');
    let prefix = parts
        .next()
        .ok_or(DataControlError::InvalidAgentDeletePlan)?;
    let space_id = EntityId::parse(
        parts
            .next()
            .ok_or(DataControlError::InvalidAgentDeletePlan)?,
    )
    .map_err(|_| DataControlError::InvalidAgentDeletePlan)?;
    let profile_id = parts
        .next()
        .ok_or(DataControlError::InvalidAgentDeletePlan)?
        .parse::<ProfileId>()
        .map_err(|_| DataControlError::InvalidAgentDeletePlan)?;
    let revision = parts
        .next()
        .ok_or(DataControlError::InvalidAgentDeletePlan)?
        .parse::<u64>()
        .map(Revision::new)
        .map_err(|_| DataControlError::InvalidAgentDeletePlan)?;
    let disposition = match parts
        .next()
        .ok_or(DataControlError::InvalidAgentDeletePlan)?
    {
        "tombstone-owned-private" => SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate,
        "retain-owned-shared" => SharedKnowledgeSpaceDeleteDisposition::RetainOwnedShared,
        "remove-membership" => SharedKnowledgeSpaceDeleteDisposition::RemoveMembership,
        "retain-tombstone" => SharedKnowledgeSpaceDeleteDisposition::RetainTombstone,
        _ => return Err(DataControlError::InvalidAgentDeletePlan),
    };
    if parts.next().is_some()
        || !matches!(
            (prefix, disposition),
            (
                "shared-space-action",
                SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate
                    | SharedKnowledgeSpaceDeleteDisposition::RemoveMembership
            ) | (
                "shared-space-retained",
                SharedKnowledgeSpaceDeleteDisposition::RetainOwnedShared
                    | SharedKnowledgeSpaceDeleteDisposition::RetainTombstone
            )
        )
    {
        return Err(DataControlError::InvalidAgentDeletePlan);
    }
    Ok((
        profile_id,
        SharedKnowledgeSpaceDeleteRecord {
            stable_key: stable_key.clone(),
            space_id,
            expected_permission_revision: revision,
            disposition,
        },
    ))
}

fn decode_shared_knowledge_space(
    record: &VersionedRecord,
) -> Result<SharedKnowledgeSpace, DataControlError> {
    if record.version.major != CURRENT_SCHEMA_VERSION.major
        || record.version.minor > CURRENT_SCHEMA_VERSION.minor
    {
        return Err(DataControlError::SharedKnowledgeSpaceCorrupt(
            "unsupported schema version".into(),
        ));
    }
    let space: SharedKnowledgeSpace = serde_json::from_value(record.payload.clone())
        .map_err(|error| DataControlError::SharedKnowledgeSpaceCorrupt(error.to_string()))?;
    if space.id != record.id
        || space.permission_revision != record.revision
        || space.members.len() > 256
        || space.source_event_ids.len() > 256
        || space.members.contains_key(&space.owner)
        || space.members.values().any(BTreeSet::is_empty)
    {
        return Err(DataControlError::SharedKnowledgeSpaceCorrupt(
            "record envelope or bounds are invalid".into(),
        ));
    }
    Ok(space)
}

fn shared_knowledge_space_delete_record(
    space: &SharedKnowledgeSpace,
    profile_id: &ProfileId,
    disposition: SharedKnowledgeSpaceDeleteDisposition,
) -> Result<SharedKnowledgeSpaceDeleteRecord, DataControlError> {
    let prefix = if matches!(
        disposition,
        SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate
            | SharedKnowledgeSpaceDeleteDisposition::RemoveMembership
    ) {
        "action"
    } else {
        "retained"
    };
    Ok(SharedKnowledgeSpaceDeleteRecord {
        stable_key: shared_knowledge_space_stable_key(
            prefix,
            &space.id,
            profile_id,
            space.permission_revision,
            disposition,
        )?,
        space_id: space.id.clone(),
        expected_permission_revision: space.permission_revision,
        disposition,
    })
}

fn shared_knowledge_space_postcondition(
    space: &SharedKnowledgeSpace,
    profile_id: &ProfileId,
    expected_revision: Revision,
    disposition: SharedKnowledgeSpaceDeleteDisposition,
) -> bool {
    let Some(applied_revision) = expected_revision.checked_next() else {
        return false;
    };
    if space.permission_revision.get() < applied_revision.get() {
        return false;
    }
    match disposition {
        SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate => {
            space.owner == *profile_id && space.deleted
        }
        SharedKnowledgeSpaceDeleteDisposition::RemoveMembership => {
            !space.members.contains_key(profile_id)
        }
        SharedKnowledgeSpaceDeleteDisposition::RetainOwnedShared
        | SharedKnowledgeSpaceDeleteDisposition::RetainTombstone => false,
    }
}

fn agent_delete_audit(
    operation: &DurableAgentDeleteOperation,
    occurred_at: UtcTimestamp,
) -> AgentDeleteAuditRecord {
    AgentDeleteAuditRecord {
        version: CURRENT_SCHEMA_VERSION,
        profile_id: operation.profile_id.clone(),
        replay_key: operation.replay_key.clone(),
        plan_digest: operation.plan_digest.clone(),
        terminal_revision: operation.revision,
        applied_steps: operation
            .steps
            .iter()
            .filter(|step| matches!(step.state, AgentDeleteStepState::Applied))
            .count(),
        remnant_steps: operation
            .steps
            .iter()
            .filter(|step| matches!(step.state, AgentDeleteStepState::Remnant { .. }))
            .count(),
        retained_shared_data: operation.retained_shared_data.len(),
        retained_audit: operation.retained_audit.len(),
        external_remnants: operation.external_remnants.len(),
        occurred_at,
    }
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
    evolution_roots: EvolutionDataRoots,
}

impl DataControl {
    /// Opens the owned data root and migrates its transactional state before use.
    ///
    /// # Errors
    ///
    /// Returns an error for an invalid root, limits, or state database.
    pub fn open(root: impl AsRef<Path>, limits: DataLimits) -> Result<Self, DataControlError> {
        fs::create_dir_all(root.as_ref())?;
        let root = fs::canonicalize(root.as_ref())?;
        let evolution_roots = EvolutionDataRoots::under(&root);
        Self::open_with_evolution_roots(root, limits, evolution_roots)
    }

    /// Opens data control with the daemon's exact installation-global registry roots.
    ///
    /// # Errors
    /// Returns an error for invalid bounds, roots, or state storage.
    pub fn open_with_evolution_roots(
        root: impl AsRef<Path>,
        limits: DataLimits,
        evolution_roots: EvolutionDataRoots,
    ) -> Result<Self, DataControlError> {
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
            evolution_roots,
        })
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Builds one standalone, digest-sealed teammate archive from authoritative records and owned
    /// files. Secret stores and Chromium credential-bearing profile bytes are never exported.
    pub fn export_teammates(
        &self,
        scope: TeammatesDataScope,
        now: UtcTimestamp,
    ) -> Result<TeammatesExport, DataControlError> {
        let mut records = Vec::new();
        for collection in teammate_collections() {
            for record in self.store.list_records(collection)? {
                if record_matches_teammates_scope(collection, &record, &scope) {
                    records.push(TeammatesExportRecord {
                        collection,
                        sha256: digest(&canonical_json_bytes(&record)?),
                        record,
                    });
                }
            }
        }
        if records.len() > self.limits.max_records {
            return Err(DataControlError::LimitExceeded);
        }
        records.sort_by(|left, right| {
            left.collection
                .as_str()
                .cmp(right.collection.as_str())
                .then_with(|| left.record.id.cmp(&right.record.id))
        });
        let files = self.export_teammates_files(&scope)?;
        let (retained, external_remnants) = teammates_retention(&records, &self.root)?;
        let mut export = TeammatesExport {
            format: TEAMMATES_PORTABLE_FORMAT.into(),
            schema_version: TEAMMATES_PORTABLE_SCHEMA_VERSION,
            product_schema: CURRENT_SCHEMA_VERSION,
            scope,
            exported_at: now,
            records,
            files,
            retained,
            external_remnants,
            credentials_excluded: true,
            browser_secrets_excluded: true,
            sha256: String::new(),
        };
        export.sha256 = teammates_export_digest(&export)?;
        export.validate()?;
        Ok(export)
    }

    /// Restores an archive only into an empty root, then reads every record and file back and
    /// compares its digest before returning a verification receipt.
    pub fn restore_teammates_to_fresh_root(
        export: &TeammatesExport,
        fresh_root: impl AsRef<Path>,
        limits: DataLimits,
        now: UtcTimestamp,
    ) -> Result<TeammatesRestore, DataControlError> {
        export.validate()?;
        let fresh_root = fresh_root.as_ref();
        if fresh_root.exists() && fs::read_dir(fresh_root)?.next().is_some() {
            return Err(DataControlError::TeammatesRestoreRootNotFresh);
        }
        fs::create_dir_all(fresh_root)?;
        let control = Self::open(fresh_root, limits)?;
        for chunk in export.records.chunks(256) {
            let mutations = chunk
                .iter()
                .map(|item| RecordMutation::Put {
                    collection: item.collection,
                    record: item.record.clone(),
                    precondition: WritePrecondition::Missing,
                })
                .collect::<Vec<_>>();
            if !mutations.is_empty() {
                control.store.transact(&mutations)?;
            }
        }
        for file in &export.files {
            let relative = validate_relative(&file.relative_path)?;
            let destination = control.root.join(relative);
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
        let mut verified_records = 0;
        let mut verification = BTreeMap::new();
        for expected in &export.records {
            let restored = control
                .store
                .get_record(expected.collection, &expected.record.id)?
                .ok_or(DataControlError::TeammatesRestoreVerificationFailed)?;
            let actual = digest(&canonical_json_bytes(&restored)?);
            if restored != expected.record || actual != expected.sha256 {
                return Err(DataControlError::TeammatesRestoreVerificationFailed);
            }
            verification.insert(
                format!(
                    "record:{}/{}",
                    expected.collection.as_str(),
                    expected.record.id
                ),
                actual,
            );
            verified_records += 1;
        }
        let mut verified_files = 0;
        for expected in &export.files {
            let restored = fs::read(control.root.join(&expected.relative_path))?;
            let actual = digest(&restored);
            if actual != expected.sha256
                || u64::try_from(restored.len()).ok() != Some(expected.byte_length)
            {
                return Err(DataControlError::TeammatesRestoreVerificationFailed);
            }
            verification.insert(format!("file:{}", expected.relative_path), actual);
            verified_files += 1;
        }
        let verification_sha256 = digest(&canonical_json_bytes(&verification)?);
        Ok(TeammatesRestore {
            scope: export.scope.clone(),
            export_sha256: export.sha256.clone(),
            fresh_root: control.root.to_string_lossy().into_owned(),
            restored_records: export.records.len(),
            restored_files: export.files.len(),
            verified_records,
            verified_files,
            verified_at: now,
            verification_sha256,
        })
    }

    /// Produces a confirmation-sealed exact-CAS erase plan. Archive, hide, and disable are
    /// lifecycle-only plans; they deliberately contain no deletion targets.
    pub fn plan_teammates_erase(
        &self,
        scope: TeammatesDataScope,
        operation: TeammatesEraseOperation,
        expected_profile_revision: Option<Revision>,
    ) -> Result<TeammatesErasePlan, DataControlError> {
        validate_teammates_erase_scope(&scope, operation, expected_profile_revision)?;
        let export = self.export_teammates(scope.clone(), UtcTimestamp::UNIX_EPOCH)?;
        let lifecycle_only = matches!(
            operation,
            TeammatesEraseOperation::Archive
                | TeammatesEraseOperation::Hide
                | TeammatesEraseOperation::Disable
        );
        let mut records = if lifecycle_only
            || matches!(
                operation,
                TeammatesEraseOperation::RemoveCredentials
                    | TeammatesEraseOperation::EraseBrowserData
            ) {
            Vec::new()
        } else {
            export
                .records
                .iter()
                .filter(|item| !retained_teammate_collection(item.collection))
                .map(|item| TeammatesEraseRecord {
                    collection: item.collection,
                    id: item.record.id.clone(),
                    expected_revision: item.record.revision,
                    sha256: item.sha256.clone(),
                })
                .collect()
        };
        let mut files = if lifecycle_only {
            Vec::new()
        } else {
            export
                .files
                .iter()
                .map(|file| TeammatesEraseFile {
                    relative_path: file.relative_path.clone(),
                    sha256: file.sha256.clone(),
                })
                .collect()
        };
        if matches!(
            operation,
            TeammatesEraseOperation::RemoveCredentials
                | TeammatesEraseOperation::DeleteProfile
                | TeammatesEraseOperation::FullInstallationErase
        ) {
            files.extend(self.credential_erase_files(&scope)?);
        }
        if operation == TeammatesEraseOperation::EraseBrowserData {
            files = self.browser_erase_files(&scope)?;
        } else if operation == TeammatesEraseOperation::FullInstallationErase {
            files.extend(self.browser_erase_files(&scope)?);
        }
        records.sort_by(|left, right| {
            left.collection
                .as_str()
                .cmp(right.collection.as_str())
                .then_with(|| left.id.cmp(&right.id))
        });
        files.sort_by(|left, right| left.relative_path.cmp(&right.relative_path));
        files.dedup_by(|left, right| left.relative_path == right.relative_path);
        let replay_key = StableKey::parse(format!(
            "teammates-erase:{}",
            digest(&canonical_json_bytes(&(
                scope.clone(),
                operation,
                expected_profile_revision
            ))?)
        ))
        .map_err(|_| DataControlError::InvalidTeammatesErasePlan)?;
        let retained = export.retained;
        let external_remnants = export.external_remnants;
        let mut plan = TeammatesErasePlan {
            version: CURRENT_SCHEMA_VERSION,
            scope,
            operation,
            expected_profile_revision,
            replay_key,
            records,
            files,
            retained,
            external_remnants,
            confirmation: String::new(),
        };
        plan.confirmation = teammates_erase_confirmation(&plan)?;
        Ok(plan)
    }

    /// Applies only the exact digests and revisions named by a confirmed plan. A stale target is
    /// rejected during preflight before any deletion begins.
    pub fn erase_teammates(
        &self,
        plan: &TeammatesErasePlan,
        confirmation: &str,
    ) -> Result<TeammatesEraseReport, DataControlError> {
        let expected = teammates_erase_confirmation(plan)?;
        if plan.confirmation != expected || confirmation != expected {
            return Err(DataControlError::ConfirmationRequired(expected));
        }
        validate_teammates_erase_scope(
            &plan.scope,
            plan.operation,
            plan.expected_profile_revision,
        )?;
        if plan.operation == TeammatesEraseOperation::DeleteProfile {
            let TeammatesDataScope::Profile { profile_id } = &plan.scope else {
                return Err(DataControlError::InvalidTeammatesEraseScope);
            };
            let expected_revision = plan
                .expected_profile_revision
                .ok_or(DataControlError::InvalidTeammatesEraseScope)?;
            let mut terminal_receipt = false;
            for record in self.store.list_records(Collection::AgentDeleteReceipts)? {
                let receipt: AgentDeleteReceipt = strict_payload(&record)?;
                if receipt.profile_id == *profile_id
                    && receipt.expected_revision == expected_revision
                {
                    terminal_receipt = true;
                    break;
                }
            }
            if !terminal_receipt {
                return Err(DataControlError::AgentDeleteLeakScanRequired);
            }
        }
        for target in &plan.records {
            if let Some(current) = self.store.get_record(target.collection, &target.id)? {
                if !record_matches_teammates_scope(target.collection, &current, &plan.scope)
                    || current.revision != target.expected_revision
                    || digest(&canonical_json_bytes(&current)?) != target.sha256
                {
                    return Err(DataControlError::TeammatesEraseStale);
                }
            }
        }
        for target in &plan.files {
            if !teammates_erase_path_matches_scope(
                &target.relative_path,
                &plan.scope,
                plan.operation,
            ) {
                return Err(DataControlError::InvalidTeammatesErasePlan);
            }
            let relative = validate_relative(&target.relative_path)?;
            match fs::read(self.root.join(relative)) {
                Ok(bytes) if digest(&bytes) == target.sha256 => {}
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Ok(_) | Err(_) => return Err(DataControlError::TeammatesEraseStale),
            }
        }
        let mut report = TeammatesEraseReport {
            retained: plan.retained.clone(),
            external_remnants: plan.external_remnants.clone(),
            ..TeammatesEraseReport::default()
        };
        for target in &plan.records {
            if self
                .store
                .get_record(target.collection, &target.id)?
                .is_none()
            {
                continue;
            }
            match self.store.transact(&[RecordMutation::Delete {
                collection: target.collection,
                id: target.id.clone(),
                precondition: WritePrecondition::Exact(target.expected_revision),
            }]) {
                Ok(_) => report.deleted_records += 1,
                Err(_) => {
                    if self
                        .store
                        .get_record(target.collection, &target.id)?
                        .is_some()
                    {
                        report.record_remnants.push(target.clone());
                    }
                }
            }
        }
        for target in &plan.files {
            let relative = validate_relative(&target.relative_path)?;
            match fs::remove_file(self.root.join(relative)) {
                Ok(()) => report.deleted_files += 1,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Err(_) => report.file_remnants.push(target.clone()),
            }
        }
        Ok(report)
    }

    pub fn scan_teammates_erase_remnants(
        &self,
        plan: &TeammatesErasePlan,
    ) -> Result<TeammatesEraseReport, DataControlError> {
        let mut report = TeammatesEraseReport {
            retained: plan.retained.clone(),
            external_remnants: plan.external_remnants.clone(),
            ..TeammatesEraseReport::default()
        };
        for target in &plan.records {
            if let Some(record) = self.store.get_record(target.collection, &target.id)? {
                report.record_remnants.push(TeammatesEraseRecord {
                    collection: target.collection,
                    id: target.id.clone(),
                    expected_revision: record.revision,
                    sha256: digest(&canonical_json_bytes(&record)?),
                });
            }
        }
        for target in &plan.files {
            let relative = validate_relative(&target.relative_path)?;
            let path = self.root.join(relative);
            if let Ok(bytes) = fs::read(path) {
                report.file_remnants.push(TeammatesEraseFile {
                    relative_path: target.relative_path.clone(),
                    sha256: digest(&bytes),
                });
            }
        }
        Ok(report)
    }

    fn export_teammates_files(
        &self,
        scope: &TeammatesDataScope,
    ) -> Result<Vec<TeammatesExportFile>, DataControlError> {
        if !matches!(
            scope,
            TeammatesDataScope::OwnerWide | TeammatesDataScope::Profile { .. }
        ) {
            return Ok(Vec::new());
        }
        let mut roots = match scope {
            TeammatesDataScope::OwnerWide => [
                "profiles",
                "sessions",
                "workspaces",
                "artifacts",
                "computers",
                "browser-profiles",
            ]
            .into_iter()
            .map(PathBuf::from)
            .collect::<Vec<_>>(),
            TeammatesDataScope::Profile { profile_id } => {
                let profile = profile_id.to_string();
                let mut roots = ["profiles", "workspaces", "artifacts", "computers"]
                    .into_iter()
                    .map(|root| PathBuf::from(root).join(&profile))
                    .collect::<Vec<_>>();
                roots.extend(self.profile_session_roots(profile_id)?);
                roots
            }
            _ => Vec::new(),
        };
        roots.sort();
        roots.dedup();
        let mut files = Vec::new();
        let mut total = 0_u64;
        for relative in roots {
            let relative_text = relative.to_str().ok_or(DataControlError::PathEscape)?;
            validate_relative(relative_text)?;
            let absolute = self.root.join(&relative);
            if absolute.exists() {
                collect_teammates_files(
                    &self.root,
                    &absolute,
                    &mut files,
                    &mut total,
                    self.limits,
                )?;
            }
        }
        files.sort_by(|left, right| left.relative_path.cmp(&right.relative_path));
        Ok(files)
    }

    fn profile_session_roots(
        &self,
        profile_id: &ProfileId,
    ) -> Result<Vec<PathBuf>, DataControlError> {
        let sessions = self.root.join("sessions");
        if !sessions.exists() {
            return Ok(Vec::new());
        }
        let profile = profile_id.to_string();
        let mut roots = Vec::new();
        for entry in fs::read_dir(sessions)? {
            let entry = entry?;
            let metadata = fs::symlink_metadata(entry.path())?;
            if metadata.file_type().is_symlink() {
                return Err(DataControlError::Symlink(entry.path()));
            }
            if !metadata.is_dir() {
                continue;
            }
            let manifest = fs::read(entry.path().join("manifest.json"))?;
            let manifest: serde_json::Value = serde_json::from_slice(&manifest)?;
            if manifest
                .get("profile_id")
                .and_then(serde_json::Value::as_str)
                == Some(profile.as_str())
            {
                roots.push(PathBuf::from("sessions").join(entry.file_name()));
            }
        }
        Ok(roots)
    }

    fn credential_erase_files(
        &self,
        scope: &TeammatesDataScope,
    ) -> Result<Vec<TeammatesEraseFile>, DataControlError> {
        let base = match scope {
            TeammatesDataScope::Profile { profile_id }
            | TeammatesDataScope::BrowserMetadata { profile_id, .. } => {
                self.root.join("credentials").join(profile_id.to_string())
            }
            TeammatesDataScope::OwnerWide => self.root.join("credentials"),
            _ => return Err(DataControlError::InvalidTeammatesEraseScope),
        };
        self.exact_erase_files(&base)
    }

    fn browser_erase_files(
        &self,
        scope: &TeammatesDataScope,
    ) -> Result<Vec<TeammatesEraseFile>, DataControlError> {
        let mut roots = BTreeSet::new();
        for record in self.store.list_records(Collection::ComputerRecords)? {
            if !record_matches_teammates_scope(Collection::ComputerRecords, &record, scope) {
                continue;
            }
            let Some(root) = record
                .payload
                .get("browser_profile_root")
                .and_then(serde_json::Value::as_str)
            else {
                return Err(DataControlError::InvalidTeammatesErasePlan);
            };
            let root = PathBuf::from(root);
            let canonical = fs::canonicalize(&root)?;
            if !canonical.starts_with(&self.root) {
                continue;
            }
            roots.insert(canonical);
        }
        let mut files = Vec::new();
        for root in roots {
            files.extend(self.exact_erase_files(&root)?);
        }
        files.sort_by(|left, right| left.relative_path.cmp(&right.relative_path));
        files.dedup_by(|left, right| left.relative_path == right.relative_path);
        Ok(files)
    }

    fn exact_erase_files(&self, base: &Path) -> Result<Vec<TeammatesEraseFile>, DataControlError> {
        if !base.exists() {
            return Ok(Vec::new());
        }
        let canonical = fs::canonicalize(base)?;
        if !canonical.starts_with(&self.root) {
            return Err(DataControlError::PathEscape);
        }
        let mut portable = Vec::new();
        let mut total = 0_u64;
        collect_files(
            &canonical,
            &canonical,
            &mut portable,
            &mut total,
            self.limits,
        )?;
        let prefix = canonical
            .strip_prefix(&self.root)
            .map_err(|_| DataControlError::PathEscape)?;
        portable
            .into_iter()
            .map(|file| {
                let relative = prefix.join(validate_relative(&file.relative_path)?);
                let relative_path = relative
                    .to_str()
                    .ok_or(DataControlError::PathEscape)?
                    .replace('\\', "/");
                validate_relative(&relative_path)?;
                Ok(TeammatesEraseFile {
                    relative_path,
                    sha256: file.sha256,
                })
            })
            .collect()
    }

    /// Persists a confirmed profile-delete saga before any destructive resource step begins.
    ///
    /// # Errors
    /// Returns an error for bad confirmation, stale profile revision, conflicting replay, or store
    /// failure.
    pub fn begin_agent_delete(
        &self,
        plan: &AgentDeletePlan,
        confirmation: &str,
        current_profile_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<DurableAgentDeleteOperation, DataControlError> {
        let report = execute_agent_delete(
            plan,
            confirmation,
            AgentDeleteExecutionContext {
                current_revision: current_profile_revision,
                prior_receipt: None,
            },
        )?;
        let id = agent_delete_record_id("operation", &plan.replay_key);
        if let Some(existing) = self
            .store
            .get_record(Collection::AgentDeleteOperations, &id)?
        {
            let operation: DurableAgentDeleteOperation = strict_payload(&existing)?;
            validate_durable_agent_delete(&operation)?;
            if operation.replay_key == plan.replay_key
                && operation.plan_digest == report.receipt.plan_digest
                && operation.profile_id == plan.profile_id
            {
                return Ok(operation);
            }
            return Err(DataControlError::AgentDeleteReplayConflict);
        }
        let steps = agent_delete_steps(&report)?;
        let directives = AgentDeleteDirectives {
            private_resources: report.private_resources_to_delete,
            owned_work: report.owned_work,
            lease_revocations: report.lease_revocations,
            private_shared_data: report.private_shared_data_to_delete,
        };
        let operation = DurableAgentDeleteOperation {
            version: CURRENT_SCHEMA_VERSION,
            profile_id: plan.profile_id.clone(),
            replay_key: plan.replay_key.clone(),
            plan_digest: report.receipt.plan_digest,
            expected_profile_revision: plan.expected_revision,
            state: AgentDeleteSagaState::Planned,
            steps,
            directives,
            domain_discovery: plan.domain_discovery.clone(),
            retained_shared_data: report.retained_shared_data,
            retained_audit: report.retained_audit,
            external_remnants: report.externally_controlled_remnants,
            leak_scan: None,
            receipt: None,
            revision: Revision::ZERO,
            created_at: now,
            updated_at: now,
        };
        validate_durable_agent_delete(&operation)?;
        self.store.transact(&[RecordMutation::Put {
            collection: Collection::AgentDeleteOperations,
            record: agent_delete_versioned_record(id, operation.revision, now, &operation)?,
            precondition: WritePrecondition::Missing,
        }])?;
        Ok(operation)
    }

    /// Loads one durable delete saga by its deterministic replay key.
    ///
    /// # Errors
    /// Returns an error when durable bytes are malformed or persistence fails.
    pub fn load_agent_delete(
        &self,
        replay_key: &StableKey,
    ) -> Result<Option<DurableAgentDeleteOperation>, DataControlError> {
        self.store
            .get_record(
                Collection::AgentDeleteOperations,
                &agent_delete_record_id("operation", replay_key),
            )?
            .map(|record| {
                let operation = strict_payload(&record)?;
                validate_durable_agent_delete(&operation)?;
                Ok(operation)
            })
            .transpose()
    }

    /// Lists durable delete sagas for one profile for restart reconciliation.
    ///
    /// # Errors
    /// Returns an error when an operation record is malformed or persistence fails.
    pub fn list_agent_deletes_for_profile(
        &self,
        profile_id: &ProfileId,
    ) -> Result<Vec<DurableAgentDeleteOperation>, DataControlError> {
        let mut operations = Vec::new();
        for record in self.store.list_records(Collection::AgentDeleteOperations)? {
            let operation: DurableAgentDeleteOperation = strict_payload(&record)?;
            validate_durable_agent_delete(&operation)?;
            if &operation.profile_id == profile_id {
                operations.push(operation);
            }
        }
        operations.sort_by(|left, right| left.replay_key.cmp(&right.replay_key));
        Ok(operations)
    }

    /// Inventories every authoritative shared-knowledge ownership or membership reference for a
    /// profile. The returned typed records can be merged directly into an [`AgentDeleteInventory`].
    ///
    /// Owner-private spaces become exact-CAS tombstone steps, foreign-owned memberships become
    /// exact-CAS removal steps, owner-shared spaces remain explicit shared remnants, and existing
    /// tombstones remain immutable audit.
    ///
    /// # Errors
    /// Returns an error for corrupt registry records, invalid stable keys, bounds, or persistence.
    #[allow(clippy::too_many_lines)]
    pub fn inventory_agent_shared_knowledge_spaces(
        &self,
        profile_id: &ProfileId,
    ) -> Result<SharedKnowledgeSpaceDeleteInventory, DataControlError> {
        let mut records = Vec::new();
        let mut discovered = Vec::new();
        let mut shared_data = Vec::new();
        let mut retained_audit = Vec::new();
        for stored in self.store.list_records(Collection::SharedKnowledgeSpaces)? {
            let space = decode_shared_knowledge_space(&stored)?;
            let disposition = if space.owner == *profile_id {
                if space.deleted {
                    SharedKnowledgeSpaceDeleteDisposition::RetainTombstone
                } else if space.members.is_empty() {
                    SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate
                } else {
                    SharedKnowledgeSpaceDeleteDisposition::RetainOwnedShared
                }
            } else if space.members.contains_key(profile_id) {
                if space.deleted {
                    SharedKnowledgeSpaceDeleteDisposition::RetainTombstone
                } else {
                    SharedKnowledgeSpaceDeleteDisposition::RemoveMembership
                }
            } else {
                continue;
            };
            let record = shared_knowledge_space_delete_record(&space, profile_id, disposition)?;
            let classification = match disposition {
                SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate
                | SharedKnowledgeSpaceDeleteDisposition::RemoveMembership => {
                    AgentDeleteRecordClassification::DeletePrivate
                }
                SharedKnowledgeSpaceDeleteDisposition::RetainOwnedShared => {
                    AgentDeleteRecordClassification::RetainShared
                }
                SharedKnowledgeSpaceDeleteDisposition::RetainTombstone => {
                    AgentDeleteRecordClassification::RetainImmutableAudit
                }
            };
            discovered.push(AgentDeleteDiscoveredRecord {
                stable_key: record.stable_key.clone(),
                classification,
            });
            match disposition {
                SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate => {
                    shared_data.push(SharedDataRecord {
                        stable_key: record.stable_key.clone(),
                        classification: SharedDataClassification::ProfilePrivateDelete,
                        owner_readable_consequence:
                            "owner-private shared knowledge space will be tombstoned".into(),
                    });
                    let tombstone_revision = space
                        .permission_revision
                        .checked_next()
                        .ok_or(DataControlError::SharedKnowledgeSpaceStateConflict)?;
                    retained_audit.push(ImmutableAuditRetention {
                        stable_key: shared_knowledge_space_stable_key(
                            "retained",
                            &space.id,
                            profile_id,
                            tombstone_revision,
                            SharedKnowledgeSpaceDeleteDisposition::RetainTombstone,
                        )?,
                        policy_reason:
                            "shared knowledge tombstone retained as immutable authorization audit"
                                .into(),
                    });
                }
                SharedKnowledgeSpaceDeleteDisposition::RemoveMembership => {
                    shared_data.push(SharedDataRecord {
                        stable_key: record.stable_key.clone(),
                        classification: SharedDataClassification::ProfilePrivateDelete,
                        owner_readable_consequence:
                            "deleting profile membership will be removed; shared space is retained"
                                .into(),
                    });
                }
                SharedKnowledgeSpaceDeleteDisposition::RetainOwnedShared => {
                    shared_data.push(SharedDataRecord {
                        stable_key: record.stable_key.clone(),
                        classification: SharedDataClassification::ExplicitlySharedRetain,
                        owner_readable_consequence:
                            "owned space retained for existing members pending explicit disposition"
                                .into(),
                    });
                }
                SharedKnowledgeSpaceDeleteDisposition::RetainTombstone => {
                    retained_audit.push(ImmutableAuditRetention {
                        stable_key: record.stable_key.clone(),
                        policy_reason:
                            "shared knowledge tombstone retained as immutable authorization audit"
                                .into(),
                    });
                }
            }
            records.push(record);
        }
        if records.len() > MAX_AGENT_DELETE_ITEMS {
            return Err(DataControlError::LimitExceeded);
        }
        records.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        discovered.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        shared_data.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        retained_audit.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        Ok(SharedKnowledgeSpaceDeleteInventory {
            profile_id: profile_id.clone(),
            records,
            discovery: AgentDeleteDomainDiscovery {
                domain: AgentDeleteDomain::SharedKnowledgeSpaces,
                support: AgentDeleteDomainSupport::Supported {
                    records: discovered,
                },
            },
            shared_data,
            retained_audit,
        })
    }

    /// Applies one shared-knowledge deletion step encoded by an inventory stable key.
    ///
    /// The operation is revision-CAS fenced and replay-safe across process restart. Retention-only
    /// keys validate the authoritative current classification without modifying the space.
    ///
    /// # Errors
    /// Returns an error for a foreign/malformed key, stale classification, corruption, or commit
    /// failure.
    pub fn apply_agent_shared_knowledge_space_step(
        &self,
        profile_id: &ProfileId,
        stable_key: &StableKey,
        now: UtcTimestamp,
    ) -> Result<SharedKnowledgeSpaceDeleteOutcome, DataControlError> {
        let (key_profile_id, record) = parse_shared_knowledge_space_stable_key(stable_key)?;
        if &key_profile_id != profile_id {
            return Err(DataControlError::SharedKnowledgeSpaceStateConflict);
        }
        let stored = self
            .store
            .get_record(Collection::SharedKnowledgeSpaces, &record.space_id)?
            .ok_or(DataControlError::SharedKnowledgeSpaceStateConflict)?;
        let mut space = decode_shared_knowledge_space(&stored)?;
        if shared_knowledge_space_postcondition(
            &space,
            profile_id,
            record.expected_permission_revision,
            record.disposition,
        ) {
            return Ok(SharedKnowledgeSpaceDeleteOutcome::Replay);
        }
        if space.permission_revision != record.expected_permission_revision {
            return Err(DataControlError::SharedKnowledgeSpaceStateConflict);
        }
        match record.disposition {
            SharedKnowledgeSpaceDeleteDisposition::RetainOwnedShared => {
                if space.deleted || space.owner != *profile_id || space.members.is_empty() {
                    return Err(DataControlError::SharedKnowledgeSpaceStateConflict);
                }
                return Ok(SharedKnowledgeSpaceDeleteOutcome::Retained);
            }
            SharedKnowledgeSpaceDeleteDisposition::RetainTombstone => {
                if !space.deleted
                    || (space.owner != *profile_id && !space.members.contains_key(profile_id))
                {
                    return Err(DataControlError::SharedKnowledgeSpaceStateConflict);
                }
                return Ok(SharedKnowledgeSpaceDeleteOutcome::Retained);
            }
            SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate => {
                if space.deleted || space.owner != *profile_id || !space.members.is_empty() {
                    return Err(DataControlError::SharedKnowledgeSpaceStateConflict);
                }
                space.deleted = true;
            }
            SharedKnowledgeSpaceDeleteDisposition::RemoveMembership => {
                if space.deleted
                    || space.owner == *profile_id
                    || space.members.remove(profile_id).is_none()
                {
                    return Err(DataControlError::SharedKnowledgeSpaceStateConflict);
                }
            }
        }
        space.permission_revision = record
            .expected_permission_revision
            .checked_next()
            .ok_or(DataControlError::SharedKnowledgeSpaceStateConflict)?;
        let updated = VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: space.id.clone(),
            revision: space.permission_revision,
            updated_at: now,
            payload: serde_json::to_value(&space)?,
        };
        match self.store.transact(&[RecordMutation::Put {
            collection: Collection::SharedKnowledgeSpaces,
            record: updated,
            precondition: WritePrecondition::Exact(record.expected_permission_revision),
        }]) {
            Ok(_) => Ok(SharedKnowledgeSpaceDeleteOutcome::Applied),
            Err(StoreError::Conflict { .. }) => {
                let current = self
                    .store
                    .get_record(Collection::SharedKnowledgeSpaces, &record.space_id)?
                    .ok_or(DataControlError::SharedKnowledgeSpaceStateConflict)?;
                let current = decode_shared_knowledge_space(&current)?;
                if shared_knowledge_space_postcondition(
                    &current,
                    profile_id,
                    record.expected_permission_revision,
                    record.disposition,
                ) {
                    Ok(SharedKnowledgeSpaceDeleteOutcome::Replay)
                } else {
                    Err(DataControlError::SharedKnowledgeSpaceStateConflict)
                }
            }
            Err(error) => Err(error.into()),
        }
    }

    /// Scans the authoritative registry after profile deletion work.
    ///
    /// Active owner-private rows and active memberships are leaks. Owner-shared rows and
    /// tombstones are returned as explicit retained records for the terminal remnant proof.
    ///
    /// # Errors
    /// Returns an error for corrupt registry records, invalid stable keys, bounds, or persistence.
    pub fn scan_agent_shared_knowledge_space_leaks(
        &self,
        profile_id: &ProfileId,
    ) -> Result<AgentDeleteDomainLeakScan, DataControlError> {
        let mut leaks = Vec::new();
        let mut retained = Vec::new();
        for stored in self.store.list_records(Collection::SharedKnowledgeSpaces)? {
            let space = decode_shared_knowledge_space(&stored)?;
            let disposition = if space.deleted
                && (space.owner == *profile_id || space.members.contains_key(profile_id))
            {
                Some(SharedKnowledgeSpaceDeleteDisposition::RetainTombstone)
            } else if space.owner == *profile_id && space.members.is_empty() {
                Some(SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate)
            } else if space.owner == *profile_id {
                Some(SharedKnowledgeSpaceDeleteDisposition::RetainOwnedShared)
            } else if space.members.contains_key(profile_id) {
                Some(SharedKnowledgeSpaceDeleteDisposition::RemoveMembership)
            } else {
                None
            };
            let Some(disposition) = disposition else {
                continue;
            };
            let record = shared_knowledge_space_delete_record(&space, profile_id, disposition)?;
            match disposition {
                SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate
                | SharedKnowledgeSpaceDeleteDisposition::RemoveMembership => {
                    leaks.push(record.stable_key);
                }
                SharedKnowledgeSpaceDeleteDisposition::RetainOwnedShared => {
                    retained.push(AgentDeleteDiscoveredRecord {
                        stable_key: record.stable_key,
                        classification: AgentDeleteRecordClassification::RetainShared,
                    });
                }
                SharedKnowledgeSpaceDeleteDisposition::RetainTombstone => {
                    retained.push(AgentDeleteDiscoveredRecord {
                        stable_key: record.stable_key,
                        classification: AgentDeleteRecordClassification::RetainImmutableAudit,
                    });
                }
            }
        }
        if leaks.len().saturating_add(retained.len()) > MAX_AGENT_DELETE_ITEMS {
            return Err(DataControlError::LimitExceeded);
        }
        leaks.sort();
        retained.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        let result = if !leaks.is_empty() {
            AgentDeleteDomainLeakResult::Leak { stable_keys: leaks }
        } else if retained.is_empty() {
            AgentDeleteDomainLeakResult::Clean
        } else {
            AgentDeleteDomainLeakResult::Retained { records: retained }
        };
        Ok(AgentDeleteDomainLeakScan {
            domain: AgentDeleteDomain::SharedKnowledgeSpaces,
            result,
        })
    }

    /// Revision-fences the transition from planned to executing.
    ///
    /// # Errors
    /// Returns an error for missing, corrupt, stale, or terminal operations.
    pub fn start_agent_delete_execution(
        &self,
        replay_key: &StableKey,
        expected_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<DurableAgentDeleteOperation, DataControlError> {
        self.update_agent_delete(replay_key, expected_revision, now, |operation| {
            if operation.state == AgentDeleteSagaState::Executing {
                return Ok(false);
            }
            if operation.state != AgentDeleteSagaState::Planned {
                return Err(DataControlError::InvalidAgentDeleteTransition);
            }
            if !agent_delete_inventory_supported(&operation.domain_discovery) {
                return Err(DataControlError::AgentDeleteInventoryUnsupported);
            }
            operation.state = AgentDeleteSagaState::Executing;
            Ok(true)
        })
    }

    /// Revisionally replaces one supported domain snapshot before any affected step executes.
    ///
    /// Stable keys are updated in the discovery, correlated typed directives, and pending resource
    /// steps together. Classification changes, unsupported refreshes, and refreshes after an
    /// affected step reaches a terminal outcome fail closed.
    ///
    /// # Errors
    /// Returns an error for stale revisions, invalid classifications, missing domains, or an
    /// operation that is not executing.
    pub fn record_agent_delete_domain_discovery(
        &self,
        replay_key: &StableKey,
        expected_revision: Revision,
        mut discovery: AgentDeleteDomainDiscovery,
        now: UtcTimestamp,
    ) -> Result<DurableAgentDeleteOperation, DataControlError> {
        if let AgentDeleteDomainSupport::Supported { records } = &mut discovery.support {
            records.sort_by(|left, right| {
                left.classification
                    .cmp(&right.classification)
                    .then_with(|| left.stable_key.cmp(&right.stable_key))
            });
        } else {
            return Err(DataControlError::AgentDeleteInventoryUnsupported);
        }
        self.update_agent_delete(replay_key, expected_revision, now, |operation| {
            if operation.state != AgentDeleteSagaState::Executing {
                return Err(DataControlError::InvalidAgentDeleteTransition);
            }
            let index = operation
                .domain_discovery
                .iter()
                .position(|current| current.domain == discovery.domain)
                .ok_or(DataControlError::AgentDeleteInventoryIncomplete)?;
            let current = &operation.domain_discovery[index];
            if current == &discovery {
                return Ok(false);
            }
            let AgentDeleteDomainSupport::Supported {
                records: current_records,
            } = &current.support
            else {
                return Err(DataControlError::AgentDeleteInventoryUnsupported);
            };
            let AgentDeleteDomainSupport::Supported {
                records: refreshed_records,
            } = &discovery.support
            else {
                unreachable!()
            };
            let mut current_records = current_records.clone();
            current_records.sort_by(|left, right| {
                left.classification
                    .cmp(&right.classification)
                    .then_with(|| left.stable_key.cmp(&right.stable_key))
            });
            if current_records.len() != refreshed_records.len()
                || current_records
                    .iter()
                    .zip(refreshed_records)
                    .any(|(old, new)| old.classification != new.classification)
            {
                return Err(DataControlError::AgentDeleteDiscoveryClassificationConflict);
            }
            for (old, new) in current_records.iter().zip(refreshed_records) {
                if old.stable_key == new.stable_key {
                    continue;
                }
                if operation.steps.iter().any(|step| {
                    step.stable_key == old.stable_key
                        && !matches!(step.state, AgentDeleteStepState::Pending)
                }) {
                    return Err(DataControlError::InvalidAgentDeleteTransition);
                }
                replace_agent_delete_stable_key(operation, &old.stable_key, &new.stable_key);
            }
            operation.domain_discovery[index] = discovery;
            validate_agent_delete_discovery(&operation.domain_discovery)?;
            Ok(true)
        })
    }

    /// Records one idempotent resource-step outcome under operation revision fencing.
    ///
    /// # Errors
    /// Returns an error for missing steps, unsafe details, stale revisions, or invalid state.
    pub fn record_agent_delete_step(
        &self,
        replay_key: &StableKey,
        expected_revision: Revision,
        step_key: &StableKey,
        outcome: AgentDeleteStepState,
        now: UtcTimestamp,
    ) -> Result<DurableAgentDeleteOperation, DataControlError> {
        validate_agent_delete_step_state(&outcome)?;
        self.update_agent_delete(replay_key, expected_revision, now, |operation| {
            if operation.state != AgentDeleteSagaState::Executing {
                return Err(DataControlError::InvalidAgentDeleteTransition);
            }
            let step = operation
                .steps
                .iter_mut()
                .find(|step| &step.stable_key == step_key)
                .ok_or(DataControlError::AgentDeleteStepNotFound)?;
            if step.state == outcome {
                return Ok(false);
            }
            if !matches!(step.state, AgentDeleteStepState::Pending)
                || matches!(outcome, AgentDeleteStepState::Pending)
            {
                return Err(DataControlError::InvalidAgentDeleteTransition);
            }
            step.state = outcome;
            Ok(true)
        })
    }

    /// Persists a typed post-delete leak scan for every mandatory domain.
    ///
    /// A scan containing leaks remains owner-readable and blocks terminalization until replaced by
    /// a clean or explicitly retained result at a newer operation revision.
    ///
    /// # Errors
    /// Returns an error for incomplete domains, mismatched replay identity, stale revisions, or an
    /// operation that is not executing.
    pub fn record_agent_delete_leak_scan(
        &self,
        replay_key: &StableKey,
        expected_revision: Revision,
        scan: AgentDeleteLeakScan,
        now: UtcTimestamp,
    ) -> Result<DurableAgentDeleteOperation, DataControlError> {
        validate_agent_delete_leak_scan(&scan, replay_key)?;
        self.update_agent_delete(replay_key, expected_revision, now, |operation| {
            if operation.state != AgentDeleteSagaState::Executing
                || operation.remaining_steps() != 0
            {
                return Err(DataControlError::InvalidAgentDeleteTransition);
            }
            if operation.leak_scan.as_ref() == Some(&scan) {
                return Ok(false);
            }
            operation.leak_scan = Some(scan);
            Ok(true)
        })
    }

    /// Atomically tombstones a fully accounted saga and writes immutable receipt and audit rows.
    ///
    /// Profile removal is authorized only by the returned terminal operation.
    ///
    /// # Errors
    /// Returns an error while steps remain pending or on stale/corrupt persistence.
    pub fn finalize_agent_delete(
        &self,
        replay_key: &StableKey,
        expected_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<DurableAgentDeleteOperation, DataControlError> {
        let mut operation = self
            .load_agent_delete(replay_key)?
            .ok_or(DataControlError::AgentDeleteOperationNotFound)?;
        if operation.state == AgentDeleteSagaState::TerminalTombstoned {
            return Ok(operation);
        }
        check_agent_delete_revision(expected_revision, operation.revision)?;
        if operation.state != AgentDeleteSagaState::Executing || operation.remaining_steps() != 0 {
            return Err(DataControlError::InvalidAgentDeleteTransition);
        }
        if !agent_delete_inventory_supported(&operation.domain_discovery) {
            return Err(DataControlError::AgentDeleteInventoryUnsupported);
        }
        let scan = operation
            .leak_scan
            .as_ref()
            .ok_or(DataControlError::AgentDeleteLeakScanRequired)?;
        validate_agent_delete_leak_scan(scan, replay_key)?;
        if scan
            .domains
            .iter()
            .any(|domain| matches!(domain.result, AgentDeleteDomainLeakResult::Leak { .. }))
        {
            return Err(DataControlError::AgentDeleteLeakDetected);
        }
        operation.revision = operation
            .revision
            .checked_next()
            .ok_or(DataControlError::AgentDeleteRevisionOverflow)?;
        operation.state = AgentDeleteSagaState::TerminalTombstoned;
        operation.updated_at = now;
        let receipt = AgentDeleteReceipt {
            version: CURRENT_SCHEMA_VERSION,
            profile_id: operation.profile_id.clone(),
            expected_revision: operation.expected_profile_revision,
            replay_key: operation.replay_key.clone(),
            plan_digest: operation.plan_digest.clone(),
        };
        operation.receipt = Some(receipt.clone());
        validate_durable_agent_delete(&operation)?;
        let audit = agent_delete_audit(&operation, now);
        let operation_id = agent_delete_record_id("operation", replay_key);
        let receipt_id = agent_delete_record_id("receipt", replay_key);
        let audit_id = agent_delete_record_id("audit", replay_key);
        self.store.transact(&[
            RecordMutation::Put {
                collection: Collection::AgentDeleteOperations,
                record: agent_delete_versioned_record(
                    operation_id,
                    operation.revision,
                    now,
                    &operation,
                )?,
                precondition: WritePrecondition::Exact(expected_revision),
            },
            RecordMutation::Put {
                collection: Collection::AgentDeleteReceipts,
                record: agent_delete_versioned_record(receipt_id, Revision::ZERO, now, &receipt)?,
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::AgentDeleteAudits,
                record: agent_delete_versioned_record(audit_id, Revision::ZERO, now, &audit)?,
                precondition: WritePrecondition::Missing,
            },
        ])?;
        Ok(operation)
    }

    fn update_agent_delete(
        &self,
        replay_key: &StableKey,
        expected_revision: Revision,
        now: UtcTimestamp,
        mutate: impl FnOnce(&mut DurableAgentDeleteOperation) -> Result<bool, DataControlError>,
    ) -> Result<DurableAgentDeleteOperation, DataControlError> {
        let mut operation = self
            .load_agent_delete(replay_key)?
            .ok_or(DataControlError::AgentDeleteOperationNotFound)?;
        if !mutate(&mut operation)? {
            return Ok(operation);
        }
        check_agent_delete_revision(expected_revision, operation.revision)?;
        operation.revision = operation
            .revision
            .checked_next()
            .ok_or(DataControlError::AgentDeleteRevisionOverflow)?;
        operation.updated_at = now;
        validate_durable_agent_delete(&operation)?;
        self.store.transact(&[RecordMutation::Put {
            collection: Collection::AgentDeleteOperations,
            record: agent_delete_versioned_record(
                agent_delete_record_id("operation", replay_key),
                operation.revision,
                now,
                &operation,
            )?,
            precondition: WritePrecondition::Exact(expected_revision),
        }])?;
        Ok(operation)
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

    /// Exports one installation-global evolution domain from its authoritative store/registry.
    ///
    /// # Errors
    /// Returns an error for corrupt signatures/registries, unsafe paths, symlinks, or limits.
    pub fn export_evolution(
        &self,
        domain: EvolutionDataDomain,
        now: UtcTimestamp,
    ) -> Result<EvolutionPortableExport, DataControlError> {
        let statement = self.evolution_scope_statement(domain);
        let mut export = EvolutionPortableExport {
            format: EVOLUTION_PORTABLE_FORMAT.into(),
            schema_version: EVOLUTION_PORTABLE_SCHEMA_VERSION,
            product_schema: CURRENT_SCHEMA_VERSION,
            domain,
            scope: EvolutionDataScope::InstallationGlobal,
            scope_statement: statement,
            exported_at: now,
            ledger: None,
            files: Vec::new(),
            embedded_corpus: None,
        };
        match domain {
            EvolutionDataDomain::Ledger => {
                let ledger = EvolutionLedgerArchive::from_repository(&self.store)
                    .map_err(|error| DataControlError::Evolution(error.to_string()))?;
                if ledger.records.len() > self.limits.max_records {
                    return Err(DataControlError::LimitExceeded);
                }
                export.ledger = Some(ledger);
            }
            EvolutionDataDomain::WorkerImages => {
                let inventory =
                    worker_image_data_inventory(&self.evolution_roots.worker_image_registry)
                        .map_err(|error| DataControlError::Evolution(error.to_string()))?;
                export.files = self
                    .export_inventory_files(&inventory.registry_root, &inventory.relative_files)?;
            }
            EvolutionDataDomain::ShadowTrees => {
                let inventory = shadow_data_inventory(&self.evolution_roots.shadow_work_root)
                    .map_err(|error| DataControlError::Evolution(error.to_string()))?;
                export.files = self
                    .export_inventory_files(&inventory.registry_root, &inventory.relative_files)?;
            }
            EvolutionDataDomain::EvaluationCorpus => {
                let inventory =
                    corpus_data_inventory(&self.evolution_roots.runtime_corpus_registry)
                        .map_err(|error| DataControlError::Evolution(error.to_string()))?;
                export.files = self
                    .export_inventory_files(&inventory.registry_root, &inventory.relative_files)?;
                export.embedded_corpus = Some(EmbeddedCorpusStatement {
                    sha256: inventory.embedded_sha256,
                    immutable: inventory.embedded_immutable,
                    deletable: false,
                    reason: "checked-in corpus bytes are embedded in the executable; only explicitly registered runtime-owned copies are data-control files".into(),
                });
            }
        }
        export.validate()?;
        Ok(export)
    }

    /// Builds a confirmation-sealed deletion plan including derived projections and previews.
    ///
    /// # Errors
    /// Returns an error when any exact scope cannot be safely inventoried.
    pub fn plan_delete_evolution(
        &self,
        domain: EvolutionDataDomain,
    ) -> Result<EvolutionDeletionPlan, DataControlError> {
        let export = self.export_evolution(domain, UtcTimestamp::UNIX_EPOCH)?;
        let (ledger_records, ledger_heads) = export.ledger.as_ref().map_or((0, 0), |ledger| {
            (
                ledger.records.len(),
                usize::from(ledger.authenticated_head.is_some()),
            )
        });
        let derived_base = self.evolution_derived_base(domain);
        let derived_files = if derived_base.exists() {
            self.export_directory_files(&derived_base)?
                .into_iter()
                .map(|file| file.relative_path)
                .collect()
        } else {
            Vec::new()
        };
        let target = EvolutionDeletionTarget {
            domain,
            scope: EvolutionDataScope::InstallationGlobal,
            scope_statement: export.scope_statement,
            files: export
                .files
                .into_iter()
                .map(|file| file.relative_path)
                .collect(),
            derived_files,
            ledger_records,
            ledger_heads,
            embedded_corpus_retained: domain == EvolutionDataDomain::EvaluationCorpus,
        };
        let confirmation = digest(&canonical_json_bytes(&target)?);
        Ok(EvolutionDeletionPlan {
            target,
            confirmation,
        })
    }

    /// Executes an installation-global deletion plan and reports every exact remnant.
    ///
    /// Ledger rows and head are erased together by the privileged data-control repository API.
    /// Registry metadata is deleted only after all registered payload files have been removed.
    ///
    /// # Errors
    /// Returns an error only for invalid confirmation/plan shape or a ledger transaction failure;
    /// individual filesystem failures are preserved in the returned remnant report.
    pub fn delete_evolution(
        &self,
        plan: &EvolutionDeletionPlan,
        confirmation: &str,
    ) -> Result<EvolutionDeletionReport, DataControlError> {
        let expected = digest(&canonical_json_bytes(&plan.target)?);
        if confirmation != expected || plan.confirmation != expected {
            return Err(DataControlError::ConfirmationRequired(expected));
        }
        if plan.target.scope != EvolutionDataScope::InstallationGlobal
            || plan.target.scope_statement != self.evolution_scope_statement(plan.target.domain)
        {
            return Err(DataControlError::InvalidExport);
        }
        if plan.target.files.len() > self.limits.max_files
            || plan.target.derived_files.len() > self.limits.max_files
        {
            return Err(DataControlError::LimitExceeded);
        }
        for relative in plan.target.files.iter().chain(&plan.target.derived_files) {
            validate_relative(relative)?;
        }
        let mut report = EvolutionDeletionReport {
            embedded_corpus_retained: plan.target.embedded_corpus_retained,
            ..EvolutionDeletionReport::default()
        };
        if plan.target.domain == EvolutionDataDomain::Ledger {
            let erased = self.store.erase_evolution_ledger_for_data_control()?;
            report.deleted_ledger_records = erased.deleted_records;
            report.deleted_ledger_heads = erased.deleted_heads;
            report.remaining_ledger_records = erased.remaining_records;
            report.remaining_ledger_heads = erased.remaining_heads;
        } else {
            let base = self.evolution_registry_base(plan.target.domain)?;
            let mut registry_files = Vec::new();
            for relative in &plan.target.files {
                if relative == "registry.json" {
                    registry_files.push(relative);
                    continue;
                }
                Self::remove_planned_file(&base, relative, false, &mut report);
            }
            if report.remaining_paths.is_empty() {
                for relative in registry_files {
                    Self::remove_planned_file(&base, relative, false, &mut report);
                }
            } else {
                report.remaining_paths.extend(
                    registry_files
                        .into_iter()
                        .map(|relative| base.join(relative).to_string_lossy().into_owned()),
                );
            }
            remove_empty_directories(&base);
        }
        let derived_base = self.evolution_derived_base(plan.target.domain);
        for relative in &plan.target.derived_files {
            Self::remove_planned_file(&derived_base, relative, true, &mut report);
        }
        remove_empty_directories(&derived_base);
        report.remaining_paths.sort();
        report.remaining_paths.dedup();
        Ok(report)
    }

    /// Re-inventories an evolution domain and returns whether any owned data remains.
    ///
    /// # Errors
    /// Returns an error when the exact owned scope cannot be inventoried safely.
    pub fn scan_evolution_remnants(
        &self,
        domain: EvolutionDataDomain,
    ) -> Result<Vec<String>, DataControlError> {
        let plan = self.plan_delete_evolution(domain)?;
        let mut remnants = plan
            .target
            .files
            .iter()
            .map(|relative| {
                self.evolution_registry_base(domain)
                    .map(|base| base.join(relative).to_string_lossy().into_owned())
            })
            .collect::<Result<Vec<_>, _>>()?;
        remnants.extend(plan.target.derived_files.iter().map(|relative| {
            self.evolution_derived_base(domain)
                .join(relative)
                .to_string_lossy()
                .into_owned()
        }));
        if plan.target.ledger_records > 0 {
            remnants.push(format!(
                "state-store:EvolutionLedger:{}",
                plan.target.ledger_records
            ));
        }
        if plan.target.ledger_heads > 0 {
            remnants.push(format!(
                "state-store:EvolutionLedgerHead:{}",
                plan.target.ledger_heads
            ));
        }
        remnants.sort();
        Ok(remnants)
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

    fn evolution_scope_statement(&self, domain: EvolutionDataDomain) -> EvolutionScopeStatement {
        let derived = self.evolution_derived_base(domain);
        match domain {
            EvolutionDataDomain::Ledger => EvolutionScopeStatement {
                installation_global: true,
                includes: vec![
                    "ordered signed EvolutionLedger rows".into(),
                    "signed authenticated EvolutionLedgerHead".into(),
                ],
                excludes: vec![
                    "profiles, sessions, prompts, private reasoning, and personal memory".into(),
                    "state.sqlite data outside the two evolution ledger collections".into(),
                ],
                deletion_effect: format!(
                    "atomically erases both ledger collections and cleans derived projections/previews under {}",
                    derived.display()
                ),
            },
            EvolutionDataDomain::WorkerImages => EvolutionScopeStatement {
                installation_global: true,
                includes: vec![format!(
                    "registry.json and registered immutable image payloads under {}",
                    self.evolution_roots.worker_image_registry.display()
                )],
                excludes: vec![
                    "registry.lock coordination state".into(),
                    "unregistered files and profile/session data".into(),
                ],
                deletion_effect: format!(
                    "removes registered payloads before registry.json and cleans derived projections/previews under {}",
                    derived.display()
                ),
            },
            EvolutionDataDomain::ShadowTrees => EvolutionScopeStatement {
                installation_global: true,
                includes: vec![format!(
                    "registry.json and every file below registered shadow IDs under {}",
                    self.evolution_roots.shadow_work_root.join("shadow-trees").display()
                )],
                excludes: vec![
                    "registry.lock coordination state".into(),
                    "source repositories and profile/session workspaces".into(),
                ],
                deletion_effect: format!(
                    "removes registered shadow files before registry.json and cleans derived projections/previews under {}",
                    derived.display()
                ),
            },
            EvolutionDataDomain::EvaluationCorpus => EvolutionScopeStatement {
                installation_global: true,
                includes: vec![format!(
                    "registry.json and explicitly registered runtime-owned corpus copies under {}",
                    self.evolution_roots.runtime_corpus_registry.display()
                )],
                excludes: vec![
                    "checked-in corpus bytes embedded in the executable (immutable and not deletable)"
                        .into(),
                    "profile/session data".into(),
                ],
                deletion_effect: format!(
                    "removes runtime-owned copies and registry.json, retains embedded corpus, and cleans derived projections/previews under {}",
                    derived.display()
                ),
            },
        }
    }

    fn evolution_registry_base(
        &self,
        domain: EvolutionDataDomain,
    ) -> Result<PathBuf, DataControlError> {
        let path = match domain {
            EvolutionDataDomain::Ledger => return Err(DataControlError::InvalidExport),
            EvolutionDataDomain::WorkerImages => self.evolution_roots.worker_image_registry.clone(),
            EvolutionDataDomain::ShadowTrees => {
                self.evolution_roots.shadow_work_root.join("shadow-trees")
            }
            EvolutionDataDomain::EvaluationCorpus => {
                self.evolution_roots.runtime_corpus_registry.clone()
            }
        };
        if path.exists() {
            Ok(fs::canonicalize(path)?)
        } else {
            Ok(path)
        }
    }

    fn evolution_derived_base(&self, domain: EvolutionDataDomain) -> PathBuf {
        self.evolution_roots.derived_root.join(domain.name())
    }

    fn export_inventory_files(
        &self,
        base: &Path,
        relative_files: &[PathBuf],
    ) -> Result<Vec<PortableFile>, DataControlError> {
        if relative_files.len() > self.limits.max_files {
            return Err(DataControlError::LimitExceeded);
        }
        let mut files = Vec::with_capacity(relative_files.len());
        let mut total = 0_u64;
        for relative in relative_files {
            let relative_string = relative.to_string_lossy().replace('\\', "/");
            let path = resolve_owned_file(base, &relative_string)?;
            let metadata = fs::symlink_metadata(&path)?;
            if !metadata.is_file() || metadata.file_type().is_symlink() {
                return Err(DataControlError::Symlink(path));
            }
            if metadata.len() > self.limits.max_file_bytes {
                return Err(DataControlError::LimitExceeded);
            }
            total = total
                .checked_add(metadata.len())
                .ok_or(DataControlError::LimitExceeded)?;
            if total > self.limits.max_total_bytes {
                return Err(DataControlError::LimitExceeded);
            }
            let content = fs::read(path)?;
            files.push(PortableFile {
                relative_path: relative_string,
                sha256: digest(&content),
                content_hex: encode_hex(&content),
            });
        }
        files.sort_by(|left, right| left.relative_path.cmp(&right.relative_path));
        Ok(files)
    }

    fn export_directory_files(&self, base: &Path) -> Result<Vec<PortableFile>, DataControlError> {
        let mut files = Vec::new();
        let mut total = 0;
        collect_files(base, base, &mut files, &mut total, self.limits)?;
        files.sort_by(|left, right| left.relative_path.cmp(&right.relative_path));
        Ok(files)
    }

    fn remove_planned_file(
        base: &Path,
        relative: &str,
        derived: bool,
        report: &mut EvolutionDeletionReport,
    ) {
        let Ok(path) = resolve_owned_file(base, relative) else {
            report
                .remaining_paths
                .push(base.join(relative).to_string_lossy().into_owned());
            return;
        };
        match fs::remove_file(&path) {
            Ok(()) => {
                if derived {
                    report.deleted_derived_files += 1;
                } else {
                    report.deleted_files += 1;
                }
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(_) => report
                .remaining_paths
                .push(path.to_string_lossy().into_owned()),
        }
    }
}

fn resolve_owned_file(base: &Path, relative: &str) -> Result<PathBuf, DataControlError> {
    let relative = validate_relative(relative)?;
    if let Ok(metadata) = fs::symlink_metadata(base)
        && metadata.file_type().is_symlink()
    {
        return Err(DataControlError::Symlink(base.to_path_buf()));
    }
    let mut current = base.to_path_buf();
    let mut missing = false;
    for component in relative.components() {
        let Component::Normal(name) = component else {
            return Err(DataControlError::PathEscape);
        };
        current.push(name);
        if missing {
            continue;
        }
        match fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                return Err(DataControlError::Symlink(current));
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => missing = true,
            Err(error) => return Err(error.into()),
        }
    }
    Ok(current)
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

fn collect_teammates_files(
    data_root: &Path,
    directory: &Path,
    files: &mut Vec<TeammatesExportFile>,
    total: &mut u64,
    limits: DataLimits,
) -> Result<(), DataControlError> {
    for entry in fs::read_dir(directory)? {
        let entry = entry?;
        let path = entry.path();
        let relative_path = path
            .strip_prefix(data_root)
            .map_err(|_| DataControlError::PathEscape)?
            .to_str()
            .ok_or(DataControlError::PathEscape)?
            .replace('\\', "/");
        validate_relative(&relative_path)?;
        let file_type = entry.file_type()?;
        if file_type.is_symlink() {
            return Err(DataControlError::Symlink(path));
        }
        if forbidden_teammates_export_path(&relative_path) {
            continue;
        }
        if file_type.is_dir() {
            collect_teammates_files(data_root, &path, files, total, limits)?;
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
        let content = fs::read(path)?;
        if u64::try_from(content.len()).ok() != Some(metadata.len()) {
            return Err(DataControlError::TeammatesExportDigestMismatch);
        }
        files.push(TeammatesExportFile {
            relative_path,
            byte_length: metadata.len(),
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

fn teammate_collections() -> Vec<Collection> {
    vec![
        Collection::Profiles,
        Collection::PendingActions,
        Collection::Conversations,
        Collection::ConversationParticipants,
        Collection::ConversationEvents,
        Collection::ReadReceipts,
        Collection::ConversationDeliveries,
        Collection::Deliveries,
        Collection::CollaborationRounds,
        Collection::Assignments,
        Collection::ScheduledJobs,
        Collection::JobAttempts,
        Collection::RoutingRules,
        Collection::ChannelOffsets,
        Collection::ConversationBindings,
        Collection::DirectMessageKeys,
        Collection::ConversationStableKeys,
        Collection::ConversationProjectionIntents,
        Collection::ConversationUnreadIntents,
        Collection::ConversationSearchIntents,
        Collection::ConversationPublicationIntents,
        Collection::ConversationPublicationOutbox,
        Collection::ConversationSupersessions,
        Collection::ConversationFinalizationIntents,
        Collection::SharedKnowledgeGrants,
        Collection::SharedKnowledgeSpaces,
        Collection::ComputerRecords,
        Collection::TakeoverLeases,
        Collection::ComputerAudits,
        Collection::TeammateAudits,
        Collection::AgentDeleteOperations,
        Collection::AgentDeleteReceipts,
        Collection::AgentDeleteAudits,
        Collection::SessionCatalog,
    ]
}

fn record_matches_teammates_scope(
    collection: Collection,
    record: &VersionedRecord,
    scope: &TeammatesDataScope,
) -> bool {
    match scope {
        TeammatesDataScope::OwnerWide => true,
        TeammatesDataScope::Profile { profile_id } => {
            (collection == Collection::Profiles && record.id.to_string() == profile_id.to_string())
                || payload_has_identity(
                    &record.payload,
                    &[
                        "profile_id",
                        "owner_profile_id",
                        "participant_profile_id",
                        "sender_profile_id",
                        "recipient_profile_id",
                        "destination_profile_id",
                        "previous_owner_profile_id",
                        "new_owner_profile_id",
                        "owner",
                        "members",
                    ],
                    &profile_id.to_string(),
                )
        }
        TeammatesDataScope::Conversation { conversation_id } => {
            (collection == Collection::Conversations
                && record.id.to_string() == conversation_id.to_string())
                || payload_has_identity(
                    &record.payload,
                    &["conversation_id", "source_conversation_id"],
                    &conversation_id.to_string(),
                )
        }
        TeammatesDataScope::Assignment { assignment_id } => {
            (collection == Collection::Assignments
                && record.id.to_string() == assignment_id.to_string())
                || payload_has_identity(
                    &record.payload,
                    &["assignment_id"],
                    &assignment_id.to_string(),
                )
        }
        TeammatesDataScope::Routine { routine_id } => {
            (matches!(
                collection,
                Collection::ScheduledJobs | Collection::JobAttempts
            ) && record.id.to_string() == routine_id.to_string())
                || payload_has_identity(
                    &record.payload,
                    &["routine_id", "job_id"],
                    &routine_id.to_string(),
                )
        }
        TeammatesDataScope::SharedKnowledge { space_id } => {
            collection == Collection::SharedKnowledgeSpaces && &record.id == space_id
                || payload_has_identity(&record.payload, &["space_id"], &space_id.to_string())
        }
        TeammatesDataScope::ComputerAudit { computer_id } => {
            matches!(
                collection,
                Collection::ComputerRecords
                    | Collection::ComputerAudits
                    | Collection::TeammateAudits
                    | Collection::TakeoverLeases
            ) && (record.id.to_string() == computer_id.to_string()
                || payload_has_identity(
                    &record.payload,
                    &["computer_id"],
                    &computer_id.to_string(),
                ))
        }
        TeammatesDataScope::BrowserMetadata {
            profile_id,
            computer_id,
        } => {
            matches!(
                collection,
                Collection::ComputerRecords
                    | Collection::ComputerAudits
                    | Collection::TeammateAudits
                    | Collection::TakeoverLeases
            ) && payload_has_identity(
                &record.payload,
                &["profile_id", "owner_profile_id"],
                &profile_id.to_string(),
            ) && (record.id.to_string() == computer_id.to_string()
                || payload_has_identity(
                    &record.payload,
                    &["computer_id"],
                    &computer_id.to_string(),
                ))
        }
    }
}

fn payload_has_identity(value: &serde_json::Value, fields: &[&str], identity: &str) -> bool {
    match value {
        serde_json::Value::Object(values) => values.iter().any(|(field, value)| {
            (fields.contains(&field.as_str())
                && (value.as_str() == Some(identity)
                    || value
                        .as_object()
                        .is_some_and(|members| members.contains_key(identity))))
                || payload_has_identity(value, fields, identity)
        }),
        serde_json::Value::Array(values) => values
            .iter()
            .any(|value| payload_has_identity(value, fields, identity)),
        _ => false,
    }
}

fn export_file_matches_scope(
    file: &TeammatesExportFile,
    files: &[TeammatesExportFile],
    scope: &TeammatesDataScope,
) -> bool {
    let parts = file.relative_path.split('/').collect::<Vec<_>>();
    let Some(root) = parts.first().copied() else {
        return false;
    };
    if !matches!(
        root,
        "profiles" | "sessions" | "workspaces" | "artifacts" | "computers" | "browser-profiles"
    ) {
        return false;
    }
    match scope {
        TeammatesDataScope::OwnerWide => true,
        TeammatesDataScope::Profile { profile_id } if root != "sessions" => {
            let profile = profile_id.to_string();
            parts.get(1).copied() == Some(profile.as_str())
        }
        TeammatesDataScope::Profile { profile_id } => {
            let profile = profile_id.to_string();
            let Some(session_id) = parts.get(1) else {
                return false;
            };
            let manifest_path = format!("sessions/{session_id}/manifest.json");
            files
                .iter()
                .find(|candidate| candidate.relative_path == manifest_path)
                .and_then(|manifest| decode_hex(&manifest.content_hex).ok())
                .and_then(|bytes| serde_json::from_slice::<serde_json::Value>(&bytes).ok())
                .is_some_and(|manifest| {
                    manifest
                        .get("profile_id")
                        .and_then(serde_json::Value::as_str)
                        == Some(profile.as_str())
                })
        }
        _ => false,
    }
}

fn teammates_export_digest(export: &TeammatesExport) -> Result<String, DataControlError> {
    let mut unsigned = export.clone();
    unsigned.sha256.clear();
    Ok(digest(&canonical_json_bytes(&unsigned)?))
}

fn teammates_erase_confirmation(plan: &TeammatesErasePlan) -> Result<String, DataControlError> {
    let mut unsigned = plan.clone();
    unsigned.confirmation.clear();
    Ok(digest(&canonical_json_bytes(&unsigned)?))
}

fn validate_teammates_erase_scope(
    scope: &TeammatesDataScope,
    operation: TeammatesEraseOperation,
    expected_profile_revision: Option<Revision>,
) -> Result<(), DataControlError> {
    let profile_scoped = matches!(
        scope,
        TeammatesDataScope::Profile { .. } | TeammatesDataScope::BrowserMetadata { .. }
    );
    let valid = match operation {
        TeammatesEraseOperation::Archive
        | TeammatesEraseOperation::Hide
        | TeammatesEraseOperation::Disable
        | TeammatesEraseOperation::DeleteProfile => {
            matches!(scope, TeammatesDataScope::Profile { .. })
                && expected_profile_revision.is_some()
        }
        TeammatesEraseOperation::RemoveCredentials => {
            profile_scoped && expected_profile_revision.is_some()
        }
        TeammatesEraseOperation::EraseBrowserData => {
            matches!(scope, TeammatesDataScope::BrowserMetadata { .. })
                && expected_profile_revision.is_some()
        }
        TeammatesEraseOperation::FullInstallationErase => {
            matches!(scope, TeammatesDataScope::OwnerWide) && expected_profile_revision.is_none()
        }
    };
    if valid {
        Ok(())
    } else {
        Err(DataControlError::InvalidTeammatesEraseScope)
    }
}

fn forbidden_teammates_export_path(path: &str) -> bool {
    path.split('/').any(|part| {
        let normalized = part.to_ascii_lowercase();
        normalized == "credentials"
            || normalized.starts_with("cookies")
            || normalized.starts_with("login data")
            || normalized.starts_with("web data")
            || matches!(
                normalized.as_str(),
                "local state" | "network persistent state"
            )
    })
}

fn teammates_erase_path_matches_scope(
    path: &str,
    scope: &TeammatesDataScope,
    operation: TeammatesEraseOperation,
) -> bool {
    let parts = path.split('/').collect::<Vec<_>>();
    let Some(root) = parts.first().copied() else {
        return false;
    };
    if matches!(
        operation,
        TeammatesEraseOperation::Archive
            | TeammatesEraseOperation::Hide
            | TeammatesEraseOperation::Disable
    ) {
        return false;
    }
    if operation == TeammatesEraseOperation::FullInstallationErase {
        return matches!(
            root,
            "profiles"
                | "sessions"
                | "workspaces"
                | "artifacts"
                | "computers"
                | "browser-profiles"
                | "credentials"
        );
    }
    let profile_id = match scope {
        TeammatesDataScope::Profile { profile_id }
        | TeammatesDataScope::BrowserMetadata { profile_id, .. } => profile_id.to_string(),
        _ => return false,
    };
    if operation == TeammatesEraseOperation::RemoveCredentials {
        return root == "credentials" && parts.get(1).copied() == Some(profile_id.as_str());
    }
    if operation == TeammatesEraseOperation::EraseBrowserData {
        return matches!(root, "profiles" | "computers" | "browser-profiles")
            && parts.get(1).copied() == Some(profile_id.as_str());
    }
    operation == TeammatesEraseOperation::DeleteProfile
        && ((matches!(
            root,
            "profiles" | "workspaces" | "artifacts" | "computers" | "credentials"
        ) && parts.get(1).copied() == Some(profile_id.as_str()))
            || root == "sessions")
}

fn retained_teammate_collection(collection: Collection) -> bool {
    matches!(
        collection,
        Collection::SharedKnowledgeGrants
            | Collection::SharedKnowledgeSpaces
            | Collection::ComputerAudits
            | Collection::TeammateAudits
            | Collection::AgentDeleteReceipts
            | Collection::AgentDeleteAudits
    )
}

fn teammates_retention(
    records: &[TeammatesExportRecord],
    data_root: &Path,
) -> Result<(Vec<TeammatesRetentionNotice>, Vec<ExternalRemnant>), DataControlError> {
    let mut retained = Vec::new();
    let mut external = Vec::new();
    for item in records {
        if retained_teammate_collection(item.collection) {
            let classification = if matches!(
                item.collection,
                Collection::SharedKnowledgeGrants | Collection::SharedKnowledgeSpaces
            ) {
                AgentDeleteRecordClassification::RetainShared
            } else {
                AgentDeleteRecordClassification::RetainImmutableAudit
            };
            retained.push(TeammatesRetentionNotice {
                stable_key: StableKey::parse(format!(
                    "retain:{}:{}",
                    item.collection.as_str(),
                    item.record.id
                ))
                .map_err(|_| DataControlError::InvalidTeammatesExport)?,
                classification,
                safe_consequence: if classification == AgentDeleteRecordClassification::RetainShared
                {
                    "explicitly shared data is retained for remaining authorized participants"
                        .into()
                } else {
                    "immutable audit is retained under configured policy".into()
                },
            });
        }
        if item.collection == Collection::ComputerRecords
            && let Some(root) = item
                .record
                .payload
                .get("browser_profile_root")
                .and_then(serde_json::Value::as_str)
        {
            let root = PathBuf::from(root);
            if !root.starts_with(data_root) {
                external.push(ExternalRemnant {
                    stable_key: StableKey::parse(format!("browser-profile:{}", item.record.id))
                        .map_err(|_| DataControlError::InvalidTeammatesExport)?,
                    controller: "host-configured Chromium profile storage".into(),
                    owner_action: format!(
                        "erase or back up the external browser profile root {} explicitly",
                        root.to_string_lossy()
                    ),
                });
            }
        }
    }
    retained.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
    external.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
    Ok((retained, external))
}

#[derive(Debug, Error)]
pub enum DataControlError {
    #[error("data control I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error(transparent)]
    Retrieval(#[from] RetrievalError),
    #[error("evolution data control failed: {0}")]
    Evolution(String),
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
    #[error("agent delete plan is invalid")]
    InvalidAgentDeletePlan,
    #[error(
        "agent archive, hide, disable, profile delete, and installation erase scopes are distinct"
    )]
    InvalidAgentDeleteScope,
    #[error("agent delete expected revision {expected:?}, found {actual:?}")]
    AgentDeleteRevisionConflict {
        expected: Revision,
        actual: Revision,
    },
    #[error("agent delete replay receipt does not match the confirmed plan")]
    AgentDeleteReplayConflict,
    #[error("durable agent delete operation was not found")]
    AgentDeleteOperationNotFound,
    #[error("agent delete resource step was not found")]
    AgentDeleteStepNotFound,
    #[error("agent delete state transition is invalid")]
    InvalidAgentDeleteTransition,
    #[error("agent delete operation revision overflowed")]
    AgentDeleteRevisionOverflow,
    #[error("agent delete inventory is missing a mandatory typed domain")]
    AgentDeleteInventoryIncomplete,
    #[error("agent delete inventory contains an unsupported mandatory domain")]
    AgentDeleteInventoryUnsupported,
    #[error("agent delete post-delete leak scan is required")]
    AgentDeleteLeakScanRequired,
    #[error("agent delete post-delete leak scan found private remnants")]
    AgentDeleteLeakDetected,
    #[error("agent delete refreshed discovery changed resource classification")]
    AgentDeleteDiscoveryClassificationConflict,
    #[error("shared knowledge space registry record is corrupt: {0}")]
    SharedKnowledgeSpaceCorrupt(String),
    #[error("shared knowledge space deletion state changed since inventory")]
    SharedKnowledgeSpaceStateConflict,
    #[error("teammate export is malformed, unbounded, or contains excluded secret material")]
    InvalidTeammatesExport,
    #[error("teammate export digest does not match its canonical contents")]
    TeammatesExportDigestMismatch,
    #[error("teammate restore requires a new or empty destination root")]
    TeammatesRestoreRootNotFresh,
    #[error("teammate restore bytes did not reproduce the exported records and files")]
    TeammatesRestoreVerificationFailed,
    #[error("teammate erase scope does not match the requested operation")]
    InvalidTeammatesEraseScope,
    #[error("teammate erase plan is malformed")]
    InvalidTeammatesErasePlan,
    #[error("teammate erase target changed after inventory")]
    TeammatesEraseStale,
    #[error("browser profile root is externally controlled and cannot be erased here: {0}")]
    TeammatesExternalBrowserRoot(String),
}

#[cfg(test)]
mod tests {
    use std::collections::{BTreeMap, BTreeSet};
    use std::process::Command;
    use std::sync::Arc;

    use keith_agent_types::Revision;
    use keith_knowledge::{SharedKnowledgePermission, SharedKnowledgeSpaceRegistry};
    use keith_retrieval::{
        LocalHashEmbedder, MemoryVectorIndex, RankWeights, RetrievalLimits, SearchSourceKind,
        SourceInput, VectorComponents,
    };
    use keith_self_evolution::{
        CORPUS_BYTES, EvolutionEvent, EvolutionLedger, LedgerText, SelfEvolutionEnablement,
        ShadowTree, register_runtime_corpus_copy,
    };
    use keith_supervisor::WorkerImageRegistry;
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

    fn initialize_source_repository(root: &Path) {
        fs::create_dir_all(root.join("src")).unwrap();
        fs::write(root.join("src/lib.rs"), "pub fn value() -> u8 { 1 }\n").unwrap();
        for args in [
            vec!["init", "-q"],
            vec!["add", "."],
            vec![
                "-c",
                "user.name=data-control",
                "-c",
                "user.email=data-control@example.invalid",
                "commit",
                "-qm",
                "initial",
            ],
        ] {
            assert!(
                Command::new("git")
                    .arg("-C")
                    .arg(root)
                    .args(args)
                    .status()
                    .unwrap()
                    .success()
            );
        }
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn evolution_export_delete_covers_every_global_scope_and_preserves_profiles() {
        let directory = tempdir().unwrap();
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        let profile_scope = scope();
        let profile_root = control
            .filesystem_scope(DataDomain::Memory, &profile_scope)
            .unwrap();
        fs::create_dir_all(&profile_root).unwrap();
        fs::write(profile_root.join("profile.md"), b"profile remains isolated").unwrap();

        let ledger_store = Arc::new(
            EmbeddedStore::open(
                &directory.path().join("state.sqlite"),
                Some(&FileBackupHook),
            )
            .unwrap(),
        );
        let ledger = Arc::new(EvolutionLedger::from_seed(ledger_store, &[71; 32]).unwrap());
        ledger
            .append(
                EntityId::from_u128(700),
                UtcTimestamp::from_unix_millis(7),
                EvolutionEvent::Enable {
                    acting_identity: LedgerText::redacted("installation-owner", 64, &[]).unwrap(),
                },
            )
            .unwrap();

        let image_root = directory.path().join("runtime/worker-images");
        fs::create_dir_all(image_root.parent().unwrap()).unwrap();
        let bootstrap = directory.path().join("bootstrap-worker");
        fs::write(&bootstrap, b"real bootstrap image bytes").unwrap();
        let image_registry = WorkerImageRegistry::open(&image_root, &bootstrap).unwrap();

        let source = directory.path().join("source-repository");
        initialize_source_repository(&source);
        let enablement = SelfEvolutionEnablement::new(
            directory.path().to_path_buf(),
            [19; 32],
            "installation-owner".into(),
            Arc::clone(&ledger),
        );
        let work_root = enablement.work_root().unwrap();
        let shadow = ShadowTree::stage(&source, "HEAD", &work_root).unwrap();
        assert!(shadow.root().join("src/lib.rs").exists());

        register_runtime_corpus_copy(
            directory.path().join("self-evolution/corpus"),
            "owned-v1",
            CORPUS_BYTES,
        )
        .unwrap();

        for domain in EvolutionDataDomain::ALL {
            let derived = control.evolution_derived_base(domain);
            fs::create_dir_all(derived.join("projections")).unwrap();
            fs::create_dir_all(derived.join("previews")).unwrap();
            fs::write(derived.join("projections/state.json"), b"projection").unwrap();
            fs::write(derived.join("previews/diff.txt"), b"preview").unwrap();

            let export = control
                .export_evolution(domain, UtcTimestamp::from_unix_millis(10))
                .unwrap();
            assert_eq!(export.scope, EvolutionDataScope::InstallationGlobal);
            assert!(export.scope_statement.installation_global);
            let bytes = export.to_bytes().unwrap();
            assert_eq!(EvolutionPortableExport::from_bytes(&bytes).unwrap(), export);
            let readable: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
            assert_eq!(readable["format"], EVOLUTION_PORTABLE_FORMAT);
            match domain {
                EvolutionDataDomain::Ledger => {
                    assert_eq!(export.ledger.as_ref().unwrap().records.len(), 1);
                    let mut tampered = export.clone();
                    tampered.ledger.as_mut().unwrap().records[0].signature[0] ^= 1;
                    assert!(matches!(
                        EvolutionPortableExport::from_bytes(&tampered.to_bytes().unwrap()),
                        Err(DataControlError::Evolution(_))
                    ));
                }
                EvolutionDataDomain::WorkerImages => {
                    assert!(
                        export
                            .files
                            .iter()
                            .any(|file| file.relative_path == "registry.json")
                    );
                    assert!(
                        export
                            .files
                            .iter()
                            .any(|file| file.relative_path.ends_with("agent-worker"))
                    );
                    assert!(
                        export
                            .files
                            .iter()
                            .all(|file| file.relative_path != "registry.lock")
                    );
                }
                EvolutionDataDomain::ShadowTrees => {
                    assert!(
                        export
                            .files
                            .iter()
                            .any(|file| file.relative_path == "registry.json")
                    );
                    assert!(
                        export
                            .files
                            .iter()
                            .any(|file| file.relative_path.ends_with("src/lib.rs"))
                    );
                }
                EvolutionDataDomain::EvaluationCorpus => {
                    let embedded = export.embedded_corpus.as_ref().unwrap();
                    assert!(embedded.immutable);
                    assert!(!embedded.deletable);
                    assert!(
                        export
                            .files
                            .iter()
                            .any(|file| file.relative_path == "copies/owned-v1.json")
                    );
                }
            }
        }

        for domain in EvolutionDataDomain::ALL {
            let plan = control.plan_delete_evolution(domain).unwrap();
            assert_eq!(plan.target.derived_files.len(), 2);
            let report = control.delete_evolution(&plan, &plan.confirmation).unwrap();
            assert!(report.complete(), "{domain:?}: {report:?}");
            assert_eq!(report.deleted_derived_files, 2);
            assert_eq!(
                report.embedded_corpus_retained,
                domain == EvolutionDataDomain::EvaluationCorpus
            );
            assert!(control.scan_evolution_remnants(domain).unwrap().is_empty());
        }
        assert_eq!(
            fs::read(profile_root.join("profile.md")).unwrap(),
            b"profile remains isolated"
        );
        assert_eq!(
            control
                .export(DataDomain::Memory, profile_scope, UtcTimestamp::UNIX_EPOCH)
                .unwrap()
                .files
                .len(),
            1
        );
        drop(shadow);
        drop(image_registry);
    }

    #[test]
    fn evolution_delete_rejects_tampering_and_reports_exact_filesystem_remnants() {
        let directory = tempdir().unwrap();
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        let corpus_root = directory.path().join("self-evolution/corpus");
        register_runtime_corpus_copy(&corpus_root, "owned-v1", CORPUS_BYTES).unwrap();
        let plan = control
            .plan_delete_evolution(EvolutionDataDomain::EvaluationCorpus)
            .unwrap();
        assert!(matches!(
            control.delete_evolution(&plan, "wrong"),
            Err(DataControlError::ConfirmationRequired(_))
        ));

        let copy = corpus_root.join("copies/owned-v1.json");
        fs::remove_file(&copy).unwrap();
        fs::create_dir(&copy).unwrap();
        fs::write(copy.join("retained"), b"cannot remove as a file").unwrap();
        let report = control.delete_evolution(&plan, &plan.confirmation).unwrap();
        assert!(!report.complete());
        assert!(
            report
                .remaining_paths
                .contains(&copy.to_string_lossy().into_owned())
        );
        assert!(
            report.remaining_paths.contains(
                &corpus_root
                    .join("registry.json")
                    .to_string_lossy()
                    .into_owned()
            )
        );
        assert!(report.embedded_corpus_retained);

        let mut traversal = plan;
        traversal.target.files.push("../outside".into());
        traversal.confirmation = digest(&canonical_json_bytes(&traversal.target).unwrap());
        assert!(matches!(
            control.delete_evolution(&traversal, &traversal.confirmation),
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

    fn agent_delete_inventory(operation: AgentDeleteOperation) -> AgentDeleteInventory {
        let profile_id = ProfileId::new();
        let mut domain_discovery = AgentDeleteDomain::MANDATORY
            .into_iter()
            .map(|domain| AgentDeleteDomainDiscovery {
                domain,
                support: AgentDeleteDomainSupport::Supported {
                    records: Vec::new(),
                },
            })
            .collect::<Vec<_>>();
        let mut add = |domain, key: &str, classification| {
            let discovery = domain_discovery
                .iter_mut()
                .find(|discovery| discovery.domain == domain)
                .unwrap();
            let AgentDeleteDomainSupport::Supported { records } = &mut discovery.support else {
                unreachable!()
            };
            records.push(AgentDeleteDiscoveredRecord {
                stable_key: StableKey::parse(key).unwrap(),
                classification,
            });
        };
        add(
            AgentDeleteDomain::Workspace,
            "workspace:private",
            AgentDeleteRecordClassification::DeletePrivate,
        );
        add(
            AgentDeleteDomain::PendingActions,
            "assignment:active",
            AgentDeleteRecordClassification::DeletePrivate,
        );
        add(
            AgentDeleteDomain::SharedKnowledgeGrants,
            "knowledge:shared",
            AgentDeleteRecordClassification::RetainShared,
        );
        add(
            AgentDeleteDomain::Computer,
            "takeover:active",
            AgentDeleteRecordClassification::DeletePrivate,
        );
        add(
            AgentDeleteDomain::Computer,
            "audit:delete",
            AgentDeleteRecordClassification::RetainImmutableAudit,
        );
        add(
            AgentDeleteDomain::SessionsPrivateContext,
            "external:channel-copy",
            AgentDeleteRecordClassification::ExternalRemnant,
        );
        AgentDeleteInventory {
            profile_id: profile_id.clone(),
            expected_revision: Revision::new(7),
            operation,
            private_resources: vec![StableKey::parse("workspace:private").unwrap()],
            owned_work: vec![OwnedWorkRecord {
                stable_key: StableKey::parse("assignment:active").unwrap(),
                revision: Revision::new(3),
                disposition: OwnedWorkDisposition::Transfer {
                    to_profile_id: ProfileId::new(),
                },
            }],
            shared_data: vec![SharedDataRecord {
                stable_key: StableKey::parse("knowledge:shared").unwrap(),
                classification: SharedDataClassification::ExplicitlySharedRetain,
                owner_readable_consequence: "retained for explicitly authorized collaborators"
                    .into(),
            }],
            lease_revocations: vec![LeaseRevocation {
                stable_key: StableKey::parse("takeover:active").unwrap(),
                kind: LeaseResourceKind::ComputerTakeover,
                expected_revision: Revision::new(2),
                fencing_token: Some(5),
            }],
            retained_audit: vec![ImmutableAuditRetention {
                stable_key: StableKey::parse("audit:delete").unwrap(),
                policy_reason: "immutable security audit retention policy".into(),
            }],
            external_remnants: vec![ExternalRemnant {
                stable_key: StableKey::parse("external:channel-copy").unwrap(),
                controller: "external channel provider".into(),
                owner_action: "delete the provider-controlled copy at its source".into(),
            }],
            domain_discovery,
        }
    }

    fn clean_agent_delete_leak_scan(replay_key: StableKey, at: i64) -> AgentDeleteLeakScan {
        AgentDeleteLeakScan {
            version: CURRENT_SCHEMA_VERSION,
            replay_key,
            domains: AgentDeleteDomain::MANDATORY
                .into_iter()
                .map(|domain| AgentDeleteDomainLeakScan {
                    domain,
                    result: AgentDeleteDomainLeakResult::Clean,
                })
                .collect(),
            scanned_at: UtcTimestamp(at),
        }
    }

    #[test]
    fn agent_delete_reports_active_work_shared_data_leases_audit_and_external_remnants() {
        let plan =
            AgentDeletePlan::build(agent_delete_inventory(AgentDeleteOperation::DeleteProfile))
                .unwrap();
        let report = execute_agent_delete(
            &plan,
            &plan.confirmation,
            AgentDeleteExecutionContext {
                current_revision: Revision::new(7),
                prior_receipt: None,
            },
        )
        .unwrap();
        assert_eq!(report.status, AgentDeleteExecutionStatus::Executed);
        assert_eq!(report.private_resources_to_delete.len(), 1);
        assert!(matches!(
            report.owned_work[0].disposition,
            OwnedWorkDisposition::Transfer { .. }
        ));
        assert_eq!(report.lease_revocations[0].fencing_token, Some(5));
        assert_eq!(report.retained_shared_data.len(), 1);
        assert_eq!(report.retained_audit.len(), 1);
        assert_eq!(report.externally_controlled_remnants.len(), 1);
    }

    #[test]
    fn agent_delete_rejects_stale_revision_and_tampering() {
        let plan =
            AgentDeletePlan::build(agent_delete_inventory(AgentDeleteOperation::DeleteProfile))
                .unwrap();
        assert!(matches!(
            execute_agent_delete(
                &plan,
                &plan.confirmation,
                AgentDeleteExecutionContext {
                    current_revision: Revision::new(8),
                    prior_receipt: None,
                }
            ),
            Err(DataControlError::AgentDeleteRevisionConflict { .. })
        ));
        let mut tampered = plan;
        tampered
            .private_resources
            .push(StableKey::parse("secret:extra").unwrap());
        assert!(matches!(
            execute_agent_delete(
                &tampered,
                &tampered.confirmation,
                AgentDeleteExecutionContext {
                    current_revision: Revision::new(7),
                    prior_receipt: None,
                }
            ),
            Err(DataControlError::ConfirmationRequired(_))
        ));
    }

    #[test]
    fn agent_delete_replay_is_idempotent_and_conflicting_receipts_fail_closed() {
        let plan =
            AgentDeletePlan::build(agent_delete_inventory(AgentDeleteOperation::DeleteProfile))
                .unwrap();
        let first = execute_agent_delete(
            &plan,
            &plan.confirmation,
            AgentDeleteExecutionContext {
                current_revision: plan.expected_revision,
                prior_receipt: None,
            },
        )
        .unwrap();
        let duplicate = execute_agent_delete(
            &plan,
            &plan.confirmation,
            AgentDeleteExecutionContext {
                current_revision: Revision::new(99),
                prior_receipt: Some(first.receipt.clone()),
            },
        )
        .unwrap();
        assert_eq!(duplicate.status, AgentDeleteExecutionStatus::Duplicate);
        let mut wrong = first.receipt;
        wrong.replay_key = StableKey::parse("agent-delete:wrong").unwrap();
        assert!(matches!(
            execute_agent_delete(
                &plan,
                &plan.confirmation,
                AgentDeleteExecutionContext {
                    current_revision: plan.expected_revision,
                    prior_receipt: Some(wrong),
                }
            ),
            Err(DataControlError::AgentDeleteReplayConflict)
        ));
    }

    #[test]
    fn agent_delete_keeps_archive_hide_disable_and_installation_erasure_distinct() {
        for operation in [
            AgentDeleteOperation::Archive,
            AgentDeleteOperation::Hide,
            AgentDeleteOperation::Disable,
            AgentDeleteOperation::FullInstallationErase,
        ] {
            assert!(matches!(
                AgentDeletePlan::build(agent_delete_inventory(operation)),
                Err(DataControlError::InvalidAgentDeleteScope)
            ));
        }
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn agent_delete_durable_saga_recovers_and_replays_at_every_boundary() {
        let directory = tempdir().unwrap();
        let migration_record = VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: EntityId::from_u128(9_001),
            revision: Revision::ZERO,
            updated_at: UtcTimestamp(0),
            payload: serde_json::json!({
                "stable_key": "migration:v1:inventory",
                "migration_version": "v1",
                "inventory_sha256": "00",
                "external_digests": {},
            }),
        };
        {
            let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
            control
                .store
                .transact(&[RecordMutation::Put {
                    collection: Collection::MigrationProvenance,
                    record: migration_record.clone(),
                    precondition: WritePrecondition::Missing,
                }])
                .unwrap();
        }
        let plan =
            AgentDeletePlan::build(agent_delete_inventory(AgentDeleteOperation::DeleteProfile))
                .unwrap();
        let replay_key = plan.replay_key.clone();
        let mut operation = {
            let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
            control
                .begin_agent_delete(
                    &plan,
                    &plan.confirmation,
                    plan.expected_revision,
                    UtcTimestamp(1),
                )
                .unwrap()
        };
        assert_eq!(operation.state, AgentDeleteSagaState::Planned);
        assert!(!operation.profile_removal_authorized());
        operation = {
            let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
            let loaded = control.load_agent_delete(&replay_key).unwrap().unwrap();
            assert_eq!(loaded, operation);
            control
                .start_agent_delete_execution(&replay_key, loaded.revision, UtcTimestamp(2))
                .unwrap()
        };
        assert_eq!(operation.state, AgentDeleteSagaState::Executing);
        assert!(!operation.profile_removal_authorized());
        let refreshed_workspace = AgentDeleteDomainDiscovery {
            domain: AgentDeleteDomain::Workspace,
            support: AgentDeleteDomainSupport::Supported {
                records: vec![AgentDeleteDiscoveredRecord {
                    stable_key: StableKey::parse("workspace:private:post-knowledge").unwrap(),
                    classification: AgentDeleteRecordClassification::DeletePrivate,
                }],
            },
        };
        operation = {
            let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
            let refreshed = control
                .record_agent_delete_domain_discovery(
                    &replay_key,
                    operation.revision,
                    refreshed_workspace.clone(),
                    UtcTimestamp(2),
                )
                .unwrap();
            let replayed = control
                .record_agent_delete_domain_discovery(
                    &replay_key,
                    Revision::ZERO,
                    refreshed_workspace,
                    UtcTimestamp(20),
                )
                .unwrap();
            assert_eq!(replayed, refreshed);
            refreshed
        };
        assert!(
            operation
                .directives
                .private_resources
                .contains(&StableKey::parse("workspace:private:post-knowledge").unwrap())
        );
        assert!(
            !operation
                .directives
                .private_resources
                .contains(&StableKey::parse("workspace:private").unwrap())
        );
        let step_keys = operation
            .steps
            .iter()
            .map(|step| step.stable_key.clone())
            .collect::<Vec<_>>();
        for (index, step_key) in step_keys.iter().enumerate() {
            let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
            let outcome = if index == 1 {
                AgentDeleteStepState::Remnant {
                    safe_detail: "externally retained active-work copy".into(),
                }
            } else {
                AgentDeleteStepState::Applied
            };
            operation = control
                .record_agent_delete_step(
                    &replay_key,
                    operation.revision,
                    step_key,
                    outcome.clone(),
                    UtcTimestamp(i64::try_from(index).unwrap() + 3),
                )
                .unwrap();
            let replayed = control
                .record_agent_delete_step(
                    &replay_key,
                    Revision::ZERO,
                    step_key,
                    outcome,
                    UtcTimestamp(20),
                )
                .unwrap();
            assert_eq!(replayed, operation);
            assert!(!operation.profile_removal_authorized());
        }
        {
            let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
            assert!(matches!(
                control.record_agent_delete_domain_discovery(
                    &replay_key,
                    operation.revision,
                    AgentDeleteDomainDiscovery {
                        domain: AgentDeleteDomain::Workspace,
                        support: AgentDeleteDomainSupport::Supported {
                            records: vec![AgentDeleteDiscoveredRecord {
                                stable_key: StableKey::parse("workspace:private:too-late").unwrap(),
                                classification: AgentDeleteRecordClassification::DeletePrivate,
                            }],
                        },
                    },
                    UtcTimestamp(27),
                ),
                Err(DataControlError::InvalidAgentDeleteTransition)
            ));
        }
        operation = {
            let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
            let mut leaking = clean_agent_delete_leak_scan(replay_key.clone(), 28);
            leaking.domains[0].result = AgentDeleteDomainLeakResult::Leak {
                stable_keys: vec![StableKey::parse("session:private-remnant").unwrap()],
            };
            control
                .record_agent_delete_leak_scan(
                    &replay_key,
                    operation.revision,
                    leaking,
                    UtcTimestamp(28),
                )
                .unwrap()
        };
        {
            let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
            assert!(matches!(
                control.finalize_agent_delete(&replay_key, operation.revision, UtcTimestamp(29)),
                Err(DataControlError::AgentDeleteLeakDetected)
            ));
            operation = control
                .record_agent_delete_leak_scan(
                    &replay_key,
                    operation.revision,
                    clean_agent_delete_leak_scan(replay_key.clone(), 29),
                    UtcTimestamp(29),
                )
                .unwrap();
        }
        let terminal = {
            let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
            control
                .finalize_agent_delete(&replay_key, operation.revision, UtcTimestamp(30))
                .unwrap()
        };
        assert!(terminal.profile_removal_authorized());
        assert_eq!(terminal.state, AgentDeleteSagaState::TerminalTombstoned);
        assert!(terminal.receipt.is_some());
        let proof = terminal.tombstone_proof().unwrap();
        assert!(proof.private_resources_terminal);
        assert!(proof.owned_work_terminal);
        assert!(proof.leases_terminal);
        assert!(proof.private_shared_data_terminal);
        assert!(proof.immutable_receipt_present);
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        assert_eq!(
            control
                .list_agent_deletes_for_profile(&plan.profile_id)
                .unwrap(),
            vec![terminal.clone()]
        );
        let replayed = control
            .begin_agent_delete(
                &plan,
                &plan.confirmation,
                plan.expected_revision,
                UtcTimestamp(40),
            )
            .unwrap();
        assert_eq!(replayed, terminal);
        assert_eq!(
            control
                .store
                .list_records(Collection::MigrationProvenance)
                .unwrap(),
            vec![migration_record]
        );
        assert_eq!(
            control
                .store
                .list_records(Collection::AgentDeleteOperations)
                .unwrap()
                .len(),
            1
        );
        assert_eq!(
            control
                .store
                .list_records(Collection::AgentDeleteReceipts)
                .unwrap()
                .len(),
            1
        );
        assert_eq!(
            control
                .store
                .list_records(Collection::AgentDeleteAudits)
                .unwrap()
                .len(),
            1
        );
    }

    #[test]
    fn agent_delete_cannot_finalize_before_every_resource_is_accounted() {
        let directory = tempdir().unwrap();
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        let plan =
            AgentDeletePlan::build(agent_delete_inventory(AgentDeleteOperation::DeleteProfile))
                .unwrap();
        let planned = control
            .begin_agent_delete(
                &plan,
                &plan.confirmation,
                plan.expected_revision,
                UtcTimestamp(1),
            )
            .unwrap();
        let executing = control
            .start_agent_delete_execution(&plan.replay_key, planned.revision, UtcTimestamp(2))
            .unwrap();
        assert!(matches!(
            control.finalize_agent_delete(&plan.replay_key, executing.revision, UtcTimestamp(3)),
            Err(DataControlError::InvalidAgentDeleteTransition)
        ));
        assert!(
            !control
                .load_agent_delete(&plan.replay_key)
                .unwrap()
                .unwrap()
                .profile_removal_authorized()
        );
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn agent_delete_shared_knowledge_spaces_are_cas_restartable_and_leak_scanned() {
        let directory = tempdir().unwrap();
        let state_path = directory.path().join("state.sqlite");
        let repository = Arc::new(EmbeddedStore::open(&state_path, Some(&FileBackupHook)).unwrap());
        let registry = SharedKnowledgeSpaceRegistry::new(Arc::clone(&repository));
        let target = ProfileId::new();
        let other = ProfileId::new();
        let unrelated = ProfileId::new();
        let private_id = EntityId::new();
        let shared_id = EntityId::new();
        let membership_id = EntityId::new();
        let tombstone_id = EntityId::new();
        let make_space =
            |id: EntityId,
             owner: ProfileId,
             members: BTreeMap<ProfileId, BTreeSet<SharedKnowledgePermission>>| {
                SharedKnowledgeSpace {
                    id,
                    owner,
                    members,
                    permission_revision: Revision::ZERO,
                    source_conversation_id: None,
                    source_event_ids: BTreeSet::new(),
                    deleted: false,
                }
            };
        registry
            .create(
                &make_space(private_id.clone(), target.clone(), BTreeMap::new()),
                UtcTimestamp(1),
            )
            .unwrap();
        registry
            .create(
                &make_space(
                    shared_id.clone(),
                    target.clone(),
                    BTreeMap::from([(
                        other.clone(),
                        BTreeSet::from([SharedKnowledgePermission::Read]),
                    )]),
                ),
                UtcTimestamp(1),
            )
            .unwrap();
        registry
            .create(
                &make_space(
                    membership_id.clone(),
                    other.clone(),
                    BTreeMap::from([(
                        target.clone(),
                        BTreeSet::from([SharedKnowledgePermission::Search]),
                    )]),
                ),
                UtcTimestamp(1),
            )
            .unwrap();
        registry
            .create(
                &make_space(tombstone_id.clone(), target.clone(), BTreeMap::new()),
                UtcTimestamp(1),
            )
            .unwrap();
        registry
            .delete(&tombstone_id, &target, Revision::ZERO, UtcTimestamp(2))
            .unwrap();
        registry
            .create(
                &make_space(EntityId::new(), unrelated, BTreeMap::new()),
                UtcTimestamp(1),
            )
            .unwrap();

        let inventory = DataControl::open(directory.path(), DataLimits::default())
            .unwrap()
            .inventory_agent_shared_knowledge_spaces(&target)
            .unwrap();
        assert_eq!(inventory.records.len(), 4);
        assert_eq!(inventory.shared_data.len(), 3);
        assert_eq!(inventory.retained_audit.len(), 2);
        assert!(matches!(
            DataControl::open(directory.path(), DataLimits::default())
                .unwrap()
                .scan_agent_shared_knowledge_space_leaks(&target)
                .unwrap()
                .result,
            AgentDeleteDomainLeakResult::Leak { ref stable_keys } if stable_keys.len() == 2
        ));

        let actions = inventory
            .records
            .iter()
            .filter(|record| {
                matches!(
                    record.disposition,
                    SharedKnowledgeSpaceDeleteDisposition::TombstoneOwnedPrivate
                        | SharedKnowledgeSpaceDeleteDisposition::RemoveMembership
                )
            })
            .map(|record| record.stable_key.clone())
            .collect::<Vec<_>>();
        for action in &actions {
            assert_eq!(
                DataControl::open(directory.path(), DataLimits::default())
                    .unwrap()
                    .apply_agent_shared_knowledge_space_step(&target, action, UtcTimestamp(3),)
                    .unwrap(),
                SharedKnowledgeSpaceDeleteOutcome::Applied
            );
            assert_eq!(
                DataControl::open(directory.path(), DataLimits::default())
                    .unwrap()
                    .apply_agent_shared_knowledge_space_step(&target, action, UtcTimestamp(4),)
                    .unwrap(),
                SharedKnowledgeSpaceDeleteOutcome::Replay
            );
        }

        let private = registry.get(&private_id).unwrap().unwrap();
        assert!(private.deleted);
        assert_eq!(private.permission_revision, Revision::new(1));
        let shared = registry.get(&shared_id).unwrap().unwrap();
        assert!(!shared.deleted);
        assert_eq!(shared.permission_revision, Revision::ZERO);
        assert!(shared.members.contains_key(&other));
        let membership = registry.get(&membership_id).unwrap().unwrap();
        assert!(!membership.members.contains_key(&target));
        assert_eq!(membership.permission_revision, Revision::new(1));
        let scan = DataControl::open(directory.path(), DataLimits::default())
            .unwrap()
            .scan_agent_shared_knowledge_space_leaks(&target)
            .unwrap();
        assert!(matches!(
            scan.result,
            AgentDeleteDomainLeakResult::Retained { ref records } if records.len() == 3
        ));
    }

    #[test]
    fn agent_delete_refuses_missing_or_unsupported_mandatory_domain_inventory() {
        let mut missing = agent_delete_inventory(AgentDeleteOperation::DeleteProfile);
        missing.domain_discovery.pop();
        assert!(matches!(
            AgentDeletePlan::build(missing),
            Err(DataControlError::AgentDeleteInventoryIncomplete)
        ));

        let mut unsupported = agent_delete_inventory(AgentDeleteOperation::DeleteProfile);
        unsupported.domain_discovery[0].support = AgentDeleteDomainSupport::Unsupported {
            safe_reason: "owning session store has no typed inventory adapter".into(),
        };
        let plan = AgentDeletePlan::build(unsupported).unwrap();
        let directory = tempdir().unwrap();
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        let planned = control
            .begin_agent_delete(
                &plan,
                &plan.confirmation,
                plan.expected_revision,
                UtcTimestamp(1),
            )
            .unwrap();
        assert!(matches!(
            control.start_agent_delete_execution(
                &plan.replay_key,
                planned.revision,
                UtcTimestamp(2)
            ),
            Err(DataControlError::AgentDeleteInventoryUnsupported)
        ));
        assert!(
            !control
                .load_agent_delete(&plan.replay_key)
                .unwrap()
                .unwrap()
                .profile_removal_authorized()
        );
    }

    fn put_teammates_record(
        control: &DataControl,
        collection: Collection,
        id: EntityId,
        revision: Revision,
        payload: serde_json::Value,
    ) {
        control
            .store
            .transact(&[RecordMutation::Put {
                collection,
                record: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id,
                    revision,
                    updated_at: UtcTimestamp(1),
                    payload,
                },
                precondition: WritePrecondition::Missing,
            }])
            .unwrap();
    }

    #[test]
    fn teammates_export_rejects_forged_scope_digest_and_secret_paths() {
        let directory = tempdir().unwrap();
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        let owner = ProfileId::new();
        let other = ProfileId::new();
        let owner_space = SharedKnowledgeSpace {
            id: EntityId::new(),
            owner: owner.clone(),
            members: BTreeMap::new(),
            permission_revision: Revision::new(3),
            source_conversation_id: None,
            source_event_ids: BTreeSet::new(),
            deleted: false,
        };
        put_teammates_record(
            &control,
            Collection::SharedKnowledgeSpaces,
            owner_space.id.clone(),
            owner_space.permission_revision,
            serde_json::to_value(&owner_space).unwrap(),
        );
        let other_space = SharedKnowledgeSpace {
            id: EntityId::new(),
            owner: other.clone(),
            members: BTreeMap::new(),
            permission_revision: Revision::new(4),
            source_conversation_id: None,
            source_event_ids: BTreeSet::new(),
            deleted: false,
        };
        put_teammates_record(
            &control,
            Collection::SharedKnowledgeSpaces,
            other_space.id.clone(),
            other_space.permission_revision,
            serde_json::to_value(&other_space).unwrap(),
        );
        let owner_memory = directory
            .path()
            .join("profiles")
            .join(owner.to_string())
            .join("memory");
        fs::create_dir_all(&owner_memory).unwrap();
        fs::write(owner_memory.join("fact.json"), b"owner-memory").unwrap();
        let other_memory = directory
            .path()
            .join("profiles")
            .join(other.to_string())
            .join("memory");
        fs::create_dir_all(&other_memory).unwrap();
        fs::write(other_memory.join("fact.json"), b"other-memory").unwrap();
        let credential_root = directory.path().join("credentials").join(owner.to_string());
        fs::create_dir_all(&credential_root).unwrap();
        fs::write(credential_root.join("provider.secret"), b"never-export").unwrap();
        let browser_root = directory
            .path()
            .join("profiles")
            .join(owner.to_string())
            .join("browser");
        fs::create_dir_all(&browser_root).unwrap();
        fs::write(browser_root.join("Preferences"), b"portable-browser-state").unwrap();
        fs::write(browser_root.join("Cookies"), b"browser-secret").unwrap();

        let export = control
            .export_teammates(
                TeammatesDataScope::Profile {
                    profile_id: owner.clone(),
                },
                UtcTimestamp(5),
            )
            .unwrap();
        assert_eq!(export.records.len(), 1);
        assert!(
            export
                .files
                .iter()
                .any(|file| file.relative_path.ends_with("fact.json"))
        );
        assert!(
            export
                .files
                .iter()
                .any(|file| file.relative_path.ends_with("Preferences"))
        );
        assert!(!export.files.iter().any(|file| {
            file.relative_path.contains(&other.to_string())
                || file.relative_path.contains("credentials")
                || file.relative_path.ends_with("Cookies")
                || decode_hex(&file.content_hex).unwrap() == b"never-export"
                || decode_hex(&file.content_hex).unwrap() == b"browser-secret"
        }));

        let mut forged = export.clone();
        forged.scope = TeammatesDataScope::Profile {
            profile_id: other.clone(),
        };
        forged.sha256 = teammates_export_digest(&forged).unwrap();
        assert!(matches!(
            forged.validate(),
            Err(DataControlError::InvalidTeammatesExport)
        ));
        let mut tampered = export;
        tampered.records[0].record.revision = Revision::new(99);
        assert!(matches!(
            tampered.validate(),
            Err(DataControlError::InvalidTeammatesExport)
                | Err(DataControlError::TeammatesExportDigestMismatch)
        ));
    }

    #[test]
    fn teammates_restore_requires_fresh_root_and_verifies_restored_bytes() {
        let source = tempdir().unwrap();
        let control = DataControl::open(source.path(), DataLimits::default()).unwrap();
        let profile_id = ProfileId::new();
        let space = SharedKnowledgeSpace {
            id: EntityId::new(),
            owner: profile_id.clone(),
            members: BTreeMap::new(),
            permission_revision: Revision::ZERO,
            source_conversation_id: None,
            source_event_ids: BTreeSet::new(),
            deleted: false,
        };
        put_teammates_record(
            &control,
            Collection::SharedKnowledgeSpaces,
            space.id.clone(),
            space.permission_revision,
            serde_json::to_value(&space).unwrap(),
        );
        let profile_root = source.path().join("profiles").join(profile_id.to_string());
        fs::create_dir_all(&profile_root).unwrap();
        fs::write(profile_root.join("identity.json"), b"identity").unwrap();
        let export = control
            .export_teammates(TeammatesDataScope::OwnerWide, UtcTimestamp(2))
            .unwrap();

        let destination = tempdir().unwrap();
        fs::write(destination.path().join("foreign"), b"occupied").unwrap();
        assert!(matches!(
            DataControl::restore_teammates_to_fresh_root(
                &export,
                destination.path(),
                DataLimits::default(),
                UtcTimestamp(3),
            ),
            Err(DataControlError::TeammatesRestoreRootNotFresh)
        ));
        let parent = tempdir().unwrap();
        let fresh = parent.path().join("restored");
        let receipt = DataControl::restore_teammates_to_fresh_root(
            &export,
            &fresh,
            DataLimits::default(),
            UtcTimestamp(4),
        )
        .unwrap();
        assert!(receipt.verified());
        assert_eq!(receipt.export_sha256, export.sha256);
        assert_eq!(
            fs::read(
                fresh
                    .join("profiles")
                    .join(profile_id.to_string())
                    .join("identity.json")
            )
            .unwrap(),
            b"identity"
        );
        assert!(matches!(
            DataControl::restore_teammates_to_fresh_root(
                &export,
                &fresh,
                DataLimits::default(),
                UtcTimestamp(5),
            ),
            Err(DataControlError::TeammatesRestoreRootNotFresh)
        ));
    }

    #[test]
    fn teammates_erase_rejects_cross_profile_and_stale_bytes_then_replays_after_crash() {
        let directory = tempdir().unwrap();
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        let profile_id = ProfileId::new();
        let other = ProfileId::new();
        let credential = directory
            .path()
            .join("credentials")
            .join(profile_id.to_string())
            .join("provider");
        let other_credential = directory
            .path()
            .join("credentials")
            .join(other.to_string())
            .join("provider");
        fs::create_dir_all(credential.parent().unwrap()).unwrap();
        fs::create_dir_all(other_credential.parent().unwrap()).unwrap();
        fs::write(&credential, b"owner-secret").unwrap();
        fs::write(&other_credential, b"other-secret").unwrap();
        let scope = TeammatesDataScope::Profile {
            profile_id: profile_id.clone(),
        };
        let plan = control
            .plan_teammates_erase(
                scope.clone(),
                TeammatesEraseOperation::RemoveCredentials,
                Some(Revision::new(7)),
            )
            .unwrap();
        assert_eq!(plan.files.len(), 1);
        let mut forged = plan.clone();
        forged.files[0].relative_path = format!("credentials/{other}/provider");
        forged.files[0].sha256 = digest(b"other-secret");
        forged.confirmation = teammates_erase_confirmation(&forged).unwrap();
        assert!(matches!(
            control.erase_teammates(&forged, &forged.confirmation),
            Err(DataControlError::InvalidTeammatesErasePlan)
        ));
        fs::write(&credential, b"changed-secret").unwrap();
        assert!(matches!(
            control.erase_teammates(&plan, &plan.confirmation),
            Err(DataControlError::TeammatesEraseStale)
        ));
        let recovered = control
            .plan_teammates_erase(
                scope,
                TeammatesEraseOperation::RemoveCredentials,
                Some(Revision::new(7)),
            )
            .unwrap();
        fs::remove_file(&credential).unwrap();
        let report = control
            .erase_teammates(&recovered, &recovered.confirmation)
            .unwrap();
        assert!(report.complete());
        assert!(
            control
                .scan_teammates_erase_remnants(&recovered)
                .unwrap()
                .complete()
        );
        assert_eq!(fs::read(other_credential).unwrap(), b"other-secret");
    }

    #[test]
    fn teammates_shared_retention_and_browser_erasure_remnants_are_explicit() {
        let directory = tempdir().unwrap();
        let control = DataControl::open(directory.path(), DataLimits::default()).unwrap();
        let owner = ProfileId::new();
        let member = ProfileId::new();
        let space = SharedKnowledgeSpace {
            id: EntityId::new(),
            owner: owner.clone(),
            members: BTreeMap::from([(member, BTreeSet::from([SharedKnowledgePermission::Read]))]),
            permission_revision: Revision::new(2),
            source_conversation_id: None,
            source_event_ids: BTreeSet::new(),
            deleted: false,
        };
        put_teammates_record(
            &control,
            Collection::SharedKnowledgeSpaces,
            space.id.clone(),
            space.permission_revision,
            serde_json::to_value(&space).unwrap(),
        );
        let computer_id = ComputerId::new();
        let browser_root = directory
            .path()
            .join("profiles")
            .join(owner.to_string())
            .join("browser");
        fs::create_dir_all(&browser_root).unwrap();
        fs::write(browser_root.join("Cookies"), b"secret-cookie").unwrap();
        fs::write(browser_root.join("Preferences"), b"settings").unwrap();
        put_teammates_record(
            &control,
            Collection::ComputerRecords,
            computer_id.as_entity_id().clone(),
            Revision::new(4),
            serde_json::json!({
                "version": CURRENT_SCHEMA_VERSION,
                "computer_id": computer_id,
                "owner_profile_id": owner,
                "browser_profile_root": browser_root.to_string_lossy(),
                "screen_key": "screen:owner",
                "state": "ready",
                "control_state": "idle",
                "current_task_key": null,
                "created_at": 1,
                "updated_at": 1,
                "revision": 4,
            }),
        );
        let export = control
            .export_teammates(
                TeammatesDataScope::Profile {
                    profile_id: owner.clone(),
                },
                UtcTimestamp(8),
            )
            .unwrap();
        assert!(export.retained.iter().any(|notice| {
            notice.classification == AgentDeleteRecordClassification::RetainShared
        }));
        assert!(
            !export
                .files
                .iter()
                .any(|file| file.relative_path.ends_with("Cookies"))
        );
        let browser_plan = control
            .plan_teammates_erase(
                TeammatesDataScope::BrowserMetadata {
                    profile_id: owner,
                    computer_id: computer_id.clone(),
                },
                TeammatesEraseOperation::EraseBrowserData,
                Some(Revision::new(4)),
            )
            .unwrap();
        assert!(
            browser_plan
                .files
                .iter()
                .any(|file| file.relative_path.ends_with("Cookies"))
        );
        assert!(
            browser_plan
                .files
                .iter()
                .any(|file| file.relative_path.ends_with("Preferences"))
        );

        let full_plan = control
            .plan_teammates_erase(
                TeammatesDataScope::OwnerWide,
                TeammatesEraseOperation::FullInstallationErase,
                None,
            )
            .unwrap();
        let computer_record_id = computer_id.as_entity_id().clone();
        let mut changed = control
            .store
            .get_record(Collection::ComputerRecords, &computer_record_id)
            .unwrap()
            .unwrap();
        changed.revision = Revision::new(5);
        changed.payload["revision"] = serde_json::json!(5);
        changed.updated_at = UtcTimestamp(2);
        changed.payload["updated_at"] = serde_json::json!(2);
        control
            .store
            .transact(&[RecordMutation::Put {
                collection: Collection::ComputerRecords,
                record: changed,
                precondition: WritePrecondition::Exact(Revision::new(4)),
            }])
            .unwrap();
        assert!(matches!(
            control.erase_teammates(&full_plan, &full_plan.confirmation),
            Err(DataControlError::TeammatesEraseStale)
        ));

        let external = tempdir().unwrap();
        let external_owner = ProfileId::new();
        let external_computer = ComputerId::new();
        put_teammates_record(
            &control,
            Collection::ComputerRecords,
            external_computer.as_entity_id().clone(),
            Revision::ZERO,
            serde_json::json!({
                "version": CURRENT_SCHEMA_VERSION,
                "computer_id": external_computer,
                "owner_profile_id": external_owner,
                "browser_profile_root": external.path().to_string_lossy(),
                "screen_key": "screen:external",
                "state": "ready",
                "control_state": "idle",
                "current_task_key": null,
                "created_at": 1,
                "updated_at": 1,
                "revision": 0,
            }),
        );
        let owner_wide = control
            .export_teammates(TeammatesDataScope::OwnerWide, UtcTimestamp(9))
            .unwrap();
        assert!(
            owner_wide.external_remnants.iter().any(|remnant| {
                remnant.controller == "host-configured Chromium profile storage"
            })
        );
    }
}
