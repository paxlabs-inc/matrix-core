use std::collections::BTreeMap;

use keith_agent_types::{EntityId, ProfileId, SessionId};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MemoryScoutPurpose {
    EvidenceRecall,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MemoryScoutCapability {
    MemoryRead,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryScoutLimits {
    pub max_depth: u16,
    pub max_children: u16,
    pub max_total_scouts: u32,
    pub max_concurrency: u16,
    pub max_evidence_per_scout: usize,
    pub max_claims: usize,
    pub max_tokens: u64,
    pub max_result_bytes: usize,
    pub max_runtime_ms: u64,
}

impl Default for MemoryScoutLimits {
    fn default() -> Self {
        Self {
            max_depth: 3,
            max_children: 4,
            max_total_scouts: 16,
            max_concurrency: 4,
            max_evidence_per_scout: 12,
            max_claims: 48,
            max_tokens: 8_000,
            max_result_bytes: 48 * 1_024,
            max_runtime_ms: 15_000,
        }
    }
}

impl MemoryScoutLimits {
    /// Validates every hard recursion and output ceiling.
    ///
    /// # Errors
    ///
    /// Returns an error when a ceiling is zero or permits impossible concurrency.
    pub fn validate(self) -> Result<(), MemoryScoutContractError> {
        if self.max_depth == 0
            || self.max_children == 0
            || self.max_total_scouts == 0
            || self.max_concurrency == 0
            || self.max_evidence_per_scout == 0
            || self.max_claims == 0
            || self.max_tokens == 0
            || self.max_result_bytes == 0
            || self.max_runtime_ms == 0
            || u32::from(self.max_concurrency) > self.max_total_scouts
        {
            return Err(MemoryScoutContractError::InvalidLimits);
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryScoutScopeManifest {
    pub profile_id: ProfileId,
    pub calling_session_id: SessionId,
    pub archive_revision: u64,
    pub evidence_digests: BTreeMap<EntityId, String>,
    pub sensitivity_ceiling: String,
    pub selector_version: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryScoutSpec {
    pub purpose: MemoryScoutPurpose,
    pub capability: MemoryScoutCapability,
    pub scope: MemoryScoutScopeManifest,
    pub limits: MemoryScoutLimits,
}

impl MemoryScoutSpec {
    /// Creates a memory-only child contract. No ordinary tool set is accepted by construction.
    ///
    /// # Errors
    ///
    /// Returns an error for an empty evidence scope or invalid bounds.
    pub fn new(
        scope: MemoryScoutScopeManifest,
        limits: MemoryScoutLimits,
    ) -> Result<Self, MemoryScoutContractError> {
        limits.validate()?;
        if scope.evidence_digests.is_empty()
            || scope.sensitivity_ceiling.trim().is_empty()
            || scope.selector_version.trim().is_empty()
            || scope
                .evidence_digests
                .values()
                .any(|digest| digest.trim().is_empty())
        {
            return Err(MemoryScoutContractError::InvalidScope);
        }
        Ok(Self {
            purpose: MemoryScoutPurpose::EvidenceRecall,
            capability: MemoryScoutCapability::MemoryRead,
            scope,
            limits,
        })
    }

    #[must_use]
    pub const fn can_mutate_memory(&self) -> bool {
        false
    }

    #[must_use]
    pub const fn can_access_workspace(&self) -> bool {
        false
    }

    #[must_use]
    pub const fn can_message_or_deliver(&self) -> bool {
        false
    }
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum MemoryScoutContractError {
    #[error("memory scout limits are invalid")]
    InvalidLimits,
    #[error("memory scout scope is empty or malformed")]
    InvalidScope,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn scout_contract_has_exactly_memory_read_authority() {
        let spec = MemoryScoutSpec::new(
            MemoryScoutScopeManifest {
                profile_id: ProfileId::new(),
                calling_session_id: SessionId::new(),
                archive_revision: 3,
                evidence_digests: BTreeMap::from([(EntityId::new(), "digest".into())]),
                sensitivity_ceiling: "personal".into(),
                selector_version: "memory-scout-v1".into(),
            },
            MemoryScoutLimits::default(),
        )
        .unwrap();
        assert_eq!(spec.capability, MemoryScoutCapability::MemoryRead);
        assert!(!spec.can_mutate_memory());
        assert!(!spec.can_access_workspace());
        assert!(!spec.can_message_or_deliver());
    }
}
