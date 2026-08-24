use std::collections::{BTreeMap, BTreeSet};

use keith_agent_types::{
    ConversationId, EntityId, EventId, ProfileId, Revision, SessionId, StableKey, UtcTimestamp,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

const MAX_MESSAGE_BYTES: usize = 128 * 1024;
const MAX_OBJECTIVE_BYTES: usize = 64 * 1024;
const MAX_REPORT_BYTES: usize = 64 * 1024;
const MAX_REASON_BYTES: usize = 8 * 1024;
const MAX_SAFE_ERROR_BYTES: usize = 2 * 1024;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerToolOperation {
    MessageAgent,
    AssignWork,
    HandoffWork,
    ReportAssignment,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerEffect {
    AppendConversationEvent,
    EnqueueRecipientAction,
    CreateAssignment,
    TransferAssignment,
    ReportAssignmentState,
    UseSandbox,
    UseComputer,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AuthoritySource {
    Installation,
    SenderProfile,
    ReceiverProfile,
    Conversation,
    Assignment,
    Grant,
    Sandbox,
    Computer,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AuthorityStage {
    Enqueue,
    Execution,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AuthorityEvidence {
    pub source: AuthoritySource,
    pub subject_revision: Revision,
    pub policy_digest_sha256: String,
    pub allowed_operations: BTreeSet<PeerToolOperation>,
    pub allowed_effects: BTreeSet<PeerEffect>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerAuthorityInputs {
    pub stage: AuthorityStage,
    pub observation_digest_sha256: String,
    pub evidence: Vec<AuthorityEvidence>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EffectivePeerAuthority {
    pub stage: AuthorityStage,
    pub operation: PeerToolOperation,
    pub required_effects: BTreeSet<PeerEffect>,
    pub observation_digest_sha256: String,
    pub source_revisions: BTreeMap<AuthoritySource, Revision>,
    pub source_policy_digests: BTreeMap<AuthoritySource, String>,
}

impl EffectivePeerAuthority {
    pub fn intersect(
        inputs: PeerAuthorityInputs,
        operation: PeerToolOperation,
        required_effects: BTreeSet<PeerEffect>,
    ) -> Result<Self, PeerToolError> {
        validate_digest(&inputs.observation_digest_sha256)?;
        let required_sources = BTreeSet::from([
            AuthoritySource::Installation,
            AuthoritySource::SenderProfile,
            AuthoritySource::ReceiverProfile,
            AuthoritySource::Conversation,
            AuthoritySource::Assignment,
            AuthoritySource::Grant,
            AuthoritySource::Sandbox,
            AuthoritySource::Computer,
        ]);
        let mut observed_sources = BTreeSet::new();
        let mut source_revisions = BTreeMap::new();
        let mut source_policy_digests = BTreeMap::new();
        for evidence in inputs.evidence {
            if !observed_sources.insert(evidence.source) {
                return Err(PeerToolError::InvalidAuthority(
                    "duplicate authority source",
                ));
            }
            validate_digest(&evidence.policy_digest_sha256)?;
            if !evidence.allowed_operations.contains(&operation) {
                return Err(PeerToolError::Denied {
                    authority_source: evidence.source,
                });
            }
            if !required_effects.is_subset(&evidence.allowed_effects) {
                return Err(PeerToolError::Denied {
                    authority_source: evidence.source,
                });
            }
            source_revisions.insert(evidence.source, evidence.subject_revision);
            source_policy_digests.insert(evidence.source, evidence.policy_digest_sha256);
        }
        if observed_sources != required_sources {
            return Err(PeerToolError::InvalidAuthority(
                "authority intersection is incomplete",
            ));
        }
        Ok(Self {
            stage: inputs.stage,
            operation,
            required_effects,
            observation_digest_sha256: inputs.observation_digest_sha256,
            source_revisions,
            source_policy_digests,
        })
    }

    pub fn execution_is_no_wider_than(&self, enqueue: &Self) -> bool {
        self.stage == AuthorityStage::Execution
            && enqueue.stage == AuthorityStage::Enqueue
            && self.operation == enqueue.operation
            && self.required_effects == enqueue.required_effects
            && self.source_revisions.iter().all(|(source, revision)| {
                enqueue
                    .source_revisions
                    .get(source)
                    .is_some_and(|observed| revision >= observed)
            })
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerToolContext {
    pub operation_key: StableKey,
    pub action_id: EntityId,
    pub authenticated_profile_id: ProfileId,
    pub authenticated_session_id: SessionId,
    pub conversation_id: ConversationId,
    pub source_event_id: EventId,
    pub expected_conversation_revision: Revision,
    pub expected_policy_revision: Revision,
    pub deadline: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MessageAgentInput {
    pub recipient_profile_id: ProfileId,
    pub recipient_session_id: SessionId,
    pub content: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AssignWorkInput {
    pub assignment_id: EntityId,
    pub owner_profile_id: ProfileId,
    pub objective: String,
    pub dependency_ids: BTreeSet<EntityId>,
    pub priority: u8,
    pub due_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HandoffWorkInput {
    pub assignment_id: EntityId,
    pub expected_assignment_revision: Revision,
    pub new_owner_profile_id: ProfileId,
    pub reason: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AssignmentReportKind {
    Active,
    Blocked,
    Completed,
    Failed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReportAssignmentInput {
    pub assignment_id: EntityId,
    pub expected_assignment_revision: Revision,
    pub kind: AssignmentReportKind,
    pub summary: String,
    pub result_event_id: Option<EventId>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerToolReceiptStatus {
    Applied,
    Duplicate,
    Denied,
    DurableFailure,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerToolReceipt {
    pub status: PeerToolReceiptStatus,
    pub operation: PeerToolOperation,
    pub operation_key: StableKey,
    pub action_id: EntityId,
    pub conversation_id: ConversationId,
    pub source_event_id: EventId,
    pub actor_profile_id: ProfileId,
    pub destination_profile_id: Option<ProfileId>,
    pub assignment_id: Option<EntityId>,
    pub delivery_id: Option<EntityId>,
    pub resulting_conversation_revision: Option<Revision>,
    pub resulting_assignment_revision: Option<Revision>,
    pub enqueue_authority_digest_sha256: String,
    pub execution_authority_digest_sha256: Option<String>,
    pub safe_reason: Option<String>,
    pub recorded_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(bound(serialize = "T: Serialize", deserialize = "T: Deserialize<'de>"))]
#[serde(deny_unknown_fields)]
pub struct AuthorizedPeerToolEnvelope<T> {
    pub context: PeerToolContext,
    pub input: T,
    pub enqueue_authority: EffectivePeerAuthority,
}

pub trait PeerAuthorityResolver {
    fn resolve(
        &self,
        stage: AuthorityStage,
        context: &PeerToolContext,
        operation: PeerToolOperation,
        required_effects: &BTreeSet<PeerEffect>,
    ) -> Result<PeerAuthorityInputs, PeerToolError>;
}

pub trait PeerToolBackend {
    fn enqueue_message(
        &self,
        request: AuthorizedPeerToolEnvelope<MessageAgentInput>,
    ) -> Result<PeerToolReceipt, PeerToolError>;

    fn enqueue_assignment(
        &self,
        request: AuthorizedPeerToolEnvelope<AssignWorkInput>,
    ) -> Result<PeerToolReceipt, PeerToolError>;

    fn enqueue_handoff(
        &self,
        request: AuthorizedPeerToolEnvelope<HandoffWorkInput>,
    ) -> Result<PeerToolReceipt, PeerToolError>;

    fn enqueue_report(
        &self,
        request: AuthorizedPeerToolEnvelope<ReportAssignmentInput>,
    ) -> Result<PeerToolReceipt, PeerToolError>;

    fn record_denial(
        &self,
        context: &PeerToolContext,
        operation: PeerToolOperation,
        safe_reason: &str,
    ) -> Result<PeerToolReceipt, PeerToolError>;
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct MessageAgentTool;

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct AssignWorkTool;

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct HandoffWorkTool;

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct ReportAssignmentTool;

impl MessageAgentTool {
    pub fn invoke<A: PeerAuthorityResolver, B: PeerToolBackend>(
        &self,
        context: PeerToolContext,
        input: MessageAgentInput,
        authority: &A,
        backend: &B,
    ) -> Result<PeerToolReceipt, PeerToolError> {
        validate_text("message", &input.content, MAX_MESSAGE_BYTES)?;
        if input.recipient_profile_id == context.authenticated_profile_id {
            return Err(PeerToolError::InvalidInput(
                "recipient must be another profile",
            ));
        }
        authorize_and_enqueue(
            context,
            input,
            PeerToolOperation::MessageAgent,
            BTreeSet::from([
                PeerEffect::AppendConversationEvent,
                PeerEffect::EnqueueRecipientAction,
            ]),
            authority,
            backend,
            PeerToolBackend::enqueue_message,
        )
    }
}

impl AssignWorkTool {
    pub fn invoke<A: PeerAuthorityResolver, B: PeerToolBackend>(
        &self,
        context: PeerToolContext,
        input: AssignWorkInput,
        authority: &A,
        backend: &B,
    ) -> Result<PeerToolReceipt, PeerToolError> {
        validate_text(
            "assignment objective",
            &input.objective,
            MAX_OBJECTIVE_BYTES,
        )?;
        authorize_and_enqueue(
            context,
            input,
            PeerToolOperation::AssignWork,
            BTreeSet::from([
                PeerEffect::CreateAssignment,
                PeerEffect::AppendConversationEvent,
                PeerEffect::EnqueueRecipientAction,
            ]),
            authority,
            backend,
            PeerToolBackend::enqueue_assignment,
        )
    }
}

impl HandoffWorkTool {
    pub fn invoke<A: PeerAuthorityResolver, B: PeerToolBackend>(
        &self,
        context: PeerToolContext,
        input: HandoffWorkInput,
        authority: &A,
        backend: &B,
    ) -> Result<PeerToolReceipt, PeerToolError> {
        validate_text("handoff reason", &input.reason, MAX_REASON_BYTES)?;
        authorize_and_enqueue(
            context,
            input,
            PeerToolOperation::HandoffWork,
            BTreeSet::from([
                PeerEffect::TransferAssignment,
                PeerEffect::AppendConversationEvent,
                PeerEffect::EnqueueRecipientAction,
            ]),
            authority,
            backend,
            PeerToolBackend::enqueue_handoff,
        )
    }
}

impl ReportAssignmentTool {
    pub fn invoke<A: PeerAuthorityResolver, B: PeerToolBackend>(
        &self,
        context: PeerToolContext,
        input: ReportAssignmentInput,
        authority: &A,
        backend: &B,
    ) -> Result<PeerToolReceipt, PeerToolError> {
        validate_text("assignment report", &input.summary, MAX_REPORT_BYTES)?;
        authorize_and_enqueue(
            context,
            input,
            PeerToolOperation::ReportAssignment,
            BTreeSet::from([
                PeerEffect::ReportAssignmentState,
                PeerEffect::AppendConversationEvent,
            ]),
            authority,
            backend,
            PeerToolBackend::enqueue_report,
        )
    }
}

pub fn authorize_peer_tool_execution<A: PeerAuthorityResolver>(
    context: &PeerToolContext,
    enqueue_authority: &EffectivePeerAuthority,
    authority: &A,
) -> Result<EffectivePeerAuthority, PeerToolError> {
    let inputs = authority.resolve(
        AuthorityStage::Execution,
        context,
        enqueue_authority.operation,
        &enqueue_authority.required_effects,
    )?;
    let execution = EffectivePeerAuthority::intersect(
        inputs,
        enqueue_authority.operation,
        enqueue_authority.required_effects.clone(),
    )?;
    if !execution.execution_is_no_wider_than(enqueue_authority) {
        return Err(PeerToolError::StaleAuthority);
    }
    Ok(execution)
}

fn authorize_and_enqueue<T, A, B>(
    context: PeerToolContext,
    input: T,
    operation: PeerToolOperation,
    required_effects: BTreeSet<PeerEffect>,
    authority: &A,
    backend: &B,
    enqueue: fn(&B, AuthorizedPeerToolEnvelope<T>) -> Result<PeerToolReceipt, PeerToolError>,
) -> Result<PeerToolReceipt, PeerToolError>
where
    A: PeerAuthorityResolver,
    B: PeerToolBackend,
{
    let inputs = match authority.resolve(
        AuthorityStage::Enqueue,
        &context,
        operation,
        &required_effects,
    ) {
        Ok(inputs) => inputs,
        Err(PeerToolError::Denied { authority_source }) => {
            return backend.record_denial(
                &context,
                operation,
                &format!("denied by {authority_source:?}"),
            );
        }
        Err(error) => return Err(error),
    };
    let enqueue_authority =
        match EffectivePeerAuthority::intersect(inputs, operation, required_effects) {
            Ok(authority) => authority,
            Err(PeerToolError::Denied { authority_source }) => {
                return backend.record_denial(
                    &context,
                    operation,
                    &format!("denied by {authority_source:?}"),
                );
            }
            Err(error) => return Err(error),
        };
    enqueue(
        backend,
        AuthorizedPeerToolEnvelope {
            context,
            input,
            enqueue_authority,
        },
    )
}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum PeerToolError {
    #[error("invalid peer tool input: {0}")]
    InvalidInput(&'static str),
    #[error("invalid peer authority: {0}")]
    InvalidAuthority(&'static str),
    #[error("peer authority denied by {authority_source:?}")]
    Denied { authority_source: AuthoritySource },
    #[error("peer authority changed before execution")]
    StaleAuthority,
    #[error("durable peer tool failure: {0}")]
    DurableFailure(String),
}

impl PeerToolError {
    pub fn durable_safe(message: impl Into<String>) -> Self {
        let message = message.into();
        if message.len() <= MAX_SAFE_ERROR_BYTES {
            Self::DurableFailure(message)
        } else {
            Self::DurableFailure("durable peer operation failed".into())
        }
    }
}

fn validate_text(field: &'static str, value: &str, max_bytes: usize) -> Result<(), PeerToolError> {
    if value.trim().is_empty() || value.len() > max_bytes {
        return Err(PeerToolError::InvalidInput(field));
    }
    Ok(())
}

fn validate_digest(value: &str) -> Result<(), PeerToolError> {
    if value.len() != 64 || !value.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err(PeerToolError::InvalidAuthority(
            "policy digest is not canonical sha256",
        ));
    }
    Ok(())
}
