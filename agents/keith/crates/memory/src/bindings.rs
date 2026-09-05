//! Exact source-attributed object bindings. Registry identity is not observed truth.
//! The vault owns associations; session-store owns required action dependencies.

use std::collections::{BTreeMap, BTreeSet};
use std::fmt;

use keith_agent_types::{BindingTargetKind, BindingTaskScope, EntityId, ObjectBindingKey,
    ObjectBindingReference, ProfileId, Revision, SchemaVersion, UtcTimestamp, WorkspaceId};
use keith_session_store::{RetentionClass, Sensitivity};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{AgentMemoryKind, EvidenceAuthority, EvidenceEffectiveInterval, EvidenceFacet,
    EvidenceFacetKind, EvidenceRecord, EvidenceSourceKind, EvidenceValidity, MemoryCorrectRequest,
    MemoryCreateRequest, MemoryError, MemoryService, ObservatoryError, ObservatoryMutation};
use crate::unified::{normalize_facets, revalidate_source, source_digest_for, strongest_sensitivity};

const VERSION: SchemaVersion = SchemaVersion::new(1, 0);
const MAX_VALUE: usize = 16 * 1024;
const MAX_ALIAS: usize = 256;
const MAX_REQUIRED: usize = 128;
const MAX_HISTORY: usize = 4096;
type EvidenceMap = BTreeMap<EntityId, EvidenceRecord>;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(tag = "mode", rename_all = "snake_case", deny_unknown_fields)]
pub enum BindingEntityTarget { Existing { entity_id: EntityId }, NewAlias { alias: String } }

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingSourceSpan { pub start: u32, pub end: u32 }

#[derive(Clone, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingDraft {
    pub entity: BindingEntityTarget,
    pub property: String,
    pub target_kind: BindingTargetKind,
    pub value_quote: String,
    #[serde(default)] pub value_span: Option<BindingSourceSpan>,
    #[serde(default)] pub effective: Option<EvidenceEffectiveInterval>,
}

#[derive(Clone, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingCorrectionDraft {
    pub value_quote: String,
    #[serde(default)] pub value_span: Option<BindingSourceSpan>,
    #[serde(default)] pub effective: Option<EvidenceEffectiveInterval>,
}

macro_rules! redacted_debug {
    ($($name:ty),+ $(,)?) => { $(impl fmt::Debug for $name {
        fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
            f.debug_struct(stringify!($name)).finish_non_exhaustive()
        }
    })+ };
}
redacted_debug!(BindingDraft, BindingCorrectionDraft, ResolvedBinding, BindingWriteReceipt, BindingRecord);

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingFreshness {
    #[serde(default)] pub max_age_ms: Option<u64>,
    #[serde(default)] pub observed_not_before: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingQuery {
    #[serde(default)] pub effective_at: Option<UtcTimestamp>,
    #[serde(default)] pub recorded_as_of: Option<u64>,
    #[serde(default)] pub freshness: BindingFreshness,
    pub max_sensitivity: Sensitivity,
}
impl Default for BindingQuery {
    fn default() -> Self { Self { effective_at: None, recorded_as_of: None,
        freshness: BindingFreshness::default(), max_sensitivity: Sensitivity::Personal } }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingUsePolicy {
    pub target_kind: BindingTargetKind,
    pub max_sensitivity: Sensitivity,
    #[serde(default)] pub freshness: BindingFreshness,
    pub allow_inferred_association: bool,
    pub allowed_source_authorities: Vec<EvidenceAuthority>,
}
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingLookupRequest { pub key: ObjectBindingKey, pub query: BindingQuery }

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BindingAssociationOrigin { Inferred, RuntimeDeclared }

#[derive(Clone, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResolvedBinding {
    pub reference: ObjectBindingReference,
    pub owner_memory_id: EntityId,
    pub owner_memory_digest: String,
    pub value: String,
    pub target_kind: BindingTargetKind,
    pub source_span: BindingSourceSpan,
    pub source_authority: EvidenceAuthority,
    pub association_origin: BindingAssociationOrigin,
    pub observed_at: UtcTimestamp,
    pub recorded_at: UtcTimestamp,
    pub effective: Option<EvidenceEffectiveInterval>,
    pub archive_revision: u64,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BindingResolutionReason {
    UnknownIdentity, MissingProperty, MissingSource, DeletedSource, MissingOwner, DeletedOwner,
    UnboundCorrection, MissingCorrectionLink, Cycle, Limit, AmbiguousAlias, ConflictingValues,
    EffectiveTimeUnknown, OutsideEffectiveInterval, TooOld, AssociationPolicy, SourceAuthorityPolicy,
    SensitivityPolicy, WrongTargetKind, ValueMismatch, Changed, DisputedSource,
}
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(tag = "status", rename_all = "snake_case", deny_unknown_fields)]
pub enum BindingResolution {
    Resolved { binding: ResolvedBinding },
    Missing { key: ObjectBindingKey, reason: BindingResolutionReason },
    Stale { key: ObjectBindingKey, reference: Option<ObjectBindingReference>, reason: BindingResolutionReason },
    Conflicting { key: ObjectBindingKey, candidates: Vec<ObjectBindingReference>, reason: BindingResolutionReason },
}
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RequiredBindingResolution {
    pub archive_revision: u64, pub bindings: Vec<BindingResolution>, pub complete: bool,
}
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingAliasCandidate {
    pub alias: String, pub key: ObjectBindingKey, pub target_kind: BindingTargetKind,
    pub association_origin: BindingAssociationOrigin,
}
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingAliasCandidates {
    pub archive_revision: u64, pub candidates: Vec<BindingAliasCandidate>,
    pub ambiguous_aliases: Vec<String>, pub truncated: bool,
}
#[derive(Clone, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingWriteReceipt {
    pub evidence: EvidenceRecord, pub binding: ObjectBindingReference,
    pub association_origin: BindingAssociationOrigin,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq, Error)]
pub enum BindingError {
    #[error("invalid or oversized binding contract")] Invalid,
    #[error("binding scope does not match its canonical owner")] Scope,
    #[error("binding cannot be used: {0:?}")] Unresolved(BindingResolutionReason),
}

/// An opaque mutation generated by canonical, source-checked binding methods.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BindingMutation(pub(crate) BindingRecord);

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct BindingEntity {
    pub id: EntityId, pub workspace_id: WorkspaceId, pub alias: String,
}
#[derive(Clone, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct BindingRecord {
    pub version: SchemaVersion,
    pub scope: BindingTaskScope,
    pub reference: ObjectBindingReference,
    pub owner_memory_id: EntityId,
    pub owner_memory_digest: String,
    pub source_span: BindingSourceSpan,
    pub target_kind: BindingTargetKind,
    pub association_origin: BindingAssociationOrigin,
    pub observed_at: UtcTimestamp,
    pub recorded_at: UtcTimestamp,
    pub effective: Option<EvidenceEffectiveInterval>,
    pub prior: Option<ObjectBindingReference>,
    pub entity: Option<BindingEntity>,
}
#[derive(Clone, Default)]
pub(crate) struct BindingIndex {
    pub entities: BTreeMap<EntityId, BindingEntity>,
    pub records: BTreeMap<EntityId, BindingRecord>,
    pub keys: BTreeMap<(WorkspaceId, ObjectBindingKey), Vec<EntityId>>,
}
pub(crate) struct BindingSnapshot {
    pub evidence: EvidenceMap,
    pub current: EvidenceMap,
    pub index: BindingIndex,
    pub revision: u64,
}

fn digest(value: &str) -> String { format!("{:x}", Sha256::digest(value.as_bytes())) }
fn interval_valid(interval: &Option<EvidenceEffectiveInterval>) -> bool {
    interval.as_ref().is_none_or(|v| v.from.zip(v.until).is_none_or(|(a,b)| a < b))
}
fn alias_valid(alias: &str) -> bool {
    !alias.trim().is_empty() && alias == alias.trim() && alias.len() <= MAX_ALIAS
        && !alias.chars().any(char::is_control)
}
fn sensitivity_allowed(actual: Sensitivity, allowed: Sensitivity) -> bool {
    strongest_sensitivity(actual, allowed) == allowed
}
fn scope_valid(profile: &ProfileId, scope: &BindingTaskScope) -> Result<(), BindingError> {
    if profile == &scope.profile_id { Ok(()) } else { Err(BindingError::Scope) }
}

impl BindingIndex {
    /// Validate at the event's original position, both before append and during replay.
    pub(crate) fn apply(&mut self, profile: &ProfileId, evidence: &EvidenceMap,
        record: &BindingRecord, sequence: u64) -> Result<(), ObservatoryError> {
        self.check(profile, evidence, record, sequence).map_err(|_| ObservatoryError::InvalidEvidence)?;
        if let Some(entity) = &record.entity { self.entities.insert(entity.id.clone(), entity.clone()); }
        self.keys.entry((record.scope.workspace_id.clone(), record.reference.key.clone()))
            .or_default().push(record.reference.binding_id.clone());
        self.records.insert(record.reference.binding_id.clone(), record.clone());
        Ok(())
    }

    fn check(&self, profile: &ProfileId, evidence: &EvidenceMap, record: &BindingRecord,
        sequence: u64) -> Result<(), BindingError> {
        scope_valid(profile, &record.scope)?;
        record.reference.validate().map_err(|_| BindingError::Invalid)?;
        if record.version != VERSION || record.reference.revision.get() != sequence
            || self.records.contains_key(&record.reference.binding_id)
            || !interval_valid(&record.effective) { return Err(BindingError::Invalid); }
        let entity = record.entity.as_ref().or_else(|| self.entities.get(&record.reference.key.entity_id))
            .ok_or(BindingError::Unresolved(BindingResolutionReason::UnknownIdentity))?;
        if entity.id != record.reference.key.entity_id || entity.workspace_id != record.scope.workspace_id
            || !alias_valid(&entity.alias)
            || (record.entity.is_some() && self.entities.contains_key(&entity.id)) {
            return Err(BindingError::Invalid);
        }
        let source = evidence.get(&record.reference.evidence_id).ok_or(BindingError::Invalid)?;
        let owner = evidence.get(&record.owner_memory_id).ok_or(BindingError::Invalid)?;
        if source.profile_id != *profile || owner.profile_id != *profile
            || source.content_digest != record.reference.evidence_digest
            || owner.content_digest != record.owner_memory_digest
            || source.occurred_at != record.observed_at
            || !matches!(source.validity, EvidenceValidity::Active | EvidenceValidity::Disputed)
            || owner.validity != EvidenceValidity::Active
            || owner.source_kind != EvidenceSourceKind::DurableMemory {
            return Err(BindingError::Invalid);
        }
        let value = span_value(source, record.source_span).ok_or(BindingError::Invalid)?;
        if value.is_empty() || value.len() > MAX_VALUE || digest(value) != record.reference.value_digest {
            return Err(BindingError::Invalid);
        }
        if let Some(prior) = &record.prior {
            let previous = self.records.get(&prior.binding_id).ok_or(BindingError::Invalid)?;
            if previous.reference != *prior || previous.reference.key != record.reference.key
                || previous.scope.workspace_id != record.scope.workspace_id
                || previous.target_kind != record.target_kind
                || self.records.values().any(|r| r.prior.as_ref() == Some(prior))
                || evidence.get(&previous.owner_memory_id).and_then(|e| e.superseded_by.as_ref()) != Some(&owner.id) {
                return Err(BindingError::Invalid);
            }
        }
        Ok(())
    }
}
fn span_value(source: &EvidenceRecord, span: BindingSourceSpan) -> Option<&str> {
    source.text.get(usize::try_from(span.start).ok()?..usize::try_from(span.end).ok()?)
}
