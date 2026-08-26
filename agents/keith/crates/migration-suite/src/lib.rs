#![forbid(unsafe_code)]
#![allow(clippy::missing_errors_doc)]

use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::fs::{self, File, OpenOptions};
use std::io::{self, Read, Write};
use std::path::{Component, Path, PathBuf};
use std::process::Command;

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, ConversationId, EntityId, EventId, ProfileId, Revision, SessionId,
    StableKey, UtcTimestamp, canonical_json_bytes,
};
use keith_conversation::{
    ConversationEvent, ConversationEventKind, ConversationKind, ConversationLifecycle,
    ConversationParticipant, ConversationRecord, EventHead, EventProvenance, HumanAgentDmKey,
    NotificationPolicy, ParticipantPrincipal, ParticipantRole, Principal,
};
use keith_profile::RegisteredProfile;
use keith_routing::ChannelRouteRule;
use keith_session_store::{SessionKind, SessionStore, SessionStoreError};
use keith_state_store::{EmbeddedStore, FileBackupHook, StoreError};
use keith_state_store_core::{Collection, RecordMutation, VersionedRecord, WritePrecondition};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OracleRecordIdentity {
    pub id: EntityId,
    pub revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammateStateSnapshot {
    pub collection_digests: BTreeMap<String, String>,
    pub record_identities: BTreeMap<String, Vec<OracleRecordIdentity>>,
    pub external_digests: ExternalStateSnapshot,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExternalStateSnapshot {
    pub workspace_digest: String,
    pub memory_digest: String,
    pub session_bytes_digest: String,
    pub route_policy_digest: String,
    pub schedule_digest: String,
    pub permission_digest: String,
    /// Digest of credential references and related policy metadata inside profile records.
    /// Credential values are never read or included.
    pub credential_store_digest: String,
    pub child_bounds_digest: String,
    pub shared_data_classification_digest: String,
}

const MAX_BACKUP_FILES: usize = 100_000;
const MAX_BACKUP_BYTES: u64 = 16 * 1024 * 1024 * 1024;
pub const RESOURCE_BACKUP_FORMAT_VERSION: u16 = 2;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ResourceRootKind {
    Workspace,
    Memory,
    Resource,
    CredentialStore,
    Skill,
    BrowserProfile,
    AuditArchive,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NamedResourceRoot {
    pub name: String,
    pub kind: ResourceRootKind,
    pub path: PathBuf,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResourceBackupEntry {
    pub root_name: String,
    pub root_kind: ResourceRootKind,
    pub relative_path: PathBuf,
    pub byte_length: u64,
    pub sha256: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResourceBackupManifest {
    pub version: u16,
    pub store_schema_version: u32,
    pub state_byte_length: u64,
    pub state_sha256: String,
    pub entries: Vec<ResourceBackupEntry>,
    pub total_bytes: u64,
    pub manifest_digest: String,
}

#[derive(Debug, Error)]
pub enum AuthoritativeMigrationError {
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error(transparent)]
    Session(#[from] SessionStoreError),
    #[error(transparent)]
    Plan(#[from] TeammateMigrationPlanError),
    #[error(transparent)]
    Oracle(#[from] TeammateMigrationOracleError),
    #[error("resource backup failed: {0}")]
    Resource(String),
    #[error("authoritative profile ID is malformed")]
    ProfileId,
    #[error("authoritative profile record is invalid: {0}")]
    Profile(String),
    #[error("authoritative route record is invalid: {0}")]
    Route(String),
}

pub struct AuthoritativeMigrationSource<'a> {
    pub store: &'a EmbeddedStore,
    pub sessions: &'a SessionStore,
    pub default_profile_id: ProfileId,
    pub migration_version: String,
    pub migrated_at: UtcTimestamp,
    pub resource_roots: Vec<NamedResourceRoot>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuthoritativeMigrationReport {
    pub outcome: keith_state_store::TeammateMigrationOutcome,
    pub selected_keith_session: Option<SessionId>,
    pub inventory_digest: String,
    pub backup_manifest: ResourceBackupManifest,
    pub oracle: TeammateMigrationOracleReport,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuthoritativeRestoreReport {
    pub manifest_digest: String,
    pub store_schema_version: u32,
    pub restored_resource_files: usize,
    pub restored_bytes: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TeammateMigrationOracle {
    before: TeammateStateSnapshot,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TeammateMigrationOracleReport {
    pub preserved_collections: BTreeSet<String>,
    pub route_identities_preserved: bool,
    pub backup_restored_into_fresh_root: bool,
}

#[derive(Debug, Error)]
pub enum TeammateMigrationOracleError {
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error("migration changed protected collection {collection}")]
    ProtectedCollectionChanged { collection: String },
    #[error("migration changed route identities or revisions")]
    RouteIdentityChanged,
    #[error("conversation bindings differ from the deterministic migration plan")]
    ConversationBindingMismatch,
    #[error("restored backup differs from the pre-migration store")]
    RestoreMismatch,
    #[error("oracle canonical encoding failed: {0}")]
    Encoding(String),
    #[error("authoritative resource backup failed: {0}")]
    ResourceBackup(String),
}

impl TeammateMigrationOracle {
    const SNAPSHOT: &'static [Collection] = &[
        Collection::WorkerLeases,
        Collection::WorkerGenerations,
        Collection::SessionCatalog,
        Collection::PromptIngress,
        Collection::Profiles,
        Collection::ProfileExecutionFences,
        Collection::ProfileExecutionRegistrations,
        Collection::PendingActions,
        Collection::Children,
        Collection::ChildMessages,
        Collection::Goals,
        Collection::Plans,
        Collection::Commitments,
        Collection::WaitingConditions,
        Collection::ScheduledJobs,
        Collection::JobAttempts,
        Collection::RoutingRules,
        Collection::ResourceGovernance,
        Collection::ChannelOffsets,
        Collection::Deliveries,
        Collection::AttentionCandidates,
        Collection::InitiativeHistory,
        Collection::EvolutionTransactions,
        Collection::EvolutionLedger,
        Collection::EvolutionLedgerHead,
        Collection::ToolExperience,
        Collection::KernelMetadata,
        Collection::ActiveOperations,
        Collection::SchemaMigrations,
        Collection::Conversations,
        Collection::ConversationParticipants,
        Collection::ConversationEvents,
        Collection::ConversationDeliveries,
        Collection::CollaborationRounds,
        Collection::Assignments,
        Collection::ReadReceipts,
        Collection::SharedKnowledgeGrants,
        Collection::SharedKnowledgeSpaces,
        Collection::ComputerRecords,
        Collection::TakeoverLeases,
        Collection::TeammateAudits,
        Collection::LegacySessions,
        Collection::MigrationProvenance,
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
        Collection::AgentDeleteOperations,
        Collection::AgentDeleteReceipts,
        Collection::AgentDeleteAudits,
        Collection::ComputerAudits,
        Collection::AgentProvisionOperations,
    ];

    /// Captures canonical profile/resource/session/child/schedule/shared-data digests and route
    /// identities directly from the durable state store.
    pub fn capture(store: &EmbeddedStore) -> Result<Self, TeammateMigrationOracleError> {
        Self::capture_with_external(store, ExternalStateSnapshot::default())
    }

    pub fn capture_with_external(
        store: &EmbeddedStore,
        external: ExternalStateSnapshot,
    ) -> Result<Self, TeammateMigrationOracleError> {
        Ok(Self {
            before: capture_snapshot(store, external)?,
        })
    }

    #[must_use]
    pub const fn before(&self) -> &TeammateStateSnapshot {
        &self.before
    }

    /// Verifies the post-migration store and proves that the verified backup opens independently
    /// at a fresh path and reproduces the complete pre-migration protected state.
    pub fn verify(
        &self,
        after: &EmbeddedStore,
        backup: &Path,
        fresh_root: &Path,
    ) -> Result<TeammateMigrationOracleReport, TeammateMigrationOracleError> {
        self.verify_with_external(after, backup, fresh_root, ExternalStateSnapshot::default())
    }

    pub fn verify_with_external(
        &self,
        after: &EmbeddedStore,
        backup: &Path,
        fresh_root: &Path,
        external: ExternalStateSnapshot,
    ) -> Result<TeammateMigrationOracleReport, TeammateMigrationOracleError> {
        let after = capture_snapshot(after, external)?;
        let mut preserved = BTreeSet::new();
        for &collection in Self::SNAPSHOT {
            if migration_mutates(collection) {
                continue;
            }
            let name = collection.as_str().to_owned();
            if self.before.collection_digests.get(&name) != after.collection_digests.get(&name) {
                if collection == Collection::RoutingRules {
                    return Err(TeammateMigrationOracleError::RouteIdentityChanged);
                }
                return Err(TeammateMigrationOracleError::ProtectedCollectionChanged {
                    collection: name,
                });
            }
            preserved.insert(name);
        }
        EmbeddedStore::restore_backup_to(backup, fresh_root)?;
        let restored = EmbeddedStore::open(fresh_root, Some(&FileBackupHook))?;
        let restored = capture_snapshot(&restored, self.before.external_digests.clone())?;
        if self.before != restored {
            return Err(TeammateMigrationOracleError::RestoreMismatch);
        }
        Ok(TeammateMigrationOracleReport {
            preserved_collections: preserved,
            route_identities_preserved: true,
            backup_restored_into_fresh_root: true,
        })
    }
}

const fn migration_mutates(collection: Collection) -> bool {
    matches!(
        collection,
        Collection::SchemaMigrations
            | Collection::Conversations
            | Collection::ConversationParticipants
            | Collection::ConversationEvents
            | Collection::LegacySessions
            | Collection::MigrationProvenance
            | Collection::ConversationBindings
            | Collection::DirectMessageKeys
            | Collection::ConversationStableKeys
            | Collection::ConversationProjectionIntents
            | Collection::ConversationUnreadIntents
            | Collection::ConversationSearchIntents
            | Collection::ConversationPublicationIntents
    )
}

fn capture_snapshot(
    store: &EmbeddedStore,
    external_digests: ExternalStateSnapshot,
) -> Result<TeammateStateSnapshot, TeammateMigrationOracleError> {
    let collections = TeammateMigrationOracle::SNAPSHOT.iter().copied();
    let mut collection_digests = BTreeMap::new();
    let mut record_identities = BTreeMap::new();
    for collection in collections {
        let records = store.list_records(collection)?;
        collection_digests.insert(collection.as_str().to_owned(), records_digest(&records)?);
        record_identities.insert(
            collection.as_str().to_owned(),
            records
                .iter()
                .map(|record| OracleRecordIdentity {
                    id: record.id.clone(),
                    revision: record.revision,
                })
                .collect(),
        );
    }
    Ok(TeammateStateSnapshot {
        collection_digests,
        record_identities,
        external_digests,
    })
}

impl AuthoritativeMigrationSource<'_> {
    pub fn capture_inventory(
        &self,
    ) -> Result<TeammateMigrationInventory, AuthoritativeMigrationError> {
        let mut profiles = BTreeSet::new();
        let mut enabled_profiles = BTreeSet::new();
        for record in self.store.list_records(Collection::Profiles)? {
            let profile: RegisteredProfile = serde_json::from_value(record.payload.clone())
                .map_err(|error| AuthoritativeMigrationError::Profile(error.to_string()))?;
            if profile.id().as_entity_id() != &record.id || profile.revision != record.revision {
                return Err(AuthoritativeMigrationError::Profile(
                    "record identity or revision disagrees with its payload".into(),
                ));
            }
            profiles.insert(profile.id().clone());
            if profile.enabled {
                enabled_profiles.insert(profile.id().clone());
            }
        }
        let mut routes = Vec::new();
        for record in self.store.list_records(Collection::RoutingRules)? {
            let stored: StoredChannelRouteWire = serde_json::from_value(record.payload.clone())
                .map_err(|error| AuthoritativeMigrationError::Route(error.to_string()))?;
            if stored.rule.id != record.id {
                return Err(AuthoritativeMigrationError::Route(
                    "route identity disagrees with its payload".into(),
                ));
            }
            routes.push(LegacyRouteInventory {
                route_record_id: record.id,
                route_revision: record.revision,
                profile_id: stored.rule.profile_id,
                session_bindings: stored.session_bindings,
                policy_sha256: hex_digest(
                    &canonical_json_bytes(&record.payload)
                        .map_err(|error| AuthoritativeMigrationError::Route(error.to_string()))?,
                ),
            });
        }
        routes.sort_by(|left, right| left.route_record_id.cmp(&right.route_record_id));
        let mut sessions = Vec::new();
        for manifest in self.sessions.discover()? {
            let (manifest_bytes, history_bytes) = self.sessions.export_raw(&manifest.session_id)?;
            let export = self.sessions.export(&manifest.session_id)?;
            let mut canonical_bytes = manifest_bytes;
            canonical_bytes.push(b'\n');
            canonical_bytes.extend(history_bytes);
            let healthy_root = manifest.kind == SessionKind::Root
                && !manifest.archived
                && manifest
                    .active_leaf
                    .as_ref()
                    .is_some_and(|leaf| export.entries.iter().any(|entry| &entry.id == leaf));
            let updated_at_ms = export
                .entries
                .iter()
                .map(|entry| entry.timestamp.unix_millis())
                .max()
                .unwrap_or(manifest.created_at.unix_millis());
            sessions.push(LegacySessionInventory {
                session_id: manifest.session_id,
                owner_profile_id: manifest.profile_id,
                canonical_bytes,
                source_entry_ids: export
                    .entries
                    .into_iter()
                    .map(|entry| entry.id.as_entity_id().clone())
                    .collect(),
                healthy_root,
                updated_at_ms: u64::try_from(updated_at_ms).unwrap_or(0),
            });
        }
        sessions.sort_by(|left, right| left.session_id.cmp(&right.session_id));
        Ok(TeammateMigrationInventory {
            migration_version: self.migration_version.clone(),
            migrated_at: self.migrated_at,
            default_profile_id: self.default_profile_id.clone(),
            profiles,
            enabled_profiles,
            routes,
            sessions,
            external: self.capture_external_state()?,
        })
    }

    fn capture_external_state(&self) -> Result<ExternalStateSnapshot, AuthoritativeMigrationError> {
        let files = inspect_resource_roots(&self.resource_roots, false, None)?;
        let workspace_digest = resource_kind_digest(&files, ResourceRootKind::Workspace)?;
        let memory_digest = resource_kind_digest(&files, ResourceRootKind::Memory)?;
        let resource_digest = resource_kind_digest(&files, ResourceRootKind::Resource)?;
        let credential_digest = resource_kind_digest(&files, ResourceRootKind::CredentialStore)?;
        let skill_digest = resource_kind_digest(&files, ResourceRootKind::Skill)?;
        let browser_digest = resource_kind_digest(&files, ResourceRootKind::BrowserProfile)?;
        let audit_archive_digest = resource_kind_digest(&files, ResourceRootKind::AuditArchive)?;
        let session_bytes_digest = session_store_digest(self.sessions)?;
        let route_policy_digest = collection_digest(self.store, Collection::RoutingRules)?;
        let schedule_digest = collection_digest(self.store, Collection::ScheduledJobs)?;
        let profile_digest = collection_digest(self.store, Collection::Profiles)?;
        let child_bounds_digest = collection_digest(self.store, Collection::Children)?;
        let shared_grant_digest = collection_digest(self.store, Collection::SharedKnowledgeGrants)?;
        let shared_space_digest = collection_digest(self.store, Collection::SharedKnowledgeSpaces)?;
        Ok(ExternalStateSnapshot {
            workspace_digest,
            memory_digest,
            session_bytes_digest,
            route_policy_digest,
            schedule_digest,
            permission_digest: profile_digest.clone(),
            credential_store_digest: credential_digest,
            child_bounds_digest,
            shared_data_classification_digest: hex_digest(
                format!(
                    "{shared_grant_digest}:{shared_space_digest}:{resource_digest}:{skill_digest}:{browser_digest}:{audit_archive_digest}"
                )
                .as_bytes(),
            ),
        })
    }
}

pub fn run_authoritative_teammate_migration(
    source: &AuthoritativeMigrationSource<'_>,
    backup_root: &Path,
    fresh_root: &Path,
) -> Result<AuthoritativeMigrationReport, AuthoritativeMigrationError> {
    if fresh_root.exists() {
        return Err(AuthoritativeMigrationError::Resource(
            "fresh root must not exist".into(),
        ));
    }
    let inventory = source.capture_inventory()?;
    let plan = TeammateMigrationPlan::build(&inventory)?;
    let mut roots = source.resource_roots.clone();
    roots.push(NamedResourceRoot {
        name: "sessions".into(),
        kind: ResourceRootKind::Resource,
        path: source.sessions.root().to_path_buf(),
    });
    let manifest = if backup_root.exists() {
        load_resource_backup_manifest(backup_root)?
    } else {
        let staging_root = sibling_staging_path(backup_root, "backup")?;
        fs::create_dir(&staging_root).map_err(resource_error)?;
        let staging_store = staging_root.join("state.sqlite");
        source.store.backup_to(&staging_store)?;
        let manifest = create_resource_backup(
            &roots,
            &staging_root,
            &staging_store,
            source.store.schema_version()?,
        )?;
        verify_resource_backup(&staging_root, &manifest)?;
        fs::rename(&staging_root, backup_root).map_err(resource_error)?;
        manifest
    };
    let store_backup = backup_root.join("state.sqlite");
    verify_resource_backup(backup_root, &manifest)?;
    let already_applied = source
        .store
        .teammate_migration_applied(&source.migration_version, &plan.mutations)?;
    let oracle =
        TeammateMigrationOracle::capture_with_external(source.store, inventory.external.clone())?;
    let backup_store = EmbeddedStore::open(&store_backup, Some(&FileBackupHook))?;
    let backup_snapshot = capture_snapshot(&backup_store, inventory.external.clone())?;
    if !already_applied && oracle.before() != &backup_snapshot {
        return Err(AuthoritativeMigrationError::Resource(
            "state backup differs from authoritative source".into(),
        ));
    }
    drop(backup_store);
    let restore_staging = sibling_staging_path(fresh_root, "restore")?;
    restore_resource_backup(backup_root, &manifest, &restore_staging)?;
    let restored_store_path = restore_staging.join("state.sqlite");
    EmbeddedStore::restore_backup_to(&store_backup, &restored_store_path)?;
    let restored_store = EmbeddedStore::open(&restored_store_path, Some(&FileBackupHook))?;
    let restored_sessions = SessionStore::open(restore_staging.join("sessions"))?;
    let restored_external = AuthoritativeMigrationSource {
        store: &restored_store,
        sessions: &restored_sessions,
        default_profile_id: source.default_profile_id.clone(),
        migration_version: source.migration_version.clone(),
        migrated_at: source.migrated_at,
        resource_roots: source
            .resource_roots
            .iter()
            .map(|root| NamedResourceRoot {
                name: root.name.clone(),
                kind: root.kind,
                path: restore_staging.join(&root.name),
            })
            .collect(),
    }
    .capture_external_state()?;
    if restored_external != inventory.external {
        return Err(AuthoritativeMigrationError::Resource(
            "restored resources differ from authoritative inventory".into(),
        ));
    }
    drop(restored_sessions);
    drop(restored_store);
    fs::rename(&restore_staging, fresh_root).map_err(resource_error)?;
    let migration_backup = backup_root.join("migration-write.sqlite");
    let outcome = source.store.migrate_teammates(
        &source.migration_version,
        &migration_backup,
        &plan.mutations,
    )?;
    let expected_bindings = plan
        .mutations
        .iter()
        .filter_map(|mutation| match mutation {
            RecordMutation::Put {
                collection: Collection::ConversationBindings,
                record,
                ..
            } => Some(record.clone()),
            _ => None,
        })
        .collect::<Vec<_>>();
    if source
        .store
        .list_records(Collection::ConversationBindings)?
        != expected_bindings
    {
        return Err(TeammateMigrationOracleError::ConversationBindingMismatch.into());
    }
    let oracle_report = if already_applied {
        TeammateMigrationOracleReport {
            preserved_collections: TeammateMigrationOracle::SNAPSHOT
                .iter()
                .copied()
                .filter(|collection| !migration_mutates(*collection))
                .map(|collection| collection.as_str().to_owned())
                .collect(),
            route_identities_preserved: true,
            backup_restored_into_fresh_root: true,
        }
    } else {
        oracle.verify_with_external(
            source.store,
            &store_backup,
            &fresh_root.join("oracle-state.sqlite"),
            source.capture_external_state()?,
        )?
    };
    Ok(AuthoritativeMigrationReport {
        outcome,
        selected_keith_session: plan.selected_keith_session,
        inventory_digest: plan.inventory_digest,
        backup_manifest: manifest,
        oracle: oracle_report,
    })
}

fn create_resource_backup(
    roots: &[NamedResourceRoot],
    backup_root: &Path,
    state_backup: &Path,
    store_schema_version: u32,
) -> Result<ResourceBackupManifest, AuthoritativeMigrationError> {
    let entries = inspect_resource_roots(roots, true, Some(backup_root))?;
    let state_metadata = fs::symlink_metadata(state_backup).map_err(resource_error)?;
    if state_metadata.file_type().is_symlink() || !state_metadata.is_file() {
        return Err(AuthoritativeMigrationError::Resource(
            "state backup must be a regular file".into(),
        ));
    }
    let state_bytes = read_bounded_file(state_backup, state_metadata.len())?;
    let state_byte_length = state_metadata.len();
    let total_bytes = entries
        .iter()
        .try_fold(state_byte_length, |total, entry| {
            total.checked_add(entry.byte_length)
        })
        .ok_or_else(|| AuthoritativeMigrationError::Resource("backup size overflow".into()))?;
    if total_bytes > MAX_BACKUP_BYTES {
        return Err(AuthoritativeMigrationError::Resource(
            "resource backup exceeds configured bounds".into(),
        ));
    }
    let state_sha256 = hex_digest(&state_bytes);
    let manifest_digest = manifest_digest(
        store_schema_version,
        state_byte_length,
        &state_sha256,
        &entries,
        total_bytes,
    )?;
    let manifest = ResourceBackupManifest {
        version: RESOURCE_BACKUP_FORMAT_VERSION,
        store_schema_version,
        state_byte_length,
        state_sha256,
        entries,
        total_bytes,
        manifest_digest,
    };
    let bytes = canonical_json_bytes(&manifest)
        .map_err(|error| AuthoritativeMigrationError::Resource(error.to_string()))?;
    let path = backup_root.join("resources.manifest.json");
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(path)
        .map_err(resource_error)?;
    file.write_all(&bytes).map_err(resource_error)?;
    file.sync_all().map_err(resource_error)?;
    Ok(manifest)
}

fn load_resource_backup_manifest(
    backup_root: &Path,
) -> Result<ResourceBackupManifest, AuthoritativeMigrationError> {
    let path = backup_root.join("resources.manifest.json");
    let metadata = fs::symlink_metadata(&path).map_err(resource_error)?;
    if metadata.file_type().is_symlink() || !metadata.is_file() || metadata.len() > 64 * 1024 * 1024
    {
        return Err(AuthoritativeMigrationError::Resource(
            "backup manifest file is invalid".into(),
        ));
    }
    let bytes = read_bounded_file(&path, metadata.len())?;
    let manifest: ResourceBackupManifest = serde_json::from_slice(&bytes)
        .map_err(|error| AuthoritativeMigrationError::Resource(error.to_string()))?;
    let canonical = canonical_json_bytes(&manifest)
        .map_err(|error| AuthoritativeMigrationError::Resource(error.to_string()))?;
    if bytes != canonical {
        return Err(AuthoritativeMigrationError::Resource(
            "backup manifest is not canonical".into(),
        ));
    }
    Ok(manifest)
}

pub fn verify_resource_backup(
    backup_root: &Path,
    manifest: &ResourceBackupManifest,
) -> Result<(), AuthoritativeMigrationError> {
    if manifest.version != RESOURCE_BACKUP_FORMAT_VERSION
        || manifest.entries.len() > MAX_BACKUP_FILES
        || manifest.total_bytes > MAX_BACKUP_BYTES
        || manifest.manifest_digest
            != manifest_digest(
                manifest.store_schema_version,
                manifest.state_byte_length,
                &manifest.state_sha256,
                &manifest.entries,
                manifest.total_bytes,
            )?
    {
        return Err(AuthoritativeMigrationError::Resource(
            "backup manifest is invalid".into(),
        ));
    }
    let state_path = backup_root.join("state.sqlite");
    let state_metadata = fs::symlink_metadata(&state_path).map_err(resource_error)?;
    if state_metadata.file_type().is_symlink()
        || !state_metadata.is_file()
        || state_metadata.len() != manifest.state_byte_length
    {
        return Err(AuthoritativeMigrationError::Resource(
            "state backup metadata differs from its manifest".into(),
        ));
    }
    let state_bytes = read_bounded_file(&state_path, manifest.state_byte_length)?;
    if hex_digest(&state_bytes) != manifest.state_sha256 {
        return Err(AuthoritativeMigrationError::Resource(
            "state backup digest mismatch".into(),
        ));
    }
    let verified = EmbeddedStore::verify_backup(&state_path)?;
    if verified.schema_version != manifest.store_schema_version {
        return Err(AuthoritativeMigrationError::Resource(
            "state backup schema differs from its manifest".into(),
        ));
    }
    let mut total = manifest.state_byte_length;
    for entry in &manifest.entries {
        validate_relative(&entry.relative_path)?;
        validate_root_name(&entry.root_name)?;
        let path = backup_root
            .join("resources")
            .join(&entry.root_name)
            .join(&entry.relative_path);
        let metadata = fs::symlink_metadata(&path).map_err(resource_error)?;
        if metadata.file_type().is_symlink() || !metadata.is_file() {
            return Err(AuthoritativeMigrationError::Resource(
                "backup contains a non-regular file".into(),
            ));
        }
        let bytes = read_bounded_file(&path, entry.byte_length)?;
        if hex_digest(&bytes) != entry.sha256 {
            return Err(AuthoritativeMigrationError::Resource(
                "backup file digest mismatch".into(),
            ));
        }
        total = total.saturating_add(entry.byte_length);
    }
    if total != manifest.total_bytes {
        return Err(AuthoritativeMigrationError::Resource(
            "backup byte count mismatch".into(),
        ));
    }
    Ok(())
}

pub fn restore_resource_backup(
    backup_root: &Path,
    manifest: &ResourceBackupManifest,
    fresh_root: &Path,
) -> Result<(), AuthoritativeMigrationError> {
    verify_resource_backup(backup_root, manifest)?;
    fs::create_dir(fresh_root).map_err(resource_error)?;
    for entry in &manifest.entries {
        let source = backup_root
            .join("resources")
            .join(&entry.root_name)
            .join(&entry.relative_path);
        let destination = fresh_root.join(&entry.root_name).join(&entry.relative_path);
        if let Some(parent) = destination.parent() {
            fs::create_dir_all(parent).map_err(resource_error)?;
        }
        fs::copy(source, destination).map_err(resource_error)?;
    }
    verify_restored_resources(fresh_root, manifest)?;
    Ok(())
}

fn verify_restored_resources(
    restored_root: &Path,
    manifest: &ResourceBackupManifest,
) -> Result<(), AuthoritativeMigrationError> {
    for entry in &manifest.entries {
        let path = restored_root
            .join(&entry.root_name)
            .join(&entry.relative_path);
        let metadata = fs::symlink_metadata(&path).map_err(resource_error)?;
        if metadata.file_type().is_symlink()
            || !metadata.is_file()
            || metadata.len() != entry.byte_length
        {
            return Err(AuthoritativeMigrationError::Resource(
                "restored resource metadata mismatch".into(),
            ));
        }
        let bytes = read_bounded_file(&path, entry.byte_length)?;
        if hex_digest(&bytes) != entry.sha256 {
            return Err(AuthoritativeMigrationError::Resource(
                "restored resource digest mismatch".into(),
            ));
        }
    }
    Ok(())
}

/// Restores the complete state/resource bundle into a new root through an atomic staging path.
/// Existing destinations are never overwritten; interrupted staging trees are never activated.
pub fn restore_authoritative_backup(
    backup_root: &Path,
    fresh_root: &Path,
) -> Result<AuthoritativeRestoreReport, AuthoritativeMigrationError> {
    if fresh_root.exists() {
        return Err(AuthoritativeMigrationError::Resource(
            "fresh root must not exist".into(),
        ));
    }
    let manifest = load_resource_backup_manifest(backup_root)?;
    verify_resource_backup(backup_root, &manifest)?;
    let staging = sibling_staging_path(fresh_root, "restore")?;
    restore_resource_backup(backup_root, &manifest, &staging)?;
    EmbeddedStore::restore_backup_to(
        &backup_root.join("state.sqlite"),
        &staging.join("state.sqlite"),
    )?;
    verify_resource_backup(backup_root, &manifest)?;
    EmbeddedStore::verify_backup(&staging.join("state.sqlite"))?;
    fs::rename(&staging, fresh_root).map_err(resource_error)?;
    Ok(AuthoritativeRestoreReport {
        manifest_digest: manifest.manifest_digest,
        store_schema_version: manifest.store_schema_version,
        restored_resource_files: manifest.entries.len(),
        restored_bytes: manifest.total_bytes,
    })
}

fn inspect_resource_roots(
    roots: &[NamedResourceRoot],
    copy: bool,
    backup_root: Option<&Path>,
) -> Result<Vec<ResourceBackupEntry>, AuthoritativeMigrationError> {
    let mut names = BTreeSet::new();
    let mut entries = Vec::new();
    let mut total = 0_u64;
    for root in roots {
        validate_root_name(&root.name)?;
        if !names.insert(root.name.clone()) {
            return Err(AuthoritativeMigrationError::Resource(
                "resource root names must be unique".into(),
            ));
        }
        let metadata = fs::symlink_metadata(&root.path).map_err(resource_error)?;
        if metadata.file_type().is_symlink() || !metadata.is_dir() {
            return Err(AuthoritativeMigrationError::Resource(
                "resource root must be a real directory".into(),
            ));
        }
        walk_root(
            root,
            &root.path,
            copy,
            backup_root,
            &mut entries,
            &mut total,
        )?;
    }
    entries.sort_by(|left, right| {
        (&left.root_name, &left.relative_path).cmp(&(&right.root_name, &right.relative_path))
    });
    Ok(entries)
}

fn walk_root(
    root: &NamedResourceRoot,
    directory: &Path,
    copy: bool,
    backup_root: Option<&Path>,
    entries: &mut Vec<ResourceBackupEntry>,
    total: &mut u64,
) -> Result<(), AuthoritativeMigrationError> {
    let mut children = fs::read_dir(directory)
        .map_err(resource_error)?
        .collect::<Result<Vec<_>, _>>()
        .map_err(resource_error)?;
    children.sort_by_key(fs::DirEntry::file_name);
    for child in children {
        let path = child.path();
        let metadata = fs::symlink_metadata(&path).map_err(resource_error)?;
        if metadata.file_type().is_symlink() {
            return Err(AuthoritativeMigrationError::Resource(
                "symlinks are forbidden in migration resources".into(),
            ));
        }
        if metadata.is_dir() {
            walk_root(root, &path, copy, backup_root, entries, total)?;
            continue;
        }
        if !metadata.is_file() {
            return Err(AuthoritativeMigrationError::Resource(
                "non-regular migration resource".into(),
            ));
        }
        let relative = path
            .strip_prefix(&root.path)
            .map_err(|error| AuthoritativeMigrationError::Resource(error.to_string()))?
            .to_path_buf();
        validate_relative(&relative)?;
        let bytes = read_bounded_file(&path, metadata.len())?;
        *total = total.saturating_add(metadata.len());
        if entries.len() >= MAX_BACKUP_FILES || *total > MAX_BACKUP_BYTES {
            return Err(AuthoritativeMigrationError::Resource(
                "resource backup exceeds configured bounds".into(),
            ));
        }
        if copy {
            let destination = backup_root
                .ok_or_else(|| AuthoritativeMigrationError::Resource("backup root missing".into()))?
                .join("resources")
                .join(&root.name)
                .join(&relative);
            if let Some(parent) = destination.parent() {
                fs::create_dir_all(parent).map_err(resource_error)?;
            }
            let mut file = OpenOptions::new()
                .create_new(true)
                .write(true)
                .open(destination)
                .map_err(resource_error)?;
            file.write_all(&bytes).map_err(resource_error)?;
            file.sync_all().map_err(resource_error)?;
        }
        entries.push(ResourceBackupEntry {
            root_name: root.name.clone(),
            root_kind: root.kind,
            relative_path: relative,
            byte_length: metadata.len(),
            sha256: hex_digest(&bytes),
        });
    }
    Ok(())
}

fn resource_kind_digest(
    entries: &[ResourceBackupEntry],
    kind: ResourceRootKind,
) -> Result<String, AuthoritativeMigrationError> {
    let selected = entries
        .iter()
        .filter(|entry| entry.root_kind == kind)
        .collect::<Vec<_>>();
    canonical_json_bytes(&selected)
        .map(|bytes| hex_digest(&bytes))
        .map_err(|error| AuthoritativeMigrationError::Resource(error.to_string()))
}

fn session_store_digest(sessions: &SessionStore) -> Result<String, AuthoritativeMigrationError> {
    let mut content = Vec::new();
    for manifest in sessions.discover()? {
        let (manifest_bytes, history_bytes) = sessions.export_raw(&manifest.session_id)?;
        content.push((manifest.session_id, manifest_bytes, history_bytes));
    }
    canonical_json_bytes(&content)
        .map(|bytes| hex_digest(&bytes))
        .map_err(|error| AuthoritativeMigrationError::Resource(error.to_string()))
}

fn collection_digest(
    store: &EmbeddedStore,
    collection: Collection,
) -> Result<String, AuthoritativeMigrationError> {
    records_digest(&store.list_records(collection)?)
        .map_err(|error| AuthoritativeMigrationError::Resource(error.to_string()))
}

fn manifest_digest(
    store_schema_version: u32,
    state_byte_length: u64,
    state_sha256: &str,
    entries: &[ResourceBackupEntry],
    total_bytes: u64,
) -> Result<String, AuthoritativeMigrationError> {
    canonical_json_bytes(&(
        RESOURCE_BACKUP_FORMAT_VERSION,
        store_schema_version,
        state_byte_length,
        state_sha256,
        entries,
        total_bytes,
    ))
    .map(|bytes| hex_digest(&bytes))
    .map_err(|error| AuthoritativeMigrationError::Resource(error.to_string()))
}

fn validate_root_name(name: &str) -> Result<(), AuthoritativeMigrationError> {
    if name.is_empty()
        || name.len() > 128
        || !name
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_'))
    {
        return Err(AuthoritativeMigrationError::Resource(
            "resource root name is invalid".into(),
        ));
    }
    Ok(())
}

fn validate_relative(path: &Path) -> Result<(), AuthoritativeMigrationError> {
    if path.as_os_str().is_empty()
        || path.is_absolute()
        || path.components().any(|component| {
            matches!(
                component,
                Component::ParentDir | Component::RootDir | Component::Prefix(_)
            )
        })
    {
        return Err(AuthoritativeMigrationError::Resource(
            "resource path is unsafe".into(),
        ));
    }
    Ok(())
}

fn read_bounded_file(path: &Path, expected: u64) -> Result<Vec<u8>, AuthoritativeMigrationError> {
    if expected > MAX_BACKUP_BYTES {
        return Err(AuthoritativeMigrationError::Resource(
            "resource file exceeds backup bound".into(),
        ));
    }
    let file = OpenOptions::new()
        .read(true)
        .open(path)
        .map_err(resource_error)?;
    let mut bytes = Vec::new();
    file.take(expected.saturating_add(1))
        .read_to_end(&mut bytes)
        .map_err(resource_error)?;
    if u64::try_from(bytes.len()).ok() != Some(expected) {
        return Err(AuthoritativeMigrationError::Resource(
            "resource file changed while being captured".into(),
        ));
    }
    Ok(bytes)
}

fn sibling_staging_path(
    target: &Path,
    operation: &str,
) -> Result<PathBuf, AuthoritativeMigrationError> {
    if target.file_name().is_none() {
        return Err(AuthoritativeMigrationError::Resource(
            "backup or restore root must have a terminal path component".into(),
        ));
    }
    if let Some(parent) = target.parent() {
        fs::create_dir_all(parent).map_err(resource_error)?;
    }
    Ok(target.with_extension(format!("{operation}-partial-{}", EntityId::new())))
}

#[allow(clippy::needless_pass_by_value)]
fn resource_error(error: io::Error) -> AuthoritativeMigrationError {
    AuthoritativeMigrationError::Resource(error.to_string())
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LegacySessionInventory {
    pub session_id: SessionId,
    pub owner_profile_id: ProfileId,
    pub canonical_bytes: Vec<u8>,
    pub source_entry_ids: Vec<EntityId>,
    pub healthy_root: bool,
    pub updated_at_ms: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammateMigrationInventory {
    pub migration_version: String,
    pub migrated_at: UtcTimestamp,
    pub default_profile_id: ProfileId,
    pub profiles: BTreeSet<ProfileId>,
    pub enabled_profiles: BTreeSet<ProfileId>,
    pub routes: Vec<LegacyRouteInventory>,
    pub sessions: Vec<LegacySessionInventory>,
    pub external: ExternalStateSnapshot,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LegacyRouteInventory {
    pub route_record_id: EntityId,
    pub route_revision: Revision,
    pub profile_id: ProfileId,
    pub session_bindings: BTreeMap<String, SessionId>,
    pub policy_sha256: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StoredChannelRouteWire {
    rule: ChannelRouteRule,
    session_bindings: BTreeMap<String, SessionId>,
}

#[derive(Clone, Debug, PartialEq)]
pub struct TeammateMigrationPlan {
    pub mutations: Vec<RecordMutation>,
    pub selected_keith_session: Option<SessionId>,
    pub inventory_digest: String,
}

#[derive(Debug, Error)]
pub enum TeammateMigrationPlanError {
    #[error("default profile is absent from the migration inventory")]
    MissingDefaultProfile,
    #[error("legacy session {0} appears more than once")]
    DuplicateSession(SessionId),
    #[error("legacy session bytes exceed the 16 MiB migration bound")]
    SessionTooLarge,
    #[error("migration inventory encoding failed: {0}")]
    Encoding(String),
}

impl TeammateMigrationPlan {
    #[allow(clippy::too_many_lines)]
    pub fn build(
        inventory: &TeammateMigrationInventory,
    ) -> Result<Self, TeammateMigrationPlanError> {
        if !inventory
            .enabled_profiles
            .contains(&inventory.default_profile_id)
        {
            return Err(TeammateMigrationPlanError::MissingDefaultProfile);
        }
        let bytes = canonical_json_bytes(inventory)
            .map_err(|error| TeammateMigrationPlanError::Encoding(error.to_string()))?;
        let inventory_digest = hex_digest(&bytes);
        let selected = inventory
            .sessions
            .iter()
            .filter(|session| {
                session.owner_profile_id == inventory.default_profile_id && session.healthy_root
            })
            .max_by_key(|session| (session.updated_at_ms, session.session_id.clone()))
            .map(|session| session.session_id.clone());
        let mut seen = BTreeSet::new();
        let mut mutations = Vec::new();
        let mut dm_profiles = inventory.enabled_profiles.clone();
        for route in &inventory.routes {
            if !inventory.profiles.contains(&route.profile_id) {
                return Err(TeammateMigrationPlanError::Encoding(format!(
                    "route {} references an absent profile",
                    route.route_record_id
                )));
            }
            dm_profiles.insert(route.profile_id.clone());
        }
        for profile_id in &dm_profiles {
            if profile_id == &inventory.default_profile_id && selected.is_some() {
                continue;
            }
            append_empty_human_dm(&mut mutations, inventory, profile_id)?;
        }
        for session in &inventory.sessions {
            if !seen.insert(session.session_id.clone()) {
                return Err(TeammateMigrationPlanError::DuplicateSession(
                    session.session_id.clone(),
                ));
            }
            if session.canonical_bytes.len() > 16 * 1024 * 1024 {
                return Err(TeammateMigrationPlanError::SessionTooLarge);
            }
            if selected.as_ref() == Some(&session.session_id) {
                let raw_conversation_id = deterministic_id(
                    "keith-dm",
                    inventory.default_profile_id.to_string().as_bytes(),
                );
                let conversation_id = ConversationId::from(raw_conversation_id.clone());
                let event_id = EventId::from(deterministic_id(
                    "keith-dm-event",
                    session.session_id.to_string().as_bytes(),
                ));
                let conversation = ConversationRecord {
                    schema_version: CURRENT_SCHEMA_VERSION,
                    id: conversation_id.clone(),
                    kind: ConversationKind::HumanAgentDm,
                    lifecycle: ConversationLifecycle::Active,
                    title: "Keith".into(),
                    creator: Principal::System,
                    created_at: inventory.migrated_at,
                    updated_at: inventory.migrated_at,
                    revision: Revision::new(3),
                    participant_revision: Revision::new(2),
                    participant_profiles: BTreeSet::from([inventory.default_profile_id.clone()]),
                    human_participant: true,
                    event_head: Some(EventHead {
                        sequence: 1,
                        event_id: event_id.clone(),
                    }),
                };
                conversation
                    .validate()
                    .map_err(|error| TeammateMigrationPlanError::Encoding(error.to_string()))?;
                mutations.push(typed_put(
                    Collection::Conversations,
                    raw_conversation_id,
                    inventory.migrated_at,
                    conversation.revision,
                    &conversation,
                )?);
                append_human_dm_key(
                    &mut mutations,
                    inventory,
                    &inventory.default_profile_id,
                    &conversation_id,
                )?;
                for (principal, role) in [
                    (ParticipantPrincipal::Human, ParticipantRole::Owner),
                    (
                        ParticipantPrincipal::Agent(inventory.default_profile_id.clone()),
                        ParticipantRole::Member,
                    ),
                ] {
                    let participant = ConversationParticipant {
                        schema_version: CURRENT_SCHEMA_VERSION,
                        conversation_id: conversation_id.clone(),
                        principal,
                        role,
                        joined_at: inventory.migrated_at,
                        left_at: None,
                        revision: Revision::ZERO,
                        applied_through_sequence: 1,
                        hidden: false,
                        muted: false,
                        notification_policy: NotificationPolicy {
                            mentions_only: false,
                            muted: false,
                        },
                    };
                    participant
                        .validate()
                        .map_err(|error| TeammateMigrationPlanError::Encoding(error.to_string()))?;
                    mutations.push(typed_put(
                        Collection::ConversationParticipants,
                        conversation_compound_id(
                            &conversation_id.to_string(),
                            &format!("{:?}", participant.principal),
                        ),
                        inventory.migrated_at,
                        participant.revision,
                        &participant,
                    )?);
                }
                let event = ConversationEvent {
                    schema_version: CURRENT_SCHEMA_VERSION,
                    id: event_id.clone(),
                    conversation_id,
                    sequence: 1,
                    publication_key: StableKey::parse(format!(
                        "migration/{}/session/{}",
                        inventory.migration_version, session.session_id
                    ))
                    .map_err(|error| TeammateMigrationPlanError::Encoding(error.to_string()))?,
                    author: Principal::System,
                    timestamp: inventory.migrated_at,
                    kind: ConversationEventKind::SystemNotice,
                    content: Some("A pre-upgrade Keith session is retained in Legacy.".into()),
                    artifacts: Vec::new(),
                    reply_to: None,
                    thread_parent: None,
                    provenance: EventProvenance {
                        source: "legacy_session".into(),
                        source_ids: vec![session.session_id.to_string()],
                        migration_version: Some(inventory.migration_version.clone()),
                    },
                };
                event
                    .validate()
                    .map_err(|error| TeammateMigrationPlanError::Encoding(error.to_string()))?;
                let event_payload = serde_json::json!({
                    "record_kind": "event",
                    "event": event,
                });
                mutations.push(put(
                    Collection::ConversationEvents,
                    event_id.as_entity_id().clone(),
                    inventory.migrated_at,
                    event_payload,
                ));
                let event_digest =
                    hex_digest(&serde_json::to_vec(&event).map_err(|error| {
                        TeammateMigrationPlanError::Encoding(error.to_string())
                    })?);
                mutations.push(put(
                    Collection::ConversationStableKeys,
                    conversation_compound_id(
                        "conversation-publication-key",
                        event.publication_key.as_str(),
                    ),
                    inventory.migrated_at,
                    serde_json::json!({
                        "record_kind": "publication_key",
                        "key": event.publication_key,
                        "event_id": event.id,
                        "event_digest": event_digest,
                    }),
                ));
                for collection in [
                    Collection::ConversationProjectionIntents,
                    Collection::ConversationUnreadIntents,
                    Collection::ConversationSearchIntents,
                    Collection::ConversationPublicationIntents,
                ] {
                    mutations.push(put(
                        collection,
                        conversation_compound_id(collection.as_str(), &event.id.to_string()),
                        inventory.migrated_at,
                        serde_json::json!({
                            "conversation_id": event.conversation_id,
                            "event_id": event.id,
                            "sequence": event.sequence,
                            "publication_key": event.publication_key,
                        }),
                    ));
                }
                let provenance_id = deterministic_id(
                    "keith-dm-provenance",
                    session.session_id.to_string().as_bytes(),
                );
                mutations.push(put(Collection::MigrationProvenance, provenance_id, inventory.migrated_at, serde_json::json!({
                    "stable_key": format!("migration:{}:seed:{}", inventory.migration_version, session.session_id),
                    "migration_version": inventory.migration_version,
                    "source_session_id": session.session_id,
                    "source_entry_ids": session.source_entry_ids,
                    "source_bytes_sha256": hex_digest(&session.canonical_bytes),
                    "migrated_at": inventory.migrated_at,
                })));
            }
            let id = deterministic_id("legacy-session", session.session_id.to_string().as_bytes());
            mutations.push(put(
                Collection::LegacySessions,
                id,
                inventory.migrated_at,
                serde_json::json!({
                    "stable_key": format!("legacy-session:{}", session.session_id),
                    "session_id": session.session_id,
                    "owner_profile_id": session.owner_profile_id,
                    "canonical_bytes_hex": hex_bytes(&session.canonical_bytes),
                    "canonical_bytes_sha256": hex_digest(&session.canonical_bytes),
                }),
            ));
        }
        for route in &inventory.routes {
            let conversation_id = ConversationId::from(deterministic_id(
                "keith-dm",
                route.profile_id.to_string().as_bytes(),
            ));
            let id = deterministic_id(
                "conversation-binding",
                route.route_record_id.to_string().as_bytes(),
            );
            mutations.push(typed_put(
                Collection::ConversationBindings,
                id,
                inventory.migrated_at,
                Revision::ZERO,
                &ConversationBinding {
                    schema_version: CURRENT_SCHEMA_VERSION,
                    route_id: route.route_record_id.clone(),
                    route_revision: route.route_revision,
                    profile_id: route.profile_id.clone(),
                    conversation_id,
                    legacy_session_bindings: route.session_bindings.clone(),
                    route_policy_sha256: route.policy_sha256.clone(),
                    revision: Revision::ZERO,
                },
            )?);
        }
        let summary_id = deterministic_id(
            "migration-provenance",
            inventory.migration_version.as_bytes(),
        );
        mutations.push(put(
            Collection::MigrationProvenance,
            summary_id,
            inventory.migrated_at,
            serde_json::json!({
                "stable_key": format!("migration:{}:inventory", inventory.migration_version),
                "migration_version": inventory.migration_version,
                "inventory_sha256": inventory_digest,
                "external_digests": inventory.external,
            }),
        ));
        Ok(Self {
            mutations,
            selected_keith_session: selected,
            inventory_digest,
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationBinding {
    pub schema_version: keith_agent_types::SchemaVersion,
    pub route_id: EntityId,
    pub route_revision: Revision,
    pub profile_id: ProfileId,
    pub conversation_id: ConversationId,
    pub legacy_session_bindings: BTreeMap<String, SessionId>,
    pub route_policy_sha256: String,
    pub revision: Revision,
}

fn append_empty_human_dm(
    mutations: &mut Vec<RecordMutation>,
    inventory: &TeammateMigrationInventory,
    profile_id: &ProfileId,
) -> Result<(), TeammateMigrationPlanError> {
    let raw_id = deterministic_id("keith-dm", profile_id.to_string().as_bytes());
    let conversation_id = ConversationId::from(raw_id.clone());
    let conversation = ConversationRecord {
        schema_version: CURRENT_SCHEMA_VERSION,
        id: conversation_id.clone(),
        kind: ConversationKind::HumanAgentDm,
        lifecycle: ConversationLifecycle::Active,
        title: "Keith".into(),
        creator: Principal::System,
        created_at: inventory.migrated_at,
        updated_at: inventory.migrated_at,
        revision: Revision::new(2),
        participant_revision: Revision::new(2),
        participant_profiles: BTreeSet::from([profile_id.clone()]),
        human_participant: true,
        event_head: None,
    };
    conversation
        .validate()
        .map_err(|error| TeammateMigrationPlanError::Encoding(error.to_string()))?;
    mutations.push(typed_put(
        Collection::Conversations,
        raw_id,
        inventory.migrated_at,
        conversation.revision,
        &conversation,
    )?);
    append_human_dm_key(mutations, inventory, profile_id, &conversation_id)?;
    for (principal, role) in [
        (ParticipantPrincipal::Human, ParticipantRole::Owner),
        (
            ParticipantPrincipal::Agent(profile_id.clone()),
            ParticipantRole::Member,
        ),
    ] {
        let participant = ConversationParticipant {
            schema_version: CURRENT_SCHEMA_VERSION,
            conversation_id: conversation_id.clone(),
            principal,
            role,
            joined_at: inventory.migrated_at,
            left_at: None,
            revision: Revision::ZERO,
            applied_through_sequence: 0,
            hidden: false,
            muted: false,
            notification_policy: NotificationPolicy {
                mentions_only: false,
                muted: false,
            },
        };
        participant
            .validate()
            .map_err(|error| TeammateMigrationPlanError::Encoding(error.to_string()))?;
        mutations.push(typed_put(
            Collection::ConversationParticipants,
            conversation_compound_id(
                &conversation_id.to_string(),
                &format!("{:?}", participant.principal),
            ),
            inventory.migrated_at,
            participant.revision,
            &participant,
        )?);
    }
    Ok(())
}

fn append_human_dm_key(
    mutations: &mut Vec<RecordMutation>,
    inventory: &TeammateMigrationInventory,
    profile_id: &ProfileId,
    conversation_id: &ConversationId,
) -> Result<(), TeammateMigrationPlanError> {
    let key = HumanAgentDmKey::new(profile_id, conversation_id)
        .map_err(|error| TeammateMigrationPlanError::Encoding(error.to_string()))?;
    mutations.push(typed_put(
        Collection::DirectMessageKeys,
        conversation_compound_id("human-agent-dm-key", &profile_id.to_string()),
        inventory.migrated_at,
        Revision::ZERO,
        &key,
    )?);
    Ok(())
}

fn typed_put<T: Serialize>(
    collection: Collection,
    id: EntityId,
    updated_at: UtcTimestamp,
    revision: Revision,
    value: &T,
) -> Result<RecordMutation, TeammateMigrationPlanError> {
    let payload = serde_json::to_value(value)
        .map_err(|error| TeammateMigrationPlanError::Encoding(error.to_string()))?;
    Ok(RecordMutation::Put {
        collection,
        record: VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id,
            revision,
            updated_at,
            payload,
        },
        precondition: WritePrecondition::Missing,
    })
}

fn put(
    collection: Collection,
    id: EntityId,
    updated_at: UtcTimestamp,
    payload: serde_json::Value,
) -> RecordMutation {
    RecordMutation::Put {
        collection,
        record: VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id,
            revision: Revision::ZERO,
            updated_at,
            payload,
        },
        precondition: WritePrecondition::Missing,
    }
}

fn deterministic_id(domain: &str, value: &[u8]) -> EntityId {
    let mut hasher = Sha256::new();
    hasher.update(domain.as_bytes());
    hasher.update([0]);
    hasher.update(value);
    let digest = hasher.finalize();
    EntityId::from_u128(u128::from_be_bytes(
        digest[..16].try_into().expect("fixed digest"),
    ))
}

fn conversation_compound_id(left: &str, right: &str) -> EntityId {
    let digest = Sha256::digest(format!("{left}\0{right}").as_bytes());
    EntityId::from_u128(u128::from_be_bytes(
        digest[..16].try_into().expect("fixed digest"),
    ))
}

fn hex_digest(value: &[u8]) -> String {
    hex_bytes(&Sha256::digest(value))
}

fn hex_bytes(value: &[u8]) -> String {
    let mut output = String::with_capacity(value.len() * 2);
    for byte in value {
        use std::fmt::Write as _;
        write!(&mut output, "{byte:02x}").expect("String writes cannot fail");
    }
    output
}

fn records_digest(records: &[VersionedRecord]) -> Result<String, TeammateMigrationOracleError> {
    let bytes = canonical_json_bytes(&records)
        .map_err(|error| TeammateMigrationOracleError::Encoding(error.to_string()))?;
    let mut output = String::with_capacity(64);
    for byte in Sha256::digest(bytes) {
        use std::fmt::Write as _;
        write!(&mut output, "{byte:02x}").expect("writing to a String cannot fail");
    }
    Ok(output)
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MigrationCase {
    LegacyConfiguration,
    ConfigurationLayering,
    InvalidConfigurationFallback,
    LegacyStateBackup,
    FailedStateRollback,
    ProtocolNegotiation,
    LegacySessionExport,
    WorkspaceSchema,
    DerivedIndexRebuild,
    PluginMismatch,
    PluginRollback,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MigrationFaultBoundary {
    BeforeStateBackup,
    AfterStateBackup,
    DuringResourceBackup,
    BeforeBackupManifestPublish,
    BeforeMigrationTransaction,
    DuringMigrationWrite,
    BeforeMigrationCommit,
    AfterMigrationCommitBeforeAck,
    BeforeRestore,
    AfterStateRestore,
    DuringResourceRestore,
    BeforeRestoreActivation,
    ManifestTamper,
    StateBackupTamper,
    ResourceBackupTamper,
}

impl MigrationFaultBoundary {
    pub const ALL: [Self; 15] = [
        Self::BeforeStateBackup,
        Self::AfterStateBackup,
        Self::DuringResourceBackup,
        Self::BeforeBackupManifestPublish,
        Self::BeforeMigrationTransaction,
        Self::DuringMigrationWrite,
        Self::BeforeMigrationCommit,
        Self::AfterMigrationCommitBeforeAck,
        Self::BeforeRestore,
        Self::AfterStateRestore,
        Self::DuringResourceRestore,
        Self::BeforeRestoreActivation,
        Self::ManifestTamper,
        Self::StateBackupTamper,
        Self::ResourceBackupTamper,
    ];

    const fn may_commit_before_interruption(self) -> bool {
        matches!(
            self,
            Self::AfterMigrationCommitBeforeAck
                | Self::BeforeRestore
                | Self::AfterStateRestore
                | Self::DuringResourceRestore
                | Self::BeforeRestoreActivation
        )
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PackagedBinaryCommand {
    pub executable: PathBuf,
    pub arguments: Vec<String>,
    pub current_directory: PathBuf,
    pub environment: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PackagedMigrationFaultCase {
    pub boundary: MigrationFaultBoundary,
    pub interrupted: PackagedBinaryCommand,
    pub replay: PackagedBinaryCommand,
    pub state_path: PathBuf,
    pub sessions_root: PathBuf,
    pub backup_root: PathBuf,
    pub restore_root: PathBuf,
    pub default_profile_id: ProfileId,
    pub migration_version: String,
    pub migrated_at: UtcTimestamp,
    pub resource_roots: Vec<NamedResourceRoot>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanonicalMigrationInvariant {
    pub snapshot: TeammateStateSnapshot,
    pub canonical_sha256: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PackagedProcessEvidence {
    pub executable_sha256: String,
    pub exit_code: Option<i32>,
    pub success: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MigrationFaultEvidence {
    pub boundary: MigrationFaultBoundary,
    pub interrupted_process: PackagedProcessEvidence,
    pub replay_process: PackagedProcessEvidence,
    pub before: CanonicalMigrationInvariant,
    pub after_interruption: CanonicalMigrationInvariant,
    pub after_replay: CanonicalMigrationInvariant,
    pub restored: CanonicalMigrationInvariant,
    pub backup_manifest_digest: String,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesFaultMatrix {
    evidence: BTreeMap<MigrationFaultBoundary, MigrationFaultEvidence>,
}

#[derive(Debug, Error)]
pub enum TeammatesFaultMatrixError {
    #[error(transparent)]
    Migration(#[from] AuthoritativeMigrationError),
    #[error(transparent)]
    Oracle(#[from] TeammateMigrationOracleError),
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error(transparent)]
    Session(#[from] SessionStoreError),
    #[error(transparent)]
    Plan(#[from] TeammateMigrationPlanError),
    #[error("packaged binary evidence is invalid: {0}")]
    Package(String),
    #[error("fault process unexpectedly succeeded")]
    FaultDidNotInterrupt,
    #[error("replay process did not succeed")]
    ReplayFailed,
    #[error("interruption changed state before its commit boundary")]
    PartialCommit,
    #[error("post-commit interruption was not replay-idempotent")]
    ReplayMismatch,
    #[error("fresh-root restore differs from the pre-migration canonical state")]
    RestoreMismatch,
    #[error("migration result differs from its deterministic canonical plan")]
    PlanMismatch,
}

impl MigrationCase {
    pub const ALL: [Self; 11] = [
        Self::LegacyConfiguration,
        Self::ConfigurationLayering,
        Self::InvalidConfigurationFallback,
        Self::LegacyStateBackup,
        Self::FailedStateRollback,
        Self::ProtocolNegotiation,
        Self::LegacySessionExport,
        Self::WorkspaceSchema,
        Self::DerivedIndexRebuild,
        Self::PluginMismatch,
        Self::PluginRollback,
    ];
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MigrationMatrix {
    passed: BTreeSet<MigrationCase>,
}

impl MigrationMatrix {
    pub fn record(&mut self, case: MigrationCase) {
        self.passed.insert(case);
    }

    pub fn missing(&self) -> Vec<MigrationCase> {
        MigrationCase::ALL
            .into_iter()
            .filter(|case| !self.passed.contains(case))
            .collect()
    }

    pub fn is_complete(&self) -> bool {
        self.missing().is_empty()
    }
}

impl TeammatesFaultMatrix {
    /// Executes one interrupted packaged-binary case, replays it, and verifies a real backup
    /// restore against canonical store/resource identities and digests.
    pub fn execute_case(
        &mut self,
        case: &PackagedMigrationFaultCase,
    ) -> Result<&MigrationFaultEvidence, TeammatesFaultMatrixError> {
        if self.evidence.contains_key(&case.boundary) {
            return Err(TeammatesFaultMatrixError::Package(
                "fault boundary was already recorded".into(),
            ));
        }
        if case.restore_root.exists() {
            return Err(TeammatesFaultMatrixError::Package(
                "qualification restore root must not exist".into(),
            ));
        }
        let (before, inventory) = capture_packaged_invariant(case)?;
        let plan = TeammateMigrationPlan::build(&inventory)?;
        let interrupted_process = run_packaged_binary(&case.interrupted)?;
        if interrupted_process.success {
            return Err(TeammatesFaultMatrixError::FaultDidNotInterrupt);
        }
        let (after_interruption, _) = capture_packaged_invariant(case)?;
        if !case.boundary.may_commit_before_interruption() && after_interruption != before {
            return Err(TeammatesFaultMatrixError::PartialCommit);
        }
        let replay_process = run_packaged_binary(&case.replay)?;
        if !replay_process.success {
            return Err(TeammatesFaultMatrixError::ReplayFailed);
        }
        if interrupted_process.executable_sha256 != replay_process.executable_sha256 {
            return Err(TeammatesFaultMatrixError::Package(
                "fault and replay used different packaged binaries".into(),
            ));
        }
        let (after_replay, _) = capture_packaged_invariant(case)?;
        if case.boundary.may_commit_before_interruption() && after_interruption != after_replay {
            return Err(TeammatesFaultMatrixError::ReplayMismatch);
        }
        verify_unchanged_invariants(&before, &after_replay)?;
        let store = EmbeddedStore::open(&case.state_path, Some(&FileBackupHook))?;
        verify_plan_applied(&store, &plan)?;
        drop(store);

        let restore = restore_authoritative_backup(&case.backup_root, &case.restore_root)?;
        let restored_case = PackagedMigrationFaultCase {
            boundary: case.boundary,
            interrupted: case.interrupted.clone(),
            replay: case.replay.clone(),
            state_path: case.restore_root.join("state.sqlite"),
            sessions_root: case.restore_root.join("sessions"),
            backup_root: case.backup_root.clone(),
            restore_root: case.restore_root.clone(),
            default_profile_id: case.default_profile_id.clone(),
            migration_version: case.migration_version.clone(),
            migrated_at: case.migrated_at,
            resource_roots: case
                .resource_roots
                .iter()
                .map(|root| NamedResourceRoot {
                    name: root.name.clone(),
                    kind: root.kind,
                    path: case.restore_root.join(&root.name),
                })
                .collect(),
        };
        let (restored, _) = capture_packaged_invariant(&restored_case)?;
        if restored != before {
            return Err(TeammatesFaultMatrixError::RestoreMismatch);
        }
        let evidence = MigrationFaultEvidence {
            boundary: case.boundary,
            interrupted_process,
            replay_process,
            before,
            after_interruption,
            after_replay,
            restored,
            backup_manifest_digest: restore.manifest_digest,
        };
        self.evidence.insert(case.boundary, evidence);
        self.evidence
            .get(&case.boundary)
            .ok_or_else(|| TeammatesFaultMatrixError::Package("fault evidence vanished".into()))
    }

    #[must_use]
    pub fn evidence(&self) -> &BTreeMap<MigrationFaultBoundary, MigrationFaultEvidence> {
        &self.evidence
    }

    #[must_use]
    pub fn missing(&self) -> Vec<MigrationFaultBoundary> {
        MigrationFaultBoundary::ALL
            .into_iter()
            .filter(|boundary| !self.evidence.contains_key(boundary))
            .collect()
    }

    #[must_use]
    pub fn is_complete(&self) -> bool {
        self.missing().is_empty()
    }
}

fn capture_packaged_invariant(
    case: &PackagedMigrationFaultCase,
) -> Result<(CanonicalMigrationInvariant, TeammateMigrationInventory), TeammatesFaultMatrixError> {
    let store = EmbeddedStore::open(&case.state_path, Some(&FileBackupHook))?;
    let sessions = SessionStore::open(&case.sessions_root)?;
    let source = AuthoritativeMigrationSource {
        store: &store,
        sessions: &sessions,
        default_profile_id: case.default_profile_id.clone(),
        migration_version: case.migration_version.clone(),
        migrated_at: case.migrated_at,
        resource_roots: case.resource_roots.clone(),
    };
    let inventory = source.capture_inventory()?;
    let snapshot = capture_snapshot(&store, inventory.external.clone())?;
    let canonical_sha256 = hex_digest(
        &canonical_json_bytes(&snapshot)
            .map_err(|error| TeammatesFaultMatrixError::Package(error.to_string()))?,
    );
    Ok((
        CanonicalMigrationInvariant {
            snapshot,
            canonical_sha256,
        },
        inventory,
    ))
}

fn run_packaged_binary(
    command: &PackagedBinaryCommand,
) -> Result<PackagedProcessEvidence, TeammatesFaultMatrixError> {
    let metadata = fs::symlink_metadata(&command.executable)
        .map_err(|error| TeammatesFaultMatrixError::Package(error.to_string()))?;
    if metadata.file_type().is_symlink() || !metadata.is_file() {
        return Err(TeammatesFaultMatrixError::Package(
            "packaged executable must be a regular non-symlink file".into(),
        ));
    }
    let executable_sha256 = digest_file(&command.executable)?;
    let status = Command::new(&command.executable)
        .args(&command.arguments)
        .current_dir(&command.current_directory)
        .env_clear()
        .envs(&command.environment)
        .status()
        .map_err(|error| TeammatesFaultMatrixError::Package(error.to_string()))?;
    Ok(PackagedProcessEvidence {
        executable_sha256,
        exit_code: status.code(),
        success: status.success(),
    })
}

fn digest_file(path: &Path) -> Result<String, TeammatesFaultMatrixError> {
    let mut file =
        File::open(path).map_err(|error| TeammatesFaultMatrixError::Package(error.to_string()))?;
    let mut hasher = Sha256::new();
    let mut buffer = [0_u8; 64 * 1024];
    loop {
        let read = file
            .read(&mut buffer)
            .map_err(|error| TeammatesFaultMatrixError::Package(error.to_string()))?;
        if read == 0 {
            break;
        }
        hasher.update(&buffer[..read]);
    }
    Ok(hex_bytes(&hasher.finalize()))
}

fn verify_unchanged_invariants(
    before: &CanonicalMigrationInvariant,
    after: &CanonicalMigrationInvariant,
) -> Result<(), TeammatesFaultMatrixError> {
    if before.snapshot.external_digests != after.snapshot.external_digests {
        return Err(TeammatesFaultMatrixError::ReplayMismatch);
    }
    for &collection in TeammateMigrationOracle::SNAPSHOT {
        if migration_mutates(collection) {
            continue;
        }
        let name = collection.as_str();
        if before.snapshot.collection_digests.get(name)
            != after.snapshot.collection_digests.get(name)
            || before.snapshot.record_identities.get(name)
                != after.snapshot.record_identities.get(name)
        {
            return Err(TeammatesFaultMatrixError::ReplayMismatch);
        }
    }
    Ok(())
}

fn verify_plan_applied(
    store: &EmbeddedStore,
    plan: &TeammateMigrationPlan,
) -> Result<(), TeammatesFaultMatrixError> {
    for mutation in &plan.mutations {
        match mutation {
            RecordMutation::Put {
                collection, record, ..
            } => {
                if store.get_record(*collection, &record.id)?.as_ref() != Some(record) {
                    return Err(TeammatesFaultMatrixError::PlanMismatch);
                }
            }
            RecordMutation::Delete { collection, id, .. } => {
                if store.get_record(*collection, id)?.is_some() {
                    return Err(TeammatesFaultMatrixError::PlanMismatch);
                }
            }
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::collections::{BTreeMap, BTreeSet};
    use std::fs;
    use std::io;
    use std::path::{Path, PathBuf};

    use keith_agent_types::{
        CURRENT_PROTOCOL_VERSION, CURRENT_SCHEMA_VERSION, ClientId, EntityId, EntryId, Generation,
        ProfileId, ProtocolVersion, Revision, RootTreeId, SessionId, TurnId, UtcTimestamp,
        WorkerId, WorkspaceId,
    };
    use keith_configuration::{
        ConfigLayer, ConfigManager, ConfigPatch, ExecutionPatch, LayerKind, RuntimeConfig,
        parse_or_migrate_toml,
    };
    use keith_plugin_host::{PluginHost, PluginHostError};
    use keith_plugin_sdk::{
        MANIFEST_FILE, MODULE_FILE, ManifestError, PluginHook, PluginKind, PluginManifest,
        ResourceGrants,
    };
    use keith_protocol::{
        ClientHello, Feature, ProtocolError, WireFormat, WireMessage, decode_negotiated_bounded,
        encode, negotiate,
    };
    use keith_retrieval::{RankWeights, RetrievalLimits, RetrievalService};
    use keith_session_store::{
        LegacySessionExportLimits, NewSession, SessionEntryPayload, SessionKind, SessionStore,
        SessionStoreError, WriterIdentity,
    };
    use keith_state_store::{
        BackupHook, EmbeddedStore, FaultPoint, FileBackupHook, StoreError, TeammateMigrationOutcome,
    };
    use keith_state_store_core::{
        AtomicStateRepository, Collection, RecordMutation, VersionedRecord, WritePrecondition,
    };
    use keith_workspace::{PersonalWorkspace, PersonalWorkspaceLimits};
    use rusqlite::Connection;
    use tempfile::tempdir;

    use super::*;

    struct FailingBackup;

    impl BackupHook for FailingBackup {
        fn before_migration(
            &self,
            _source: &Path,
            _destination: &Path,
            _from_version: u32,
            _to_version: u32,
        ) -> io::Result<()> {
            Err(io::Error::other("matrix backup failure"))
        }
    }

    fn legacy_database(path: &Path) {
        let connection = Connection::open(path).unwrap();
        connection
            .execute_batch(
                "CREATE TABLE legacy_data(value TEXT NOT NULL);
                 INSERT INTO legacy_data(value) VALUES('preserve-me');
                 PRAGMA user_version = 0;",
            )
            .unwrap();
    }

    fn plugin_package(root: &Path, version: &str, migrate_status: i32) -> PathBuf {
        let package = root.join(format!("matrix-{version}"));
        fs::create_dir_all(&package).unwrap();
        let manifest = PluginManifest {
            manifest_version: 1,
            id: "matrix".into(),
            name: "matrix".into(),
            version: version.into(),
            host_api_min: 1,
            host_api_max: 1,
            kind: PluginKind::WasiComponent,
            hooks: BTreeSet::from([PluginHook::Activate, PluginHook::Migrate]),
            grants: ResourceGrants::default(),
        };
        fs::write(
            package.join(MANIFEST_FILE),
            toml::to_string(&manifest).unwrap(),
        )
        .unwrap();
        fs::write(
            package.join(MODULE_FILE),
            wat::parse_str(format!(
                "(module
                    (func (export \"keith_activate\") (result i32) i32.const 0)
                    (func (export \"keith_migrate\") (result i32) i32.const {migrate_status}))"
            ))
            .unwrap(),
        )
        .unwrap();
        package
    }

    fn valid_plugin_toml() -> String {
        toml::to_string(&PluginManifest {
            manifest_version: 1,
            id: "bounded".into(),
            name: "Bounded".into(),
            version: "1.0.0".into(),
            host_api_min: 1,
            host_api_max: 1,
            kind: PluginKind::WasiComponent,
            hooks: BTreeSet::new(),
            grants: ResourceGrants::default(),
        })
        .unwrap()
    }

    fn oracle_record(id: EntityId, value: &str) -> VersionedRecord {
        VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id,
            revision: Revision::ZERO,
            updated_at: UtcTimestamp::UNIX_EPOCH,
            payload: serde_json::json!({"value": value}),
        }
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn teammates_oracle_proves_integrity_replay_rollback_and_fresh_root_restore() {
        let root = tempdir().unwrap();
        let path = root.path().join("state.sqlite");
        let backup = root.path().join("pre-teammates.sqlite");
        let fresh = root.path().join("fresh-root.sqlite");
        let store = EmbeddedStore::open(&path, None).unwrap();
        let protected = [
            (Collection::Profiles, "profile"),
            (Collection::ResourceGovernance, "resources"),
            (Collection::SessionCatalog, "session-digest"),
            (Collection::Children, "temporary-child"),
            (Collection::ScheduledJobs, "schedule"),
            (Collection::SharedKnowledgeGrants, "private"),
            (Collection::RoutingRules, "route-authority"),
        ];
        let mut seed = Vec::new();
        for (collection, value) in protected {
            seed.push(RecordMutation::Put {
                collection,
                record: oracle_record(EntityId::new(), value),
                precondition: WritePrecondition::Missing,
            });
        }
        let shared_space_id = EntityId::new();
        let shared_space_owner = ProfileId::new();
        seed.push(RecordMutation::Put {
            collection: Collection::SharedKnowledgeSpaces,
            record: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: shared_space_id.clone(),
                revision: Revision::new(3),
                updated_at: UtcTimestamp::UNIX_EPOCH,
                payload: serde_json::json!({
                    "id": shared_space_id,
                    "owner": shared_space_owner,
                    "members": {},
                    "permission_revision": 3,
                    "source_conversation_id": null,
                    "source_event_ids": [],
                    "deleted": false
                }),
            },
            precondition: WritePrecondition::Missing,
        });
        store.transact(&seed).unwrap();
        let oracle = TeammateMigrationOracle::capture(&store).unwrap();
        let conversation_id = EntityId::new();
        let migration = [RecordMutation::Put {
            collection: Collection::Conversations,
            record: oracle_record(conversation_id.clone(), "first-keith-dm-with-provenance"),
            precondition: WritePrecondition::Missing,
        }];

        store.inject_fault_once(FaultPoint::BeforeCommit);
        assert!(matches!(
            store.migrate_teammates("keith-teammates-v1", &backup, &migration),
            Err(StoreError::Injected(FaultPoint::BeforeCommit))
        ));
        assert!(
            store
                .get_record(Collection::Conversations, &conversation_id)
                .unwrap()
                .is_none()
        );

        let retry_backup = root.path().join("pre-teammates-retry.sqlite");
        assert!(matches!(
            store
                .migrate_teammates("keith-teammates-v1", &retry_backup, &migration)
                .unwrap(),
            TeammateMigrationOutcome::Applied(_)
        ));
        assert_eq!(
            store
                .migrate_teammates(
                    "keith-teammates-v1",
                    &root.path().join("unused.sqlite"),
                    &migration
                )
                .unwrap(),
            TeammateMigrationOutcome::AlreadyApplied
        );
        let report = oracle.verify(&store, &retry_backup, &fresh).unwrap();
        assert!(report.backup_restored_into_fresh_root);
        assert!(report.route_identities_preserved);
        let expected_preserved_collections = TeammateMigrationOracle::SNAPSHOT
            .iter()
            .copied()
            .filter(|collection| !migration_mutates(*collection))
            .map(|collection| collection.as_str().to_owned())
            .collect();
        assert_eq!(report.preserved_collections, expected_preserved_collections);
        assert!(
            report
                .preserved_collections
                .contains(Collection::SharedKnowledgeSpaces.as_str())
        );

        let mut changed_space = store
            .get_record(Collection::SharedKnowledgeSpaces, &shared_space_id)
            .unwrap()
            .unwrap();
        changed_space.revision = Revision::new(4);
        changed_space.payload["permission_revision"] = serde_json::json!(4);
        store
            .transact(&[RecordMutation::Put {
                collection: Collection::SharedKnowledgeSpaces,
                record: changed_space,
                precondition: WritePrecondition::Exact(Revision::new(3)),
            }])
            .unwrap();
        assert!(matches!(
            oracle.verify(
                &store,
                &retry_backup,
                &root.path().join("fresh-root-after-space-change.sqlite")
            ),
            Err(TeammateMigrationOracleError::ProtectedCollectionChanged { collection })
                if collection == Collection::SharedKnowledgeSpaces.as_str()
        ));
    }

    #[test]
    fn teammate_planner_is_deterministic_and_preserves_legacy_bytes_with_provenance() {
        let default_profile = ProfileId::from(EntityId::from_u128(1));
        let other_profile = ProfileId::from(EntityId::from_u128(2));
        let disabled_profile = ProfileId::from(EntityId::from_u128(3));
        let selected = SessionId::from(EntityId::from_u128(10));
        let legacy = SessionId::from(EntityId::from_u128(11));
        let inventory = TeammateMigrationInventory {
            migration_version: "v1".into(),
            migrated_at: UtcTimestamp::UNIX_EPOCH,
            default_profile_id: default_profile.clone(),
            profiles: BTreeSet::from([
                default_profile.clone(),
                other_profile.clone(),
                disabled_profile.clone(),
            ]),
            enabled_profiles: BTreeSet::from([default_profile.clone(), other_profile.clone()]),
            routes: vec![
                LegacyRouteInventory {
                    route_record_id: EntityId::from_u128(20),
                    route_revision: Revision::new(4),
                    profile_id: default_profile.clone(),
                    session_bindings: BTreeMap::from([(
                        "terminal/account/dm".into(),
                        selected.clone(),
                    )]),
                    policy_sha256: "route-policy-digest".into(),
                },
                LegacyRouteInventory {
                    route_record_id: EntityId::from_u128(21),
                    route_revision: Revision::new(1),
                    profile_id: disabled_profile,
                    session_bindings: BTreeMap::new(),
                    policy_sha256: "disabled-route-policy-digest".into(),
                },
            ],
            sessions: vec![
                LegacySessionInventory {
                    session_id: selected.clone(),
                    owner_profile_id: default_profile,
                    canonical_bytes: b"selected-session".to_vec(),
                    source_entry_ids: vec![EntityId::from_u128(20)],
                    healthy_root: true,
                    updated_at_ms: 2,
                },
                LegacySessionInventory {
                    session_id: legacy.clone(),
                    owner_profile_id: other_profile,
                    canonical_bytes: b"legacy-session-exact-bytes".to_vec(),
                    source_entry_ids: vec![],
                    healthy_root: true,
                    updated_at_ms: 3,
                },
            ],
            external: ExternalStateSnapshot {
                memory_digest: "memory-sha256".into(),
                workspace_digest: "workspace-sha256".into(),
                ..ExternalStateSnapshot::default()
            },
        };
        let first = TeammateMigrationPlan::build(&inventory).unwrap();
        let second = TeammateMigrationPlan::build(&inventory).unwrap();
        assert_eq!(first, second);
        assert_eq!(first.selected_keith_session, Some(selected));
        let payloads = first
            .mutations
            .iter()
            .filter_map(|mutation| match mutation {
                RecordMutation::Put {
                    collection, record, ..
                } => Some((*collection, &record.payload)),
                RecordMutation::Delete { .. } => None,
            })
            .collect::<Vec<_>>();
        assert_eq!(
            payloads
                .iter()
                .filter(|(collection, _)| *collection == Collection::Conversations)
                .count(),
            3
        );
        let legacy_payloads = payloads
            .iter()
            .filter(|(collection, _)| *collection == Collection::LegacySessions)
            .filter_map(|(_, payload)| payload["canonical_bytes_hex"].as_str())
            .collect::<BTreeSet<_>>();
        assert_eq!(legacy_payloads.len(), 2);
        let bindings = payloads
            .iter()
            .filter(|(collection, _)| *collection == Collection::ConversationBindings)
            .collect::<Vec<_>>();
        assert_eq!(bindings.len(), 2);
        assert!(
            bindings
                .iter()
                .any(|(_, payload)| payload["route_policy_sha256"] == "route-policy-digest")
        );
        let expected_legacy = hex_bytes(b"legacy-session-exact-bytes");
        assert!(legacy_payloads.contains(expected_legacy.as_str()));
        assert!(payloads.iter().any(|(collection, payload)| *collection
            == Collection::MigrationProvenance
            && payload.get("source_entry_ids").is_some()));
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn teammates_authoritative_backup_restores_real_sessions_and_rejects_tampering() {
        let root = tempdir().unwrap();
        let database = root.path().join("state.sqlite");
        let store = EmbeddedStore::open(&database, None).unwrap();
        let profile_id = ProfileId::new();
        let profile: RegisteredProfile = serde_json::from_value(serde_json::json!({
            "profile": {
                "version": CURRENT_SCHEMA_VERSION,
                "id": profile_id,
                "display_name": "Keith",
                "workspace_id": WorkspaceId::new(),
                "persona_file": "PERSONA.md",
                "user_file": "USER.md",
                "rule_files": [],
                "model_route": { "provider": "openai", "model": "model", "fallbacks": [], "credential_ref": null },
                "thinking": "medium",
                "tool_rules": {},
                "enabled_skills": [], "enabled_mcp_servers": [], "enabled_plugins": [], "channels": [],
                "autonomy": { "mode": "bounded", "max_children": 2, "max_depth": 2, "daily_token_budget": 1000 },
                "notifications": { "quiet_hours_start": "22:00", "quiet_hours_end": "08:00", "time_zone": "UTC", "daily_limit": 4 },
                "refinement": { "enabled": false, "require_confirmation": true, "editable_targets": [] }
            },
            "resources": { "workspace_root": ".", "memory_root": "memory", "schedule_root": "schedules" },
            "enabled": true,
            "authorized_callers": [],
            "revision": 0,
            "updated_at": 0
        })).unwrap();
        store
            .transact(&[RecordMutation::Put {
                collection: Collection::Profiles,
                record: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: profile_id.as_entity_id().clone(),
                    revision: Revision::ZERO,
                    updated_at: UtcTimestamp::UNIX_EPOCH,
                    payload: serde_json::to_value(profile).unwrap(),
                },
                precondition: WritePrecondition::Missing,
            }])
            .unwrap();
        let sessions = SessionStore::open(root.path().join("session-root")).unwrap();
        let session_id = SessionId::new();
        sessions
            .create(NewSession {
                kind: SessionKind::Root,
                session_id: session_id.clone(),
                root_tree_id: RootTreeId::new(),
                parent_session_id: None,
                profile_id: profile_id.clone(),
                workspace_id: WorkspaceId::new(),
                created_at: UtcTimestamp::from_unix_millis(10),
                label: Some("healthy root".into()),
                profile_snapshot: None,
            })
            .unwrap();
        let mut writer = sessions
            .acquire_writer(
                &session_id,
                WriterIdentity {
                    worker_id: WorkerId::new(),
                    owner_instance: EntityId::new(),
                    generation: Generation::new(1),
                    acquired_at: UtcTimestamp::from_unix_millis(11),
                },
            )
            .unwrap();
        writer
            .append(
                None,
                UtcTimestamp::from_unix_millis(12),
                SessionEntryPayload::ControllerGuidance {
                    turn_id: TurnId::new(),
                    source_id: "migration-test".into(),
                    text: "preserve exact session bytes".into(),
                },
            )
            .unwrap();
        drop(writer);
        let workspace = root.path().join("workspace");
        let memory = root.path().join("memory");
        let credentials = root.path().join("credentials");
        fs::create_dir(&workspace).unwrap();
        fs::create_dir(&memory).unwrap();
        fs::create_dir(&credentials).unwrap();
        fs::write(workspace.join("project.txt"), b"workspace-state").unwrap();
        fs::write(memory.join("MEMORY.md"), b"private-memory-state").unwrap();
        fs::write(
            credentials.join("provider.cred"),
            b"encrypted-credential-record",
        )
        .unwrap();
        let source = AuthoritativeMigrationSource {
            store: &store,
            sessions: &sessions,
            default_profile_id: profile_id,
            migration_version: "keith-teammates-v1".into(),
            migrated_at: UtcTimestamp::from_unix_millis(20),
            resource_roots: vec![
                NamedResourceRoot {
                    name: "workspace".into(),
                    kind: ResourceRootKind::Workspace,
                    path: workspace,
                },
                NamedResourceRoot {
                    name: "memory".into(),
                    kind: ResourceRootKind::Memory,
                    path: memory,
                },
                NamedResourceRoot {
                    name: "credentials".into(),
                    kind: ResourceRootKind::CredentialStore,
                    path: credentials,
                },
            ],
        };
        let inventory = source.capture_inventory().unwrap();
        assert!(inventory.sessions[0].healthy_root);
        assert_ne!(
            inventory.external.credential_store_digest,
            inventory.external.permission_digest
        );
        let backup = root.path().join("backup");
        let restored = root.path().join("fresh");
        let report = run_authoritative_teammate_migration(&source, &backup, &restored).unwrap();
        assert_eq!(report.selected_keith_session, Some(session_id.clone()));
        assert_eq!(
            fs::read(restored.join("workspace/project.txt")).unwrap(),
            b"workspace-state"
        );
        assert_eq!(
            fs::read(restored.join("memory/MEMORY.md")).unwrap(),
            b"private-memory-state"
        );
        assert_eq!(
            SessionStore::open(restored.join("sessions"))
                .unwrap()
                .discover()
                .unwrap()
                .len(),
            1
        );
        let conversation = store
            .list_records(Collection::Conversations)
            .unwrap()
            .pop()
            .unwrap();
        let typed: ConversationRecord = serde_json::from_value(conversation.payload).unwrap();
        typed.validate().unwrap();
        let repository = keith_conversation::ConversationRepository::new();
        assert!(
            repository
                .conversation_durable(&store, &typed.id)
                .unwrap()
                .is_some()
        );
        assert_eq!(
            repository
                .events_after_durable(&store, &typed.id, 0, 10)
                .unwrap()
                .len(),
            1
        );
        assert_eq!(
            store
                .list_records(Collection::ConversationParticipants)
                .unwrap()
                .len(),
            2
        );
        let replay = run_authoritative_teammate_migration(
            &source,
            &backup,
            &root.path().join("fresh-replay"),
        )
        .unwrap();
        assert_eq!(replay.outcome, TeammateMigrationOutcome::AlreadyApplied);
        fs::write(
            source.resource_roots[0].path.join("project.txt"),
            b"changed-after-backup",
        )
        .unwrap();
        let parent = sessions.manifest(&session_id).unwrap().active_leaf;
        let mut writer = sessions
            .acquire_writer(
                &session_id,
                WriterIdentity {
                    worker_id: WorkerId::new(),
                    owner_instance: EntityId::new(),
                    generation: Generation::new(2),
                    acquired_at: UtcTimestamp::from_unix_millis(30),
                },
            )
            .unwrap();
        writer
            .append(
                parent,
                UtcTimestamp::from_unix_millis(31),
                SessionEntryPayload::ControllerGuidance {
                    turn_id: TurnId::new(),
                    source_id: "post-backup-change".into(),
                    text: "must invalidate stale backup".into(),
                },
            )
            .unwrap();
        drop(writer);
        let marker_count = store
            .list_records(Collection::SchemaMigrations)
            .unwrap()
            .len();
        let conversation_count = store.list_records(Collection::Conversations).unwrap().len();
        let changed_source = AuthoritativeMigrationSource {
            store: &store,
            sessions: &sessions,
            default_profile_id: source.default_profile_id.clone(),
            migration_version: "keith-teammates-v2".into(),
            migrated_at: UtcTimestamp::from_unix_millis(32),
            resource_roots: source.resource_roots.clone(),
        };
        assert!(
            run_authoritative_teammate_migration(
                &changed_source,
                &backup,
                &root.path().join("fresh-stale-backup"),
            )
            .is_err()
        );
        assert_eq!(
            store
                .list_records(Collection::SchemaMigrations)
                .unwrap()
                .len(),
            marker_count
        );
        assert_eq!(
            store.list_records(Collection::Conversations).unwrap().len(),
            conversation_count
        );
        let first = &report.backup_manifest.entries[0];
        fs::write(
            backup
                .join("resources")
                .join(&first.root_name)
                .join(&first.relative_path),
            b"tampered",
        )
        .unwrap();
        assert!(verify_resource_backup(&backup, &report.backup_manifest).is_err());
    }

    #[test]
    fn teammates_authoritative_backup_rejects_missing_files_and_symlinks() {
        let root = tempdir().unwrap();
        let source_root = root.path().join("resources");
        fs::create_dir(&source_root).unwrap();
        fs::write(source_root.join("state"), b"value").unwrap();
        let backup = root.path().join("backup");
        fs::create_dir(&backup).unwrap();
        let source_state = root.path().join("source-state.sqlite");
        let store = EmbeddedStore::open(&source_state, None).unwrap();
        let state_backup = backup.join("state.sqlite");
        store.backup_to(&state_backup).unwrap();
        let store_schema_version = store.schema_version().unwrap();
        let manifest = create_resource_backup(
            &[NamedResourceRoot {
                name: "resources".into(),
                kind: ResourceRootKind::Resource,
                path: source_root.clone(),
            }],
            &backup,
            &state_backup,
            store_schema_version,
        )
        .unwrap();
        fs::remove_file(backup.join("resources/resources/state")).unwrap();
        assert!(verify_resource_backup(&backup, &manifest).is_err());
        #[cfg(unix)]
        {
            use std::os::unix::fs::symlink;
            symlink(root.path(), source_root.join("escape")).unwrap();
            let next = root.path().join("next-backup");
            fs::create_dir(&next).unwrap();
            let next_state_backup = next.join("state.sqlite");
            store.backup_to(&next_state_backup).unwrap();
            assert!(
                create_resource_backup(
                    &[NamedResourceRoot {
                        name: "resources".into(),
                        kind: ResourceRootKind::Resource,
                        path: source_root,
                    }],
                    &next,
                    &next_state_backup,
                    store_schema_version,
                )
                .is_err()
            );
        }
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn every_supported_upgrade_failure_compatibility_and_rollback_path_runs() {
        let mut matrix = MigrationMatrix::default();

        let legacy = r#"
config_version = 0
data_root = "legacy-data"
max_workers = 4
max_processes = 6
"#;
        let global = parse_or_migrate_toml(legacy).unwrap();
        assert_eq!(global.kind, LayerKind::Global);
        assert_eq!(global.version, CURRENT_SCHEMA_VERSION);
        matrix.record(MigrationCase::LegacyConfiguration);

        let defaults = RuntimeConfig::secure_defaults();
        let mut manager = ConfigManager::new(defaults).unwrap();
        manager.apply(global).unwrap();
        for kind in [
            LayerKind::Profile,
            LayerKind::Workspace,
            LayerKind::Session,
            LayerKind::Action,
        ] {
            manager
                .apply(ConfigLayer::new(kind, ConfigPatch::default()))
                .unwrap();
        }
        assert_eq!(manager.revision().get(), 5);
        matrix.record(MigrationCase::ConfigurationLayering);
        let before = manager.active().clone();
        let notifications = manager.subscribe();
        let invalid = ConfigLayer::new(
            LayerKind::Action,
            ConfigPatch {
                execution: Some(ExecutionPatch {
                    workspace_confinement: Some(false),
                    ..ExecutionPatch::default()
                }),
                ..ConfigPatch::default()
            },
        );
        assert!(manager.apply(invalid).is_err());
        assert_eq!(manager.active(), &before);
        assert!(matches!(
            notifications.recv().unwrap(),
            keith_configuration::ConfigNotification::Rejected { .. }
        ));
        matrix.record(MigrationCase::InvalidConfigurationFallback);

        let state_root = tempdir().unwrap();
        let state_path = state_root.path().join("state.sqlite");
        legacy_database(&state_path);
        let state = EmbeddedStore::open(&state_path, Some(&FileBackupHook)).unwrap();
        assert_eq!(state.schema_version().unwrap(), 1);
        let backups = fs::read_dir(state_root.path())
            .unwrap()
            .map(|entry| entry.unwrap().file_name().to_string_lossy().into_owned())
            .filter(|name| name.contains("pre-v0-to-v1"))
            .count();
        assert_eq!(backups, 1);
        let legacy_value: String = Connection::open(&state_path)
            .unwrap()
            .query_row("SELECT value FROM legacy_data", [], |row| row.get(0))
            .unwrap();
        assert_eq!(legacy_value, "preserve-me");
        matrix.record(MigrationCase::LegacyStateBackup);

        let failed_path = state_root.path().join("failed.sqlite");
        legacy_database(&failed_path);
        assert!(matches!(
            EmbeddedStore::open(&failed_path, Some(&FailingBackup)),
            Err(StoreError::Backup(_))
        ));
        let failed = Connection::open(&failed_path).unwrap();
        let version: u32 = failed
            .pragma_query_value(None, "user_version", |row| row.get(0))
            .unwrap();
        assert_eq!(version, 0);
        let preserved: String = failed
            .query_row("SELECT value FROM legacy_data", [], |row| row.get(0))
            .unwrap();
        assert_eq!(preserved, "preserve-me");
        matrix.record(MigrationCase::FailedStateRollback);

        let hello = ClientHello {
            protocol: CURRENT_PROTOCOL_VERSION,
            client_id: ClientId::new(),
            client_name: "migration-matrix".into(),
            client_version: "0.9.0".into(),
            supported_features: BTreeSet::from([Feature::SessionLifecycle]),
            resume: None,
        };
        let negotiated = negotiate(
            &hello,
            CURRENT_PROTOCOL_VERSION,
            EntityId::new(),
            &hello.supported_features,
        )
        .unwrap();
        let encoded = encode(WireFormat::Json, &WireMessage::ClientHello(hello)).unwrap();
        assert!(matches!(
            decode_negotiated_bounded(
                WireFormat::Json,
                &encoded,
                negotiated.protocol,
                encoded.len() - 1,
            ),
            Err(ProtocolError::MessageTooLarge { .. })
        ));
        assert!(matches!(
            negotiate(
                &ClientHello {
                    protocol: ProtocolVersion::new(CURRENT_PROTOCOL_VERSION.major + 1, 0),
                    client_id: ClientId::new(),
                    client_name: "future".into(),
                    client_version: "2".into(),
                    supported_features: BTreeSet::new(),
                    resume: None,
                },
                CURRENT_PROTOCOL_VERSION,
                EntityId::new(),
                &BTreeSet::new(),
            ),
            Err(ProtocolError::MajorMismatch { .. })
        ));
        matrix.record(MigrationCase::ProtocolNegotiation);

        let entry_id = EntryId::new();
        let session_id = SessionId::new();
        let legacy_manifest = serde_json::to_vec(&serde_json::json!({
            "schema_version": 0,
            "kind": SessionKind::Root,
            "session_id": session_id,
            "root_tree_id": RootTreeId::new(),
            "parent_session_id": null,
            "profile_id": ProfileId::new(),
            "workspace_id": WorkspaceId::new(),
            "created_at": UtcTimestamp::UNIX_EPOCH,
            "active_leaf": entry_id,
            "label": "legacy",
            "branch_labels": BTreeMap::from([("main", entry_id.clone())]),
            "archived": false
        }))
        .unwrap();
        let legacy_history = serde_json::to_vec(&serde_json::json!({
            "id": entry_id,
            "parent_id": null,
            "timestamp": UtcTimestamp::UNIX_EPOCH,
            "payload": {
                "payload": "lifecycle",
                "state": "ready",
                "detail": null
            }
        }))
        .unwrap();
        let before_manifest = legacy_manifest.clone();
        let before_history = legacy_history.clone();
        let migrated = SessionStore::migrate_legacy_export(
            &legacy_manifest,
            &legacy_history,
            LegacySessionExportLimits::default(),
        )
        .unwrap();
        assert_eq!(migrated.version, CURRENT_SCHEMA_VERSION);
        assert_eq!(migrated.entries.len(), 1);
        assert_eq!(legacy_manifest, before_manifest);
        assert_eq!(legacy_history, before_history);
        assert!(matches!(
            SessionStore::migrate_legacy_export(
                &legacy_manifest,
                &legacy_history,
                LegacySessionExportLimits {
                    max_history_bytes: legacy_history.len() - 1,
                    ..LegacySessionExportLimits::default()
                },
            ),
            Err(SessionStoreError::LegacyExportLimit)
        ));
        matrix.record(MigrationCase::LegacySessionExport);

        let workspace_root = tempdir().unwrap();
        let markdown = b"# User memory\nNever rewrite this text.\n";
        fs::write(workspace_root.path().join("MEMORY.md"), markdown).unwrap();
        let workspace = PersonalWorkspace::open(
            workspace_root.path(),
            PersonalWorkspaceLimits::default(),
            UtcTimestamp::UNIX_EPOCH,
        )
        .unwrap();
        assert_eq!(fs::read(workspace.layout().memory).unwrap(), markdown);
        let index: serde_json::Value = serde_json::from_slice(
            &fs::read(workspace_root.path().join(".keith/index.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(index["version"]["major"], CURRENT_SCHEMA_VERSION.major);
        matrix.record(MigrationCase::WorkspaceSchema);
        let retrieval = RetrievalService::open(
            workspace_root.path().join("index"),
            RetrievalLimits::default(),
            RankWeights::default(),
            None,
        )
        .unwrap();
        let profile = ProfileId::new();
        assert_eq!(
            retrieval
                .rebuild_workspace(&profile, workspace_root.path(), UtcTimestamp::UNIX_EPOCH)
                .unwrap(),
            1
        );
        assert!(
            !retrieval
                .search(&profile, "Never rewrite", 5)
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            fs::read(workspace_root.path().join("MEMORY.md")).unwrap(),
            markdown
        );
        matrix.record(MigrationCase::DerivedIndexRebuild);

        let plugin_toml = valid_plugin_toml();
        assert!(PluginManifest::parse_bounded(&plugin_toml, plugin_toml.len()).is_ok());
        assert_eq!(
            PluginManifest::parse_bounded(&plugin_toml, plugin_toml.len() - 1),
            Err(ManifestError::TooLarge)
        );
        let mut incompatible: toml::Value = toml::from_str(&plugin_toml).unwrap();
        incompatible["host_api_min"] = toml::Value::Integer(2);
        assert_eq!(
            PluginManifest::parse(&toml::to_string(&incompatible).unwrap()),
            Err(ManifestError::Incompatible)
        );
        matrix.record(MigrationCase::PluginMismatch);

        let plugin_root = tempdir().unwrap();
        let packages = tempdir().unwrap();
        let first = plugin_package(packages.path(), "1.0.0", 0);
        let second = plugin_package(packages.path(), "2.0.0", 0);
        let failed_plugin = plugin_package(packages.path(), "3.0.0", 7);
        let mut host = PluginHost::open(plugin_root.path(), false).unwrap();
        host.install(first).unwrap();
        host.install(second).unwrap();
        assert!(matches!(
            host.install(failed_plugin),
            Err(PluginHostError::HookStatus(7))
        ));
        assert_eq!(host.record("matrix").unwrap().active_version, "2.0.0");
        host.rollback("matrix", "1.0.0").unwrap();
        assert_eq!(host.record("matrix").unwrap().active_version, "1.0.0");
        matrix.record(MigrationCase::PluginRollback);

        assert!(
            matrix.is_complete(),
            "missing migration cases: {:?}",
            matrix.missing()
        );
    }
}
