use std::fmt::Display;

use keith_agent_types::{
    AssignmentId, AuditId, CURRENT_SCHEMA_VERSION, DeliveryId, EntityId, EventId, ProfileId,
    Revision, UtcTimestamp,
};
use keith_state_store_core::StateRecordRepository;
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{
    AssignmentClaim, AssignmentRecord, AssignmentState, AuditAction, CanonicalHandoffEventIntent,
    ConversationDelivery, CoordinationAuditRecord, CoordinationRepository, CoordinationWrite,
    DurableCoordinationRepository, OwnershipTransfer, SupersessionTarget, TargetedSupersession,
    WritePrecondition,
};

const MAX_BLOCK_REASON_BYTES: usize = 1_024;

pub trait AssignmentRepository {
    fn assignment_records(&self) -> Result<Vec<AssignmentRecord>, AssignmentServiceError>;
    fn assignment_audits(&self) -> Result<Vec<CoordinationAuditRecord>, AssignmentServiceError>;
    fn delivery_record(
        &self,
        id: &DeliveryId,
    ) -> Result<Option<ConversationDelivery>, AssignmentServiceError>;
    fn apply_assignment_writes(
        &mut self,
        writes: Vec<CoordinationWrite>,
    ) -> Result<(), AssignmentServiceError>;
}

impl AssignmentRepository for CoordinationRepository {
    fn assignment_records(&self) -> Result<Vec<AssignmentRecord>, AssignmentServiceError> {
        self.assignments().map_err(repository_error)
    }

    fn assignment_audits(&self) -> Result<Vec<CoordinationAuditRecord>, AssignmentServiceError> {
        self.audits().map_err(repository_error)
    }

    fn delivery_record(
        &self,
        id: &DeliveryId,
    ) -> Result<Option<ConversationDelivery>, AssignmentServiceError> {
        self.delivery(id).map_err(repository_error)
    }

    fn apply_assignment_writes(
        &mut self,
        writes: Vec<CoordinationWrite>,
    ) -> Result<(), AssignmentServiceError> {
        self.apply_atomic(writes).map_err(repository_error)
    }
}

impl<R> AssignmentRepository for DurableCoordinationRepository<R>
where
    R: StateRecordRepository,
    R::Error: Display,
{
    fn assignment_records(&self) -> Result<Vec<AssignmentRecord>, AssignmentServiceError> {
        self.assignments().map_err(repository_error)
    }

    fn assignment_audits(&self) -> Result<Vec<CoordinationAuditRecord>, AssignmentServiceError> {
        self.audits().map_err(repository_error)
    }

    fn delivery_record(
        &self,
        id: &DeliveryId,
    ) -> Result<Option<ConversationDelivery>, AssignmentServiceError> {
        self.delivery(id).map_err(repository_error)
    }

    fn apply_assignment_writes(
        &mut self,
        writes: Vec<CoordinationWrite>,
    ) -> Result<(), AssignmentServiceError> {
        self.apply_atomic(writes).map_err(repository_error)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AssignmentLease {
    pub assignment_id: AssignmentId,
    pub token: EntityId,
    pub claimant: ProfileId,
    pub fence: Revision,
    pub expires_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AssignmentHandoff {
    pub transfer: OwnershipTransfer,
    pub obsolete_delivery_id: DeliveryId,
    pub new_owner_delivery: ConversationDelivery,
    pub event_intent: CanonicalHandoffEventIntent,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AssignmentHandoffReceipt {
    pub assignment: AssignmentRecord,
    pub superseded_delivery: ConversationDelivery,
    pub new_owner_delivery: ConversationDelivery,
    pub event_intent: CanonicalHandoffEventIntent,
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum AssignmentServiceError {
    #[error("assignment repository failed: {0}")]
    Repository(String),
    #[error("assignment was not found")]
    NotFound,
    #[error("assignment revision is stale")]
    RevisionConflict,
    #[error("assignment dependency graph is not ready")]
    DependenciesNotReady,
    #[error("assignment lease is stale, expired, or owned by another profile")]
    LeaseLost,
    #[error("assignment transition is invalid")]
    InvalidTransition,
    #[error("assignment input is invalid: {0}")]
    Invalid(&'static str),
    #[error("assignment revision overflow")]
    RevisionOverflow,
}

pub struct AssignmentService<R> {
    repository: R,
}

impl<R: AssignmentRepository> AssignmentService<R> {
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    pub fn into_repository(self) -> R {
        self.repository
    }

    pub fn create(
        &mut self,
        mut assignment: AssignmentRecord,
    ) -> Result<AssignmentRecord, AssignmentServiceError> {
        if assignment.revision != Revision::ZERO
            || assignment.claim.is_some()
            || assignment.result_event_id.is_some()
            || !assignment.ownership_history.is_empty()
        {
            return Err(AssignmentServiceError::Invalid("non-initial assignment"));
        }
        let records = self.repository.assignment_records()?;
        ensure_dependencies_exist(&assignment, &records)?;
        assignment.state = if dependencies_complete(&assignment, &records) {
            AssignmentState::Ready
        } else {
            AssignmentState::Proposed
        };
        assignment.block_reason = None;
        self.repository
            .apply_assignment_writes(vec![CoordinationWrite::PutAssignment(
                assignment.clone(),
                WritePrecondition::Missing,
            )])?;
        Ok(assignment)
    }

    pub fn refresh_readiness(
        &mut self,
        id: &AssignmentId,
        expected_revision: Revision,
    ) -> Result<AssignmentRecord, AssignmentServiceError> {
        let records = self.repository.assignment_records()?;
        let mut assignment = required(&records, id)?;
        require_revision(&assignment, expected_revision)?;
        if matches!(
            assignment.state,
            AssignmentState::Claimed
                | AssignmentState::Active
                | AssignmentState::Completed
                | AssignmentState::Cancelled
        ) {
            return Err(AssignmentServiceError::InvalidTransition);
        }
        if dependencies_complete(&assignment, &records) {
            assignment.state = AssignmentState::Ready;
            assignment.block_reason = None;
        } else {
            assignment.state = AssignmentState::Blocked;
            assignment.block_reason = Some("waiting for assignment dependencies".into());
        }
        advance(&mut assignment)?;
        put_assignment(&mut self.repository, assignment, expected_revision)
    }

    pub fn claim(
        &mut self,
        id: &AssignmentId,
        expected_revision: Revision,
        claimant: ProfileId,
        expires_at: UtcTimestamp,
        now: UtcTimestamp,
    ) -> Result<AssignmentLease, AssignmentServiceError> {
        let records = self.repository.assignment_records()?;
        let mut assignment = required(&records, id)?;
        require_revision(&assignment, expected_revision)?;
        if !matches!(
            assignment.state,
            AssignmentState::Ready | AssignmentState::Transferred
        ) || claimant != assignment.owner_profile_id
            || expires_at <= now
            || !dependencies_complete(&assignment, &records)
        {
            return Err(AssignmentServiceError::DependenciesNotReady);
        }
        let token = EntityId::new();
        assignment.state = AssignmentState::Claimed;
        assignment.claim = Some(AssignmentClaim {
            token: token.clone(),
            claimant: claimant.clone(),
            expires_at,
        });
        assignment.block_reason = None;
        advance(&mut assignment)?;
        let fence = assignment.revision;
        let assignment_id = assignment.id.clone();
        put_assignment(&mut self.repository, assignment, expected_revision)?;
        Ok(AssignmentLease {
            assignment_id,
            token,
            claimant,
            fence,
            expires_at,
        })
    }

    pub fn activate(
        &mut self,
        lease: &AssignmentLease,
        now: UtcTimestamp,
    ) -> Result<AssignmentRecord, AssignmentServiceError> {
        self.transition_claimed(lease, now, AssignmentState::Active, None, None)
    }

    pub fn block(
        &mut self,
        lease: &AssignmentLease,
        reason: String,
        now: UtcTimestamp,
    ) -> Result<AssignmentRecord, AssignmentServiceError> {
        if reason.trim().is_empty() || reason.len() > MAX_BLOCK_REASON_BYTES {
            return Err(AssignmentServiceError::Invalid("block reason"));
        }
        self.transition_claimed(lease, now, AssignmentState::Blocked, Some(reason), None)
    }

    pub fn complete(
        &mut self,
        lease: &AssignmentLease,
        result_event_id: EventId,
        now: UtcTimestamp,
    ) -> Result<AssignmentRecord, AssignmentServiceError> {
        self.transition_claimed(
            lease,
            now,
            AssignmentState::Completed,
            None,
            Some(result_event_id),
        )
    }

    pub fn cancel(
        &mut self,
        id: &AssignmentId,
        expected_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<AssignmentRecord, AssignmentServiceError> {
        let records = self.repository.assignment_records()?;
        let mut assignment = required(&records, id)?;
        require_revision(&assignment, expected_revision)?;
        if matches!(
            assignment.state,
            AssignmentState::Completed | AssignmentState::Cancelled
        ) {
            return Err(AssignmentServiceError::InvalidTransition);
        }
        assignment.state = AssignmentState::Cancelled;
        assignment.claim = None;
        assignment.block_reason = None;
        assignment.result_event_id = None;
        advance(&mut assignment)?;
        put_assignment(&mut self.repository, assignment, expected_revision)
    }

    pub fn handoff(
        &mut self,
        assignment_id: &AssignmentId,
        request: AssignmentHandoff,
    ) -> Result<AssignmentHandoffReceipt, AssignmentServiceError> {
        let records = self.repository.assignment_records()?;
        if let Some(persisted) = self
            .repository
            .assignment_audits()?
            .into_iter()
            .filter_map(|audit| audit.handoff_event_intent)
            .find(|intent| intent.stable_key == request.event_intent.stable_key)
        {
            if persisted != request.event_intent {
                return Err(AssignmentServiceError::Invalid("handoff replay conflict"));
            }
            let assignment = required(&records, assignment_id)?;
            let superseded_delivery = self
                .repository
                .delivery_record(&request.obsolete_delivery_id)?
                .ok_or(AssignmentServiceError::NotFound)?;
            let new_owner_delivery = self
                .repository
                .delivery_record(&request.new_owner_delivery.id)?
                .ok_or(AssignmentServiceError::NotFound)?;
            return Ok(AssignmentHandoffReceipt {
                assignment,
                superseded_delivery,
                new_owner_delivery,
                event_intent: persisted,
            });
        }
        let mut assignment = required(&records, assignment_id)?;
        require_revision(&assignment, request.transfer.expected_revision)?;
        if assignment.owner_profile_id != request.transfer.from_profile_id
            || assignment.conversation_id != request.event_intent.conversation_id
            || request.event_intent.assignment_id != *assignment_id
            || request.event_intent.from_profile_id != request.transfer.from_profile_id
            || request.event_intent.to_profile_id != request.transfer.to_profile_id
            || request.event_intent.source_event_id != request.transfer.source_event_id
            || request.event_intent.occurred_at != request.transfer.occurred_at
            || request.transfer.from_profile_id == request.transfer.to_profile_id
        {
            return Err(AssignmentServiceError::Invalid(
                "ownership transfer binding",
            ));
        }
        let mut obsolete = self
            .repository
            .delivery_record(&request.obsolete_delivery_id)?
            .ok_or(AssignmentServiceError::NotFound)?;
        if obsolete.destination_profile_id != request.transfer.from_profile_id
            || obsolete.conversation_id != assignment.conversation_id
            || request.new_owner_delivery.destination_profile_id != request.transfer.to_profile_id
            || request.new_owner_delivery.conversation_id != assignment.conversation_id
            || request.new_owner_delivery.source_event_id != request.event_intent.event_id
        {
            return Err(AssignmentServiceError::Invalid("handoff delivery binding"));
        }
        assignment.owner_profile_id = request.transfer.to_profile_id.clone();
        assignment.state = AssignmentState::Transferred;
        assignment.claim = None;
        assignment.block_reason = None;
        assignment.ownership_history.push(request.transfer.clone());
        advance(&mut assignment)?;
        if request.event_intent.ownership_revision != assignment.revision {
            return Err(AssignmentServiceError::Invalid("handoff event revision"));
        }
        let obsolete_revision = obsolete.revision;
        obsolete.revision = next_revision(obsolete.revision)?;
        obsolete.state = crate::DeliveryState::Superseded;
        obsolete.claim = None;
        obsolete.retry_at = None;
        obsolete.safe_error = None;
        obsolete.supersession = Some(TargetedSupersession {
            target: SupersessionTarget::Assignment {
                assignment_id: assignment_id.clone(),
            },
            superseded_by_event_id: request.event_intent.event_id.clone(),
            reason: "assignment ownership transferred".into(),
            occurred_at: request.transfer.occurred_at,
        });
        let audit = CoordinationAuditRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: AuditId::new(),
            stable_key: request.event_intent.stable_key.clone(),
            actor_profile_id: request.transfer.actor_profile_id.clone(),
            conversation_id: assignment.conversation_id.clone(),
            action: AuditAction::OwnershipTransferred,
            subject_key: assignment.stable_key.clone(),
            expected_revision: Some(request.transfer.expected_revision),
            resulting_revision: assignment.revision,
            occurred_at: request.transfer.occurred_at,
            safe_detail: Some("canonical assignment handoff event pending publication".into()),
            handoff_event_intent: Some(request.event_intent.clone()),
        };
        self.repository.apply_assignment_writes(vec![
            CoordinationWrite::PutAssignment(
                assignment.clone(),
                WritePrecondition::Revision(request.transfer.expected_revision),
            ),
            CoordinationWrite::PutDelivery(
                obsolete.clone(),
                WritePrecondition::Revision(obsolete_revision),
            ),
            CoordinationWrite::PutDelivery(
                request.new_owner_delivery.clone(),
                WritePrecondition::Missing,
            ),
            CoordinationWrite::AppendAudit(audit),
        ])?;
        Ok(AssignmentHandoffReceipt {
            assignment,
            superseded_delivery: obsolete,
            new_owner_delivery: request.new_owner_delivery,
            event_intent: request.event_intent,
        })
    }

    pub fn recover_handoff_event_intents(
        &self,
    ) -> Result<Vec<CanonicalHandoffEventIntent>, AssignmentServiceError> {
        let mut intents = self
            .repository
            .assignment_audits()?
            .into_iter()
            .filter_map(|audit| audit.handoff_event_intent)
            .collect::<Vec<_>>();
        intents.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        Ok(intents)
    }

    fn transition_claimed(
        &mut self,
        lease: &AssignmentLease,
        now: UtcTimestamp,
        state: AssignmentState,
        block_reason: Option<String>,
        result_event_id: Option<EventId>,
    ) -> Result<AssignmentRecord, AssignmentServiceError> {
        let records = self.repository.assignment_records()?;
        let mut assignment = required(&records, &lease.assignment_id)?;
        verify_lease(&assignment, lease, now)?;
        if state == AssignmentState::Active && assignment.state != AssignmentState::Claimed
            || matches!(state, AssignmentState::Blocked | AssignmentState::Completed)
                && !matches!(
                    assignment.state,
                    AssignmentState::Claimed | AssignmentState::Active
                )
        {
            return Err(AssignmentServiceError::InvalidTransition);
        }
        let expected = assignment.revision;
        assignment.state = state;
        assignment.block_reason = block_reason;
        assignment.result_event_id = result_event_id;
        if state != AssignmentState::Active {
            assignment.claim = None;
        }
        advance(&mut assignment)?;
        put_assignment(&mut self.repository, assignment, expected)
    }
}

fn required(
    records: &[AssignmentRecord],
    id: &AssignmentId,
) -> Result<AssignmentRecord, AssignmentServiceError> {
    records
        .iter()
        .find(|record| &record.id == id)
        .cloned()
        .ok_or(AssignmentServiceError::NotFound)
}

fn require_revision(
    assignment: &AssignmentRecord,
    expected: Revision,
) -> Result<(), AssignmentServiceError> {
    (assignment.revision == expected)
        .then_some(())
        .ok_or(AssignmentServiceError::RevisionConflict)
}

fn ensure_dependencies_exist(
    assignment: &AssignmentRecord,
    records: &[AssignmentRecord],
) -> Result<(), AssignmentServiceError> {
    if assignment.dependencies.contains(&assignment.id)
        || assignment
            .dependencies
            .iter()
            .any(|dependency| !records.iter().any(|candidate| &candidate.id == dependency))
    {
        return Err(AssignmentServiceError::Invalid("assignment dependency"));
    }
    Ok(())
}

fn dependencies_complete(assignment: &AssignmentRecord, records: &[AssignmentRecord]) -> bool {
    assignment.dependencies.iter().all(|dependency| {
        records.iter().any(|candidate| {
            &candidate.id == dependency && candidate.state == AssignmentState::Completed
        })
    })
}

fn verify_lease(
    assignment: &AssignmentRecord,
    lease: &AssignmentLease,
    now: UtcTimestamp,
) -> Result<(), AssignmentServiceError> {
    let valid = assignment.revision == lease.fence
        && assignment.id == lease.assignment_id
        && assignment.owner_profile_id == lease.claimant
        && lease.expires_at > now
        && assignment.claim.as_ref().is_some_and(|claim| {
            claim.token == lease.token
                && claim.claimant == lease.claimant
                && claim.expires_at == lease.expires_at
        });
    valid.then_some(()).ok_or(AssignmentServiceError::LeaseLost)
}

fn advance(assignment: &mut AssignmentRecord) -> Result<(), AssignmentServiceError> {
    assignment.revision = next_revision(assignment.revision)?;
    Ok(())
}

fn next_revision(revision: Revision) -> Result<Revision, AssignmentServiceError> {
    revision
        .checked_next()
        .ok_or(AssignmentServiceError::RevisionOverflow)
}

fn put_assignment<R: AssignmentRepository>(
    repository: &mut R,
    assignment: AssignmentRecord,
    expected: Revision,
) -> Result<AssignmentRecord, AssignmentServiceError> {
    repository.apply_assignment_writes(vec![CoordinationWrite::PutAssignment(
        assignment.clone(),
        WritePrecondition::Revision(expected),
    )])?;
    Ok(assignment)
}

fn repository_error(error: impl Display) -> AssignmentServiceError {
    AssignmentServiceError::Repository(error.to_string())
}
