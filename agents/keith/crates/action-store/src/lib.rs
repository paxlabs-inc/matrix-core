#![forbid(unsafe_code)]

use std::cmp::Ordering;
use std::fmt::Display;
use std::sync::{Mutex, MutexGuard};

use keith_agent_types::{
    ActionId, ArtifactId, CURRENT_SCHEMA_VERSION, ChildId, ClientId, EntityId, GoalId, JobId,
    MessageId, Revision, SchemaVersion, SessionId, UtcTimestamp,
};
use keith_state_store_core::{ActionRepository, VersionedRecord, WritePrecondition};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "source")]
pub enum ActionSource {
    Interactive {
        client_id: ClientId,
    },
    Channel {
        channel: String,
        message_id: String,
    },
    Schedule {
        job_id: JobId,
        attempt: u32,
    },
    Child {
        child_id: ChildId,
        message_id: MessageId,
    },
    Steering {
        client_id: ClientId,
    },
    FollowUp,
    Waiting {
        wake_id: EntityId,
    },
    Awareness {
        event_id: EntityId,
    },
    Refinement {
        transaction_id: EntityId,
    },
    AutonomousContinuation {
        goal_id: GoalId,
    },
    Evolution {
        generation_id: EntityId,
        ancestry: Vec<ActionAncestorKind>,
        execution: EvolutionExecution,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ActionAncestorKind {
    Ordinary,
    Evolution,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EvolutionExecution {
    OrdinarySession,
    DedicatedChild,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EvolutionOperation {
    EvaluateHypothesis,
    PrepareShadow,
    BuildCandidate,
    RunCanary,
    ObservePromotion,
    ReclaimResources,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DeliveryPolicy {
    Immediate,
    NextTurnBoundary,
    WhenIdle,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ActionPriority {
    Interrupt,
    User,
    ChildResult,
    Scheduled,
    Background,
}

impl ActionPriority {
    const fn rank(self) -> u8 {
        match self {
            Self::Interrupt => 0,
            Self::User => 1,
            Self::ChildResult => 2,
            Self::Scheduled => 3,
            Self::Background => 4,
        }
    }
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ActionLimits {
    pub max_turns: Option<u32>,
    pub max_tokens: Option<u64>,
    pub max_elapsed_ms: Option<u64>,
    pub max_tool_calls: Option<u32>,
    pub max_children: Option<u16>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "route")]
pub enum ReplyRoute {
    Client {
        client_id: ClientId,
    },
    Channel {
        channel: String,
        external_account: Option<String>,
        conversation_id: String,
        thread_id: Option<String>,
        reply_to_message: Option<String>,
    },
    Session {
        session_id: SessionId,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "payload")]
pub enum ActionPayload {
    Prompt {
        text: String,
    },
    ChannelMessage {
        text: String,
        attachments: Vec<ArtifactId>,
    },
    Scheduled {
        instruction: String,
    },
    ChildMessage {
        text: String,
        artifacts: Vec<ArtifactId>,
    },
    Steering {
        text: String,
    },
    FollowUp {
        text: String,
    },
    ResumeWaiting {
        waiting_id: EntityId,
    },
    ContinueGoal {
        goal_id: GoalId,
    },
    Awareness {
        event_id: EntityId,
        summary: String,
    },
    Refinement {
        transaction_id: EntityId,
    },
    SystemMaintenance {
        operation: String,
    },
    Evolution {
        operation: EvolutionOperation,
    },
}

impl ActionPayload {
    fn text(&self) -> Option<&str> {
        match self {
            Self::Prompt { text }
            | Self::ChannelMessage { text, .. }
            | Self::ChildMessage { text, .. }
            | Self::Steering { text }
            | Self::FollowUp { text } => Some(text),
            Self::Scheduled { instruction } => Some(instruction),
            Self::Awareness { summary, .. } => Some(summary),
            Self::ResumeWaiting { .. } | Self::ContinueGoal { .. } | Self::Refinement { .. } => {
                None
            }
            Self::SystemMaintenance { operation } => Some(operation),
            Self::Evolution { .. } => None,
        }
    }

    const fn is_steering(&self) -> bool {
        matches!(self, Self::Steering { .. })
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SessionAction {
    pub id: ActionId,
    pub session_id: SessionId,
    pub source: ActionSource,
    pub delivery: DeliveryPolicy,
    pub priority: ActionPriority,
    pub created_at: UtcTimestamp,
    pub not_before: Option<UtcTimestamp>,
    pub deadline: Option<UtcTimestamp>,
    pub limits: ActionLimits,
    pub reply_route: Option<ReplyRoute>,
    pub payload: ActionPayload,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ActionState {
    Queued,
    Admitted,
    Running,
    Waiting,
    Completed,
    Failed,
    Cancelled,
    Expired,
}

impl ActionState {
    pub const fn is_terminal(self) -> bool {
        matches!(
            self,
            Self::Completed | Self::Failed | Self::Cancelled | Self::Expired
        )
    }

    const fn allows(self, next: Self) -> bool {
        match self {
            Self::Queued => matches!(next, Self::Admitted | Self::Cancelled | Self::Expired),
            Self::Admitted => matches!(
                next,
                Self::Running | Self::Completed | Self::Cancelled | Self::Expired
            ),
            Self::Running => matches!(
                next,
                Self::Waiting | Self::Completed | Self::Failed | Self::Cancelled
            ),
            Self::Waiting => matches!(
                next,
                Self::Admitted | Self::Completed | Self::Failed | Self::Cancelled | Self::Expired
            ),
            Self::Completed | Self::Failed | Self::Cancelled | Self::Expired => false,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ActionRecord {
    pub version: SchemaVersion,
    pub action: SessionAction,
    pub state: ActionState,
    pub enqueue_sequence: u64,
    pub revision: Revision,
    pub updated_at: UtcTimestamp,
    pub terminal_detail: Option<String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ActionInboxConfig {
    pub max_queued_per_session: usize,
    pub max_background_queued_per_session: usize,
}

impl Default for ActionInboxConfig {
    fn default() -> Self {
        Self {
            max_queued_per_session: 1_024,
            max_background_queued_per_session: 128,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct PumpContext {
    pub active_action: Option<ActionId>,
    pub at_turn_boundary: bool,
    pub session_idle: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AdmissionKind {
    StartTurn,
    ApplySteering,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SelectedAction {
    pub record: ActionRecord,
    pub kind: AdmissionKind,
}

#[derive(Debug, Error)]
pub enum ActionStoreError {
    #[error("action repository failed: {0}")]
    Repository(String),
    #[error("action {0} was not found")]
    NotFound(ActionId),
    #[error("action record is corrupt: {0}")]
    Corrupt(String),
    #[error("action is invalid: {0}")]
    Invalid(String),
    #[error("session action queue reached its configured limit")]
    QueueFull,
    #[error("session background queue reached its configured limit")]
    BackgroundQueueFull,
    #[error("evolution actions must use background priority and when-idle delivery")]
    EvolutionScheduling,
    #[error("recursive evolution actions are refused at admission")]
    RecursiveEvolution,
    #[error("children dedicated to evolution are refused at admission")]
    DedicatedEvolutionChild,
    #[error("action cannot transition from {from:?} to {to:?}")]
    IllegalTransition { from: ActionState, to: ActionState },
    #[error("session already has a running model turn")]
    TurnAlreadyRunning,
    #[error("action revision overflow")]
    RevisionOverflow,
    #[error("action enqueue sequence overflow")]
    SequenceOverflow,
    #[error("action store lock was poisoned")]
    LockPoisoned,
}

struct StoredAction {
    value: ActionRecord,
    storage_revision: Revision,
}

pub struct PersistentActionInbox<R> {
    repository: R,
    config: ActionInboxConfig,
    serial: Mutex<()>,
}

impl<R> PersistentActionInbox<R>
where
    R: ActionRepository,
    R::Error: Display,
{
    /// # Errors
    ///
    /// Returns an error when the queue limits are inconsistent.
    pub fn new(repository: R, config: ActionInboxConfig) -> Result<Self, ActionStoreError> {
        if config.max_queued_per_session == 0
            || config.max_background_queued_per_session > config.max_queued_per_session
        {
            return Err(ActionStoreError::Invalid(
                "queue limits must be non-zero and background cannot exceed total".into(),
            ));
        }
        Ok(Self {
            repository,
            config,
            serial: Mutex::new(()),
        })
    }

    /// # Errors
    ///
    /// Returns an error for invalid, duplicate, over-limit, or unpersistable actions.
    pub fn submit(
        &self,
        action: SessionAction,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        validate_action(&action)?;
        let _guard = self.lock()?;
        let records = self.load_all()?;
        if records
            .iter()
            .any(|record| record.value.action.id == action.id)
        {
            return Err(ActionStoreError::Invalid("action ID already exists".into()));
        }
        let session_records = records
            .iter()
            .filter(|record| record.value.action.session_id == action.session_id)
            .collect::<Vec<_>>();
        let queued = session_records
            .iter()
            .filter(|record| !record.value.state.is_terminal())
            .count();
        if queued >= self.config.max_queued_per_session {
            return Err(ActionStoreError::QueueFull);
        }
        let background = session_records
            .iter()
            .filter(|record| {
                !record.value.state.is_terminal()
                    && record.value.action.priority == ActionPriority::Background
            })
            .count();
        if action.priority == ActionPriority::Background
            && background >= self.config.max_background_queued_per_session
        {
            return Err(ActionStoreError::BackgroundQueueFull);
        }
        let enqueue_sequence = session_records
            .iter()
            .map(|record| record.value.enqueue_sequence)
            .max()
            .unwrap_or(0)
            .checked_add(1)
            .ok_or(ActionStoreError::SequenceOverflow)?;
        let expired = action.deadline.is_some_and(|deadline| now >= deadline);
        let record = ActionRecord {
            version: CURRENT_SCHEMA_VERSION,
            action,
            state: if expired {
                ActionState::Expired
            } else {
                ActionState::Queued
            },
            enqueue_sequence,
            revision: Revision::ZERO,
            updated_at: now,
            terminal_detail: expired.then(|| "deadline elapsed before admission".into()),
        };
        self.put_new(&record)?;
        Ok(record)
    }

    /// # Errors
    ///
    /// Returns an error when the repository cannot read or decode the action.
    pub fn get(&self, id: &ActionId) -> Result<Option<ActionRecord>, ActionStoreError> {
        let _guard = self.lock()?;
        self.load(id).map(|stored| stored.map(|value| value.value))
    }

    /// # Errors
    ///
    /// Returns an error when the repository cannot list or decode the session actions.
    pub fn list_session(
        &self,
        session_id: &SessionId,
    ) -> Result<Vec<ActionRecord>, ActionStoreError> {
        let _guard = self.lock()?;
        let mut records = self
            .load_all()?
            .into_iter()
            .filter(|record| &record.value.action.session_id == session_id)
            .map(|record| record.value)
            .collect::<Vec<_>>();
        records.sort_by(action_order);
        Ok(records)
    }

    /// # Errors
    ///
    /// Returns an error when expiry or admission cannot be persisted.
    pub fn select_next(
        &self,
        session_id: &SessionId,
        now: UtcTimestamp,
        context: &PumpContext,
    ) -> Result<Option<SelectedAction>, ActionStoreError> {
        let _guard = self.lock()?;
        let mut records = self.load_all()?;
        for record in records.iter_mut().filter(|record| {
            record.value.action.session_id == *session_id
                && matches!(
                    record.value.state,
                    ActionState::Queued | ActionState::Waiting
                )
                && record
                    .value
                    .action
                    .deadline
                    .is_some_and(|deadline| now >= deadline)
        }) {
            self.transition_stored(
                record,
                ActionState::Expired,
                now,
                Some("deadline elapsed".into()),
            )?;
        }
        let mut eligible = records
            .into_iter()
            .filter(|record| {
                record.value.action.session_id == *session_id
                    && record.value.state == ActionState::Queued
                    && record
                        .value
                        .action
                        .not_before
                        .is_none_or(|not_before| now >= not_before)
                    && eligibility(&record.value.action, context).is_some()
            })
            .collect::<Vec<_>>();
        eligible.sort_by(|left, right| action_order(&left.value, &right.value));
        let Some(mut selected) = eligible.into_iter().next() else {
            return Ok(None);
        };
        let kind = eligibility(&selected.value.action, context)
            .ok_or_else(|| ActionStoreError::Invalid("selected action became ineligible".into()))?;
        self.transition_stored(&mut selected, ActionState::Admitted, now, None)?;
        Ok(Some(SelectedAction {
            record: selected.value,
            kind,
        }))
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition, concurrent turn, or persistence failure.
    pub fn mark_running(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        if self.load_all()?.iter().any(|record| {
            record.value.action.session_id == stored.value.action.session_id
                && record.value.action.id != *id
                && record.value.state == ActionState::Running
        }) {
            return Err(ActionStoreError::TurnAlreadyRunning);
        }
        self.transition_stored(&mut stored, ActionState::Running, now, None)?;
        Ok(stored.value)
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition or persistence failure.
    pub fn mark_waiting(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.transition(id, ActionState::Waiting, now, None)
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition or persistence failure.
    pub fn resume_waiting(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.transition(id, ActionState::Admitted, now, None)
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition or persistence failure.
    pub fn complete(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.transition(id, ActionState::Completed, now, None)
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition or persistence failure.
    pub fn fail(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
        detail: impl Into<String>,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.transition(id, ActionState::Failed, now, Some(detail.into()))
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition or persistence failure.
    pub fn cancel(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
        detail: impl Into<String>,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.transition(id, ActionState::Cancelled, now, Some(detail.into()))
    }

    fn transition(
        &self,
        id: &ActionId,
        next: ActionState,
        now: UtcTimestamp,
        detail: Option<String>,
    ) -> Result<ActionRecord, ActionStoreError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        self.transition_stored(&mut stored, next, now, detail)?;
        Ok(stored.value)
    }

    fn transition_stored(
        &self,
        stored: &mut StoredAction,
        next: ActionState,
        now: UtcTimestamp,
        detail: Option<String>,
    ) -> Result<(), ActionStoreError> {
        if !stored.value.state.allows(next) {
            return Err(ActionStoreError::IllegalTransition {
                from: stored.value.state,
                to: next,
            });
        }
        let next_revision = stored
            .storage_revision
            .checked_next()
            .ok_or(ActionStoreError::RevisionOverflow)?;
        stored.value.state = next;
        stored.value.revision = next_revision;
        stored.value.updated_at = now;
        stored.value.terminal_detail = detail;
        self.repository
            .put_action(
                encode(&stored.value)?,
                WritePrecondition::Exact(stored.storage_revision),
            )
            .map_err(repository_error)?;
        stored.storage_revision = next_revision;
        Ok(())
    }

    fn required(&self, id: &ActionId) -> Result<StoredAction, ActionStoreError> {
        self.load(id)?
            .ok_or_else(|| ActionStoreError::NotFound(id.clone()))
    }

    fn load(&self, id: &ActionId) -> Result<Option<StoredAction>, ActionStoreError> {
        self.repository
            .get_action(id.as_entity_id())
            .map_err(repository_error)?
            .map(decode)
            .transpose()
    }

    fn load_all(&self) -> Result<Vec<StoredAction>, ActionStoreError> {
        self.repository
            .list_actions()
            .map_err(repository_error)?
            .into_iter()
            .map(decode)
            .collect()
    }

    fn put_new(&self, record: &ActionRecord) -> Result<(), ActionStoreError> {
        self.repository
            .put_action(encode(record)?, WritePrecondition::Missing)
            .map_err(repository_error)?;
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, ActionStoreError> {
        self.serial
            .lock()
            .map_err(|_| ActionStoreError::LockPoisoned)
    }
}

fn validate_action(action: &SessionAction) -> Result<(), ActionStoreError> {
    if action
        .payload
        .text()
        .is_some_and(|text| text.trim().is_empty())
    {
        return Err(ActionStoreError::Invalid(
            "text-bearing payloads cannot be empty".into(),
        ));
    }
    if action
        .not_before
        .zip(action.deadline)
        .is_some_and(|(not_before, deadline)| not_before >= deadline)
    {
        return Err(ActionStoreError::Invalid(
            "not-before must precede deadline".into(),
        ));
    }
    if action.delivery == DeliveryPolicy::NextTurnBoundary && !action.payload.is_steering() {
        return Err(ActionStoreError::Invalid(
            "turn-boundary delivery is reserved for steering payloads".into(),
        ));
    }
    match (&action.source, &action.payload) {
        (
            ActionSource::Evolution {
                ancestry,
                execution,
                ..
            },
            ActionPayload::Evolution { .. },
        ) => {
            if action.priority != ActionPriority::Background
                || action.delivery != DeliveryPolicy::WhenIdle
            {
                return Err(ActionStoreError::EvolutionScheduling);
            }
            if *execution == EvolutionExecution::DedicatedChild {
                return Err(ActionStoreError::DedicatedEvolutionChild);
            }
            if ancestry.len() > 32 || ancestry.contains(&ActionAncestorKind::Evolution) {
                return Err(ActionStoreError::RecursiveEvolution);
            }
        }
        (ActionSource::Evolution { .. }, _) | (_, ActionPayload::Evolution { .. }) => {
            return Err(ActionStoreError::Invalid(
                "evolution source and payload must be paired".into(),
            ));
        }
        _ => {}
    }
    Ok(())
}

fn eligibility(action: &SessionAction, context: &PumpContext) -> Option<AdmissionKind> {
    match action.delivery {
        DeliveryPolicy::Immediate => {
            if action.priority == ActionPriority::Interrupt && action.payload.is_steering() {
                context
                    .active_action
                    .as_ref()
                    .map(|_| AdmissionKind::ApplySteering)
            } else if context.active_action.is_none() {
                Some(AdmissionKind::StartTurn)
            } else {
                None
            }
        }
        DeliveryPolicy::NextTurnBoundary => {
            if context.active_action.is_some() && context.at_turn_boundary {
                Some(AdmissionKind::ApplySteering)
            } else if context.active_action.is_none() {
                Some(AdmissionKind::StartTurn)
            } else {
                None
            }
        }
        DeliveryPolicy::WhenIdle => (context.active_action.is_none() && context.session_idle)
            .then_some(AdmissionKind::StartTurn),
    }
}

fn action_order(left: &ActionRecord, right: &ActionRecord) -> Ordering {
    left.action
        .priority
        .rank()
        .cmp(&right.action.priority.rank())
        .then_with(|| left.enqueue_sequence.cmp(&right.enqueue_sequence))
        .then_with(|| left.action.id.cmp(&right.action.id))
}

fn encode(record: &ActionRecord) -> Result<VersionedRecord, ActionStoreError> {
    let payload = serde_json::to_value(record)
        .map_err(|error| ActionStoreError::Corrupt(error.to_string()))?;
    Ok(VersionedRecord {
        version: record.version,
        id: record.action.id.as_entity_id().clone(),
        revision: record.revision,
        updated_at: record.updated_at,
        payload,
    })
}

fn decode(record: VersionedRecord) -> Result<StoredAction, ActionStoreError> {
    let value: ActionRecord = serde_json::from_value(record.payload)
        .map_err(|error| ActionStoreError::Corrupt(error.to_string()))?;
    if value.version.major != CURRENT_SCHEMA_VERSION.major
        || value.version.minor > CURRENT_SCHEMA_VERSION.minor
        || value.action.id.as_entity_id() != &record.id
        || value.revision != record.revision
    {
        return Err(ActionStoreError::Corrupt(
            "record envelope does not match its action payload".into(),
        ));
    }
    Ok(StoredAction {
        value,
        storage_revision: record.revision,
    })
}

fn repository_error(error: impl Display) -> ActionStoreError {
    ActionStoreError::Repository(error.to_string())
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use keith_state_store::EmbeddedStore;
    use tempfile::tempdir;

    use super::*;

    fn action(
        session_id: &SessionId,
        priority: ActionPriority,
        delivery: DeliveryPolicy,
        text: &str,
        created_at: i64,
    ) -> SessionAction {
        let client_id = ClientId::new();
        SessionAction {
            id: ActionId::new(),
            session_id: session_id.clone(),
            source: if delivery == DeliveryPolicy::NextTurnBoundary {
                ActionSource::Steering {
                    client_id: client_id.clone(),
                }
            } else {
                ActionSource::Interactive {
                    client_id: client_id.clone(),
                }
            },
            delivery,
            priority,
            created_at: UtcTimestamp::from_unix_millis(created_at),
            not_before: None,
            deadline: None,
            limits: ActionLimits::default(),
            reply_route: Some(ReplyRoute::Client { client_id }),
            payload: if delivery == DeliveryPolicy::NextTurnBoundary {
                ActionPayload::Steering { text: text.into() }
            } else {
                ActionPayload::Prompt { text: text.into() }
            },
        }
    }

    fn inbox(path: &Path, config: ActionInboxConfig) -> PersistentActionInbox<EmbeddedStore> {
        PersistentActionInbox::new(EmbeddedStore::open(path, None).unwrap(), config).unwrap()
    }

    fn evolution_action(session_id: &SessionId) -> SessionAction {
        SessionAction {
            id: ActionId::new(),
            session_id: session_id.clone(),
            source: ActionSource::Evolution {
                generation_id: EntityId::new(),
                ancestry: vec![ActionAncestorKind::Ordinary],
                execution: EvolutionExecution::OrdinarySession,
            },
            delivery: DeliveryPolicy::WhenIdle,
            priority: ActionPriority::Background,
            created_at: UtcTimestamp::UNIX_EPOCH,
            not_before: None,
            deadline: None,
            limits: ActionLimits::default(),
            reply_route: None,
            payload: ActionPayload::Evolution {
                operation: EvolutionOperation::EvaluateHypothesis,
            },
        }
    }

    #[test]
    fn interleaved_sources_preserve_priority_and_fifo_across_restart() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("actions.sqlite");
        let session_id = SessionId::new();
        let config = ActionInboxConfig::default();
        let first_user = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "first user",
            10,
        );
        let second_user = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "second user",
            10,
        );
        let scheduled = action(
            &session_id,
            ActionPriority::Scheduled,
            DeliveryPolicy::Immediate,
            "scheduled",
            1,
        );
        let background = action(
            &session_id,
            ActionPriority::Background,
            DeliveryPolicy::WhenIdle,
            "background",
            0,
        );
        {
            let inbox = inbox(&path, config);
            inbox
                .submit(background.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .submit(scheduled.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .submit(first_user.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .submit(second_user.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
        }
        let inbox = inbox(&path, config);
        let context = PumpContext {
            active_action: None,
            at_turn_boundary: false,
            session_idle: true,
        };
        let mut selected = Vec::new();
        for _ in 0..4 {
            let next = inbox
                .select_next(&session_id, UtcTimestamp::UNIX_EPOCH, &context)
                .unwrap()
                .unwrap();
            selected.push(next.record.action.id.clone());
            inbox
                .mark_running(&next.record.action.id, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .complete(&next.record.action.id, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
        }
        assert_eq!(
            selected,
            vec![first_user.id, second_user.id, scheduled.id, background.id]
        );
    }

    #[test]
    fn steering_waits_for_a_turn_boundary_and_does_not_start_another_turn() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let inbox = PersistentActionInbox::new(store, ActionInboxConfig::default()).unwrap();
        let session_id = SessionId::new();
        let active_id = ActionId::new();
        let steering = action(
            &session_id,
            ActionPriority::Interrupt,
            DeliveryPolicy::NextTurnBoundary,
            "change course",
            0,
        );
        inbox
            .submit(steering.clone(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let before_boundary = PumpContext {
            active_action: Some(active_id.clone()),
            at_turn_boundary: false,
            session_idle: false,
        };
        assert!(
            inbox
                .select_next(&session_id, UtcTimestamp::UNIX_EPOCH, &before_boundary)
                .unwrap()
                .is_none()
        );
        let at_boundary = PumpContext {
            active_action: Some(active_id),
            at_turn_boundary: true,
            session_idle: false,
        };
        let selected = inbox
            .select_next(&session_id, UtcTimestamp::UNIX_EPOCH, &at_boundary)
            .unwrap()
            .unwrap();
        assert_eq!(selected.kind, AdmissionKind::ApplySteering);
        assert_eq!(selected.record.action.id, steering.id);
        assert_eq!(selected.record.state, ActionState::Admitted);
        inbox
            .complete(&steering.id, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
    }

    #[test]
    fn queue_bounds_reserve_capacity_from_background_work() {
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ActionInboxConfig {
                max_queued_per_session: 2,
                max_background_queued_per_session: 1,
            },
        )
        .unwrap();
        let session_id = SessionId::new();
        inbox
            .submit(
                action(
                    &session_id,
                    ActionPriority::Background,
                    DeliveryPolicy::WhenIdle,
                    "one",
                    0,
                ),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let second_background = inbox.submit(
            action(
                &session_id,
                ActionPriority::Background,
                DeliveryPolicy::WhenIdle,
                "two",
                1,
            ),
            UtcTimestamp::UNIX_EPOCH,
        );
        assert!(matches!(
            second_background,
            Err(ActionStoreError::BackgroundQueueFull)
        ));
        inbox
            .submit(
                action(
                    &session_id,
                    ActionPriority::User,
                    DeliveryPolicy::Immediate,
                    "user",
                    2,
                ),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
    }

    #[test]
    fn expiry_cancellation_and_terminal_states_are_durable() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("actions.sqlite");
        let session_id = SessionId::new();
        let mut expired = action(
            &session_id,
            ActionPriority::Scheduled,
            DeliveryPolicy::Immediate,
            "late",
            0,
        );
        expired.deadline = Some(UtcTimestamp::from_unix_millis(5));
        let cancelled = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "cancel me",
            0,
        );
        {
            let inbox = inbox(&path, ActionInboxConfig::default());
            let record = inbox
                .submit(expired.clone(), UtcTimestamp::from_unix_millis(5))
                .unwrap();
            assert_eq!(record.state, ActionState::Expired);
            inbox
                .submit(cancelled.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .cancel(
                    &cancelled.id,
                    UtcTimestamp::from_unix_millis(6),
                    "user request",
                )
                .unwrap();
        }
        let inbox = inbox(&path, ActionInboxConfig::default());
        assert_eq!(
            inbox.get(&expired.id).unwrap().unwrap().state,
            ActionState::Expired
        );
        let record = inbox.get(&cancelled.id).unwrap().unwrap();
        assert_eq!(record.state, ActionState::Cancelled);
        assert_eq!(record.terminal_detail.as_deref(), Some("user request"));
    }

    #[test]
    fn only_one_model_turn_can_run_per_session() {
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ActionInboxConfig::default(),
        )
        .unwrap();
        let session_id = SessionId::new();
        let first = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "first",
            0,
        );
        let second = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "second",
            1,
        );
        inbox
            .submit(first.clone(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        inbox
            .submit(second.clone(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let idle = PumpContext {
            session_idle: true,
            ..PumpContext::default()
        };
        inbox
            .select_next(&session_id, UtcTimestamp::UNIX_EPOCH, &idle)
            .unwrap();
        inbox
            .mark_running(&first.id, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        assert!(matches!(
            inbox.mark_running(&second.id, UtcTimestamp::UNIX_EPOCH),
            Err(ActionStoreError::TurnAlreadyRunning | ActionStoreError::IllegalTransition { .. })
        ));
    }

    #[test]
    fn evolution_requires_background_when_idle_and_typed_non_recursive_origin() {
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ActionInboxConfig::default(),
        )
        .unwrap();
        let session_id = SessionId::new();

        let mut foreground = evolution_action(&session_id);
        foreground.priority = ActionPriority::User;
        assert!(matches!(
            inbox.submit(foreground, UtcTimestamp::UNIX_EPOCH),
            Err(ActionStoreError::EvolutionScheduling)
        ));

        let mut recursive = evolution_action(&session_id);
        if let ActionSource::Evolution { ancestry, .. } = &mut recursive.source {
            ancestry.push(ActionAncestorKind::Evolution);
        }
        assert!(matches!(
            inbox.submit(recursive, UtcTimestamp::UNIX_EPOCH),
            Err(ActionStoreError::RecursiveEvolution)
        ));

        let mut child = evolution_action(&session_id);
        if let ActionSource::Evolution { execution, .. } = &mut child.source {
            *execution = EvolutionExecution::DedicatedChild;
        }
        assert!(matches!(
            inbox.submit(child, UtcTimestamp::UNIX_EPOCH),
            Err(ActionStoreError::DedicatedEvolutionChild)
        ));

        let accepted = evolution_action(&session_id);
        assert_eq!(
            inbox
                .submit(accepted, UtcTimestamp::UNIX_EPOCH)
                .unwrap()
                .state,
            ActionState::Queued
        );
    }

    #[test]
    fn user_channel_and_scheduled_work_precede_evolution() {
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ActionInboxConfig::default(),
        )
        .unwrap();
        let session_id = SessionId::new();
        let evolution = evolution_action(&session_id);
        let mut scheduled = action(
            &session_id,
            ActionPriority::Scheduled,
            DeliveryPolicy::Immediate,
            "scheduled",
            0,
        );
        scheduled.source = ActionSource::Schedule {
            job_id: JobId::new(),
            attempt: 1,
        };
        scheduled.payload = ActionPayload::Scheduled {
            instruction: "scheduled".into(),
        };
        let mut channel = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "channel",
            0,
        );
        channel.source = ActionSource::Channel {
            channel: "test".into(),
            message_id: "message".into(),
        };
        channel.payload = ActionPayload::ChannelMessage {
            text: "channel".into(),
            attachments: Vec::new(),
        };
        let user = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "user",
            0,
        );
        for candidate in [&evolution, &scheduled, &channel, &user] {
            inbox
                .submit(candidate.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
        }
        let context = PumpContext {
            session_idle: true,
            ..PumpContext::default()
        };
        let mut selected = Vec::new();
        for _ in 0..4 {
            let next = inbox
                .select_next(&session_id, UtcTimestamp::UNIX_EPOCH, &context)
                .unwrap()
                .unwrap();
            selected.push(next.record.action.id.clone());
            inbox
                .mark_running(&next.record.action.id, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .complete(&next.record.action.id, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
        }
        assert_eq!(
            selected,
            vec![channel.id, user.id, scheduled.id, evolution.id]
        );
    }
}
