#![forbid(unsafe_code)]

use std::collections::BTreeSet;

use keith_agent_types::{Generation, GoalId, Sequence, SessionId, UtcTimestamp};
use keith_protocol::{
    DaemonEvent, EventEnvelope, MemoryChangeProjection, MessageProjection, MessageRole,
    SessionSnapshot,
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
}

impl OperatorSurface {
    pub const ALL: [Self; 21] = [
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
}

impl OperatorCommand {
    pub const ALL: [Self; 23] = [
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

#[derive(Clone, Debug, Eq, PartialEq)]
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
}
