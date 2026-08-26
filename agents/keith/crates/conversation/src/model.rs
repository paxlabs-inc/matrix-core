#![allow(clippy::missing_errors_doc)]

use keith_agent_types::{
    ArtifactId, AuditId, CURRENT_SCHEMA_VERSION, ConversationId, EventId, GrantId, ProfileId,
    Revision, SchemaVersion, StableKey, UtcTimestamp,
};
use serde::{Deserialize, Serialize};
use std::collections::{BTreeMap, BTreeSet};
use thiserror::Error;

pub const CONVERSATION_SCHEMA_VERSION: SchemaVersion = CURRENT_SCHEMA_VERSION;
pub const MAX_TITLE_BYTES: usize = 256;
pub const MAX_CONTENT_BYTES: usize = 1_048_576;
pub const MAX_KEY_BYTES: usize = 256;
pub const MAX_PROVENANCE_ITEMS: usize = 64;
pub const MAX_PARTICIPANTS: usize = 256;
pub const MAX_ARTIFACTS: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationKind {
    HumanAgentDm,
    AgentAgentDm,
    Group,
    Thread,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationLifecycle {
    Active,
    Archived,
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(
    tag = "kind",
    content = "id",
    rename_all = "snake_case",
    deny_unknown_fields
)]
pub enum Principal {
    Human,
    Agent(ProfileId),
    System,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EventHead {
    pub sequence: u64,
    pub event_id: EventId,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationProjection {
    pub conversation: ConversationRecord,
    pub participants: Vec<ConversationParticipant>,
    pub events: Vec<ConversationEvent>,
    pub read_through_sequence: u64,
    pub unread_count: u64,
    pub pinned: bool,
    pub hidden: bool,
    pub archived: bool,
    pub materialized: ConversationMaterializedState,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationMaterializedState {
    pub effective_content: BTreeMap<EventId, String>,
    pub redacted: BTreeSet<EventId>,
    pub reactions: BTreeMap<EventId, BTreeSet<String>>,
    pub pinned: BTreeSet<EventId>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationContextCursor {
    pub conversation_id: ConversationId,
    pub applied_through_sequence: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationContext {
    pub cursor: ConversationContextCursor,
    pub visible_events: Vec<ConversationEvent>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationAuthorizationObservation {
    pub conversation_id: ConversationId,
    pub principal: Principal,
    pub conversation_revision: Revision,
    pub participant_revision: Revision,
    pub relevant_grant_revisions: BTreeMap<GrantId, Revision>,
    pub grant_evidence: BTreeMap<GrantId, GrantAuthorizationEvidence>,
    pub policy_digest_sha256: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GrantAuthorizationEvidence {
    pub revision: Revision,
    pub resource_policy_revision: Revision,
    pub operations: BTreeSet<GrantOperation>,
    pub expires_at: Option<UtcTimestamp>,
    pub revoked_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationSearchHit {
    pub conversation_id: ConversationId,
    pub event_id: EventId,
    pub sequence: u64,
    pub author: Principal,
    pub timestamp: UtcTimestamp,
    pub content: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConversationSearch;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(tag = "operation", rename_all = "snake_case", deny_unknown_fields)]
pub enum MembershipEventPayload {
    Join {
        actor: Principal,
        participant: ParticipantPrincipal,
    },
    Leave {
        actor: Principal,
        participant: ParticipantPrincipal,
    },
    Rejoin {
        actor: Principal,
        participant: ParticipantPrincipal,
        expected_revision: Revision,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(tag = "operation", rename_all = "snake_case", deny_unknown_fields)]
pub enum TargetEventPayload {
    Edit {
        actor: Principal,
        target: EventId,
        replacement: String,
    },
    Redact {
        actor: Principal,
        target: EventId,
    },
    React {
        actor: Principal,
        target: EventId,
        reaction: String,
        remove: bool,
    },
    Pin {
        actor: Principal,
        target: EventId,
        pinned: bool,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationRecord {
    pub schema_version: SchemaVersion,
    pub id: ConversationId,
    pub kind: ConversationKind,
    pub lifecycle: ConversationLifecycle,
    pub title: String,
    pub creator: Principal,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub revision: Revision,
    pub participant_revision: Revision,
    pub participant_profiles: BTreeSet<ProfileId>,
    pub human_participant: bool,
    pub event_head: Option<EventHead>,
}

impl ConversationRecord {
    pub fn validate(&self) -> Result<(), DomainError> {
        validate_version(self.schema_version)?;
        validate_text("title", &self.title, MAX_TITLE_BYTES, true)?;
        if self.updated_at < self.created_at {
            return Err(DomainError::Invalid("updated timestamp precedes creation"));
        }
        if self.participant_profiles.len() > MAX_PARTICIPANTS {
            return Err(DomainError::BoundExceeded("participants"));
        }
        match self.kind {
            ConversationKind::HumanAgentDm
                if !self.human_participant || self.participant_profiles.len() != 1 =>
            {
                Err(DomainError::Invalid(
                    "human-agent DM requires one profile and the human",
                ))
            }
            ConversationKind::AgentAgentDm
                if self.human_participant || self.participant_profiles.len() != 2 =>
            {
                Err(DomainError::Invalid("agent-agent DM requires two profiles"))
            }
            ConversationKind::Thread
                if self.human_participant || self.participant_profiles.is_empty() =>
            {
                Err(DomainError::Invalid("thread requires at least one profile"))
            }
            _ => Ok(()),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationParticipant {
    pub schema_version: SchemaVersion,
    pub conversation_id: ConversationId,
    pub principal: ParticipantPrincipal,
    pub role: ParticipantRole,
    pub joined_at: UtcTimestamp,
    pub left_at: Option<UtcTimestamp>,
    pub revision: Revision,
    pub applied_through_sequence: u64,
    pub hidden: bool,
    pub muted: bool,
    pub notification_policy: NotificationPolicy,
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(
    tag = "kind",
    content = "id",
    rename_all = "snake_case",
    deny_unknown_fields
)]
pub enum ParticipantPrincipal {
    Human,
    Agent(ProfileId),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ParticipantRole {
    Owner,
    Member,
    Observer,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NotificationPolicy {
    pub mentions_only: bool,
    pub muted: bool,
}

impl ConversationParticipant {
    pub fn validate(&self) -> Result<(), DomainError> {
        validate_version(self.schema_version)?;
        if self.left_at.is_some_and(|left| left < self.joined_at) {
            return Err(DomainError::Invalid("participant left before joining"));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationEventKind {
    Message,
    Edit,
    Redaction,
    Reaction,
    MembershipChange,
    Pin,
    AssignmentChange,
    Handoff,
    RoutineResult,
    ComputerEvent,
    SystemNotice,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ArtifactReference {
    pub artifact_id: ArtifactId,
    pub digest_sha256: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EventProvenance {
    pub source: String,
    pub source_ids: Vec<String>,
    pub migration_version: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationEvent {
    pub schema_version: SchemaVersion,
    pub id: EventId,
    pub conversation_id: ConversationId,
    pub sequence: u64,
    pub publication_key: StableKey,
    pub author: Principal,
    pub timestamp: UtcTimestamp,
    pub kind: ConversationEventKind,
    pub content: Option<String>,
    pub artifacts: Vec<ArtifactReference>,
    pub reply_to: Option<EventId>,
    pub thread_parent: Option<EventId>,
    pub provenance: EventProvenance,
}

impl ConversationEvent {
    pub fn validate(&self) -> Result<(), DomainError> {
        validate_version(self.schema_version)?;
        if self.sequence == 0 {
            return Err(DomainError::Invalid("event sequence must start at one"));
        }
        if let Some(content) = &self.content {
            validate_message_content(content)?;
        }
        if self.artifacts.len() > MAX_ARTIFACTS {
            return Err(DomainError::BoundExceeded("event artifacts"));
        }
        if matches!(
            self.kind,
            ConversationEventKind::Edit
                | ConversationEventKind::Redaction
                | ConversationEventKind::Reaction
        ) && self.reply_to.is_none()
        {
            return Err(DomainError::Invalid(
                "edit, redaction, and reaction require a target event",
            ));
        }
        if self.kind == ConversationEventKind::MembershipChange {
            let content = self.content.as_deref().ok_or(DomainError::Invalid(
                "membership event requires typed payload",
            ))?;
            let payload: MembershipEventPayload = serde_json::from_str(content)
                .map_err(|_| DomainError::Invalid("membership payload is malformed"))?;
            let actor = match payload {
                MembershipEventPayload::Join { actor, .. }
                | MembershipEventPayload::Leave { actor, .. }
                | MembershipEventPayload::Rejoin { actor, .. } => actor,
            };
            if actor != self.author {
                return Err(DomainError::Invalid(
                    "membership actor differs from event author",
                ));
            }
        }
        if matches!(
            self.kind,
            ConversationEventKind::Edit
                | ConversationEventKind::Redaction
                | ConversationEventKind::Reaction
                | ConversationEventKind::Pin
        ) {
            let content = self
                .content
                .as_deref()
                .ok_or(DomainError::Invalid("target event requires typed payload"))?;
            let payload: TargetEventPayload = serde_json::from_str(content)
                .map_err(|_| DomainError::Invalid("target payload is malformed"))?;
            let actor = match &payload {
                TargetEventPayload::Edit { actor, .. }
                | TargetEventPayload::Redact { actor, .. }
                | TargetEventPayload::React { actor, .. }
                | TargetEventPayload::Pin { actor, .. } => actor,
            };
            if actor != &self.author {
                return Err(DomainError::Invalid(
                    "payload actor differs from event author",
                ));
            }
        }
        validate_text(
            "provenance source",
            &self.provenance.source,
            MAX_KEY_BYTES,
            false,
        )?;
        if self.provenance.source_ids.len() > MAX_PROVENANCE_ITEMS {
            return Err(DomainError::BoundExceeded("provenance source IDs"));
        }
        for source_id in &self.provenance.source_ids {
            validate_text("provenance source ID", source_id, MAX_KEY_BYTES, false)?;
        }
        if let Some(version) = &self.provenance.migration_version {
            validate_text("migration version", version, MAX_KEY_BYTES, false)?;
        }
        for artifact in &self.artifacts {
            if artifact.digest_sha256.len() != 64
                || !artifact
                    .digest_sha256
                    .bytes()
                    .all(|b| b.is_ascii_hexdigit())
            {
                return Err(DomainError::Invalid(
                    "artifact digest must be 64 hexadecimal characters",
                ));
            }
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReadReceipt {
    pub schema_version: SchemaVersion,
    pub conversation_id: ConversationId,
    pub reader: Principal,
    pub read_through_sequence: u64,
    pub updated_at: UtcTimestamp,
    pub revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SharedResourceKind {
    Artifact,
    File,
    KnowledgeSpace,
    Conversation,
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GrantOperation {
    Read,
    Search,
    Append,
    Export,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SharedKnowledgeGrant {
    pub schema_version: SchemaVersion,
    pub id: GrantId,
    pub resource_kind: SharedResourceKind,
    pub resource_id: String,
    pub grantor: Principal,
    pub grantee: ProfileId,
    pub purpose: String,
    pub provenance: GrantProvenance,
    pub resource_policy_revision: Revision,
    pub deletion_policy: SharedDeletionPolicy,
    pub operations: BTreeSet<GrantOperation>,
    pub created_at: UtcTimestamp,
    pub expires_at: Option<UtcTimestamp>,
    pub revoked_at: Option<UtcTimestamp>,
    pub revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GrantProvenance {
    pub source_actor: Principal,
    pub source_conversation_id: Option<ConversationId>,
    pub source_event_ids: Vec<EventId>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SharedDeletionPolicy {
    RetainUntilExplicitDelete,
    DeleteWhenSourceDeleted,
}

impl SharedKnowledgeGrant {
    pub fn validate(&self) -> Result<(), DomainError> {
        validate_version(self.schema_version)?;
        validate_text("resource_id", &self.resource_id, MAX_KEY_BYTES, false)?;
        validate_text("purpose", &self.purpose, 1024, false)?;
        if self.operations.is_empty() {
            return Err(DomainError::Invalid("grant requires an operation"));
        }
        if self.provenance.source_event_ids.len() > MAX_PROVENANCE_ITEMS {
            return Err(DomainError::BoundExceeded("grant provenance events"));
        }
        if self.expires_at.is_some_and(|time| time < self.created_at) {
            return Err(DomainError::Invalid("grant expires before creation"));
        }
        if self.revoked_at.is_some_and(|time| time < self.created_at) {
            return Err(DomainError::Invalid("grant revoked before creation"));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationAuditRecord {
    pub schema_version: SchemaVersion,
    pub id: AuditId,
    pub actor: Principal,
    pub action: String,
    pub conversation_id: Option<ConversationId>,
    pub event_id: Option<EventId>,
    pub correlation_key: String,
    pub occurred_at: UtcTimestamp,
    pub outcome: String,
}

impl ConversationAuditRecord {
    pub fn validate(&self) -> Result<(), DomainError> {
        validate_version(self.schema_version)?;
        validate_text("audit action", &self.action, MAX_KEY_BYTES, false)?;
        validate_text(
            "correlation_key",
            &self.correlation_key,
            MAX_KEY_BYTES,
            false,
        )?;
        validate_text("audit outcome", &self.outcome, 1024, false)
    }
}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum DomainError {
    #[error("unsupported schema version {0}")]
    UnsupportedVersion(u16),
    #[error("{0} exceeds its bound")]
    BoundExceeded(&'static str),
    #[error("invalid record: {0}")]
    Invalid(&'static str),
    #[error("malformed canonical JSON: {0}")]
    Malformed(String),
}

pub fn canonical_json<T: Serialize>(value: &T) -> Result<Vec<u8>, DomainError> {
    serde_json::to_vec(value).map_err(|error| DomainError::Malformed(error.to_string()))
}

pub trait ValidateRecord {
    fn validate_record(&self) -> Result<(), DomainError>;
}

impl ValidateRecord for ConversationRecord {
    fn validate_record(&self) -> Result<(), DomainError> {
        self.validate()
    }
}
impl ValidateRecord for ConversationParticipant {
    fn validate_record(&self) -> Result<(), DomainError> {
        self.validate()
    }
}
impl ValidateRecord for ConversationEvent {
    fn validate_record(&self) -> Result<(), DomainError> {
        self.validate()
    }
}
impl ValidateRecord for SharedKnowledgeGrant {
    fn validate_record(&self) -> Result<(), DomainError> {
        self.validate()
    }
}
impl ValidateRecord for ReadReceipt {
    fn validate_record(&self) -> Result<(), DomainError> {
        validate_version(self.schema_version)
    }
}
impl ValidateRecord for ConversationAuditRecord {
    fn validate_record(&self) -> Result<(), DomainError> {
        self.validate()
    }
}

pub fn decode_canonical<'de, T: Deserialize<'de> + Serialize + ValidateRecord>(
    bytes: &'de [u8],
) -> Result<T, DomainError> {
    let value: T =
        serde_json::from_slice(bytes).map_err(|error| DomainError::Malformed(error.to_string()))?;
    if canonical_json(&value)? != bytes {
        return Err(DomainError::Invalid("encoding is not canonical"));
    }
    value.validate_record()?;
    Ok(value)
}

fn validate_version(version: SchemaVersion) -> Result<(), DomainError> {
    if version == CONVERSATION_SCHEMA_VERSION {
        Ok(())
    } else {
        Err(DomainError::UnsupportedVersion(version.major))
    }
}

fn validate_text(
    field: &'static str,
    text: &str,
    max: usize,
    allow_empty: bool,
) -> Result<(), DomainError> {
    if !allow_empty && text.is_empty() {
        return Err(DomainError::Invalid("required text is empty"));
    }
    if text.len() > max {
        return Err(DomainError::BoundExceeded(field));
    }
    if text.chars().any(char::is_control) {
        return Err(DomainError::Invalid("text contains control characters"));
    }
    Ok(())
}

fn validate_message_content(text: &str) -> Result<(), DomainError> {
    if text.len() > MAX_CONTENT_BYTES {
        return Err(DomainError::BoundExceeded("content"));
    }
    if text
        .chars()
        .any(|character| character.is_control() && !matches!(character, '\n' | '\t'))
    {
        return Err(DomainError::Invalid("text contains control characters"));
    }
    Ok(())
}
