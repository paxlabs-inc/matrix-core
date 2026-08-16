#![forbid(unsafe_code)]

use keith_agent_types::{EntityId, ProfileId, SessionId, UtcTimestamp};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct InitiativeSignals {
    pub urgency: u16,
    pub expected_value: u16,
    pub confidence: u16,
    pub interruption_cost: u16,
    pub resource_cost: u16,
    pub duplication_penalty: u16,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct InitiativeCandidate {
    pub id: EntityId,
    pub awareness_event_id: EntityId,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub channel: String,
    pub topic: String,
    pub proposed_action: String,
    pub signals: InitiativeSignals,
    pub created_at: UtcTimestamp,
    pub expires_at: UtcTimestamp,
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum CandidateError {
    #[error("initiative candidate text and channel must be non-empty")]
    Empty,
    #[error("initiative signals must be in the inclusive 0..=1000 range")]
    SignalRange,
    #[error("initiative candidate must expire after creation")]
    Expiry,
}

impl InitiativeCandidate {
    /// Validates bounded candidate facts before attention policy sees them.
    ///
    /// # Errors
    ///
    /// Returns an error for empty text, out-of-range signals, or invalid expiry.
    pub fn validate(&self) -> Result<(), CandidateError> {
        if self.channel.trim().is_empty()
            || self.topic.trim().is_empty()
            || self.proposed_action.trim().is_empty()
        {
            return Err(CandidateError::Empty);
        }
        let signals = [
            self.signals.urgency,
            self.signals.expected_value,
            self.signals.confidence,
            self.signals.interruption_cost,
            self.signals.resource_cost,
            self.signals.duplication_penalty,
        ];
        if signals.into_iter().any(|signal| signal > 1_000) {
            return Err(CandidateError::SignalRange);
        }
        if self.expires_at <= self.created_at {
            return Err(CandidateError::Expiry);
        }
        Ok(())
    }

    pub fn base_score(&self) -> i64 {
        i64::from(self.signals.urgency)
            .saturating_mul(3)
            .saturating_add(i64::from(self.signals.expected_value).saturating_mul(3))
            .saturating_add(i64::from(self.signals.confidence).saturating_mul(2))
            .saturating_sub(i64::from(self.signals.interruption_cost).saturating_mul(2))
            .saturating_sub(i64::from(self.signals.resource_cost))
            .saturating_sub(i64::from(self.signals.duplication_penalty).saturating_mul(2))
            / 8
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn score_rewards_value_and_penalizes_interruption() {
        let valuable = InitiativeSignals {
            urgency: 900,
            expected_value: 900,
            confidence: 900,
            interruption_cost: 50,
            resource_cost: 50,
            duplication_penalty: 0,
        };
        let costly = InitiativeSignals {
            interruption_cost: 1_000,
            resource_cost: 1_000,
            duplication_penalty: 1_000,
            ..valuable
        };
        let candidate = |signals| InitiativeCandidate {
            id: EntityId::new(),
            awareness_event_id: EntityId::new(),
            profile_id: ProfileId::new(),
            session_id: SessionId::new(),
            channel: "desktop".to_owned(),
            topic: "deadline".to_owned(),
            proposed_action: "prepare response".to_owned(),
            signals,
            created_at: UtcTimestamp::UNIX_EPOCH,
            expires_at: UtcTimestamp::from_unix_millis(1),
        };
        assert!(candidate(valuable).base_score() > candidate(costly).base_score());
    }
}
