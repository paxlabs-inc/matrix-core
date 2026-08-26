use std::collections::{BTreeSet, VecDeque};

use keith_agent_types::{
    EntityId, EntryId, ProfileId, SessionId, UtcTimestamp, canonical_json_bytes,
};
use keith_provider_core::CancellationToken;
use keith_session_store::{RetentionClass, Sensitivity, SessionEntry};
use keith_subagents::MemoryScoutLimits;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{
    AtlasCoverage, AtlasSearchRequest, EvidenceAuthority, EvidenceFacet, EvidenceFacetKind,
    EvidenceRecord, EvidenceSourceKind, EvidenceValidity, MemoryError, MemoryService,
    ObservatoryMutation, RecallCapsule,
};

pub const MEMORY_CONTEXT_SELECTOR_VERSION: &str = "memory-context-v1";
const MAX_PENDING_INGESTIONS: usize = 64;
const MAX_HOT_ANCHORS: usize = 32;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AgentMemoryKind {
    Preference,
    PersonalFact,
    ProjectContext,
    Routine,
    Relationship,
    Commitment,
    Procedure,
    PreferredName,
}

impl AgentMemoryKind {
    fn theme(self) -> &'static str {
        match self {
            Self::Preference => "preference",
            Self::PersonalFact => "personal_fact",
            Self::ProjectContext => "project_context",
            Self::Routine => "routine",
            Self::Relationship | Self::PreferredName => "relationship",
            Self::Commitment => "commitment",
            Self::Procedure => "procedure",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MemoryWriteSource {
    pub source_entry_id: EntryId,
    pub evidence_quote: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MemoryCreateRequest {
    pub source: MemoryWriteSource,
    pub text: String,
    pub kind: AgentMemoryKind,
    pub facets: Vec<EvidenceFacet>,
    pub sensitivity: Sensitivity,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MemoryCorrectRequest {
    pub evidence_id: EntityId,
    pub source: MemoryWriteSource,
    pub replacement: String,
    pub facets: Vec<EvidenceFacet>,
    pub sensitivity: Option<Sensitivity>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MemoryForgetRequest {
    pub evidence_id: EntityId,
    pub source: MemoryWriteSource,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryContradiction {
    pub earlier: EntityId,
    pub later: EntityId,
    pub reason: String,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryContextBundle {
    pub bundle_id: String,
    pub selector_version: String,
    pub profile_id: ProfileId,
    pub calling_session_id: SessionId,
    pub query: String,
    pub archive_revision: u64,
    pub evidence: Vec<EvidenceRecord>,
    pub temporal_neighbors: Vec<EvidenceRecord>,
    pub entities: Vec<String>,
    pub graph_nodes: Vec<String>,
    pub corrections: Vec<EvidenceRecord>,
    pub contradictions: Vec<MemoryContradiction>,
    pub gaps: Vec<String>,
    pub coverage: AtlasCoverage,
    pub deep_recall: Option<RecallCapsule>,
    pub token_price: u64,
    pub truncated: bool,
}

#[derive(Clone)]
pub(crate) struct PendingIngestion {
    pub session_id: SessionId,
    pub entries: Vec<SessionEntry>,
}

#[derive(Default)]
pub(crate) struct PendingIngestionQueue {
    items: VecDeque<PendingIngestion>,
}

impl PendingIngestionQueue {
    pub(crate) fn push(&mut self, item: PendingIngestion) {
        if self.items.len() >= MAX_PENDING_INGESTIONS {
            self.items.pop_front();
        }
        self.items.push_back(item);
    }

    fn take(&mut self) -> VecDeque<PendingIngestion> {
        std::mem::take(&mut self.items)
    }

    fn restore(&mut self, mut items: VecDeque<PendingIngestion>) {
        items.append(&mut self.items);
        self.items = items;
        while self.items.len() > MAX_PENDING_INGESTIONS {
            self.items.pop_front();
        }
    }
}

#[derive(Default)]
pub(crate) struct HotMemoryCache {
    revision: u64,
    anchors: Vec<EvidenceRecord>,
}

impl MemoryService {
    /// Queues committed session evidence for fail-open ingestion. The committed session log is the
    /// durable cursor: replaying its ancestry after a crash is safe because source identities are
    /// idempotent in the evidence vault.
    pub fn enqueue_session_entries(&self, session_id: &SessionId, entries: &[SessionEntry]) {
        if entries.is_empty() {
            return;
        }
        if let Ok(mut pending) = self.pending_ingestion.lock() {
            pending.push(PendingIngestion {
                session_id: session_id.clone(),
                entries: entries.to_vec(),
            });
        }
    }

    /// Drains queued ingestion as optional maintenance. Failed items are restored for a later
    /// attempt and never become turn, finalization, outbox, or delivery failures.
    pub fn flush_pending_ingestion(&self, now: UtcTimestamp) -> Result<u64, MemoryError> {
        let mut items = self
            .pending_ingestion
            .lock()
            .map_err(|_| MemoryError::LockPoisoned)?
            .take();
        let mut revision = self.observatory.revision()?;
        while let Some(item) = items.pop_front() {
            match self
                .observatory
                .ingest_session_entries(&item.session_id, &item.entries, now)
            {
                Ok(next_revision) => revision = next_revision,
                Err(error) => {
                    items.push_front(item);
                    self.pending_ingestion
                        .lock()
                        .map_err(|_| MemoryError::LockPoisoned)?
                        .restore(items);
                    return Err(error.into());
                }
            }
        }
        Ok(revision)
    }

    /// Creates source-cited durable memory. The host validates exact evidence and schema while the
    /// agent remains responsible for interpreting what the source means.
    pub fn memory_create(
        &self,
        request: MemoryCreateRequest,
        now: UtcTimestamp,
    ) -> Result<EvidenceRecord, MemoryError> {
        self.flush_pending_ingestion(now)?;
        let user_assertion = matches!(
            request.kind,
            AgentMemoryKind::Preference
                | AgentMemoryKind::PersonalFact
                | AgentMemoryKind::Routine
                | AgentMemoryKind::Relationship
                | AgentMemoryKind::PreferredName
        );
        let source = self.validate_write_source(
            &request.source,
            user_assertion.then_some(EvidenceAuthority::UserAsserted),
        )?;
        if request.text.trim().is_empty() {
            return Err(MemoryError::EmptyText);
        }
        if request.kind == AgentMemoryKind::PreferredName {
            let relationship = self
                .relationship
                .as_ref()
                .ok_or(MemoryError::Relationship(crate::RelationshipError::Invalid))?;
            let context = relationship.confirm_preferred_name(
                &source.source_session,
                &request.source.source_entry_id,
                &source_digest_for(&source, &request.source.source_entry_id)?,
                request.text.trim(),
                now,
            )?;
            relationship.sync_evidence(&self.observatory, now)?;
            let preferred = context
                .preferred_name
                .ok_or(MemoryError::Relationship(crate::RelationshipError::Invalid))?;
            return self
                .observatory
                .evidence(&[preferred.evidence_id], Sensitivity::Secret)?
                .into_iter()
                .next()
                .ok_or(MemoryError::MissingRecord);
        }

        let mut facets = request.facets;
        facets.push(EvidenceFacet {
            kind: EvidenceFacetKind::Theme,
            value: request.kind.theme().into(),
        });
        normalize_facets(&mut facets)?;
        let id = EntityId::new();
        let source_identity = format!("agent-memory:{id}");
        let mut evidence = EvidenceRecord::new(
            source.profile_id.clone(),
            source.source_session.clone(),
            vec![request.source.source_entry_id.clone()],
            vec![source_digest_for(&source, &request.source.source_entry_id)?],
            source_identity,
            Some(request.source.source_entry_id),
            EvidenceSourceKind::DurableMemory,
            source.authority,
            request.text.trim().to_owned(),
            now,
            request.sensitivity,
            RetentionClass::Durable,
            facets,
        );
        evidence.id = id;
        self.observatory
            .apply(vec![ObservatoryMutation::Observe(evidence.clone())], now)?;
        self.invalidate_hot_cache();
        Ok(evidence)
    }

    /// Supersedes one exact evidence record with a newly source-cited correction.
    pub fn memory_correct(
        &self,
        request: MemoryCorrectRequest,
        now: UtcTimestamp,
    ) -> Result<EvidenceRecord, MemoryError> {
        self.flush_pending_ingestion(now)?;
        if request.replacement.trim().is_empty() {
            return Err(MemoryError::EmptyText);
        }
        let prior = self
            .observatory
            .evidence(&[request.evidence_id.clone()], Sensitivity::Secret)?
            .into_iter()
            .next()
            .ok_or(MemoryError::MissingRecord)?;
        let source = self.validate_write_source(
            &request.source,
            (prior.authority == EvidenceAuthority::UserAsserted)
                .then_some(EvidenceAuthority::UserAsserted),
        )?;
        if has_tag(&prior, "preferred_name") {
            let relationship = self
                .relationship
                .as_ref()
                .ok_or(MemoryError::Relationship(crate::RelationshipError::Invalid))?;
            let context = relationship.confirm_preferred_name(
                &source.source_session,
                &request.source.source_entry_id,
                &source_digest_for(&source, &request.source.source_entry_id)?,
                request.replacement.trim(),
                now,
            )?;
            relationship.sync_evidence(&self.observatory, now)?;
            let preferred = context
                .preferred_name
                .ok_or(MemoryError::Relationship(crate::RelationshipError::Invalid))?;
            return self
                .observatory
                .evidence(&[preferred.evidence_id], Sensitivity::Secret)?
                .into_iter()
                .next()
                .ok_or(MemoryError::MissingRecord);
        }

        let mut facets = if request.facets.is_empty() {
            prior.facets.clone()
        } else {
            request.facets
        };
        normalize_facets(&mut facets)?;
        let id = EntityId::new();
        let mut replacement = EvidenceRecord::new(
            prior.profile_id.clone(),
            source.source_session.clone(),
            vec![request.source.source_entry_id.clone()],
            vec![source_digest_for(&source, &request.source.source_entry_id)?],
            format!("agent-memory:{id}"),
            Some(request.source.source_entry_id),
            EvidenceSourceKind::DurableMemory,
            source.authority,
            request.replacement.trim().to_owned(),
            now,
            request.sensitivity.unwrap_or(prior.sensitivity),
            RetentionClass::Durable,
            facets,
        );
        replacement.id = id;
        self.observatory.apply(
            vec![ObservatoryMutation::Supersede {
                prior_id: prior.id,
                replacement: replacement.clone(),
            }],
            now,
        )?;
        self.invalidate_hot_cache();
        Ok(replacement)
    }

    /// Removes one record from all future activation while retaining a source-cited tombstone.
    pub fn memory_forget(
        &self,
        request: MemoryForgetRequest,
        now: UtcTimestamp,
    ) -> Result<(), MemoryError> {
        self.flush_pending_ingestion(now)?;
        let prior = self
            .observatory
            .evidence(&[request.evidence_id.clone()], Sensitivity::Secret)?
            .into_iter()
            .next()
            .ok_or(MemoryError::MissingRecord)?;
        let source = self.validate_write_source(
            &request.source,
            (prior.authority == EvidenceAuthority::UserAsserted)
                .then_some(EvidenceAuthority::UserAsserted),
        )?;
        let source_digest = source_digest_for(&source, &request.source.source_entry_id)?;
        if has_tag(&prior, "preferred_name") {
            let relationship = self
                .relationship
                .as_ref()
                .ok_or(MemoryError::Relationship(crate::RelationshipError::Invalid))?;
            relationship.forget_preferred_name(
                &source.source_session,
                &request.source.source_entry_id,
                &source_digest,
                now,
            )?;
            relationship.sync_evidence(&self.observatory, now)?;
        } else {
            self.observatory.apply(
                vec![ObservatoryMutation::Delete {
                    evidence_id: request.evidence_id,
                    source_entries: vec![request.source.source_entry_id],
                    source_digests: vec![source_digest],
                }],
                now,
            )?;
        }
        self.invalidate_hot_cache();
        Ok(())
    }

    pub fn memory_search(
        &self,
        query: &str,
        limit: usize,
        max_sensitivity: Sensitivity,
    ) -> Result<(Vec<crate::AtlasSearchResult>, AtlasCoverage), MemoryError> {
        self.observatory
            .search(&AtlasSearchRequest {
                query: query.to_owned(),
                limit,
                max_sensitivity,
                include_disputed: true,
            })
            .map_err(Into::into)
    }

    pub fn memory_get(
        &self,
        evidence_ids: &[EntityId],
        max_sensitivity: Sensitivity,
    ) -> Result<Vec<EvidenceRecord>, MemoryError> {
        self.observatory
            .evidence(evidence_ids, max_sensitivity)
            .map_err(Into::into)
    }

    #[allow(clippy::too_many_arguments, clippy::too_many_lines)]
    pub fn memory_context(
        &self,
        calling_session_id: &SessionId,
        query: &str,
        token_budget: u64,
        max_sensitivity: Sensitivity,
        deep: bool,
        cancellation: &CancellationToken,
        now: UtcTimestamp,
    ) -> Result<MemoryContextBundle, MemoryError> {
        if query.trim().is_empty() || !(128..=16_000).contains(&token_budget) {
            return Err(MemoryError::InvalidRequest);
        }
        let revision = self.observatory.revision()?;
        let (search, coverage) = self.memory_search(query, 32, max_sensitivity)?;
        let snapshot = self.observatory.evidence_snapshot()?;
        if self.observatory.revision()? != revision {
            return Err(MemoryError::Changed);
        }

        let mut candidates = search
            .iter()
            .map(|result| result.evidence.clone())
            .collect::<Vec<_>>();
        for anchor in self.hot_anchors(max_sensitivity)? {
            if !candidates.iter().any(|record| record.id == anchor.id) {
                candidates.push(anchor);
            }
        }
        let mut token_price = 0_u64;
        let mut truncated = coverage.truncated;
        let mut evidence = Vec::new();
        for record in candidates {
            let price = evidence_token_price(&record);
            if token_price.saturating_add(price) > token_budget {
                truncated = true;
                continue;
            }
            token_price = token_price.saturating_add(price);
            evidence.push(record);
        }
        let selected_ids = evidence
            .iter()
            .map(|record| record.id.clone())
            .collect::<BTreeSet<_>>();
        let selected_sessions = evidence
            .iter()
            .map(|record| record.source_session.clone())
            .collect::<BTreeSet<_>>();
        let mut ordered_timeline = snapshot
            .values()
            .filter(|record| {
                selected_sessions.contains(&record.source_session)
                    && matches!(
                        record.validity,
                        EvidenceValidity::Active | EvidenceValidity::Disputed
                    )
                    && sensitivity_rank(record.sensitivity) <= sensitivity_rank(max_sensitivity)
            })
            .cloned()
            .collect::<Vec<_>>();
        ordered_timeline.sort_by_key(|record| {
            (
                record.source_session.clone(),
                record.occurred_at,
                record.id.clone(),
            )
        });
        let temporal_ids = evidence
            .iter()
            .flat_map(|selected| {
                let position = ordered_timeline
                    .iter()
                    .position(|record| record.id == selected.id);
                position
                    .into_iter()
                    .flat_map(|index| {
                        [index.checked_sub(1), index.checked_add(1)]
                            .into_iter()
                            .flatten()
                    })
                    .filter_map(|index| ordered_timeline.get(index))
                    .filter(|record| record.source_session == selected.source_session)
                    .map(|record| record.id.clone())
                    .collect::<Vec<_>>()
            })
            .filter(|id| !selected_ids.contains(id))
            .collect::<BTreeSet<_>>();
        let mut temporal_neighbors = Vec::new();
        for record in ordered_timeline
            .into_iter()
            .filter(|record| temporal_ids.contains(&record.id))
            .take(12)
        {
            let price = evidence_token_price(&record);
            if token_price.saturating_add(price) > token_budget {
                truncated = true;
                break;
            }
            token_price = token_price.saturating_add(price);
            temporal_neighbors.push(record);
        }

        let correction_candidates = snapshot
            .values()
            .filter(|record| {
                record.validity == EvidenceValidity::Disputed
                    || record
                        .supersedes
                        .as_ref()
                        .is_some_and(|prior| selected_ids.contains(prior))
                    || record
                        .superseded_by
                        .as_ref()
                        .is_some_and(|later| selected_ids.contains(later))
            })
            .filter(|record| {
                sensitivity_rank(record.sensitivity) <= sensitivity_rank(max_sensitivity)
            })
            .cloned()
            .collect::<Vec<_>>();
        let mut corrections = Vec::new();
        for record in correction_candidates {
            let price = evidence_token_price(&record);
            if token_price.saturating_add(price) > token_budget {
                truncated = true;
                continue;
            }
            token_price = token_price.saturating_add(price);
            corrections.push(record);
        }
        let contradictions = corrections
            .iter()
            .filter_map(|record| {
                record.supersedes.as_ref().map(|prior| MemoryContradiction {
                    earlier: prior.clone(),
                    later: record.id.clone(),
                    reason: "later source-cited correction supersedes earlier evidence".into(),
                })
            })
            .collect::<Vec<_>>();
        let entities = evidence
            .iter()
            .chain(&corrections)
            .flat_map(|record| &record.facets)
            .filter(|facet| facet.kind == EvidenceFacetKind::Entity)
            .map(|facet| facet.value.clone())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect();
        let graph_nodes = search
            .iter()
            .flat_map(|result| result.matched_nodes.iter().cloned())
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect();

        let mut gaps = Vec::new();
        if evidence.is_empty() {
            gaps.push("no source-linked evidence matched the query".into());
        }
        if coverage.truncated || truncated {
            gaps.push("matching evidence remains outside this bounded capsule".into());
        }
        if corrections
            .iter()
            .any(|record| record.validity == EvidenceValidity::Disputed)
        {
            gaps.push("disputed evidence requires user or source confirmation".into());
        }
        let remaining_tokens = token_budget.saturating_sub(token_price);
        let deep_recall = if deep
            && !evidence.is_empty()
            && remaining_tokens >= 128
            && !cancellation.is_cancelled()
        {
            let limits = MemoryScoutLimits {
                max_depth: 3,
                max_children: 3,
                max_total_scouts: 12,
                max_concurrency: 3,
                max_tokens: remaining_tokens.min(8_000),
                ..MemoryScoutLimits::default()
            };
            self.recall
                .prepare(
                    &self.observatory,
                    calling_session_id,
                    query,
                    max_sensitivity,
                    limits,
                    cancellation,
                    now,
                )
                .and_then(|request| {
                    self.recall
                        .execute(&self.observatory, &request, cancellation, now)
                })
                .ok()
        } else {
            None
        };
        if let Some(capsule) = &deep_recall {
            token_price = token_price.saturating_add(capsule.token_price);
        }
        if deep && deep_recall.is_none() {
            gaps.push("deep read-only recall produced no validated capsule".into());
        }

        let identity = canonical_json_bytes(&(
            MEMORY_CONTEXT_SELECTOR_VERSION,
            &self.profile_id,
            calling_session_id,
            query,
            revision,
            evidence
                .iter()
                .map(|record| (&record.id, &record.content_digest))
                .collect::<Vec<_>>(),
        ))
        .map_err(|error| MemoryError::Identity(error.to_string()))?;
        Ok(MemoryContextBundle {
            bundle_id: hex_digest(&identity),
            selector_version: MEMORY_CONTEXT_SELECTOR_VERSION.into(),
            profile_id: self.profile_id.clone(),
            calling_session_id: calling_session_id.clone(),
            query: query.to_owned(),
            archive_revision: revision,
            evidence,
            temporal_neighbors,
            entities,
            graph_nodes,
            corrections,
            contradictions,
            gaps,
            coverage,
            deep_recall,
            token_price,
            truncated,
        })
    }

    fn validate_write_source(
        &self,
        source: &MemoryWriteSource,
        authority: Option<EvidenceAuthority>,
    ) -> Result<EvidenceRecord, MemoryError> {
        let quote = source.evidence_quote.trim();
        if quote.is_empty() {
            return Err(MemoryError::InvalidEvidenceQuote);
        }
        let snapshot = self.observatory.evidence_snapshot()?;
        snapshot
            .values()
            .find(|record| {
                record.source_entries.contains(&source.source_entry_id)
                    && record.source_kind == EvidenceSourceKind::UserMessage
                    && matches!(
                        record.validity,
                        EvidenceValidity::Active | EvidenceValidity::Disputed
                    )
                    && authority.is_none_or(|expected| record.authority == expected)
                    && record.text.contains(quote)
            })
            .cloned()
            .ok_or(MemoryError::InvalidEvidenceQuote)
    }

    fn hot_anchors(
        &self,
        max_sensitivity: Sensitivity,
    ) -> Result<Vec<EvidenceRecord>, MemoryError> {
        let revision = self.observatory.revision()?;
        let mut cache = self
            .hot_cache
            .lock()
            .map_err(|_| MemoryError::LockPoisoned)?;
        if cache.revision != revision {
            let snapshot = self.observatory.evidence_snapshot()?;
            cache.anchors = snapshot
                .values()
                .filter(|record| {
                    record.source_kind == EvidenceSourceKind::DurableMemory
                        && record.validity == EvidenceValidity::Active
                        && record.facets.iter().any(|facet| {
                            facet.kind == EvidenceFacetKind::Theme
                                && matches!(
                                    facet.value.as_str(),
                                    "preference" | "personal_fact" | "relationship" | "routine"
                                )
                        })
                })
                .cloned()
                .take(MAX_HOT_ANCHORS)
                .collect();
            cache.revision = revision;
        }
        Ok(cache
            .anchors
            .iter()
            .filter(|record| {
                sensitivity_rank(record.sensitivity) <= sensitivity_rank(max_sensitivity)
            })
            .cloned()
            .collect())
    }

    fn invalidate_hot_cache(&self) {
        if let Ok(mut cache) = self.hot_cache.lock() {
            cache.revision = u64::MAX;
            cache.anchors.clear();
        }
    }
}

fn source_digest_for(record: &EvidenceRecord, entry_id: &EntryId) -> Result<String, MemoryError> {
    record
        .source_entries
        .iter()
        .position(|candidate| candidate == entry_id)
        .and_then(|index| record.source_digests.get(index))
        .cloned()
        .ok_or(MemoryError::InvalidEvidenceQuote)
}

fn normalize_facets(facets: &mut Vec<EvidenceFacet>) -> Result<(), MemoryError> {
    if facets.len() > 64
        || facets.iter().any(|facet| {
            facet.value.trim().is_empty()
                || facet.value.len() > 256
                || facet.value != facet.value.trim()
        })
    {
        return Err(MemoryError::InvalidRequest);
    }
    facets.sort();
    facets.dedup();
    Ok(())
}

fn has_tag(record: &EvidenceRecord, value: &str) -> bool {
    record
        .facets
        .iter()
        .any(|facet| facet.kind == EvidenceFacetKind::Tag && facet.value == value)
}

fn evidence_token_price(record: &EvidenceRecord) -> u64 {
    u64::try_from(record.text.len().saturating_add(3) / 4).unwrap_or(u64::MAX)
}

const fn sensitivity_rank(sensitivity: Sensitivity) -> u8 {
    match sensitivity {
        Sensitivity::Public => 0,
        Sensitivity::Personal => 1,
        Sensitivity::Sensitive => 2,
        Sensitivity::Secret => 3,
    }
}

fn hex_digest(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .map(|byte| format!("{byte:02x}"))
        .collect()
}
