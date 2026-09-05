#![forbid(unsafe_code)]

use std::error::Error;

use keith_agent_types::{EntityId, Revision, SchemaVersion, UtcTimestamp};
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Collection {
    WorkerLeases,
    WorkerGenerations,
    SessionCatalog,
    PromptIngress,
    Profiles,
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
    ChannelAccounts,
    ChannelOffsets,
    Deliveries,
    AcpSessions,
    PluginRegistry,
    ConnectedApps,
    ComputerSessions,
    ControlLeases,
    Demonstrations,
    TaskRecipes,
    HarnessRepairs,
    IntegrationOperations,
    IntegrationAudit,
    AttentionCandidates,
    InitiativeHistory,
    EvolutionTransactions,
    EvolutionLedger,
    EvolutionLedgerHead,
    ToolExperience,
    KernelMetadata,
    ActiveOperations,
    SchemaMigrations,
}

impl Collection {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::WorkerLeases => "worker_leases",
            Self::WorkerGenerations => "worker_generations",
            Self::SessionCatalog => "session_catalog",
            Self::PromptIngress => "prompt_ingress",
            Self::Profiles => "profiles",
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
            Self::ChannelAccounts => "channel_accounts",
            Self::ChannelOffsets => "channel_offsets",
            Self::Deliveries => "deliveries",
            Self::AcpSessions => "acp_sessions",
            Self::PluginRegistry => "plugin_registry",
            Self::ConnectedApps => "connected_apps",
            Self::ComputerSessions => "computer_sessions",
            Self::ControlLeases => "control_leases",
            Self::Demonstrations => "demonstrations",
            Self::TaskRecipes => "task_recipes",
            Self::HarnessRepairs => "harness_repairs",
            Self::IntegrationOperations => "integration_operations",
            Self::IntegrationAudit => "integration_audit",
            Self::AttentionCandidates => "attention_candidates",
            Self::InitiativeHistory => "initiative_history",
            Self::EvolutionTransactions => "evolution_transactions",
            Self::EvolutionLedger => "evolution_ledger",
            Self::EvolutionLedgerHead => "evolution_ledger_head",
            Self::ToolExperience => "tool_experience",
            Self::KernelMetadata => "kernel_metadata",
            Self::ActiveOperations => "active_operations",
            Self::SchemaMigrations => "schema_migrations",
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
