use std::collections::{BTreeMap, BTreeSet};

use keith_agent_types::{EntryId, ProfileId, SessionId, canonical_json_bytes};
use keith_session_store::{
    MemoryActivationCoverage, MemoryActivationEvidence, MemoryActivationKind,
    MemoryActivationManifest, Sensitivity,
};
use serde::Serialize;
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    AtlasSearchRequest, EvidenceFacetKind, EvidenceRecord, EvidenceSourceKind, EvidenceValidity,
    MemoryObservatory, ObservatoryError,
};

pub const ACTIVATION_SELECTOR_VERSION: &str = "reflex-memory-v1";
const MIN_RELEVANCE_SCORE: f32 = 0.12;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize)]
pub struct ActivationPolicy {
    pub max_total_tokens: u64,
    pub max_item_tokens: u64,
    pub max_items: usize,
    pub max_confirmed_anchors: usize,
    pub max_active_work: usize,
    pub max_corrections: usize,
    pub max_relevant_evidence: usize,
    pub search_pool: usize,
}

impl Default for ActivationPolicy {
    fn default() -> Self {
        Self {
            max_total_tokens: 1_600,
            max_item_tokens: 320,
            max_items: 12,
            max_confirmed_anchors: 3,
            max_active_work: 3,
            max_corrections: 3,
            max_relevant_evidence: 8,
            search_pool: 64,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct ActivationRequest {
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub query: String,
    pub max_sensitivity: Sensitivity,
    pub excluded_entries: BTreeSet<EntryId>,
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum ActivationError {
    #[error("memory activation policy or query is invalid")]
    Invalid,
    #[error("memory activation surface changed while selecting evidence")]
    Stale,
    #[error("memory activation manifest contains missing, changed, or out-of-scope evidence")]
    InvalidManifest,
    #[error("memory activation serialization failed: {0}")]
    Serialize(String),
    #[error("memory observatory failed: {0}")]
    Observatory(String),
}

impl From<ObservatoryError> for ActivationError {
    fn from(error: ObservatoryError) -> Self {
        Self::Observatory(error.to_string())
    }
}

struct Candidate {
    record: EvidenceRecord,
    kind: MemoryActivationKind,
    score: f32,
}

/// Selects a small immutable memory manifest from the current authoritative archive revision.
///
/// # Errors
///
/// Returns an error for invalid bounds or when the archive changes during selection.
#[allow(clippy::too_many_lines)]
pub fn select_activation(
    observatory: &MemoryObservatory,
    request: &ActivationRequest,
    policy: ActivationPolicy,
) -> Result<MemoryActivationManifest, ActivationError> {
    validate_policy(request, policy)?;
    let revision = observatory.revision()?;
    let (search, _) = observatory.search(&AtlasSearchRequest {
        query: request.query.clone(),
        limit: policy.search_pool,
        max_sensitivity: request.max_sensitivity,
        include_disputed: true,
    })?;
    let scores = search
        .iter()
        .map(|result| (result.evidence.id.clone(), result.merged_score))
        .collect::<BTreeMap<_, _>>();
    let matched = scores.len();
    let snapshot = observatory.evidence_snapshot()?;
    if observatory.revision()? != revision {
        return Err(ActivationError::Stale);
    }
    let mut excluded_current_thread = 0;
    let mut candidates = Vec::new();
    for record in snapshot.values() {
        if record.profile_id != request.profile_id
            || matches!(
                record.validity,
                EvidenceValidity::Deleted | EvidenceValidity::Superseded
            )
            || sensitivity_rank(record.sensitivity) > sensitivity_rank(request.max_sensitivity)
        {
            continue;
        }
        if record
            .source_entries
            .iter()
            .any(|entry| request.excluded_entries.contains(entry))
        {
            excluded_current_thread += 1;
            continue;
        }
        let score = scores.get(&record.id).copied().unwrap_or_default();
        let kind = classify(record, score);
        if let Some(kind) = kind {
            candidates.push(Candidate {
                record: record.clone(),
                kind,
                score,
            });
        }
    }
    candidates.sort_by(|left, right| {
        activation_priority(left.kind)
            .cmp(&activation_priority(right.kind))
            .then_with(|| right.score.total_cmp(&left.score))
            .then_with(|| right.record.occurred_at.cmp(&left.record.occurred_at))
            .then_with(|| left.record.id.cmp(&right.record.id))
    });
    let eligible = candidates.len();
    let mut counts = BTreeMap::new();
    let mut seen_sources = BTreeSet::new();
    let mut seen_digests = BTreeSet::new();
    let mut deduplicated = 0;
    let mut token_price = 0_u64;
    let mut evidence = Vec::new();
    let mut truncated = false;
    for candidate in candidates {
        if !seen_sources.insert(candidate.record.source_identity.clone())
            || !seen_digests.insert(candidate.record.content_digest.clone())
        {
            deduplicated += 1;
            continue;
        }
        let count = counts.entry(candidate.kind).or_insert(0_usize);
        if *count >= kind_limit(candidate.kind, policy) || evidence.len() >= policy.max_items {
            truncated = true;
            continue;
        }
        let text = truncate_to_tokens(&candidate.record.text, policy.max_item_tokens);
        let price = token_price_for(&text);
        if price == 0 || token_price.saturating_add(price) > policy.max_total_tokens {
            truncated = true;
            continue;
        }
        *count += 1;
        token_price = token_price.saturating_add(price);
        evidence.push(MemoryActivationEvidence {
            kind: candidate.kind,
            evidence_id: candidate.record.id,
            source_entries: candidate.record.source_entries,
            source_digests: candidate.record.source_digests,
            source_identity: candidate.record.source_identity,
            content_digest: candidate.record.content_digest,
            authority: enum_name(candidate.record.authority),
            validity: enum_name(candidate.record.validity),
            text,
            token_price: price,
        });
    }
    if observatory.revision()? != revision {
        return Err(ActivationError::Stale);
    }
    let query_identity = query_identity(request, policy, revision)?;
    let manifest_id = manifest_identity(&query_identity, &evidence)?;
    Ok(MemoryActivationManifest {
        manifest_id,
        selector_version: ACTIVATION_SELECTOR_VERSION.into(),
        query_identity,
        profile_id: request.profile_id.clone(),
        session_id: request.session_id.clone(),
        archive_revision: revision,
        coverage: MemoryActivationCoverage {
            examined: snapshot.len(),
            matched,
            eligible,
            selected: evidence.len(),
            excluded_current_thread,
            deduplicated,
            truncated,
        },
        evidence,
        token_price,
    })
}

/// Revalidates a frozen manifest before reuse on a later authoritative surface.
///
/// # Errors
///
/// Returns an error after revision, deletion, correction, sensitivity, digest, or scope changes.
pub fn validate_activation(
    observatory: &MemoryObservatory,
    manifest: &MemoryActivationManifest,
    max_sensitivity: Sensitivity,
) -> Result<(), ActivationError> {
    if manifest.selector_version != ACTIVATION_SELECTOR_VERSION
        || observatory.revision()? != manifest.archive_revision
    {
        return Err(ActivationError::Stale);
    }
    let snapshot = observatory.evidence_snapshot()?;
    for selected in &manifest.evidence {
        let record = snapshot
            .get(&selected.evidence_id)
            .ok_or(ActivationError::InvalidManifest)?;
        if record.profile_id != manifest.profile_id
            || !matches!(
                record.validity,
                EvidenceValidity::Active | EvidenceValidity::Disputed
            )
            || sensitivity_rank(record.sensitivity) > sensitivity_rank(max_sensitivity)
            || record.content_digest != selected.content_digest
            || record.source_identity != selected.source_identity
            || record.source_entries != selected.source_entries
            || record.source_digests != selected.source_digests
            || enum_name(record.validity) != selected.validity
        {
            return Err(ActivationError::InvalidManifest);
        }
    }
    Ok(())
}

fn validate_policy(
    request: &ActivationRequest,
    policy: ActivationPolicy,
) -> Result<(), ActivationError> {
    if request.query.trim().is_empty()
        || request.query.len() > 16 * 1_024
        || policy.max_total_tokens == 0
        || policy.max_item_tokens == 0
        || policy.max_items == 0
        || policy.search_pool == 0
        || policy.search_pool > 128
    {
        return Err(ActivationError::Invalid);
    }
    Ok(())
}

fn classify(record: &EvidenceRecord, score: f32) -> Option<MemoryActivationKind> {
    if record.validity == EvidenceValidity::Disputed || record.supersedes.is_some() {
        return (score >= MIN_RELEVANCE_SCORE || record.supersedes.is_some())
            .then_some(MemoryActivationKind::Correction);
    }
    let facet = |kind, values: &[&str]| {
        record
            .facets
            .iter()
            .any(|facet| facet.kind == kind && values.contains(&facet.value.as_str()))
    };
    if record.source_kind == EvidenceSourceKind::DurableMemory
        && facet(
            EvidenceFacetKind::Theme,
            &["preference", "personal_fact", "relationship", "routine"],
        )
    {
        return Some(MemoryActivationKind::ConfirmedAnchor);
    }
    if record.source_kind == EvidenceSourceKind::CurrentState
        || record.facets.iter().any(|facet| {
            matches!(
                facet.kind,
                EvidenceFacetKind::Goal | EvidenceFacetKind::Project | EvidenceFacetKind::Procedure
            )
        })
    {
        return Some(MemoryActivationKind::ActiveWork);
    }
    (score >= MIN_RELEVANCE_SCORE).then_some(MemoryActivationKind::RelevantEvidence)
}

const fn activation_priority(kind: MemoryActivationKind) -> u8 {
    match kind {
        MemoryActivationKind::Correction => 0,
        MemoryActivationKind::ConfirmedAnchor => 1,
        MemoryActivationKind::ActiveWork => 2,
        MemoryActivationKind::RelevantEvidence => 3,
    }
}

const fn kind_limit(kind: MemoryActivationKind, policy: ActivationPolicy) -> usize {
    match kind {
        MemoryActivationKind::ConfirmedAnchor => policy.max_confirmed_anchors,
        MemoryActivationKind::ActiveWork => policy.max_active_work,
        MemoryActivationKind::Correction => policy.max_corrections,
        MemoryActivationKind::RelevantEvidence => policy.max_relevant_evidence,
    }
}

fn query_identity(
    request: &ActivationRequest,
    policy: ActivationPolicy,
    revision: u64,
) -> Result<String, ActivationError> {
    #[derive(Serialize)]
    struct Identity<'a> {
        selector_version: &'static str,
        request: &'a ActivationRequest,
        policy: ActivationPolicy,
        revision: u64,
    }
    let bytes = canonical_json_bytes(&Identity {
        selector_version: ACTIVATION_SELECTOR_VERSION,
        request,
        policy,
        revision,
    })
    .map_err(|error| ActivationError::Serialize(error.to_string()))?;
    Ok(hex_digest(&bytes))
}

fn manifest_identity(
    query_identity: &str,
    evidence: &[MemoryActivationEvidence],
) -> Result<String, ActivationError> {
    let bytes = canonical_json_bytes(&(query_identity, evidence))
        .map_err(|error| ActivationError::Serialize(error.to_string()))?;
    Ok(hex_digest(&bytes))
}

fn truncate_to_tokens(text: &str, max_tokens: u64) -> String {
    let max_bytes = usize::try_from(max_tokens.saturating_mul(4)).unwrap_or(usize::MAX);
    let mut value = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if value.len() <= max_bytes {
        return value;
    }
    let mut boundary = max_bytes.min(value.len());
    while boundary > 0 && !value.is_char_boundary(boundary) {
        boundary -= 1;
    }
    value.truncate(boundary);
    value
}

fn token_price_for(text: &str) -> u64 {
    u64::try_from(text.len().saturating_add(3) / 4).unwrap_or(u64::MAX)
}

fn enum_name<T: std::fmt::Debug>(value: T) -> String {
    format!("{value:?}").to_ascii_lowercase()
}

const fn sensitivity_rank(value: Sensitivity) -> u8 {
    match value {
        Sensitivity::Public => 0,
        Sensitivity::Personal => 1,
        Sensitivity::Sensitive => 2,
        Sensitivity::Secret => 3,
    }
}

fn hex_digest(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    let mut value = String::with_capacity(digest.len() * 2);
    for byte in digest {
        use std::fmt::Write as _;
        let _ = write!(value, "{byte:02x}");
    }
    value
}

#[cfg(test)]
mod tests {
    use keith_agent_types::UtcTimestamp;
    use keith_session_store::RetentionClass;
    use tempfile::TempDir;

    use super::*;
    use crate::{EvidenceAuthority, EvidenceFacet, ObservatoryLimits, ObservatoryMutation};

    #[allow(clippy::too_many_arguments)]
    fn record(
        profile: &ProfileId,
        session: &SessionId,
        entry: EntryId,
        identity: &str,
        text: &str,
        source_kind: EvidenceSourceKind,
        facets: Vec<EvidenceFacet>,
        occurred_at: i64,
    ) -> EvidenceRecord {
        EvidenceRecord::new(
            profile.clone(),
            session.clone(),
            vec![entry],
            vec![format!("source-{identity}")],
            identity.into(),
            None,
            source_kind,
            EvidenceAuthority::UserAsserted,
            text.into(),
            UtcTimestamp::from_unix_millis(occurred_at),
            Sensitivity::Personal,
            RetentionClass::Durable,
            facets,
        )
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn activation_is_relevant_correction_aware_deduplicated_and_stale_safe() {
        let root = TempDir::new().unwrap();
        let profile = ProfileId::new();
        let session = SessionId::new();
        let other_session = SessionId::new();
        let current_entry = EntryId::new();
        let observatory = MemoryObservatory::open(
            root.path(),
            &profile,
            ObservatoryLimits::default(),
            UtcTimestamp::UNIX_EPOCH,
        )
        .unwrap();
        let anchor = record(
            &profile,
            &other_session,
            EntryId::new(),
            "anchor",
            "The user's name is Alex",
            EvidenceSourceKind::DurableMemory,
            vec![EvidenceFacet {
                kind: EvidenceFacetKind::Theme,
                value: "personal_fact".into(),
            }],
            1,
        );
        let active_work = record(
            &profile,
            &other_session,
            EntryId::new(),
            "active-project",
            "The active project is Keith memory",
            EvidenceSourceKind::CurrentState,
            vec![EvidenceFacet {
                kind: EvidenceFacetKind::Project,
                value: "keith".into(),
            }],
            2,
        );
        let relevant = record(
            &profile,
            &other_session,
            EntryId::new(),
            "routing",
            "The routing problem was solved with a durable boundary",
            EvidenceSourceKind::AssistantFinal,
            Vec::new(),
            3,
        );
        let duplicate = record(
            &profile,
            &other_session,
            EntryId::new(),
            "routing-duplicate",
            "The routing problem was solved with a durable boundary",
            EvidenceSourceKind::AssistantMessage,
            Vec::new(),
            4,
        );
        let current = record(
            &profile,
            &session,
            current_entry.clone(),
            "current-thread",
            "The current routing prompt must not be duplicated",
            EvidenceSourceKind::UserMessage,
            Vec::new(),
            5,
        );
        let unrelated = record(
            &profile,
            &other_session,
            EntryId::new(),
            "gardening",
            "Tomatoes need a sunny garden",
            EvidenceSourceKind::AssistantMessage,
            Vec::new(),
            6,
        );
        let prior = record(
            &profile,
            &other_session,
            EntryId::new(),
            "prior-preference",
            "The user prefers verbose routing explanations",
            EvidenceSourceKind::DurableMemory,
            vec![EvidenceFacet {
                kind: EvidenceFacetKind::Theme,
                value: "preference".into(),
            }],
            7,
        );
        let prior_id = prior.id.clone();
        let replacement = record(
            &profile,
            &other_session,
            EntryId::new(),
            "corrected-preference",
            "The user prefers concise routing explanations",
            EvidenceSourceKind::DurableMemory,
            vec![EvidenceFacet {
                kind: EvidenceFacetKind::Theme,
                value: "preference".into(),
            }],
            8,
        );
        let deleted = record(
            &profile,
            &other_session,
            EntryId::new(),
            "deleted-routing",
            "Deleted routing secret",
            EvidenceSourceKind::UserMessage,
            Vec::new(),
            9,
        );
        let deleted_id = deleted.id.clone();
        observatory
            .apply(
                vec![
                    ObservatoryMutation::Observe(anchor),
                    ObservatoryMutation::Observe(active_work),
                    ObservatoryMutation::Observe(relevant),
                    ObservatoryMutation::Observe(duplicate),
                    ObservatoryMutation::Observe(current),
                    ObservatoryMutation::Observe(unrelated),
                    ObservatoryMutation::Observe(prior),
                    ObservatoryMutation::Supersede {
                        prior_id,
                        replacement,
                    },
                    ObservatoryMutation::Observe(deleted),
                    ObservatoryMutation::Delete {
                        evidence_id: deleted_id.clone(),
                        source_entries: Vec::new(),
                        source_digests: Vec::new(),
                    },
                ],
                UtcTimestamp::from_unix_millis(10),
            )
            .unwrap();
        let request = ActivationRequest {
            profile_id: profile.clone(),
            session_id: session.clone(),
            query: "routing architecture".into(),
            max_sensitivity: Sensitivity::Personal,
            excluded_entries: BTreeSet::from([current_entry]),
        };
        let manifest =
            select_activation(&observatory, &request, ActivationPolicy::default()).unwrap();
        validate_activation(&observatory, &manifest, Sensitivity::Personal).unwrap();
        assert!(manifest.evidence.iter().any(|item| {
            item.kind == MemoryActivationKind::ConfirmedAnchor && item.text.contains("name is Alex")
        }));
        assert!(manifest.evidence.iter().any(|item| {
            item.kind == MemoryActivationKind::Correction && item.text.contains("concise")
        }));
        assert!(manifest.evidence.iter().any(|item| {
            item.kind == MemoryActivationKind::RelevantEvidence
                && item.text.contains("durable boundary")
        }));
        assert!(
            !manifest
                .evidence
                .iter()
                .any(|item| item.text.contains("current routing prompt"))
        );
        assert!(
            !manifest
                .evidence
                .iter()
                .any(|item| item.text.contains("Deleted routing secret"))
        );
        assert_eq!(
            manifest
                .evidence
                .iter()
                .filter(|item| item.text.contains("durable boundary"))
                .count(),
            1
        );
        assert!(manifest.coverage.excluded_current_thread > 0);
        assert!(manifest.coverage.deduplicated > 0);

        let gardening = select_activation(
            &observatory,
            &ActivationRequest {
                query: "sunny garden tomatoes".into(),
                ..request.clone()
            },
            ActivationPolicy::default(),
        )
        .unwrap();
        assert!(
            gardening
                .evidence
                .iter()
                .any(|item| item.text.contains("sunny garden"))
        );
        assert!(
            !manifest
                .evidence
                .iter()
                .any(|item| item.text.contains("sunny garden"))
        );

        observatory
            .apply(
                vec![ObservatoryMutation::Observe(record(
                    &profile,
                    &other_session,
                    EntryId::new(),
                    "later",
                    "later evidence invalidates frozen activation",
                    EvidenceSourceKind::AssistantFinal,
                    Vec::new(),
                    11,
                ))],
                UtcTimestamp::from_unix_millis(11),
            )
            .unwrap();
        assert_eq!(
            validate_activation(&observatory, &manifest, Sensitivity::Personal).unwrap_err(),
            ActivationError::Stale
        );
    }
}
