//! Additive provenance metadata. Absence means unknown, never inferred history.

use std::collections::BTreeSet;

use crate::CandidateEvidenceReference;
use keith_agent_types::{EntryId, SchemaVersion, SessionId, UtcTimestamp};
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const EVIDENCE_CAUSAL_VERSION: SchemaVersion = SchemaVersion::new(1, 0);
const MAX_ROOTS: usize = 256;

/// An originating observation, rather than each derived view of that observation.
/// Canonical resolution must still verify the entry and its checksum in this profile.
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvidenceSourceRoot {
    pub source_session: SessionId,
    pub source_entry: EntryId,
    pub source_digest: String,
}

/// Half-open effective interval; missing boundaries remain unknown/unbounded.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvidenceEffectiveInterval {
    pub from: Option<UtcTimestamp>,
    pub until: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(try_from = "RawEvidenceCausalMetadata")]
pub struct EvidenceCausalMetadata {
    pub version: SchemaVersion,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub effective: Option<EvidenceEffectiveInterval>,
    pub source_roots: Vec<EvidenceSourceRoot>,
    /// Context provenance only; these are not positive per-claim support votes.
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub derived_from: Vec<CandidateEvidenceReference>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub gaps: Vec<SourceLineageGap>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SourceLineageGapReason {
    MissingSource,
    DeletedSource,
    ConflictingSource,
    CrossProfile,
    UnsupportedSource,
    PendingFinal,
    PendingCompaction,
    Limit,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SourceLineageGap {
    pub source_session: SessionId,
    pub source_entry: EntryId,
    pub reason: SourceLineageGapReason,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawEvidenceCausalMetadata {
    version: SchemaVersion,
    #[serde(default)]
    effective: Option<EvidenceEffectiveInterval>,
    source_roots: Vec<EvidenceSourceRoot>,
    #[serde(default)]
    derived_from: Vec<CandidateEvidenceReference>,
    #[serde(default)]
    gaps: Vec<SourceLineageGap>,
}

impl TryFrom<RawEvidenceCausalMetadata> for EvidenceCausalMetadata {
    type Error = EvidenceMetadataError;

    fn try_from(value: RawEvidenceCausalMetadata) -> Result<Self, Self::Error> {
        let metadata = Self {
            version: value.version,
            effective: value.effective,
            source_roots: value.source_roots,
            derived_from: value.derived_from,
            gaps: value.gaps,
        };
        metadata.validate()?;
        Ok(metadata)
    }
}

impl EvidenceCausalMetadata {
    /// # Errors
    ///
    /// Rejects unsupported versions, empty metadata, inverted intervals, and
    /// duplicate or malformed roots. This does not authenticate a source root.
    pub fn validate(&self) -> Result<(), EvidenceMetadataError> {
        if self.version != EVIDENCE_CAUSAL_VERSION {
            return Err(EvidenceMetadataError::UnsupportedVersion);
        }
        if self.source_roots.len() > MAX_ROOTS
            || self.derived_from.len() > MAX_ROOTS
            || self.gaps.len() > MAX_ROOTS
            || self.derived_from.iter().any(|source| source.archive_revision == 0 || !valid_digest(&source.content_digest))
            || (self.source_roots.is_empty() && self.effective.is_none() && self.derived_from.is_empty() && self.gaps.is_empty())
            || self.effective.as_ref().is_some_and(|interval| {
                (interval.from.is_none() && interval.until.is_none())
                    || matches!((interval.from, interval.until), (Some(from), Some(until)) if from >= until)
            })
        {
            return Err(EvidenceMetadataError::Invalid);
        }
        let mut roots = BTreeSet::new();
        for root in &self.source_roots {
            if !valid_digest(&root.source_digest)
                || !roots.insert((&root.source_session, &root.source_entry))
            {
                return Err(EvidenceMetadataError::Invalid);
            }
        }
        Ok(())
    }
}

pub(crate) fn valid_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Error)]
pub enum EvidenceMetadataError {
    #[error("unsupported evidence causal metadata version")]
    UnsupportedVersion,
    #[error("invalid evidence causal metadata")]
    Invalid,
}
