#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Display;

use keith_agent_types::{
    AssignmentId, CURRENT_SCHEMA_VERSION, ConversationId, EntityId, GrantId, ProfileId, Revision,
    RootTreeId, SchemaVersion, SessionId, StableKey, UtcTimestamp, WorkspaceId,
};
use keith_model_registry::{
    ModelPurpose, ModelRegistry, ModelRoute as RegistryModelRoute,
    ModelSelection as RegistryModelSelection, RegistryError, ResolvedRoute,
};
use keith_profile::{ProfileError, ProfileRegistry, RegisteredProfile};
use keith_session_store::{
    NewSession, ProfileSnapshotMetadata, SessionKind, SessionManifest, SessionStore,
    SessionStoreError, WriterIdentity,
};
use keith_state_store_core::{
    Collection, ProfileRepository, RecordMutation, RouteRepository, StateRecordRepository,
    VersionedRecord, WritePrecondition,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const MAX_PEER_AUTHORITY_ITEMS: usize = 256;
pub const MAX_PEER_AUTHORITY_VALUE_BYTES: usize = 512;
pub const MAX_PEER_GRANTS: usize = 256;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerAutonomyCeiling {
    Denied,
    Observe,
    Propose,
    ExecuteReversible,
    ExecuteConsequential,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerAuthorityCapabilities {
    pub tools: BTreeSet<String>,
    pub credential_references: BTreeSet<String>,
    pub model_routes: BTreeSet<String>,
    pub network_scopes: BTreeSet<String>,
    pub filesystem_scopes: BTreeSet<String>,
    pub sandbox_profiles: BTreeSet<String>,
    pub computer_ids: BTreeSet<EntityId>,
    pub may_request_approval: bool,
    pub autonomy: PeerAutonomyCeiling,
    pub self_evolution: bool,
    pub max_tool_calls: u32,
    pub max_tokens: u64,
    pub max_elapsed_millis: u64,
}

impl PeerAuthorityCapabilities {
    fn validate(&self) -> Result<(), PeerAuthorityError> {
        for values in [
            &self.tools,
            &self.credential_references,
            &self.model_routes,
            &self.network_scopes,
            &self.filesystem_scopes,
            &self.sandbox_profiles,
        ] {
            if values.len() > MAX_PEER_AUTHORITY_ITEMS
                || values.iter().any(|value| {
                    value.trim().is_empty()
                        || value.len() > MAX_PEER_AUTHORITY_VALUE_BYTES
                        || value.contains('\0')
                })
            {
                return Err(PeerAuthorityError::Invalid(
                    "peer authority string set is empty, malformed, or over bound",
                ));
            }
        }
        if self.computer_ids.len() > MAX_PEER_AUTHORITY_ITEMS {
            return Err(PeerAuthorityError::Invalid(
                "peer authority computer set is over bound",
            ));
        }
        Ok(())
    }

    fn intersect(&self, other: &Self) -> Self {
        Self {
            tools: set_intersection(&self.tools, &other.tools),
            credential_references: set_intersection(
                &self.credential_references,
                &other.credential_references,
            ),
            model_routes: set_intersection(&self.model_routes, &other.model_routes),
            network_scopes: set_intersection(&self.network_scopes, &other.network_scopes),
            filesystem_scopes: set_intersection(&self.filesystem_scopes, &other.filesystem_scopes),
            sandbox_profiles: set_intersection(&self.sandbox_profiles, &other.sandbox_profiles),
            computer_ids: set_intersection(&self.computer_ids, &other.computer_ids),
            may_request_approval: self.may_request_approval && other.may_request_approval,
            autonomy: self.autonomy.min(other.autonomy),
            self_evolution: false,
            max_tool_calls: self.max_tool_calls.min(other.max_tool_calls),
            max_tokens: self.max_tokens.min(other.max_tokens),
            max_elapsed_millis: self.max_elapsed_millis.min(other.max_elapsed_millis),
        }
    }

    fn contains(&self, requested: &Self) -> bool {
        requested.tools.is_subset(&self.tools)
            && requested
                .credential_references
                .is_subset(&self.credential_references)
            && requested.model_routes.is_subset(&self.model_routes)
            && requested.network_scopes.is_subset(&self.network_scopes)
            && requested
                .filesystem_scopes
                .is_subset(&self.filesystem_scopes)
            && requested.sandbox_profiles.is_subset(&self.sandbox_profiles)
            && requested.computer_ids.is_subset(&self.computer_ids)
            && (!requested.may_request_approval || self.may_request_approval)
            && requested.autonomy <= self.autonomy
            && !requested.self_evolution
            && requested.max_tool_calls <= self.max_tool_calls
            && requested.max_tokens <= self.max_tokens
            && requested.max_elapsed_millis <= self.max_elapsed_millis
    }
}

fn set_intersection<T: Clone + Ord>(left: &BTreeSet<T>, right: &BTreeSet<T>) -> BTreeSet<T> {
    left.intersection(right).cloned().collect()
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerAuthorityLayer {
    Installation,
    SenderProfile,
    ReceiverProfile,
    Conversation,
    Assignment,
    Grant,
    Sandbox,
    Computer,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerAuthorityPolicySnapshot {
    pub layer: PeerAuthorityLayer,
    pub subject_id: EntityId,
    pub revision: Revision,
    pub policy_digest_sha256: String,
    pub enabled: bool,
    pub revoked_at: Option<UtcTimestamp>,
    pub expires_at: Option<UtcTimestamp>,
    pub capabilities: PeerAuthorityCapabilities,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerAuthorityContextSnapshot {
    pub installation: PeerAuthorityPolicySnapshot,
    pub sender: PeerAuthorityPolicySnapshot,
    pub receiver: PeerAuthorityPolicySnapshot,
    pub conversation: PeerAuthorityPolicySnapshot,
    pub assignment: Option<PeerAuthorityPolicySnapshot>,
    pub grants: BTreeMap<GrantId, PeerAuthorityPolicySnapshot>,
    pub sandbox: PeerAuthorityPolicySnapshot,
    pub computer: PeerAuthorityPolicySnapshot,
    pub observed_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerAuthorityRequest {
    pub request_key: StableKey,
    pub correlation_key: StableKey,
    pub enqueue_decision_key: StableKey,
    pub execution_decision_key: StableKey,
    pub installation_id: EntityId,
    pub sender_profile_id: ProfileId,
    pub receiver_profile_id: ProfileId,
    pub conversation_id: ConversationId,
    pub assignment_id: Option<AssignmentId>,
    pub required_grant_ids: BTreeSet<GrantId>,
    pub operation: PeerOperation,
    pub requested: PeerAuthorityCapabilities,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerOperation {
    MessageAgent,
    AssignWork,
    HandoffWork,
    ReportAssignment,
}

impl PeerOperation {
    pub const fn capability(self) -> &'static str {
        match self {
            Self::MessageAgent => "peer.message_agent",
            Self::AssignWork => "peer.assign_work",
            Self::HandoffWork => "peer.handoff_work",
            Self::ReportAssignment => "peer.report_assignment",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerAuthorityAdmissionPhase {
    Enqueue,
    Execution,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerAuthorityDenial {
    InvalidBinding,
    Disabled,
    Revoked,
    Expired,
    MissingAssignment,
    MissingGrant,
    StaleSnapshot,
    Escalation,
    SelfEvolutionProtected,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "decision", content = "reason")]
pub enum PeerAuthorityDecision {
    Allowed,
    Denied(PeerAuthorityDenial),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerAuthorityRevisionEvidence {
    pub installation: Revision,
    pub sender: Revision,
    pub receiver: Revision,
    pub conversation: Revision,
    pub assignment: Option<Revision>,
    pub grants: BTreeMap<GrantId, Revision>,
    pub sandbox: Revision,
    pub computer: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerAuthorityDigestEvidence {
    pub installation: String,
    pub sender: String,
    pub receiver: String,
    pub conversation: String,
    pub assignment: Option<String>,
    pub grants: BTreeMap<GrantId, String>,
    pub sandbox: String,
    pub computer: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EffectivePeerAuthority {
    pub request_key: StableKey,
    pub correlation_key: StableKey,
    pub decision_key: StableKey,
    pub phase: PeerAuthorityAdmissionPhase,
    pub sender_profile_id: ProfileId,
    pub receiver_profile_id: ProfileId,
    pub conversation_id: ConversationId,
    pub assignment_id: Option<AssignmentId>,
    pub operation: PeerOperation,
    pub requested: PeerAuthorityCapabilities,
    pub revisions: PeerAuthorityRevisionEvidence,
    pub policy_digests: PeerAuthorityDigestEvidence,
    pub sender_enabled: bool,
    pub receiver_enabled: bool,
    pub effective: PeerAuthorityCapabilities,
    pub decision: PeerAuthorityDecision,
    pub observed_at: UtcTimestamp,
}

impl EffectivePeerAuthority {
    pub const fn is_allowed(&self) -> bool {
        matches!(self.decision, PeerAuthorityDecision::Allowed)
    }

    pub fn require_allowed(&self) -> Result<&PeerAuthorityCapabilities, PeerAuthorityError> {
        if self.is_allowed() {
            Ok(&self.effective)
        } else {
            Err(PeerAuthorityError::Denied)
        }
    }
}

pub trait PeerAuthorityResolver {
    type Error: std::error::Error + Send + Sync + 'static;

    /// Resolves all authority layers from one current authoritative read. Implementations must not
    /// accept caller-supplied policy bodies or revision assertions as authority.
    fn resolve_current(
        &self,
        request: &PeerAuthorityRequest,
        phase: PeerAuthorityAdmissionPhase,
        now: UtcTimestamp,
    ) -> Result<PeerAuthorityContextSnapshot, Self::Error>;
}

#[derive(Debug, Error)]
pub enum PeerAuthorityError {
    #[error("peer authority request is invalid: {0}")]
    Invalid(&'static str),
    #[error("peer authority resolver failed: {0}")]
    Resolver(String),
    #[error("peer authority admission was denied")]
    Denied,
}

pub struct PeerAuthorityRouter<R> {
    resolver: R,
}

impl<R: PeerAuthorityResolver> PeerAuthorityRouter<R> {
    pub const fn new(resolver: R) -> Self {
        Self { resolver }
    }

    pub fn admit_enqueue(
        &self,
        request: &PeerAuthorityRequest,
        now: UtcTimestamp,
    ) -> Result<EffectivePeerAuthority, PeerAuthorityError> {
        self.admit(request, PeerAuthorityAdmissionPhase::Enqueue, now, None)
    }

    pub fn admit_execution(
        &self,
        request: &PeerAuthorityRequest,
        enqueue_authority: &EffectivePeerAuthority,
        now: UtcTimestamp,
    ) -> Result<EffectivePeerAuthority, PeerAuthorityError> {
        self.admit(
            request,
            PeerAuthorityAdmissionPhase::Execution,
            now,
            Some(enqueue_authority),
        )
    }

    fn admit(
        &self,
        request: &PeerAuthorityRequest,
        phase: PeerAuthorityAdmissionPhase,
        now: UtcTimestamp,
        enqueue_authority: Option<&EffectivePeerAuthority>,
    ) -> Result<EffectivePeerAuthority, PeerAuthorityError> {
        validate_peer_request(request)?;
        let snapshot = self
            .resolver
            .resolve_current(request, phase, now)
            .map_err(|error| PeerAuthorityError::Resolver(error.to_string()))?;
        evaluate_peer_authority(request, phase, now, snapshot, enqueue_authority)
    }
}

fn evaluate_peer_authority(
    request: &PeerAuthorityRequest,
    phase: PeerAuthorityAdmissionPhase,
    now: UtcTimestamp,
    snapshot: PeerAuthorityContextSnapshot,
    enqueue_authority: Option<&EffectivePeerAuthority>,
) -> Result<EffectivePeerAuthority, PeerAuthorityError> {
    validate_peer_snapshot(request, &snapshot, now)?;
    let policies = peer_policies(&snapshot);
    let mut effective = snapshot.installation.capabilities.clone();
    for policy in policies.iter().skip(1) {
        effective = effective.intersect(&policy.capabilities);
    }
    effective.self_evolution = false;
    let revisions = revision_evidence(&snapshot);
    let policy_digests = digest_evidence(&snapshot);
    let mut denial = policy_denial(&policies, now);
    if denial.is_none() && request.assignment_id.is_some() && snapshot.assignment.is_none() {
        denial = Some(PeerAuthorityDenial::MissingAssignment);
    }
    if denial.is_none()
        && request
            .required_grant_ids
            .iter()
            .any(|grant_id| !snapshot.grants.contains_key(grant_id))
    {
        denial = Some(PeerAuthorityDenial::MissingGrant);
    }
    if denial.is_none() && request.requested.self_evolution {
        denial = Some(PeerAuthorityDenial::SelfEvolutionProtected);
    }
    if denial.is_none() && !effective.contains(&request.requested) {
        denial = Some(PeerAuthorityDenial::Escalation);
    }
    if let Some(enqueue) = enqueue_authority {
        let exact_binding = enqueue.phase == PeerAuthorityAdmissionPhase::Enqueue
            && enqueue.request_key == request.request_key
            && enqueue.correlation_key == request.correlation_key
            && enqueue.sender_profile_id == request.sender_profile_id
            && enqueue.receiver_profile_id == request.receiver_profile_id
            && enqueue.conversation_id == request.conversation_id
            && enqueue.assignment_id == request.assignment_id
            && enqueue.operation == request.operation
            && enqueue.requested == request.requested;
        if !exact_binding {
            denial = Some(PeerAuthorityDenial::InvalidBinding);
        } else if !enqueue.is_allowed() {
            denial = Some(PeerAuthorityDenial::StaleSnapshot);
        } else if denial.is_none()
            && (enqueue.revisions != revisions || enqueue.policy_digests != policy_digests)
        {
            denial = Some(PeerAuthorityDenial::StaleSnapshot);
        }
    } else if phase == PeerAuthorityAdmissionPhase::Execution {
        denial = Some(PeerAuthorityDenial::StaleSnapshot);
    }
    Ok(EffectivePeerAuthority {
        request_key: request.request_key.clone(),
        correlation_key: request.correlation_key.clone(),
        decision_key: match phase {
            PeerAuthorityAdmissionPhase::Enqueue => request.enqueue_decision_key.clone(),
            PeerAuthorityAdmissionPhase::Execution => request.execution_decision_key.clone(),
        },
        phase,
        sender_profile_id: request.sender_profile_id.clone(),
        receiver_profile_id: request.receiver_profile_id.clone(),
        conversation_id: request.conversation_id.clone(),
        assignment_id: request.assignment_id.clone(),
        operation: request.operation,
        requested: request.requested.clone(),
        revisions,
        policy_digests,
        sender_enabled: snapshot.sender.enabled,
        receiver_enabled: snapshot.receiver.enabled,
        effective,
        decision: denial.map_or(
            PeerAuthorityDecision::Allowed,
            PeerAuthorityDecision::Denied,
        ),
        observed_at: snapshot.observed_at,
    })
}

fn validate_peer_request(request: &PeerAuthorityRequest) -> Result<(), PeerAuthorityError> {
    request.requested.validate()?;
    if request.sender_profile_id == request.receiver_profile_id
        || request.required_grant_ids.len() > MAX_PEER_GRANTS
        || request.request_key == request.correlation_key
        || request.request_key == request.enqueue_decision_key
        || request.request_key == request.execution_decision_key
        || request.correlation_key == request.enqueue_decision_key
        || request.correlation_key == request.execution_decision_key
        || request.enqueue_decision_key == request.execution_decision_key
    {
        return Err(PeerAuthorityError::Invalid(
            "peer route identity or grant bound is invalid",
        ));
    }
    if !request
        .requested
        .tools
        .contains(request.operation.capability())
    {
        return Err(PeerAuthorityError::Invalid(
            "peer operation capability was not declared",
        ));
    }
    if matches!(
        request.operation,
        PeerOperation::HandoffWork | PeerOperation::ReportAssignment
    ) && request.assignment_id.is_none()
    {
        return Err(PeerAuthorityError::Invalid(
            "peer operation requires an assignment binding",
        ));
    }
    Ok(())
}

fn validate_peer_snapshot(
    request: &PeerAuthorityRequest,
    snapshot: &PeerAuthorityContextSnapshot,
    now: UtcTimestamp,
) -> Result<(), PeerAuthorityError> {
    let exact_layers = snapshot.installation.layer == PeerAuthorityLayer::Installation
        && snapshot.sender.layer == PeerAuthorityLayer::SenderProfile
        && snapshot.receiver.layer == PeerAuthorityLayer::ReceiverProfile
        && snapshot.conversation.layer == PeerAuthorityLayer::Conversation
        && snapshot.sandbox.layer == PeerAuthorityLayer::Sandbox
        && snapshot.computer.layer == PeerAuthorityLayer::Computer
        && snapshot
            .assignment
            .as_ref()
            .is_none_or(|policy| policy.layer == PeerAuthorityLayer::Assignment)
        && snapshot
            .grants
            .values()
            .all(|policy| policy.layer == PeerAuthorityLayer::Grant);
    let exact_subjects = snapshot.observed_at == now
        && &snapshot.installation.subject_id == &request.installation_id
        && &snapshot.sender.subject_id == request.sender_profile_id.as_entity_id()
        && &snapshot.receiver.subject_id == request.receiver_profile_id.as_entity_id()
        && &snapshot.conversation.subject_id == request.conversation_id.as_entity_id()
        && match (&request.assignment_id, &snapshot.assignment) {
            (None, None) | (Some(_), None) => true,
            (Some(id), Some(policy)) => &policy.subject_id == id.as_entity_id(),
            (None, Some(_)) => false,
        }
        && snapshot.grants.iter().all(|(grant_id, policy)| {
            request.required_grant_ids.contains(grant_id)
                && &policy.subject_id == grant_id.as_entity_id()
        });
    if !exact_layers || !exact_subjects {
        return Err(PeerAuthorityError::Invalid(
            "resolved peer authority layers do not match the requested subjects",
        ));
    }
    for policy in peer_policies(snapshot) {
        policy.capabilities.validate()?;
        if policy.policy_digest_sha256.len() != 64
            || !policy
                .policy_digest_sha256
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit())
        {
            return Err(PeerAuthorityError::Invalid(
                "peer policy digest is not hexadecimal SHA-256",
            ));
        }
    }
    Ok(())
}

fn peer_policies(snapshot: &PeerAuthorityContextSnapshot) -> Vec<&PeerAuthorityPolicySnapshot> {
    let mut policies = vec![
        &snapshot.installation,
        &snapshot.sender,
        &snapshot.receiver,
        &snapshot.conversation,
    ];
    if let Some(assignment) = &snapshot.assignment {
        policies.push(assignment);
    }
    policies.extend(snapshot.grants.values());
    policies.push(&snapshot.sandbox);
    policies.push(&snapshot.computer);
    policies
}

fn policy_denial(
    policies: &[&PeerAuthorityPolicySnapshot],
    now: UtcTimestamp,
) -> Option<PeerAuthorityDenial> {
    if policies.iter().any(|policy| !policy.enabled) {
        Some(PeerAuthorityDenial::Disabled)
    } else if policies.iter().any(|policy| policy.revoked_at.is_some()) {
        Some(PeerAuthorityDenial::Revoked)
    } else if policies.iter().any(|policy| {
        policy
            .expires_at
            .is_some_and(|expires_at| expires_at <= now)
    }) {
        Some(PeerAuthorityDenial::Expired)
    } else {
        None
    }
}

fn revision_evidence(snapshot: &PeerAuthorityContextSnapshot) -> PeerAuthorityRevisionEvidence {
    PeerAuthorityRevisionEvidence {
        installation: snapshot.installation.revision,
        sender: snapshot.sender.revision,
        receiver: snapshot.receiver.revision,
        conversation: snapshot.conversation.revision,
        assignment: snapshot.assignment.as_ref().map(|policy| policy.revision),
        grants: snapshot
            .grants
            .iter()
            .map(|(id, policy)| (id.clone(), policy.revision))
            .collect(),
        sandbox: snapshot.sandbox.revision,
        computer: snapshot.computer.revision,
    }
}

fn digest_evidence(snapshot: &PeerAuthorityContextSnapshot) -> PeerAuthorityDigestEvidence {
    PeerAuthorityDigestEvidence {
        installation: snapshot.installation.policy_digest_sha256.clone(),
        sender: snapshot.sender.policy_digest_sha256.clone(),
        receiver: snapshot.receiver.policy_digest_sha256.clone(),
        conversation: snapshot.conversation.policy_digest_sha256.clone(),
        assignment: snapshot
            .assignment
            .as_ref()
            .map(|policy| policy.policy_digest_sha256.clone()),
        grants: snapshot
            .grants
            .iter()
            .map(|(id, policy)| (id.clone(), policy.policy_digest_sha256.clone()))
            .collect(),
        sandbox: snapshot.sandbox.policy_digest_sha256.clone(),
        computer: snapshot.computer.policy_digest_sha256.clone(),
    }
}

#[cfg(test)]
mod peer_authority_tests {
    use super::*;

    fn capabilities(tools: &[&str]) -> PeerAuthorityCapabilities {
        PeerAuthorityCapabilities {
            tools: tools.iter().map(|value| (*value).to_owned()).collect(),
            credential_references: BTreeSet::from(["credential:shared-browser".into()]),
            model_routes: BTreeSet::from(["route:peer".into()]),
            network_scopes: BTreeSet::from(["https:example.com".into()]),
            filesystem_scopes: BTreeSet::from(["workspace:shared".into()]),
            sandbox_profiles: BTreeSet::from(["sandbox:peer".into()]),
            computer_ids: BTreeSet::from([EntityId::from_u128(90)]),
            may_request_approval: true,
            autonomy: PeerAutonomyCeiling::ExecuteReversible,
            self_evolution: false,
            max_tool_calls: 8,
            max_tokens: 8_000,
            max_elapsed_millis: 60_000,
        }
    }

    fn policy(
        layer: PeerAuthorityLayer,
        subject_id: EntityId,
        revision: u64,
        tools: &[&str],
    ) -> PeerAuthorityPolicySnapshot {
        PeerAuthorityPolicySnapshot {
            layer,
            subject_id,
            revision: Revision::new(revision),
            policy_digest_sha256: "ab".repeat(32),
            enabled: true,
            revoked_at: None,
            expires_at: None,
            capabilities: capabilities(tools),
        }
    }

    fn request() -> PeerAuthorityRequest {
        PeerAuthorityRequest {
            request_key: StableKey::parse("peer:request:test").unwrap(),
            correlation_key: StableKey::parse("peer:authority:test").unwrap(),
            enqueue_decision_key: StableKey::parse("peer:decision:enqueue:test").unwrap(),
            execution_decision_key: StableKey::parse("peer:decision:execution:test").unwrap(),
            installation_id: EntityId::from_u128(1),
            sender_profile_id: ProfileId::from(EntityId::from_u128(2)),
            receiver_profile_id: ProfileId::from(EntityId::from_u128(3)),
            conversation_id: ConversationId::from(EntityId::from_u128(4)),
            assignment_id: None,
            required_grant_ids: BTreeSet::new(),
            operation: PeerOperation::MessageAgent,
            requested: capabilities(&["read", "peer.message_agent"]),
        }
    }

    fn snapshot(request: &PeerAuthorityRequest, now: UtcTimestamp) -> PeerAuthorityContextSnapshot {
        let broad = &["read", "write", "peer.message_agent"];
        PeerAuthorityContextSnapshot {
            installation: policy(
                PeerAuthorityLayer::Installation,
                request.installation_id.clone(),
                1,
                broad,
            ),
            sender: policy(
                PeerAuthorityLayer::SenderProfile,
                request.sender_profile_id.as_entity_id().clone(),
                2,
                broad,
            ),
            receiver: policy(
                PeerAuthorityLayer::ReceiverProfile,
                request.receiver_profile_id.as_entity_id().clone(),
                3,
                &["read", "peer.message_agent"],
            ),
            conversation: policy(
                PeerAuthorityLayer::Conversation,
                request.conversation_id.as_entity_id().clone(),
                4,
                broad,
            ),
            assignment: None,
            grants: BTreeMap::new(),
            sandbox: policy(
                PeerAuthorityLayer::Sandbox,
                EntityId::from_u128(5),
                5,
                broad,
            ),
            computer: policy(
                PeerAuthorityLayer::Computer,
                EntityId::from_u128(6),
                6,
                broad,
            ),
            observed_at: now,
        }
    }

    #[test]
    fn peer_authority_intersects_every_layer_and_rejects_escalation() {
        let now = UtcTimestamp::from_unix_millis(100);
        let request = request();
        let allowed = evaluate_peer_authority(
            &request,
            PeerAuthorityAdmissionPhase::Enqueue,
            now,
            snapshot(&request, now),
            None,
        )
        .unwrap();
        assert!(allowed.is_allowed());
        assert_eq!(
            allowed.effective.tools,
            BTreeSet::from(["peer.message_agent".into(), "read".into()])
        );

        let mut escalation = request.clone();
        escalation.requested.tools.insert("write".into());
        let denied = evaluate_peer_authority(
            &escalation,
            PeerAuthorityAdmissionPhase::Enqueue,
            now,
            snapshot(&escalation, now),
            None,
        )
        .unwrap();
        assert_eq!(
            denied.decision,
            PeerAuthorityDecision::Denied(PeerAuthorityDenial::Escalation)
        );
    }

    #[test]
    fn peer_execution_recomputes_and_rejects_revision_drift_and_revocation() {
        let enqueue_at = UtcTimestamp::from_unix_millis(100);
        let request = request();
        let enqueue = evaluate_peer_authority(
            &request,
            PeerAuthorityAdmissionPhase::Enqueue,
            enqueue_at,
            snapshot(&request, enqueue_at),
            None,
        )
        .unwrap();
        let execution_at = UtcTimestamp::from_unix_millis(200);
        let mut stale = snapshot(&request, execution_at);
        stale.conversation.revision = Revision::new(5);
        let denied = evaluate_peer_authority(
            &request,
            PeerAuthorityAdmissionPhase::Execution,
            execution_at,
            stale,
            Some(&enqueue),
        )
        .unwrap();
        assert_eq!(
            denied.decision,
            PeerAuthorityDecision::Denied(PeerAuthorityDenial::StaleSnapshot)
        );

        let mut revoked = snapshot(&request, execution_at);
        revoked.receiver.revoked_at = Some(execution_at);
        let denied = evaluate_peer_authority(
            &request,
            PeerAuthorityAdmissionPhase::Execution,
            execution_at,
            revoked,
            Some(&enqueue),
        )
        .unwrap();
        assert_eq!(
            denied.decision,
            PeerAuthorityDecision::Denied(PeerAuthorityDenial::Revoked)
        );
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReplyRoute {
    pub channel: String,
    pub destination: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ProfileRefreshPolicy {
    KeepPinned,
    ApplyLatest,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SessionPolicy {
    pub profile_refresh: ProfileRefreshPolicy,
    pub memory_enabled: bool,
    pub schedules_enabled: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RouteRequest {
    pub profile_id: Option<ProfileId>,
    pub workspace_id: Option<WorkspaceId>,
    pub caller: String,
    pub reply: ReplyRoute,
    pub session_policy: SessionPolicy,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResolvedModelConfiguration {
    pub provider: String,
    pub model: String,
    pub credential_ref: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResolvedProfileSnapshot {
    pub profile: RegisteredProfile,
    pub model: ResolvedModelConfiguration,
    pub reply: ReplyRoute,
    pub session_policy: SessionPolicy,
    pub resolved_at: UtcTimestamp,
}

pub struct ResolvedSessionRoute {
    pub snapshot: ResolvedProfileSnapshot,
    pub model_route: ResolvedRoute,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NewRootSession {
    pub session_id: SessionId,
    pub root_tree_id: RootTreeId,
    pub created_at: UtcTimestamp,
    pub label: Option<String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelSessionPolicy {
    PerConversation,
    PerSender,
    PerThread,
    ExplicitOnly,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GroupMentionPolicy {
    Always,
    RequireMention,
    Ignore,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GroupMemoryPolicy {
    Shared,
    PrivatePerParticipant,
    Disabled,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ParticipantRetentionPolicy {
    Retain,
    Ephemeral,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "authority", content = "principals")]
pub enum GroupAuthority {
    Disabled,
    ProfileCallers,
    AllowList(BTreeSet<String>),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ProactivePostPolicy {
    Denied,
    Allowed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GroupRoutePolicy {
    pub mention: GroupMentionPolicy,
    pub memory: GroupMemoryPolicy,
    pub participant_retention: ParticipantRetentionPolicy,
    pub tools: GroupAuthority,
    pub schedules: GroupAuthority,
    pub proactive_posts: ProactivePostPolicy,
}

pub const MAX_CONVERSATION_BINDINGS: usize = 4_096;
pub const MAX_EXTERNAL_ROUTE_VALUE_BYTES: usize = 512;

/// The platform identity which owns an external transcript. It is only an addressing key; the
/// canonical history always lives under `ConversationBinding::conversation_id`.
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExternalConversationIdentity {
    pub channel: String,
    pub external_account: String,
    pub conversation: String,
    pub thread: Option<String>,
}

impl ExternalConversationIdentity {
    fn validate(&self) -> Result<(), ConversationBindingError> {
        let values = [
            Some(self.channel.as_str()),
            Some(self.external_account.as_str()),
            Some(self.conversation.as_str()),
            self.thread.as_deref(),
        ];
        if values.into_iter().flatten().any(|value| {
            value.trim().is_empty()
                || value.len() > MAX_EXTERNAL_ROUTE_VALUE_BYTES
                || value.contains('\0')
        }) {
            return Err(ConversationBindingError::Invalid(
                "external route identity is empty, malformed, or over bound",
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationBindingState {
    Active,
    Revoked,
}

/// Exact policy and authority revision observed when an external route was bound.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationBindingPolicy {
    pub route_revision: Revision,
    pub conversation_revision: Revision,
    pub participant_revision: Revision,
    pub policy_digest_sha256: String,
    pub group: GroupRoutePolicy,
}

/// Durable bridge from a platform transcript to one canonical Keith conversation participant.
/// `participant_session_id` is a private processing session and is never a second transcript.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationBinding {
    pub version: SchemaVersion,
    pub id: EntityId,
    pub stable_key: StableKey,
    pub external: ExternalConversationIdentity,
    pub route_id: EntityId,
    pub conversation_id: ConversationId,
    pub participant_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub policy: ConversationBindingPolicy,
    pub state: ConversationBindingState,
    pub revision: Revision,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub revoked_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConversationBindingAuthorization {
    pub route_id: EntityId,
    pub route_revision: Revision,
    pub route_enabled: bool,
    pub conversation_id: ConversationId,
    pub conversation_revision: Revision,
    pub conversation_enabled: bool,
    pub participant_profile_id: ProfileId,
    pub participant_revision: Revision,
    pub participant_active: bool,
    pub profile_enabled: bool,
    pub policy_digest_sha256: String,
    pub group_policy: GroupRoutePolicy,
    pub observed_at: UtcTimestamp,
}

/// Adapter seam for a single authoritative read of current route, conversation, participant and
/// profile state. Callers cannot assert their own authorization snapshots.
pub trait ConversationBindingAuthorizer {
    type Error: std::error::Error + Send + Sync + 'static;

    fn resolve_current(
        &self,
        binding: &ConversationBinding,
        now: UtcTimestamp,
    ) -> Result<ConversationBindingAuthorization, Self::Error>;
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuthorizedConversationBinding {
    pub binding: ConversationBinding,
    pub authorization: ConversationBindingAuthorization,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConversationBindingMigration {
    pub id: EntityId,
    pub stable_key: StableKey,
    pub external: ExternalConversationIdentity,
    pub route_id: EntityId,
    pub route_revision: Revision,
    pub conversation_id: ConversationId,
    pub conversation_revision: Revision,
    pub participant_profile_id: ProfileId,
    pub participant_revision: Revision,
    pub participant_session_id: SessionId,
    pub policy_digest_sha256: String,
    pub group_policy: GroupRoutePolicy,
}

#[derive(Debug, Error)]
pub enum ConversationBindingError {
    #[error("conversation binding is invalid: {0}")]
    Invalid(&'static str),
    #[error("conversation binding repository failed: {0}")]
    Repository(String),
    #[error("conversation binding is corrupt: {0}")]
    Corrupt(String),
    #[error("conversation binding was not found")]
    Missing,
    #[error("external route has more than one active conversation binding")]
    Ambiguous,
    #[error("conversation binding stable or external identity already exists")]
    Duplicate,
    #[error("conversation binding revision is stale")]
    Stale,
    #[error("conversation binding is revoked")]
    Revoked,
    #[error("conversation binding authority is disabled or revoked")]
    Unauthorized,
    #[error("conversation binding authority revisions or policy changed")]
    StaleAuthority,
    #[error("conversation binding authority resolver failed: {0}")]
    Resolver(String),
}

pub struct ConversationBindingRegistry<R> {
    repository: R,
}

impl<R> ConversationBindingRegistry<R>
where
    R: StateRecordRepository,
    R::Error: Display,
{
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    /// Creates a canonical binding, rejecting duplicate stable keys and external identities.
    pub fn create(
        &self,
        migration: ConversationBindingMigration,
        now: UtcTimestamp,
    ) -> Result<ConversationBinding, ConversationBindingError> {
        let binding = ConversationBinding {
            version: CURRENT_SCHEMA_VERSION,
            id: migration.id,
            stable_key: migration.stable_key,
            external: migration.external,
            route_id: migration.route_id,
            conversation_id: migration.conversation_id,
            participant_profile_id: migration.participant_profile_id,
            participant_session_id: migration.participant_session_id,
            policy: ConversationBindingPolicy {
                route_revision: migration.route_revision,
                conversation_revision: migration.conversation_revision,
                participant_revision: migration.participant_revision,
                policy_digest_sha256: migration.policy_digest_sha256,
                group: migration.group_policy,
            },
            state: ConversationBindingState::Active,
            revision: Revision::ZERO,
            created_at: now,
            updated_at: now,
            revoked_at: None,
        };
        validate_conversation_binding(&binding)?;
        let records = self.records()?;
        if records.iter().any(|existing| {
            existing.id == binding.id
                || existing.stable_key == binding.stable_key
                || (existing.external == binding.external
                    && existing.state == ConversationBindingState::Active)
        }) {
            return Err(ConversationBindingError::Duplicate);
        }
        self.repository
            .transact(&[RecordMutation::Put {
                collection: Collection::ConversationBindings,
                record: encode_conversation_binding(&binding)?,
                precondition: WritePrecondition::Missing,
            }])
            .map_err(binding_repository_error)?;
        Ok(binding)
    }

    /// Replay-safe migration entry point for legacy session route bindings.
    pub fn migrate(
        &self,
        migration: ConversationBindingMigration,
        now: UtcTimestamp,
    ) -> Result<ConversationBinding, ConversationBindingError> {
        if let Some(existing) = self.find_by_stable_key(&migration.stable_key)? {
            let expected = ConversationBinding {
                version: CURRENT_SCHEMA_VERSION,
                id: migration.id,
                stable_key: migration.stable_key,
                external: migration.external,
                route_id: migration.route_id,
                conversation_id: migration.conversation_id,
                participant_profile_id: migration.participant_profile_id,
                participant_session_id: migration.participant_session_id,
                policy: ConversationBindingPolicy {
                    route_revision: migration.route_revision,
                    conversation_revision: migration.conversation_revision,
                    participant_revision: migration.participant_revision,
                    policy_digest_sha256: migration.policy_digest_sha256,
                    group: migration.group_policy,
                },
                state: ConversationBindingState::Active,
                revision: existing.revision,
                created_at: existing.created_at,
                updated_at: existing.updated_at,
                revoked_at: None,
            };
            if existing == expected {
                return Ok(existing);
            }
            return Err(ConversationBindingError::Duplicate);
        }
        self.create(migration, now)
    }

    pub fn get(
        &self,
        id: &EntityId,
    ) -> Result<Option<ConversationBinding>, ConversationBindingError> {
        self.repository
            .get_record(Collection::ConversationBindings, id)
            .map_err(binding_repository_error)?
            .map(decode_conversation_binding)
            .transpose()
    }

    pub fn list(&self) -> Result<Vec<ConversationBinding>, ConversationBindingError> {
        let mut bindings = self.records()?;
        bindings.sort_by(|left, right| left.id.cmp(&right.id));
        Ok(bindings)
    }

    /// Resolves an external route and re-evaluates all current authority before returning it.
    pub fn resolve<A: ConversationBindingAuthorizer>(
        &self,
        external: &ExternalConversationIdentity,
        authorizer: &A,
        now: UtcTimestamp,
    ) -> Result<AuthorizedConversationBinding, ConversationBindingError> {
        external.validate()?;
        let mut matching = self
            .records()?
            .into_iter()
            .filter(|binding| &binding.external == external)
            .collect::<Vec<_>>();
        matching.retain(|binding| binding.state == ConversationBindingState::Active);
        if matching.len() > 1 {
            return Err(ConversationBindingError::Ambiguous);
        }
        let binding = matching.pop().ok_or(ConversationBindingError::Missing)?;
        self.authorize(binding, authorizer, now)
    }

    /// Revalidates an already queued binding before execution or publication.
    pub fn revalidate<A: ConversationBindingAuthorizer>(
        &self,
        id: &EntityId,
        expected_revision: Revision,
        authorizer: &A,
        now: UtcTimestamp,
    ) -> Result<AuthorizedConversationBinding, ConversationBindingError> {
        let binding = self.get(id)?.ok_or(ConversationBindingError::Missing)?;
        if binding.revision != expected_revision {
            return Err(ConversationBindingError::Stale);
        }
        self.authorize(binding, authorizer, now)
    }

    pub fn revoke(
        &self,
        id: &EntityId,
        expected_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<ConversationBinding, ConversationBindingError> {
        let mut binding = self.get(id)?.ok_or(ConversationBindingError::Missing)?;
        if binding.revision != expected_revision {
            return Err(ConversationBindingError::Stale);
        }
        if binding.state == ConversationBindingState::Revoked {
            return Ok(binding);
        }
        binding.revision = binding
            .revision
            .checked_next()
            .ok_or(ConversationBindingError::Stale)?;
        binding.state = ConversationBindingState::Revoked;
        binding.revoked_at = Some(now);
        binding.updated_at = now;
        self.repository
            .transact(&[RecordMutation::Put {
                collection: Collection::ConversationBindings,
                record: encode_conversation_binding(&binding)?,
                precondition: WritePrecondition::Exact(expected_revision),
            }])
            .map_err(binding_repository_error)?;
        Ok(binding)
    }

    fn authorize<A: ConversationBindingAuthorizer>(
        &self,
        binding: ConversationBinding,
        authorizer: &A,
        now: UtcTimestamp,
    ) -> Result<AuthorizedConversationBinding, ConversationBindingError> {
        if binding.state != ConversationBindingState::Active || binding.revoked_at.is_some() {
            return Err(ConversationBindingError::Revoked);
        }
        let authorization = authorizer
            .resolve_current(&binding, now)
            .map_err(|error| ConversationBindingError::Resolver(error.to_string()))?;
        let exact_identity = authorization.observed_at == now
            && authorization.route_id == binding.route_id
            && authorization.conversation_id == binding.conversation_id
            && authorization.participant_profile_id == binding.participant_profile_id;
        if !exact_identity {
            return Err(ConversationBindingError::Unauthorized);
        }
        if !authorization.route_enabled
            || !authorization.conversation_enabled
            || !authorization.participant_active
            || !authorization.profile_enabled
        {
            return Err(ConversationBindingError::Unauthorized);
        }
        let exact_policy = authorization.route_revision == binding.policy.route_revision
            && authorization.conversation_revision == binding.policy.conversation_revision
            && authorization.participant_revision == binding.policy.participant_revision
            && authorization.policy_digest_sha256 == binding.policy.policy_digest_sha256
            && authorization.group_policy == binding.policy.group;
        if !exact_policy {
            return Err(ConversationBindingError::StaleAuthority);
        }
        Ok(AuthorizedConversationBinding {
            binding,
            authorization,
        })
    }

    fn records(&self) -> Result<Vec<ConversationBinding>, ConversationBindingError> {
        let records = self
            .repository
            .list_records(Collection::ConversationBindings)
            .map_err(binding_repository_error)?;
        if records.len() > MAX_CONVERSATION_BINDINGS {
            return Err(ConversationBindingError::Corrupt(
                "conversation binding collection exceeds its bound".into(),
            ));
        }
        records
            .into_iter()
            .map(decode_conversation_binding)
            .collect()
    }

    fn find_by_stable_key(
        &self,
        stable_key: &StableKey,
    ) -> Result<Option<ConversationBinding>, ConversationBindingError> {
        let mut matching = self
            .records()?
            .into_iter()
            .filter(|binding| &binding.stable_key == stable_key);
        let first = matching.next();
        if matching.next().is_some() {
            return Err(ConversationBindingError::Corrupt(
                "duplicate conversation binding stable key".into(),
            ));
        }
        Ok(first)
    }
}

fn validate_conversation_binding(
    binding: &ConversationBinding,
) -> Result<(), ConversationBindingError> {
    binding.external.validate()?;
    if binding.version.major != CURRENT_SCHEMA_VERSION.major
        || binding.version.minor > CURRENT_SCHEMA_VERSION.minor
        || binding.policy.policy_digest_sha256.len() != 64
        || !binding
            .policy
            .policy_digest_sha256
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
        || (binding.state == ConversationBindingState::Active && binding.revoked_at.is_some())
        || (binding.state == ConversationBindingState::Revoked && binding.revoked_at.is_none())
    {
        return Err(ConversationBindingError::Invalid(
            "conversation binding schema, digest, or lifecycle is invalid",
        ));
    }
    Ok(())
}

fn encode_conversation_binding(
    binding: &ConversationBinding,
) -> Result<VersionedRecord, ConversationBindingError> {
    validate_conversation_binding(binding)?;
    Ok(VersionedRecord {
        version: binding.version,
        id: binding.id.clone(),
        revision: binding.revision,
        updated_at: binding.updated_at,
        payload: serde_json::to_value(binding)
            .map_err(|error| ConversationBindingError::Corrupt(error.to_string()))?,
    })
}

fn decode_conversation_binding(
    record: VersionedRecord,
) -> Result<ConversationBinding, ConversationBindingError> {
    let binding = serde_json::from_value::<ConversationBinding>(record.payload)
        .map_err(|error| ConversationBindingError::Corrupt(error.to_string()))?;
    validate_conversation_binding(&binding)?;
    if record.version != binding.version
        || record.id != binding.id
        || record.revision != binding.revision
        || record.updated_at != binding.updated_at
    {
        return Err(ConversationBindingError::Corrupt(
            "conversation binding envelope does not match payload".into(),
        ));
    }
    Ok(binding)
}

fn binding_repository_error(error: impl Display) -> ConversationBindingError {
    ConversationBindingError::Repository(error.to_string())
}

pub const MAX_GROUP_MENTION_FANOUT: usize = 64;
pub const MAX_GROUP_ROUTE_PARTICIPANTS: usize = 256;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GroupMentionRoutingPolicy {
    ExplicitOnly,
    AllParticipants,
    CoordinatorSelected,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GroupRouteParticipant {
    pub profile_id: ProfileId,
    pub membership_revision: keith_agent_types::Revision,
    pub enabled: bool,
    pub active_member: bool,
    pub mention_policy: GroupMentionPolicy,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GroupMentionRouteRequest {
    pub sender_profile_id: ProfileId,
    pub policy: GroupMentionRoutingPolicy,
    pub requested_targets: Vec<ProfileId>,
    pub participants: Vec<GroupRouteParticipant>,
    pub max_fanout: usize,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GroupMentionRouteTarget {
    pub profile_id: ProfileId,
    pub membership_revision: keith_agent_types::Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GroupMentionRouteDecision {
    pub sender_profile_id: ProfileId,
    pub sender_membership_revision: keith_agent_types::Revision,
    pub selection: GroupMentionRoutingPolicy,
    pub targets: Vec<GroupMentionRouteTarget>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum GroupMentionRouteError {
    InvalidFanout,
    TooManyParticipants,
    DuplicateParticipant(ProfileId),
    DuplicateTarget(ProfileId),
    SenderNotAuthorized(ProfileId),
    TargetNotAuthorized(ProfileId),
    TargetIgnoresGroup(ProfileId),
    UnexpectedTargets,
    FanoutExceeded { selected: usize, maximum: usize },
}

impl std::fmt::Display for GroupMentionRouteError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::InvalidFanout => formatter.write_str("group mention fanout bound is invalid"),
            Self::TooManyParticipants => {
                formatter.write_str("group routing participant set is too large")
            }
            Self::DuplicateParticipant(profile_id) => {
                write!(formatter, "duplicate group participant {profile_id}")
            }
            Self::DuplicateTarget(profile_id) => {
                write!(formatter, "duplicate group mention target {profile_id}")
            }
            Self::SenderNotAuthorized(profile_id) => write!(
                formatter,
                "group sender {profile_id} is not an enabled active participant"
            ),
            Self::TargetNotAuthorized(profile_id) => write!(
                formatter,
                "group target {profile_id} is not an enabled active participant"
            ),
            Self::TargetIgnoresGroup(profile_id) => write!(
                formatter,
                "group target {profile_id} does not accept group routing"
            ),
            Self::UnexpectedTargets => {
                formatter.write_str("all-participants routing cannot carry selected targets")
            }
            Self::FanoutExceeded { selected, maximum } => write!(
                formatter,
                "group mention fanout {selected} exceeds bound {maximum}"
            ),
        }
    }
}

impl std::error::Error for GroupMentionRouteError {}

pub fn route_group_mentions(
    request: GroupMentionRouteRequest,
) -> Result<GroupMentionRouteDecision, GroupMentionRouteError> {
    if request.max_fanout == 0 || request.max_fanout > MAX_GROUP_MENTION_FANOUT {
        return Err(GroupMentionRouteError::InvalidFanout);
    }
    if request.participants.len() > MAX_GROUP_ROUTE_PARTICIPANTS {
        return Err(GroupMentionRouteError::TooManyParticipants);
    }

    let mut participants = BTreeMap::new();
    for participant in request.participants {
        let profile_id = participant.profile_id.clone();
        if participants
            .insert(profile_id.clone(), participant)
            .is_some()
        {
            return Err(GroupMentionRouteError::DuplicateParticipant(profile_id));
        }
    }
    let sender = participants
        .get(&request.sender_profile_id)
        .filter(|participant| participant.enabled && participant.active_member)
        .ok_or_else(|| {
            GroupMentionRouteError::SenderNotAuthorized(request.sender_profile_id.clone())
        })?;
    let sender_membership_revision = sender.membership_revision.clone();

    if matches!(request.policy, GroupMentionRoutingPolicy::AllParticipants)
        && !request.requested_targets.is_empty()
    {
        return Err(GroupMentionRouteError::UnexpectedTargets);
    }
    if request.requested_targets.len() > MAX_GROUP_ROUTE_PARTICIPANTS {
        return Err(GroupMentionRouteError::TooManyParticipants);
    }

    let mut selected = BTreeSet::new();
    let mut seen_targets = BTreeSet::new();
    match request.policy {
        GroupMentionRoutingPolicy::AllParticipants => {
            for participant in participants.values() {
                if participant.profile_id != request.sender_profile_id
                    && participant.enabled
                    && participant.active_member
                    && !matches!(participant.mention_policy, GroupMentionPolicy::Ignore)
                {
                    selected.insert(participant.profile_id.clone());
                }
            }
        }
        GroupMentionRoutingPolicy::ExplicitOnly
        | GroupMentionRoutingPolicy::CoordinatorSelected => {
            for profile_id in request.requested_targets {
                if !seen_targets.insert(profile_id.clone()) {
                    return Err(GroupMentionRouteError::DuplicateTarget(profile_id));
                }
                if profile_id == request.sender_profile_id {
                    continue;
                }
                let participant = participants
                    .get(&profile_id)
                    .filter(|participant| participant.enabled && participant.active_member)
                    .ok_or_else(|| {
                        GroupMentionRouteError::TargetNotAuthorized(profile_id.clone())
                    })?;
                if matches!(participant.mention_policy, GroupMentionPolicy::Ignore) {
                    return Err(GroupMentionRouteError::TargetIgnoresGroup(profile_id));
                }
                selected.insert(profile_id);
            }
        }
    }

    if selected.len() > request.max_fanout {
        return Err(GroupMentionRouteError::FanoutExceeded {
            selected: selected.len(),
            maximum: request.max_fanout,
        });
    }
    let targets = selected
        .into_iter()
        .map(|profile_id| {
            let participant = participants
                .get(&profile_id)
                .expect("selected targets were validated against canonical membership");
            GroupMentionRouteTarget {
                profile_id,
                membership_revision: participant.membership_revision.clone(),
            }
        })
        .collect();
    Ok(GroupMentionRouteDecision {
        sender_profile_id: request.sender_profile_id,
        sender_membership_revision,
        selection: request.policy,
        targets,
    })
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelRouteMatcher {
    pub channel: Option<String>,
    pub account: Option<String>,
    pub conversation: Option<String>,
    pub thread: Option<String>,
    pub sender: Option<String>,
    pub command_prefix: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelRouteRule {
    pub version: SchemaVersion,
    pub id: EntityId,
    pub profile_id: ProfileId,
    pub priority: i32,
    pub enabled: bool,
    pub matcher: ChannelRouteMatcher,
    pub session_policy: ChannelSessionPolicy,
    pub group_policy: GroupRoutePolicy,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RegisteredChannelRoute {
    pub rule: ChannelRouteRule,
    pub revision: Revision,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ChannelRoutePurpose {
    Inbound,
    ProactivePost,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ChannelRouteContext {
    pub channel: String,
    pub account: String,
    pub conversation: String,
    pub thread: Option<String>,
    pub sender: String,
    pub caller: String,
    pub text: String,
    pub explicit_profile: Option<ProfileId>,
    pub explicit_session: Option<SessionId>,
    pub is_group: bool,
    pub mentions_profile: bool,
    pub purpose: ChannelRoutePurpose,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum MemoryPartition {
    Shared(String),
    Participant(String),
    Disabled,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResolvedChannelRoute {
    pub route_id: EntityId,
    pub profile: RegisteredProfile,
    pub session_id: SessionId,
    pub session_key: String,
    pub memory_partition: MemoryPartition,
    pub retain_participant: bool,
    pub group_policy: GroupRoutePolicy,
    pub is_group: bool,
}

impl ResolvedChannelRoute {
    pub fn tools_allowed_for(&self, principal: &str) -> bool {
        !self.is_group || authority_allows(&self.group_policy.tools, principal)
    }

    pub fn schedules_allowed_for(&self, principal: &str) -> bool {
        !self.is_group || authority_allows(&self.group_policy.schedules, principal)
    }

    pub const fn proactive_posts_allowed(&self) -> bool {
        !self.is_group
            || matches!(
                self.group_policy.proactive_posts,
                ProactivePostPolicy::Allowed
            )
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct StoredChannelRoute {
    rule: ChannelRouteRule,
    session_bindings: BTreeMap<String, SessionId>,
}

#[derive(Debug, Error)]
pub enum RouteError {
    #[error("no profile matches the requested route")]
    Missing,
    #[error("the route matches more than one profile")]
    Ambiguous,
    #[error("profile {0} is disabled")]
    Disabled(ProfileId),
    #[error("the caller is not authorized for profile {0}")]
    Unauthorized(ProfileId),
    #[error("reply channel {channel} is not enabled for profile {profile}")]
    ChannelDisabled { profile: ProfileId, channel: String },
    #[error("route input is invalid: {0}")]
    Invalid(String),
    #[error("the model registry route differs from the profile snapshot")]
    ModelRouteMismatch,
    #[error("the requested route does not match the durable session identity")]
    SessionIdentityMismatch,
    #[error("the session has no resolved profile snapshot")]
    MissingSnapshot,
    #[error("profile registry failed: {0}")]
    Profile(#[from] ProfileError),
    #[error("model registry failed: {0}")]
    Model(#[from] RegistryError),
    #[error("session storage failed: {0}")]
    Session(#[from] SessionStoreError),
    #[error("route snapshot serialization failed: {0}")]
    Serialize(#[from] serde_json::Error),
    #[error("channel route repository failed: {0}")]
    Repository(String),
    #[error("channel route record is corrupt: {0}")]
    Corrupt(String),
    #[error("channel route {0} already exists")]
    RouteAlreadyExists(EntityId),
    #[error("channel route {0} was not found")]
    RouteNotFound(EntityId),
    #[error("channel route revision is stale")]
    StaleRoute,
    #[error("channel route {0} is disabled")]
    RouteDisabled(EntityId),
    #[error("profile {0} referenced by the route is unavailable")]
    ProfileUnavailable(ProfileId),
    #[error("this group route requires an explicit mention")]
    MentionRequired,
    #[error("this group route ignores inbound messages")]
    GroupIgnored,
    #[error("proactive posts are denied for this group route")]
    ProactivePostDenied,
}

pub struct ChannelRouteTable<R, P> {
    routes: R,
    profiles: ProfileRegistry<P>,
}

impl<R, P> ChannelRouteTable<R, P>
where
    R: RouteRepository,
    R::Error: Display,
    P: ProfileRepository,
{
    pub const fn new(routes: R, profiles: ProfileRegistry<P>) -> Self {
        Self { routes, profiles }
    }

    /// # Errors
    ///
    /// Returns an error for invalid rules, unavailable profiles, duplicates, or persistence failure.
    pub fn register(
        &self,
        rule: ChannelRouteRule,
        now: UtcTimestamp,
    ) -> Result<RegisteredChannelRoute, RouteError> {
        validate_rule(&rule)?;
        self.require_profile(&rule.profile_id)?;
        if self.load(&rule.id)?.is_some() {
            return Err(RouteError::RouteAlreadyExists(rule.id));
        }
        let stored = StoredChannelRoute {
            rule: rule.clone(),
            session_bindings: BTreeMap::new(),
        };
        self.routes
            .put_route(
                encode_channel_route(&stored, Revision::ZERO, now)?,
                WritePrecondition::Missing,
            )
            .map_err(route_repository_error)?;
        Ok(RegisteredChannelRoute {
            rule,
            revision: Revision::ZERO,
            updated_at: now,
        })
    }

    /// # Errors
    ///
    /// Returns an error for stale revisions, invalid rules, unavailable profiles, or persistence failure.
    pub fn update(
        &self,
        rule: ChannelRouteRule,
        expected: Revision,
        now: UtcTimestamp,
    ) -> Result<RegisteredChannelRoute, RouteError> {
        validate_rule(&rule)?;
        self.require_profile(&rule.profile_id)?;
        let current = self
            .load(&rule.id)?
            .ok_or_else(|| RouteError::RouteNotFound(rule.id.clone()))?;
        if current.revision != expected {
            return Err(RouteError::StaleRoute);
        }
        let revision = expected.checked_next().ok_or(RouteError::StaleRoute)?;
        let session_bindings = if current.stored.rule.profile_id == rule.profile_id
            && current.stored.rule.session_policy == rule.session_policy
        {
            current.stored.session_bindings
        } else {
            BTreeMap::new()
        };
        let stored = StoredChannelRoute {
            rule: rule.clone(),
            session_bindings,
        };
        self.routes
            .put_route(
                encode_channel_route(&stored, revision, now)?,
                WritePrecondition::Exact(expected),
            )
            .map_err(route_repository_error)?;
        Ok(RegisteredChannelRoute {
            rule,
            revision,
            updated_at: now,
        })
    }

    /// # Errors
    ///
    /// Returns an error when route persistence or decoding fails.
    pub fn list(&self) -> Result<Vec<RegisteredChannelRoute>, RouteError> {
        let mut routes = self
            .routes
            .list_routes()
            .map_err(route_repository_error)?
            .into_iter()
            .map(decode_channel_route)
            .map(|result| {
                result.map(|stored| RegisteredChannelRoute {
                    rule: stored.stored.rule,
                    revision: stored.revision,
                    updated_at: stored.updated_at,
                })
            })
            .collect::<Result<Vec<_>, _>>()?;
        routes.sort_by(|left, right| left.rule.id.cmp(&right.rule.id));
        Ok(routes)
    }

    /// # Errors
    ///
    /// Returns an error for missing or stale routes and persistence failure.
    pub fn delete(&self, id: &EntityId, expected: Revision) -> Result<(), RouteError> {
        let current = self
            .load(id)?
            .ok_or_else(|| RouteError::RouteNotFound(id.clone()))?;
        if current.revision != expected {
            return Err(RouteError::StaleRoute);
        }
        self.routes
            .delete_route(id, WritePrecondition::Exact(expected))
            .map_err(route_repository_error)?;
        Ok(())
    }

    /// Resolves a channel message and durably binds the configured session identity.
    ///
    /// # Errors
    ///
    /// Returns a fail-closed error for invalid, missing, ambiguous, disabled, unauthorized,
    /// deleted-profile, or forbidden group routes.
    pub fn resolve(
        &self,
        context: &ChannelRouteContext,
        now: UtcTimestamp,
    ) -> Result<ResolvedChannelRoute, RouteError> {
        validate_channel_context(context)?;
        let selected = self.select(context)?;
        if !selected.stored.rule.enabled {
            return Err(RouteError::RouteDisabled(selected.stored.rule.id));
        }
        let profile = self
            .profiles
            .get(&selected.stored.rule.profile_id)?
            .ok_or_else(|| {
                RouteError::ProfileUnavailable(selected.stored.rule.profile_id.clone())
            })?;
        if !profile.enabled {
            return Err(RouteError::Disabled(profile.profile.id));
        }
        if !profile.authorized_callers.contains(&context.caller) {
            return Err(RouteError::Unauthorized(profile.profile.id));
        }
        if !profile.profile.channels.contains(&context.channel) {
            return Err(RouteError::ChannelDisabled {
                profile: profile.profile.id,
                channel: context.channel.clone(),
            });
        }
        enforce_group_policy(&selected.stored.rule.group_policy, context)?;
        let session_key = session_key(&selected.stored.rule, context)?;
        let session_id = if matches!(
            selected.stored.rule.session_policy,
            ChannelSessionPolicy::ExplicitOnly
        ) {
            context.explicit_session.clone().ok_or_else(|| {
                RouteError::Invalid("explicit-only routing requires an explicit session".into())
            })?
        } else if let Some(session_id) = selected.stored.session_bindings.get(&session_key) {
            session_id.clone()
        } else {
            self.bind_session(&selected, &session_key, now)?
        };
        let memory_partition = memory_partition(&selected.stored.rule.group_policy, context);
        Ok(ResolvedChannelRoute {
            route_id: selected.stored.rule.id,
            profile,
            session_id,
            session_key,
            memory_partition,
            retain_participant: !context.is_group
                || matches!(
                    selected.stored.rule.group_policy.participant_retention,
                    ParticipantRetentionPolicy::Retain
                ),
            group_policy: selected.stored.rule.group_policy,
            is_group: context.is_group,
        })
    }

    fn select(&self, context: &ChannelRouteContext) -> Result<LoadedChannelRoute, RouteError> {
        let routes = self
            .routes
            .list_routes()
            .map_err(route_repository_error)?
            .into_iter()
            .map(decode_channel_route)
            .collect::<Result<Vec<_>, _>>()?;
        let mut matching = routes
            .into_iter()
            .filter(|route| rule_matches(&route.stored.rule, context))
            .collect::<Vec<_>>();
        let priority = matching
            .iter()
            .map(|route| route.stored.rule.priority)
            .max()
            .ok_or(RouteError::Missing)?;
        matching.retain(|route| route.stored.rule.priority == priority);
        if matching.len() != 1 {
            return Err(RouteError::Ambiguous);
        }
        matching.pop().ok_or(RouteError::Missing)
    }

    fn bind_session(
        &self,
        selected: &LoadedChannelRoute,
        key: &str,
        now: UtcTimestamp,
    ) -> Result<SessionId, RouteError> {
        let mut stored = selected.stored.clone();
        let session_id = SessionId::new();
        stored
            .session_bindings
            .insert(key.to_owned(), session_id.clone());
        let revision = selected
            .revision
            .checked_next()
            .ok_or(RouteError::StaleRoute)?;
        self.routes
            .put_route(
                encode_channel_route(&stored, revision, now)?,
                WritePrecondition::Exact(selected.revision),
            )
            .map_err(route_repository_error)?;
        Ok(session_id)
    }

    fn load(&self, id: &EntityId) -> Result<Option<LoadedChannelRoute>, RouteError> {
        self.routes
            .get_route(id)
            .map_err(route_repository_error)?
            .map(decode_channel_route)
            .transpose()
    }

    fn require_profile(&self, id: &ProfileId) -> Result<RegisteredProfile, RouteError> {
        self.profiles
            .get(id)?
            .ok_or_else(|| RouteError::ProfileUnavailable(id.clone()))
    }
}

struct LoadedChannelRoute {
    stored: StoredChannelRoute,
    revision: Revision,
    updated_at: UtcTimestamp,
}

pub struct RouteResolver<'a, R> {
    profiles: &'a ProfileRegistry<R>,
    models: &'a ModelRegistry,
    sessions: &'a SessionStore,
}

impl<'a, R> RouteResolver<'a, R>
where
    R: ProfileRepository,
{
    pub const fn new(
        profiles: &'a ProfileRegistry<R>,
        models: &'a ModelRegistry,
        sessions: &'a SessionStore,
    ) -> Self {
        Self {
            profiles,
            models,
            sessions,
        }
    }

    /// Deliberately binds a profile's configured model route to discovered real providers.
    ///
    /// # Errors
    ///
    /// Returns an error when a provider/model is missing or the route is invalid.
    pub fn synchronize_model_route(&self, profile: &RegisteredProfile) -> Result<(), RouteError> {
        let configured = &profile.profile.model_route;
        self.models.set_profile_route(
            profile.profile.id.clone(),
            RegistryModelRoute {
                primary: RegistryModelSelection {
                    provider: configured.provider.clone(),
                    model: configured.model.clone(),
                    credential_ref: configured.credential_ref.clone(),
                },
                fallbacks: configured
                    .fallbacks
                    .iter()
                    .map(|selection| RegistryModelSelection {
                        provider: selection.provider.clone(),
                        model: selection.model.clone(),
                        credential_ref: None,
                    })
                    .collect(),
                classification: None,
                summarization: None,
                review: None,
                vision: None,
            },
        )?;
        Ok(())
    }

    /// Resolves every profile, workspace, model, policy, and reply decision before session work.
    ///
    /// # Errors
    ///
    /// Returns visible errors for missing, ambiguous, disabled, unauthorized, or invalid routes.
    pub fn resolve(
        &self,
        request: &RouteRequest,
        now: UtcTimestamp,
    ) -> Result<ResolvedSessionRoute, RouteError> {
        validate_request(request)?;
        let profiles = self.profiles.list()?;
        let mut candidates = profiles
            .into_iter()
            .filter(|profile| {
                request
                    .profile_id
                    .as_ref()
                    .is_none_or(|id| &profile.profile.id == id)
                    && request
                        .workspace_id
                        .as_ref()
                        .is_none_or(|id| &profile.profile.workspace_id == id)
            })
            .collect::<Vec<_>>();
        if candidates.is_empty() {
            return Err(RouteError::Missing);
        }
        if candidates.len() != 1 {
            return Err(RouteError::Ambiguous);
        }
        let profile = candidates.pop().ok_or(RouteError::Missing)?;
        if !profile.enabled {
            return Err(RouteError::Disabled(profile.profile.id));
        }
        if !profile.authorized_callers.contains(&request.caller) {
            return Err(RouteError::Unauthorized(profile.profile.id));
        }
        if !profile.profile.channels.contains(&request.reply.channel) {
            return Err(RouteError::ChannelDisabled {
                profile: profile.profile.id,
                channel: request.reply.channel.clone(),
            });
        }
        let model_route = self
            .models
            .resolve(&profile.profile.id, ModelPurpose::Primary)?;
        ensure_model_route_matches(&profile, &model_route)?;
        let primary = model_route
            .candidates
            .first()
            .ok_or(RouteError::ModelRouteMismatch)?;
        let snapshot = ResolvedProfileSnapshot {
            profile,
            model: ResolvedModelConfiguration {
                provider: primary.selection.provider.clone(),
                model: primary.selection.model.clone(),
                credential_ref: primary.selection.credential_ref.clone(),
            },
            reply: request.reply.clone(),
            session_policy: request.session_policy.clone(),
            resolved_at: now,
        };
        Ok(ResolvedSessionRoute {
            snapshot,
            model_route,
        })
    }

    /// Resolves first and only then creates the durable root session with a pinned snapshot.
    ///
    /// # Errors
    ///
    /// Returns an error without creating a session when any route decision fails.
    pub fn create_root(
        &self,
        request: &RouteRequest,
        new_session: NewRootSession,
    ) -> Result<(SessionManifest, ResolvedSessionRoute), RouteError> {
        let resolved = self.resolve(request, new_session.created_at)?;
        let metadata = snapshot_metadata(&resolved.snapshot)?;
        let manifest = self.sessions.create(NewSession {
            kind: SessionKind::Root,
            session_id: new_session.session_id,
            root_tree_id: new_session.root_tree_id,
            parent_session_id: None,
            profile_id: resolved.snapshot.profile.profile.id.clone(),
            workspace_id: resolved.snapshot.profile.profile.workspace_id.clone(),
            created_at: new_session.created_at,
            label: new_session.label,
            profile_snapshot: Some(metadata),
        })?;
        Ok((manifest, resolved))
    }

    /// Resolves the current authorized route before resume and keeps or deliberately refreshes the snapshot.
    ///
    /// # Errors
    ///
    /// Returns an error for route/session mismatch, missing snapshots, or stale updates.
    pub fn prepare_resume(
        &self,
        session_id: &SessionId,
        request: &RouteRequest,
        writer_identity: Option<WriterIdentity>,
        now: UtcTimestamp,
    ) -> Result<ResolvedProfileSnapshot, RouteError> {
        let resolved = self.resolve(request, now)?;
        let manifest = self.sessions.manifest(session_id)?;
        if manifest.profile_id != resolved.snapshot.profile.profile.id
            || manifest.workspace_id != resolved.snapshot.profile.profile.workspace_id
        {
            return Err(RouteError::SessionIdentityMismatch);
        }
        let pinned = manifest
            .profile_snapshot
            .ok_or(RouteError::MissingSnapshot)?;
        let pinned_snapshot: ResolvedProfileSnapshot =
            serde_json::from_value(pinned.snapshot.clone())?;
        match request.session_policy.profile_refresh {
            ProfileRefreshPolicy::KeepPinned => Ok(pinned_snapshot),
            ProfileRefreshPolicy::ApplyLatest => {
                let identity = writer_identity.ok_or_else(|| {
                    RouteError::Invalid(
                        "applying a profile update requires an explicit writer identity".into(),
                    )
                })?;
                let replacement = snapshot_metadata(&resolved.snapshot)?;
                let mut writer = self.sessions.acquire_writer(session_id, identity)?;
                writer.update_profile_snapshot(Some(&pinned.digest), replacement)?;
                Ok(resolved.snapshot)
            }
        }
    }
}

fn validate_rule(rule: &ChannelRouteRule) -> Result<(), RouteError> {
    if rule.version.major != CURRENT_SCHEMA_VERSION.major
        || rule.version.minor > CURRENT_SCHEMA_VERSION.minor
    {
        return Err(RouteError::Invalid(
            "channel route schema version is unsupported".into(),
        ));
    }
    let values = [
        rule.matcher.channel.as_deref(),
        rule.matcher.account.as_deref(),
        rule.matcher.conversation.as_deref(),
        rule.matcher.thread.as_deref(),
        rule.matcher.sender.as_deref(),
        rule.matcher.command_prefix.as_deref(),
    ];
    if values.into_iter().flatten().any(str::is_empty) {
        return Err(RouteError::Invalid(
            "channel route match values must be non-empty".into(),
        ));
    }
    for authority in [&rule.group_policy.tools, &rule.group_policy.schedules] {
        if let GroupAuthority::AllowList(principals) = authority
            && (principals.is_empty() || principals.iter().any(String::is_empty))
        {
            return Err(RouteError::Invalid(
                "group authority lists must be non-empty".into(),
            ));
        }
    }
    Ok(())
}

fn validate_channel_context(context: &ChannelRouteContext) -> Result<(), RouteError> {
    if context.channel.is_empty()
        || context.account.is_empty()
        || context.conversation.is_empty()
        || context.sender.is_empty()
        || context.caller.is_empty()
        || context.thread.as_ref().is_some_and(String::is_empty)
    {
        return Err(RouteError::Invalid(
            "channel, account, conversation, sender, caller, and present thread must be non-empty"
                .into(),
        ));
    }
    Ok(())
}

fn rule_matches(rule: &ChannelRouteRule, context: &ChannelRouteContext) -> bool {
    context
        .explicit_profile
        .as_ref()
        .is_none_or(|profile| profile == &rule.profile_id)
        && rule
            .matcher
            .channel
            .as_ref()
            .is_none_or(|value| value == &context.channel)
        && rule
            .matcher
            .account
            .as_ref()
            .is_none_or(|value| value == &context.account)
        && rule
            .matcher
            .conversation
            .as_ref()
            .is_none_or(|value| value == &context.conversation)
        && rule
            .matcher
            .thread
            .as_ref()
            .is_none_or(|value| context.thread.as_ref() == Some(value))
        && rule
            .matcher
            .sender
            .as_ref()
            .is_none_or(|value| value == &context.sender)
        && rule
            .matcher
            .command_prefix
            .as_ref()
            .is_none_or(|prefix| context.text.starts_with(prefix))
}

fn enforce_group_policy(
    policy: &GroupRoutePolicy,
    context: &ChannelRouteContext,
) -> Result<(), RouteError> {
    if !context.is_group {
        return Ok(());
    }
    match policy.mention {
        GroupMentionPolicy::RequireMention if !context.mentions_profile => {
            return Err(RouteError::MentionRequired);
        }
        GroupMentionPolicy::Ignore if matches!(context.purpose, ChannelRoutePurpose::Inbound) => {
            return Err(RouteError::GroupIgnored);
        }
        GroupMentionPolicy::Always
        | GroupMentionPolicy::RequireMention
        | GroupMentionPolicy::Ignore => {}
    }
    if matches!(context.purpose, ChannelRoutePurpose::ProactivePost)
        && matches!(policy.proactive_posts, ProactivePostPolicy::Denied)
    {
        return Err(RouteError::ProactivePostDenied);
    }
    Ok(())
}

fn session_key(
    rule: &ChannelRouteRule,
    context: &ChannelRouteContext,
) -> Result<String, RouteError> {
    let base = format!(
        "{}\u{1f}{}\u{1f}{}",
        context.channel, context.account, context.conversation
    );
    match rule.session_policy {
        ChannelSessionPolicy::PerConversation => Ok(base),
        ChannelSessionPolicy::PerSender => Ok(format!("{base}\u{1f}sender:{}", context.sender)),
        ChannelSessionPolicy::PerThread => context
            .thread
            .as_ref()
            .map(|thread| format!("{base}\u{1f}thread:{thread}"))
            .ok_or_else(|| {
                RouteError::Invalid("per-thread routing requires a thread identity".into())
            }),
        ChannelSessionPolicy::ExplicitOnly => context
            .explicit_session
            .as_ref()
            .map(|session| format!("explicit:{session}"))
            .ok_or_else(|| {
                RouteError::Invalid("explicit-only routing requires an explicit session".into())
            }),
    }
}

fn memory_partition(policy: &GroupRoutePolicy, context: &ChannelRouteContext) -> MemoryPartition {
    if !context.is_group {
        return MemoryPartition::Participant(context.sender.clone());
    }
    match policy.memory {
        GroupMemoryPolicy::Shared => MemoryPartition::Shared(context.conversation.clone()),
        GroupMemoryPolicy::PrivatePerParticipant => {
            MemoryPartition::Participant(context.sender.clone())
        }
        GroupMemoryPolicy::Disabled => MemoryPartition::Disabled,
    }
}

fn authority_allows(authority: &GroupAuthority, principal: &str) -> bool {
    match authority {
        GroupAuthority::Disabled => false,
        GroupAuthority::ProfileCallers => true,
        GroupAuthority::AllowList(principals) => principals.contains(principal),
    }
}

fn encode_channel_route(
    route: &StoredChannelRoute,
    revision: Revision,
    updated_at: UtcTimestamp,
) -> Result<VersionedRecord, RouteError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: route.rule.id.clone(),
        revision,
        updated_at,
        payload: serde_json::to_value(route)?,
    })
}

fn decode_channel_route(record: VersionedRecord) -> Result<LoadedChannelRoute, RouteError> {
    if record.version.major != CURRENT_SCHEMA_VERSION.major
        || record.version.minor > CURRENT_SCHEMA_VERSION.minor
    {
        return Err(RouteError::Corrupt(
            "unsupported channel route record version".into(),
        ));
    }
    let stored: StoredChannelRoute = serde_json::from_value(record.payload)
        .map_err(|error| RouteError::Corrupt(error.to_string()))?;
    if stored.rule.id != record.id || stored.rule.version != record.version {
        return Err(RouteError::Corrupt(
            "channel route envelope does not match its payload".into(),
        ));
    }
    validate_rule(&stored.rule).map_err(|error| RouteError::Corrupt(error.to_string()))?;
    Ok(LoadedChannelRoute {
        stored,
        revision: record.revision,
        updated_at: record.updated_at,
    })
}

fn route_repository_error(error: impl Display) -> RouteError {
    RouteError::Repository(error.to_string())
}

fn validate_request(request: &RouteRequest) -> Result<(), RouteError> {
    if request.profile_id.is_none() && request.workspace_id.is_none() {
        return Err(RouteError::Invalid(
            "a profile or workspace selector is required".into(),
        ));
    }
    if request.caller.trim().is_empty()
        || request.reply.channel.trim().is_empty()
        || request.reply.destination.trim().is_empty()
    {
        return Err(RouteError::Invalid(
            "caller, channel, and destination must be non-empty".into(),
        ));
    }
    Ok(())
}

fn ensure_model_route_matches(
    profile: &RegisteredProfile,
    resolved: &ResolvedRoute,
) -> Result<(), RouteError> {
    let configured = &profile.profile.model_route;
    let expected = std::iter::once((
        configured.provider.as_str(),
        configured.model.as_str(),
        configured.credential_ref.as_deref(),
    ))
    .chain(
        configured
            .fallbacks
            .iter()
            .map(|selection| (selection.provider.as_str(), selection.model.as_str(), None)),
    )
    .collect::<Vec<_>>();
    let actual = resolved
        .candidates
        .iter()
        .map(|candidate| {
            (
                candidate.selection.provider.as_str(),
                candidate.selection.model.as_str(),
                candidate.selection.credential_ref.as_deref(),
            )
        })
        .collect::<Vec<_>>();
    if expected == actual {
        Ok(())
    } else {
        Err(RouteError::ModelRouteMismatch)
    }
}

fn snapshot_metadata(
    snapshot: &ResolvedProfileSnapshot,
) -> Result<ProfileSnapshotMetadata, RouteError> {
    Ok(ProfileSnapshotMetadata::new(
        snapshot.profile.profile.id.clone(),
        snapshot.profile.profile.workspace_id.clone(),
        snapshot.profile.revision,
        snapshot.resolved_at,
        serde_json::to_value(snapshot)?,
    )?)
}

#[cfg(test)]
mod tests {
    use std::collections::{BTreeMap, BTreeSet};
    use std::fs;
    use std::io::{Read, Write};
    use std::net::TcpListener;
    use std::sync::Arc;
    use std::thread;

    use keith_agent_types::{
        CURRENT_SCHEMA_VERSION, EntityId, Generation, ProfileId, Revision, TimeZoneName, WorkerId,
    };
    use keith_profile::{
        AgentProfile, AutonomyMode, ModelRoute, NotificationSettings, ProfileAutonomy,
        ProfileResources, RefinementSettings, RegisteredProfile, ThinkingLevel, ToolPermission,
    };
    use keith_provider_adapters::{OpenAiProvider, ProviderHttpConfig};
    use keith_provider_core::{ModelProvider, ProviderCredential};
    use keith_state_store::EmbeddedStore;
    use tempfile::TempDir;

    use super::*;

    fn serve_models() -> (String, thread::JoinHandle<()>) {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let handle = thread::spawn(move || {
            let (mut stream, _) = listener.accept().unwrap();
            let mut request = Vec::new();
            let mut buffer = [0_u8; 1_024];
            while !request.windows(4).any(|window| window == b"\r\n\r\n") {
                let read = stream.read(&mut buffer).unwrap();
                if read == 0 {
                    break;
                }
                request.extend_from_slice(&buffer[..read]);
            }
            assert!(String::from_utf8_lossy(&request).starts_with("GET /v1/models "));
            let body = r#"{"data":[{"id":"model-a"},{"id":"model-b"}]}"#;
            write!(
                stream,
                "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
                body.len()
            )
            .unwrap();
            stream.flush().unwrap();
        });
        (format!("http://{address}"), handle)
    }

    fn registered(
        root: &TempDir,
        name: &str,
        model: &str,
        channel: &str,
        tool: &str,
        caller: &str,
    ) -> RegisteredProfile {
        fs::write(root.path().join("PERSONA.md"), format!("persona {name}")).unwrap();
        fs::write(root.path().join("USER.md"), format!("user {name}")).unwrap();
        fs::write(root.path().join("RULES.md"), format!("rules {name}")).unwrap();
        fs::create_dir(root.path().join("memory")).unwrap();
        fs::create_dir(root.path().join("schedules")).unwrap();
        RegisteredProfile {
            profile: AgentProfile {
                version: CURRENT_SCHEMA_VERSION,
                id: ProfileId::new(),
                display_name: name.into(),
                workspace_id: WorkspaceId::new(),
                persona_file: "PERSONA.md".into(),
                user_file: "USER.md".into(),
                rule_files: vec!["RULES.md".into()],
                model_route: ModelRoute {
                    provider: "openai".into(),
                    model: model.into(),
                    fallbacks: vec![],
                    credential_ref: Some(format!("credential-{name}")),
                },
                thinking: ThinkingLevel::High,
                tool_rules: BTreeMap::from([(tool.into(), ToolPermission::Allow)]),
                enabled_skills: vec![format!("skill-{name}")],
                enabled_mcp_servers: vec![format!("mcp-{name}")],
                enabled_plugins: vec![format!("plugin-{name}")],
                channels: vec![channel.into()],
                autonomy: ProfileAutonomy {
                    mode: AutonomyMode::Bounded,
                    max_children: 2,
                    max_depth: 2,
                    daily_token_budget: 10_000,
                },
                notifications: NotificationSettings {
                    quiet_hours_start: "22:00".into(),
                    quiet_hours_end: "08:00".into(),
                    time_zone: TimeZoneName::parse("Europe/Berlin").unwrap(),
                    daily_limit: 4,
                },
                refinement: RefinementSettings {
                    enabled: true,
                    require_confirmation: true,
                    editable_targets: BTreeSet::from([format!("persona-{name}")]),
                },
            },
            resources: ProfileResources {
                workspace_root: root.path().into(),
                memory_root: root.path().join("memory"),
                schedule_root: root.path().join("schedules"),
            },
            enabled: true,
            authorized_callers: BTreeSet::from([caller.into()]),
            revision: Revision::ZERO,
            updated_at: UtcTimestamp::UNIX_EPOCH,
        }
    }

    fn request(profile: &RegisteredProfile, caller: &str, channel: &str) -> RouteRequest {
        RouteRequest {
            profile_id: Some(profile.profile.id.clone()),
            workspace_id: Some(profile.profile.workspace_id.clone()),
            caller: caller.into(),
            reply: ReplyRoute {
                channel: channel.into(),
                destination: format!("destination-{caller}"),
            },
            session_policy: SessionPolicy {
                profile_refresh: ProfileRefreshPolicy::KeepPinned,
                memory_enabled: true,
                schedules_enabled: true,
            },
        }
    }

    fn writer_identity() -> WriterIdentity {
        WriterIdentity {
            worker_id: WorkerId::new(),
            owner_instance: EntityId::new(),
            generation: Generation::ZERO,
            acquired_at: UtcTimestamp::from_unix_millis(3),
        }
    }

    fn group_policy() -> GroupRoutePolicy {
        GroupRoutePolicy {
            mention: GroupMentionPolicy::RequireMention,
            memory: GroupMemoryPolicy::PrivatePerParticipant,
            participant_retention: ParticipantRetentionPolicy::Ephemeral,
            tools: GroupAuthority::AllowList(BTreeSet::from(["sender-a".into()])),
            schedules: GroupAuthority::Disabled,
            proactive_posts: ProactivePostPolicy::Denied,
        }
    }

    fn channel_rule(
        profile_id: ProfileId,
        priority: i32,
        session_policy: ChannelSessionPolicy,
    ) -> ChannelRouteRule {
        ChannelRouteRule {
            version: CURRENT_SCHEMA_VERSION,
            id: EntityId::new(),
            profile_id,
            priority,
            enabled: true,
            matcher: ChannelRouteMatcher {
                channel: Some("discord".into()),
                account: Some("bot-main".into()),
                conversation: None,
                thread: None,
                sender: None,
                command_prefix: None,
            },
            session_policy,
            group_policy: group_policy(),
        }
    }

    fn channel_context() -> ChannelRouteContext {
        ChannelRouteContext {
            channel: "discord".into(),
            account: "bot-main".into(),
            conversation: "guild-1".into(),
            thread: Some("thread-1".into()),
            sender: "sender-a".into(),
            caller: "gateway".into(),
            text: "!keith help".into(),
            explicit_profile: None,
            explicit_session: None,
            is_group: true,
            mentions_profile: true,
            purpose: ChannelRoutePurpose::Inbound,
        }
    }

    fn channel_table(
        database: &std::path::Path,
    ) -> ChannelRouteTable<EmbeddedStore, EmbeddedStore> {
        let route_store = EmbeddedStore::open(database, None).unwrap();
        let profile_store = EmbeddedStore::open(database, None).unwrap();
        ChannelRouteTable::new(route_store, ProfileRegistry::new(profile_store))
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn channel_routes_are_durable_deterministic_and_fail_closed() {
        let database_root = TempDir::new().unwrap();
        let database = database_root.path().join("state.db");
        let setup_store = EmbeddedStore::open(&database, None).unwrap();
        let setup_profiles = ProfileRegistry::new(setup_store);
        let workspace_a = TempDir::new().unwrap();
        let workspace_b = TempDir::new().unwrap();
        let profile_a = setup_profiles
            .register(registered(
                &workspace_a,
                "route-a",
                "model-a",
                "discord",
                "read",
                "gateway",
            ))
            .unwrap();
        let profile_b = setup_profiles
            .register(registered(
                &workspace_b,
                "route-b",
                "model-b",
                "discord",
                "shell",
                "gateway",
            ))
            .unwrap();
        drop(setup_profiles);

        let table = channel_table(&database);
        let default = table
            .register(
                channel_rule(
                    profile_b.profile.id.clone(),
                    10,
                    ChannelSessionPolicy::PerConversation,
                ),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let mut specific_rule = channel_rule(
            profile_a.profile.id.clone(),
            20,
            ChannelSessionPolicy::PerSender,
        );
        specific_rule.matcher.conversation = Some("guild-1".into());
        specific_rule.matcher.command_prefix = Some("!keith".into());
        let specific = table
            .register(specific_rule, UtcTimestamp::UNIX_EPOCH)
            .unwrap();

        let context = channel_context();
        let first = table
            .resolve(&context, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        assert_eq!(first.route_id, specific.rule.id);
        assert_eq!(first.profile.profile.id, profile_a.profile.id);
        assert_eq!(
            first.memory_partition,
            MemoryPartition::Participant("sender-a".into())
        );
        assert!(!first.retain_participant);
        assert!(first.tools_allowed_for("sender-a"));
        assert!(!first.tools_allowed_for("sender-b"));
        assert!(!first.schedules_allowed_for("sender-a"));
        assert!(!first.proactive_posts_allowed());
        let replay = table
            .resolve(&context, UtcTimestamp::from_unix_millis(2))
            .unwrap();
        assert_eq!(replay.session_id, first.session_id);

        drop(table);
        let table = channel_table(&database);
        let after_restart = table
            .resolve(&context, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        assert_eq!(after_restart.session_id, first.session_id);
        let mut other_sender = context.clone();
        other_sender.sender = "sender-b".into();
        let isolated = table
            .resolve(&other_sender, UtcTimestamp::from_unix_millis(4))
            .unwrap();
        assert_ne!(isolated.session_id, first.session_id);

        let mut without_prefix = context.clone();
        without_prefix.text = "ordinary message".into();
        let fallback = table
            .resolve(&without_prefix, UtcTimestamp::from_unix_millis(5))
            .unwrap();
        assert_eq!(fallback.route_id, default.rule.id);
        assert_eq!(fallback.profile.profile.id, profile_b.profile.id);
        let mut explicit = without_prefix.clone();
        explicit.explicit_profile = Some(profile_a.profile.id.clone());
        assert!(matches!(
            table.resolve(&explicit, UtcTimestamp::from_unix_millis(6)),
            Err(RouteError::Missing)
        ));

        let mut no_mention = context.clone();
        no_mention.mentions_profile = false;
        assert!(matches!(
            table.resolve(&no_mention, UtcTimestamp::from_unix_millis(6)),
            Err(RouteError::MentionRequired)
        ));
        let mut proactive = context.clone();
        proactive.purpose = ChannelRoutePurpose::ProactivePost;
        assert!(matches!(
            table.resolve(&proactive, UtcTimestamp::from_unix_millis(6)),
            Err(RouteError::ProactivePostDenied)
        ));

        let current = table
            .list()
            .unwrap()
            .into_iter()
            .find(|route| route.rule.id == specific.rule.id)
            .unwrap();
        let mut updated_rule = current.rule;
        updated_rule.group_policy.proactive_posts = ProactivePostPolicy::Allowed;
        let updated = table
            .update(
                updated_rule,
                current.revision,
                UtcTimestamp::from_unix_millis(7),
            )
            .unwrap();
        let proactive_route = table
            .resolve(&proactive, UtcTimestamp::from_unix_millis(8))
            .unwrap();
        assert_eq!(proactive_route.session_id, first.session_id);
        assert!(proactive_route.proactive_posts_allowed());
        assert!(matches!(
            table.update(
                updated.rule.clone(),
                current.revision,
                UtcTimestamp::from_unix_millis(9)
            ),
            Err(RouteError::StaleRoute)
        ));

        let mut ambiguous = updated.rule.clone();
        ambiguous.id = EntityId::new();
        table
            .register(ambiguous, UtcTimestamp::from_unix_millis(9))
            .unwrap();
        assert!(matches!(
            table.resolve(&context, UtcTimestamp::from_unix_millis(10)),
            Err(RouteError::Ambiguous)
        ));
    }

    #[test]
    fn channel_session_policies_and_unavailable_profiles_are_isolated() {
        let database_root = TempDir::new().unwrap();
        let database = database_root.path().join("state.db");
        let setup_store = EmbeddedStore::open(&database, None).unwrap();
        let setup_profiles = ProfileRegistry::new(setup_store);
        let workspace = TempDir::new().unwrap();
        let profile = setup_profiles
            .register(registered(
                &workspace, "route", "model-a", "discord", "read", "gateway",
            ))
            .unwrap();
        drop(setup_profiles);

        let table = channel_table(&database);
        let mut context = channel_context();
        context.is_group = false;
        let mut thread_rule = channel_rule(
            profile.profile.id.clone(),
            10,
            ChannelSessionPolicy::PerThread,
        );
        thread_rule.matcher.thread = Some("thread-1".into());
        let thread_route = table
            .register(thread_rule, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let threaded = table
            .resolve(&context, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        context.thread = None;
        assert!(matches!(
            table.resolve(&context, UtcTimestamp::from_unix_millis(2)),
            Err(RouteError::Missing)
        ));
        let listed = table
            .list()
            .unwrap()
            .into_iter()
            .find(|route| route.rule.id == thread_route.rule.id)
            .unwrap();
        let mut explicit_rule = listed.rule;
        explicit_rule.matcher.thread = None;
        explicit_rule.session_policy = ChannelSessionPolicy::ExplicitOnly;
        let explicit_route = table
            .update(
                explicit_rule,
                listed.revision,
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert!(matches!(
            table.resolve(&context, UtcTimestamp::from_unix_millis(3)),
            Err(RouteError::Invalid(_))
        ));
        let explicit_session = SessionId::new();
        context.explicit_profile = Some(profile.profile.id.clone());
        context.explicit_session = Some(explicit_session.clone());
        let explicit = table
            .resolve(&context, UtcTimestamp::from_unix_millis(4))
            .unwrap();
        assert_eq!(explicit.session_id, explicit_session);
        assert_ne!(explicit.session_id, threaded.session_id);

        let mut disabled = explicit_route.rule.clone();
        disabled.enabled = false;
        let disabled = table
            .update(
                disabled,
                explicit_route.revision,
                UtcTimestamp::from_unix_millis(5),
            )
            .unwrap();
        assert!(matches!(
            table.resolve(&context, UtcTimestamp::from_unix_millis(6)),
            Err(RouteError::RouteDisabled(_))
        ));
        let mut enabled = disabled.rule;
        enabled.enabled = true;
        table
            .update(
                enabled,
                disabled.revision,
                UtcTimestamp::from_unix_millis(7),
            )
            .unwrap();
        context.caller = "intruder".into();
        assert!(matches!(
            table.resolve(&context, UtcTimestamp::from_unix_millis(8)),
            Err(RouteError::Unauthorized(_))
        ));
        context.caller = "gateway".into();

        let admin_store = EmbeddedStore::open(&database, None).unwrap();
        admin_store
            .delete_profile(
                profile.profile.id.as_entity_id(),
                WritePrecondition::Exact(Revision::ZERO),
            )
            .unwrap();
        assert!(matches!(
            table.resolve(&context, UtcTimestamp::from_unix_millis(9)),
            Err(RouteError::ProfileUnavailable(_))
        ));
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn cross_profile_routes_are_real_isolated_durable_and_fail_closed() {
        let (base_url, server) = serve_models();
        let models = ModelRegistry::new();
        let provider: Arc<dyn ModelProvider> =
            Arc::new(OpenAiProvider::new(ProviderHttpConfig::new(base_url).unwrap()).unwrap());
        models.register_provider(provider).unwrap();
        models
            .refresh_models("openai", &ProviderCredential::new("secret").unwrap())
            .unwrap();
        server.join().unwrap();

        let profiles = ProfileRegistry::new(EmbeddedStore::open_in_memory().unwrap());
        let workspace_a = TempDir::new().unwrap();
        let workspace_b = TempDir::new().unwrap();
        let profile_a = profiles
            .register(registered(
                &workspace_a,
                "a",
                "model-a",
                "terminal",
                "read",
                "caller-a",
            ))
            .unwrap();
        let profile_b = profiles
            .register(registered(
                &workspace_b,
                "b",
                "model-b",
                "webhook",
                "shell",
                "caller-b",
            ))
            .unwrap();
        let session_root = TempDir::new().unwrap();
        let sessions = SessionStore::open(session_root.path()).unwrap();
        let resolver = RouteResolver::new(&profiles, &models, &sessions);
        resolver.synchronize_model_route(&profile_a).unwrap();
        resolver.synchronize_model_route(&profile_b).unwrap();

        let resolved_a = resolver
            .resolve(
                &request(&profile_a, "caller-a", "terminal"),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let resolved_b = resolver
            .resolve(
                &request(&profile_b, "caller-b", "webhook"),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert_eq!(resolved_a.snapshot.model.model, "model-a");
        assert_eq!(resolved_b.snapshot.model.model, "model-b");
        assert_ne!(
            resolved_a.snapshot.profile.resources.workspace_root,
            resolved_b.snapshot.profile.resources.workspace_root
        );
        assert_ne!(
            resolved_a.snapshot.profile.resources.memory_root,
            resolved_b.snapshot.profile.resources.memory_root
        );
        assert_ne!(
            resolved_a.snapshot.profile.resources.schedule_root,
            resolved_b.snapshot.profile.resources.schedule_root
        );
        assert!(
            resolved_a
                .snapshot
                .profile
                .profile
                .tool_rules
                .contains_key("read")
        );
        assert!(
            resolved_b
                .snapshot
                .profile
                .profile
                .tool_rules
                .contains_key("shell")
        );
        assert_eq!(resolved_a.snapshot.reply.channel, "terminal");
        assert_eq!(resolved_b.snapshot.reply.channel, "webhook");

        fs::write(
            resolved_a
                .snapshot
                .profile
                .resources
                .memory_root
                .join("entry"),
            "memory-a",
        )
        .unwrap();
        fs::write(
            resolved_b
                .snapshot
                .profile
                .resources
                .schedule_root
                .join("job"),
            "schedule-b",
        )
        .unwrap();
        assert!(
            !resolved_b
                .snapshot
                .profile
                .resources
                .memory_root
                .join("entry")
                .exists()
        );
        assert!(
            !resolved_a
                .snapshot
                .profile
                .resources
                .schedule_root
                .join("job")
                .exists()
        );

        let session_a = SessionId::new();
        let (manifest_a, _) = resolver
            .create_root(
                &request(&profile_a, "caller-a", "terminal"),
                NewRootSession {
                    session_id: session_a.clone(),
                    root_tree_id: RootTreeId::new(),
                    created_at: UtcTimestamp::from_unix_millis(1),
                    label: Some("a".into()),
                },
            )
            .unwrap();
        let session_b = SessionId::new();
        let (manifest_b, _) = resolver
            .create_root(
                &request(&profile_b, "caller-b", "webhook"),
                NewRootSession {
                    session_id: session_b,
                    root_tree_id: RootTreeId::new(),
                    created_at: UtcTimestamp::from_unix_millis(1),
                    label: Some("b".into()),
                },
            )
            .unwrap();
        assert_ne!(manifest_a.profile_id, manifest_b.profile_id);
        assert_ne!(manifest_a.workspace_id, manifest_b.workspace_id);
        assert!(manifest_a.profile_snapshot.is_some());
        assert!(manifest_b.profile_snapshot.is_some());

        let failed_session = SessionId::new();
        let mut unauthorized = request(&profile_a, "intruder", "terminal");
        assert!(matches!(
            resolver.create_root(
                &unauthorized,
                NewRootSession {
                    session_id: failed_session.clone(),
                    root_tree_id: RootTreeId::new(),
                    created_at: UtcTimestamp::from_unix_millis(2),
                    label: None,
                }
            ),
            Err(RouteError::Unauthorized(_))
        ));
        assert!(matches!(
            sessions.manifest(&failed_session),
            Err(SessionStoreError::NotFound(_))
        ));
        unauthorized.caller = "caller-a".into();
        unauthorized.reply.channel = "webhook".into();
        assert!(matches!(
            resolver.resolve(&unauthorized, UtcTimestamp::from_unix_millis(2)),
            Err(RouteError::ChannelDisabled { .. })
        ));
        let mut missing = request(&profile_a, "caller-a", "terminal");
        missing.profile_id = Some(ProfileId::new());
        assert!(matches!(
            resolver.resolve(&missing, UtcTimestamp::from_unix_millis(2)),
            Err(RouteError::Missing)
        ));

        let mut updated_a = profiles.get(&profile_a.profile.id).unwrap().unwrap();
        updated_a.profile.thinking = ThinkingLevel::Low;
        updated_a.updated_at = UtcTimestamp::from_unix_millis(2);
        let updated_a = profiles.update(updated_a, Revision::ZERO).unwrap();
        let pinned = resolver
            .prepare_resume(
                &session_a,
                &request(&updated_a, "caller-a", "terminal"),
                None,
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert_eq!(pinned.profile.profile.thinking, ThinkingLevel::High);
        let mut refresh = request(&updated_a, "caller-a", "terminal");
        refresh.session_policy.profile_refresh = ProfileRefreshPolicy::ApplyLatest;
        let latest = resolver
            .prepare_resume(
                &session_a,
                &refresh,
                Some(writer_identity()),
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        assert_eq!(latest.profile.profile.thinking, ThinkingLevel::Low);
        let stored: ResolvedProfileSnapshot = serde_json::from_value(
            sessions
                .manifest(&session_a)
                .unwrap()
                .profile_snapshot
                .unwrap()
                .snapshot,
        )
        .unwrap();
        assert_eq!(stored.profile.revision, Revision::new(1));

        let mut disabled_b = profiles.get(&profile_b.profile.id).unwrap().unwrap();
        disabled_b.enabled = false;
        disabled_b.updated_at = UtcTimestamp::from_unix_millis(4);
        let disabled_b = profiles.update(disabled_b, Revision::ZERO).unwrap();
        assert!(matches!(
            resolver.resolve(
                &request(&disabled_b, "caller-b", "webhook"),
                UtcTimestamp::from_unix_millis(4)
            ),
            Err(RouteError::Disabled(_))
        ));

        let mut ambiguous_profile = profile_a.clone();
        ambiguous_profile.profile.id = ProfileId::new();
        ambiguous_profile.profile.display_name = "ambiguous".into();
        ambiguous_profile.authorized_callers = BTreeSet::from(["caller-a".into()]);
        ambiguous_profile.revision = Revision::ZERO;
        let ambiguous_profile = profiles.register(ambiguous_profile).unwrap();
        resolver
            .synchronize_model_route(&ambiguous_profile)
            .unwrap();
        let mut ambiguous = request(&profile_a, "caller-a", "terminal");
        ambiguous.profile_id = None;
        assert!(matches!(
            resolver.resolve(&ambiguous, UtcTimestamp::from_unix_millis(5)),
            Err(RouteError::Ambiguous)
        ));
    }
}
