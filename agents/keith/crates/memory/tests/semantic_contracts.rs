use keith_agent_types::{ActionId, EntityId, GoalId, ProfileId, SchemaVersion, SessionId};
use keith_memory::{
    CandidateEvidenceReference, SEMANTIC_CANDIDATE_VERSION, SemanticCandidate,
    SemanticCandidateBatch, SemanticCandidateError, SemanticCandidateLane, SemanticCandidateQuery,
    SemanticDegradedReason, SemanticIndexIdentity,
};
use keith_provider_core::{
    EMBEDDING_CONTRACT_VERSION, EmbeddingDistance, EmbeddingNormalization, EmbeddingSpaceIdentity,
};
use keith_session_store::Sensitivity;

fn query() -> SemanticCandidateQuery {
    SemanticCandidateQuery {
        version: SEMANTIC_CANDIDATE_VERSION,
        profile_id: ProfileId::new(),
        session_id: SessionId::new(),
        action_id: ActionId::new(),
        goal_id: Some(GoalId::new()),
        query: "Where is the synthetic compass?".into(),
        query_identity: "a".repeat(64),
        archive_revision: 12,
        index: SemanticIndexIdentity {
            generation: EntityId::new(),
            space: EmbeddingSpaceIdentity {
                version: EMBEDDING_CONTRACT_VERSION,
                provider: "contract-fixture".into(),
                model: "encoder-a".into(),
                revision: "revision-a".into(),
                dimensions: 768,
                distance: EmbeddingDistance::Cosine,
                normalization: EmbeddingNormalization::UnitL2,
                representation_version: "observation-v1".into(),
            },
        },
        max_sensitivity: Sensitivity::Personal,
        limit: 8,
        timeout_ms: 1_000,
    }
}

fn batch(query: &SemanticCandidateQuery) -> SemanticCandidateBatch {
    SemanticCandidateBatch {
        version: query.version,
        profile_id: query.profile_id.clone(),
        session_id: query.session_id.clone(),
        action_id: query.action_id.clone(),
        goal_id: query.goal_id.clone(),
        query_identity: query.query_identity.clone(),
        index: query.index.clone(),
        source_revision: query.archive_revision,
        candidates: vec![SemanticCandidate {
            evidence: CandidateEvidenceReference {
                evidence_id: EntityId::new(),
                content_digest: "b".repeat(64),
                archive_revision: query.archive_revision,
            },
            lane: SemanticCandidateLane::ObservationMeaning,
            rank: 1,
        }],
        degraded: vec![],
    }
}

#[test]
fn candidate_contract_roundtrips_references_without_authoritative_text() {
    let query = query();
    let batch = batch(&query);
    let encoded = serde_json::to_value(&batch).unwrap();
    assert!(encoded["candidates"][0].get("text").is_none());
    let decoded: SemanticCandidateBatch = serde_json::from_value(encoded).unwrap();
    assert_eq!(decoded, batch);
    decoded.validate_for(&query).unwrap();
    let decoded: SemanticCandidateQuery =
        serde_json::from_slice(&serde_json::to_vec(&query).unwrap()).unwrap();
    assert_eq!(decoded, query);
    decoded.validate().unwrap();
}

#[test]
fn candidates_cannot_cross_profile_session_action_goal_or_query_scope() {
    let query = query();
    let original = batch(&query);
    let mut variants = vec![original.clone(); 5];
    variants[0].profile_id = ProfileId::new();
    variants[1].session_id = SessionId::new();
    variants[2].action_id = ActionId::new();
    variants[3].goal_id = Some(GoalId::new());
    variants[4].query_identity = "c".repeat(64);
    for changed in variants {
        assert_eq!(
            changed.validate_for(&query),
            Err(SemanticCandidateError::InvalidBatch)
        );
    }
}

#[test]
fn query_debug_does_not_disclose_memory_search_text() {
    let mut query = query();
    query.query = "private source content must stay out of diagnostics".into();
    let debug = format!("{query:?}");
    assert!(!debug.contains(&query.query));
    assert!(debug.contains("query_bytes"));
    assert!(debug.contains(&query.query_identity));
}

#[test]
fn equal_dimensions_do_not_allow_mixed_encoders_representations_or_generations() {
    let query = query();
    let original = batch(&query);
    let mut variants = vec![original.clone(); 4];
    variants[0].index.space.model = "encoder-b".into();
    variants[1].index.space.revision = "revision-b".into();
    variants[2].index.space.representation_version = "action-v1".into();
    variants[3].index.generation = EntityId::new();
    for changed in variants {
        assert_eq!(
            changed.validate_for(&query),
            Err(SemanticCandidateError::InvalidBatch)
        );
    }
}

#[test]
fn lag_must_be_explicit_and_future_revisions_are_rejected() {
    let query = query();
    let mut batch = batch(&query);
    batch.source_revision -= 1;
    batch.candidates[0].evidence.archive_revision -= 1;
    assert!(batch.validate_for(&query).is_err());
    batch.degraded.push(SemanticDegradedReason::IndexLag);
    batch.validate_for(&query).unwrap();
    batch.candidates[0].evidence.archive_revision = query.archive_revision;
    assert!(batch.validate_for(&query).is_err());
    batch.source_revision = query.archive_revision + 1;
    assert!(batch.validate_for(&query).is_err());
}

#[test]
fn duplicate_lane_hits_ranks_and_malformed_digests_are_rejected() {
    let query = query();
    let mut batch = batch(&query);
    batch.candidates.push(batch.candidates[0].clone());
    assert!(batch.validate_for(&query).is_err());
    batch.candidates[1].evidence.evidence_id = EntityId::new();
    assert!(batch.validate_for(&query).is_err());
    batch.candidates[1].rank = 2;
    batch.validate_for(&query).unwrap();
    batch.candidates[1].evidence.content_digest = "unverified".into();
    assert!(batch.validate_for(&query).is_err());
}

#[test]
fn unknown_versions_and_unbounded_queries_are_explicit_errors() {
    let original = query();
    let mut query = original.clone();
    query.version = SchemaVersion::new(1, 1);
    assert_eq!(
        query.validate(),
        Err(SemanticCandidateError::UnsupportedVersion)
    );
    let mut batch = batch(&original);
    batch.version = SchemaVersion::new(2, 0);
    assert_eq!(
        batch.validate_for(&original),
        Err(SemanticCandidateError::UnsupportedVersion)
    );
    query = original.clone();
    query.limit = 129;
    assert_eq!(query.validate(), Err(SemanticCandidateError::InvalidQuery));
    query = original.clone();
    query.timeout_ms = 0;
    assert!(query.validate().is_err());
    query = original;
    query.query = "x".repeat(16 * 1024 + 1);
    assert!(query.validate().is_err());
}
