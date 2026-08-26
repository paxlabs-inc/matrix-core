#![forbid(unsafe_code)]

use std::error::Error;

use keith_agent_types::{
    EntityId, Generation, ProfileId, Revision, RootTreeId, SchemaVersion, SessionId, StableKey,
    UtcTimestamp, WorkerId,
};
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Collection {
    WorkerLeases,
    WorkerGenerations,
    SessionCatalog,
    PromptIngress,
    Profiles,
    ProfileExecutionFences,
    ProfileExecutionRegistrations,
    PendingActions,
    Children,
    ChildMessages,
    Goals,
    Plans,
    Commitments,
    WaitingConditions,
    ScheduledJobs,
    JobAttempts,
    RoutingRules,
    ResourceGovernance,
    ChannelOffsets,
    Deliveries,
    AttentionCandidates,
    InitiativeHistory,
    EvolutionTransactions,
    EvolutionLedger,
    EvolutionLedgerHead,
    ToolExperience,
    KernelMetadata,
    ActiveOperations,
    SchemaMigrations,
    Conversations,
    ConversationParticipants,
    ConversationEvents,
    ConversationDeliveries,
    CollaborationRounds,
    Assignments,
    ReadReceipts,
    SharedKnowledgeGrants,
    ComputerRecords,
    TakeoverLeases,
    TeammateAudits,
    LegacySessions,
    MigrationProvenance,
    ConversationBindings,
    DirectMessageKeys,
    ConversationStableKeys,
    ConversationProjectionIntents,
    ConversationUnreadIntents,
    ConversationSearchIntents,
    ConversationPublicationIntents,
    ConversationPublicationOutbox,
    ConversationSupersessions,
    ConversationFinalizationIntents,
    AgentDeleteOperations,
    AgentDeleteReceipts,
    AgentDeleteAudits,
    ComputerAudits,
    AgentProvisionOperations,
    SharedKnowledgeSpaces,
}

impl Collection {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::WorkerLeases => "worker_leases",
            Self::WorkerGenerations => "worker_generations",
            Self::SessionCatalog => "session_catalog",
            Self::PromptIngress => "prompt_ingress",
            Self::Profiles => "profiles",
            Self::ProfileExecutionFences => "profile_execution_fences",
            Self::ProfileExecutionRegistrations => "profile_execution_registrations",
            Self::PendingActions => "pending_actions",
            Self::Children => "children",
            Self::ChildMessages => "child_messages",
            Self::Goals => "goals",
            Self::Plans => "plans",
            Self::Commitments => "commitments",
            Self::WaitingConditions => "waiting_conditions",
            Self::ScheduledJobs => "scheduled_jobs",
            Self::JobAttempts => "job_attempts",
            Self::RoutingRules => "routing_rules",
            Self::ResourceGovernance => "resource_governance",
            Self::ChannelOffsets => "channel_offsets",
            Self::Deliveries => "deliveries",
            Self::AttentionCandidates => "attention_candidates",
            Self::InitiativeHistory => "initiative_history",
            Self::EvolutionTransactions => "evolution_transactions",
            Self::EvolutionLedger => "evolution_ledger",
            Self::EvolutionLedgerHead => "evolution_ledger_head",
            Self::ToolExperience => "tool_experience",
            Self::KernelMetadata => "kernel_metadata",
            Self::ActiveOperations => "active_operations",
            Self::SchemaMigrations => "schema_migrations",
            Self::Conversations => "conversations",
            Self::ConversationParticipants => "conversation_participants",
            Self::ConversationEvents => "conversation_events",
            Self::ConversationDeliveries => "conversation_deliveries",
            Self::CollaborationRounds => "collaboration_rounds",
            Self::Assignments => "assignments",
            Self::ReadReceipts => "read_receipts",
            Self::SharedKnowledgeGrants => "shared_knowledge_grants",
            Self::ComputerRecords => "computer_records",
            Self::TakeoverLeases => "takeover_leases",
            Self::TeammateAudits => "teammate_audits",
            Self::LegacySessions => "legacy_sessions",
            Self::MigrationProvenance => "migration_provenance",
            Self::ConversationBindings => "conversation_bindings",
            Self::DirectMessageKeys => "direct_message_keys",
            Self::ConversationStableKeys => "conversation_stable_keys",
            Self::ConversationProjectionIntents => "conversation_projection_intents",
            Self::ConversationUnreadIntents => "conversation_unread_intents",
            Self::ConversationSearchIntents => "conversation_search_intents",
            Self::ConversationPublicationIntents => "conversation_publication_intents",
            Self::ConversationPublicationOutbox => "conversation_publication_outbox",
            Self::ConversationSupersessions => "conversation_supersessions",
            Self::ConversationFinalizationIntents => "conversation_finalization_intents",
            Self::AgentDeleteOperations => "agent_delete_operations",
            Self::AgentDeleteReceipts => "agent_delete_receipts",
            Self::AgentDeleteAudits => "agent_delete_audits",
            Self::ComputerAudits => "computer_audits",
            Self::AgentProvisionOperations => "agent_provision_operations",
            Self::SharedKnowledgeSpaces => "shared_knowledge_spaces",
        }
    }
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct VersionedRecord {
    pub version: SchemaVersion,
    pub id: EntityId,
    pub revision: Revision,
    pub updated_at: UtcTimestamp,
    pub payload: serde_json::Value,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "condition", content = "revision")]
pub enum WritePrecondition {
    Any,
    Missing,
    Exact(Revision),
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "operation")]
pub enum RecordMutation {
    Put {
        collection: Collection,
        record: VersionedRecord,
        precondition: WritePrecondition,
    },
    Delete {
        collection: Collection,
        id: EntityId,
        precondition: WritePrecondition,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CommitReceipt {
    pub applied_mutations: usize,
}

/// Outcome of the installation-wide, data-control-only evolution ledger erasure.
///
/// This operation deliberately has no profile or session scope: the evolution ledger and its
/// authenticated head are installation-global state.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EvolutionLedgerErasureReport {
    pub deleted_records: usize,
    pub deleted_heads: usize,
    pub remaining_records: usize,
    pub remaining_heads: usize,
}

pub trait ClassifiedRepositoryError: Error + Send + Sync + 'static {
    fn is_conflict(&self) -> bool;
}

pub trait AtomicStateRepository: Send + Sync {
    type Error: ClassifiedRepositoryError;

    /// # Errors
    ///
    /// Returns the backend error when validation, persistence, or commit fails.
    fn transact(&self, mutations: &[RecordMutation]) -> Result<CommitReceipt, Self::Error>;
}

pub trait StateRecordRepository: AtomicStateRepository {
    /// # Errors
    /// Returns the backend error when the record cannot be read or decoded.
    fn get_record(
        &self,
        collection: Collection,
        id: &EntityId,
    ) -> Result<Option<VersionedRecord>, Self::Error>;

    /// # Errors
    /// Returns the backend error when the collection cannot be read or decoded.
    fn list_records(&self, collection: Collection) -> Result<Vec<VersionedRecord>, Self::Error>;

    /// Reads several collections from one repository snapshot.
    ///
    /// # Errors
    /// Returns the backend error when the snapshot cannot be opened or any record cannot be
    /// decoded.
    fn list_records_snapshot(
        &self,
        collections: &[Collection],
    ) -> Result<Vec<(Collection, Vec<VersionedRecord>)>, Self::Error>;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ProfileExecutionFenceState {
    Open,
    Closing,
    Closed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileExecutionFence {
    pub version: SchemaVersion,
    pub profile_id: ProfileId,
    pub state: ProfileExecutionFenceState,
    pub epoch: u64,
    pub revision: Revision,
    pub cancellation_requested_at: Option<UtcTimestamp>,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileExecutionWorkerBinding {
    pub root_tree_id: RootTreeId,
    pub worker_id: WorkerId,
    pub generation: Generation,
    pub lease_authentication: EntityId,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ProfileExecutionRegistrationState {
    Active,
    Completed,
    Reclaimed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileExecutionRegistration {
    pub version: SchemaVersion,
    pub id: EntityId,
    pub profile_id: ProfileId,
    pub profile_revision: Revision,
    pub session_id: SessionId,
    pub worker: ProfileExecutionWorkerBinding,
    pub owner_instance: EntityId,
    pub token: StableKey,
    pub fence_epoch: u64,
    pub lease_expires_at: UtcTimestamp,
    pub state: ProfileExecutionRegistrationState,
    pub cancellation_requested_at: Option<UtcTimestamp>,
    pub admitted_at: UtcTimestamp,
    pub terminal_at: Option<UtcTimestamp>,
    pub revision: Revision,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileExecutionPermit {
    pub registration_id: EntityId,
    pub profile_id: ProfileId,
    pub profile_revision: Revision,
    pub session_id: SessionId,
    pub worker: ProfileExecutionWorkerBinding,
    pub owner_instance: EntityId,
    pub token: StableKey,
    pub fence_epoch: u64,
    pub registration_revision: Revision,
    pub lease_expires_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileExecutionAdmissionRequest {
    pub registration_id: EntityId,
    pub profile_id: ProfileId,
    pub expected_profile_revision: Revision,
    pub expected_fence_epoch: u64,
    pub expected_fence_revision: Revision,
    pub session_id: SessionId,
    pub root_tree_id: RootTreeId,
    pub worker_id: WorkerId,
    pub worker_binding: ProfileExecutionWorkerBinding,
    pub worker_lease_expires_at: UtcTimestamp,
    pub owner_instance: EntityId,
    pub token: StableKey,
    pub lease_expires_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileExecutionCloseRequest {
    pub profile_id: ProfileId,
    pub expected_epoch: u64,
    pub expected_revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileExecutionReopenRequest {
    pub profile_id: ProfileId,
    pub expected_profile_revision: Revision,
    pub expected_epoch: u64,
    pub expected_revision: Revision,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ProfileExecutionMutationStatus {
    Applied,
    Replay,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileExecutionAdmissionOutcome {
    pub status: ProfileExecutionMutationStatus,
    pub permit: ProfileExecutionPermit,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileExecutionFenceSnapshot {
    pub status: ProfileExecutionMutationStatus,
    pub fence: ProfileExecutionFence,
    pub active: Vec<ProfileExecutionRegistration>,
}

pub trait ProfileExecutionAdmissionRepository: StateRecordRepository {
    /// Creates the initial open fence for an enabled profile, or replays the identical state.
    ///
    /// # Errors
    /// Returns the backend error for a missing/disabled profile, conflict, corruption, or commit
    /// failure.
    fn initialize_profile_execution_fence(
        &self,
        profile_id: &ProfileId,
        expected_profile_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error>;

    /// Reads one fence and its active durable registrations.
    ///
    /// # Errors
    /// Returns the backend error when durable state is missing, corrupt, or unavailable.
    fn profile_execution_snapshot(
        &self,
        profile_id: &ProfileId,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error>;

    /// Atomically admits an execution only while the exact enabled profile and open fence remain
    /// current, the session catalog binds the profile to the worker root, and the repository-owned
    /// current worker generation and lease are live. The returned permit contains the exact
    /// generation and lease authentication resolved inside that transaction.
    ///
    /// # Errors
    /// Returns the backend error for stale lifecycle/fence/worker state, duplicate session or token,
    /// invalid lease bounds, corruption, or commit failure.
    fn admit_profile_execution(
        &self,
        request: &ProfileExecutionAdmissionRequest,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionAdmissionOutcome, Self::Error>;

    /// Renews an active execution lease after atomically revalidating its complete permit.
    ///
    /// # Errors
    /// Returns the backend error for stale/cancelled/expired ownership or commit failure.
    fn renew_profile_execution(
        &self,
        permit: &ProfileExecutionPermit,
        lease_expires_at: UtcTimestamp,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionPermit, Self::Error>;

    /// Advances the fence epoch, rejects new admission, durably requests cancellation for every
    /// active registration, and returns those registrations for quiescence.
    ///
    /// # Errors
    /// Returns the backend error for stale fence state, epoch overflow, corruption, or commit
    /// failure.
    fn close_profile_execution_fence(
        &self,
        request: &ProfileExecutionCloseRequest,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error>;

    /// Completes one exact registration. Completion remains permitted after its fence epoch has
    /// closed, but never authorizes further state mutation.
    ///
    /// # Errors
    /// Returns the backend error for mismatched ownership, reclaimed state, corruption, or commit
    /// failure.
    fn complete_profile_execution(
        &self,
        permit: &ProfileExecutionPermit,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error>;

    /// Reclaims registrations whose execution lease expired or whose complete worker lease tuple
    /// is no longer current, and seals a quiescent closing fence.
    ///
    /// # Errors
    /// Returns the backend error for corrupt state, revision overflow, or commit failure.
    fn reclaim_profile_executions(
        &self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error>;

    /// Reopens a closed, quiescent fence only while the exact profile revision remains enabled.
    ///
    /// # Errors
    /// Returns the backend error for stale lifecycle/fence state, active registrations, epoch
    /// overflow, corruption, or commit failure.
    fn reopen_profile_execution_fence(
        &self,
        request: &ProfileExecutionReopenRequest,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error>;

    /// Atomically revalidates the complete active permit, current open epoch, exact enabled profile
    /// revision, and live worker lease before applying the supplied final mutations.
    ///
    /// # Errors
    /// Returns the backend error for stale/cancelled/expired ownership, protected mutations,
    /// precondition conflicts, or commit failure.
    fn transact_profile_execution_commit(
        &self,
        permit: &ProfileExecutionPermit,
        mutations: &[RecordMutation],
        now: UtcTimestamp,
    ) -> Result<CommitReceipt, Self::Error>;
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanonicalConversationAppend {
    pub conversation_id: EntityId,
    pub expected_head_revision: Revision,
    pub expected_next_sequence: u64,
    pub event: VersionedRecord,
    pub head: VersionedRecord,
    pub stable_key: VersionedRecord,
    pub intents: Vec<RecordMutation>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "outcome")]
pub enum CanonicalAppendOutcome {
    Applied { receipt: CommitReceipt },
    Replay { event: VersionedRecord },
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationProjectionRebuild {
    pub conversation_id: EntityId,
    pub expected_head_revision: Revision,
    pub mutations: Vec<RecordMutation>,
}

pub trait CanonicalConversationRepository: StateRecordRepository {
    /// Atomically appends one immutable event, advances its head, reserves its stable key, and
    /// records every supplied projection intent. Byte-identical stable-key replay returns the
    /// original event.
    ///
    /// # Errors
    /// Returns the backend error on invalid records, key collision, stale head, or commit failure.
    fn append_canonical_conversation(
        &self,
        append: &CanonicalConversationAppend,
    ) -> Result<CanonicalAppendOutcome, Self::Error>;

    /// Rebuilds derived projection records only when the canonical head still has the expected
    /// revision. Canonical events and stable-key reservations cannot be changed through this seam.
    ///
    /// # Errors
    /// Returns the backend error on an invalid mutation, stale head, or commit failure.
    fn rebuild_conversation_projections(
        &self,
        rebuild: &ConversationProjectionRebuild,
    ) -> Result<CommitReceipt, Self::Error>;
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanonicalDirectConversationBinding {
    pub key: VersionedRecord,
    pub conversation: VersionedRecord,
    pub participants: Vec<VersionedRecord>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "outcome")]
pub enum CanonicalDirectConversationOutcome {
    Applied { receipt: CommitReceipt },
    Replay { conversation: VersionedRecord },
}

pub trait DirectConversationRepository: StateRecordRepository {
    /// Atomically reserves one immutable direct-message key and creates its conversation and
    /// participant rows. An exact retry returns the durable conversation; a partial or mismatched
    /// retry fails closed.
    ///
    /// # Errors
    /// Returns the backend error for malformed/colliding keys, invalid participant batches,
    /// partial replay, precondition conflict, corruption, or commit failure.
    fn bind_direct_conversation(
        &self,
        binding: &CanonicalDirectConversationBinding,
    ) -> Result<CanonicalDirectConversationOutcome, Self::Error>;
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationDeliveryFinalization {
    pub delivery: VersionedRecord,
    pub expected_delivery_revision: Revision,
    pub finalization_intent: VersionedRecord,
    pub publication_outbox: Option<VersionedRecord>,
    pub supersession: Option<VersionedRecord>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "outcome")]
pub enum ConversationDeliveryFinalizationOutcome {
    Applied { receipt: CommitReceipt },
    Replay { delivery: VersionedRecord },
}

pub trait ConversationDeliveryRepository: StateRecordRepository {
    /// Atomically advances a delivery by exact revision and records its immutable finalization
    /// intent together with an optional publication outbox or targeted supersession row. A
    /// dead-letter transition is represented by the terminal delivery state and bounded safe
    /// error in `delivery`, not by a competing record family.
    ///
    /// # Errors
    /// Returns the backend error for malformed linkage, stale delivery revision, non-immutable
    /// intent rows, partial replay, uniqueness conflict, corruption, or commit failure.
    fn finalize_conversation_delivery(
        &self,
        finalization: &ConversationDeliveryFinalization,
    ) -> Result<ConversationDeliveryFinalizationOutcome, Self::Error>;
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "outcome")]
pub enum AuthoritativeTransitionOutcome {
    Applied { receipt: CommitReceipt },
    Replay { record: VersionedRecord },
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GroupMembershipTransition {
    pub canonical_append: CanonicalConversationAppend,
    pub expected_participant_revision: Revision,
    pub participant_mutations: Vec<RecordMutation>,
}

pub trait GroupMembershipRepository: StateRecordRepository {
    /// Atomically advances a conversation's participant revision and applies only the named
    /// participant membership mutations.
    ///
    /// # Errors
    /// Returns the backend error for stale conversation or participant revisions, malformed
    /// participant linkage, partial replay, corruption, or commit failure.
    fn transition_group_membership(
        &self,
        transition: &GroupMembershipTransition,
    ) -> Result<AuthoritativeTransitionOutcome, Self::Error>;
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CollaborationRoundTransition {
    pub round: VersionedRecord,
    pub precondition: WritePrecondition,
    pub delivery_mutations: Vec<RecordMutation>,
    pub supersessions: Vec<VersionedRecord>,
}

pub trait CollaborationRoundRepository: StateRecordRepository {
    /// Creates or advances one collaboration round while validating immutable scope, legal state
    /// progression, non-increasing budgets, referenced active deliveries, and targeted branch
    /// supersessions in one transaction.
    ///
    /// # Errors
    /// Returns the backend error for stale revisions, invalid state/budget/delivery linkage,
    /// uniqueness conflict, partial replay, corruption, or commit failure.
    fn transition_collaboration_round(
        &self,
        transition: &CollaborationRoundTransition,
    ) -> Result<AuthoritativeTransitionOutcome, Self::Error>;
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AssignmentTransition {
    pub assignment: VersionedRecord,
    pub precondition: WritePrecondition,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AssignmentHandoffTransaction {
    pub assignment: VersionedRecord,
    pub expected_assignment_revision: Revision,
    pub obsolete_delivery_mutations: Vec<RecordMutation>,
    pub new_owner_delivery: VersionedRecord,
    pub handoff_audit: VersionedRecord,
}

pub trait AssignmentRepository: StateRecordRepository {
    /// Creates or advances an assignment using exact revisions, durable monotonic claim fences,
    /// and transaction-time dependency readiness.
    ///
    /// # Errors
    /// Returns the backend error for stale revisions, unmet dependencies, invalid claims or state
    /// transitions, uniqueness conflict, corruption, or commit failure.
    fn transition_assignment(
        &self,
        transition: &AssignmentTransition,
    ) -> Result<AuthoritativeTransitionOutcome, Self::Error>;

    /// Atomically transfers assignment ownership, appends one immutable ownership history row,
    /// updates only the named obsolete deliveries, creates the new-owner delivery, and records the
    /// canonical handoff publication intent.
    ///
    /// # Errors
    /// Returns the backend error for stale ownership/revisions, malformed linkage, incomplete or
    /// mismatched replay, uniqueness conflict, corruption, or commit failure.
    fn handoff_assignment(
        &self,
        handoff: &AssignmentHandoffTransaction,
    ) -> Result<AuthoritativeTransitionOutcome, Self::Error>;
}

pub trait EvolutionLedgerRepository: Send + Sync {
    type Error: ClassifiedRepositoryError;

    /// # Errors
    /// Returns the backend error when the record cannot be read.
    fn get_evolution_record(&self, id: &EntityId) -> Result<Option<VersionedRecord>, Self::Error>;
    /// # Errors
    /// Returns the backend error when the ledger cannot be read.
    fn list_evolution_records(&self) -> Result<Vec<VersionedRecord>, Self::Error>;
    /// # Errors
    /// Returns the backend error when the authenticated head cannot be read.
    fn get_evolution_head(&self) -> Result<Option<VersionedRecord>, Self::Error>;
    /// # Errors
    /// Returns the backend error when the append precondition or commit fails.
    fn append_evolution_record(
        &self,
        record: VersionedRecord,
        head: VersionedRecord,
        head_precondition: WritePrecondition,
    ) -> Result<CommitReceipt, Self::Error>;
}

/// Privileged repository surface reserved for an explicit data-control erasure flow.
///
/// Generic transactions and [`EvolutionLedgerRepository`] remain append-only; consumers must
/// deliberately import this separate capability to erase both installation-global collections.
pub trait EvolutionLedgerDataControlRepository: Send + Sync {
    type Error: ClassifiedRepositoryError;

    /// Atomically erases the signed evolution ledger and its authenticated head.
    ///
    /// # Errors
    /// Returns the backend error when deletion, remnant verification, or commit fails.
    fn erase_evolution_ledger_for_data_control(
        &self,
    ) -> Result<EvolutionLedgerErasureReport, Self::Error>;
}

macro_rules! repository_trait {
    ($trait_name:ident, $get:ident, $list:ident, $put:ident, $delete:ident) => {
        pub trait $trait_name: Send + Sync {
            type Error: Error + Send + Sync + 'static;

            /// # Errors
            ///
            /// Returns the backend error when the record cannot be read or decoded.
            fn $get(&self, id: &EntityId) -> Result<Option<VersionedRecord>, Self::Error>;
            /// # Errors
            ///
            /// Returns the backend error when the collection cannot be read or decoded.
            fn $list(&self) -> Result<Vec<VersionedRecord>, Self::Error>;
            /// # Errors
            ///
            /// Returns the backend error when validation, precondition, or commit fails.
            fn $put(
                &self,
                record: VersionedRecord,
                precondition: WritePrecondition,
            ) -> Result<CommitReceipt, Self::Error>;
            /// # Errors
            ///
            /// Returns the backend error when the precondition or commit fails.
            fn $delete(
                &self,
                id: &EntityId,
                precondition: WritePrecondition,
            ) -> Result<CommitReceipt, Self::Error>;
        }
    };
}

repository_trait!(
    LeaseRepository,
    get_lease,
    list_leases,
    put_lease,
    delete_lease
);
repository_trait!(
    GenerationRepository,
    get_generation,
    list_generations,
    put_generation,
    delete_generation
);
repository_trait!(
    CatalogRepository,
    get_catalog_entry,
    list_catalog_entries,
    put_catalog_entry,
    delete_catalog_entry
);
repository_trait!(
    ActionRepository,
    get_action,
    list_actions,
    put_action,
    delete_action
);
repository_trait!(GoalRepository, get_goal, list_goals, put_goal, delete_goal);
repository_trait!(
    ProfileRepository,
    get_profile,
    list_profiles,
    put_profile,
    delete_profile
);
repository_trait!(
    ChildRepository,
    get_child,
    list_children,
    put_child,
    delete_child
);
repository_trait!(
    ChildMessageRepository,
    get_child_message,
    list_child_messages,
    put_child_message,
    delete_child_message
);
repository_trait!(PlanRepository, get_plan, list_plans, put_plan, delete_plan);
repository_trait!(
    CommitmentRepository,
    get_commitment,
    list_commitments,
    put_commitment,
    delete_commitment
);
repository_trait!(WaitRepository, get_wait, list_waits, put_wait, delete_wait);
repository_trait!(
    ScheduleRepository,
    get_schedule,
    list_schedules,
    put_schedule,
    delete_schedule
);
repository_trait!(
    JobAttemptRepository,
    get_job_attempt,
    list_job_attempts,
    put_job_attempt,
    delete_job_attempt
);
repository_trait!(
    RouteRepository,
    get_route,
    list_routes,
    put_route,
    delete_route
);
repository_trait!(
    ResourceRepository,
    get_resource_record,
    list_resource_records,
    put_resource_record,
    delete_resource_record
);
repository_trait!(
    ChannelOffsetRepository,
    get_channel_offset,
    list_channel_offsets,
    put_channel_offset,
    delete_channel_offset
);
repository_trait!(
    DeliveryRepository,
    get_delivery,
    list_deliveries,
    put_delivery,
    delete_delivery
);
repository_trait!(
    AttentionRepository,
    get_attention_candidate,
    list_attention_candidates,
    put_attention_candidate,
    delete_attention_candidate
);
repository_trait!(
    InitiativeRepository,
    get_initiative,
    list_initiatives,
    put_initiative,
    delete_initiative
);
repository_trait!(
    RefinementRepository,
    get_refinement,
    list_refinements,
    put_refinement,
    delete_refinement
);
repository_trait!(
    ToolExperienceRepository,
    get_tool_experience,
    list_tool_experience,
    put_tool_experience,
    delete_tool_experience
);
repository_trait!(
    MigrationRepository,
    get_migration,
    list_migrations,
    put_migration,
    delete_migration
);
repository_trait!(
    SharedKnowledgeSpaceRepository,
    get_shared_knowledge_space,
    list_shared_knowledge_spaces,
    put_shared_knowledge_space,
    delete_shared_knowledge_space
);
