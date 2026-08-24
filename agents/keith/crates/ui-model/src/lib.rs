#![forbid(unsafe_code)]

use std::collections::BTreeSet;

use keith_agent_types::{
    ActionId, ChildId, CommitmentId, DeliveryId, EntityId, EntryId, Generation, GoalId, JobId,
    Sequence, SessionId, ToolCallId, TurnId, UtcTimestamp,
};
use keith_protocol::{
    DaemonEvent, EventEnvelope, EvolutionAvailabilityProjection, GoalState, MemoryChangeKind,
    MemoryChangeProjection, MessageProjection, MessageRole, SessionSnapshot, TurnTerminalStatus,
};
pub use keith_protocol::{PresenceProjection, PresenceState};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum OperatorSurface {
    Chat,
    Queue,
    Sessions,
    Models,
    Goals,
    Plans,
    Children,
    Tools,
    Kernels,
    Artifacts,
    Schedules,
    Commitments,
    Waiting,
    Confirmations,
    Memory,
    Knowledge,
    Channels,
    Settings,
    Refinement,
    Logs,
    Diagnostics,
    Evolution,
}

impl OperatorSurface {
    pub const ALL: [Self; 22] = [
        Self::Chat,
        Self::Queue,
        Self::Sessions,
        Self::Models,
        Self::Goals,
        Self::Plans,
        Self::Children,
        Self::Tools,
        Self::Kernels,
        Self::Artifacts,
        Self::Schedules,
        Self::Commitments,
        Self::Waiting,
        Self::Confirmations,
        Self::Memory,
        Self::Knowledge,
        Self::Channels,
        Self::Settings,
        Self::Refinement,
        Self::Logs,
        Self::Diagnostics,
        Self::Evolution,
    ];

    pub const fn route(self) -> &'static str {
        match self {
            Self::Chat => "chat",
            Self::Queue => "queue",
            Self::Sessions => "sessions",
            Self::Models => "models",
            Self::Goals => "goals",
            Self::Plans => "plans",
            Self::Children => "children",
            Self::Tools => "tools",
            Self::Kernels => "kernels",
            Self::Artifacts => "artifacts",
            Self::Schedules => "schedules",
            Self::Commitments => "commitments",
            Self::Waiting => "waiting",
            Self::Confirmations => "confirmations",
            Self::Memory => "memory",
            Self::Knowledge => "knowledge",
            Self::Channels => "channels",
            Self::Settings => "settings",
            Self::Refinement => "refinement",
            Self::Logs => "logs",
            Self::Diagnostics => "diagnostics",
            Self::Evolution => "evolution",
        }
    }

    pub const fn label(self) -> &'static str {
        match self {
            Self::Chat => "Chat",
            Self::Queue => "Queue",
            Self::Sessions => "Sessions",
            Self::Models => "Models",
            Self::Goals => "Goals",
            Self::Plans => "Plans",
            Self::Children => "Children",
            Self::Tools => "Tools",
            Self::Kernels => "Kernels",
            Self::Artifacts => "Artifacts",
            Self::Schedules => "Schedules",
            Self::Commitments => "Commitments",
            Self::Waiting => "Waiting",
            Self::Confirmations => "Confirmations",
            Self::Memory => "Memory",
            Self::Knowledge => "Knowledge",
            Self::Channels => "Channels",
            Self::Settings => "Settings",
            Self::Refinement => "Refinement",
            Self::Logs => "Logs",
            Self::Diagnostics => "Diagnostics",
            Self::Evolution => "Evolution",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum OperatorCommand {
    SubmitPrompt,
    Steer,
    Cancel,
    Retry,
    Branch,
    Resume,
    ListSessions,
    SelectModel,
    ResolveConfirmation,
    SelectBranch,
    CreateGoal,
    UpdateGoal,
    ListGoals,
    ListChildren,
    CreateChild,
    SendChildMessage,
    ArchiveChild,
    CreateSchedule,
    UpdateSchedule,
    DeleteSchedule,
    QueryMemory,
    Export,
    SetBackgroundControl,
    EvolutionStatus,
    EvolutionEnable,
    EvolutionDisable,
    EvolutionApprove,
    EvolutionRevert,
    EvolutionRestoreBaseline,
    EvolutionBrowseLedger,
}

impl OperatorCommand {
    pub const ALL: [Self; 30] = [
        Self::SubmitPrompt,
        Self::Steer,
        Self::Cancel,
        Self::Retry,
        Self::Branch,
        Self::Resume,
        Self::ListSessions,
        Self::SelectModel,
        Self::ResolveConfirmation,
        Self::SelectBranch,
        Self::CreateGoal,
        Self::UpdateGoal,
        Self::ListGoals,
        Self::ListChildren,
        Self::CreateChild,
        Self::SendChildMessage,
        Self::ArchiveChild,
        Self::CreateSchedule,
        Self::UpdateSchedule,
        Self::DeleteSchedule,
        Self::QueryMemory,
        Self::Export,
        Self::SetBackgroundControl,
        Self::EvolutionStatus,
        Self::EvolutionEnable,
        Self::EvolutionDisable,
        Self::EvolutionApprove,
        Self::EvolutionRevert,
        Self::EvolutionRestoreBaseline,
        Self::EvolutionBrowseLedger,
    ];
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ClientParity {
    pub surfaces: BTreeSet<OperatorSurface>,
    pub commands: BTreeSet<OperatorCommand>,
}

impl ClientParity {
    pub fn full() -> Self {
        Self {
            surfaces: OperatorSurface::ALL.into_iter().collect(),
            commands: OperatorCommand::ALL.into_iter().collect(),
        }
    }

    pub fn is_full(&self) -> bool {
        self == &Self::full()
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct EvolutionLedgerItem {
    pub title: String,
    pub state: String,
    pub occurred_at: UtcTimestamp,
    pub evidence: Vec<String>,
    pub readable_diff: Option<String>,
    pub measured_result: Option<String>,
    pub reversal_promotion_id: Option<EntityId>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct EvolutionSurfaceProjection {
    pub status: String,
    pub availability: String,
    pub guidance: Option<String>,
    pub disclosure: Vec<(String, String)>,
    pub active_title: Option<String>,
    pub active_state: Option<String>,
    pub evidence: Vec<String>,
    pub readable_diff: Option<String>,
    pub measured_result: Option<String>,
    pub approval_hypothesis_id: Option<EntityId>,
    pub ledger: Vec<EvolutionLedgerItem>,
    pub has_more_ledger: bool,
}

#[must_use]
pub fn project_evolution(
    projection: &keith_protocol::EvolutionProjection,
) -> EvolutionSurfaceProjection {
    let availability = match &projection.availability {
        EvolutionAvailabilityProjection::Available { rustc, cargo } => {
            format!("Available with {rustc} and {cargo}")
        }
        EvolutionAvailabilityProjection::Unavailable { reasons } => {
            format!("Unavailable: {}", reasons.join("; "))
        }
    };
    let active = projection.active.as_ref();
    EvolutionSurfaceProjection {
        status: if projection.enabled {
            format!(
                "Self-evolution is enabled — {}",
                humanize_state(&projection.state)
            )
        } else {
            format!(
                "Self-evolution is disabled — {}",
                humanize_state(&projection.state)
            )
        },
        availability,
        guidance: projection.guidance.clone(),
        disclosure: vec![
            (
                "May change".into(),
                projection.disclosure.editable_surface.clone(),
            ),
            (
                "Never changes".into(),
                projection.disclosure.protected_surface.clone(),
            ),
            ("Autonomy".into(), projection.disclosure.autonomy.clone()),
            ("Reversal".into(), projection.disclosure.reversal.clone()),
        ],
        active_title: active.map(|item| format!("Improving {}", item.target)),
        active_state: active.map(|item| humanize_state(&item.state)),
        evidence: active.map_or_else(Vec::new, |item| item.evidence.clone()),
        readable_diff: active.and_then(|item| item.readable_diff.clone()),
        measured_result: active.and_then(|item| item.measured_result.clone()),
        approval_hypothesis_id: active
            .filter(|item| item.approval_required)
            .map(|item| item.hypothesis_id.clone()),
        ledger: projection
            .ledger
            .iter()
            .map(|item| EvolutionLedgerItem {
                title: item.summary.clone(),
                state: humanize_state(&item.state),
                occurred_at: item.occurred_at,
                evidence: item.evidence.clone(),
                readable_diff: item.readable_diff.clone(),
                measured_result: item.measured_result.clone(),
                reversal_promotion_id: item.reversible.then(|| item.promotion_id.clone()).flatten(),
            })
            .collect(),
        has_more_ledger: projection.has_more_ledger,
    }
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PersonalSurface {
    Home,
    Conversation,
    Work,
    YourWorld,
    Settings,
}

impl PersonalSurface {
    pub const ALL: [Self; 5] = [
        Self::Home,
        Self::Conversation,
        Self::Work,
        Self::YourWorld,
        Self::Settings,
    ];

    pub const fn label(self) -> &'static str {
        match self {
            Self::Home => "Home",
            Self::Conversation => "Conversation",
            Self::Work => "Work",
            Self::YourWorld => "Your World",
            Self::Settings => "Settings",
        }
    }

    pub const fn empty_message(self) -> &'static str {
        match self {
            Self::Home => "Keith is ready when you are.",
            Self::Conversation => "Start with anything you want help thinking through or doing.",
            Self::Work => "Ask Keith to take something on and the outcome will stay visible here.",
            Self::YourWorld => {
                "Keith will show saved context here when there is something to review."
            }
            Self::Settings => "Your privacy, connections, and preferences live here.",
        }
    }

    pub const fn empty_action(self) -> &'static str {
        match self {
            Self::Home | Self::Conversation | Self::Work => "Message Keith",
            Self::YourWorld => "Ask what Keith remembers",
            Self::Settings => "Review settings",
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", content = "id")]
pub enum PersonalReference {
    Action(ActionId),
    Goal(GoalId),
    Plan(EntityId),
    Child(ChildId),
    Tool(ToolCallId),
    Commitment(CommitmentId),
    Schedule(JobId),
    Confirmation(EntityId),
    Wait(EntityId),
    Delivery(DeliveryId),
    Memory(EntryId),
    Final { turn_id: TurnId, final_id: EntryId },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PersonalItemKind {
    Request,
    Goal,
    Plan,
    DelegatedWork,
    Action,
    Decision,
    Commitment,
    Schedule,
    Waiting,
    Delivery,
    SavedContext,
    Output,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct PersonalItem {
    pub reference: PersonalReference,
    pub kind: PersonalItemKind,
    pub title: String,
    pub detail: Option<String>,
    pub state_label: String,
    pub occurred_at: Option<UtcTimestamp>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PersonalPresenceTone {
    Ready,
    Active,
    Waiting,
    NeedsYou,
    Complete,
    Failed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct PersonalPresence {
    pub tone: PersonalPresenceTone,
    pub label: String,
    pub detail: Option<String>,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct PersonalIntelligenceProjection {
    pub session_id: SessionId,
    pub session_title: String,
    pub presence: PersonalPresence,
    pub work: Vec<PersonalItem>,
    pub needs_you: Vec<PersonalItem>,
    pub completed: Vec<PersonalItem>,
    pub upcoming: Vec<PersonalItem>,
    pub saved_context: Vec<PersonalItem>,
    pub outputs: Vec<PersonalItem>,
}

#[allow(clippy::too_many_lines)]
pub fn project_personal_intelligence(snapshot: &SessionSnapshot) -> PersonalIntelligenceProjection {
    let mut projection = PersonalIntelligenceProjection {
        session_id: snapshot.session.session_id.clone(),
        session_title: snapshot
            .session
            .title
            .clone()
            .unwrap_or_else(|| "New conversation".into()),
        presence: personal_presence(snapshot),
        work: Vec::new(),
        needs_you: Vec::new(),
        completed: Vec::new(),
        upcoming: Vec::new(),
        saved_context: Vec::new(),
        outputs: Vec::new(),
    };

    for confirmation in &snapshot.confirmations {
        projection.needs_you.push(PersonalItem {
            reference: PersonalReference::Confirmation(confirmation.confirmation_id.clone()),
            kind: PersonalItemKind::Decision,
            title: confirmation.summary.clone(),
            detail: Some("Keith needs your decision before continuing.".into()),
            state_label: "Needs your decision".into(),
            occurred_at: None,
        });
    }
    for action in &snapshot.actions {
        let item = PersonalItem {
            reference: PersonalReference::Action(action.action_id.clone()),
            kind: PersonalItemKind::Request,
            title: humanize_source(&action.source),
            detail: None,
            state_label: humanize_state(&action.state),
            occurred_at: Some(action.created_at),
        };
        push_state_item(&mut projection, item, &action.state, false);
    }
    for goal in &snapshot.goals {
        let state = goal_state_name(goal.state);
        let item = PersonalItem {
            reference: PersonalReference::Goal(goal.goal_id.clone()),
            kind: PersonalItemKind::Goal,
            title: goal.objective.clone(),
            detail: None,
            state_label: humanize_state(state),
            occurred_at: None,
        };
        match goal.state {
            GoalState::Complete | GoalState::Cancelled => projection.completed.push(item),
            GoalState::Blocked | GoalState::Failed | GoalState::Paused => {
                projection.needs_you.push(item);
            }
            GoalState::Draft
            | GoalState::Ready
            | GoalState::Running
            | GoalState::Waiting
            | GoalState::Reviewing => projection.work.push(item),
        }
    }
    for plan in &snapshot.plans {
        let item = PersonalItem {
            reference: PersonalReference::Plan(plan.plan_id.clone()),
            kind: PersonalItemKind::Plan,
            title: plan.summary.clone(),
            detail: None,
            state_label: humanize_state(&plan.state),
            occurred_at: None,
        };
        push_state_item(&mut projection, item, &plan.state, plan.terminal);
    }
    for child in &snapshot.children {
        let item = PersonalItem {
            reference: PersonalReference::Child(child.child_id.clone()),
            kind: PersonalItemKind::DelegatedWork,
            title: child.objective.clone(),
            detail: None,
            state_label: humanize_state(&child.state),
            occurred_at: None,
        };
        push_state_item(
            &mut projection,
            item,
            &child.state,
            state_is_terminal(&child.state),
        );
    }
    for tool in &snapshot.tools {
        let title = tool
            .tool
            .as_deref()
            .map_or_else(|| "Taking an action".into(), humanize_source);
        let item = PersonalItem {
            reference: PersonalReference::Tool(tool.tool_call_id.clone()),
            kind: PersonalItemKind::Action,
            title,
            detail: None,
            state_label: humanize_state(&tool.state),
            occurred_at: None,
        };
        push_state_item(&mut projection, item, &tool.state, tool.terminal);
    }
    for wait in &snapshot.waits {
        let item = PersonalItem {
            reference: PersonalReference::Wait(wait.wait_id.clone()),
            kind: PersonalItemKind::Waiting,
            title: "Waiting before continuing".into(),
            detail: None,
            state_label: humanize_state(&wait.state),
            occurred_at: None,
        };
        push_state_item(&mut projection, item, &wait.state, wait.terminal);
    }
    for commitment in &snapshot.commitments {
        let item = PersonalItem {
            reference: PersonalReference::Commitment(commitment.commitment_id.clone()),
            kind: PersonalItemKind::Commitment,
            title: commitment.summary.clone(),
            detail: commitment.due_at.map(|due| format!("Due {due:?}")),
            state_label: humanize_state(&commitment.state),
            occurred_at: commitment.due_at,
        };
        if commitment.terminal {
            push_state_item(&mut projection, item, &commitment.state, true);
        } else if state_needs_attention(&commitment.state) {
            projection.needs_you.push(item);
        } else {
            projection.upcoming.push(item);
        }
    }
    for schedule in &snapshot.schedules {
        let item = PersonalItem {
            reference: PersonalReference::Schedule(schedule.job_id.clone()),
            kind: PersonalItemKind::Schedule,
            title: if schedule.paused {
                "A paused scheduled task".into()
            } else {
                "Scheduled work".into()
            },
            detail: schedule.next_run.map(|next| format!("Next {next:?}")),
            state_label: if schedule.paused {
                "Paused".into()
            } else {
                "Upcoming".into()
            },
            occurred_at: schedule.next_run,
        };
        if schedule.paused {
            projection.needs_you.push(item);
        } else {
            projection.upcoming.push(item);
        }
    }
    for delivery in &snapshot.deliveries {
        let item = PersonalItem {
            reference: PersonalReference::Delivery(delivery.delivery_id.clone()),
            kind: PersonalItemKind::Delivery,
            title: if delivery.acknowledged {
                "Result delivered".into()
            } else {
                "Delivering your result".into()
            },
            detail: None,
            state_label: humanize_state(&delivery.state),
            occurred_at: None,
        };
        push_state_item(&mut projection, item, &delivery.state, delivery.terminal);
    }
    for memory in &snapshot.memory_changes {
        projection.saved_context.push(PersonalItem {
            reference: PersonalReference::Memory(memory.entry_id.clone()),
            kind: PersonalItemKind::SavedContext,
            title: memory_title(memory.change),
            detail: Some(humanize_source(&memory.source)),
            state_label: "Saved context".into(),
            occurred_at: Some(memory.occurred_at),
        });
    }
    if let Some(terminal) = &snapshot.terminal {
        let state_label = match terminal.status {
            TurnTerminalStatus::Completed => "Completed",
            TurnTerminalStatus::Failed => "Could not finish",
            TurnTerminalStatus::Cancelled => "Stopped",
            TurnTerminalStatus::Exhausted => "Reached its limit",
        };
        let output = PersonalItem {
            reference: PersonalReference::Final {
                turn_id: terminal.turn_id.clone(),
                final_id: terminal.final_id.clone(),
            },
            kind: PersonalItemKind::Output,
            title: "Conversation result".into(),
            detail: terminal.detail.clone(),
            state_label: state_label.into(),
            occurred_at: None,
        };
        projection.outputs.push(output.clone());
        match terminal.status {
            TurnTerminalStatus::Completed | TurnTerminalStatus::Cancelled => {
                projection.completed.push(output);
            }
            TurnTerminalStatus::Failed | TurnTerminalStatus::Exhausted => {
                projection.needs_you.push(output);
            }
        }
    }

    projection
}

fn personal_presence(snapshot: &SessionSnapshot) -> PersonalPresence {
    let presence = &snapshot.presence;
    let (tone, label, detail) = match presence.state {
        PresenceState::Available => (PersonalPresenceTone::Ready, "Ready when you are", None),
        PresenceState::Thinking => (
            PersonalPresenceTone::Active,
            "Working on it",
            Some("Keith is considering the next step.".into()),
        ),
        PresenceState::UsingTools => (
            PersonalPresenceTone::Active,
            "Taking action",
            Some("Keith is using an available capability.".into()),
        ),
        PresenceState::WaitingChild => (
            PersonalPresenceTone::Waiting,
            "Coordinating the work",
            Some("Keith is waiting for delegated work to return.".into()),
        ),
        PresenceState::WaitingExternal => (
            PersonalPresenceTone::Waiting,
            "Waiting before continuing",
            presence
                .next_wake
                .map(|next| format!("Next check {next:?}")),
        ),
        PresenceState::PausedForUser => (
            PersonalPresenceTone::NeedsYou,
            "Needs your input",
            presence.safe_error.clone(),
        ),
        PresenceState::Scheduled => (
            PersonalPresenceTone::Waiting,
            "Scheduled",
            presence.next_wake.map(|next| format!("Next run {next:?}")),
        ),
        PresenceState::Completed => (
            PersonalPresenceTone::Complete,
            "Finished",
            presence.safe_error.clone(),
        ),
        PresenceState::Failed => (
            PersonalPresenceTone::Failed,
            "Could not finish",
            presence.safe_error.clone(),
        ),
    };
    PersonalPresence {
        tone,
        label: label.into(),
        detail,
        updated_at: presence.updated_at,
    }
}

fn push_state_item(
    projection: &mut PersonalIntelligenceProjection,
    item: PersonalItem,
    state: &str,
    terminal: bool,
) {
    if state_needs_attention(state) {
        projection.needs_you.push(item);
    } else if terminal || state_is_terminal(state) {
        projection.completed.push(item);
    } else {
        projection.work.push(item);
    }
}

fn state_needs_attention(state: &str) -> bool {
    let state = state.to_ascii_lowercase();
    ["blocked", "failed", "error", "paused", "confirm", "unknown"]
        .iter()
        .any(|needle| state.contains(needle))
}

fn state_is_terminal(state: &str) -> bool {
    let state = state.to_ascii_lowercase();
    [
        "complete",
        "completed",
        "done",
        "sent",
        "fulfilled",
        "cancelled",
        "canceled",
        "expired",
        "archived",
    ]
    .iter()
    .any(|needle| state.contains(needle))
}

fn humanize_source(source: &str) -> String {
    let mut output = String::new();
    for (index, word) in source
        .split(['_', '-', '.'])
        .filter(|word| !word.is_empty())
        .enumerate()
    {
        if index > 0 {
            output.push(' ');
        }
        if index == 0 {
            let mut characters = word.chars();
            if let Some(first) = characters.next() {
                output.extend(first.to_uppercase());
                output.extend(characters);
            }
        } else {
            output.push_str(word);
        }
    }
    if output.is_empty() {
        "Current work".into()
    } else {
        output
    }
}

fn humanize_state(state: &str) -> String {
    humanize_source(state)
}

const fn goal_state_name(state: GoalState) -> &'static str {
    match state {
        GoalState::Draft => "draft",
        GoalState::Ready => "ready",
        GoalState::Running => "running",
        GoalState::Waiting => "waiting",
        GoalState::Reviewing => "reviewing",
        GoalState::Paused => "paused",
        GoalState::Blocked => "blocked",
        GoalState::Complete => "complete",
        GoalState::Failed => "failed",
        GoalState::Cancelled => "cancelled",
    }
}

fn memory_title(change: MemoryChangeKind) -> String {
    match change {
        MemoryChangeKind::Created => "Keith saved something for later".into(),
        MemoryChangeKind::Updated => "Saved context was updated".into(),
        MemoryChangeKind::Deleted => "Saved context was forgotten".into(),
        MemoryChangeKind::Consolidated => "Saved context was organized".into(),
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct VirtualizationConfig {
    pub max_retained_messages: usize,
    pub max_window_items: usize,
    pub overscan_items: usize,
}

impl VirtualizationConfig {
    /// # Errors
    ///
    /// Returns an error when a limit is zero or overscan cannot fit inside a window.
    pub const fn new(
        max_retained_messages: usize,
        max_window_items: usize,
        overscan_items: usize,
    ) -> Result<Self, ProjectionError> {
        if max_retained_messages == 0 || max_window_items == 0 || overscan_items >= max_window_items
        {
            return Err(ProjectionError::InvalidLimit);
        }
        Ok(Self {
            max_retained_messages,
            max_window_items,
            overscan_items,
        })
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SnapshotRequiredReason {
    GenerationGap,
    SequenceGap,
    QueueOverflow,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProjectionStreamState {
    Current,
    Coalescing {
        collapsed_events: u64,
    },
    Backpressured {
        pending: usize,
        capacity: usize,
    },
    SnapshotRequired {
        reason: SnapshotRequiredReason,
        generation: Generation,
        expected_sequence: Sequence,
        received_sequence: Sequence,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReductionOutcome {
    Applied,
    AppliedCoalesced { collapsed_events: u64 },
    SnapshotReplaced,
    Duplicate,
    StaleGeneration,
    Gap,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct HistoryWindow {
    pub absolute_start: usize,
    pub total_known: usize,
    pub items: Vec<MessageProjection>,
    pub has_older: bool,
    pub has_newer: bool,
    pub requires_backfill: bool,
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum ProjectionError {
    #[error("projection limits are invalid")]
    InvalidLimit,
    #[error("event belongs to another root tree")]
    RootMismatch,
    #[error("snapshot or event belongs to another session")]
    SessionMismatch,
    #[error("event sequence range is invalid")]
    InvalidSequenceRange,
    #[error("projection revision overflowed")]
    RevisionOverflow,
    #[error("event sequence overflowed")]
    SequenceOverflow,
    #[error("slow-client queue state is invalid")]
    InvalidQueueState,
}

pub struct ProjectionReducer {
    snapshot: SessionSnapshot,
    virtualization: VirtualizationConfig,
    history_offset: usize,
    stream_state: ProjectionStreamState,
}

impl ProjectionReducer {
    /// # Errors
    ///
    /// Returns an error when the snapshot contains a cross-session presence projection.
    pub fn new(
        snapshot: SessionSnapshot,
        virtualization: VirtualizationConfig,
    ) -> Result<Self, ProjectionError> {
        validate_snapshot_identity(&snapshot, &snapshot)?;
        let mut reducer = Self {
            snapshot,
            virtualization,
            history_offset: 0,
            stream_state: ProjectionStreamState::Current,
        };
        reducer.bound_history();
        Ok(reducer)
    }

    pub const fn snapshot(&self) -> &SessionSnapshot {
        &self.snapshot
    }

    pub const fn stream_state(&self) -> ProjectionStreamState {
        self.stream_state
    }

    /// Applies a complete authoritative snapshot without regressing generation or sequence.
    ///
    /// # Errors
    ///
    /// Returns an error for cross-root or cross-session snapshots.
    pub fn apply_snapshot(
        &mut self,
        snapshot: SessionSnapshot,
    ) -> Result<ReductionOutcome, ProjectionError> {
        validate_snapshot_identity(&self.snapshot, &snapshot)?;
        if snapshot.generation < self.snapshot.generation {
            return Ok(ReductionOutcome::StaleGeneration);
        }
        if snapshot.generation == self.snapshot.generation
            && snapshot.through_sequence < self.snapshot.through_sequence
        {
            return Ok(ReductionOutcome::Duplicate);
        }
        if snapshot.generation == self.snapshot.generation
            && snapshot.through_sequence == self.snapshot.through_sequence
            && snapshot.revision == self.snapshot.revision
        {
            return Ok(ReductionOutcome::Duplicate);
        }
        self.snapshot = snapshot;
        self.history_offset = 0;
        self.bound_history();
        self.stream_state = ProjectionStreamState::Current;
        Ok(ReductionOutcome::SnapshotReplaced)
    }

    /// Applies exactly the next event or an explicitly represented coalesced sequence range.
    ///
    /// # Errors
    ///
    /// Returns an error for malformed sequence ranges, identity conflicts, or counter overflow.
    pub fn apply_event(
        &mut self,
        envelope: &EventEnvelope,
    ) -> Result<ReductionOutcome, ProjectionError> {
        if envelope.root_tree_id != self.snapshot.session.root_tree_id {
            return Err(ProjectionError::RootMismatch);
        }
        if envelope.first_sequence > envelope.sequence {
            return Err(ProjectionError::InvalidSequenceRange);
        }
        if envelope.generation < self.snapshot.generation {
            return Ok(ReductionOutcome::StaleGeneration);
        }
        let expected = self
            .snapshot
            .through_sequence
            .checked_next()
            .ok_or(ProjectionError::SequenceOverflow)?;
        if envelope.generation > self.snapshot.generation {
            self.require_snapshot(
                SnapshotRequiredReason::GenerationGap,
                envelope.generation,
                expected,
                envelope.first_sequence,
            );
            return Ok(ReductionOutcome::Gap);
        }
        if envelope.sequence <= self.snapshot.through_sequence {
            return Ok(ReductionOutcome::Duplicate);
        }
        if envelope.first_sequence != expected {
            self.require_snapshot(
                SnapshotRequiredReason::SequenceGap,
                envelope.generation,
                expected,
                envelope.first_sequence,
            );
            return Ok(ReductionOutcome::Gap);
        }
        validate_event_identity(&self.snapshot, &envelope.event)?;
        let revision = self
            .snapshot
            .revision
            .checked_next()
            .ok_or(ProjectionError::RevisionOverflow)?;
        apply_event_payload(&mut self.snapshot, &envelope.event);
        self.snapshot.generation = envelope.generation;
        self.snapshot.through_sequence = envelope.sequence;
        self.snapshot.revision = revision;
        self.bound_history();
        let collapsed_events = envelope
            .sequence
            .get()
            .saturating_sub(envelope.first_sequence.get());
        if collapsed_events == 0 {
            self.stream_state = ProjectionStreamState::Current;
            Ok(ReductionOutcome::Applied)
        } else {
            self.stream_state = ProjectionStreamState::Coalescing { collapsed_events };
            Ok(ReductionOutcome::AppliedCoalesced { collapsed_events })
        }
    }

    /// Records bounded transport pressure without fabricating runtime activity.
    ///
    /// # Errors
    ///
    /// Returns an error when capacity is zero or pending exceeds capacity.
    pub fn note_client_pressure(
        &mut self,
        pending: usize,
        capacity: usize,
    ) -> Result<(), ProjectionError> {
        if capacity == 0 || pending > capacity {
            return Err(ProjectionError::InvalidQueueState);
        }
        if pending == capacity {
            let expected = self
                .snapshot
                .through_sequence
                .checked_next()
                .ok_or(ProjectionError::SequenceOverflow)?;
            self.require_snapshot(
                SnapshotRequiredReason::QueueOverflow,
                self.snapshot.generation,
                expected,
                expected,
            );
        } else if pending.saturating_mul(4) >= capacity.saturating_mul(3) {
            self.stream_state = ProjectionStreamState::Backpressured { pending, capacity };
        } else {
            self.stream_state = ProjectionStreamState::Current;
        }
        Ok(())
    }

    pub fn history_window(&self, start: usize, requested: usize) -> HistoryWindow {
        let total_known = self
            .history_offset
            .saturating_add(self.snapshot.messages.len());
        let requested = requested.min(self.virtualization.max_window_items);
        let wanted_start = start.saturating_sub(self.virtualization.overscan_items);
        let available_start = wanted_start.max(self.history_offset).min(total_known);
        let wanted_end = start
            .saturating_add(requested)
            .saturating_add(self.virtualization.overscan_items)
            .min(total_known);
        let bounded_end = available_start
            .saturating_add(self.virtualization.max_window_items)
            .min(wanted_end);
        let local_start = available_start.saturating_sub(self.history_offset);
        let local_end = bounded_end.saturating_sub(self.history_offset);
        HistoryWindow {
            absolute_start: available_start,
            total_known,
            items: self.snapshot.messages[local_start..local_end].to_vec(),
            has_older: available_start > 0,
            has_newer: bounded_end < total_known,
            requires_backfill: wanted_start < self.history_offset,
        }
    }

    fn bound_history(&mut self) {
        let overflow = self
            .snapshot
            .messages
            .len()
            .saturating_sub(self.virtualization.max_retained_messages);
        if overflow > 0 {
            self.snapshot.messages.drain(..overflow);
            self.history_offset = self.history_offset.saturating_add(overflow);
        }
    }

    fn require_snapshot(
        &mut self,
        reason: SnapshotRequiredReason,
        generation: Generation,
        expected_sequence: Sequence,
        received_sequence: Sequence,
    ) {
        self.stream_state = ProjectionStreamState::SnapshotRequired {
            reason,
            generation,
            expected_sequence,
            received_sequence,
        };
    }
}

fn validate_snapshot_identity(
    current: &SessionSnapshot,
    incoming: &SessionSnapshot,
) -> Result<(), ProjectionError> {
    if current.session.root_tree_id != incoming.session.root_tree_id {
        return Err(ProjectionError::RootMismatch);
    }
    if current.session.session_id != incoming.session.session_id
        || incoming.presence.session_id != incoming.session.session_id
    {
        return Err(ProjectionError::SessionMismatch);
    }
    Ok(())
}

fn validate_event_identity(
    snapshot: &SessionSnapshot,
    event: &DaemonEvent,
) -> Result<(), ProjectionError> {
    match event {
        DaemonEvent::Snapshot(replacement) => validate_snapshot_identity(snapshot, replacement),
        DaemonEvent::SessionChanged(session)
            if session.session_id != snapshot.session.session_id
                || session.root_tree_id != snapshot.session.root_tree_id =>
        {
            Err(ProjectionError::SessionMismatch)
        }
        DaemonEvent::PresenceChanged(presence)
            if presence.session_id != snapshot.session.session_id =>
        {
            Err(ProjectionError::SessionMismatch)
        }
        _ => Ok(()),
    }
}

#[allow(clippy::too_many_lines)]
fn apply_event_payload(snapshot: &mut SessionSnapshot, event: &DaemonEvent) {
    match event {
        DaemonEvent::Snapshot(replacement) => *snapshot = *replacement.clone(),
        DaemonEvent::SessionChanged(session) => snapshot.session = session.clone(),
        DaemonEvent::ActionQueued(action) | DaemonEvent::ActionStarted(action) => {
            upsert(&mut snapshot.actions, action.clone(), |item| {
                item.action_id.clone()
            });
            snapshot.active_action = Some(action.clone());
        }
        DaemonEvent::ActionFinished(action) => {
            upsert(&mut snapshot.actions, action.clone(), |item| {
                item.action_id.clone()
            });
            if snapshot
                .active_action
                .as_ref()
                .is_some_and(|active| active.action_id == action.action_id)
            {
                snapshot.active_action = None;
            }
        }
        DaemonEvent::AssistantDelta { message_id, text } => {
            if let Some(message) = snapshot
                .messages
                .iter_mut()
                .find(|message| message.message_id == *message_id)
            {
                message.text.push_str(text);
            } else {
                snapshot.messages.push(MessageProjection {
                    message_id: message_id.clone(),
                    final_id: None,
                    role: MessageRole::Assistant,
                    text: text.clone(),
                    committed: false,
                });
            }
        }
        DaemonEvent::MessageCommitted(message) => {
            upsert(&mut snapshot.messages, message.clone(), |item| {
                item.message_id.clone()
            });
        }
        DaemonEvent::TurnTerminal(terminal) => snapshot.terminal = Some(terminal.clone()),
        DaemonEvent::GoalChanged(goal) => {
            upsert(&mut snapshot.goals, goal.clone(), |item| {
                item.goal_id.clone()
            });
        }
        DaemonEvent::PlanChanged(plan) => {
            upsert(&mut snapshot.plans, plan.clone(), |item| {
                item.plan_id.clone()
            });
        }
        DaemonEvent::ChildChanged(child) => {
            upsert(&mut snapshot.children, child.clone(), |item| {
                item.child_id.clone()
            });
        }
        DaemonEvent::KernelChanged(kernel) => {
            upsert(&mut snapshot.kernels, kernel.clone(), |item| {
                item.kernel_id.clone()
            });
        }
        DaemonEvent::CommitmentChanged(commitment) => {
            upsert(&mut snapshot.commitments, commitment.clone(), |item| {
                item.commitment_id.clone()
            });
        }
        DaemonEvent::ScheduleChanged(schedule) => {
            upsert(&mut snapshot.schedules, schedule.clone(), |item| {
                item.job_id.clone()
            });
        }
        DaemonEvent::ToolChanged(tool) => {
            upsert(&mut snapshot.tools, tool.clone(), |item| {
                item.tool_call_id.clone()
            });
        }
        DaemonEvent::WaitChanged(wait) => {
            upsert(&mut snapshot.waits, wait.clone(), |item| {
                item.wait_id.clone()
            });
        }
        DaemonEvent::DeliveryChanged(delivery) => {
            upsert(&mut snapshot.deliveries, delivery.clone(), |item| {
                item.delivery_id.clone()
            });
        }
        DaemonEvent::MemoryChanged(change) => {
            upsert_memory(&mut snapshot.memory_changes, change.clone());
        }
        DaemonEvent::UsageChanged(usage) => snapshot.usage = *usage,
        DaemonEvent::PresenceChanged(presence) => snapshot.presence = presence.clone(),
        DaemonEvent::ConfirmationRequested {
            confirmation_id,
            summary,
        } => upsert(
            &mut snapshot.confirmations,
            keith_protocol::ConfirmationProjection {
                confirmation_id: confirmation_id.clone(),
                summary: summary.clone(),
            },
            |item| item.confirmation_id.clone(),
        ),
        DaemonEvent::ConfirmationResolved { confirmation_id } => snapshot
            .confirmations
            .retain(|item| item.confirmation_id != *confirmation_id),
        DaemonEvent::CommandAccepted { .. }
        | DaemonEvent::CommandRejected(_)
        | DaemonEvent::AgentActivity(_)
        | DaemonEvent::Teammates(_)
        | DaemonEvent::Computer(_)
        | DaemonEvent::EvolutionChanged(_)
        | DaemonEvent::Warning(_)
        | DaemonEvent::Error(_) => {}
    }
}

fn upsert<T, K: Eq>(items: &mut Vec<T>, value: T, key: impl Fn(&T) -> K) {
    let value_key = key(&value);
    if let Some(index) = items.iter().position(|item| key(item) == value_key) {
        items[index] = value;
    } else {
        items.push(value);
    }
}

fn upsert_memory(items: &mut Vec<MemoryChangeProjection>, value: MemoryChangeProjection) {
    upsert(items, value, |item| item.entry_id.clone());
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProjectionCursor {
    pub version: keith_agent_types::SchemaVersion,
    pub generation: keith_agent_types::Generation,
    pub sequence: u64,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AuthoritativeProjectionOrigin {
    pub authority_key: keith_agent_types::StableKey,
    pub producer: keith_agent_types::StableKey,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RosterAgentProjection {
    pub profile_id: keith_agent_types::ProfileId,
    pub name: String,
    pub role: String,
    pub avatar: Option<String>,
    pub lifecycle: keith_protocol::AgentLifecycleState,
    pub hidden: bool,
    pub enabled: bool,
    pub revision: keith_agent_types::Revision,
    pub presence: Option<keith_protocol::PresenceProjection>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RosterSnapshot {
    pub cursor: ProjectionCursor,
    pub origin: AuthoritativeProjectionOrigin,
    pub agents: Vec<RosterAgentProjection>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", deny_unknown_fields)]
pub enum RosterChange {
    Upsert {
        agent: RosterAgentProjection,
    },
    Remove {
        profile_id: keith_agent_types::ProfileId,
        expected_revision: keith_agent_types::Revision,
    },
    Presence {
        profile_id: keith_agent_types::ProfileId,
        profile_revision: keith_agent_types::Revision,
        presence: Option<keith_protocol::PresenceProjection>,
    },
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RosterDelta {
    pub cursor: ProjectionCursor,
    pub origin: AuthoritativeProjectionOrigin,
    pub changes: Vec<RosterChange>,
}

#[derive(Clone, Debug)]
pub struct RosterProjection {
    trusted_authority_key: keith_agent_types::StableKey,
    cursor: Option<ProjectionCursor>,
    agents: std::collections::BTreeMap<keith_agent_types::ProfileId, RosterAgentProjection>,
}

impl RosterProjection {
    #[must_use]
    pub fn new(trusted_authority_key: keith_agent_types::StableKey) -> Self {
        Self {
            trusted_authority_key,
            cursor: None,
            agents: std::collections::BTreeMap::new(),
        }
    }

    #[must_use]
    pub const fn cursor(&self) -> Option<ProjectionCursor> {
        self.cursor
    }

    #[must_use]
    pub fn agents(
        &self,
    ) -> &std::collections::BTreeMap<keith_agent_types::ProfileId, RosterAgentProjection> {
        &self.agents
    }

    pub fn apply_snapshot(
        &mut self,
        snapshot: RosterSnapshot,
    ) -> Result<(), TeammateProjectionError> {
        validate_origin(&self.trusted_authority_key, &snapshot.origin)?;
        validate_snapshot_cursor(self.cursor, snapshot.cursor)?;
        if snapshot.agents.len() > MAX_ROSTER_AGENTS {
            return Err(TeammateProjectionError::InvalidRoster);
        }
        let mut agents = std::collections::BTreeMap::new();
        for agent in snapshot.agents {
            validate_roster_agent(&agent)?;
            if agents.insert(agent.profile_id.clone(), agent).is_some() {
                return Err(TeammateProjectionError::InvalidRoster);
            }
        }
        self.agents = agents;
        self.cursor = Some(snapshot.cursor);
        Ok(())
    }

    pub fn apply_delta(&mut self, delta: RosterDelta) -> Result<(), TeammateProjectionError> {
        validate_origin(&self.trusted_authority_key, &delta.origin)?;
        validate_delta_cursor(self.cursor, delta.cursor)?;
        if delta.changes.len() > MAX_PROJECTION_CHANGES {
            return Err(TeammateProjectionError::InvalidRoster);
        }
        let mut agents = self.agents.clone();
        for change in delta.changes {
            match change {
                RosterChange::Upsert { agent } => {
                    validate_roster_agent(&agent)?;
                    if let Some(previous) = agents.get(&agent.profile_id) {
                        if agent.revision < previous.revision {
                            return Err(TeammateProjectionError::RevisionRegression);
                        }
                    }
                    agents.insert(agent.profile_id.clone(), agent);
                }
                RosterChange::Remove {
                    profile_id,
                    expected_revision,
                } => {
                    let Some(current) = agents.get(&profile_id) else {
                        return Err(TeammateProjectionError::UnknownMember);
                    };
                    if current.revision != expected_revision {
                        return Err(TeammateProjectionError::RevisionRegression);
                    }
                    agents.remove(&profile_id);
                }
                RosterChange::Presence {
                    profile_id,
                    profile_revision,
                    presence,
                } => {
                    let Some(current) = agents.get_mut(&profile_id) else {
                        return Err(TeammateProjectionError::UnknownMember);
                    };
                    if current.revision != profile_revision {
                        return Err(TeammateProjectionError::RevisionRegression);
                    }
                    validate_presence(presence.as_ref())?;
                    current.presence = presence;
                }
            }
        }
        self.agents = agents;
        self.cursor = Some(delta.cursor);
        Ok(())
    }
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", content = "profile_id")]
pub enum ConversationAuthorProjection {
    Human,
    Agent(keith_agent_types::ProfileId),
    System,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationMemberProjection {
    pub profile_id: keith_agent_types::ProfileId,
    pub display_name: String,
    pub role: String,
    pub revision: keith_agent_types::Revision,
    pub active: bool,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationAuthorizationProjection {
    pub authorized: bool,
    pub participant_revision: keith_agent_types::Revision,
    pub conversation_revision: keith_agent_types::Revision,
    pub relevant_grant_revisions:
        std::collections::BTreeMap<keith_agent_types::GrantId, keith_agent_types::Revision>,
    pub policy_digest_sha256: String,
}

#[derive(Clone, Copy, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationDeliveryState {
    Pending,
    Claimed,
    Published,
    Retryable,
    DeadLetter,
    Cancelled,
    Superseded,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationDeliveryProjection {
    pub delivery_id: Option<keith_agent_types::DeliveryId>,
    pub state: ConversationDeliveryState,
    pub attempt_count: u32,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationAttachmentProjection {
    pub artifact_id: keith_agent_types::EntityId,
    pub name: String,
    pub media_type: String,
    pub delivery: ConversationDeliveryProjection,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanonicalTranscriptEventProjection {
    pub event_id: keith_agent_types::EventId,
    pub sequence: u64,
    pub author: ConversationAuthorProjection,
    pub content: String,
    pub created_at: keith_agent_types::UtcTimestamp,
    pub thread_root_event_id: Option<keith_agent_types::EventId>,
    pub reactions: std::collections::BTreeMap<
        String,
        std::collections::BTreeSet<keith_agent_types::ProfileId>,
    >,
    pub attachments: Vec<ConversationAttachmentProjection>,
    pub delivery: ConversationDeliveryProjection,
    pub client_correlation_key: Option<keith_agent_types::StableKey>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationSearchHitProjection {
    pub event_id: keith_agent_types::EventId,
    pub sequence: u64,
    pub excerpt: String,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationRoundProjection {
    pub round_id: keith_agent_types::RoundId,
    pub state: String,
    pub revision: keith_agent_types::Revision,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationAssignmentProjection {
    pub assignment_id: keith_agent_types::AssignmentId,
    pub owner_profile_id: keith_agent_types::ProfileId,
    pub state: String,
    pub revision: keith_agent_types::Revision,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationThreadProjection {
    pub root_event_id: keith_agent_types::EventId,
    pub event_ids: Vec<keith_agent_types::EventId>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct LegacyConversationProjection {
    pub session_id: keith_agent_types::SessionId,
    pub label: String,
    pub archived: bool,
    pub last_updated_at: keith_agent_types::UtcTimestamp,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanonicalConversationProjection {
    pub conversation_id: keith_agent_types::ConversationId,
    pub title: String,
    pub revision: keith_agent_types::Revision,
    pub authorization: ConversationAuthorizationProjection,
    pub members:
        std::collections::BTreeMap<keith_agent_types::ProfileId, ConversationMemberProjection>,
    pub transcript: Vec<CanonicalTranscriptEventProjection>,
    pub unread_count: u64,
    pub read_through_sequence: u64,
    pub search_results: Vec<ConversationSearchHitProjection>,
    pub rounds: std::collections::BTreeMap<keith_agent_types::RoundId, ConversationRoundProjection>,
    pub assignments: std::collections::BTreeMap<
        keith_agent_types::AssignmentId,
        ConversationAssignmentProjection,
    >,
    pub threads:
        std::collections::BTreeMap<keith_agent_types::EventId, ConversationThreadProjection>,
    pub pinned: bool,
    pub hidden: bool,
    pub archived: bool,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationProjectionSnapshot {
    pub cursor: ProjectionCursor,
    pub origin: AuthoritativeProjectionOrigin,
    pub conversations: Vec<CanonicalConversationProjection>,
    pub legacy: Vec<LegacyConversationProjection>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", deny_unknown_fields)]
pub enum ConversationProjectionChange {
    UpsertConversation {
        conversation: CanonicalConversationProjection,
    },
    AuthorizationUpdated {
        conversation_id: keith_agent_types::ConversationId,
        expected_revision: keith_agent_types::Revision,
        authorization: ConversationAuthorizationProjection,
    },
    MemberUpsert {
        conversation_id: keith_agent_types::ConversationId,
        member: ConversationMemberProjection,
    },
    MemberRemove {
        conversation_id: keith_agent_types::ConversationId,
        profile_id: keith_agent_types::ProfileId,
        expected_revision: keith_agent_types::Revision,
    },
    CanonicalEventAppended {
        conversation_id: keith_agent_types::ConversationId,
        event: CanonicalTranscriptEventProjection,
    },
    UnreadUpdated {
        conversation_id: keith_agent_types::ConversationId,
        read_through_sequence: u64,
        unread_count: u64,
    },
    SearchResultsReplaced {
        conversation_id: keith_agent_types::ConversationId,
        results: Vec<ConversationSearchHitProjection>,
    },
    RoundUpsert {
        conversation_id: keith_agent_types::ConversationId,
        round: ConversationRoundProjection,
    },
    AssignmentUpsert {
        conversation_id: keith_agent_types::ConversationId,
        assignment: ConversationAssignmentProjection,
    },
    ThreadUpsert {
        conversation_id: keith_agent_types::ConversationId,
        thread: ConversationThreadProjection,
    },
    ReactionSet {
        conversation_id: keith_agent_types::ConversationId,
        event_id: keith_agent_types::EventId,
        reaction: String,
        profile_ids: std::collections::BTreeSet<keith_agent_types::ProfileId>,
    },
    AttachmentDeliveryUpdated {
        conversation_id: keith_agent_types::ConversationId,
        event_id: keith_agent_types::EventId,
        artifact_id: keith_agent_types::EntityId,
        delivery: ConversationDeliveryProjection,
    },
    ConversationFlagsUpdated {
        conversation_id: keith_agent_types::ConversationId,
        pinned: bool,
        hidden: bool,
        archived: bool,
    },
    LegacyUpsert {
        legacy: LegacyConversationProjection,
    },
    LegacyRemove {
        session_id: keith_agent_types::SessionId,
    },
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationProjectionDelta {
    pub cursor: ProjectionCursor,
    pub origin: AuthoritativeProjectionOrigin,
    pub changes: Vec<ConversationProjectionChange>,
}

#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PendingConversationSend {
    pub correlation_key: keith_agent_types::StableKey,
    pub conversation_id: keith_agent_types::ConversationId,
    pub destination_profile_id: keith_agent_types::ProfileId,
    pub content: String,
    pub queued_at: keith_agent_types::UtcTimestamp,
}

#[derive(Clone, Debug)]
pub struct ConversationProjectionReducer {
    trusted_authority_key: keith_agent_types::StableKey,
    cursor: Option<ProjectionCursor>,
    conversations: std::collections::BTreeMap<
        keith_agent_types::ConversationId,
        CanonicalConversationProjection,
    >,
    legacy: std::collections::BTreeMap<keith_agent_types::SessionId, LegacyConversationProjection>,
    pending_sends:
        std::collections::BTreeMap<keith_agent_types::StableKey, PendingConversationSend>,
}

impl ConversationProjectionReducer {
    #[must_use]
    pub fn new(trusted_authority_key: keith_agent_types::StableKey) -> Self {
        Self {
            trusted_authority_key,
            cursor: None,
            conversations: std::collections::BTreeMap::new(),
            legacy: std::collections::BTreeMap::new(),
            pending_sends: std::collections::BTreeMap::new(),
        }
    }

    #[must_use]
    pub const fn cursor(&self) -> Option<ProjectionCursor> {
        self.cursor
    }

    #[must_use]
    pub fn conversations(
        &self,
    ) -> &std::collections::BTreeMap<
        keith_agent_types::ConversationId,
        CanonicalConversationProjection,
    > {
        &self.conversations
    }

    #[must_use]
    pub fn legacy(
        &self,
    ) -> &std::collections::BTreeMap<keith_agent_types::SessionId, LegacyConversationProjection>
    {
        &self.legacy
    }

    #[must_use]
    pub fn pending_sends(
        &self,
    ) -> &std::collections::BTreeMap<keith_agent_types::StableKey, PendingConversationSend> {
        &self.pending_sends
    }

    pub fn record_pending_send(
        &mut self,
        pending: PendingConversationSend,
    ) -> Result<(), TeammateProjectionError> {
        validate_bounded_text(&pending.content, MAX_MESSAGE_BYTES, false)?;
        if let Some(existing) = self.pending_sends.get(&pending.correlation_key) {
            if existing != &pending {
                return Err(TeammateProjectionError::PendingSendConflict);
            }
            return Ok(());
        }
        if self.pending_sends.len() >= MAX_PENDING_SENDS {
            return Err(TeammateProjectionError::InvalidConversation);
        }
        self.pending_sends
            .insert(pending.correlation_key.clone(), pending);
        Ok(())
    }

    pub fn apply_snapshot(
        &mut self,
        snapshot: ConversationProjectionSnapshot,
    ) -> Result<(), TeammateProjectionError> {
        validate_origin(&self.trusted_authority_key, &snapshot.origin)?;
        validate_snapshot_cursor(self.cursor, snapshot.cursor)?;
        if snapshot.conversations.len() > MAX_CONVERSATIONS
            || snapshot.legacy.len() > MAX_LEGACY_CONVERSATIONS
        {
            return Err(TeammateProjectionError::InvalidConversation);
        }
        let mut conversations = std::collections::BTreeMap::new();
        for conversation in snapshot.conversations {
            validate_conversation(&conversation)?;
            if conversations
                .insert(conversation.conversation_id.clone(), conversation)
                .is_some()
            {
                return Err(TeammateProjectionError::InvalidConversation);
            }
        }
        let mut legacy = std::collections::BTreeMap::new();
        for item in snapshot.legacy {
            validate_legacy(&item)?;
            if legacy.insert(item.session_id.clone(), item).is_some() {
                return Err(TeammateProjectionError::InvalidConversation);
            }
        }
        self.conversations = conversations;
        self.legacy = legacy;
        self.cursor = Some(snapshot.cursor);
        self.reconcile_authoritative_receipts();
        Ok(())
    }

    pub fn apply_delta(
        &mut self,
        delta: ConversationProjectionDelta,
    ) -> Result<(), TeammateProjectionError> {
        validate_origin(&self.trusted_authority_key, &delta.origin)?;
        validate_delta_cursor(self.cursor, delta.cursor)?;
        if delta.changes.len() > MAX_PROJECTION_CHANGES {
            return Err(TeammateProjectionError::InvalidConversation);
        }
        let mut conversations = self.conversations.clone();
        let mut legacy = self.legacy.clone();
        let mut acknowledged = std::collections::BTreeSet::new();
        for change in delta.changes {
            apply_conversation_change(&mut conversations, &mut legacy, &mut acknowledged, change)?;
        }
        self.conversations = conversations;
        self.legacy = legacy;
        for key in acknowledged {
            self.pending_sends.remove(&key);
        }
        self.cursor = Some(delta.cursor);
        Ok(())
    }

    fn reconcile_authoritative_receipts(&mut self) {
        let acknowledged = self
            .conversations
            .values()
            .flat_map(|conversation| conversation.transcript.iter())
            .filter_map(|event| event.client_correlation_key.clone())
            .collect::<Vec<_>>();
        for key in acknowledged {
            self.pending_sends.remove(&key);
        }
    }
}

fn apply_conversation_change(
    conversations: &mut std::collections::BTreeMap<
        keith_agent_types::ConversationId,
        CanonicalConversationProjection,
    >,
    legacy: &mut std::collections::BTreeMap<
        keith_agent_types::SessionId,
        LegacyConversationProjection,
    >,
    acknowledged: &mut std::collections::BTreeSet<keith_agent_types::StableKey>,
    change: ConversationProjectionChange,
) -> Result<(), TeammateProjectionError> {
    match change {
        ConversationProjectionChange::UpsertConversation { conversation } => {
            validate_conversation(&conversation)?;
            if let Some(previous) = conversations.get(&conversation.conversation_id) {
                if conversation.revision < previous.revision {
                    return Err(TeammateProjectionError::RevisionRegression);
                }
            }
            for event in &conversation.transcript {
                if let Some(key) = &event.client_correlation_key {
                    acknowledged.insert(key.clone());
                }
            }
            conversations.insert(conversation.conversation_id.clone(), conversation);
        }
        ConversationProjectionChange::AuthorizationUpdated {
            conversation_id,
            expected_revision,
            authorization,
        } => {
            validate_authorization(&authorization)?;
            let conversation = conversation_mut(conversations, &conversation_id)?;
            if conversation.revision != expected_revision
                || authorization.conversation_revision < conversation.revision
            {
                return Err(TeammateProjectionError::RevisionRegression);
            }
            conversation.authorization = authorization;
        }
        ConversationProjectionChange::MemberUpsert {
            conversation_id,
            member,
        } => {
            validate_member(&member)?;
            let conversation = conversation_mut(conversations, &conversation_id)?;
            if let Some(previous) = conversation.members.get(&member.profile_id) {
                if member.revision < previous.revision {
                    return Err(TeammateProjectionError::RevisionRegression);
                }
            }
            conversation
                .members
                .insert(member.profile_id.clone(), member);
        }
        ConversationProjectionChange::MemberRemove {
            conversation_id,
            profile_id,
            expected_revision,
        } => {
            let conversation = conversation_mut(conversations, &conversation_id)?;
            let Some(member) = conversation.members.get(&profile_id) else {
                return Err(TeammateProjectionError::UnknownMember);
            };
            if member.revision != expected_revision {
                return Err(TeammateProjectionError::RevisionRegression);
            }
            conversation.members.remove(&profile_id);
        }
        ConversationProjectionChange::CanonicalEventAppended {
            conversation_id,
            event,
        } => {
            let conversation = conversation_mut(conversations, &conversation_id)?;
            validate_transcript_event(&event, &conversation.members)?;
            let expected = conversation
                .transcript
                .last()
                .map_or(Some(1), |last| last.sequence.checked_add(1))
                .ok_or(TeammateProjectionError::SequenceGap)?;
            if event.sequence != expected
                || conversation
                    .transcript
                    .iter()
                    .any(|existing| existing.event_id == event.event_id)
            {
                return Err(TeammateProjectionError::DuplicateEvent);
            }
            if let Some(root) = &event.thread_root_event_id {
                if !conversation
                    .transcript
                    .iter()
                    .any(|existing| &existing.event_id == root)
                {
                    return Err(TeammateProjectionError::UnknownEvent);
                }
            }
            if let Some(key) = &event.client_correlation_key {
                acknowledged.insert(key.clone());
            }
            conversation.transcript.push(event);
        }
        ConversationProjectionChange::UnreadUpdated {
            conversation_id,
            read_through_sequence,
            unread_count,
        } => {
            let conversation = conversation_mut(conversations, &conversation_id)?;
            validate_unread(
                conversation
                    .transcript
                    .last()
                    .map_or(0, |event| event.sequence),
                read_through_sequence,
                unread_count,
            )?;
            conversation.read_through_sequence = read_through_sequence;
            conversation.unread_count = unread_count;
        }
        ConversationProjectionChange::SearchResultsReplaced {
            conversation_id,
            results,
        } => {
            let conversation = conversation_mut(conversations, &conversation_id)?;
            validate_search_results(&results, &conversation.transcript)?;
            conversation.search_results = results;
        }
        ConversationProjectionChange::RoundUpsert {
            conversation_id,
            round,
        } => {
            validate_bounded_text(&round.state, MAX_STATE_BYTES, false)?;
            let conversation = conversation_mut(conversations, &conversation_id)?;
            if let Some(previous) = conversation.rounds.get(&round.round_id) {
                if round.revision < previous.revision {
                    return Err(TeammateProjectionError::RevisionRegression);
                }
            }
            conversation.rounds.insert(round.round_id.clone(), round);
        }
        ConversationProjectionChange::AssignmentUpsert {
            conversation_id,
            assignment,
        } => {
            validate_bounded_text(&assignment.state, MAX_STATE_BYTES, false)?;
            let conversation = conversation_mut(conversations, &conversation_id)?;
            if !conversation
                .members
                .contains_key(&assignment.owner_profile_id)
            {
                return Err(TeammateProjectionError::UnknownMember);
            }
            if let Some(previous) = conversation.assignments.get(&assignment.assignment_id) {
                if assignment.revision < previous.revision {
                    return Err(TeammateProjectionError::RevisionRegression);
                }
            }
            conversation
                .assignments
                .insert(assignment.assignment_id.clone(), assignment);
        }
        ConversationProjectionChange::ThreadUpsert {
            conversation_id,
            thread,
        } => {
            let conversation = conversation_mut(conversations, &conversation_id)?;
            validate_thread(&thread, &conversation.transcript)?;
            conversation
                .threads
                .insert(thread.root_event_id.clone(), thread);
        }
        ConversationProjectionChange::ReactionSet {
            conversation_id,
            event_id,
            reaction,
            profile_ids,
        } => {
            validate_bounded_text(&reaction, MAX_REACTION_BYTES, false)?;
            if profile_ids.len() > MAX_REACTION_ACTORS {
                return Err(TeammateProjectionError::InvalidConversation);
            }
            let conversation = conversation_mut(conversations, &conversation_id)?;
            if profile_ids
                .iter()
                .any(|profile_id| !conversation.members.contains_key(profile_id))
            {
                return Err(TeammateProjectionError::UnknownMember);
            }
            let event = event_mut(conversation, &event_id)?;
            if profile_ids.is_empty() {
                event.reactions.remove(&reaction);
            } else {
                event.reactions.insert(reaction, profile_ids);
            }
        }
        ConversationProjectionChange::AttachmentDeliveryUpdated {
            conversation_id,
            event_id,
            artifact_id,
            delivery,
        } => {
            validate_delivery(&delivery)?;
            let conversation = conversation_mut(conversations, &conversation_id)?;
            let event = event_mut(conversation, &event_id)?;
            let Some(attachment) = event
                .attachments
                .iter_mut()
                .find(|attachment| attachment.artifact_id == artifact_id)
            else {
                return Err(TeammateProjectionError::UnknownEvent);
            };
            attachment.delivery = delivery;
        }
        ConversationProjectionChange::ConversationFlagsUpdated {
            conversation_id,
            pinned,
            hidden,
            archived,
        } => {
            let conversation = conversation_mut(conversations, &conversation_id)?;
            conversation.pinned = pinned;
            conversation.hidden = hidden;
            conversation.archived = archived;
        }
        ConversationProjectionChange::LegacyUpsert { legacy: item } => {
            validate_legacy(&item)?;
            legacy.insert(item.session_id.clone(), item);
        }
        ConversationProjectionChange::LegacyRemove { session_id } => {
            if legacy.remove(&session_id).is_none() {
                return Err(TeammateProjectionError::UnknownConversation);
            }
        }
    }
    Ok(())
}

fn conversation_mut<'a>(
    conversations: &'a mut std::collections::BTreeMap<
        keith_agent_types::ConversationId,
        CanonicalConversationProjection,
    >,
    conversation_id: &keith_agent_types::ConversationId,
) -> Result<&'a mut CanonicalConversationProjection, TeammateProjectionError> {
    conversations
        .get_mut(conversation_id)
        .ok_or(TeammateProjectionError::UnknownConversation)
}

fn event_mut<'a>(
    conversation: &'a mut CanonicalConversationProjection,
    event_id: &keith_agent_types::EventId,
) -> Result<&'a mut CanonicalTranscriptEventProjection, TeammateProjectionError> {
    conversation
        .transcript
        .iter_mut()
        .find(|event| &event.event_id == event_id)
        .ok_or(TeammateProjectionError::UnknownEvent)
}

fn validate_origin(
    trusted: &keith_agent_types::StableKey,
    origin: &AuthoritativeProjectionOrigin,
) -> Result<(), TeammateProjectionError> {
    if trusted != &origin.authority_key {
        return Err(TeammateProjectionError::ForgedFrame);
    }
    Ok(())
}

fn validate_cursor(cursor: ProjectionCursor) -> Result<(), TeammateProjectionError> {
    if cursor.version != keith_agent_types::CURRENT_SCHEMA_VERSION || cursor.sequence == 0 {
        return Err(TeammateProjectionError::UnsupportedVersion);
    }
    Ok(())
}

fn validate_snapshot_cursor(
    current: Option<ProjectionCursor>,
    incoming: ProjectionCursor,
) -> Result<(), TeammateProjectionError> {
    validate_cursor(incoming)?;
    if let Some(current) = current {
        if incoming.generation < current.generation
            || (incoming.generation == current.generation && incoming.sequence <= current.sequence)
        {
            return Err(TeammateProjectionError::GenerationRegression);
        }
    }
    Ok(())
}

fn validate_delta_cursor(
    current: Option<ProjectionCursor>,
    incoming: ProjectionCursor,
) -> Result<(), TeammateProjectionError> {
    validate_cursor(incoming)?;
    let Some(current) = current else {
        return Err(TeammateProjectionError::SnapshotRequired);
    };
    if incoming.generation != current.generation
        || current.sequence.checked_add(1) != Some(incoming.sequence)
    {
        return Err(TeammateProjectionError::SequenceGap);
    }
    Ok(())
}

fn validate_roster_agent(agent: &RosterAgentProjection) -> Result<(), TeammateProjectionError> {
    validate_bounded_text(&agent.name, MAX_NAME_BYTES, false)?;
    validate_bounded_text(&agent.role, MAX_ROLE_BYTES, false)?;
    if let Some(avatar) = &agent.avatar {
        validate_bounded_text(avatar, MAX_AVATAR_BYTES, false)?;
    }
    validate_presence(agent.presence.as_ref())
}

fn validate_presence(
    presence: Option<&keith_protocol::PresenceProjection>,
) -> Result<(), TeammateProjectionError> {
    if let Some(presence) = presence {
        if let Some(safe_error) = &presence.safe_error {
            validate_bounded_text(safe_error, MAX_SAFE_ERROR_BYTES, false)?;
        }
    }
    Ok(())
}

fn validate_conversation(
    conversation: &CanonicalConversationProjection,
) -> Result<(), TeammateProjectionError> {
    validate_bounded_text(&conversation.title, MAX_TITLE_BYTES, true)?;
    validate_authorization(&conversation.authorization)?;
    if conversation.members.len() > MAX_MEMBERS
        || conversation.transcript.len() > MAX_TRANSCRIPT_EVENTS
        || conversation.search_results.len() > MAX_SEARCH_RESULTS
        || conversation.rounds.len() > MAX_ROUNDS
        || conversation.assignments.len() > MAX_ASSIGNMENTS
        || conversation.threads.len() > MAX_THREADS
    {
        return Err(TeammateProjectionError::InvalidConversation);
    }
    for (profile_id, member) in &conversation.members {
        if profile_id != &member.profile_id {
            return Err(TeammateProjectionError::InvalidConversation);
        }
        validate_member(member)?;
    }
    let mut event_ids = std::collections::BTreeSet::new();
    for (index, event) in conversation.transcript.iter().enumerate() {
        validate_transcript_event(event, &conversation.members)?;
        if event.sequence != (index as u64 + 1) || !event_ids.insert(event.event_id.clone()) {
            return Err(TeammateProjectionError::InvalidConversation);
        }
        if let Some(root) = &event.thread_root_event_id {
            if !event_ids.contains(root) {
                return Err(TeammateProjectionError::InvalidConversation);
            }
        }
    }
    validate_unread(
        conversation
            .transcript
            .last()
            .map_or(0, |event| event.sequence),
        conversation.read_through_sequence,
        conversation.unread_count,
    )?;
    validate_search_results(&conversation.search_results, &conversation.transcript)?;
    for round in conversation.rounds.values() {
        validate_bounded_text(&round.state, MAX_STATE_BYTES, false)?;
    }
    for assignment in conversation.assignments.values() {
        validate_bounded_text(&assignment.state, MAX_STATE_BYTES, false)?;
        if !conversation
            .members
            .contains_key(&assignment.owner_profile_id)
        {
            return Err(TeammateProjectionError::InvalidConversation);
        }
    }
    for thread in conversation.threads.values() {
        validate_thread(thread, &conversation.transcript)?;
    }
    Ok(())
}

fn validate_member(member: &ConversationMemberProjection) -> Result<(), TeammateProjectionError> {
    validate_bounded_text(&member.display_name, MAX_NAME_BYTES, false)?;
    validate_bounded_text(&member.role, MAX_ROLE_BYTES, false)
}

fn validate_authorization(
    authorization: &ConversationAuthorizationProjection,
) -> Result<(), TeammateProjectionError> {
    if authorization.relevant_grant_revisions.len() > MAX_GRANT_EVIDENCE
        || authorization.policy_digest_sha256.len() != 64
        || !authorization
            .policy_digest_sha256
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(TeammateProjectionError::InvalidConversation);
    }
    Ok(())
}

fn validate_transcript_event(
    event: &CanonicalTranscriptEventProjection,
    members: &std::collections::BTreeMap<
        keith_agent_types::ProfileId,
        ConversationMemberProjection,
    >,
) -> Result<(), TeammateProjectionError> {
    if event.sequence == 0
        || event.reactions.len() > MAX_REACTIONS
        || event.attachments.len() > MAX_ATTACHMENTS
    {
        return Err(TeammateProjectionError::InvalidConversation);
    }
    validate_bounded_text(&event.content, MAX_MESSAGE_BYTES, false)?;
    if let ConversationAuthorProjection::Agent(profile_id) = &event.author {
        if !members.get(profile_id).is_some_and(|member| member.active) {
            return Err(TeammateProjectionError::UnknownMember);
        }
    }
    for (reaction, profiles) in &event.reactions {
        validate_bounded_text(reaction, MAX_REACTION_BYTES, false)?;
        if profiles.len() > MAX_REACTION_ACTORS
            || profiles
                .iter()
                .any(|profile_id| !members.contains_key(profile_id))
        {
            return Err(TeammateProjectionError::InvalidConversation);
        }
    }
    let mut artifacts = std::collections::BTreeSet::new();
    for attachment in &event.attachments {
        validate_bounded_text(&attachment.name, MAX_ATTACHMENT_NAME_BYTES, false)?;
        validate_bounded_text(&attachment.media_type, MAX_MEDIA_TYPE_BYTES, false)?;
        validate_delivery(&attachment.delivery)?;
        if !artifacts.insert(attachment.artifact_id.clone()) {
            return Err(TeammateProjectionError::InvalidConversation);
        }
    }
    validate_delivery(&event.delivery)
}

fn validate_delivery(
    delivery: &ConversationDeliveryProjection,
) -> Result<(), TeammateProjectionError> {
    if let Some(safe_error) = &delivery.safe_error {
        validate_bounded_text(safe_error, MAX_SAFE_ERROR_BYTES, false)?;
    }
    match delivery.state {
        ConversationDeliveryState::Pending | ConversationDeliveryState::Claimed
            if delivery.safe_error.is_some() =>
        {
            Err(TeammateProjectionError::InvalidConversation)
        }
        _ => Ok(()),
    }
}

fn validate_search_results(
    results: &[ConversationSearchHitProjection],
    transcript: &[CanonicalTranscriptEventProjection],
) -> Result<(), TeammateProjectionError> {
    if results.len() > MAX_SEARCH_RESULTS {
        return Err(TeammateProjectionError::InvalidConversation);
    }
    let mut seen = std::collections::BTreeSet::new();
    for result in results {
        validate_bounded_text(&result.excerpt, MAX_SEARCH_EXCERPT_BYTES, true)?;
        if !seen.insert(result.event_id.clone())
            || !transcript
                .iter()
                .any(|event| event.event_id == result.event_id && event.sequence == result.sequence)
        {
            return Err(TeammateProjectionError::UnknownEvent);
        }
    }
    Ok(())
}

fn validate_thread(
    thread: &ConversationThreadProjection,
    transcript: &[CanonicalTranscriptEventProjection],
) -> Result<(), TeammateProjectionError> {
    if thread.event_ids.is_empty() || thread.event_ids.len() > MAX_THREAD_EVENTS {
        return Err(TeammateProjectionError::InvalidConversation);
    }
    let mut previous_sequence = None;
    let mut seen = std::collections::BTreeSet::new();
    for event_id in &thread.event_ids {
        let Some(event) = transcript.iter().find(|event| &event.event_id == event_id) else {
            return Err(TeammateProjectionError::UnknownEvent);
        };
        if !seen.insert(event_id.clone())
            || previous_sequence.is_some_and(|previous| previous >= event.sequence)
            || (event.event_id != thread.root_event_id
                && event.thread_root_event_id.as_ref() != Some(&thread.root_event_id))
        {
            return Err(TeammateProjectionError::InvalidConversation);
        }
        previous_sequence = Some(event.sequence);
    }
    if thread.event_ids.first() != Some(&thread.root_event_id) {
        return Err(TeammateProjectionError::InvalidConversation);
    }
    Ok(())
}

fn validate_unread(
    last_sequence: u64,
    read_through_sequence: u64,
    unread_count: u64,
) -> Result<(), TeammateProjectionError> {
    if read_through_sequence > last_sequence
        || unread_count != last_sequence.saturating_sub(read_through_sequence)
    {
        return Err(TeammateProjectionError::InvalidConversation);
    }
    Ok(())
}

fn validate_legacy(legacy: &LegacyConversationProjection) -> Result<(), TeammateProjectionError> {
    validate_bounded_text(&legacy.label, MAX_TITLE_BYTES, false)
}

fn validate_bounded_text(
    value: &str,
    max_bytes: usize,
    allow_empty: bool,
) -> Result<(), TeammateProjectionError> {
    if value.len() > max_bytes
        || (!allow_empty && value.is_empty())
        || value.trim() != value
        || value.chars().any(char::is_control)
    {
        return Err(TeammateProjectionError::InvalidConversation);
    }
    Ok(())
}

const MAX_ROSTER_AGENTS: usize = 1_024;
const MAX_PROJECTION_CHANGES: usize = 4_096;
const MAX_CONVERSATIONS: usize = 2_048;
const MAX_LEGACY_CONVERSATIONS: usize = 2_048;
const MAX_MEMBERS: usize = 256;
const MAX_TRANSCRIPT_EVENTS: usize = 16_384;
const MAX_SEARCH_RESULTS: usize = 512;
const MAX_ROUNDS: usize = 512;
const MAX_ASSIGNMENTS: usize = 2_048;
const MAX_THREADS: usize = 2_048;
const MAX_THREAD_EVENTS: usize = 2_048;
const MAX_REACTIONS: usize = 128;
const MAX_REACTION_ACTORS: usize = 512;
const MAX_ATTACHMENTS: usize = 128;
const MAX_GRANT_EVIDENCE: usize = 256;
const MAX_PENDING_SENDS: usize = 1_024;
const MAX_NAME_BYTES: usize = 256;
const MAX_ROLE_BYTES: usize = 128;
const MAX_AVATAR_BYTES: usize = 2_048;
const MAX_TITLE_BYTES: usize = 512;
const MAX_MESSAGE_BYTES: usize = 64 * 1024;
const MAX_STATE_BYTES: usize = 128;
const MAX_REACTION_BYTES: usize = 64;
const MAX_ATTACHMENT_NAME_BYTES: usize = 512;
const MAX_MEDIA_TYPE_BYTES: usize = 256;
const MAX_SEARCH_EXCERPT_BYTES: usize = 2_048;
const MAX_SAFE_ERROR_BYTES: usize = 2_048;

#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum TeammateProjectionError {
    #[error("unsupported projection version")]
    UnsupportedVersion,
    #[error("projection frame did not come from the trusted authority")]
    ForgedFrame,
    #[error("a projection snapshot is required before deltas")]
    SnapshotRequired,
    #[error("projection generation or snapshot sequence regressed")]
    GenerationRegression,
    #[error("projection delta sequence is not contiguous")]
    SequenceGap,
    #[error("roster projection is invalid")]
    InvalidRoster,
    #[error("conversation projection is invalid")]
    InvalidConversation,
    #[error("projection revision regressed or did not match")]
    RevisionRegression,
    #[error("conversation does not exist")]
    UnknownConversation,
    #[error("conversation member does not exist")]
    UnknownMember,
    #[error("canonical event does not exist")]
    UnknownEvent,
    #[error("canonical event is duplicate or out of order")]
    DuplicateEvent,
    #[error("pending send idempotency key conflicts")]
    PendingSendConflict,
}

pub enum RuntimeFact {
    ActionStarted {
        at: UtcTimestamp,
    },
    ToolStarted {
        at: UtcTimestamp,
        tool: String,
    },
    ToolFinished {
        at: UtcTimestamp,
        tool: String,
    },
    ChildStarted {
        at: UtcTimestamp,
        child: String,
    },
    ChildFinished {
        at: UtcTimestamp,
        child: String,
    },
    WaitingExternal {
        at: UtcTimestamp,
        next_wake: Option<UtcTimestamp>,
    },
    PausedForUser {
        at: UtcTimestamp,
    },
    Scheduled {
        at: UtcTimestamp,
        next_wake: UtcTimestamp,
    },
    Recovered {
        at: UtcTimestamp,
    },
    Completed {
        at: UtcTimestamp,
    },
    Failed {
        at: UtcTimestamp,
        safe_error: String,
    },
    BecameIdle {
        at: UtcTimestamp,
    },
}

impl RuntimeFact {
    const fn occurred_at(&self) -> UtcTimestamp {
        match self {
            Self::ActionStarted { at }
            | Self::ToolStarted { at, .. }
            | Self::ToolFinished { at, .. }
            | Self::ChildStarted { at, .. }
            | Self::ChildFinished { at, .. }
            | Self::WaitingExternal { at, .. }
            | Self::PausedForUser { at }
            | Self::Scheduled { at, .. }
            | Self::Recovered { at }
            | Self::Completed { at }
            | Self::Failed { at, .. }
            | Self::BecameIdle { at } => *at,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProgressNotification {
    pub session_id: SessionId,
    pub goal_id: Option<GoalId>,
    pub state: PresenceState,
    pub occurred_at: UtcTimestamp,
    pub next_wake: Option<UtcTimestamp>,
    pub safe_error: Option<String>,
    pub summary: String,
}

type PresenceTransition = (
    PresenceState,
    Option<UtcTimestamp>,
    Option<String>,
    String,
    bool,
);

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FabricatedClaim {
    CursorMovement,
    Typing,
    ToolUseWithoutEvent,
    BackgroundThought,
    ProgressPercentage,
    CompletionWithoutEvent,
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum PresenceError {
    #[error("progress notification interval must be non-negative")]
    InvalidInterval,
    #[error("runtime event timestamp regressed")]
    TimeRegression,
    #[error("safe failure detail must be non-empty and bounded")]
    UnsafeFailureDetail,
    #[error("presentation claim is not supported by runtime evidence")]
    Fabricated,
}

pub struct PresenceProjector {
    projection: PresenceProjection,
    minimum_notification_interval_ms: i64,
    last_notification_at: Option<UtcTimestamp>,
}

impl PresenceProjector {
    /// Creates an available projection with no implied activity.
    ///
    /// # Errors
    ///
    /// Returns an error for a negative notification interval.
    pub fn new(
        session_id: SessionId,
        goal_id: Option<GoalId>,
        now: UtcTimestamp,
        minimum_notification_interval_ms: i64,
    ) -> Result<Self, PresenceError> {
        if minimum_notification_interval_ms < 0 {
            return Err(PresenceError::InvalidInterval);
        }
        Ok(Self {
            projection: PresenceProjection {
                session_id,
                goal_id,
                state: PresenceState::Available,
                updated_at: now,
                next_wake: None,
                safe_error: None,
            },
            minimum_notification_interval_ms,
            last_notification_at: None,
        })
    }

    /// Restores only an authoritative persisted projection after restart.
    ///
    /// # Errors
    ///
    /// Returns an error for a negative notification interval.
    pub fn restore(
        projection: PresenceProjection,
        minimum_notification_interval_ms: i64,
    ) -> Result<Self, PresenceError> {
        if minimum_notification_interval_ms < 0 {
            return Err(PresenceError::InvalidInterval);
        }
        Ok(Self {
            projection,
            minimum_notification_interval_ms,
            last_notification_at: None,
        })
    }

    pub const fn projection(&self) -> &PresenceProjection {
        &self.projection
    }

    /// Applies one real runtime fact and optionally emits a meaningful rate-limited transition.
    ///
    /// Terminal failures and completions are never rate-suppressed.
    ///
    /// # Errors
    ///
    /// Returns an error for regressing time or unsafe failure detail.
    pub fn apply(
        &mut self,
        fact: &RuntimeFact,
    ) -> Result<Option<ProgressNotification>, PresenceError> {
        let at = fact.occurred_at();
        if at < self.projection.updated_at {
            return Err(PresenceError::TimeRegression);
        }
        let previous = self.projection.state;
        let (state, next_wake, safe_error, summary, always_emit) = transition(fact)?;
        self.projection.state = state;
        self.projection.updated_at = at;
        self.projection.next_wake = next_wake;
        self.projection.safe_error.clone_from(&safe_error);
        let meaningful = previous != state || matches!(fact, RuntimeFact::Recovered { .. });
        if !meaningful {
            return Ok(None);
        }
        let rate_limited = self.last_notification_at.is_some_and(|last| {
            at.unix_millis().saturating_sub(last.unix_millis())
                < self.minimum_notification_interval_ms
        });
        if rate_limited && !always_emit {
            return Ok(None);
        }
        self.last_notification_at = Some(at);
        Ok(Some(ProgressNotification {
            session_id: self.projection.session_id.clone(),
            goal_id: self.projection.goal_id.clone(),
            state,
            occurred_at: at,
            next_wake,
            safe_error,
            summary,
        }))
    }
}

/// Explicitly rejects presentation states that have no runtime evidence representation.
///
/// # Errors
///
/// Always returns [`PresenceError::Fabricated`].
pub const fn reject_fabricated_claim(_claim: FabricatedClaim) -> Result<(), PresenceError> {
    Err(PresenceError::Fabricated)
}

fn transition(fact: &RuntimeFact) -> Result<PresenceTransition, PresenceError> {
    let transition = match fact {
        RuntimeFact::ActionStarted { .. } => (
            PresenceState::Thinking,
            None,
            None,
            "Work started".to_owned(),
            false,
        ),
        RuntimeFact::ToolStarted { tool, .. } => (
            PresenceState::UsingTools,
            None,
            None,
            bounded_summary(&format!("Using {tool}")),
            false,
        ),
        RuntimeFact::ToolFinished { tool, .. } => (
            PresenceState::Thinking,
            None,
            None,
            bounded_summary(&format!("Finished {tool}")),
            false,
        ),
        RuntimeFact::ChildStarted { child, .. } => (
            PresenceState::WaitingChild,
            None,
            None,
            bounded_summary(&format!("Waiting for {child}")),
            false,
        ),
        RuntimeFact::ChildFinished { child, .. } => (
            PresenceState::Thinking,
            None,
            None,
            bounded_summary(&format!("Received result from {child}")),
            false,
        ),
        RuntimeFact::WaitingExternal { next_wake, .. } => (
            PresenceState::WaitingExternal,
            *next_wake,
            None,
            "Waiting for an external event".to_owned(),
            false,
        ),
        RuntimeFact::PausedForUser { .. } => (
            PresenceState::PausedForUser,
            None,
            None,
            "Waiting for your input".to_owned(),
            false,
        ),
        RuntimeFact::Scheduled { next_wake, .. } => (
            PresenceState::Scheduled,
            Some(*next_wake),
            None,
            "Scheduled".to_owned(),
            false,
        ),
        RuntimeFact::Recovered { .. } => (
            PresenceState::Thinking,
            None,
            None,
            "Recovered and resumed".to_owned(),
            true,
        ),
        RuntimeFact::Completed { .. } => (
            PresenceState::Completed,
            None,
            None,
            "Completed".to_owned(),
            true,
        ),
        RuntimeFact::Failed { safe_error, .. } => {
            let safe_error = safe_error.trim();
            if safe_error.is_empty() || safe_error.len() > 512 {
                return Err(PresenceError::UnsafeFailureDetail);
            }
            (
                PresenceState::Failed,
                None,
                Some(safe_error.to_owned()),
                "Failed".to_owned(),
                true,
            )
        }
        RuntimeFact::BecameIdle { .. } => (
            PresenceState::Available,
            None,
            None,
            "Available".to_owned(),
            false,
        ),
    };
    Ok(transition)
}

fn bounded_summary(summary: &str) -> String {
    const LIMIT: usize = 256;
    if summary.len() <= LIMIT {
        return summary.to_owned();
    }
    let mut boundary = LIMIT;
    while !summary.is_char_boundary(boundary) {
        boundary -= 1;
    }
    summary[..boundary].to_owned()
}

#[cfg(test)]
mod tests {
    use keith_agent_types::{
        ActionId, CURRENT_PROTOCOL_VERSION, ChildId, CommitmentId, DeliveryId, EntityId, EntryId,
        JobId, KernelId, MessageId, ProfileId, Revision, RootTreeId, ToolCallId,
    };
    use keith_protocol::{
        ActionProjection, ChildProjection, CommitmentProjection, DeliveryProjection,
        EvolutionDisclosureProjection, EvolutionLedgerProjection, EvolutionProjection,
        GoalProjection, GoalState, KernelProjection, MemoryChangeKind, PlanProjection,
        ScheduleExpression, ScheduleProjection, SessionState, SessionSummary, ToolProjection,
        UsageProjection, WaitProjection,
    };

    use super::*;

    fn session_snapshot() -> SessionSnapshot {
        let session_id = SessionId::new();
        SessionSnapshot {
            session: SessionSummary {
                session_id: session_id.clone(),
                root_tree_id: RootTreeId::new(),
                profile_id: ProfileId::new(),
                title: Some("work".into()),
                state: SessionState::Ready,
                updated_at: UtcTimestamp::UNIX_EPOCH,
            },
            generation: Generation::new(1),
            through_sequence: Sequence::ZERO,
            active_action: None,
            actions: Vec::new(),
            messages: Vec::new(),
            goals: Vec::new(),
            plans: Vec::new(),
            children: Vec::new(),
            kernels: Vec::new(),
            commitments: Vec::new(),
            schedules: Vec::new(),
            tools: Vec::new(),
            confirmations: Vec::new(),
            waits: Vec::new(),
            deliveries: Vec::new(),
            memory_changes: Vec::new(),
            usage: UsageProjection::default(),
            presence: PresenceProjection {
                session_id,
                goal_id: None,
                state: PresenceState::Available,
                updated_at: UtcTimestamp::UNIX_EPOCH,
                next_wake: None,
                safe_error: None,
            },
            terminal: None,
            revision: Revision::ZERO,
        }
    }

    fn envelope(
        snapshot: &SessionSnapshot,
        generation: Generation,
        first_sequence: u64,
        sequence: u64,
        event: DaemonEvent,
    ) -> EventEnvelope {
        EventEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            root_tree_id: snapshot.session.root_tree_id.clone(),
            generation,
            first_sequence: Sequence::new(first_sequence),
            sequence: Sequence::new(sequence),
            occurred_at: UtcTimestamp::from_unix_millis(i64::try_from(sequence).unwrap()),
            event,
        }
    }

    fn virtualization() -> VirtualizationConfig {
        VirtualizationConfig::new(8, 4, 1).unwrap()
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn reducer_projects_every_authoritative_domain_without_phantom_state() {
        let initial = session_snapshot();
        let action = ActionProjection {
            action_id: ActionId::new(),
            source: "user".into(),
            state: "running".into(),
            created_at: UtcTimestamp::from_unix_millis(1),
        };
        let goal = GoalProjection {
            goal_id: GoalId::new(),
            objective: "ship".into(),
            state: GoalState::Running,
        };
        let confirmation_id = EntityId::new();
        let events = vec![
            DaemonEvent::ActionStarted(action.clone()),
            DaemonEvent::GoalChanged(goal.clone()),
            DaemonEvent::PlanChanged(PlanProjection {
                plan_id: EntityId::new(),
                summary: "plan".into(),
                state: "running".into(),
                revision: Revision::ZERO,
                terminal: false,
            }),
            DaemonEvent::ChildChanged(ChildProjection {
                child_id: ChildId::new(),
                session_id: SessionId::new(),
                objective: "research".into(),
                state: "running".into(),
            }),
            DaemonEvent::KernelChanged(KernelProjection {
                kernel_id: KernelId::new(),
                runtime: "python".into(),
                state: "ready".into(),
                terminal: false,
            }),
            DaemonEvent::WaitChanged(WaitProjection {
                wait_id: EntityId::new(),
                state: "armed".into(),
                terminal: false,
            }),
            DaemonEvent::CommitmentChanged(CommitmentProjection {
                commitment_id: CommitmentId::new(),
                summary: "follow up".into(),
                state: "open".into(),
                due_at: Some(UtcTimestamp::from_unix_millis(100)),
                terminal: false,
            }),
            DaemonEvent::ScheduleChanged(ScheduleProjection {
                job_id: JobId::new(),
                expression: ScheduleExpression::IntervalSeconds(60),
                next_run: Some(UtcTimestamp::from_unix_millis(100)),
                paused: false,
            }),
            DaemonEvent::DeliveryChanged(DeliveryProjection {
                delivery_id: DeliveryId::new(),
                state: "pending".into(),
                terminal: false,
                turn_id: None,
                final_id: None,
                acknowledged: false,
            }),
            DaemonEvent::MemoryChanged(MemoryChangeProjection {
                entry_id: EntryId::new(),
                source: "memory/facts.md".into(),
                change: MemoryChangeKind::Updated,
                occurred_at: UtcTimestamp::from_unix_millis(10),
            }),
            DaemonEvent::UsageChanged(UsageProjection {
                input_tokens: 10,
                output_tokens: 5,
                cached_input_tokens: 2,
                estimated_cost_microunits: 7,
            }),
            DaemonEvent::ConfirmationRequested {
                confirmation_id: confirmation_id.clone(),
                summary: "allow write".into(),
            },
            DaemonEvent::PresenceChanged(PresenceProjection {
                session_id: initial.session.session_id.clone(),
                goal_id: Some(goal.goal_id.clone()),
                state: PresenceState::UsingTools,
                updated_at: UtcTimestamp::from_unix_millis(13),
                next_wake: None,
                safe_error: None,
            }),
            DaemonEvent::ActionFinished(ActionProjection {
                state: "complete".into(),
                ..action
            }),
        ];
        let mut left = ProjectionReducer::new(initial.clone(), virtualization()).unwrap();
        let mut right = ProjectionReducer::new(initial, virtualization()).unwrap();
        for (index, event) in events.into_iter().enumerate() {
            let sequence = u64::try_from(index + 1).unwrap();
            let left_event = envelope(
                left.snapshot(),
                Generation::new(1),
                sequence,
                sequence,
                event.clone(),
            );
            let right_event = envelope(
                right.snapshot(),
                Generation::new(1),
                sequence,
                sequence,
                event,
            );
            assert_eq!(
                left.apply_event(&left_event).unwrap(),
                ReductionOutcome::Applied
            );
            right.apply_event(&right_event).unwrap();
        }
        assert_eq!(left.snapshot(), right.snapshot());
        let snapshot = left.snapshot();
        assert!(snapshot.active_action.is_none());
        assert_eq!(snapshot.actions[0].state, "complete");
        assert_eq!(snapshot.goals, vec![goal]);
        assert_eq!(snapshot.plans.len(), 1);
        assert_eq!(snapshot.children.len(), 1);
        assert_eq!(snapshot.kernels.len(), 1);
        assert_eq!(snapshot.waits.len(), 1);
        assert_eq!(snapshot.commitments.len(), 1);
        assert_eq!(snapshot.schedules.len(), 1);
        assert_eq!(snapshot.deliveries.len(), 1);
        assert_eq!(snapshot.memory_changes.len(), 1);
        assert_eq!(snapshot.usage.output_tokens, 5);
        assert_eq!(snapshot.confirmations[0].confirmation_id, confirmation_id);
        assert_eq!(snapshot.presence.state, PresenceState::UsingTools);

        let no_runtime_claim = envelope(
            snapshot,
            Generation::new(1),
            15,
            15,
            DaemonEvent::ConfirmationResolved { confirmation_id },
        );
        left.apply_event(&no_runtime_claim).unwrap();
        assert_eq!(left.snapshot().presence.state, PresenceState::UsingTools);
        assert!(left.snapshot().confirmations.is_empty());
    }

    #[test]
    fn reducer_rejects_gaps_duplicates_out_of_order_and_generation_drift() {
        let initial = session_snapshot();
        let message = MessageProjection {
            message_id: MessageId::new(),
            final_id: None,
            role: MessageRole::Assistant,
            text: "done".into(),
            committed: true,
        };
        let mut reducer = ProjectionReducer::new(initial, virtualization()).unwrap();
        let first = envelope(
            reducer.snapshot(),
            Generation::new(1),
            1,
            1,
            DaemonEvent::MessageCommitted(message.clone()),
        );
        assert_eq!(
            reducer.apply_event(&first).unwrap(),
            ReductionOutcome::Applied
        );
        assert_eq!(
            reducer.apply_event(&first).unwrap(),
            ReductionOutcome::Duplicate
        );
        let out_of_order = envelope(
            reducer.snapshot(),
            Generation::new(1),
            0,
            1,
            DaemonEvent::MessageCommitted(message.clone()),
        );
        assert_eq!(
            reducer.apply_event(&out_of_order).unwrap(),
            ReductionOutcome::Duplicate
        );
        let gap = envelope(
            reducer.snapshot(),
            Generation::new(1),
            3,
            3,
            DaemonEvent::MessageCommitted(message.clone()),
        );
        assert_eq!(reducer.apply_event(&gap).unwrap(), ReductionOutcome::Gap);
        assert!(matches!(
            reducer.stream_state(),
            ProjectionStreamState::SnapshotRequired {
                reason: SnapshotRequiredReason::SequenceGap,
                expected_sequence: Sequence(2),
                received_sequence: Sequence(3),
                ..
            }
        ));
        assert_eq!(reducer.snapshot().messages.len(), 1);
        let newer_generation = envelope(
            reducer.snapshot(),
            Generation::new(2),
            1,
            1,
            DaemonEvent::MessageCommitted(message),
        );
        assert_eq!(
            reducer.apply_event(&newer_generation).unwrap(),
            ReductionOutcome::Gap
        );
        let mut replacement = session_snapshot();
        replacement.session = reducer.snapshot().session.clone();
        replacement.presence.session_id = replacement.session.session_id.clone();
        replacement.generation = Generation::new(2);
        assert_eq!(
            reducer.apply_snapshot(replacement).unwrap(),
            ReductionOutcome::SnapshotReplaced
        );
        assert_eq!(reducer.stream_state(), ProjectionStreamState::Current);
        assert_eq!(
            reducer.apply_event(&first).unwrap(),
            ReductionOutcome::StaleGeneration
        );
    }

    #[test]
    fn coalescing_virtualization_and_slow_clients_remain_bounded_and_terminal() {
        let initial = session_snapshot();
        let mut reducer =
            ProjectionReducer::new(initial, VirtualizationConfig::new(5, 3, 1).unwrap()).unwrap();
        let message_id = MessageId::new();
        let coalesced = envelope(
            reducer.snapshot(),
            Generation::new(1),
            1,
            3,
            DaemonEvent::AssistantDelta {
                message_id: message_id.clone(),
                text: "complete stream".into(),
            },
        );
        assert_eq!(
            reducer.apply_event(&coalesced).unwrap(),
            ReductionOutcome::AppliedCoalesced {
                collapsed_events: 2
            }
        );
        let terminal_tool = ToolProjection {
            tool_call_id: ToolCallId::new(),
            tool: Some("test_tool".into()),
            state: "complete".into(),
            terminal: true,
        };
        let terminal = envelope(
            reducer.snapshot(),
            Generation::new(1),
            4,
            4,
            DaemonEvent::ToolChanged(terminal_tool.clone()),
        );
        reducer.apply_event(&terminal).unwrap();
        for index in 0..10 {
            let sequence = u64::try_from(index + 5).unwrap();
            let committed = envelope(
                reducer.snapshot(),
                Generation::new(1),
                sequence,
                sequence,
                DaemonEvent::MessageCommitted(MessageProjection {
                    message_id: MessageId::new(),
                    final_id: None,
                    role: MessageRole::Assistant,
                    text: format!("message {index}"),
                    committed: true,
                }),
            );
            reducer.apply_event(&committed).unwrap();
        }
        assert_eq!(reducer.snapshot().messages.len(), 5);
        let window = reducer.history_window(0, 10);
        assert!(window.items.len() <= 3);
        assert!(window.requires_backfill);
        assert!(window.has_older);
        assert_eq!(reducer.snapshot().tools, vec![terminal_tool]);

        reducer.note_client_pressure(3, 4).unwrap();
        assert!(matches!(
            reducer.stream_state(),
            ProjectionStreamState::Backpressured { .. }
        ));
        reducer.note_client_pressure(4, 4).unwrap();
        assert!(matches!(
            reducer.stream_state(),
            ProjectionStreamState::SnapshotRequired {
                reason: SnapshotRequiredReason::QueueOverflow,
                ..
            }
        ));
        assert_eq!(reducer.snapshot().tools[0].state, "complete");
    }

    #[test]
    fn real_history_drives_tool_child_wait_recovery_failure_and_restart_projection() {
        let session = SessionId::new();
        let goal = GoalId::new();
        let mut projector = PresenceProjector::new(
            session.clone(),
            Some(goal.clone()),
            UtcTimestamp::UNIX_EPOCH,
            10,
        )
        .expect("projector");
        assert_eq!(projector.projection().state, PresenceState::Available);
        let history = [
            RuntimeFact::ActionStarted {
                at: UtcTimestamp::from_unix_millis(10),
            },
            RuntimeFact::ToolStarted {
                at: UtcTimestamp::from_unix_millis(11),
                tool: "search".to_owned(),
            },
            RuntimeFact::ToolFinished {
                at: UtcTimestamp::from_unix_millis(12),
                tool: "search".to_owned(),
            },
            RuntimeFact::ChildStarted {
                at: UtcTimestamp::from_unix_millis(20),
                child: "research".to_owned(),
            },
            RuntimeFact::ChildFinished {
                at: UtcTimestamp::from_unix_millis(30),
                child: "research".to_owned(),
            },
            RuntimeFact::WaitingExternal {
                at: UtcTimestamp::from_unix_millis(40),
                next_wake: Some(UtcTimestamp::from_unix_millis(100)),
            },
            RuntimeFact::Recovered {
                at: UtcTimestamp::from_unix_millis(50),
            },
        ];
        let mut emitted = Vec::new();
        for fact in &history {
            if let Some(notification) = projector.apply(fact).expect("real transition") {
                emitted.push(notification);
            }
        }
        assert!(emitted.len() < history.len());
        assert_eq!(projector.projection().state, PresenceState::Thinking);
        let restored = PresenceProjector::restore(projector.projection().clone(), 10)
            .expect("restart projection");
        assert_eq!(restored.projection(), projector.projection());

        let mut restored = restored;
        let failure = restored
            .apply(&RuntimeFact::Failed {
                at: UtcTimestamp::from_unix_millis(51),
                safe_error: "provider unavailable".to_owned(),
            })
            .expect("failure")
            .expect("terminal notification bypasses rate limit");
        assert_eq!(failure.session_id, session);
        assert_eq!(failure.goal_id, Some(goal));
        assert_eq!(failure.state, PresenceState::Failed);
        assert_eq!(failure.safe_error.as_deref(), Some("provider unavailable"));
    }

    #[test]
    fn scheduled_completion_and_zero_activity_are_truthful() {
        let mut projector =
            PresenceProjector::new(SessionId::new(), None, UtcTimestamp::UNIX_EPOCH, 1_000)
                .expect("projector");
        assert_eq!(projector.projection().state, PresenceState::Available);
        let scheduled = projector
            .apply(&RuntimeFact::Scheduled {
                at: UtcTimestamp::from_unix_millis(1),
                next_wake: UtcTimestamp::from_unix_millis(2_000),
            })
            .expect("scheduled")
            .expect("notification");
        assert_eq!(
            scheduled.next_wake,
            Some(UtcTimestamp::from_unix_millis(2_000))
        );
        let completed = projector
            .apply(&RuntimeFact::Completed {
                at: UtcTimestamp::from_unix_millis(2),
            })
            .expect("completed")
            .expect("terminal not rate limited");
        assert_eq!(completed.state, PresenceState::Completed);
    }

    #[test]
    fn personal_projection_groups_authoritative_work_without_exposing_machinery() {
        let mut snapshot = session_snapshot();
        snapshot.session.title = Some("Plan grandma's visit".into());
        snapshot.actions.push(ActionProjection {
            action_id: ActionId::new(),
            source: "user_request".into(),
            state: "running".into(),
            created_at: UtcTimestamp::from_unix_millis(1),
        });
        snapshot.goals.push(GoalProjection {
            goal_id: GoalId::new(),
            objective: "Arrange the visit".into(),
            state: GoalState::Blocked,
        });
        snapshot
            .confirmations
            .push(keith_protocol::ConfirmationProjection {
                confirmation_id: EntityId::new(),
                summary: "Book the selected train".into(),
            });
        snapshot.schedules.push(ScheduleProjection {
            job_id: JobId::new(),
            expression: ScheduleExpression::IntervalSeconds(3_600),
            next_run: Some(UtcTimestamp::from_unix_millis(2)),
            paused: false,
        });
        snapshot.memory_changes.push(MemoryChangeProjection {
            entry_id: EntryId::new(),
            source: "travel_preferences".into(),
            change: MemoryChangeKind::Updated,
            occurred_at: UtcTimestamp::from_unix_millis(3),
        });

        let personal = project_personal_intelligence(&snapshot);
        assert_eq!(personal.session_title, "Plan grandma's visit");
        assert_eq!(personal.presence.label, "Ready when you are");
        assert_eq!(personal.work.len(), 1);
        assert_eq!(personal.work[0].title, "User request");
        assert_eq!(personal.needs_you.len(), 2);
        assert_eq!(personal.upcoming.len(), 1);
        assert_eq!(personal.saved_context.len(), 1);
        let serialized = serde_json::to_string(&personal).unwrap();
        for internal_label in ["kernel", "generation", "protocol", "queue"] {
            assert!(!serialized.contains(internal_label));
        }
    }

    #[test]
    fn personal_projection_corrects_from_snapshots_and_does_not_fill_sequence_gaps() {
        let initial = session_snapshot();
        let mut reducer = ProjectionReducer::new(initial, virtualization()).unwrap();
        let confirmation = envelope(
            reducer.snapshot(),
            Generation::new(1),
            1,
            1,
            DaemonEvent::ConfirmationRequested {
                confirmation_id: EntityId::new(),
                summary: "Share the draft".into(),
            },
        );
        reducer.apply_event(&confirmation).unwrap();
        assert_eq!(
            project_personal_intelligence(reducer.snapshot())
                .needs_you
                .len(),
            1
        );

        let mut corrected = reducer.snapshot().clone();
        corrected.through_sequence = Sequence::new(2);
        corrected.revision = Revision::new(2);
        corrected.confirmations.clear();
        reducer.apply_snapshot(corrected).unwrap();
        assert!(
            project_personal_intelligence(reducer.snapshot())
                .needs_you
                .is_empty()
        );

        let gap = envelope(
            reducer.snapshot(),
            Generation::new(1),
            4,
            4,
            DaemonEvent::PresenceChanged(PresenceProjection {
                session_id: reducer.snapshot().session.session_id.clone(),
                goal_id: None,
                state: PresenceState::Thinking,
                updated_at: UtcTimestamp::from_unix_millis(4),
                next_wake: None,
                safe_error: None,
            }),
        );
        assert_eq!(reducer.apply_event(&gap).unwrap(), ReductionOutcome::Gap);
        let personal = project_personal_intelligence(reducer.snapshot());
        assert_eq!(personal.presence.tone, PersonalPresenceTone::Ready);
        assert!(personal.work.is_empty());
    }

    #[test]
    fn personal_projection_reports_terminal_failure_without_claiming_completion() {
        let mut snapshot = session_snapshot();
        snapshot.presence.state = PresenceState::Failed;
        snapshot.presence.safe_error =
            Some("Connection was lost before the result committed".into());
        snapshot.terminal = Some(keith_protocol::TurnTerminalProjection {
            session_id: snapshot.session.session_id.clone(),
            turn_id: keith_agent_types::TurnId::new(),
            final_id: EntryId::new(),
            status: TurnTerminalStatus::Failed,
            execution_succeeded: false,
            final_created: true,
            artifacts_persisted: false,
            delivery_enqueued: false,
            delivery_acknowledged: false,
            detail: Some("The outcome is incomplete".into()),
        });

        let personal = project_personal_intelligence(&snapshot);
        assert_eq!(personal.presence.tone, PersonalPresenceTone::Failed);
        assert_eq!(personal.presence.label, "Could not finish");
        assert_eq!(personal.needs_you.len(), 1);
        assert!(personal.completed.is_empty());
        assert_eq!(personal.outputs[0].state_label, "Could not finish");
    }

    #[test]
    fn fabricated_presentation_claims_are_rejected() {
        for claim in [
            FabricatedClaim::CursorMovement,
            FabricatedClaim::Typing,
            FabricatedClaim::ToolUseWithoutEvent,
            FabricatedClaim::BackgroundThought,
            FabricatedClaim::ProgressPercentage,
            FabricatedClaim::CompletionWithoutEvent,
        ] {
            assert_eq!(
                reject_fabricated_claim(claim),
                Err(PresenceError::Fabricated)
            );
        }
    }

    #[test]
    fn evolution_projection_uses_readable_labels_and_keeps_ids_out_of_titles() {
        let promotion_id = EntityId::new();
        let projection = EvolutionProjection {
            protocol_version: CURRENT_PROTOCOL_VERSION,
            enabled: true,
            state: "observing_candidate".into(),
            availability: EvolutionAvailabilityProjection::Available {
                rustc: "rustc 1.90".into(),
                cargo: "cargo 1.90".into(),
            },
            disclosure: EvolutionDisclosureProjection {
                editable_surface: "the worker harness".into(),
                protected_surface: "memory and recovery".into(),
                autonomy: "verified changes only".into(),
                reversal: "one action".into(),
            },
            active: None,
            ledger: vec![EvolutionLedgerProjection {
                sequence: 4,
                occurred_at: UtcTimestamp::UNIX_EPOCH,
                kind: "promotion".into(),
                summary: "Reduced repeated tool calls".into(),
                state: "observing".into(),
                evidence: vec!["Repeated calls fell from 4 to 1".into()],
                measured_result: Some("75% fewer repeated calls".into()),
                readable_diff: Some("Stops after the first matching result".into()),
                hypothesis_id: None,
                promotion_id: Some(promotion_id.clone()),
                reversible: true,
            }],
            has_more_ledger: false,
            guidance: None,
        };
        let view = project_evolution(&projection);
        assert_eq!(view.ledger[0].title, "Reduced repeated tool calls");
        assert!(!view.ledger[0].title.contains(&promotion_id.to_string()));
        assert_eq!(view.ledger[0].reversal_promotion_id, Some(promotion_id));
        assert_eq!(view.ledger[0].state, "Observing");
        assert_eq!(
            view.ledger[0].evidence,
            vec!["Repeated calls fell from 4 to 1"]
        );
        assert_eq!(
            view.ledger[0].readable_diff.as_deref(),
            Some("Stops after the first matching result")
        );
        assert_eq!(
            view.ledger[0].measured_result.as_deref(),
            Some("75% fewer repeated calls")
        );
    }
}
