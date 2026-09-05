//! Memory-owned candidate port. Adapters propose references, never authoritative text.
//!
//! These contracts do not configure an encoder, establish trained-encoder readiness,
//! or activate a retrieval path. Consumers validate the batch, then rehydrate every
//! reference from the profile's canonical vault at the requested snapshot revision.

use std::collections::BTreeSet;
use std::fmt;

use keith_agent_types::{ActionId, EntityId, GoalId, ProfileId, SchemaVersion, SessionId};
use keith_provider_core::{CancellationToken, EmbeddingSpaceIdentity};
use keith_session_store::Sensitivity;
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::causal::valid_digest;

pub const SEMANTIC_CANDIDATE_VERSION: SchemaVersion = SchemaVersion::new(1, 0);
const MAX_QUERY_BYTES: usize = 16 * 1024;
const MAX_CANDIDATES: usize = 128;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SemanticIndexIdentity {
    pub generation: EntityId,
    pub space: EmbeddingSpaceIdentity,
}

/// Revision is the vault snapshot at which this reference was indexed, not an
/// invented per-record counter. Rehydration also checks validity and sensitivity.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CandidateEvidenceReference {
    pub evidence_id: EntityId,
    pub content_digest: String,
    pub archive_revision: u64,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SemanticCandidateLane {
    ObservationMeaning,
    ActionPreconditionOutcome,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SemanticCandidate {
    pub evidence: CandidateEvidenceReference,
    pub lane: SemanticCandidateLane,
    /// One-based rank within a lane; raw scores from different spaces are absent.
    pub rank: u32,
}

#[derive(Clone, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SemanticCandidateQuery {
    pub version: SchemaVersion,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub action_id: ActionId,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub goal_id: Option<GoalId>,
    pub query: String,
    /// Digest of the caller's versioned query representation, not a provider key.
    pub query_identity: String,
    pub archive_revision: u64,
    pub index: SemanticIndexIdentity,
    pub max_sensitivity: Sensitivity,
    pub limit: usize,
    pub timeout_ms: u64,
}

impl fmt::Debug for SemanticCandidateQuery {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("SemanticCandidateQuery")
            .field("version", &self.version)
            .field("profile_id", &self.profile_id)
            .field("session_id", &self.session_id)
            .field("action_id", &self.action_id)
            .field("goal_id", &self.goal_id)
            .field("query_bytes", &self.query.len())
            .field("query_identity", &self.query_identity)
            .field("archive_revision", &self.archive_revision)
            .field("index", &self.index)
            .field("max_sensitivity", &self.max_sensitivity)
            .field("limit", &self.limit)
            .field("timeout_ms", &self.timeout_ms)
            .finish()
    }
}

impl SemanticCandidateQuery {
    /// # Errors
    ///
    /// Rejects unknown versions, unbounded requests and malformed encoder identities.
    pub fn validate(&self) -> Result<(), SemanticCandidateError> {
        validate_version(self.version)?;
        if self.query.trim().is_empty()
            || self.query.len() > MAX_QUERY_BYTES
            || !valid_digest(&self.query_identity)
            || self.limit == 0
            || self.limit > MAX_CANDIDATES
            || self.timeout_ms == 0
            || self.timeout_ms > 600_000
            || self.index.space.validate().is_err()
        {
            return Err(SemanticCandidateError::InvalidQuery);
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SemanticDegradedReason {
    EncoderUnavailable,
    IndexUnavailable,
    IndexLag,
    Timeout,
    PolicyRestricted,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SemanticCandidateBatch {
    pub version: SchemaVersion,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub action_id: ActionId,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub goal_id: Option<GoalId>,
    pub query_identity: String,
    pub index: SemanticIndexIdentity,
    /// Canonical source watermark for this generation, not a readiness claim.
    pub source_revision: u64,
    pub candidates: Vec<SemanticCandidate>,
    pub degraded: Vec<SemanticDegradedReason>,
}

impl SemanticCandidateBatch {
    /// # Errors
    ///
    /// Rejects mixed scopes/spaces/generations, future revisions, silent lag,
    /// duplicate lane hits, and malformed references. A valid batch remains untrusted.
    pub fn validate_for(
        &self,
        query: &SemanticCandidateQuery,
    ) -> Result<(), SemanticCandidateError> {
        query.validate()?;
        validate_version(self.version)?;
        if self.profile_id != query.profile_id
            || self.session_id != query.session_id
            || self.action_id != query.action_id
            || self.goal_id != query.goal_id
            || self.query_identity != query.query_identity
            || self.index != query.index
            || self.source_revision > query.archive_revision
            || (self.source_revision < query.archive_revision
                && !self.degraded.contains(&SemanticDegradedReason::IndexLag))
            || self.candidates.len() > query.limit
            || self.degraded.len() > 5
            || self.degraded.iter().collect::<BTreeSet<_>>().len() != self.degraded.len()
        {
            return Err(SemanticCandidateError::InvalidBatch);
        }
        let mut hits = BTreeSet::new();
        let mut ranks = BTreeSet::new();
        for candidate in &self.candidates {
            if candidate.rank == 0
                || !valid_digest(&candidate.evidence.content_digest)
                || candidate.evidence.archive_revision == 0
                || candidate.evidence.archive_revision > self.source_revision
                || !hits.insert((candidate.lane, &candidate.evidence.evidence_id))
                || !ranks.insert((candidate.lane, candidate.rank))
            {
                return Err(SemanticCandidateError::InvalidBatch);
            }
        }
        Ok(())
    }
}

fn validate_version(version: SchemaVersion) -> Result<(), SemanticCandidateError> {
    if version != SEMANTIC_CANDIDATE_VERSION {
        return Err(SemanticCandidateError::UnsupportedVersion);
    }
    Ok(())
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Error)]
pub enum SemanticCandidateError {
    #[error("unsupported semantic candidate contract version")]
    UnsupportedVersion,
    #[error("invalid semantic candidate query")]
    InvalidQuery,
    #[error("invalid semantic candidate batch")]
    InvalidBatch,
    #[error("semantic candidate request cancelled")]
    Cancelled,
    #[error("semantic candidate source unavailable: {0:?}")]
    Unavailable(SemanticDegradedReason),
}

/// Implementations must validate input before encoding, honor the timeout and
/// cancellation before/after I/O, and never silently truncate or switch generations.
pub trait SemanticCandidateSource: Send + Sync {
    /// # Errors
    ///
    /// Returns invalid input, cancellation or an explicit unavailable/degraded reason.
    fn search(
        &self,
        query: &SemanticCandidateQuery,
        cancellation: &CancellationToken,
    ) -> Result<SemanticCandidateBatch, SemanticCandidateError>;
}
