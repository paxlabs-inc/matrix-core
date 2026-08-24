#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};

use cap_fs_ext::{FollowSymlinks, OpenOptionsFollowExt};
use cap_std::ambient_authority;
use cap_std::fs::{Dir, OpenOptions};
use keith_agent_types::{
    ArtifactId, CURRENT_SCHEMA_VERSION, ChildId, ConversationId, EventId, GrantId, ProfileId,
    Revision, RootTreeId, SchemaVersion, SessionId, StableKey, UtcTimestamp,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

const TREES_DIRECTORY: &str = "trees";
const ARTIFACTS_DIRECTORY: &str = "artifacts";
const METADATA_FILE: &str = "metadata.json";
const CONTENT_FILE: &str = "content";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ArtifactLimits {
    pub max_artifact_bytes: usize,
    pub max_preview_bytes: usize,
    pub max_artifacts_per_tree: usize,
}

impl Default for ArtifactLimits {
    fn default() -> Self {
        Self {
            max_artifact_bytes: 256 * 1_024 * 1_024,
            max_preview_bytes: 4 * 1_024,
            max_artifacts_per_tree: 100_000,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ArtifactScope {
    pub root_tree_id: RootTreeId,
    pub session_id: SessionId,
    pub profile_id: ProfileId,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "source", content = "id")]
pub enum ArtifactSource {
    Tool,
    Kernel,
    Child(ChildId),
    User,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ArtifactState {
    Active,
    Archived,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "retention", content = "at")]
pub enum RetentionPolicy {
    Retain,
    DeleteAt(UtcTimestamp),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DisplayMetadata {
    pub name: Option<String>,
    pub description: Option<String>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "visibility", deny_unknown_fields)]
pub enum ArtifactAccessPolicy {
    #[default]
    Private,
    Revoked {
        revision: Revision,
    },
    Conversation {
        conversation_id: ConversationId,
        participants: BTreeSet<ProfileId>,
        revision: Revision,
    },
    ExplicitGrant {
        conversation_id: Option<ConversationId>,
        grant_id: GrantId,
        revision: Revision,
    },
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ArtifactProvenance {
    pub conversation_id: Option<ConversationId>,
    pub source_event_ids: BTreeSet<EventId>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ArtifactMetadata {
    pub version: SchemaVersion,
    pub id: ArtifactId,
    pub root_tree_id: RootTreeId,
    pub session_id: SessionId,
    pub profile_id: ProfileId,
    pub source: ArtifactSource,
    pub media_type: String,
    pub byte_length: u64,
    pub sha256: String,
    pub relative_path: PathBuf,
    pub created_at: UtcTimestamp,
    pub display: Option<DisplayMetadata>,
    pub state: ArtifactState,
    pub retention: RetentionPolicy,
    #[serde(default)]
    pub access: ArtifactAccessPolicy,
    #[serde(default)]
    pub provenance: ArtifactProvenance,
    #[serde(default)]
    pub promotion_receipts: BTreeMap<StableKey, ArtifactPromotionReceipt>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ArtifactReference {
    pub id: ArtifactId,
    pub root_tree_id: RootTreeId,
    pub profile_id: ProfileId,
}

impl From<&ArtifactMetadata> for ArtifactReference {
    fn from(metadata: &ArtifactMetadata) -> Self {
        Self {
            id: metadata.id.clone(),
            root_tree_id: metadata.root_tree_id.clone(),
            profile_id: metadata.profile_id.clone(),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NewArtifact<'a> {
    pub scope: ArtifactScope,
    pub source: ArtifactSource,
    pub media_type: &'a str,
    pub bytes: &'a [u8],
    pub created_at: UtcTimestamp,
    pub display: Option<DisplayMetadata>,
    pub retention: RetentionPolicy,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactExport {
    pub metadata: ArtifactMetadata,
    pub content: Vec<u8>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum DeletionClassification {
    DeletePrivate,
    RetainShared,
    RetainImmutableAudit,
    ExternalRemnant,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactDeletionRecord {
    pub stable_key: String,
    pub reference: ArtifactReference,
    pub metadata_digest: String,
    pub policy_revision: Revision,
    pub classification: DeletionClassification,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactDeletionInventory {
    pub profile_id: ProfileId,
    pub inventory_digest: String,
    pub records: Vec<ArtifactDeletionRecord>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactDeletionReceipt {
    pub profile_id: ProfileId,
    pub inventory_digest: String,
    pub erased_stable_keys: Vec<String>,
    pub retained: Vec<ArtifactDeletionRecord>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactLeakScan {
    pub profile_id: ProfileId,
    pub leaked_private_keys: Vec<String>,
    pub retained: Vec<ArtifactDeletionRecord>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactAuthorization {
    pub actor: ArtifactActor,
    pub operation: ArtifactOperation,
    pub now: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "actor", content = "profile_id")]
pub enum ArtifactActor {
    HumanOwner,
    Agent(ProfileId),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConversationArtifactPromotion {
    pub operation_key: StableKey,
    pub actor: ArtifactActor,
    pub conversation_id: ConversationId,
    pub expected_revision: Revision,
    pub source_event_ids: BTreeSet<EventId>,
    pub now: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ArtifactPromotionReceipt {
    pub operation_key: StableKey,
    pub actor: ArtifactActor,
    pub conversation_id: ConversationId,
    pub artifact_id: ArtifactId,
    pub artifact_sha256: String,
    pub expected_revision: Revision,
    pub result_revision: Revision,
    pub source_event_ids: BTreeSet<EventId>,
    pub promoted_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum ArtifactOperation {
    Inspect,
    Download,
    Append,
}

pub trait ArtifactAccessResolver {
    /// # Errors
    /// Returns an error when authoritative conversation state cannot be read safely.
    fn authorize_conversation_actor(
        &self,
        conversation_id: &ConversationId,
        actor: &ArtifactActor,
        operation: ArtifactOperation,
        now: UtcTimestamp,
    ) -> Result<bool, ArtifactError>;

    /// # Errors
    /// Returns an error when authoritative conversation membership cannot be read safely.
    fn conversation_participants(
        &self,
        conversation_id: &ConversationId,
    ) -> Result<BTreeSet<ProfileId>, ArtifactError>;

    /// # Errors
    /// Returns an error when current owner authority cannot be read safely.
    fn authorize_artifact_owner(
        &self,
        actor: &ArtifactActor,
        owner_profile_id: &ProfileId,
        operation: ArtifactOperation,
        now: UtcTimestamp,
    ) -> Result<bool, ArtifactError>;

    /// # Errors
    /// Returns an error when canonical event visibility cannot be read safely.
    fn authorize_source_events(
        &self,
        conversation_id: &ConversationId,
        actor: &ArtifactActor,
        source_event_ids: &BTreeSet<EventId>,
        now: UtcTimestamp,
    ) -> Result<bool, ArtifactError>;

    /// # Errors
    /// Returns an error when authoritative grant state cannot be read safely.
    fn authorize_grant(
        &self,
        grant_id: &GrantId,
        actor: &ArtifactActor,
        operation: ArtifactOperation,
        now: UtcTimestamp,
    ) -> Result<bool, ArtifactError>;
}

pub trait ConversationArtifactResolver {
    /// # Errors
    /// Returns an error unless the artifact is current, digest-bound, scoped to the conversation,
    /// provenance-bound to the supplied events, and append-authorized at `now`.
    fn validate_reference(
        &self,
        actor: &ArtifactActor,
        conversation_id: &ConversationId,
        artifact_id: &ArtifactId,
        expected_sha256: &str,
        source_event_ids: &[EventId],
        now: UtcTimestamp,
    ) -> Result<(), ArtifactError>;
}

pub struct ConversationArtifactVerifier<'a, R> {
    service: &'a ArtifactService,
    access: &'a R,
}

impl<'a, R> ConversationArtifactVerifier<'a, R> {
    pub const fn new(service: &'a ArtifactService, access: &'a R) -> Self {
        Self { service, access }
    }
}

impl<R: ArtifactAccessResolver> ConversationArtifactResolver
    for ConversationArtifactVerifier<'_, R>
{
    fn validate_reference(
        &self,
        actor: &ArtifactActor,
        conversation_id: &ConversationId,
        artifact_id: &ArtifactId,
        expected_sha256: &str,
        source_event_ids: &[EventId],
        now: UtcTimestamp,
    ) -> Result<(), ArtifactError> {
        if expected_sha256.len() != 64 || source_event_ids.len() > 64 {
            return Err(ArtifactError::Invalid);
        }
        let metadata = self.service.find_unique_metadata(artifact_id)?;
        let reference = ArtifactReference::from(&metadata);
        validate_metadata(&metadata, &reference)?;
        if metadata.sha256 != expected_sha256
            || metadata.provenance.conversation_id.as_ref() != Some(conversation_id)
            || metadata.provenance.source_event_ids
                != source_event_ids.iter().cloned().collect::<BTreeSet<_>>()
        {
            return Err(ArtifactError::AccessDenied);
        }
        let authorization = ArtifactAuthorization {
            actor: actor.clone(),
            operation: ArtifactOperation::Append,
            now,
        };
        if !artifact_authorized(&metadata, &authorization, self.access)? {
            return Err(ArtifactError::AccessDenied);
        }
        self.service.download(
            &ArtifactScope {
                root_tree_id: metadata.root_tree_id,
                session_id: metadata.session_id,
                profile_id: metadata.profile_id,
            },
            &reference,
        )?;
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChildDeliverable {
    pub child_id: ChildId,
    pub artifacts: Vec<ArtifactReference>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SpilledOutput {
    pub artifact_id: ArtifactId,
    pub path: PathBuf,
    pub bytes: usize,
    pub preview: String,
    pub media_type: String,
}

pub trait OutputSpill: Send + Sync {
    /// # Errors
    ///
    /// Returns an artifact error when output cannot be stored durably.
    fn spill(&self, bytes: &[u8]) -> Result<SpilledOutput, ArtifactError>;
}

#[derive(Debug, Error)]
pub enum ArtifactError {
    #[error("artifact I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("artifact metadata encoding failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("artifact is missing")]
    NotFound,
    #[error("artifact access crosses an owning tree or profile")]
    AccessDenied,
    #[error("artifact content exceeds its configured limit")]
    Oversized,
    #[error("artifact metadata, media type, or relative path is invalid")]
    Invalid,
    #[error("artifact content digest or length is corrupt")]
    Corrupt,
    #[error("artifact store lock was poisoned")]
    LockPoisoned,
    #[error("artifact tree reached its configured count limit")]
    CountLimit,
    #[error("artifact operation key conflicts with a different durable request")]
    OperationConflict,
}

pub struct ArtifactService {
    root: Dir,
    ambient_root: PathBuf,
    limits: ArtifactLimits,
    lock: Mutex<()>,
}

impl ArtifactService {
    /// Returns an exact, digest-bound inventory for profile deletion orchestration.
    ///
    /// # Errors
    /// Returns an error for corrupt metadata, duplicate identities, or unreadable storage.
    pub fn inventory_profile_deletion(
        &self,
        profile_id: &ProfileId,
    ) -> Result<ArtifactDeletionInventory, ArtifactError> {
        let mut records = self
            .all_metadata()?
            .into_iter()
            .filter(|metadata| &metadata.profile_id == profile_id)
            .map(|metadata| {
                let reference = ArtifactReference::from(&metadata);
                validate_metadata(&metadata, &reference)?;
                let metadata_digest = hex_digest(&serde_json::to_vec(&metadata)?);
                let policy_revision = access_revision(&metadata.access);
                let classification = match (&metadata.state, &metadata.access) {
                    (ArtifactState::Archived, _) => DeletionClassification::RetainImmutableAudit,
                    (
                        _,
                        ArtifactAccessPolicy::Conversation { .. }
                        | ArtifactAccessPolicy::ExplicitGrant { .. },
                    ) => DeletionClassification::RetainShared,
                    _ => DeletionClassification::DeletePrivate,
                };
                Ok(ArtifactDeletionRecord {
                    stable_key: format!(
                        "artifact:{profile_id}:{}:{}:{metadata_digest}",
                        metadata.id,
                        policy_revision.get()
                    ),
                    reference,
                    metadata_digest,
                    policy_revision,
                    classification,
                })
            })
            .collect::<Result<Vec<_>, ArtifactError>>()?;
        records.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        if records
            .windows(2)
            .any(|pair| pair[0].stable_key == pair[1].stable_key)
        {
            return Err(ArtifactError::Corrupt);
        }
        let inventory_digest = deletion_inventory_digest(&records);
        Ok(ArtifactDeletionInventory {
            profile_id: profile_id.clone(),
            inventory_digest,
            records,
        })
    }

    /// Erases only exact private records from a fresh inventory; shared and immutable records remain.
    ///
    /// # Errors
    /// Returns an error when any stable key/revision/digest is stale or storage mutation fails.
    pub fn erase_profile_inventory(
        &self,
        inventory: &ArtifactDeletionInventory,
    ) -> Result<ArtifactDeletionReceipt, ArtifactError> {
        let _guard = self.lock()?;
        let current = self.inventory_profile_deletion(&inventory.profile_id)?;
        if &current != inventory {
            let expected_retained = inventory
                .records
                .iter()
                .filter(|record| record.classification != DeletionClassification::DeletePrivate)
                .cloned()
                .collect::<Vec<_>>();
            if current.records == expected_retained {
                return Ok(ArtifactDeletionReceipt {
                    profile_id: inventory.profile_id.clone(),
                    inventory_digest: inventory.inventory_digest.clone(),
                    erased_stable_keys: inventory
                        .records
                        .iter()
                        .filter(|record| {
                            record.classification == DeletionClassification::DeletePrivate
                        })
                        .map(|record| record.stable_key.clone())
                        .collect(),
                    retained: expected_retained,
                });
            }
            return Err(ArtifactError::AccessDenied);
        }
        let mut erased = Vec::new();
        let mut retained = Vec::new();
        for record in &inventory.records {
            if record.classification != DeletionClassification::DeletePrivate {
                retained.push(record.clone());
                continue;
            }
            let directory =
                artifact_directory(&record.reference.root_tree_id, &record.reference.id);
            self.root.remove_file(directory.join(CONTENT_FILE))?;
            self.root.remove_file(directory.join(METADATA_FILE))?;
            self.root.remove_dir(&directory)?;
            erased.push(record.stable_key.clone());
        }
        Ok(ArtifactDeletionReceipt {
            profile_id: inventory.profile_id.clone(),
            inventory_digest: inventory.inventory_digest.clone(),
            erased_stable_keys: erased,
            retained,
        })
    }

    /// Re-enumerates remaining records, separating unexpected private leaks from retained remnants.
    ///
    /// # Errors
    /// Returns an error for corrupt or unreadable artifact state.
    pub fn scan_profile_deletion_leaks(
        &self,
        profile_id: &ProfileId,
    ) -> Result<ArtifactLeakScan, ArtifactError> {
        let inventory = self.inventory_profile_deletion(profile_id)?;
        let (leaks, retained): (Vec<_>, Vec<_>) = inventory
            .records
            .into_iter()
            .partition(|record| record.classification == DeletionClassification::DeletePrivate);
        Ok(ArtifactLeakScan {
            profile_id: profile_id.clone(),
            leaked_private_keys: leaks.into_iter().map(|record| record.stable_key).collect(),
            retained,
        })
    }
    /// # Errors
    ///
    /// Returns an error when the content root cannot be created, restricted, or opened.
    pub fn open(root: impl AsRef<Path>, limits: ArtifactLimits) -> Result<Self, ArtifactError> {
        std::fs::create_dir_all(root.as_ref())?;
        restrict_directory(root.as_ref())?;
        let ambient_root = std::fs::canonicalize(root.as_ref())?;
        let root = Dir::open_ambient_dir(&ambient_root, ambient_authority())?;
        root.create_dir_all(TREES_DIRECTORY)?;
        Ok(Self {
            root,
            ambient_root,
            limits,
            lock: Mutex::new(()),
        })
    }

    /// # Errors
    ///
    /// Returns an error when validation, count bounds, or atomic creation fails.
    pub fn create(&self, new: NewArtifact<'_>) -> Result<ArtifactMetadata, ArtifactError> {
        validate_media_type(new.media_type)?;
        validate_display(new.display.as_ref())?;
        if new.bytes.len() > self.limits.max_artifact_bytes {
            return Err(ArtifactError::Oversized);
        }
        let _guard = self.lock()?;
        let artifacts_directory = tree_artifacts_directory(&new.scope.root_tree_id);
        self.root.create_dir_all(&artifacts_directory)?;
        if self.root.read_dir(&artifacts_directory)?.count() >= self.limits.max_artifacts_per_tree {
            return Err(ArtifactError::CountLimit);
        }
        let id = ArtifactId::new();
        let directory = artifacts_directory.join(id.to_string());
        self.root.create_dir(&directory)?;
        let content_path = directory.join(CONTENT_FILE);
        let metadata = ArtifactMetadata {
            version: CURRENT_SCHEMA_VERSION,
            id: id.clone(),
            root_tree_id: new.scope.root_tree_id,
            session_id: new.scope.session_id,
            profile_id: new.scope.profile_id,
            source: new.source,
            media_type: new.media_type.to_owned(),
            byte_length: u64::try_from(new.bytes.len()).map_err(|_| ArtifactError::Oversized)?,
            sha256: hex_digest(new.bytes),
            relative_path: content_path.clone(),
            created_at: new.created_at,
            display: new.display,
            state: ArtifactState::Active,
            retention: new.retention,
            access: ArtifactAccessPolicy::Private,
            provenance: ArtifactProvenance::default(),
            promotion_receipts: BTreeMap::new(),
        };
        let result: Result<(), ArtifactError> = (|| {
            self.write_new(&content_path, new.bytes)?;
            self.atomic_metadata_write(&directory, &metadata)?;
            self.sync_directory(&directory)?;
            self.sync_directory(&artifacts_directory)?;
            Ok(())
        })();
        if result.is_err() {
            let _ = self.root.remove_file(&content_path);
            let _ = self.root.remove_file(directory.join(METADATA_FILE));
            let _ = self.root.remove_dir(&directory);
        }
        result?;
        Ok(metadata)
    }

    /// # Errors
    ///
    /// Returns an error for missing, corrupt, or cross-scope artifact metadata.
    pub fn inspect(
        &self,
        access: &ArtifactScope,
        reference: &ArtifactReference,
    ) -> Result<ArtifactMetadata, ArtifactError> {
        validate_reference_access(access, reference)?;
        let metadata = self.read_metadata(reference)?;
        validate_metadata(&metadata, reference)?;
        validate_metadata_access(access, &metadata)?;
        if !self.root.try_exists(&metadata.relative_path)? {
            return Err(ArtifactError::NotFound);
        }
        Ok(metadata)
    }

    /// Applies a revisioned conversation or explicit-grant policy without changing content bytes.
    ///
    /// # Errors
    /// Returns an error for non-owner access, stale policy revision, invalid provenance, or I/O.
    pub fn set_access_policy(
        &self,
        owner: &ArtifactScope,
        reference: &ArtifactReference,
        expected_revision: Revision,
        policy: ArtifactAccessPolicy,
        provenance: ArtifactProvenance,
    ) -> Result<ArtifactMetadata, ArtifactError> {
        if matches!(policy, ArtifactAccessPolicy::Conversation { .. }) {
            return Err(ArtifactError::Invalid);
        }
        let _guard = self.lock()?;
        validate_reference_access(owner, reference)?;
        let mut metadata = self.read_metadata(reference)?;
        validate_metadata(&metadata, reference)?;
        validate_metadata_access(owner, &metadata)?;
        let current = access_revision(&metadata.access);
        if current != expected_revision
            || access_revision(&policy)
                != expected_revision
                    .checked_next()
                    .ok_or(ArtifactError::Invalid)?
        {
            return Err(ArtifactError::AccessDenied);
        }
        validate_access_policy(&metadata.profile_id, &policy, &provenance)?;
        metadata.access = policy;
        metadata.provenance = provenance;
        let directory =
            tree_artifacts_directory(&metadata.root_tree_id).join(metadata.id.to_string());
        self.atomic_metadata_write(&directory, &metadata)?;
        self.sync_directory(&directory)?;
        Ok(metadata)
    }

    /// # Errors
    /// Returns an error for stale revision, absent provenance, or denied current authority.
    pub fn promote_to_conversation(
        &self,
        reference: &ArtifactReference,
        promotion: ConversationArtifactPromotion,
        resolver: &impl ArtifactAccessResolver,
    ) -> Result<ArtifactMetadata, ArtifactError> {
        if promotion.source_event_ids.is_empty() || promotion.source_event_ids.len() > 64 {
            return Err(ArtifactError::Invalid);
        }
        let _guard = self.lock()?;
        let mut metadata = self.read_metadata(reference)?;
        validate_metadata(&metadata, reference)?;
        let result_revision = promotion
            .expected_revision
            .checked_next()
            .ok_or(ArtifactError::Invalid)?;
        let receipt = ArtifactPromotionReceipt {
            operation_key: promotion.operation_key.clone(),
            actor: promotion.actor.clone(),
            conversation_id: promotion.conversation_id.clone(),
            artifact_id: metadata.id.clone(),
            artifact_sha256: metadata.sha256.clone(),
            expected_revision: promotion.expected_revision,
            result_revision,
            source_event_ids: promotion.source_event_ids.clone(),
            promoted_at: promotion.now,
        };
        if let Some(existing) = metadata.promotion_receipts.get(&promotion.operation_key) {
            return if existing == &receipt {
                Ok(metadata)
            } else {
                Err(ArtifactError::OperationConflict)
            };
        }
        if metadata.state != ArtifactState::Active
            || access_revision(&metadata.access) != promotion.expected_revision
        {
            return Err(ArtifactError::AccessDenied);
        }
        let owns_artifact = match &promotion.actor {
            ArtifactActor::Agent(profile_id) => profile_id == &metadata.profile_id,
            ArtifactActor::HumanOwner => resolver.authorize_artifact_owner(
                &promotion.actor,
                &metadata.profile_id,
                ArtifactOperation::Append,
                promotion.now,
            )?,
        };
        if !owns_artifact
            || !resolver.authorize_conversation_actor(
                &promotion.conversation_id,
                &promotion.actor,
                ArtifactOperation::Append,
                promotion.now,
            )?
        {
            return Err(ArtifactError::AccessDenied);
        }
        if !resolver.authorize_source_events(
            &promotion.conversation_id,
            &promotion.actor,
            &promotion.source_event_ids,
            promotion.now,
        )? {
            return Err(ArtifactError::AccessDenied);
        }
        let participants = resolver.conversation_participants(&promotion.conversation_id)?;
        let access = ArtifactAccessPolicy::Conversation {
            conversation_id: promotion.conversation_id.clone(),
            participants,
            revision: result_revision,
        };
        let provenance = ArtifactProvenance {
            conversation_id: Some(promotion.conversation_id),
            source_event_ids: promotion.source_event_ids,
        };
        validate_access_policy(&metadata.profile_id, &access, &provenance)?;
        metadata.access = access;
        metadata.provenance = provenance;
        metadata
            .promotion_receipts
            .insert(promotion.operation_key, receipt);
        let directory = artifact_directory(&metadata.root_tree_id, &metadata.id);
        self.atomic_metadata_write(&directory, &metadata)?;
        self.sync_directory(&directory)?;
        Ok(metadata)
    }

    /// Reads metadata only when current conversation membership or an explicit grant allows it.
    ///
    /// # Errors
    /// Returns an error for revoked/nonparticipant access or corrupt metadata.
    pub fn inspect_authorized(
        &self,
        authorization: &ArtifactAuthorization,
        resolver: &impl ArtifactAccessResolver,
        reference: &ArtifactReference,
    ) -> Result<ArtifactMetadata, ArtifactError> {
        let metadata = self.read_metadata(reference)?;
        validate_metadata(&metadata, reference)?;
        if !artifact_authorized(&metadata, authorization, resolver)? {
            return Err(ArtifactError::AccessDenied);
        }
        Ok(metadata)
    }

    /// Downloads content after query-time policy authorization and digest verification.
    ///
    /// # Errors
    /// Returns an error for denied access, bounds, missing bytes, or digest corruption.
    pub fn download_authorized(
        &self,
        authorization: &ArtifactAuthorization,
        resolver: &impl ArtifactAccessResolver,
        reference: &ArtifactReference,
    ) -> Result<Vec<u8>, ArtifactError> {
        if authorization.operation != ArtifactOperation::Download {
            return Err(ArtifactError::AccessDenied);
        }
        let metadata = self.inspect_authorized(authorization, resolver, reference)?;
        let content = self.read_bounded(&metadata.relative_path)?;
        if u64::try_from(content.len()).ok() != Some(metadata.byte_length)
            || hex_digest(&content) != metadata.sha256
        {
            return Err(ArtifactError::Corrupt);
        }
        Ok(content)
    }

    /// # Errors
    ///
    /// Returns an error for cross-scope access, missing data, oversize data, or digest corruption.
    pub fn download(
        &self,
        access: &ArtifactScope,
        reference: &ArtifactReference,
    ) -> Result<Vec<u8>, ArtifactError> {
        let metadata = self.inspect(access, reference)?;
        let content = self.read_bounded(&metadata.relative_path)?;
        if u64::try_from(content.len()).ok() != Some(metadata.byte_length)
            || hex_digest(&content) != metadata.sha256
        {
            return Err(ArtifactError::Corrupt);
        }
        Ok(content)
    }

    /// # Errors
    ///
    /// Returns an error when tree metadata cannot be read or an entry is cross-profile/corrupt.
    pub fn list(&self, access: &ArtifactScope) -> Result<Vec<ArtifactMetadata>, ArtifactError> {
        let directory = tree_artifacts_directory(&access.root_tree_id);
        let entries = match self.root.read_dir(&directory) {
            Ok(entries) => entries,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(error.into()),
        };
        let mut artifacts = Vec::new();
        for entry in entries {
            let entry = entry?;
            if entry.file_type()?.is_symlink() || !entry.file_type()?.is_dir() {
                return Err(ArtifactError::Corrupt);
            }
            let id = entry
                .file_name()
                .to_string_lossy()
                .parse::<ArtifactId>()
                .map_err(|_| ArtifactError::Corrupt)?;
            let reference = ArtifactReference {
                id,
                root_tree_id: access.root_tree_id.clone(),
                profile_id: access.profile_id.clone(),
            };
            artifacts.push(self.inspect(access, &reference)?);
        }
        artifacts.sort_by(|left, right| left.created_at.cmp(&right.created_at));
        Ok(artifacts)
    }

    /// # Errors
    ///
    /// Returns an error when access, content validation, or export assembly fails.
    pub fn export(
        &self,
        access: &ArtifactScope,
        reference: &ArtifactReference,
    ) -> Result<ArtifactExport, ArtifactError> {
        Ok(ArtifactExport {
            metadata: self.inspect(access, reference)?,
            content: self.download(access, reference)?,
        })
    }

    /// # Errors
    ///
    /// Returns an error when access fails or archived metadata cannot be persisted atomically.
    pub fn archive(
        &self,
        access: &ArtifactScope,
        reference: &ArtifactReference,
    ) -> Result<ArtifactMetadata, ArtifactError> {
        let _guard = self.lock()?;
        let mut metadata = self.inspect(access, reference)?;
        metadata.state = ArtifactState::Archived;
        let directory = artifact_directory(&reference.root_tree_id, &reference.id);
        self.atomic_metadata_write(&directory, &metadata)?;
        Ok(metadata)
    }

    /// # Errors
    ///
    /// Returns an error when access fails or known artifact files cannot be removed explicitly.
    pub fn delete(
        &self,
        access: &ArtifactScope,
        reference: &ArtifactReference,
    ) -> Result<(), ArtifactError> {
        let _guard = self.lock()?;
        let metadata = self.inspect(access, reference)?;
        let directory = artifact_directory(&reference.root_tree_id, &reference.id);
        self.root.remove_file(&metadata.relative_path)?;
        self.root.remove_file(directory.join(METADATA_FILE))?;
        self.root.remove_dir(&directory)?;
        self.sync_directory(&tree_artifacts_directory(&reference.root_tree_id))?;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when retention metadata cannot be scanned or expired content cannot delete.
    pub fn cleanup_expired(&self, now: UtcTimestamp) -> Result<Vec<ArtifactId>, ArtifactError> {
        let mut expired = Vec::new();
        for tree in self.root.read_dir(TREES_DIRECTORY)? {
            let tree = tree?;
            if tree.file_type()?.is_symlink() || !tree.file_type()?.is_dir() {
                return Err(ArtifactError::Corrupt);
            }
            let root_tree_id = tree
                .file_name()
                .to_string_lossy()
                .parse::<RootTreeId>()
                .map_err(|_| ArtifactError::Corrupt)?;
            let directory = tree_artifacts_directory(&root_tree_id);
            let artifacts = match self.root.read_dir(&directory) {
                Ok(artifacts) => artifacts,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
                Err(error) => return Err(error.into()),
            };
            for entry in artifacts {
                let entry = entry?;
                let id = entry
                    .file_name()
                    .to_string_lossy()
                    .parse::<ArtifactId>()
                    .map_err(|_| ArtifactError::Corrupt)?;
                let metadata = self.read_metadata(&ArtifactReference {
                    id: id.clone(),
                    root_tree_id: root_tree_id.clone(),
                    profile_id: ProfileId::new(),
                })?;
                if matches!(metadata.retention, RetentionPolicy::DeleteAt(at) if at <= now) {
                    let access = ArtifactScope {
                        root_tree_id: metadata.root_tree_id.clone(),
                        session_id: metadata.session_id.clone(),
                        profile_id: metadata.profile_id.clone(),
                    };
                    self.delete(&access, &ArtifactReference::from(&metadata))?;
                    expired.push(id);
                }
            }
        }
        Ok(expired)
    }

    /// # Errors
    ///
    /// Returns an error when any referenced artifact is cross-scope, missing, or not child-owned.
    pub fn child_deliverable(
        &self,
        access: &ArtifactScope,
        child_id: ChildId,
        references: Vec<ArtifactReference>,
    ) -> Result<ChildDeliverable, ArtifactError> {
        for reference in &references {
            let metadata = self.inspect(access, reference)?;
            if metadata.source != ArtifactSource::Child(child_id.clone()) {
                return Err(ArtifactError::AccessDenied);
            }
        }
        Ok(ChildDeliverable {
            child_id,
            artifacts: references,
        })
    }

    pub fn scoped_spill(
        self: &Arc<Self>,
        scope: ArtifactScope,
        source: ArtifactSource,
        media_type: impl Into<String>,
        retention: RetentionPolicy,
    ) -> ArtifactSpill {
        ArtifactSpill {
            service: Arc::clone(self),
            scope,
            source,
            media_type: media_type.into(),
            retention,
        }
    }

    fn read_metadata(
        &self,
        reference: &ArtifactReference,
    ) -> Result<ArtifactMetadata, ArtifactError> {
        let path = artifact_directory(&reference.root_tree_id, &reference.id).join(METADATA_FILE);
        let bytes = match self.read_bounded_with_limit(&path, 1024 * 1024) {
            Ok(bytes) => bytes,
            Err(ArtifactError::Io(error)) if error.kind() == std::io::ErrorKind::NotFound => {
                return Err(ArtifactError::NotFound);
            }
            Err(error) => return Err(error),
        };
        serde_json::from_slice(&bytes).map_err(|_| ArtifactError::Corrupt)
    }

    fn find_unique_metadata(
        &self,
        artifact_id: &ArtifactId,
    ) -> Result<ArtifactMetadata, ArtifactError> {
        let mut found = None;
        for tree in self.root.read_dir(TREES_DIRECTORY)? {
            let tree = tree?;
            if tree.file_type()?.is_symlink() || !tree.file_type()?.is_dir() {
                return Err(ArtifactError::Corrupt);
            }
            let root_tree_id = tree
                .file_name()
                .to_string_lossy()
                .parse::<RootTreeId>()
                .map_err(|_| ArtifactError::Corrupt)?;
            let path = artifact_directory(&root_tree_id, artifact_id).join(METADATA_FILE);
            let bytes = match self.read_bounded_with_limit(&path, 1024 * 1024) {
                Ok(bytes) => bytes,
                Err(ArtifactError::Io(error)) if error.kind() == std::io::ErrorKind::NotFound => {
                    continue;
                }
                Err(error) => return Err(error),
            };
            let metadata: ArtifactMetadata =
                serde_json::from_slice(&bytes).map_err(|_| ArtifactError::Corrupt)?;
            if found.replace(metadata).is_some() {
                return Err(ArtifactError::Corrupt);
            }
        }
        found.ok_or(ArtifactError::NotFound)
    }

    fn all_metadata(&self) -> Result<Vec<ArtifactMetadata>, ArtifactError> {
        let mut metadata = Vec::new();
        for tree in self.root.read_dir(TREES_DIRECTORY)? {
            let tree = tree?;
            if tree.file_type()?.is_symlink() || !tree.file_type()?.is_dir() {
                return Err(ArtifactError::Corrupt);
            }
            let root_tree_id = tree
                .file_name()
                .to_string_lossy()
                .parse::<RootTreeId>()
                .map_err(|_| ArtifactError::Corrupt)?;
            let directory = tree_artifacts_directory(&root_tree_id);
            let entries = match self.root.read_dir(&directory) {
                Ok(entries) => entries,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
                Err(error) => return Err(error.into()),
            };
            for entry in entries {
                let entry = entry?;
                if entry.file_type()?.is_symlink() || !entry.file_type()?.is_dir() {
                    return Err(ArtifactError::Corrupt);
                }
                let artifact_id = entry
                    .file_name()
                    .to_string_lossy()
                    .parse::<ArtifactId>()
                    .map_err(|_| ArtifactError::Corrupt)?;
                let path = artifact_directory(&root_tree_id, &artifact_id).join(METADATA_FILE);
                let bytes = self.read_bounded_with_limit(&path, 1024 * 1024)?;
                metadata.push(serde_json::from_slice(&bytes).map_err(|_| ArtifactError::Corrupt)?);
            }
        }
        Ok(metadata)
    }

    fn read_bounded(&self, path: &Path) -> Result<Vec<u8>, ArtifactError> {
        self.read_bounded_with_limit(path, self.limits.max_artifact_bytes)
    }

    fn read_bounded_with_limit(&self, path: &Path, limit: usize) -> Result<Vec<u8>, ArtifactError> {
        if self.root.symlink_metadata(path)?.file_type().is_symlink() {
            return Err(ArtifactError::Corrupt);
        }
        let mut options = OpenOptions::new();
        options.read(true).follow(FollowSymlinks::No);
        let file = self.root.open_with(path, &options)?;
        if usize::try_from(file.metadata()?.len()).map_or(true, |length| length > limit) {
            return Err(ArtifactError::Oversized);
        }
        let mut bytes = Vec::new();
        file.take(u64::try_from(limit).unwrap_or(u64::MAX).saturating_add(1))
            .read_to_end(&mut bytes)?;
        if bytes.len() > limit {
            return Err(ArtifactError::Oversized);
        }
        Ok(bytes)
    }

    fn write_new(&self, path: &Path, bytes: &[u8]) -> Result<(), ArtifactError> {
        let mut options = OpenOptions::new();
        options
            .write(true)
            .create_new(true)
            .follow(FollowSymlinks::No);
        configure_file_mode(&mut options);
        let mut file = self.root.open_with(path, &options)?;
        file.write_all(bytes)?;
        file.sync_all()?;
        Ok(())
    }

    fn atomic_metadata_write(
        &self,
        directory: &Path,
        metadata: &ArtifactMetadata,
    ) -> Result<(), ArtifactError> {
        let temporary = directory.join(format!(".{METADATA_FILE}.{}.tmp", ArtifactId::new()));
        let target = directory.join(METADATA_FILE);
        let bytes = serde_json::to_vec(metadata)?;
        let result = (|| {
            self.write_new(&temporary, &bytes)?;
            self.root.rename(&temporary, &self.root, &target)?;
            self.sync_directory(directory)?;
            Ok(())
        })();
        if result.is_err() {
            let _ = self.root.remove_file(&temporary);
        }
        result
    }

    fn sync_directory(&self, relative: &Path) -> Result<(), ArtifactError> {
        std::fs::File::open(self.ambient_root.join(relative))?.sync_all()?;
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, ArtifactError> {
        self.lock.lock().map_err(|_| ArtifactError::LockPoisoned)
    }
}

pub struct ArtifactSpill {
    service: Arc<ArtifactService>,
    scope: ArtifactScope,
    source: ArtifactSource,
    media_type: String,
    retention: RetentionPolicy,
}

impl OutputSpill for ArtifactSpill {
    fn spill(&self, bytes: &[u8]) -> Result<SpilledOutput, ArtifactError> {
        let media_type = if self.media_type == "auto" {
            detect_media_type(bytes)
        } else {
            self.media_type.clone()
        };
        let preview = preview(bytes, &media_type, self.service.limits.max_preview_bytes);
        let metadata = self.service.create(NewArtifact {
            scope: self.scope.clone(),
            source: self.source.clone(),
            media_type: &media_type,
            bytes,
            created_at: UtcTimestamp::now().map_err(|_| ArtifactError::Invalid)?,
            display: None,
            retention: self.retention,
        })?;
        Ok(SpilledOutput {
            artifact_id: metadata.id,
            path: self.service.ambient_root.join(&metadata.relative_path),
            bytes: bytes.len(),
            preview,
            media_type,
        })
    }
}

fn artifact_directory(root: &RootTreeId, id: &ArtifactId) -> PathBuf {
    tree_artifacts_directory(root).join(id.to_string())
}

fn tree_artifacts_directory(root: &RootTreeId) -> PathBuf {
    PathBuf::from(TREES_DIRECTORY)
        .join(root.to_string())
        .join(ARTIFACTS_DIRECTORY)
}

fn validate_reference_access(
    access: &ArtifactScope,
    reference: &ArtifactReference,
) -> Result<(), ArtifactError> {
    if access.root_tree_id != reference.root_tree_id || access.profile_id != reference.profile_id {
        return Err(ArtifactError::AccessDenied);
    }
    Ok(())
}

fn validate_metadata_access(
    access: &ArtifactScope,
    metadata: &ArtifactMetadata,
) -> Result<(), ArtifactError> {
    if access.root_tree_id != metadata.root_tree_id || access.profile_id != metadata.profile_id {
        return Err(ArtifactError::AccessDenied);
    }
    Ok(())
}

fn validate_metadata(
    metadata: &ArtifactMetadata,
    reference: &ArtifactReference,
) -> Result<(), ArtifactError> {
    if metadata.version.major != CURRENT_SCHEMA_VERSION.major
        || metadata.id != reference.id
        || metadata.root_tree_id != reference.root_tree_id
        || metadata.profile_id != reference.profile_id
        || metadata.relative_path
            != artifact_directory(&metadata.root_tree_id, &metadata.id).join(CONTENT_FILE)
    {
        return Err(ArtifactError::Corrupt);
    }
    validate_media_type(&metadata.media_type)?;
    validate_display(metadata.display.as_ref())?;
    validate_access_policy(&metadata.profile_id, &metadata.access, &metadata.provenance)?;
    for (key, receipt) in &metadata.promotion_receipts {
        if key != &receipt.operation_key
            || receipt.artifact_id != metadata.id
            || receipt.artifact_sha256 != metadata.sha256
            || receipt.source_event_ids.is_empty()
            || receipt.source_event_ids.len() > 64
            || receipt.expected_revision.checked_next() != Some(receipt.result_revision)
        {
            return Err(ArtifactError::Corrupt);
        }
    }
    Ok(())
}

fn access_revision(policy: &ArtifactAccessPolicy) -> Revision {
    match policy {
        ArtifactAccessPolicy::Private => Revision::ZERO,
        ArtifactAccessPolicy::Revoked { revision }
        | ArtifactAccessPolicy::Conversation { revision, .. }
        | ArtifactAccessPolicy::ExplicitGrant { revision, .. } => *revision,
    }
}

fn deletion_inventory_digest(records: &[ArtifactDeletionRecord]) -> String {
    let canonical = records
        .iter()
        .map(|record| record.stable_key.as_str())
        .collect::<Vec<_>>()
        .join("\n");
    hex_digest(canonical.as_bytes())
}

fn validate_access_policy(
    owner: &ProfileId,
    policy: &ArtifactAccessPolicy,
    provenance: &ArtifactProvenance,
) -> Result<(), ArtifactError> {
    if provenance.source_event_ids.len() > 64 {
        return Err(ArtifactError::Invalid);
    }
    match policy {
        ArtifactAccessPolicy::Private => {
            if provenance.conversation_id.is_some() || !provenance.source_event_ids.is_empty() {
                return Err(ArtifactError::Invalid);
            }
        }
        ArtifactAccessPolicy::Revoked { revision } => {
            if *revision == Revision::ZERO {
                return Err(ArtifactError::Invalid);
            }
        }
        ArtifactAccessPolicy::Conversation {
            conversation_id,
            participants,
            revision,
        } => {
            if participants.is_empty()
                || participants.len() > 256
                || !participants.contains(owner)
                || *revision == Revision::ZERO
                || provenance.conversation_id.as_ref() != Some(conversation_id)
            {
                return Err(ArtifactError::Invalid);
            }
        }
        ArtifactAccessPolicy::ExplicitGrant {
            conversation_id,
            revision,
            ..
        } => {
            if *revision == Revision::ZERO || provenance.conversation_id != *conversation_id {
                return Err(ArtifactError::Invalid);
            }
        }
    }
    Ok(())
}

fn artifact_authorized(
    metadata: &ArtifactMetadata,
    authorization: &ArtifactAuthorization,
    resolver: &impl ArtifactAccessResolver,
) -> Result<bool, ArtifactError> {
    match &metadata.access {
        ArtifactAccessPolicy::Private => match &authorization.actor {
            ArtifactActor::Agent(profile_id) => Ok(profile_id == &metadata.profile_id),
            ArtifactActor::HumanOwner => resolver.authorize_artifact_owner(
                &authorization.actor,
                &metadata.profile_id,
                authorization.operation,
                authorization.now,
            ),
        },
        ArtifactAccessPolicy::Revoked { .. } => Ok(false),
        ArtifactAccessPolicy::Conversation {
            conversation_id, ..
        } => resolver.authorize_conversation_actor(
            conversation_id,
            &authorization.actor,
            authorization.operation,
            authorization.now,
        ),
        ArtifactAccessPolicy::ExplicitGrant { grant_id, .. } => resolver.authorize_grant(
            grant_id,
            &authorization.actor,
            authorization.operation,
            authorization.now,
        ),
    }
}

fn validate_media_type(media_type: &str) -> Result<(), ArtifactError> {
    let valid = media_type.len() <= 255
        && media_type.split_once('/').is_some_and(|(kind, subtype)| {
            !kind.is_empty()
                && !subtype.is_empty()
                && kind.chars().chain(subtype.chars()).all(|character| {
                    character.is_ascii_alphanumeric() || matches!(character, '-' | '+' | '.')
                })
        });
    if valid {
        Ok(())
    } else {
        Err(ArtifactError::Invalid)
    }
}

fn validate_display(display: Option<&DisplayMetadata>) -> Result<(), ArtifactError> {
    if display.is_some_and(|display| {
        display.name.as_ref().is_some_and(|value| value.len() > 512)
            || display
                .description
                .as_ref()
                .is_some_and(|value| value.len() > 8 * 1_024)
    }) {
        return Err(ArtifactError::Invalid);
    }
    Ok(())
}

fn detect_media_type(bytes: &[u8]) -> String {
    if std::str::from_utf8(bytes).is_ok() {
        "text/plain".into()
    } else {
        "application/octet-stream".into()
    }
}

fn preview(bytes: &[u8], media_type: &str, limit: usize) -> String {
    if media_type.starts_with("text/")
        && let Ok(text) = std::str::from_utf8(bytes)
    {
        let mut preview = String::new();
        for character in text.chars() {
            if preview.len().saturating_add(character.len_utf8()) > limit {
                break;
            }
            preview.push(character);
        }
        return preview;
    }
    let byte_limit = limit / 2;
    hex_encode(&bytes[..bytes.len().min(byte_limit)])
}

fn hex_digest(bytes: &[u8]) -> String {
    hex_encode(&Sha256::digest(bytes))
}

fn hex_encode(bytes: &[u8]) -> String {
    let mut encoded = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        write!(&mut encoded, "{byte:02x}").expect("writing to a string cannot fail");
    }
    encoded
}

#[cfg(unix)]
fn restrict_directory(path: &Path) -> Result<(), ArtifactError> {
    use std::os::unix::fs::PermissionsExt;

    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))?;
    Ok(())
}

#[cfg(not(unix))]
fn restrict_directory(_path: &Path) -> Result<(), ArtifactError> {
    Ok(())
}

#[cfg(unix)]
fn configure_file_mode(options: &mut OpenOptions) {
    use cap_std::fs::OpenOptionsExt;

    options.mode(0o600);
}

#[cfg(not(unix))]
fn configure_file_mode(_options: &mut OpenOptions) {}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;
    use std::sync::Arc;
    use std::thread;

    use super::*;

    struct CurrentAuthority {
        participants: BTreeSet<(ConversationId, ProfileId)>,
        human_conversations: BTreeSet<ConversationId>,
        owner_profiles: BTreeSet<ProfileId>,
        visible_events: BTreeSet<(ConversationId, ArtifactActor, EventId)>,
        grants: BTreeMap<
            GrantId,
            (
                ArtifactActor,
                BTreeSet<ArtifactOperation>,
                UtcTimestamp,
                bool,
            ),
        >,
    }

    impl ArtifactAccessResolver for CurrentAuthority {
        fn authorize_conversation_actor(
            &self,
            conversation_id: &ConversationId,
            actor: &ArtifactActor,
            _operation: ArtifactOperation,
            _now: UtcTimestamp,
        ) -> Result<bool, ArtifactError> {
            Ok(match actor {
                ArtifactActor::HumanOwner => self.human_conversations.contains(conversation_id),
                ArtifactActor::Agent(profile_id) => self
                    .participants
                    .contains(&(conversation_id.clone(), profile_id.clone())),
            })
        }

        fn conversation_participants(
            &self,
            conversation_id: &ConversationId,
        ) -> Result<BTreeSet<ProfileId>, ArtifactError> {
            Ok(self
                .participants
                .iter()
                .filter(|(candidate, _)| candidate == conversation_id)
                .map(|(_, profile_id)| profile_id.clone())
                .collect())
        }

        fn authorize_artifact_owner(
            &self,
            actor: &ArtifactActor,
            owner_profile_id: &ProfileId,
            _operation: ArtifactOperation,
            _now: UtcTimestamp,
        ) -> Result<bool, ArtifactError> {
            Ok(actor == &ArtifactActor::HumanOwner
                && self.owner_profiles.contains(owner_profile_id))
        }

        fn authorize_source_events(
            &self,
            conversation_id: &ConversationId,
            actor: &ArtifactActor,
            source_event_ids: &BTreeSet<EventId>,
            _now: UtcTimestamp,
        ) -> Result<bool, ArtifactError> {
            Ok(source_event_ids.iter().all(|event_id| {
                self.visible_events.contains(&(
                    conversation_id.clone(),
                    actor.clone(),
                    event_id.clone(),
                ))
            }))
        }

        fn authorize_grant(
            &self,
            grant_id: &GrantId,
            actor: &ArtifactActor,
            operation: ArtifactOperation,
            now: UtcTimestamp,
        ) -> Result<bool, ArtifactError> {
            Ok(self.grants.get(grant_id).is_some_and(
                |(grantee, operations, expires_at, revoked)| {
                    grantee == actor
                        && operations.contains(&operation)
                        && now < *expires_at
                        && !revoked
                },
            ))
        }
    }

    fn scope() -> ArtifactScope {
        ArtifactScope {
            root_tree_id: RootTreeId::new(),
            session_id: SessionId::new(),
            profile_id: ProfileId::new(),
        }
    }

    fn create(
        service: &ArtifactService,
        scope: &ArtifactScope,
        bytes: &[u8],
        source: ArtifactSource,
        retention: RetentionPolicy,
    ) -> ArtifactMetadata {
        service
            .create(NewArtifact {
                scope: scope.clone(),
                source,
                media_type: "text/plain",
                bytes,
                created_at: UtcTimestamp::from_unix_millis(10),
                display: Some(DisplayMetadata {
                    name: Some("result.txt".into()),
                    description: Some("checked output".into()),
                }),
                retention,
            })
            .unwrap()
    }

    #[test]
    fn digest_validates_across_restart_and_corruption_is_detected() {
        let directory = tempfile::tempdir().unwrap();
        let access = scope();
        let metadata = {
            let service =
                ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
            create(
                &service,
                &access,
                b"durable content",
                ArtifactSource::User,
                RetentionPolicy::Retain,
            )
        };
        let service = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        let reference = ArtifactReference::from(&metadata);
        assert_eq!(
            service.download(&access, &reference).unwrap(),
            b"durable content"
        );
        assert_eq!(service.inspect(&access, &reference).unwrap(), metadata);
        std::fs::write(directory.path().join(&metadata.relative_path), b"corrupt").unwrap();
        assert!(matches!(
            service.download(&access, &reference),
            Err(ArtifactError::Corrupt)
        ));
    }

    #[test]
    fn cross_tree_and_cross_profile_access_are_denied_before_content_lookup() {
        let directory = tempfile::tempdir().unwrap();
        let service = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        let access = scope();
        let metadata = create(
            &service,
            &access,
            b"private",
            ArtifactSource::User,
            RetentionPolicy::Retain,
        );
        let reference = ArtifactReference::from(&metadata);
        let other_profile = ArtifactScope {
            profile_id: ProfileId::new(),
            ..access.clone()
        };
        assert!(matches!(
            service.inspect(&other_profile, &reference),
            Err(ArtifactError::AccessDenied)
        ));
        let other_tree = ArtifactScope {
            root_tree_id: RootTreeId::new(),
            ..access
        };
        assert!(matches!(
            service.download(&other_tree, &reference),
            Err(ArtifactError::AccessDenied)
        ));
    }

    #[test]
    fn concurrent_creation_produces_unique_complete_artifacts() {
        let directory = tempfile::tempdir().unwrap();
        let service =
            Arc::new(ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap());
        let access = scope();
        let handles = (0..16)
            .map(|index| {
                let service = Arc::clone(&service);
                let access = access.clone();
                thread::spawn(move || {
                    create(
                        &service,
                        &access,
                        format!("artifact-{index}").as_bytes(),
                        ArtifactSource::Tool,
                        RetentionPolicy::Retain,
                    )
                })
            })
            .collect::<Vec<_>>();
        let metadata = handles
            .into_iter()
            .map(|handle| handle.join().unwrap())
            .collect::<Vec<_>>();
        let ids = metadata
            .iter()
            .map(|artifact| artifact.id.clone())
            .collect::<std::collections::BTreeSet<_>>();
        assert_eq!(ids.len(), 16);
        assert_eq!(service.list(&access).unwrap().len(), 16);
        for artifact in metadata {
            service
                .download(&access, &ArtifactReference::from(&artifact))
                .unwrap();
        }
    }

    #[test]
    fn spill_has_bounded_preview_media_detection_and_oversize_rejection() {
        let directory = tempfile::tempdir().unwrap();
        let service = Arc::new(
            ArtifactService::open(
                directory.path(),
                ArtifactLimits {
                    max_artifact_bytes: 32,
                    max_preview_bytes: 8,
                    max_artifacts_per_tree: 10,
                },
            )
            .unwrap(),
        );
        let access = scope();
        let spill = service.scoped_spill(
            access.clone(),
            ArtifactSource::Kernel,
            "auto",
            RetentionPolicy::Retain,
        );
        let text = spill.spill(b"abcdefghijk").unwrap();
        assert_eq!(text.preview, "abcdefgh");
        assert_eq!(text.media_type, "text/plain");
        let binary = spill.spill(&[0xff, 0x00, 0x01, 0x02, 0x03]).unwrap();
        assert_eq!(binary.preview.len(), 8);
        assert_eq!(binary.media_type, "application/octet-stream");
        assert!(matches!(
            spill.spill(&[b'x'; 33]),
            Err(ArtifactError::Oversized)
        ));
        assert_eq!(service.list(&access).unwrap().len(), 2);
    }

    #[test]
    fn archive_export_child_refs_delete_retention_and_stale_refs_are_consistent() {
        let directory = tempfile::tempdir().unwrap();
        let service = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        let access = scope();
        let child_id = ChildId::new();
        let child = create(
            &service,
            &access,
            b"child result",
            ArtifactSource::Child(child_id.clone()),
            RetentionPolicy::Retain,
        );
        let child_ref = ArtifactReference::from(&child);
        let deliverable = service
            .child_deliverable(&access, child_id.clone(), vec![child_ref.clone()])
            .unwrap();
        assert_eq!(deliverable.child_id, child_id);
        assert_eq!(deliverable.artifacts, vec![child_ref.clone()]);
        let archived = service.archive(&access, &child_ref).unwrap();
        assert_eq!(archived.state, ArtifactState::Archived);
        let exported = service.export(&access, &child_ref).unwrap();
        assert_eq!(exported.content, b"child result");
        service.delete(&access, &child_ref).unwrap();
        assert!(matches!(
            service.inspect(&access, &child_ref),
            Err(ArtifactError::NotFound)
        ));

        let expired = create(
            &service,
            &access,
            b"expired",
            ArtifactSource::Tool,
            RetentionPolicy::DeleteAt(UtcTimestamp::from_unix_millis(50)),
        );
        assert!(
            service
                .cleanup_expired(UtcTimestamp::from_unix_millis(49))
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            service
                .cleanup_expired(UtcTimestamp::from_unix_millis(50))
                .unwrap(),
            vec![expired.id]
        );
    }

    #[test]
    fn metadata_path_tampering_and_tree_count_limits_are_rejected() {
        let directory = tempfile::tempdir().unwrap();
        let service = ArtifactService::open(
            directory.path(),
            ArtifactLimits {
                max_artifacts_per_tree: 1,
                ..ArtifactLimits::default()
            },
        )
        .unwrap();
        let access = scope();
        let metadata = create(
            &service,
            &access,
            b"first",
            ArtifactSource::User,
            RetentionPolicy::Retain,
        );
        assert!(matches!(
            service.create(NewArtifact {
                scope: access.clone(),
                source: ArtifactSource::User,
                media_type: "text/plain",
                bytes: b"second",
                created_at: UtcTimestamp::from_unix_millis(20),
                display: None,
                retention: RetentionPolicy::Retain,
            }),
            Err(ArtifactError::CountLimit)
        ));
        let metadata_path = directory
            .path()
            .join(artifact_directory(&metadata.root_tree_id, &metadata.id))
            .join(METADATA_FILE);
        let mut value: serde_json::Value =
            serde_json::from_slice(&std::fs::read(&metadata_path).unwrap()).unwrap();
        value["relative_path"] = serde_json::json!("../../outside");
        std::fs::write(metadata_path, serde_json::to_vec(&value).unwrap()).unwrap();
        assert!(matches!(
            service.inspect(&access, &ArtifactReference::from(&metadata)),
            Err(ArtifactError::Corrupt)
        ));
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn conversation_attachment_policy_is_bounded_revocable_and_restart_safe() {
        let directory = tempfile::tempdir().unwrap();
        let owner = scope();
        let member = ProfileId::new();
        let conversation_id = ConversationId::new();
        let metadata = {
            let service =
                ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
            let created = create(
                &service,
                &owner,
                b"shared attachment",
                ArtifactSource::User,
                RetentionPolicy::Retain,
            );
            let reference = ArtifactReference::from(&created);
            let source_event_id = EventId::new();
            service
                .promote_to_conversation(
                    &reference,
                    ConversationArtifactPromotion {
                        operation_key: StableKey::parse("promotion:agent:one").unwrap(),
                        actor: ArtifactActor::Agent(owner.profile_id.clone()),
                        conversation_id: conversation_id.clone(),
                        expected_revision: Revision::ZERO,
                        source_event_ids: BTreeSet::from([source_event_id.clone()]),
                        now: UtcTimestamp::from_unix_millis(11),
                    },
                    &CurrentAuthority {
                        participants: BTreeSet::from([
                            (conversation_id.clone(), owner.profile_id.clone()),
                            (conversation_id.clone(), member.clone()),
                        ]),
                        human_conversations: BTreeSet::new(),
                        owner_profiles: BTreeSet::new(),
                        visible_events: BTreeSet::from([(
                            conversation_id.clone(),
                            ArtifactActor::Agent(owner.profile_id.clone()),
                            source_event_id,
                        )]),
                        grants: BTreeMap::new(),
                    },
                )
                .unwrap()
        };
        let reference = ArtifactReference::from(&metadata);
        let service = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        let member_access = ArtifactAuthorization {
            actor: ArtifactActor::Agent(member.clone()),
            operation: ArtifactOperation::Download,
            now: UtcTimestamp::from_unix_millis(20),
        };
        let authority = CurrentAuthority {
            participants: BTreeSet::from([(conversation_id.clone(), member)]),
            human_conversations: BTreeSet::new(),
            owner_profiles: BTreeSet::new(),
            visible_events: metadata
                .provenance
                .source_event_ids
                .iter()
                .cloned()
                .map(|event_id| {
                    (
                        conversation_id.clone(),
                        member_access.actor.clone(),
                        event_id,
                    )
                })
                .collect(),
            grants: BTreeMap::new(),
        };
        assert_eq!(
            service
                .download_authorized(&member_access, &authority, &reference)
                .unwrap(),
            b"shared attachment"
        );
        let verifier = ConversationArtifactVerifier::new(&service, &authority);
        let source_events = metadata
            .provenance
            .source_event_ids
            .iter()
            .cloned()
            .collect::<Vec<_>>();
        verifier
            .validate_reference(
                &member_access.actor,
                &conversation_id,
                &metadata.id,
                &metadata.sha256,
                &source_events,
                member_access.now,
            )
            .unwrap();
        assert!(
            verifier
                .validate_reference(
                    &member_access.actor,
                    &conversation_id,
                    &metadata.id,
                    &"0".repeat(64),
                    &source_events,
                    member_access.now,
                )
                .is_err()
        );
        assert!(matches!(
            service.download_authorized(
                &member_access,
                &CurrentAuthority {
                    participants: BTreeSet::new(),
                    human_conversations: BTreeSet::new(),
                    owner_profiles: BTreeSet::new(),
                    visible_events: BTreeSet::new(),
                    grants: BTreeMap::new(),
                },
                &reference,
            ),
            Err(ArtifactError::AccessDenied)
        ));
        assert!(matches!(
            service.download_authorized(
                &ArtifactAuthorization {
                    actor: ArtifactActor::Agent(ProfileId::new()),
                    operation: ArtifactOperation::Download,
                    now: UtcTimestamp::from_unix_millis(20),
                },
                &authority,
                &reference,
            ),
            Err(ArtifactError::AccessDenied)
        ));
        service
            .set_access_policy(
                &owner,
                &reference,
                Revision::ZERO.checked_next().unwrap(),
                ArtifactAccessPolicy::Revoked {
                    revision: Revision::ZERO
                        .checked_next()
                        .unwrap()
                        .checked_next()
                        .unwrap(),
                },
                metadata.provenance,
            )
            .unwrap();
        assert!(matches!(
            service.download_authorized(&member_access, &authority, &reference),
            Err(ArtifactError::AccessDenied)
        ));

        let grant_id = GrantId::new();
        let revision_two = Revision::ZERO
            .checked_next()
            .unwrap()
            .checked_next()
            .unwrap();
        service
            .set_access_policy(
                &owner,
                &reference,
                revision_two,
                ArtifactAccessPolicy::ExplicitGrant {
                    conversation_id: None,
                    grant_id: grant_id.clone(),
                    revision: revision_two.checked_next().unwrap(),
                },
                ArtifactProvenance::default(),
            )
            .unwrap();
        let grantee = ProfileId::new();
        let grant_authority = CurrentAuthority {
            participants: BTreeSet::new(),
            human_conversations: BTreeSet::new(),
            owner_profiles: BTreeSet::new(),
            visible_events: BTreeSet::new(),
            grants: BTreeMap::from([(
                grant_id.clone(),
                (
                    ArtifactActor::Agent(grantee.clone()),
                    BTreeSet::from([ArtifactOperation::Download]),
                    UtcTimestamp::from_unix_millis(50),
                    false,
                ),
            )]),
        };
        let grant_access = ArtifactAuthorization {
            actor: ArtifactActor::Agent(grantee.clone()),
            operation: ArtifactOperation::Download,
            now: UtcTimestamp::from_unix_millis(40),
        };
        assert_eq!(
            service
                .download_authorized(&grant_access, &grant_authority, &reference)
                .unwrap(),
            b"shared attachment"
        );
        for denied in [
            ArtifactAuthorization {
                actor: ArtifactActor::Agent(grantee.clone()),
                operation: ArtifactOperation::Inspect,
                now: UtcTimestamp::from_unix_millis(40),
            },
            ArtifactAuthorization {
                actor: ArtifactActor::Agent(grantee),
                operation: ArtifactOperation::Download,
                now: UtcTimestamp::from_unix_millis(50),
            },
        ] {
            assert!(matches!(
                service.download_authorized(&denied, &grant_authority, &reference),
                Err(ArtifactError::AccessDenied)
            ));
        }
        let revoked_authority = CurrentAuthority {
            participants: BTreeSet::new(),
            human_conversations: BTreeSet::new(),
            owner_profiles: BTreeSet::new(),
            visible_events: BTreeSet::new(),
            grants: BTreeMap::from([(
                grant_id,
                (
                    grant_access.actor.clone(),
                    BTreeSet::from([ArtifactOperation::Download]),
                    UtcTimestamp::from_unix_millis(50),
                    true,
                ),
            )]),
        };
        assert!(matches!(
            service.download_authorized(&grant_access, &revoked_authority, &reference),
            Err(ArtifactError::AccessDenied)
        ));
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn conversation_promotion_requires_exact_authenticated_actor_and_provenance() {
        let directory = tempfile::tempdir().unwrap();
        let owner = scope();
        let conversation_id = ConversationId::new();
        let service = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        let created = create(
            &service,
            &owner,
            b"owner attachment",
            ArtifactSource::User,
            RetentionPolicy::Retain,
        );
        let reference = ArtifactReference::from(&created);
        let source_event_id = EventId::new();
        let human_authority = CurrentAuthority {
            participants: BTreeSet::from([(conversation_id.clone(), owner.profile_id.clone())]),
            human_conversations: BTreeSet::from([conversation_id.clone()]),
            owner_profiles: BTreeSet::from([owner.profile_id.clone()]),
            visible_events: BTreeSet::from([(
                conversation_id.clone(),
                ArtifactActor::HumanOwner,
                source_event_id.clone(),
            )]),
            grants: BTreeMap::new(),
        };
        let verifier = ConversationArtifactVerifier::new(&service, &human_authority);
        assert!(matches!(
            verifier.validate_reference(
                &ArtifactActor::HumanOwner,
                &conversation_id,
                &created.id,
                &created.sha256,
                std::slice::from_ref(&source_event_id),
                UtcTimestamp::from_unix_millis(11),
            ),
            Err(ArtifactError::AccessDenied)
        ));
        assert!(matches!(
            service.promote_to_conversation(
                &reference,
                ConversationArtifactPromotion {
                    operation_key: StableKey::parse("promotion:wrong-agent").unwrap(),
                    actor: ArtifactActor::Agent(ProfileId::new()),
                    conversation_id: conversation_id.clone(),
                    expected_revision: Revision::ZERO,
                    source_event_ids: BTreeSet::from([source_event_id.clone()]),
                    now: UtcTimestamp::from_unix_millis(11),
                },
                &human_authority,
            ),
            Err(ArtifactError::AccessDenied)
        ));
        assert!(matches!(
            service.promote_to_conversation(
                &reference,
                ConversationArtifactPromotion {
                    operation_key: StableKey::parse("promotion:empty-source").unwrap(),
                    actor: ArtifactActor::HumanOwner,
                    conversation_id: conversation_id.clone(),
                    expected_revision: Revision::ZERO,
                    source_event_ids: BTreeSet::new(),
                    now: UtcTimestamp::from_unix_millis(11),
                },
                &human_authority,
            ),
            Err(ArtifactError::Invalid)
        ));
        for (operation_key, visible_events) in [
            ("promotion:nonexistent", BTreeSet::new()),
            (
                "promotion:foreign",
                BTreeSet::from([(
                    ConversationId::new(),
                    ArtifactActor::HumanOwner,
                    source_event_id.clone(),
                )]),
            ),
            (
                "promotion:invisible",
                BTreeSet::from([(
                    conversation_id.clone(),
                    ArtifactActor::Agent(owner.profile_id.clone()),
                    source_event_id.clone(),
                )]),
            ),
        ] {
            let denied_authority = CurrentAuthority {
                participants: human_authority.participants.clone(),
                human_conversations: human_authority.human_conversations.clone(),
                owner_profiles: human_authority.owner_profiles.clone(),
                visible_events,
                grants: BTreeMap::new(),
            };
            assert!(matches!(
                service.promote_to_conversation(
                    &reference,
                    ConversationArtifactPromotion {
                        operation_key: StableKey::parse(operation_key).unwrap(),
                        actor: ArtifactActor::HumanOwner,
                        conversation_id: conversation_id.clone(),
                        expected_revision: Revision::ZERO,
                        source_event_ids: BTreeSet::from([source_event_id.clone()]),
                        now: UtcTimestamp::from_unix_millis(11),
                    },
                    &denied_authority,
                ),
                Err(ArtifactError::AccessDenied)
            ));
        }
        let promotion = ConversationArtifactPromotion {
            operation_key: StableKey::parse("promotion:human:one").unwrap(),
            actor: ArtifactActor::HumanOwner,
            conversation_id: conversation_id.clone(),
            expected_revision: Revision::ZERO,
            source_event_ids: BTreeSet::from([source_event_id.clone()]),
            now: UtcTimestamp::from_unix_millis(11),
        };
        let promoted = service
            .promote_to_conversation(&reference, promotion.clone(), &human_authority)
            .unwrap();
        assert_eq!(
            service
                .promote_to_conversation(&reference, promotion.clone(), &human_authority)
                .unwrap(),
            promoted
        );
        let mut conflict = promotion.clone();
        conflict.source_event_ids = BTreeSet::from([EventId::new()]);
        assert!(matches!(
            service.promote_to_conversation(&reference, conflict, &human_authority),
            Err(ArtifactError::OperationConflict)
        ));
        let mut different_key = promotion.clone();
        different_key.operation_key = StableKey::parse("promotion:human:two").unwrap();
        assert!(matches!(
            service.promote_to_conversation(&reference, different_key, &human_authority),
            Err(ArtifactError::AccessDenied)
        ));
        assert_eq!(
            promoted.provenance.source_event_ids,
            BTreeSet::from([source_event_id.clone()])
        );
        verifier
            .validate_reference(
                &ArtifactActor::HumanOwner,
                &conversation_id,
                &promoted.id,
                &promoted.sha256,
                std::slice::from_ref(&source_event_id),
                UtcTimestamp::from_unix_millis(12),
            )
            .unwrap();
        assert!(matches!(
            verifier.validate_reference(
                &ArtifactActor::Agent(ProfileId::new()),
                &conversation_id,
                &promoted.id,
                &promoted.sha256,
                std::slice::from_ref(&source_event_id),
                UtcTimestamp::from_unix_millis(12),
            ),
            Err(ArtifactError::AccessDenied)
        ));
        let revoked_human = CurrentAuthority {
            participants: human_authority.participants.clone(),
            human_conversations: BTreeSet::new(),
            owner_profiles: human_authority.owner_profiles.clone(),
            visible_events: human_authority.visible_events.clone(),
            grants: BTreeMap::new(),
        };
        assert!(matches!(
            ConversationArtifactVerifier::new(&service, &revoked_human).validate_reference(
                &ArtifactActor::HumanOwner,
                &conversation_id,
                &promoted.id,
                &promoted.sha256,
                std::slice::from_ref(&source_event_id),
                UtcTimestamp::from_unix_millis(13),
            ),
            Err(ArtifactError::AccessDenied)
        ));
        let reopened = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        assert_eq!(
            reopened
                .promote_to_conversation(&reference, promotion, &human_authority)
                .unwrap(),
            promoted
        );
    }

    #[test]
    fn profile_deletion_inventory_rejects_stale_and_replays_after_restart() {
        let directory = tempfile::tempdir().unwrap();
        let access = scope();
        let service = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        create(
            &service,
            &access,
            b"private deletion payload",
            ArtifactSource::User,
            RetentionPolicy::Retain,
        );
        let stale = service
            .inventory_profile_deletion(&access.profile_id)
            .unwrap();
        create(
            &service,
            &access,
            b"concurrent payload",
            ArtifactSource::User,
            RetentionPolicy::Retain,
        );
        assert!(service.erase_profile_inventory(&stale).is_err());
        let inventory = service
            .inventory_profile_deletion(&access.profile_id)
            .unwrap();
        let receipt = service.erase_profile_inventory(&inventory).unwrap();
        assert_eq!(receipt.erased_stable_keys.len(), 2);
        drop(service);
        let reopened = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        assert_eq!(
            reopened.erase_profile_inventory(&inventory).unwrap(),
            receipt
        );
        assert!(
            reopened
                .scan_profile_deletion_leaks(&access.profile_id)
                .unwrap()
                .leaked_private_keys
                .is_empty()
        );
    }
}
