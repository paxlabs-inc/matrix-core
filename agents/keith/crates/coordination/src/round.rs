use std::collections::BTreeSet;
use std::fmt::Display;

use keith_agent_types::{
    AuditId, CURRENT_SCHEMA_VERSION, ConversationId, DeliveryId, EntityId, EventId, ProfileId,
    Revision, RoundId, StableKey, UtcTimestamp,
};
use keith_state_store_core::{
    Collection, RecordMutation, StateRecordRepository, VersionedRecord, WritePrecondition,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    AuditAction, CollaborationRound, ConversationDelivery, CoordinationAuditRecord, DeliveryState,
    MentionPolicy, RoundState, SupersessionTarget, TargetedSupersession,
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RoundCoordinatorConfig {
    pub max_open_rounds: usize,
    pub max_open_rounds_per_conversation: usize,
    pub max_active_deliveries: usize,
    pub max_depth: u16,
    pub max_turns: u32,
}

impl Default for RoundCoordinatorConfig {
    fn default() -> Self {
        Self {
            max_open_rounds: 128,
            max_open_rounds_per_conversation: 8,
            max_active_deliveries: 256,
            max_depth: 16,
            max_turns: 256,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoundTrigger {
    pub stable_key: StableKey,
    pub conversation_id: ConversationId,
    pub trigger_event_id: EventId,
    pub coordinator_profile_id: ProfileId,
    pub eligible_participants: BTreeSet<ProfileId>,
    pub mention_policy: MentionPolicy,
    pub max_depth: u16,
    pub max_turns: u32,
    pub triggered_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoundTransition {
    pub stable_key: StableKey,
    pub round_id: RoundId,
    pub actor_profile_id: ProfileId,
    pub expected_revision: Revision,
    pub occurred_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoundBranchCancellation {
    pub transition: RoundTransition,
    pub delivery_id: DeliveryId,
    pub superseded_by_event_id: EventId,
    pub reason: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RoundMutationStatus {
    Applied,
    Duplicate,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoundMutationReceipt {
    pub status: RoundMutationStatus,
    pub round: CollaborationRound,
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum RoundCoordinatorError {
    #[error("round repository failed: {0}")]
    Repository(String),
    #[error("round was not found")]
    NotFound,
    #[error("round revision is stale")]
    StaleRevision,
    #[error("round transition is invalid: {0}")]
    Invalid(&'static str),
    #[error("round concurrency or budget limit was reached")]
    Limit,
}

pub struct RoundCoordinator<R> {
    repository: R,
    config: RoundCoordinatorConfig,
}

impl<R> RoundCoordinator<R>
where
    R: StateRecordRepository,
    R::Error: Display,
{
    pub fn new(
        repository: R,
        config: RoundCoordinatorConfig,
    ) -> Result<Self, RoundCoordinatorError> {
        if config.max_open_rounds == 0
            || config.max_open_rounds_per_conversation == 0
            || config.max_active_deliveries == 0
            || config.max_depth == 0
            || config.max_turns == 0
        {
            return Err(RoundCoordinatorError::Invalid("zero coordinator bound"));
        }
        Ok(Self { repository, config })
    }

    pub fn trigger(
        &self,
        trigger: &RoundTrigger,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        if trigger.eligible_participants.is_empty()
            || trigger.max_depth == 0
            || trigger.max_depth > self.config.max_depth
            || trigger.max_turns == 0
            || trigger.max_turns > self.config.max_turns
            || !trigger
                .eligible_participants
                .contains(&trigger.coordinator_profile_id)
        {
            return Err(RoundCoordinatorError::Invalid("round trigger"));
        }
        let round_id = RoundId::from(deterministic_id(
            "collaboration-round",
            trigger.stable_key.as_str(),
        ));
        if let Some(existing) = self.round(&round_id)? {
            if existing.stable_key == trigger.stable_key.as_str()
                && existing.conversation_id == trigger.conversation_id
                && existing.trigger_event_id == trigger.trigger_event_id
                && existing.eligible_participants == trigger.eligible_participants
                && existing.mention_policy == trigger.mention_policy
                && existing.max_depth == trigger.max_depth
                && existing.max_turns == trigger.max_turns
            {
                return Ok(RoundMutationReceipt {
                    status: RoundMutationStatus::Duplicate,
                    round: existing,
                });
            }
            return Err(RoundCoordinatorError::Invalid("round stable key collision"));
        }
        let open = self
            .rounds()?
            .into_iter()
            .filter(|round| !terminal(round.state))
            .collect::<Vec<_>>();
        if open.len() >= self.config.max_open_rounds
            || open
                .iter()
                .filter(|round| round.conversation_id == trigger.conversation_id)
                .count()
                >= self.config.max_open_rounds_per_conversation
        {
            return Err(RoundCoordinatorError::Limit);
        }
        let round = CollaborationRound {
            version: CURRENT_SCHEMA_VERSION,
            id: round_id,
            stable_key: trigger.stable_key.to_string(),
            conversation_id: trigger.conversation_id.clone(),
            trigger_event_id: trigger.trigger_event_id.clone(),
            eligible_participants: trigger.eligible_participants.clone(),
            mention_policy: trigger.mention_policy,
            state: RoundState::Open,
            max_depth: trigger.max_depth,
            remaining_depth: trigger.max_depth,
            max_turns: trigger.max_turns,
            remaining_turns: trigger.max_turns,
            active_deliveries: BTreeSet::new(),
            terminal_reason: None,
            revision: Revision::ZERO,
        };
        let audit = transition_audit(
            trigger.stable_key.clone(),
            &round,
            trigger.coordinator_profile_id.clone(),
            None,
            trigger.triggered_at,
            "round triggered",
        );
        self.commit_round(&round, WritePrecondition::Missing, audit)?;
        Ok(RoundMutationReceipt {
            status: RoundMutationStatus::Applied,
            round,
        })
    }

    pub fn eligible_participants(
        &self,
        round_id: &RoundId,
    ) -> Result<BTreeSet<ProfileId>, RoundCoordinatorError> {
        Ok(self
            .round(round_id)?
            .ok_or(RoundCoordinatorError::NotFound)?
            .eligible_participants)
    }

    pub fn activate_delivery(
        &self,
        transition: &RoundTransition,
        delivery_id: &DeliveryId,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        self.mutate(transition, |round| {
            if round.state != RoundState::Open || round.remaining_turns == 0 {
                return Err(RoundCoordinatorError::Limit);
            }
            if round.active_deliveries.len() >= self.config.max_active_deliveries {
                return Err(RoundCoordinatorError::Limit);
            }
            let delivery = self
                .delivery(delivery_id)?
                .ok_or(RoundCoordinatorError::NotFound)?;
            if delivery.conversation_id != round.conversation_id
                || !round
                    .eligible_participants
                    .contains(&delivery.destination_profile_id)
            {
                return Err(RoundCoordinatorError::Invalid(
                    "delivery is not eligible for round",
                ));
            }
            if !round.active_deliveries.insert(delivery_id.clone()) {
                return Err(RoundCoordinatorError::Invalid("delivery already active"));
            }
            round.remaining_turns -= 1;
            Ok("round delivery activated")
        })
    }

    pub fn settle_delivery(
        &self,
        transition: &RoundTransition,
        delivery_id: &DeliveryId,
        spawned_branch: bool,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        self.mutate(transition, |round| {
            if !round.active_deliveries.remove(delivery_id) {
                return Err(RoundCoordinatorError::Invalid("delivery is not active"));
            }
            if spawned_branch {
                if round.remaining_depth == 0 {
                    round.state = RoundState::BudgetClosed;
                    round.terminal_reason = Some("round depth budget exhausted".into());
                } else {
                    round.remaining_depth -= 1;
                }
            }
            if round.remaining_turns == 0 && round.active_deliveries.is_empty() {
                round.state = RoundState::BudgetClosed;
                round.terminal_reason = Some("round turn budget exhausted".into());
            }
            Ok("round delivery settled")
        })
    }

    pub fn quiet(
        &self,
        transition: &RoundTransition,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        self.change_state(transition, RoundState::Quiet, None, "round became quiet")
    }

    pub fn resume(
        &self,
        transition: &RoundTransition,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        self.change_state(transition, RoundState::Open, None, "round resumed")
    }

    pub fn blocked(
        &self,
        transition: &RoundTransition,
        reason: String,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        self.change_state(
            transition,
            RoundState::Blocked,
            Some(reason),
            "round blocked",
        )
    }

    pub fn converge(
        &self,
        transition: &RoundTransition,
        reason: String,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        self.change_state(
            transition,
            RoundState::Converged,
            Some(reason),
            "round converged",
        )
    }

    pub fn close_budget(
        &self,
        transition: &RoundTransition,
        reason: String,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        self.change_state(
            transition,
            RoundState::BudgetClosed,
            Some(reason),
            "round budget closed",
        )
    }

    pub fn cancel(
        &self,
        transition: &RoundTransition,
        reason: String,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        self.change_state(
            transition,
            RoundState::Cancelled,
            Some(reason),
            "round cancelled",
        )
    }

    pub fn cancel_branch(
        &self,
        cancellation: &RoundBranchCancellation,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        if let Some(replay) = self.replay(
            &cancellation.transition.stable_key,
            &cancellation.transition.round_id,
        )? {
            return Ok(replay);
        }
        let mut round = self.required_round(
            &cancellation.transition.round_id,
            cancellation.transition.expected_revision,
        )?;
        if terminal(round.state) || !round.active_deliveries.remove(&cancellation.delivery_id) {
            return Err(RoundCoordinatorError::Invalid("round branch is not active"));
        }
        let mut delivery = self
            .delivery(&cancellation.delivery_id)?
            .ok_or(RoundCoordinatorError::NotFound)?;
        if delivery.conversation_id != round.conversation_id
            || matches!(
                delivery.state,
                DeliveryState::Published | DeliveryState::Cancelled | DeliveryState::DeadLetter
            )
        {
            return Err(RoundCoordinatorError::Invalid(
                "delivery branch cannot be cancelled",
            ));
        }
        let prior_delivery_revision = delivery.revision;
        delivery.revision = next_revision(delivery.revision)?;
        delivery.state = DeliveryState::Superseded;
        delivery.claim = None;
        delivery.supersession = Some(TargetedSupersession {
            target: SupersessionTarget::RoundBranch {
                round_id: round.id.clone(),
                source_event_id: delivery.source_event_id.clone(),
            },
            superseded_by_event_id: cancellation.superseded_by_event_id.clone(),
            reason: cancellation.reason.clone(),
            occurred_at: cancellation.transition.occurred_at,
        });
        round.revision = next_revision(round.revision)?;
        let audit = transition_audit(
            cancellation.transition.stable_key.clone(),
            &round,
            cancellation.transition.actor_profile_id.clone(),
            Some(cancellation.transition.expected_revision),
            cancellation.transition.occurred_at,
            "round branch cancelled",
        );
        self.repository
            .transact(&[
                put_record(
                    Collection::CollaborationRounds,
                    round.id.as_entity_id(),
                    round.revision,
                    cancellation.transition.occurred_at,
                    &round,
                    WritePrecondition::Exact(cancellation.transition.expected_revision),
                )?,
                put_record(
                    Collection::ConversationDeliveries,
                    delivery.id.as_entity_id(),
                    delivery.revision,
                    cancellation.transition.occurred_at,
                    &delivery,
                    WritePrecondition::Exact(prior_delivery_revision),
                )?,
                audit_record(&audit)?,
            ])
            .map_err(repository_error)?;
        Ok(RoundMutationReceipt {
            status: RoundMutationStatus::Applied,
            round,
        })
    }

    fn change_state(
        &self,
        transition: &RoundTransition,
        next: RoundState,
        reason: Option<String>,
        detail: &'static str,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        self.mutate(transition, |round| {
            if terminal(round.state) || !round.active_deliveries.is_empty() {
                return Err(RoundCoordinatorError::Invalid(
                    "round has active or terminal branches",
                ));
            }
            if !legal_transition(round.state, next) {
                return Err(RoundCoordinatorError::Invalid("round state transition"));
            }
            if matches!(
                next,
                RoundState::Blocked
                    | RoundState::Converged
                    | RoundState::BudgetClosed
                    | RoundState::Cancelled
            ) && reason.as_deref().is_none_or(str::is_empty)
            {
                return Err(RoundCoordinatorError::Invalid("round reason is required"));
            }
            round.state = next;
            round.terminal_reason = reason;
            Ok(detail)
        })
    }

    fn mutate(
        &self,
        transition: &RoundTransition,
        operation: impl FnOnce(&mut CollaborationRound) -> Result<&'static str, RoundCoordinatorError>,
    ) -> Result<RoundMutationReceipt, RoundCoordinatorError> {
        if let Some(replay) = self.replay(&transition.stable_key, &transition.round_id)? {
            return Ok(replay);
        }
        let mut round = self.required_round(&transition.round_id, transition.expected_revision)?;
        let detail = operation(&mut round)?;
        round.revision = next_revision(round.revision)?;
        let audit = transition_audit(
            transition.stable_key.clone(),
            &round,
            transition.actor_profile_id.clone(),
            Some(transition.expected_revision),
            transition.occurred_at,
            detail,
        );
        self.commit_round(
            &round,
            WritePrecondition::Exact(transition.expected_revision),
            audit,
        )?;
        Ok(RoundMutationReceipt {
            status: RoundMutationStatus::Applied,
            round,
        })
    }

    fn commit_round(
        &self,
        round: &CollaborationRound,
        precondition: WritePrecondition,
        audit: CoordinationAuditRecord,
    ) -> Result<(), RoundCoordinatorError> {
        self.repository
            .transact(&[
                put_record(
                    Collection::CollaborationRounds,
                    round.id.as_entity_id(),
                    round.revision,
                    audit.occurred_at,
                    round,
                    precondition,
                )?,
                audit_record(&audit)?,
            ])
            .map_err(repository_error)?;
        Ok(())
    }

    fn replay(
        &self,
        stable_key: &StableKey,
        round_id: &RoundId,
    ) -> Result<Option<RoundMutationReceipt>, RoundCoordinatorError> {
        let audit_id = AuditId::from(deterministic_id("round-transition", stable_key.as_str()));
        let Some(record) = self
            .repository
            .get_record(Collection::TeammateAudits, audit_id.as_entity_id())
            .map_err(repository_error)?
        else {
            return Ok(None);
        };
        let audit: CoordinationAuditRecord = serde_json::from_value(record.payload)
            .map_err(|_| RoundCoordinatorError::Invalid("corrupt round transition audit"))?;
        if audit.stable_key != stable_key.as_str() || audit.subject_key != round_id.to_string() {
            return Err(RoundCoordinatorError::Invalid(
                "round transition key collision",
            ));
        }
        let round = self
            .round(round_id)?
            .ok_or(RoundCoordinatorError::NotFound)?;
        Ok(Some(RoundMutationReceipt {
            status: RoundMutationStatus::Duplicate,
            round,
        }))
    }

    fn required_round(
        &self,
        id: &RoundId,
        expected: Revision,
    ) -> Result<CollaborationRound, RoundCoordinatorError> {
        let round = self.round(id)?.ok_or(RoundCoordinatorError::NotFound)?;
        if round.revision != expected {
            return Err(RoundCoordinatorError::StaleRevision);
        }
        Ok(round)
    }

    pub fn round(&self, id: &RoundId) -> Result<Option<CollaborationRound>, RoundCoordinatorError> {
        self.repository
            .get_record(Collection::CollaborationRounds, id.as_entity_id())
            .map_err(repository_error)?
            .map(decode_record)
            .transpose()
    }

    fn rounds(&self) -> Result<Vec<CollaborationRound>, RoundCoordinatorError> {
        self.repository
            .list_records(Collection::CollaborationRounds)
            .map_err(repository_error)?
            .into_iter()
            .map(decode_record)
            .collect()
    }

    fn delivery(
        &self,
        id: &DeliveryId,
    ) -> Result<Option<ConversationDelivery>, RoundCoordinatorError> {
        self.repository
            .get_record(Collection::ConversationDeliveries, id.as_entity_id())
            .map_err(repository_error)?
            .map(decode_record)
            .transpose()
    }
}

fn terminal(state: RoundState) -> bool {
    matches!(
        state,
        RoundState::Converged | RoundState::BudgetClosed | RoundState::Cancelled
    )
}

fn legal_transition(previous: RoundState, next: RoundState) -> bool {
    previous == next
        || matches!(
            previous,
            RoundState::Open | RoundState::Quiet | RoundState::Blocked
        ) && matches!(
            next,
            RoundState::Open
                | RoundState::Quiet
                | RoundState::Blocked
                | RoundState::Converged
                | RoundState::BudgetClosed
                | RoundState::Cancelled
        )
}

fn next_revision(revision: Revision) -> Result<Revision, RoundCoordinatorError> {
    revision
        .checked_next()
        .ok_or(RoundCoordinatorError::Invalid("revision overflow"))
}

fn transition_audit(
    stable_key: StableKey,
    round: &CollaborationRound,
    actor_profile_id: ProfileId,
    expected_revision: Option<Revision>,
    occurred_at: UtcTimestamp,
    safe_detail: &'static str,
) -> CoordinationAuditRecord {
    CoordinationAuditRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: AuditId::from(deterministic_id("round-transition", stable_key.as_str())),
        stable_key: stable_key.to_string(),
        actor_profile_id,
        conversation_id: round.conversation_id.clone(),
        action: AuditAction::RoundRecorded,
        subject_key: round.id.to_string(),
        expected_revision,
        resulting_revision: round.revision,
        occurred_at,
        safe_detail: Some(safe_detail.into()),
        handoff_event_intent: None,
    }
}

fn audit_record(audit: &CoordinationAuditRecord) -> Result<RecordMutation, RoundCoordinatorError> {
    put_record(
        Collection::TeammateAudits,
        audit.id.as_entity_id(),
        audit.resulting_revision,
        audit.occurred_at,
        audit,
        WritePrecondition::Missing,
    )
}

fn put_record<T: Serialize>(
    collection: Collection,
    id: &EntityId,
    revision: Revision,
    now: UtcTimestamp,
    payload: &T,
    precondition: WritePrecondition,
) -> Result<RecordMutation, RoundCoordinatorError> {
    Ok(RecordMutation::Put {
        collection,
        record: VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: id.clone(),
            revision,
            updated_at: now,
            payload: serde_json::to_value(payload)
                .map_err(|_| RoundCoordinatorError::Invalid("round serialization"))?,
        },
        precondition,
    })
}

fn decode_record<T: for<'de> Deserialize<'de>>(
    record: VersionedRecord,
) -> Result<T, RoundCoordinatorError> {
    serde_json::from_value(record.payload)
        .map_err(|_| RoundCoordinatorError::Invalid("corrupt round record"))
}

fn deterministic_id(namespace: &str, key: &str) -> EntityId {
    let digest = Sha256::digest(format!("{namespace}\0{key}").as_bytes());
    let mut bytes = [0_u8; 16];
    bytes.copy_from_slice(&digest[..16]);
    EntityId::from_u128(u128::from_be_bytes(bytes))
}

fn repository_error(error: impl Display) -> RoundCoordinatorError {
    RoundCoordinatorError::Repository(error.to_string().chars().take(512).collect())
}
