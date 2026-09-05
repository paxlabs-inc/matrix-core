use std::collections::{BTreeMap, VecDeque};

use keith_agent_types::{
    CURRENT_PROTOCOL_VERSION, ClientId, CommandId, Generation, RootTreeId, Sequence, UtcTimestamp,
};
use keith_protocol::{
    CommandResultEnvelope, ConfirmationProjection, DaemonEvent, EventEnvelope, MessageProjection,
    MessageRole, ResumeCursor, ResumeMode, SessionSnapshot,
};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RecoveryBatch {
    pub mode: ResumeMode,
    pub snapshot: Option<SessionSnapshot>,
    pub events: Vec<EventEnvelope>,
}

#[derive(Debug, Error)]
pub enum EventStreamError {
    #[error("event and client queue capacities must be non-zero")]
    InvalidCapacity,
    #[error("snapshot root does not match stream root")]
    RootMismatch,
    #[error("event sequence overflow")]
    SequenceOverflow,
    #[error("snapshot revision overflow")]
    RevisionOverflow,
    #[error("event clock failed: {0}")]
    Clock(#[from] keith_agent_types::TimestampError),
    #[error("client {0} is not attached")]
    UnknownClient(ClientId),
    #[error("client acknowledgement is stale, ahead, or from another generation")]
    InvalidAcknowledgement,
    #[error("new generation must be strictly greater than the current generation")]
    InvalidGeneration,
}

#[derive(Clone, Debug)]
struct AttachedClient {
    generation: Generation,
    acknowledged: Sequence,
    pending: VecDeque<EventEnvelope>,
}

pub struct EventHub {
    root_tree_id: RootTreeId,
    generation: Generation,
    next_sequence: Sequence,
    replay_capacity: usize,
    client_queue_capacity: usize,
    replay: VecDeque<EventEnvelope>,
    snapshot: SessionSnapshot,
    clients: BTreeMap<ClientId, AttachedClient>,
}

impl EventHub {
    /// Creates a bounded event stream from a reconstructible snapshot.
    ///
    /// # Errors
    ///
    /// Returns an error for zero capacities or a snapshot belonging to another root.
    pub fn new(
        root_tree_id: RootTreeId,
        generation: Generation,
        mut snapshot: SessionSnapshot,
        replay_capacity: usize,
        client_queue_capacity: usize,
    ) -> Result<Self, EventStreamError> {
        if replay_capacity == 0 || client_queue_capacity == 0 {
            return Err(EventStreamError::InvalidCapacity);
        }
        if snapshot.session.root_tree_id != root_tree_id {
            return Err(EventStreamError::RootMismatch);
        }
        snapshot.generation = generation;
        let next_sequence = snapshot.through_sequence;
        Ok(Self {
            root_tree_id,
            generation,
            next_sequence,
            replay_capacity,
            client_queue_capacity,
            replay: VecDeque::with_capacity(replay_capacity),
            snapshot,
            clients: BTreeMap::new(),
        })
    }

    pub const fn generation(&self) -> Generation {
        self.generation
    }

    pub const fn through_sequence(&self) -> Sequence {
        self.next_sequence
    }

    pub fn snapshot(&self) -> &SessionSnapshot {
        &self.snapshot
    }

    pub fn attached_clients(&self) -> usize {
        self.clients.len()
    }

    /// Publishes one typed event with a strictly increasing sequence.
    ///
    /// Slow clients are disconnected before any non-replaceable event is dropped.
    ///
    /// # Errors
    ///
    /// Returns an error on clock, sequence, or snapshot revision overflow.
    pub fn publish(&mut self, event: DaemonEvent) -> Result<EventEnvelope, EventStreamError> {
        let sequence = self
            .next_sequence
            .checked_next()
            .ok_or(EventStreamError::SequenceOverflow)?;
        let occurred_at = UtcTimestamp::now()?;
        apply_event(&mut self.snapshot, &event)?;
        self.snapshot.generation = self.generation;
        self.snapshot.through_sequence = sequence;
        let envelope = EventEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            root_tree_id: self.root_tree_id.clone(),
            generation: self.generation,
            first_sequence: sequence,
            sequence,
            occurred_at,
            event,
        };
        self.next_sequence = sequence;
        self.replay.push_back(envelope.clone());
        while self.replay.len() > self.replay_capacity {
            self.replay.pop_front();
        }

        let mut disconnect = Vec::new();
        for (client_id, client) in &mut self.clients {
            if coalesce_delta(&mut client.pending, &envelope) {
                continue;
            }
            if client.pending.len() == self.client_queue_capacity {
                disconnect.push(client_id.clone());
            } else {
                client.pending.push_back(envelope.clone());
            }
        }
        for client_id in disconnect {
            self.clients.remove(&client_id);
        }
        Ok(envelope)
    }

    /// Publishes a replacement snapshot and returns the stream-stamped authoritative value.
    ///
    /// # Errors
    ///
    /// Returns an error when the snapshot belongs to another root or stream counters overflow.
    pub fn publish_snapshot(
        &mut self,
        snapshot: SessionSnapshot,
    ) -> Result<SessionSnapshot, EventStreamError> {
        if snapshot.session.root_tree_id != self.root_tree_id {
            return Err(EventStreamError::RootMismatch);
        }
        self.publish(DaemonEvent::Snapshot(Box::new(snapshot)))?;
        Ok(self.snapshot.clone())
    }

    /// Computes exact delta replay when retained, otherwise snapshot replacement.
    pub fn recover(&self, cursor: Option<&ResumeCursor>) -> RecoveryBatch {
        let Some(cursor) = cursor else {
            return self.snapshot_recovery();
        };
        if cursor.root_tree_id != self.root_tree_id
            || cursor.generation != self.generation
            || cursor.last_sequence > self.next_sequence
        {
            return self.snapshot_recovery();
        }
        let missing = cursor.last_sequence.checked_next();
        let oldest = self.replay.front().map(|event| event.sequence);
        let retained = cursor.last_sequence == self.next_sequence
            || missing.is_some_and(|missing| oldest.is_some_and(|oldest| missing >= oldest));
        if retained {
            RecoveryBatch {
                mode: ResumeMode::Delta,
                snapshot: None,
                events: self
                    .replay
                    .iter()
                    .filter(|event| event.sequence > cursor.last_sequence)
                    .cloned()
                    .collect(),
            }
        } else {
            self.snapshot_recovery()
        }
    }

    /// Attaches one client and returns its initial recovery batch.
    pub fn attach(&mut self, client_id: ClientId, cursor: Option<&ResumeCursor>) -> RecoveryBatch {
        let recovery = self.recover(cursor);
        let acknowledged = cursor
            .filter(|cursor| {
                recovery.mode == ResumeMode::Delta && cursor.generation == self.generation
            })
            .map_or(self.snapshot.through_sequence, |cursor| {
                cursor.last_sequence
            });
        self.clients.insert(
            client_id,
            AttachedClient {
                generation: self.generation,
                acknowledged,
                pending: VecDeque::with_capacity(self.client_queue_capacity),
            },
        );
        recovery
    }

    pub fn detach(&mut self, client_id: &ClientId) -> bool {
        self.clients.remove(client_id).is_some()
    }

    /// Advances a client's monotonic acknowledgement cursor.
    ///
    /// # Errors
    ///
    /// Returns an error for unknown clients, generation gaps, regression, or future sequences.
    pub fn acknowledge(
        &mut self,
        client_id: &ClientId,
        generation: Generation,
        sequence: Sequence,
    ) -> Result<(), EventStreamError> {
        let client = self
            .clients
            .get_mut(client_id)
            .ok_or_else(|| EventStreamError::UnknownClient(client_id.clone()))?;
        if generation != self.generation
            || generation != client.generation
            || sequence < client.acknowledged
            || sequence > self.next_sequence
        {
            return Err(EventStreamError::InvalidAcknowledgement);
        }
        client.acknowledged = sequence;
        Ok(())
    }

    /// Drains at most `limit` already-bounded events for one client.
    ///
    /// # Errors
    ///
    /// Returns an error when a slow client was disconnected or never attached.
    pub fn poll(
        &mut self,
        client_id: &ClientId,
        limit: usize,
    ) -> Result<Vec<EventEnvelope>, EventStreamError> {
        let client = self
            .clients
            .get_mut(client_id)
            .ok_or_else(|| EventStreamError::UnknownClient(client_id.clone()))?;
        let count = limit.min(client.pending.len());
        Ok(client.pending.drain(..count).collect())
    }

    /// Replaces the stream after worker recovery and forces attached clients through a gap.
    ///
    /// # Errors
    ///
    /// Returns an error unless generation advances and the snapshot root matches.
    pub fn replace_generation(
        &mut self,
        generation: Generation,
        mut snapshot: SessionSnapshot,
    ) -> Result<(), EventStreamError> {
        if generation <= self.generation {
            return Err(EventStreamError::InvalidGeneration);
        }
        if snapshot.session.root_tree_id != self.root_tree_id {
            return Err(EventStreamError::RootMismatch);
        }
        snapshot.generation = generation;
        snapshot.through_sequence = Sequence::ZERO;
        self.generation = generation;
        self.next_sequence = Sequence::ZERO;
        self.snapshot = snapshot;
        self.replay.clear();
        self.clients.clear();
        Ok(())
    }

    fn snapshot_recovery(&self) -> RecoveryBatch {
        RecoveryBatch {
            mode: ResumeMode::SnapshotThenDelta,
            snapshot: Some(self.snapshot.clone()),
            events: Vec::new(),
        }
    }
}

fn coalesce_delta(queue: &mut VecDeque<EventEnvelope>, incoming: &EventEnvelope) -> bool {
    let DaemonEvent::AssistantDelta {
        message_id,
        text: incoming_text,
    } = &incoming.event
    else {
        return false;
    };
    let Some(last) = queue.back_mut() else {
        return false;
    };
    let DaemonEvent::AssistantDelta {
        message_id: queued_id,
        text: queued_text,
    } = &mut last.event
    else {
        return false;
    };
    if queued_id != message_id {
        return false;
    }
    queued_text.push_str(incoming_text);
    last.sequence = incoming.sequence;
    last.occurred_at = incoming.occurred_at;
    true
}

#[allow(clippy::too_many_lines)]
fn apply_event(
    snapshot: &mut SessionSnapshot,
    event: &DaemonEvent,
) -> Result<(), EventStreamError> {
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
            upsert(&mut snapshot.memory_changes, change.clone(), |item| {
                item.entry_id.clone()
            });
        }
        DaemonEvent::UsageChanged(usage) => snapshot.usage = *usage,
        DaemonEvent::PresenceChanged(presence) => snapshot.presence = presence.clone(),
        DaemonEvent::ConfirmationRequested {
            confirmation_id,
            summary,
        } => upsert(
            &mut snapshot.confirmations,
            ConfirmationProjection {
                confirmation_id: confirmation_id.clone(),
                summary: summary.clone(),
            },
            |item| item.confirmation_id.clone(),
        ),
        DaemonEvent::ConfirmationResolved { confirmation_id } => snapshot
            .confirmations
            .retain(|item| item.confirmation_id != *confirmation_id),
        DaemonEvent::AgentActivity(_)
        | DaemonEvent::EvolutionChanged(_)
        | DaemonEvent::CommandAccepted { .. }
        | DaemonEvent::CommandRejected(_)
        | DaemonEvent::Warning(_)
        | DaemonEvent::Error(_) => {}
    }
    snapshot.revision = snapshot
        .revision
        .checked_next()
        .ok_or(EventStreamError::RevisionOverflow)?;
    Ok(())
}

fn upsert<T, K: Eq>(items: &mut Vec<T>, value: T, key: impl Fn(&T) -> K) {
    let value_key = key(&value);
    if let Some(index) = items.iter().position(|item| key(item) == value_key) {
        items[index] = value;
    } else {
        items.push(value);
    }
}

pub struct CommandLedger {
    capacity: usize,
    order: VecDeque<CommandId>,
    results: BTreeMap<CommandId, CommandResultEnvelope>,
}

impl CommandLedger {
    /// # Errors
    ///
    /// Returns an error when `capacity` is zero.
    pub fn new(capacity: usize) -> Result<Self, EventStreamError> {
        if capacity == 0 {
            return Err(EventStreamError::InvalidCapacity);
        }
        Ok(Self {
            capacity,
            order: VecDeque::with_capacity(capacity),
            results: BTreeMap::new(),
        })
    }

    pub fn execute_once(
        &mut self,
        command_id: CommandId,
        execute: impl FnOnce() -> CommandResultEnvelope,
    ) -> (bool, CommandResultEnvelope) {
        if let Some(result) = self.results.get(&command_id) {
            return (false, result.clone());
        }
        let result = execute();
        self.order.push_back(command_id.clone());
        self.results.insert(command_id, result.clone());
        while self.order.len() > self.capacity {
            if let Some(oldest) = self.order.pop_front() {
                self.results.remove(&oldest);
            }
        }
        (true, result)
    }

    pub fn result(&self, command_id: &CommandId) -> Option<&CommandResultEnvelope> {
        self.results.get(command_id)
    }

    pub fn record(&mut self, result: CommandResultEnvelope) {
        if self.results.contains_key(&result.command_id) {
            return;
        }
        self.order.push_back(result.command_id.clone());
        self.results.insert(result.command_id.clone(), result);
        while self.order.len() > self.capacity {
            if let Some(oldest) = self.order.pop_front() {
                self.results.remove(&oldest);
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use std::cell::Cell;

    use keith_agent_types::{
        CURRENT_PROTOCOL_VERSION, ChildId, DeliveryId, EntityId, EntryId, GoalId, MessageId,
        ProfileId, Revision, SessionId, ToolCallId, TurnId,
    };
    use keith_protocol::{
        ChildProjection, CommandResult, DeliveryProjection, GoalProjection, GoalState,
        MessageProjection, SessionState, SessionSummary, ToolProjection, TurnTerminalProjection,
        TurnTerminalStatus, WaitProjection,
    };

    use super::*;

    fn snapshot(root: RootTreeId, generation: Generation) -> SessionSnapshot {
        let session_id = SessionId::new();
        SessionSnapshot {
            session: SessionSummary {
                session_id: session_id.clone(),
                root_tree_id: root,
                profile_id: ProfileId::new(),
                title: None,
                state: SessionState::Ready,
                updated_at: UtcTimestamp::UNIX_EPOCH,
            },
            generation,
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
            usage: keith_protocol::UsageProjection::default(),
            presence: keith_protocol::PresenceProjection {
                session_id,
                goal_id: None,
                state: keith_protocol::PresenceState::Available,
                updated_at: UtcTimestamp::UNIX_EPOCH,
                next_wake: None,
                safe_error: None,
            },
            terminal: None,
            revision: Revision::ZERO,
        }
    }

    fn committed(index: usize) -> DaemonEvent {
        DaemonEvent::MessageCommitted(MessageProjection {
            message_id: MessageId::new(),
            final_id: None,
            role: MessageRole::Assistant,
            text: format!("message {index}"),
            committed: true,
        })
    }

    #[test]
    fn disconnect_at_every_boundary_replays_exact_missing_suffix() {
        let root = RootTreeId::new();
        let generation = Generation::new(4);
        let mut hub = EventHub::new(
            root.clone(),
            generation,
            snapshot(root.clone(), generation),
            32,
            8,
        )
        .unwrap();
        let events: Vec<_> = (0..8)
            .map(|index| hub.publish(committed(index)).unwrap())
            .collect();
        for boundary in 0..=events.len() {
            let recovery = hub.recover(Some(&ResumeCursor {
                root_tree_id: root.clone(),
                generation,
                last_sequence: Sequence::new(u64::try_from(boundary).unwrap()),
            }));
            assert_eq!(recovery.mode, ResumeMode::Delta);
            assert_eq!(recovery.events, events[boundary..]);
        }
    }

    #[test]
    fn overflow_and_generation_gap_use_reconstructible_snapshot() {
        let root = RootTreeId::new();
        let generation = Generation::new(1);
        let mut hub = EventHub::new(
            root.clone(),
            generation,
            snapshot(root.clone(), generation),
            2,
            2,
        )
        .unwrap();
        for index in 0..5 {
            hub.publish(committed(index)).unwrap();
        }
        let overflow = hub.recover(Some(&ResumeCursor {
            root_tree_id: root.clone(),
            generation,
            last_sequence: Sequence::new(1),
        }));
        assert_eq!(overflow.mode, ResumeMode::SnapshotThenDelta);
        assert_eq!(overflow.snapshot.unwrap().messages.len(), 5);

        hub.replace_generation(
            Generation::new(2),
            snapshot(root.clone(), Generation::new(2)),
        )
        .unwrap();
        let gap = hub.recover(Some(&ResumeCursor {
            root_tree_id: root,
            generation,
            last_sequence: Sequence::new(5),
        }));
        assert_eq!(gap.mode, ResumeMode::SnapshotThenDelta);
        assert_eq!(gap.snapshot.unwrap().generation, Generation::new(2));
    }

    #[test]
    fn published_snapshot_returns_the_stream_stamped_authoritative_value() {
        let root = RootTreeId::new();
        let generation = Generation::new(3);
        let mut hub = EventHub::new(
            root.clone(),
            generation,
            snapshot(root.clone(), generation),
            8,
            8,
        )
        .unwrap();
        hub.publish(committed(0)).unwrap();
        let mut replacement = snapshot(root, generation);
        replacement.messages.push(MessageProjection {
            message_id: MessageId::new(),
            final_id: None,
            role: MessageRole::Assistant,
            text: "completed turn".into(),
            committed: true,
        });
        replacement.revision = Revision::new(7);

        let authoritative = hub.publish_snapshot(replacement).unwrap();

        assert_eq!(authoritative.generation, generation);
        assert_eq!(authoritative.through_sequence, Sequence::new(2));
        assert_eq!(authoritative.revision, Revision::new(8));
        assert_eq!(authoritative.messages[0].text, "completed turn");
        assert_eq!(hub.snapshot(), &authoritative);
    }

    #[test]
    fn disconnect_before_or_after_terminal_replays_one_final_and_the_same_terminal() {
        let root = RootTreeId::new();
        let generation = Generation::new(6);
        let initial = snapshot(root.clone(), generation);
        let session_id = initial.session.session_id.clone();
        let mut hub = EventHub::new(root.clone(), generation, initial, 8, 8).unwrap();
        let final_event = hub.publish(committed(0)).unwrap();
        let terminal = TurnTerminalProjection {
            session_id,
            turn_id: TurnId::new(),
            final_id: EntryId::new(),
            status: TurnTerminalStatus::Failed,
            execution_succeeded: false,
            final_created: true,
            artifacts_persisted: true,
            delivery_enqueued: true,
            delivery_acknowledged: false,
            detail: Some("provider unavailable".into()),
        };
        let terminal_event = hub
            .publish(DaemonEvent::TurnTerminal(terminal.clone()))
            .unwrap();
        let before_final = hub.recover(Some(&ResumeCursor {
            root_tree_id: root.clone(),
            generation,
            last_sequence: Sequence::ZERO,
        }));
        assert_eq!(
            before_final.events,
            vec![final_event, terminal_event.clone()]
        );
        let before_terminal = hub.recover(Some(&ResumeCursor {
            root_tree_id: root.clone(),
            generation,
            last_sequence: Sequence::new(1),
        }));
        assert_eq!(before_terminal.events, vec![terminal_event]);
        let after_terminal = hub.recover(Some(&ResumeCursor {
            root_tree_id: root,
            generation,
            last_sequence: Sequence::new(2),
        }));
        assert!(after_terminal.events.is_empty());
        assert_eq!(hub.snapshot().messages.len(), 1);
        assert_eq!(hub.snapshot().terminal.as_ref(), Some(&terminal));
    }

    #[test]
    fn slow_clients_are_bounded_deltas_coalesce_and_terminal_state_is_recoverable() {
        let root = RootTreeId::new();
        let generation = Generation::new(1);
        let mut hub =
            EventHub::new(root.clone(), generation, snapshot(root, generation), 16, 2).unwrap();
        let fast = ClientId::new();
        let slow = ClientId::new();
        hub.attach(fast.clone(), None);
        hub.attach(slow.clone(), None);
        for index in 0..3 {
            hub.publish(committed(index)).unwrap();
            hub.poll(&fast, 1).unwrap();
        }
        assert_eq!(hub.attached_clients(), 1);
        assert!(matches!(
            hub.poll(&slow, 1),
            Err(EventStreamError::UnknownClient(_))
        ));

        let delta_client = ClientId::new();
        hub.attach(delta_client.clone(), None);
        let message_id = MessageId::new();
        for text in ["hel", "lo"] {
            hub.publish(DaemonEvent::AssistantDelta {
                message_id: message_id.clone(),
                text: text.into(),
            })
            .unwrap();
        }
        let pending = hub.poll(&delta_client, 8).unwrap();
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].first_sequence, Sequence::new(4));
        assert_eq!(pending[0].sequence, Sequence::new(5));
        assert!(matches!(
            &pending[0].event,
            DaemonEvent::AssistantDelta { text, .. } if text == "hello"
        ));

        let confirmation_id = EntityId::new();
        hub.publish(DaemonEvent::ConfirmationRequested {
            confirmation_id: confirmation_id.clone(),
            summary: "approve".into(),
        })
        .unwrap();
        hub.publish(DaemonEvent::GoalChanged(GoalProjection {
            goal_id: GoalId::new(),
            objective: "finish".into(),
            state: GoalState::Complete,
        }))
        .unwrap();
        hub.publish(DaemonEvent::ChildChanged(ChildProjection {
            child_id: ChildId::new(),
            session_id: SessionId::new(),
            objective: "child".into(),
            state: "complete".into(),
        }))
        .unwrap();
        hub.publish(DaemonEvent::ToolChanged(ToolProjection {
            tool_call_id: ToolCallId::new(),
            tool: Some("test_tool".into()),
            state: "complete".into(),
            terminal: true,
        }))
        .unwrap();
        hub.publish(DaemonEvent::WaitChanged(WaitProjection {
            wait_id: EntityId::new(),
            state: "complete".into(),
            terminal: true,
        }))
        .unwrap();
        hub.publish(DaemonEvent::DeliveryChanged(DeliveryProjection {
            delivery_id: DeliveryId::new(),
            state: "sent".into(),
            terminal: true,
            turn_id: None,
            final_id: None,
            acknowledged: true,
        }))
        .unwrap();
        let snapshot = hub.snapshot();
        assert_eq!(snapshot.confirmations[0].confirmation_id, confirmation_id);
        assert_eq!(snapshot.goals.len(), 1);
        assert_eq!(snapshot.children.len(), 1);
        assert_eq!(snapshot.tools.len(), 1);
        assert_eq!(snapshot.waits.len(), 1);
        assert_eq!(snapshot.deliveries.len(), 1);
    }

    #[test]
    fn acknowledgements_detach_and_command_deduplication_are_explicit() {
        let root = RootTreeId::new();
        let generation = Generation::new(1);
        let mut hub =
            EventHub::new(root.clone(), generation, snapshot(root, generation), 8, 8).unwrap();
        let client = ClientId::new();
        hub.attach(client.clone(), None);
        hub.publish(committed(0)).unwrap();
        hub.acknowledge(&client, generation, Sequence::new(1))
            .unwrap();
        assert!(
            hub.acknowledge(&client, generation, Sequence::ZERO)
                .is_err()
        );
        assert!(hub.detach(&client));

        let mut ledger = CommandLedger::new(4).unwrap();
        let command_id = CommandId::new();
        let calls = Cell::new(0);
        let make_result = || {
            calls.set(calls.get() + 1);
            CommandResultEnvelope {
                protocol: CURRENT_PROTOCOL_VERSION,
                command_id: command_id.clone(),
                completed_at: UtcTimestamp::UNIX_EPOCH,
                result: CommandResult::Accepted { action_id: None },
            }
        };
        let (executed, first) = ledger.execute_once(command_id.clone(), make_result);
        assert!(executed);
        let (executed, second) = ledger.execute_once(command_id.clone(), make_result);
        assert!(!executed);
        assert_eq!(first, second);
        assert_eq!(calls.get(), 1);
    }
}
