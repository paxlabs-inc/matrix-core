#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::fs;
use std::path::{Path, PathBuf};
use std::str::FromStr;
use std::sync::{Arc, Mutex, MutexGuard, RwLock};

use keith_agent_types::{
    ConversationId, EntityId, EventId, GrantId, ProfileId, Revision, UtcTimestamp,
};
use rusqlite::{Connection, OptionalExtension, params};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

const INDEX_FILE: &str = "retrieval.sqlite3";
const INDEX_SCHEMA: i64 = 1;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SearchSourceKind {
    DurableMemory,
    DailyMemory,
    CurrentState,
    Knowledge,
    SessionSummary,
    Skill,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SourceInput {
    pub source_path: String,
    pub source_version: String,
    pub source_kind: SearchSourceKind,
    pub modified_at: UtcTimestamp,
    pub text: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum KnowledgeVisibility {
    Private,
    ConversationShared,
    ExplicitlyShared,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct KnowledgeSourceProvenance {
    pub owner_profile_id: ProfileId,
    pub conversation_id: Option<ConversationId>,
    pub source_event_ids: BTreeSet<EventId>,
    pub visibility: KnowledgeVisibility,
    pub explicit_grantees: BTreeSet<ProfileId>,
    pub grant_id: Option<GrantId>,
    pub space_id: Option<EntityId>,
    pub observed_permission_revision: Option<Revision>,
    pub policy_revision: Revision,
    pub revoked: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuthorizedSourceInput {
    pub source: SourceInput,
    pub provenance: KnowledgeSourceProvenance,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuthenticatedKnowledgeQuery {
    pub requester: ProfileId,
    pub space_id: EntityId,
    pub operation: KnowledgeOperation,
    pub now: UtcTimestamp,
    pub query: String,
    pub limit: usize,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResolvedSpaceAuthorization {
    pub space_revision: Revision,
    pub membership_permission_revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResolvedGrantAuthorization {
    pub grant_id: GrantId,
    pub grant_revision: Revision,
    pub resource_policy_revision: Revision,
    pub status: GrantAuthorizationStatus,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GrantAuthorizationStatus {
    Active,
    Revoked,
    Expired,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuthorizedKnowledgeProvenance {
    pub owner_profile_id: ProfileId,
    pub space_id: EntityId,
    pub observed_permission_revision: Revision,
    pub space_revision: Revision,
    pub membership_permission_revision: Revision,
    pub grant_id: GrantId,
    pub grant_revision: Revision,
    pub resource_policy_revision: Revision,
    pub source_conversation_id: Option<ConversationId>,
    pub source_event_ids: BTreeSet<EventId>,
    pub source_policy_revision: Revision,
}

#[derive(Clone, Debug, PartialEq)]
pub struct AuthorizedKnowledgeSearchResult {
    pub result: SearchResult,
    pub provenance: AuthorizedKnowledgeProvenance,
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct KnowledgeAuthorizationObservation {
    pub space: Option<ResolvedSpaceAuthorization>,
    pub grants: BTreeMap<GrantId, ResolvedGrantAuthorization>,
}

#[derive(Clone, Debug, Default, PartialEq)]
pub struct AuthorizedKnowledgeSearchResponse {
    pub results: Vec<AuthorizedKnowledgeSearchResult>,
    pub authorization: KnowledgeAuthorizationObservation,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum KnowledgeOperation {
    Search,
    Read,
}

pub trait KnowledgeAccessResolver {
    /// # Errors
    /// Returns an error when authoritative conversation state cannot be read safely.
    fn is_active_participant(
        &self,
        conversation_id: &ConversationId,
        requester: &ProfileId,
    ) -> Result<bool, RetrievalError>;

    /// # Errors
    /// Returns an error when authoritative grant state cannot be read safely.
    fn authorize_grant(
        &self,
        grant_id: &GrantId,
        space_id: &EntityId,
        requester: &ProfileId,
        operation: KnowledgeOperation,
        now: UtcTimestamp,
    ) -> Result<Option<ResolvedGrantAuthorization>, RetrievalError>;

    /// # Errors
    /// Returns an error when authoritative shared-space state cannot be read safely.
    fn authorize_space(
        &self,
        space_id: &EntityId,
        observed_permission_revision: Revision,
        requester: &ProfileId,
        operation: KnowledgeOperation,
        now: UtcTimestamp,
    ) -> Result<Option<ResolvedSpaceAuthorization>, RetrievalError>;
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SearchDocument {
    pub document_id: EntityId,
    pub profile_id: ProfileId,
    pub source_path: String,
    pub source_version: String,
    pub heading_path: Vec<String>,
    pub text: String,
    pub source_kind: SearchSourceKind,
    pub modified_at: UtcTimestamp,
}

#[derive(Clone, Debug, PartialEq)]
pub struct SearchResult {
    pub document_id: EntityId,
    pub source_path: String,
    pub source_version: String,
    pub heading_path: Vec<String>,
    pub excerpt: String,
    pub source_kind: SearchSourceKind,
    pub modified_at: UtcTimestamp,
    pub lexical_score: f32,
    pub trigram_score: f32,
    pub vector_score: Option<f32>,
    pub merged_score: f32,
}

#[derive(Clone, Copy, Debug, PartialEq)]
pub struct RankWeights {
    pub lexical: f32,
    pub trigram: f32,
    pub vector: f32,
}

impl Default for RankWeights {
    fn default() -> Self {
        Self {
            lexical: 0.45,
            trigram: 0.35,
            vector: 0.20,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RetrievalLimits {
    pub max_source_bytes: usize,
    pub max_chunk_bytes: usize,
    pub max_documents_per_profile: usize,
    pub max_scan_results: usize,
    pub max_excerpt_chars: usize,
}

impl Default for RetrievalLimits {
    fn default() -> Self {
        Self {
            max_source_bytes: 4 * 1_024 * 1_024,
            max_chunk_bytes: 8 * 1_024,
            max_documents_per_profile: 100_000,
            max_scan_results: 10_000,
            max_excerpt_chars: 480,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RetrievalHealth {
    pub degraded: bool,
    pub vector_available: bool,
    pub detail: Option<String>,
    pub quarantined_index: Option<PathBuf>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DeletionClassification {
    DeletePrivate,
    RetainShared,
    RetainImmutableAudit,
    ExternalRemnant,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum RetrievalDeletionRecordKind {
    Document {
        document_id: EntityId,
        source_version: String,
        source_path: String,
    },
    SourceAccess {
        source_path: String,
        policy_revision: Revision,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RetrievalDeletionRecord {
    pub stable_key: String,
    pub classification: DeletionClassification,
    pub kind: RetrievalDeletionRecordKind,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RetrievalDeletionInventory {
    pub profile_id: ProfileId,
    pub inventory_digest: String,
    pub records: Vec<RetrievalDeletionRecord>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RetrievalDeletionReceipt {
    pub profile_id: ProfileId,
    pub inventory_digest: String,
    pub erased_stable_keys: Vec<String>,
    pub retained: Vec<RetrievalDeletionRecord>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RetrievalLeakScan {
    pub profile_id: ProfileId,
    pub leaked_private_keys: Vec<String>,
    pub retained: Vec<RetrievalDeletionRecord>,
}

#[derive(Clone, Debug, PartialEq)]
pub struct VectorRecord {
    pub document_id: EntityId,
    pub profile_id: ProfileId,
    pub source_path: String,
    pub vector: Vec<f32>,
}

#[derive(Clone, Debug, PartialEq)]
pub struct VectorHit {
    pub document_id: EntityId,
    pub score: f32,
}

pub trait Embedder: Send + Sync {
    fn dimensions(&self) -> usize;
    /// # Errors
    ///
    /// Returns an error when local or external embedding production is unavailable.
    fn embed(&self, text: &str) -> Result<Vec<f32>, RetrievalError>;
}

pub trait VectorIndex: Send + Sync {
    /// # Errors
    ///
    /// Returns an error when vector records cannot be stored.
    fn upsert(&self, records: Vec<VectorRecord>) -> Result<(), RetrievalError>;
    /// # Errors
    ///
    /// Returns an error when source projections cannot be removed.
    fn remove_source(
        &self,
        profile_id: &ProfileId,
        source_path: &str,
    ) -> Result<(), RetrievalError>;
    /// # Errors
    ///
    /// Returns an error when the vector index is unavailable or rejects the query.
    fn search(
        &self,
        profile_id: &ProfileId,
        query: &[f32],
        limit: usize,
    ) -> Result<Vec<VectorHit>, RetrievalError>;
}

#[derive(Clone)]
pub struct VectorComponents {
    pub embedder: Arc<dyn Embedder>,
    pub index: Arc<dyn VectorIndex>,
}

#[derive(Debug, Error)]
pub enum RetrievalError {
    #[error("retrieval I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("retrieval database failed: {0}")]
    Sql(#[from] rusqlite::Error),
    #[error("retrieval JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("retrieval state lock was poisoned")]
    LockPoisoned,
    #[error("retrieval policy contains an invalid bound or weight")]
    InvalidPolicy,
    #[error("search query must be non-empty")]
    EmptyQuery,
    #[error("source is oversized, invalid, or outside the supported workspace set")]
    InvalidSource,
    #[error("stored retrieval identity is invalid")]
    InvalidIdentity,
    #[error("knowledge source authorization or provenance is invalid")]
    InvalidAuthorization,
    #[error("knowledge authorization backend failed: {0}")]
    AuthorizationBackend(String),
    #[error("vector backend failed: {0}")]
    Vector(String),
}

pub struct RetrievalService {
    connection: Mutex<Connection>,
    limits: RetrievalLimits,
    weights: RankWeights,
    vectors: Option<VectorComponents>,
    health: Mutex<RetrievalHealth>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
struct StoredSourceAccess {
    owner_profile_id: ProfileId,
    source_path: String,
    visibility: KnowledgeVisibility,
    conversation_id: Option<ConversationId>,
    source_event_ids: BTreeSet<EventId>,
    explicit_grantees: BTreeSet<ProfileId>,
    grant_id: Option<GrantId>,
    space_id: Option<EntityId>,
    observed_permission_revision: Option<Revision>,
    policy_revision: Revision,
    revoked: bool,
}

struct PolicyAuthorizationObservation {
    provenance: Option<AuthorizedKnowledgeProvenance>,
    space: Option<ResolvedSpaceAuthorization>,
    grant: Option<ResolvedGrantAuthorization>,
}

impl StoredSourceAccess {
    fn authorize_space_query(
        &self,
        authorization: &AuthenticatedKnowledgeQuery,
        resolver: &impl KnowledgeAccessResolver,
    ) -> Result<PolicyAuthorizationObservation, RetrievalError> {
        let denied = || PolicyAuthorizationObservation {
            provenance: None,
            space: None,
            grant: None,
        };
        if self.revoked {
            return Ok(denied());
        }
        if self.visibility != KnowledgeVisibility::ExplicitlyShared
            || self.space_id.as_ref() != Some(&authorization.space_id)
        {
            return Ok(denied());
        }
        let observed = self
            .observed_permission_revision
            .ok_or(RetrievalError::InvalidAuthorization)?;
        let Some(space) = resolver.authorize_space(
            &authorization.space_id,
            observed,
            &authorization.requester,
            authorization.operation,
            authorization.now,
        )?
        else {
            return Ok(denied());
        };
        let grant_id = self
            .grant_id
            .as_ref()
            .ok_or(RetrievalError::InvalidAuthorization)?;
        let Some(grant) = resolver.authorize_grant(
            grant_id,
            &authorization.space_id,
            &authorization.requester,
            authorization.operation,
            authorization.now,
        )?
        else {
            return Ok(PolicyAuthorizationObservation {
                provenance: None,
                space: Some(space),
                grant: None,
            });
        };
        if grant.grant_id != *grant_id {
            return Err(RetrievalError::InvalidAuthorization);
        }
        let provenance = (grant.status == GrantAuthorizationStatus::Active
            && grant.resource_policy_revision == self.policy_revision)
            .then(|| AuthorizedKnowledgeProvenance {
                owner_profile_id: self.owner_profile_id.clone(),
                space_id: authorization.space_id.clone(),
                observed_permission_revision: observed,
                space_revision: space.space_revision,
                membership_permission_revision: space.membership_permission_revision,
                grant_id: grant.grant_id.clone(),
                grant_revision: grant.grant_revision,
                resource_policy_revision: grant.resource_policy_revision,
                source_conversation_id: self.conversation_id.clone(),
                source_event_ids: self.source_event_ids.clone(),
                source_policy_revision: self.policy_revision,
            });
        Ok(PolicyAuthorizationObservation {
            provenance,
            space: Some(space),
            grant: Some(grant),
        })
    }
}

impl RetrievalService {
    /// Returns exact derived-index records owned by a profile for deletion orchestration.
    ///
    /// # Errors
    /// Returns an error for corrupt identities/policies or database failure.
    pub fn inventory_profile_deletion(
        &self,
        profile_id: &ProfileId,
    ) -> Result<RetrievalDeletionInventory, RetrievalError> {
        let mut records = self
            .load_documents(profile_id)?
            .into_iter()
            .map(|document| RetrievalDeletionRecord {
                stable_key: format!(
                    "retrieval:document:{profile_id}:{}:{}",
                    document.document_id, document.source_version
                ),
                classification: DeletionClassification::DeletePrivate,
                kind: RetrievalDeletionRecordKind::Document {
                    document_id: document.document_id,
                    source_version: document.source_version,
                    source_path: document.source_path,
                },
            })
            .collect::<Vec<_>>();
        records.extend(
            self.list_source_access()?
                .into_iter()
                .filter(|access| &access.owner_profile_id == profile_id)
                .map(|access| RetrievalDeletionRecord {
                    stable_key: format!(
                        "retrieval:access:{profile_id}:{}:{}",
                        access.source_path,
                        access.policy_revision.get()
                    ),
                    classification: if matches!(access.visibility, KnowledgeVisibility::Private) {
                        DeletionClassification::DeletePrivate
                    } else {
                        DeletionClassification::RetainShared
                    },
                    kind: RetrievalDeletionRecordKind::SourceAccess {
                        source_path: access.source_path,
                        policy_revision: access.policy_revision,
                    },
                }),
        );
        records.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        let inventory_digest = retrieval_deletion_digest(&records);
        Ok(RetrievalDeletionInventory {
            profile_id: profile_id.clone(),
            inventory_digest,
            records,
        })
    }

    /// Erases exact private projections and rejects inventories invalidated by concurrent writes.
    ///
    /// # Errors
    /// Returns an error for a stale inventory or transactional/vector-index failure.
    pub fn erase_profile_inventory(
        &self,
        inventory: &RetrievalDeletionInventory,
    ) -> Result<RetrievalDeletionReceipt, RetrievalError> {
        let current = self.inventory_profile_deletion(&inventory.profile_id)?;
        let retained = inventory
            .records
            .iter()
            .filter(|record| record.classification != DeletionClassification::DeletePrivate)
            .cloned()
            .collect::<Vec<_>>();
        if &current != inventory {
            if current.records == retained {
                return Ok(retrieval_deletion_receipt(inventory, retained));
            }
            return Err(RetrievalError::InvalidAuthorization);
        }
        let mut connection = self.connection()?;
        let transaction = connection.transaction()?;
        let mut removed_paths = BTreeSet::new();
        for record in inventory
            .records
            .iter()
            .filter(|record| record.classification == DeletionClassification::DeletePrivate)
        {
            match &record.kind {
                RetrievalDeletionRecordKind::Document {
                    document_id,
                    source_version,
                    source_path,
                } => {
                    let changed = transaction.execute(
                        "DELETE FROM documents WHERE profile_id = ?1 AND document_id = ?2 AND source_version = ?3",
                        params![inventory.profile_id.to_string(), document_id.to_string(), source_version],
                    )?;
                    if changed != 1 {
                        return Err(RetrievalError::InvalidAuthorization);
                    }
                    removed_paths.insert(source_path.clone());
                }
                RetrievalDeletionRecordKind::SourceAccess {
                    source_path,
                    policy_revision,
                } => {
                    let changed = transaction.execute(
                        "DELETE FROM source_access WHERE owner_profile_id = ?1 AND source_path = ?2 AND policy_revision = ?3",
                        params![inventory.profile_id.to_string(), source_path, i64::try_from(policy_revision.get()).map_err(|_| RetrievalError::InvalidAuthorization)?],
                    )?;
                    if changed != 1 {
                        return Err(RetrievalError::InvalidAuthorization);
                    }
                }
            }
        }
        transaction.execute(
            "INSERT INTO documents_fts(documents_fts) VALUES('rebuild')",
            [],
        )?;
        transaction.commit()?;
        if let Some(vector) = &self.vectors {
            for source_path in removed_paths {
                vector
                    .index
                    .remove_source(&inventory.profile_id, &source_path)?;
            }
        }
        Ok(retrieval_deletion_receipt(inventory, retained))
    }

    /// Re-enumerates remaining private leaks and classified retained projections.
    ///
    /// # Errors
    /// Returns an error for corrupt or unreadable index state.
    pub fn scan_profile_deletion_leaks(
        &self,
        profile_id: &ProfileId,
    ) -> Result<RetrievalLeakScan, RetrievalError> {
        let inventory = self.inventory_profile_deletion(profile_id)?;
        let (leaks, retained): (Vec<_>, Vec<_>) = inventory
            .records
            .into_iter()
            .partition(|record| record.classification == DeletionClassification::DeletePrivate);
        Ok(RetrievalLeakScan {
            profile_id: profile_id.clone(),
            leaked_private_keys: leaks.into_iter().map(|record| record.stable_key).collect(),
            retained,
        })
    }
    /// Opens or creates the derived retrieval index, quarantining corrupt database bytes.
    ///
    /// # Errors
    ///
    /// Returns an error when the index directory or fresh replacement database cannot be opened.
    pub fn open(
        index_root: impl AsRef<Path>,
        limits: RetrievalLimits,
        weights: RankWeights,
        vectors: Option<VectorComponents>,
    ) -> Result<Self, RetrievalError> {
        validate_configuration(limits, weights)?;
        fs::create_dir_all(index_root.as_ref())?;
        let index_path = index_root.as_ref().join(INDEX_FILE);
        let (connection, quarantined_index, detail) = open_or_quarantine(&index_path)?;
        let degraded = quarantined_index.is_some();
        Ok(Self {
            connection: Mutex::new(connection),
            limits,
            weights,
            health: Mutex::new(RetrievalHealth {
                degraded,
                vector_available: vectors.is_some(),
                detail,
                quarantined_index,
            }),
            vectors,
        })
    }

    /// Parses and atomically replaces all supplied source versions for one profile.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid sources, database failures, or poisoned state.
    pub fn index_sources(
        &self,
        profile_id: &ProfileId,
        sources: &[SourceInput],
    ) -> Result<usize, RetrievalError> {
        let mut parsed = Vec::new();
        for source in sources {
            validate_source(source, self.limits)?;
            parsed.extend(parse_source(
                profile_id,
                source,
                self.limits.max_chunk_bytes,
            ));
        }
        if parsed.len() > self.limits.max_documents_per_profile {
            return Err(RetrievalError::InvalidSource);
        }
        self.replace_documents(profile_id, &parsed)?;
        self.index_vectors(&parsed);
        let mut health = self.health()?;
        health.degraded = false;
        if health.detail.as_deref() == Some("derived index was quarantined") {
            health.detail = None;
        }
        Ok(parsed.len())
    }

    /// Indexes sources together with durable conversation/share provenance.
    ///
    /// # Errors
    /// Returns an error for invalid bounds, mismatched ownership, or persistence failure.
    pub fn index_authorized_sources(
        &self,
        sources: &[AuthorizedSourceInput],
    ) -> Result<usize, RetrievalError> {
        if sources.is_empty() {
            return Ok(0);
        }
        for source in sources {
            validate_provenance(&source.provenance)?;
            validate_source(&source.source, self.limits)?;
        }
        let owners = sources
            .iter()
            .map(|source| source.provenance.owner_profile_id.clone())
            .collect::<BTreeSet<_>>();
        if owners.len() != 1 {
            return Err(RetrievalError::InvalidAuthorization);
        }
        let owner = owners
            .into_iter()
            .next()
            .ok_or(RetrievalError::InvalidAuthorization)?;
        let plain = sources
            .iter()
            .map(|source| source.source.clone())
            .collect::<Vec<_>>();
        let count = self.index_sources(&owner, &plain)?;
        let mut connection = self.connection()?;
        let transaction = connection.transaction()?;
        for source in sources {
            transaction.execute(
                "INSERT INTO source_access(
                    owner_profile_id, source_path, visibility, conversation_id,
                    source_event_ids, explicit_grantees, grant_id, space_id,
                    observed_permission_revision, policy_revision, revoked
                 ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, ?9, ?10, ?11)
                 ON CONFLICT(owner_profile_id, source_path) DO UPDATE SET
                    visibility = excluded.visibility,
                    conversation_id = excluded.conversation_id,
                    source_event_ids = excluded.source_event_ids,
                    explicit_grantees = excluded.explicit_grantees,
                    grant_id = excluded.grant_id,
                    space_id = excluded.space_id,
                    observed_permission_revision = excluded.observed_permission_revision,
                    policy_revision = excluded.policy_revision,
                    revoked = excluded.revoked",
                params![
                    owner.to_string(),
                    source.source.source_path,
                    visibility_name(source.provenance.visibility),
                    source
                        .provenance
                        .conversation_id
                        .as_ref()
                        .map(ToString::to_string),
                    serde_json::to_string(&source.provenance.source_event_ids)?,
                    serde_json::to_string(&source.provenance.explicit_grantees)?,
                    source.provenance.grant_id.as_ref().map(ToString::to_string),
                    source.provenance.space_id.as_ref().map(ToString::to_string),
                    source
                        .provenance
                        .observed_permission_revision
                        .map(Revision::get)
                        .map(i64::try_from)
                        .transpose()
                        .map_err(|_| RetrievalError::InvalidAuthorization)?,
                    i64::try_from(source.provenance.policy_revision.get())
                        .map_err(|_| RetrievalError::InvalidAuthorization)?,
                    i64::from(source.provenance.revoked),
                ],
            )?;
        }
        transaction.commit()?;
        Ok(count)
    }

    /// Revokes one explicit grantee using an optimistic policy revision.
    ///
    /// # Errors
    /// Returns an error for missing/stale policy or persistence failure.
    pub fn revoke_source_grant(
        &self,
        owner: &ProfileId,
        source_path: &str,
        grantee: &ProfileId,
        expected_revision: Revision,
    ) -> Result<(), RetrievalError> {
        let connection = self.connection()?;
        let stored = load_source_access(&connection, owner, source_path)?
            .ok_or(RetrievalError::InvalidAuthorization)?;
        if stored.policy_revision != expected_revision {
            return Err(RetrievalError::InvalidAuthorization);
        }
        let mut grantees = stored.explicit_grantees;
        if !grantees.remove(grantee) {
            return Err(RetrievalError::InvalidAuthorization);
        }
        let next = expected_revision
            .checked_next()
            .ok_or(RetrievalError::InvalidAuthorization)?;
        let revoked = grantees.is_empty();
        let changed = connection.execute(
            "UPDATE source_access SET explicit_grantees = ?1, policy_revision = ?2, revoked = ?3
             WHERE owner_profile_id = ?4 AND source_path = ?5 AND policy_revision = ?6",
            params![
                serde_json::to_string(&grantees)?,
                i64::try_from(next.get()).map_err(|_| RetrievalError::InvalidAuthorization)?,
                i64::from(revoked),
                owner.to_string(),
                source_path,
                i64::try_from(expected_revision.get())
                    .map_err(|_| RetrievalError::InvalidAuthorization)?,
            ],
        )?;
        if changed != 1 {
            return Err(RetrievalError::InvalidAuthorization);
        }
        Ok(())
    }

    /// Rebuilds supported memory, state, knowledge, summary, and skill sources from readable files.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe/oversized sources, filesystem failure, or index failure.
    pub fn rebuild_workspace(
        &self,
        profile_id: &ProfileId,
        workspace_root: impl AsRef<Path>,
        modified_at: UtcTimestamp,
    ) -> Result<usize, RetrievalError> {
        let sources = collect_workspace_sources(workspace_root.as_ref(), modified_at, self.limits)?;
        let mut connection = self.connection()?;
        let transaction = connection.transaction()?;
        transaction.execute(
            "DELETE FROM documents WHERE profile_id = ?1",
            params![profile_id.to_string()],
        )?;
        transaction.execute(
            "INSERT INTO documents_fts(documents_fts) VALUES('rebuild')",
            [],
        )?;
        transaction.commit()?;
        drop(connection);
        self.index_sources(profile_id, &sources)
    }

    /// Removes a source and all lexical, trigram, and vector projections for one profile.
    ///
    /// # Errors
    ///
    /// Returns an error when durable derived state cannot be updated.
    pub fn remove_source(
        &self,
        profile_id: &ProfileId,
        source_path: &str,
    ) -> Result<(), RetrievalError> {
        let mut connection = self.connection()?;
        let transaction = connection.transaction()?;
        transaction.execute(
            "DELETE FROM documents WHERE profile_id = ?1 AND source_path = ?2",
            params![profile_id.to_string(), source_path],
        )?;
        transaction.execute(
            "DELETE FROM source_access WHERE owner_profile_id = ?1 AND source_path = ?2",
            params![profile_id.to_string(), source_path],
        )?;
        transaction.execute(
            "INSERT INTO documents_fts(documents_fts) VALUES('rebuild')",
            [],
        )?;
        transaction.commit()?;
        drop(connection);
        if let Some(vectors) = &self.vectors
            && let Err(error) = vectors.index.remove_source(profile_id, source_path)
        {
            self.vector_failed(error.to_string());
        }
        Ok(())
    }

    /// Strictly removes lexical, trigram, and vector projections for a deleted source.
    ///
    /// Unlike ordinary degraded retrieval, source deletion must not hide a vector cleanup
    /// failure because that could retain data after the source is gone.
    ///
    /// # Errors
    ///
    /// Returns an error when any associated projection cannot be removed.
    pub fn purge_source(
        &self,
        profile_id: &ProfileId,
        source_path: &str,
    ) -> Result<(), RetrievalError> {
        let mut connection = self.connection()?;
        let transaction = connection.transaction()?;
        transaction.execute(
            "DELETE FROM documents WHERE profile_id = ?1 AND source_path = ?2",
            params![profile_id.to_string(), source_path],
        )?;
        transaction.execute(
            "DELETE FROM source_access WHERE owner_profile_id = ?1 AND source_path = ?2",
            params![profile_id.to_string(), source_path],
        )?;
        transaction.execute(
            "INSERT INTO documents_fts(documents_fts) VALUES('rebuild')",
            [],
        )?;
        transaction.commit()?;
        drop(connection);
        if let Some(vectors) = &self.vectors {
            vectors.index.remove_source(profile_id, source_path)?;
        }
        Ok(())
    }

    /// Searches one profile with normalized lexical, Unicode trigram, and optional vector ranks.
    ///
    /// The merger uses bounded scores in `[0, 1]` and renormalizes available weights when the
    /// optional vector path is absent or unhealthy.
    ///
    /// # Errors
    ///
    /// Returns an error for an empty query, database failure, or invalid persisted identity.
    pub fn search(
        &self,
        profile_id: &ProfileId,
        query: &str,
        limit: usize,
    ) -> Result<Vec<SearchResult>, RetrievalError> {
        if query.trim().is_empty() {
            return Err(RetrievalError::EmptyQuery);
        }
        let documents = self.load_documents(profile_id)?;
        let lexical = self.lexical_scores(profile_id, query)?;
        let vector = self.vector_scores(profile_id, query, limit.max(32));
        let query_normalized = normalize(query);
        let query_trigrams = trigrams(&query_normalized);
        let mut results = documents
            .into_iter()
            .filter_map(|document| {
                score_document(
                    document,
                    &query_normalized,
                    &query_trigrams,
                    &lexical,
                    vector.as_ref(),
                    self.weights,
                    self.limits.max_excerpt_chars,
                )
            })
            .collect::<Vec<_>>();
        results.sort_by(|left, right| {
            right
                .merged_score
                .total_cmp(&left.merged_score)
                .then_with(|| left.source_path.cmp(&right.source_path))
        });
        results.truncate(limit);
        Ok(results)
    }

    /// Searches one authenticated shared space and returns the exact authorization observation.
    ///
    /// # Errors
    /// Returns an error for malformed persisted policy, empty query, or database failure.
    pub fn search_authorized(
        &self,
        authorization: &AuthenticatedKnowledgeQuery,
        resolver: &impl KnowledgeAccessResolver,
    ) -> Result<AuthorizedKnowledgeSearchResponse, RetrievalError> {
        if authorization.query.trim().is_empty() {
            return Err(RetrievalError::EmptyQuery);
        }
        if authorization.limit == 0 || authorization.limit > self.limits.max_scan_results {
            return Err(RetrievalError::InvalidPolicy);
        }
        let policies = self.list_source_access()?;
        let mut allowed =
            BTreeMap::<ProfileId, BTreeMap<String, AuthorizedKnowledgeProvenance>>::new();
        let mut observation = KnowledgeAuthorizationObservation::default();
        for policy in policies {
            let decision = policy.authorize_space_query(authorization, resolver)?;
            if let Some(space) = decision.space {
                if observation
                    .space
                    .as_ref()
                    .is_some_and(|current| current != &space)
                {
                    return Err(RetrievalError::InvalidAuthorization);
                }
                observation.space = Some(space);
            }
            if let Some(grant) = decision.grant
                && observation
                    .grants
                    .insert(grant.grant_id.clone(), grant.clone())
                    .is_some_and(|current| current != grant)
            {
                return Err(RetrievalError::InvalidAuthorization);
            }
            if let Some(provenance) = decision.provenance {
                allowed
                    .entry(policy.owner_profile_id)
                    .or_default()
                    .insert(policy.source_path, provenance);
            }
        }
        let mut results = Vec::new();
        for (owner, paths) in allowed {
            for result in self.search(&owner, &authorization.query, authorization.limit.max(32))? {
                if let Some(provenance) = paths.get(&result.source_path) {
                    results.push(AuthorizedKnowledgeSearchResult {
                        result,
                        provenance: provenance.clone(),
                    });
                }
            }
        }
        results.sort_by(|left, right| {
            right
                .result
                .merged_score
                .total_cmp(&left.result.merged_score)
                .then_with(|| left.result.source_path.cmp(&right.result.source_path))
        });
        results.truncate(authorization.limit);
        Ok(AuthorizedKnowledgeSearchResponse {
            results,
            authorization: observation,
        })
    }

    /// Returns current degradation, quarantine, and optional-vector availability state.
    ///
    /// # Errors
    ///
    /// Returns an error when the health lock is poisoned.
    pub fn health_snapshot(&self) -> Result<RetrievalHealth, RetrievalError> {
        Ok(self.health()?.clone())
    }

    fn replace_documents(
        &self,
        profile_id: &ProfileId,
        documents: &[SearchDocument],
    ) -> Result<(), RetrievalError> {
        let paths = documents
            .iter()
            .map(|document| document.source_path.as_str())
            .collect::<BTreeSet<_>>();
        let mut connection = self.connection()?;
        let transaction = connection.transaction()?;
        for path in paths {
            transaction.execute(
                "DELETE FROM documents WHERE profile_id = ?1 AND source_path = ?2",
                params![profile_id.to_string(), path],
            )?;
        }
        for document in documents {
            transaction.execute(
                "INSERT INTO documents (
                    document_id, profile_id, source_path, source_version, heading_path,
                    text, source_kind, modified_at
                 ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8)",
                params![
                    document.document_id.to_string(),
                    document.profile_id.to_string(),
                    document.source_path,
                    document.source_version,
                    serde_json::to_string(&document.heading_path)?,
                    document.text,
                    source_kind_name(document.source_kind),
                    document.modified_at.unix_millis(),
                ],
            )?;
        }
        transaction.execute(
            "INSERT INTO documents_fts(documents_fts) VALUES('rebuild')",
            [],
        )?;
        transaction.commit()?;
        Ok(())
    }

    fn index_vectors(&self, documents: &[SearchDocument]) {
        let Some(vectors) = &self.vectors else {
            return;
        };
        let records = documents
            .iter()
            .map(|document| {
                vectors
                    .embedder
                    .embed(&document.text)
                    .map(|vector| VectorRecord {
                        document_id: document.document_id.clone(),
                        profile_id: document.profile_id.clone(),
                        source_path: document.source_path.clone(),
                        vector,
                    })
            })
            .collect::<Result<Vec<_>, _>>()
            .and_then(|records| vectors.index.upsert(records));
        if let Err(error) = records {
            self.vector_failed(error.to_string());
        }
    }

    fn vector_scores(
        &self,
        profile_id: &ProfileId,
        query: &str,
        limit: usize,
    ) -> Option<BTreeMap<EntityId, f32>> {
        let vectors = self.vectors.as_ref()?;
        let result = vectors
            .embedder
            .embed(query)
            .and_then(|embedding| vectors.index.search(profile_id, &embedding, limit));
        match result {
            Ok(hits) => Some(
                hits.into_iter()
                    .map(|hit| (hit.document_id, hit.score.clamp(0.0, 1.0)))
                    .collect(),
            ),
            Err(error) => {
                self.vector_failed(error.to_string());
                None
            }
        }
    }

    fn lexical_scores(
        &self,
        profile_id: &ProfileId,
        query: &str,
    ) -> Result<BTreeMap<EntityId, f32>, RetrievalError> {
        let fts_query = query
            .split_whitespace()
            .filter(|token| !token.is_empty())
            .map(|token| format!("\"{}\"", token.replace('"', "\"\"")))
            .collect::<Vec<_>>()
            .join(" OR ");
        if fts_query.is_empty() {
            return Ok(BTreeMap::new());
        }
        let connection = self.connection()?;
        let mut statement = connection.prepare(
            "SELECT d.document_id, bm25(documents_fts)
             FROM documents_fts
             JOIN documents d ON d.row_id = documents_fts.rowid
             WHERE documents_fts MATCH ?1 AND d.profile_id = ?2
             LIMIT ?3",
        )?;
        let rows = statement.query_map(
            params![
                fts_query,
                profile_id.to_string(),
                i64::try_from(self.limits.max_scan_results).unwrap_or(i64::MAX)
            ],
            |row| Ok((row.get::<_, String>(0)?, row.get::<_, f32>(1)?)),
        )?;
        let mut scores = BTreeMap::new();
        for row in rows {
            let (id, rank) = row?;
            scores.insert(
                EntityId::from_str(&id).map_err(|_| RetrievalError::InvalidIdentity)?,
                1.0 / (1.0 + rank.abs()),
            );
        }
        Ok(scores)
    }

    fn load_documents(
        &self,
        profile_id: &ProfileId,
    ) -> Result<Vec<SearchDocument>, RetrievalError> {
        let connection = self.connection()?;
        let mut statement = connection.prepare(
            "SELECT document_id, profile_id, source_path, source_version, heading_path,
                    text, source_kind, modified_at
             FROM documents WHERE profile_id = ?1 ORDER BY row_id LIMIT ?2",
        )?;
        let rows = statement.query_map(
            params![
                profile_id.to_string(),
                i64::try_from(self.limits.max_scan_results).unwrap_or(i64::MAX)
            ],
            stored_document,
        )?;
        rows.map(|row| row.map_err(RetrievalError::from)).collect()
    }

    fn list_source_access(&self) -> Result<Vec<StoredSourceAccess>, RetrievalError> {
        let connection = self.connection()?;
        let mut statement = connection.prepare(
            "SELECT owner_profile_id, source_path, visibility, conversation_id,
                    source_event_ids, explicit_grantees, grant_id, space_id,
                    observed_permission_revision, policy_revision, revoked
             FROM source_access ORDER BY owner_profile_id, source_path",
        )?;
        let rows = statement.query_map([], |row| {
            Ok((
                row.get::<_, String>(0)?,
                row.get::<_, String>(1)?,
                row.get::<_, String>(2)?,
                row.get::<_, Option<String>>(3)?,
                row.get::<_, String>(4)?,
                row.get::<_, String>(5)?,
                row.get::<_, Option<String>>(6)?,
                row.get::<_, Option<String>>(7)?,
                row.get::<_, Option<i64>>(8)?,
                row.get::<_, i64>(9)?,
                row.get::<_, i64>(10)?,
            ))
        })?;
        rows.map(|row| parse_source_access(row?)).collect()
    }

    fn vector_failed(&self, detail: String) {
        if let Ok(mut health) = self.health.lock() {
            health.vector_available = false;
            health.detail = Some(detail);
        }
    }

    fn connection(&self) -> Result<MutexGuard<'_, Connection>, RetrievalError> {
        self.connection
            .lock()
            .map_err(|_| RetrievalError::LockPoisoned)
    }

    fn health(&self) -> Result<MutexGuard<'_, RetrievalHealth>, RetrievalError> {
        self.health.lock().map_err(|_| RetrievalError::LockPoisoned)
    }
}

#[derive(Clone, Debug)]
pub struct LocalHashEmbedder {
    dimensions: usize,
}

impl LocalHashEmbedder {
    /// Creates a deterministic local Unicode n-gram embedding model.
    ///
    /// # Errors
    ///
    /// Returns an error when the dimension is too small for useful projection.
    pub fn new(dimensions: usize) -> Result<Self, RetrievalError> {
        if dimensions < 16 {
            Err(RetrievalError::InvalidPolicy)
        } else {
            Ok(Self { dimensions })
        }
    }
}

impl Embedder for LocalHashEmbedder {
    fn dimensions(&self) -> usize {
        self.dimensions
    }

    fn embed(&self, text: &str) -> Result<Vec<f32>, RetrievalError> {
        let normalized = normalize(text);
        let features = semantic_features(&normalized);
        let mut vector = vec![0.0_f32; self.dimensions];
        for feature in features {
            let digest = Sha256::digest(feature.as_bytes());
            let mut bytes = [0_u8; 8];
            bytes.copy_from_slice(&digest[..8]);
            let hash = u64::from_le_bytes(bytes);
            let dimensions =
                u64::try_from(self.dimensions).map_err(|_| RetrievalError::InvalidPolicy)?;
            let index =
                usize::try_from(hash % dimensions).map_err(|_| RetrievalError::InvalidPolicy)?;
            let sign = if digest[8] & 1 == 0 { 1.0 } else { -1.0 };
            vector[index] += sign;
        }
        normalize_vector(&mut vector);
        Ok(vector)
    }
}

#[derive(Default)]
pub struct MemoryVectorIndex {
    records: RwLock<BTreeMap<EntityId, VectorRecord>>,
}

impl VectorIndex for MemoryVectorIndex {
    fn upsert(&self, records: Vec<VectorRecord>) -> Result<(), RetrievalError> {
        let mut state = self
            .records
            .write()
            .map_err(|_| RetrievalError::LockPoisoned)?;
        for record in records {
            state.insert(record.document_id.clone(), record);
        }
        Ok(())
    }

    fn remove_source(
        &self,
        profile_id: &ProfileId,
        source_path: &str,
    ) -> Result<(), RetrievalError> {
        self.records
            .write()
            .map_err(|_| RetrievalError::LockPoisoned)?
            .retain(|_, record| {
                &record.profile_id != profile_id || record.source_path != source_path
            });
        Ok(())
    }

    fn search(
        &self,
        profile_id: &ProfileId,
        query: &[f32],
        limit: usize,
    ) -> Result<Vec<VectorHit>, RetrievalError> {
        let state = self
            .records
            .read()
            .map_err(|_| RetrievalError::LockPoisoned)?;
        let mut hits = state
            .values()
            .filter(|record| &record.profile_id == profile_id && record.vector.len() == query.len())
            .map(|record| VectorHit {
                document_id: record.document_id.clone(),
                score: f32::midpoint(cosine(query, &record.vector), 1.0).clamp(0.0, 1.0),
            })
            .collect::<Vec<_>>();
        hits.sort_by(|left, right| right.score.total_cmp(&left.score));
        hits.truncate(limit);
        Ok(hits)
    }
}

fn validate_configuration(
    limits: RetrievalLimits,
    weights: RankWeights,
) -> Result<(), RetrievalError> {
    let sum = weights.lexical + weights.trigram + weights.vector;
    if limits.max_source_bytes == 0
        || limits.max_chunk_bytes == 0
        || limits.max_documents_per_profile == 0
        || limits.max_scan_results == 0
        || limits.max_excerpt_chars == 0
        || weights.lexical < 0.0
        || weights.trigram < 0.0
        || weights.vector < 0.0
        || sum <= f32::EPSILON
    {
        Err(RetrievalError::InvalidPolicy)
    } else {
        Ok(())
    }
}

fn open_or_quarantine(
    index_path: &Path,
) -> Result<(Connection, Option<PathBuf>, Option<String>), RetrievalError> {
    match open_healthy(index_path) {
        Ok(connection) => Ok((connection, None, None)),
        Err(_) if index_path.exists() => {
            let quarantine =
                index_path.with_file_name(format!("retrieval.corrupt-{}.sqlite3", EntityId::new()));
            fs::rename(index_path, &quarantine)?;
            let connection = open_healthy(index_path)?;
            Ok((
                connection,
                Some(quarantine),
                Some("derived index was quarantined".into()),
            ))
        }
        Err(error) => Err(error),
    }
}

fn open_healthy(path: &Path) -> Result<Connection, RetrievalError> {
    let connection = Connection::open(path)?;
    connection.execute_batch(
        "PRAGMA journal_mode = WAL;
         PRAGMA foreign_keys = ON;
         CREATE TABLE IF NOT EXISTS metadata (
            key TEXT PRIMARY KEY,
            value INTEGER NOT NULL
         );
         CREATE TABLE IF NOT EXISTS documents (
            row_id INTEGER PRIMARY KEY,
            document_id TEXT NOT NULL UNIQUE,
            profile_id TEXT NOT NULL,
            source_path TEXT NOT NULL,
            source_version TEXT NOT NULL,
            heading_path TEXT NOT NULL,
            text TEXT NOT NULL,
            source_kind TEXT NOT NULL,
            modified_at INTEGER NOT NULL
         );
         CREATE INDEX IF NOT EXISTS documents_profile_path
            ON documents(profile_id, source_path);
         CREATE TABLE IF NOT EXISTS source_access (
            owner_profile_id TEXT NOT NULL,
            source_path TEXT NOT NULL,
            visibility TEXT NOT NULL,
            conversation_id TEXT,
            source_event_ids TEXT NOT NULL,
            explicit_grantees TEXT NOT NULL,
            grant_id TEXT,
            space_id TEXT,
            observed_permission_revision INTEGER,
            policy_revision INTEGER NOT NULL,
            revoked INTEGER NOT NULL CHECK(revoked IN (0, 1)),
            PRIMARY KEY(owner_profile_id, source_path)
         ) WITHOUT ROWID;
         CREATE VIRTUAL TABLE IF NOT EXISTS documents_fts USING fts5(
            text,
            content='documents',
            content_rowid='row_id',
            tokenize='unicode61 remove_diacritics 2'
         );",
    )?;
    let has_grant_id = {
        let mut statement = connection.prepare("PRAGMA table_info(source_access)")?;
        let columns = statement.query_map([], |row| row.get::<_, String>(1))?;
        columns
            .collect::<Result<Vec<_>, _>>()?
            .iter()
            .any(|column| column == "grant_id")
    };
    if !has_grant_id {
        connection.execute("ALTER TABLE source_access ADD COLUMN grant_id TEXT", [])?;
    }
    let columns = {
        let mut statement = connection.prepare("PRAGMA table_info(source_access)")?;
        statement
            .query_map([], |row| row.get::<_, String>(1))?
            .collect::<Result<BTreeSet<_>, _>>()?
    };
    if !columns.contains("space_id") {
        connection.execute("ALTER TABLE source_access ADD COLUMN space_id TEXT", [])?;
    }
    if !columns.contains("observed_permission_revision") {
        connection.execute(
            "ALTER TABLE source_access ADD COLUMN observed_permission_revision INTEGER",
            [],
        )?;
    }
    let existing = connection
        .query_row(
            "SELECT value FROM metadata WHERE key = 'schema'",
            [],
            |row| row.get::<_, i64>(0),
        )
        .optional()?;
    match existing {
        Some(version) if version != INDEX_SCHEMA => return Err(RetrievalError::InvalidPolicy),
        Some(_) => {}
        None => {
            connection.execute(
                "INSERT INTO metadata(key, value) VALUES('schema', ?1)",
                params![INDEX_SCHEMA],
            )?;
        }
    }
    let check = connection.query_row("PRAGMA quick_check", [], |row| row.get::<_, String>(0))?;
    if check != "ok" {
        return Err(RetrievalError::InvalidPolicy);
    }
    Ok(connection)
}

fn validate_source(source: &SourceInput, limits: RetrievalLimits) -> Result<(), RetrievalError> {
    if source.source_path.trim().is_empty()
        || source.source_version.trim().is_empty()
        || source.text.len() > limits.max_source_bytes
        || source.source_path.starts_with('/')
        || source.source_path.split('/').any(|part| part == "..")
    {
        Err(RetrievalError::InvalidSource)
    } else {
        Ok(())
    }
}

fn validate_provenance(provenance: &KnowledgeSourceProvenance) -> Result<(), RetrievalError> {
    if provenance.source_event_ids.len() > 256
        || provenance.explicit_grantees.len() > 256
        || matches!(
            provenance.visibility,
            KnowledgeVisibility::ConversationShared
        ) && provenance.conversation_id.is_none()
        || matches!(provenance.visibility, KnowledgeVisibility::Private)
            && (provenance.conversation_id.is_some()
                || !provenance.explicit_grantees.is_empty()
                || provenance.grant_id.is_some()
                || provenance.space_id.is_some()
                || provenance.observed_permission_revision.is_some())
        || matches!(
            provenance.visibility,
            KnowledgeVisibility::ConversationShared
        ) && (provenance.space_id.is_some() || provenance.observed_permission_revision.is_some())
        || matches!(provenance.visibility, KnowledgeVisibility::ExplicitlyShared)
            && (provenance.grant_id.is_none()
                || provenance.space_id.is_none()
                || provenance.observed_permission_revision.is_none())
    {
        return Err(RetrievalError::InvalidAuthorization);
    }
    Ok(())
}

const fn visibility_name(visibility: KnowledgeVisibility) -> &'static str {
    match visibility {
        KnowledgeVisibility::Private => "private",
        KnowledgeVisibility::ConversationShared => "conversation_shared",
        KnowledgeVisibility::ExplicitlyShared => "explicitly_shared",
    }
}

fn retrieval_deletion_digest(records: &[RetrievalDeletionRecord]) -> String {
    digest_text(
        &records
            .iter()
            .map(|record| record.stable_key.as_str())
            .collect::<Vec<_>>()
            .join("\n"),
    )
}

fn retrieval_deletion_receipt(
    inventory: &RetrievalDeletionInventory,
    retained: Vec<RetrievalDeletionRecord>,
) -> RetrievalDeletionReceipt {
    RetrievalDeletionReceipt {
        profile_id: inventory.profile_id.clone(),
        inventory_digest: inventory.inventory_digest.clone(),
        erased_stable_keys: inventory
            .records
            .iter()
            .filter(|record| record.classification == DeletionClassification::DeletePrivate)
            .map(|record| record.stable_key.clone())
            .collect(),
        retained,
    }
}

fn parse_visibility(value: &str) -> Result<KnowledgeVisibility, RetrievalError> {
    match value {
        "private" => Ok(KnowledgeVisibility::Private),
        "conversation_shared" => Ok(KnowledgeVisibility::ConversationShared),
        "explicitly_shared" => Ok(KnowledgeVisibility::ExplicitlyShared),
        _ => Err(RetrievalError::InvalidAuthorization),
    }
}

fn load_source_access(
    connection: &Connection,
    owner: &ProfileId,
    source_path: &str,
) -> Result<Option<StoredSourceAccess>, RetrievalError> {
    let raw = connection
        .query_row(
            "SELECT owner_profile_id, source_path, visibility, conversation_id,
                    source_event_ids, explicit_grantees, grant_id, space_id,
                    observed_permission_revision, policy_revision, revoked
             FROM source_access WHERE owner_profile_id = ?1 AND source_path = ?2",
            params![owner.to_string(), source_path],
            |row| {
                Ok((
                    row.get::<_, String>(0)?,
                    row.get::<_, String>(1)?,
                    row.get::<_, String>(2)?,
                    row.get::<_, Option<String>>(3)?,
                    row.get::<_, String>(4)?,
                    row.get::<_, String>(5)?,
                    row.get::<_, Option<String>>(6)?,
                    row.get::<_, Option<String>>(7)?,
                    row.get::<_, Option<i64>>(8)?,
                    row.get::<_, i64>(9)?,
                    row.get::<_, i64>(10)?,
                ))
            },
        )
        .optional()?;
    raw.map(parse_source_access).transpose()
}

type RawSourceAccess = (
    String,
    String,
    String,
    Option<String>,
    String,
    String,
    Option<String>,
    Option<String>,
    Option<i64>,
    i64,
    i64,
);

fn parse_source_access(raw: RawSourceAccess) -> Result<StoredSourceAccess, RetrievalError> {
    let policy_revision = u64::try_from(raw.9).map_err(|_| RetrievalError::InvalidAuthorization)?;
    if !matches!(raw.10, 0 | 1) {
        return Err(RetrievalError::InvalidAuthorization);
    }
    let stored = StoredSourceAccess {
        owner_profile_id: ProfileId::from_str(&raw.0)
            .map_err(|_| RetrievalError::InvalidIdentity)?,
        source_path: raw.1,
        visibility: parse_visibility(&raw.2)?,
        conversation_id: raw
            .3
            .map(|value| ConversationId::from_str(&value))
            .transpose()
            .map_err(|_| RetrievalError::InvalidIdentity)?,
        source_event_ids: serde_json::from_str(&raw.4)?,
        explicit_grantees: serde_json::from_str(&raw.5)?,
        grant_id: raw
            .6
            .map(|value| GrantId::from_str(&value))
            .transpose()
            .map_err(|_| RetrievalError::InvalidIdentity)?,
        space_id: raw
            .7
            .map(|value| EntityId::from_str(&value))
            .transpose()
            .map_err(|_| RetrievalError::InvalidIdentity)?,
        observed_permission_revision: raw
            .8
            .map(u64::try_from)
            .transpose()
            .map_err(|_| RetrievalError::InvalidAuthorization)?
            .map(Revision::new),
        policy_revision: Revision::new(policy_revision),
        revoked: raw.10 == 1,
    };
    validate_provenance(&KnowledgeSourceProvenance {
        owner_profile_id: stored.owner_profile_id.clone(),
        conversation_id: stored.conversation_id.clone(),
        source_event_ids: stored.source_event_ids.clone(),
        visibility: stored.visibility,
        explicit_grantees: stored.explicit_grantees.clone(),
        grant_id: stored.grant_id.clone(),
        space_id: stored.space_id.clone(),
        observed_permission_revision: stored.observed_permission_revision,
        policy_revision: stored.policy_revision,
        revoked: stored.revoked,
    })?;
    Ok(stored)
}

fn parse_source(
    profile_id: &ProfileId,
    source: &SourceInput,
    max_chunk_bytes: usize,
) -> Vec<SearchDocument> {
    let mut headings = Vec::<String>::new();
    let mut buffer = String::new();
    let mut documents = Vec::new();
    for line in source.text.lines() {
        if let Some((level, title)) = markdown_heading(line) {
            flush_chunks(
                profile_id,
                source,
                &headings,
                &mut buffer,
                max_chunk_bytes,
                &mut documents,
            );
            headings.truncate(level.saturating_sub(1));
            headings.push(title.to_owned());
        } else {
            if !buffer.is_empty() {
                buffer.push('\n');
            }
            buffer.push_str(line);
            if buffer.len() >= max_chunk_bytes {
                flush_chunks(
                    profile_id,
                    source,
                    &headings,
                    &mut buffer,
                    max_chunk_bytes,
                    &mut documents,
                );
            }
        }
    }
    flush_chunks(
        profile_id,
        source,
        &headings,
        &mut buffer,
        max_chunk_bytes,
        &mut documents,
    );
    documents
}

fn markdown_heading(line: &str) -> Option<(usize, &str)> {
    let hashes = line.bytes().take_while(|byte| *byte == b'#').count();
    if (1..=6).contains(&hashes) && line.as_bytes().get(hashes) == Some(&b' ') {
        let title = line[hashes + 1..].trim();
        (!title.is_empty()).then_some((hashes, title))
    } else {
        None
    }
}

fn flush_chunks(
    profile_id: &ProfileId,
    source: &SourceInput,
    headings: &[String],
    buffer: &mut String,
    max_chunk_bytes: usize,
    documents: &mut Vec<SearchDocument>,
) {
    let text = buffer.trim();
    if text.is_empty() {
        buffer.clear();
        return;
    }
    for chunk in bounded_chunks(text, max_chunk_bytes) {
        documents.push(SearchDocument {
            document_id: EntityId::new(),
            profile_id: profile_id.clone(),
            source_path: source.source_path.clone(),
            source_version: source.source_version.clone(),
            heading_path: headings.to_vec(),
            text: chunk,
            source_kind: source.source_kind,
            modified_at: source.modified_at,
        });
    }
    buffer.clear();
}

fn bounded_chunks(text: &str, max_bytes: usize) -> Vec<String> {
    let mut chunks = Vec::new();
    let mut current = String::new();
    for word in text.split_whitespace() {
        let needed = word.len() + usize::from(!current.is_empty());
        if !current.is_empty() && current.len().saturating_add(needed) > max_bytes {
            chunks.push(std::mem::take(&mut current));
        }
        if word.len() > max_bytes {
            for character in word.chars() {
                if current.len() + character.len_utf8() > max_bytes && !current.is_empty() {
                    chunks.push(std::mem::take(&mut current));
                }
                current.push(character);
            }
        } else {
            if !current.is_empty() {
                current.push(' ');
            }
            current.push_str(word);
        }
    }
    if !current.is_empty() {
        chunks.push(current);
    }
    chunks
}

fn collect_workspace_sources(
    root: &Path,
    modified_at: UtcTimestamp,
    limits: RetrievalLimits,
) -> Result<Vec<SourceInput>, RetrievalError> {
    let root = fs::canonicalize(root)?;
    let mut files = Vec::new();
    for relative in [
        "MEMORY.md",
        "memory/daily",
        "state",
        "knowledge",
        "skills",
        "summaries",
        ".keith/MEMORY.md",
        ".keith/memory/daily",
        ".keith/state",
        ".keith/knowledge",
        ".keith/skills",
        ".keith/summaries",
    ] {
        collect_supported_files(&root, Path::new(relative), &mut files, limits)?;
    }
    files.sort();
    files
        .into_iter()
        .map(|path| {
            let relative = path
                .strip_prefix(&root)
                .map_err(|_| RetrievalError::InvalidSource)?;
            let text = fs::read_to_string(&path).map_err(|error| {
                if error.kind() == std::io::ErrorKind::InvalidData {
                    RetrievalError::InvalidSource
                } else {
                    error.into()
                }
            })?;
            let source_path = relative.to_string_lossy().replace('\\', "/");
            Ok(SourceInput {
                source_version: digest_text(&text),
                source_kind: classify_source(&source_path)?,
                source_path,
                modified_at,
                text,
            })
        })
        .collect()
}

fn collect_supported_files(
    root: &Path,
    relative: &Path,
    files: &mut Vec<PathBuf>,
    limits: RetrievalLimits,
) -> Result<(), RetrievalError> {
    let path = root.join(relative);
    let metadata = match fs::symlink_metadata(&path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(()),
        Err(error) => return Err(error.into()),
    };
    if metadata.file_type().is_symlink() {
        return Err(RetrievalError::InvalidSource);
    }
    if metadata.is_file() {
        if metadata.len() > u64::try_from(limits.max_source_bytes).unwrap_or(u64::MAX) {
            return Err(RetrievalError::InvalidSource);
        }
        if path.extension().is_some_and(|extension| extension == "md")
            || path
                .extension()
                .is_some_and(|extension| extension == "toml")
        {
            files.push(path);
        }
        return Ok(());
    }
    if metadata.is_dir() {
        for entry in fs::read_dir(path)? {
            let entry = entry?;
            let child = relative.join(entry.file_name());
            collect_supported_files(root, &child, files, limits)?;
            if files.len() > limits.max_documents_per_profile {
                return Err(RetrievalError::InvalidSource);
            }
        }
    }
    Ok(())
}

fn classify_source(path: &str) -> Result<SearchSourceKind, RetrievalError> {
    let path = path.strip_prefix(".keith/").unwrap_or(path);
    if path == "MEMORY.md" {
        Ok(SearchSourceKind::DurableMemory)
    } else if path.starts_with("memory/daily/") {
        Ok(SearchSourceKind::DailyMemory)
    } else if path.starts_with("state/") {
        Ok(SearchSourceKind::CurrentState)
    } else if path.starts_with("knowledge/") {
        Ok(SearchSourceKind::Knowledge)
    } else if path.starts_with("summaries/") {
        Ok(SearchSourceKind::SessionSummary)
    } else if path.starts_with("skills/") {
        Ok(SearchSourceKind::Skill)
    } else {
        Err(RetrievalError::InvalidSource)
    }
}

fn digest_text(text: &str) -> String {
    Sha256::digest(text.as_bytes())
        .iter()
        .fold(String::with_capacity(64), |mut output, byte| {
            write!(output, "{byte:02x}").expect("writing to a String cannot fail");
            output
        })
}

fn stored_document(row: &rusqlite::Row<'_>) -> rusqlite::Result<SearchDocument> {
    let document_id = EntityId::from_str(&row.get::<_, String>(0)?).map_err(|_| {
        rusqlite::Error::FromSqlConversionFailure(
            0,
            rusqlite::types::Type::Text,
            Box::new(RetrievalError::InvalidIdentity),
        )
    })?;
    let profile_id = ProfileId::from_str(&row.get::<_, String>(1)?).map_err(|_| {
        rusqlite::Error::FromSqlConversionFailure(
            1,
            rusqlite::types::Type::Text,
            Box::new(RetrievalError::InvalidIdentity),
        )
    })?;
    let source_kind = parse_source_kind(&row.get::<_, String>(6)?).map_err(|error| {
        rusqlite::Error::FromSqlConversionFailure(6, rusqlite::types::Type::Text, Box::new(error))
    })?;
    let headings = serde_json::from_str(&row.get::<_, String>(4)?).map_err(|error| {
        rusqlite::Error::FromSqlConversionFailure(4, rusqlite::types::Type::Text, Box::new(error))
    })?;
    Ok(SearchDocument {
        document_id,
        profile_id,
        source_path: row.get(2)?,
        source_version: row.get(3)?,
        heading_path: headings,
        text: row.get(5)?,
        source_kind,
        modified_at: UtcTimestamp::from_unix_millis(row.get(7)?),
    })
}

const fn source_kind_name(kind: SearchSourceKind) -> &'static str {
    match kind {
        SearchSourceKind::DurableMemory => "durable_memory",
        SearchSourceKind::DailyMemory => "daily_memory",
        SearchSourceKind::CurrentState => "current_state",
        SearchSourceKind::Knowledge => "knowledge",
        SearchSourceKind::SessionSummary => "session_summary",
        SearchSourceKind::Skill => "skill",
    }
}

fn parse_source_kind(value: &str) -> Result<SearchSourceKind, RetrievalError> {
    match value {
        "durable_memory" => Ok(SearchSourceKind::DurableMemory),
        "daily_memory" => Ok(SearchSourceKind::DailyMemory),
        "current_state" => Ok(SearchSourceKind::CurrentState),
        "knowledge" => Ok(SearchSourceKind::Knowledge),
        "session_summary" => Ok(SearchSourceKind::SessionSummary),
        "skill" => Ok(SearchSourceKind::Skill),
        _ => Err(RetrievalError::InvalidSource),
    }
}

fn score_document(
    document: SearchDocument,
    query: &str,
    query_trigrams: &BTreeSet<String>,
    lexical: &BTreeMap<EntityId, f32>,
    vector: Option<&BTreeMap<EntityId, f32>>,
    weights: RankWeights,
    excerpt_chars: usize,
) -> Option<SearchResult> {
    let text = normalize(&document.text);
    let manual_lexical = lexical_overlap(query, &text);
    let lexical_score = lexical
        .get(&document.document_id)
        .copied()
        .unwrap_or(0.0)
        .max(manual_lexical);
    let trigram_score = jaccard(query_trigrams, &trigrams(&text));
    let vector_score = vector.and_then(|scores| scores.get(&document.document_id).copied());
    let (weighted, available) = vector_score.map_or_else(
        || {
            (
                lexical_score * weights.lexical + trigram_score * weights.trigram,
                weights.lexical + weights.trigram,
            )
        },
        |score| {
            (
                lexical_score * weights.lexical
                    + trigram_score * weights.trigram
                    + score * weights.vector,
                weights.lexical + weights.trigram + weights.vector,
            )
        },
    );
    let merged_score = if available > f32::EPSILON {
        weighted / available
    } else {
        0.0
    };
    (merged_score > 0.0).then(|| SearchResult {
        document_id: document.document_id,
        source_path: document.source_path,
        source_version: document.source_version,
        heading_path: document.heading_path,
        excerpt: excerpt(&document.text, query, excerpt_chars),
        source_kind: document.source_kind,
        modified_at: document.modified_at,
        lexical_score,
        trigram_score,
        vector_score,
        merged_score,
    })
}

fn normalize(text: &str) -> String {
    text.chars()
        .flat_map(char::to_lowercase)
        .map(|character| {
            if character.is_alphanumeric() || character == '_' || character == '-' {
                character
            } else {
                ' '
            }
        })
        .collect::<String>()
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
}

fn trigrams(text: &str) -> BTreeSet<String> {
    let characters = text.chars().collect::<Vec<_>>();
    if characters.is_empty() {
        return BTreeSet::new();
    }
    let width = characters.len().min(3);
    characters
        .windows(width)
        .map(|window| window.iter().collect())
        .collect()
}

fn lexical_overlap(query: &str, text: &str) -> f32 {
    if text.contains(query) {
        return 1.0;
    }
    let query_tokens = query.split_whitespace().collect::<BTreeSet<_>>();
    if query_tokens.is_empty() {
        return 0.0;
    }
    let text_tokens = text.split_whitespace().collect::<BTreeSet<_>>();
    let matches = query_tokens.intersection(&text_tokens).count();
    ratio(matches, query_tokens.len())
}

fn jaccard(left: &BTreeSet<String>, right: &BTreeSet<String>) -> f32 {
    if left.is_empty() || right.is_empty() {
        return 0.0;
    }
    let intersection = left.intersection(right).count();
    let union = left.union(right).count();
    ratio(intersection, union)
}

fn ratio(numerator: usize, denominator: usize) -> f32 {
    let numerator = u16::try_from(numerator.min(usize::from(u16::MAX))).unwrap_or(u16::MAX);
    let denominator = u16::try_from(denominator.min(usize::from(u16::MAX))).unwrap_or(u16::MAX);
    f32::from(numerator) / f32::from(denominator)
}

fn excerpt(text: &str, query: &str, max_chars: usize) -> String {
    let lower = text.to_lowercase();
    let query = query.to_lowercase();
    let byte_start = lower.find(&query).unwrap_or(0);
    let prefix_chars = text[..byte_start].chars().count();
    let start_chars = prefix_chars.saturating_sub(max_chars / 4);
    text.chars().skip(start_chars).take(max_chars).collect()
}

fn semantic_features(text: &str) -> BTreeSet<String> {
    let mut features = trigrams(text);
    features.extend(text.split_whitespace().map(|token| {
        let prefix = token.chars().take(5).collect::<String>();
        format!("word:{prefix}")
    }));
    features
}

fn normalize_vector(vector: &mut [f32]) {
    let norm = vector.iter().map(|value| value * value).sum::<f32>().sqrt();
    if norm > f32::EPSILON {
        for value in vector {
            *value /= norm;
        }
    }
}

fn cosine(left: &[f32], right: &[f32]) -> f32 {
    left.iter()
        .zip(right)
        .map(|(left, right)| left * right)
        .sum::<f32>()
        .clamp(-1.0, 1.0)
}

#[cfg(test)]
mod tests {
    use tempfile::tempdir;

    use super::*;

    #[derive(Clone)]
    struct CurrentAuthority {
        participants: BTreeSet<(ConversationId, ProfileId)>,
        grants: BTreeSet<(GrantId, EntityId, ProfileId, KnowledgeOperation)>,
        spaces: BTreeSet<(EntityId, ProfileId, KnowledgeOperation)>,
        grant_resource_policy_revision: Revision,
    }

    impl KnowledgeAccessResolver for CurrentAuthority {
        fn is_active_participant(
            &self,
            conversation_id: &ConversationId,
            requester: &ProfileId,
        ) -> Result<bool, RetrievalError> {
            Ok(self
                .participants
                .contains(&(conversation_id.clone(), requester.clone())))
        }

        fn authorize_grant(
            &self,
            grant_id: &GrantId,
            space_id: &EntityId,
            requester: &ProfileId,
            operation: KnowledgeOperation,
            now: UtcTimestamp,
        ) -> Result<Option<ResolvedGrantAuthorization>, RetrievalError> {
            Ok(self
                .grants
                .contains(&(
                    grant_id.clone(),
                    space_id.clone(),
                    requester.clone(),
                    operation,
                ))
                .then(|| ResolvedGrantAuthorization {
                    grant_id: grant_id.clone(),
                    grant_revision: Revision::new(4),
                    resource_policy_revision: self.grant_resource_policy_revision,
                    status: if now >= UtcTimestamp::from_unix_millis(100) {
                        GrantAuthorizationStatus::Expired
                    } else {
                        GrantAuthorizationStatus::Active
                    },
                }))
        }

        fn authorize_space(
            &self,
            space_id: &EntityId,
            _observed_permission_revision: Revision,
            requester: &ProfileId,
            operation: KnowledgeOperation,
            _now: UtcTimestamp,
        ) -> Result<Option<ResolvedSpaceAuthorization>, RetrievalError> {
            Ok(self
                .spaces
                .contains(&(space_id.clone(), requester.clone(), operation))
                .then_some(ResolvedSpaceAuthorization {
                    space_revision: Revision::new(2),
                    membership_permission_revision: Revision::new(2),
                }))
        }
    }

    fn service(root: &Path, vectors: bool) -> RetrievalService {
        let components = vectors.then(|| VectorComponents {
            embedder: Arc::new(LocalHashEmbedder::new(128).unwrap()),
            index: Arc::new(MemoryVectorIndex::default()),
        });
        RetrievalService::open(
            root,
            RetrievalLimits::default(),
            RankWeights::default(),
            components,
        )
        .unwrap()
    }

    fn source(path: &str, kind: SearchSourceKind, text: &str) -> SourceInput {
        SourceInput {
            source_path: path.into(),
            source_version: digest_text(text),
            source_kind: kind,
            modified_at: UtcTimestamp::UNIX_EPOCH,
            text: text.into(),
        }
    }

    #[test]
    fn multilingual_identifier_misspelling_and_vector_corpus_is_ranked_with_citations() {
        let directory = tempdir().unwrap();
        let profile = ProfileId::new();
        let retrieval = service(directory.path(), true);
        retrieval
            .index_sources(
                &profile,
                &[
                    source(
                        "MEMORY.md",
                        SearchSourceKind::DurableMemory,
                        "# Preferences\nUse release_calendar_v2 for every launch schedule.",
                    ),
                    source(
                        "knowledge/agent.md",
                        SearchSourceKind::Knowledge,
                        "# 系统\n机器学习代理使用混合检索。",
                    ),
                    source(
                        "state/config.md",
                        SearchSourceKind::CurrentState,
                        "The application configuration controls provider selection.",
                    ),
                ],
            )
            .unwrap();
        let identifier = retrieval
            .search(&profile, "release_calendar_v2", 5)
            .unwrap();
        assert_eq!(identifier[0].source_path, "MEMORY.md");
        assert_eq!(identifier[0].heading_path, vec!["Preferences"]);
        assert!(!identifier[0].source_version.is_empty());
        let chinese = retrieval.search(&profile, "机器学习", 5).unwrap();
        assert_eq!(chinese[0].source_path, "knowledge/agent.md");
        let misspelled = retrieval.search(&profile, "calender", 5).unwrap();
        assert_eq!(misspelled[0].source_path, "MEMORY.md");
        assert!(misspelled[0].trigram_score > 0.0);
        let semantic = retrieval
            .search(&profile, "configure providers", 5)
            .unwrap();
        assert_eq!(semantic[0].source_path, "state/config.md");
        assert!(semantic[0].vector_score.is_some());
    }

    #[test]
    fn deletion_profile_isolation_and_lexical_fallback_are_enforced() {
        let directory = tempdir().unwrap();
        let first = ProfileId::new();
        let second = ProfileId::new();
        let retrieval = service(directory.path(), false);
        retrieval
            .index_sources(
                &first,
                &[source(
                    "MEMORY.md",
                    SearchSourceKind::DurableMemory,
                    "private alpha launch",
                )],
            )
            .unwrap();
        retrieval
            .index_sources(
                &second,
                &[source(
                    "MEMORY.md",
                    SearchSourceKind::DurableMemory,
                    "private beta launch",
                )],
            )
            .unwrap();
        let results = retrieval.search(&first, "private launch", 10).unwrap();
        assert_eq!(results.len(), 1);
        assert!(results[0].excerpt.contains("alpha"));
        assert!(!retrieval.health_snapshot().unwrap().vector_available);
        retrieval.remove_source(&first, "MEMORY.md").unwrap();
        assert!(retrieval.search(&first, "alpha", 10).unwrap().is_empty());
        assert_eq!(retrieval.search(&second, "beta", 10).unwrap().len(), 1);
    }

    #[test]
    fn corrupt_index_is_quarantined_and_rebuilt_from_readable_sources() {
        let directory = tempdir().unwrap();
        let index_root = directory.path().join("index");
        let workspace = directory.path().join("workspace");
        fs::create_dir_all(workspace.join("knowledge")).unwrap();
        fs::write(
            workspace.join("knowledge/recovery.md"),
            "# Recovery\nReadable source survives derived-index loss.",
        )
        .unwrap();
        let profile = ProfileId::new();
        let initial = service(&index_root, false);
        initial
            .index_sources(
                &profile,
                &[source(
                    "knowledge/old.md",
                    SearchSourceKind::Knowledge,
                    "old derived content",
                )],
            )
            .unwrap();
        drop(initial);
        fs::write(index_root.join(INDEX_FILE), b"not a sqlite database").unwrap();
        let recovered = service(&index_root, false);
        let health = recovered.health_snapshot().unwrap();
        assert!(health.degraded);
        assert!(health.quarantined_index.unwrap().exists());
        recovered
            .rebuild_workspace(&profile, &workspace, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        assert!(!recovered.health_snapshot().unwrap().degraded);
        let results = recovered.search(&profile, "derived-index loss", 5).unwrap();
        assert_eq!(results[0].source_path, "knowledge/recovery.md");
    }

    #[test]
    fn hidden_profile_layout_is_indexed_with_truthful_source_paths() {
        let directory = tempdir().unwrap();
        let index_root = directory.path().join("index");
        let workspace = directory.path().join("workspace");
        fs::create_dir_all(workspace.join(".keith/memory/daily")).unwrap();
        fs::create_dir_all(workspace.join(".keith/skills/release")).unwrap();
        fs::write(
            workspace.join(".keith/MEMORY.md"),
            "# Preference\nUse the stable release channel.",
        )
        .unwrap();
        fs::write(
            workspace.join(".keith/memory/daily/2026-08-16.md"),
            "# Today\nVerified the packaged daemon.",
        )
        .unwrap();
        fs::write(
            workspace.join(".keith/skills/release/SKILL.md"),
            "# Release\nValidate artifacts before activation.",
        )
        .unwrap();

        let profile = ProfileId::new();
        let retrieval = service(&index_root, false);
        retrieval
            .rebuild_workspace(&profile, &workspace, UtcTimestamp::UNIX_EPOCH)
            .unwrap();

        let memory = retrieval.search(&profile, "stable release", 5).unwrap();
        assert_eq!(memory[0].source_path, ".keith/MEMORY.md");
        assert_eq!(memory[0].source_kind, SearchSourceKind::DurableMemory);
        let daily = retrieval.search(&profile, "packaged daemon", 5).unwrap();
        assert_eq!(daily[0].source_kind, SearchSourceKind::DailyMemory);
        let skill = retrieval
            .search(&profile, "artifacts activation", 5)
            .unwrap();
        assert_eq!(skill[0].source_kind, SearchSourceKind::Skill);
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn authorized_knowledge_is_query_time_isolated_revocable_and_restart_safe() {
        let directory = tempdir().unwrap();
        let owner = ProfileId::new();
        let member = ProfileId::new();
        let grantee = ProfileId::new();
        let stranger = ProfileId::new();
        let conversation = ConversationId::new();
        let grant_id = GrantId::new();
        let space_id = EntityId::new();
        let event = EventId::new();
        let retrieval = service(directory.path(), false);
        let entries = [
            AuthorizedSourceInput {
                source: source(
                    "knowledge/private.md",
                    SearchSourceKind::Knowledge,
                    "private-needle",
                ),
                provenance: KnowledgeSourceProvenance {
                    owner_profile_id: owner.clone(),
                    conversation_id: None,
                    source_event_ids: BTreeSet::new(),
                    visibility: KnowledgeVisibility::Private,
                    explicit_grantees: BTreeSet::new(),
                    grant_id: None,
                    space_id: None,
                    observed_permission_revision: None,
                    policy_revision: Revision::ZERO,
                    revoked: false,
                },
            },
            AuthorizedSourceInput {
                source: source(
                    "knowledge/group.md",
                    SearchSourceKind::Knowledge,
                    "group-needle",
                ),
                provenance: KnowledgeSourceProvenance {
                    owner_profile_id: owner.clone(),
                    conversation_id: Some(conversation.clone()),
                    source_event_ids: BTreeSet::from([event]),
                    visibility: KnowledgeVisibility::ConversationShared,
                    explicit_grantees: BTreeSet::new(),
                    grant_id: None,
                    space_id: None,
                    observed_permission_revision: None,
                    policy_revision: Revision::ZERO,
                    revoked: false,
                },
            },
            AuthorizedSourceInput {
                source: source(
                    "knowledge/grant.md",
                    SearchSourceKind::Knowledge,
                    "grant-needle",
                ),
                provenance: KnowledgeSourceProvenance {
                    owner_profile_id: owner.clone(),
                    conversation_id: Some(conversation.clone()),
                    source_event_ids: BTreeSet::new(),
                    visibility: KnowledgeVisibility::ExplicitlyShared,
                    explicit_grantees: BTreeSet::from([grantee.clone()]),
                    grant_id: Some(grant_id.clone()),
                    space_id: Some(space_id.clone()),
                    observed_permission_revision: Some(Revision::ZERO),
                    policy_revision: Revision::new(3),
                    revoked: false,
                },
            },
        ];
        retrieval.index_authorized_sources(&entries).unwrap();
        let auth = |requester, now, operation| AuthenticatedKnowledgeQuery {
            requester,
            space_id: space_id.clone(),
            operation,
            now,
            query: "grant-needle".into(),
            limit: 10,
        };
        let authority = CurrentAuthority {
            participants: BTreeSet::from([(conversation.clone(), member.clone())]),
            grants: BTreeSet::from([(
                grant_id.clone(),
                space_id.clone(),
                grantee.clone(),
                KnowledgeOperation::Search,
            )]),
            spaces: BTreeSet::from([(
                space_id.clone(),
                grantee.clone(),
                KnowledgeOperation::Search,
            )]),
            grant_resource_policy_revision: Revision::new(3),
        };
        for limit in [0, RetrievalLimits::default().max_scan_results + 1] {
            let mut bounded = auth(
                grantee.clone(),
                UtcTimestamp::UNIX_EPOCH,
                KnowledgeOperation::Search,
            );
            bounded.limit = limit;
            assert!(matches!(
                retrieval.search_authorized(&bounded, &authority),
                Err(RetrievalError::InvalidPolicy)
            ));
        }
        let mut wrong_space = auth(
            grantee.clone(),
            UtcTimestamp::UNIX_EPOCH,
            KnowledgeOperation::Search,
        );
        wrong_space.space_id = EntityId::new();
        assert!(
            retrieval
                .search_authorized(&wrong_space, &authority)
                .unwrap()
                .results
                .is_empty()
        );
        let result = retrieval
            .search_authorized(
                &auth(
                    grantee.clone(),
                    UtcTimestamp::UNIX_EPOCH,
                    KnowledgeOperation::Search,
                ),
                &authority,
            )
            .unwrap();
        assert_eq!(result.results.len(), 1);
        assert_eq!(result.results[0].provenance.space_id, space_id);
        assert_eq!(result.results[0].provenance.grant_id, grant_id);
        assert_eq!(
            result.results[0].provenance.space_revision,
            Revision::new(2)
        );
        assert_eq!(
            result.results[0].provenance.grant_revision,
            Revision::new(4)
        );
        let mut stale_policy = authority.clone();
        stale_policy.grant_resource_policy_revision = Revision::new(9);
        assert!(
            retrieval
                .search_authorized(
                    &auth(
                        grantee.clone(),
                        UtcTimestamp::UNIX_EPOCH,
                        KnowledgeOperation::Search,
                    ),
                    &stale_policy,
                )
                .unwrap()
                .results
                .is_empty()
        );
        for denied in [
            auth(
                grantee.clone(),
                UtcTimestamp::UNIX_EPOCH,
                KnowledgeOperation::Read,
            ),
            auth(
                grantee.clone(),
                UtcTimestamp::from_unix_millis(100),
                KnowledgeOperation::Search,
            ),
        ] {
            assert!(
                retrieval
                    .search_authorized(&denied, &authority)
                    .unwrap()
                    .results
                    .is_empty()
            );
        }
        assert!(
            retrieval
                .search_authorized(
                    &auth(
                        stranger,
                        UtcTimestamp::UNIX_EPOCH,
                        KnowledgeOperation::Search,
                    ),
                    &authority,
                )
                .unwrap()
                .results
                .is_empty()
        );
        retrieval
            .revoke_source_grant(&owner, "knowledge/grant.md", &grantee, Revision::new(3))
            .unwrap();
        drop(retrieval);
        let reopened = service(directory.path(), false);
        assert!(
            reopened
                .search_authorized(
                    &auth(
                        grantee,
                        UtcTimestamp::UNIX_EPOCH,
                        KnowledgeOperation::Search,
                    ),
                    &authority,
                )
                .unwrap()
                .results
                .is_empty()
        );
    }

    #[test]
    fn profile_index_deletion_rejects_stale_and_replays_after_restart() {
        let directory = tempdir().unwrap();
        let profile = ProfileId::new();
        let retrieval = service(directory.path(), false);
        retrieval
            .index_sources(
                &profile,
                &[source(
                    "knowledge/one.md",
                    SearchSourceKind::Knowledge,
                    "one",
                )],
            )
            .unwrap();
        let stale = retrieval.inventory_profile_deletion(&profile).unwrap();
        retrieval
            .index_sources(
                &profile,
                &[source(
                    "knowledge/two.md",
                    SearchSourceKind::Knowledge,
                    "two",
                )],
            )
            .unwrap();
        assert!(retrieval.erase_profile_inventory(&stale).is_err());
        let inventory = retrieval.inventory_profile_deletion(&profile).unwrap();
        let receipt = retrieval.erase_profile_inventory(&inventory).unwrap();
        assert_eq!(receipt.erased_stable_keys.len(), 2);
        drop(retrieval);
        let reopened = service(directory.path(), false);
        assert_eq!(
            reopened.erase_profile_inventory(&inventory).unwrap(),
            receipt
        );
        assert!(
            reopened
                .scan_profile_deletion_leaks(&profile)
                .unwrap()
                .leaked_private_keys
                .is_empty()
        );
    }
}
