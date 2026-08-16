#![forbid(unsafe_code)]

use std::fmt::Display;
use std::sync::{Arc, Mutex, MutexGuard};

use keith_action_store::{
    ActionLimits, ActionPayload, ActionPriority, ActionSource, DeliveryPolicy,
    PersistentActionInbox, ReplyRoute, SessionAction,
};
use keith_agent_types::{
    ActionId, CURRENT_SCHEMA_VERSION, ChildId, EntityId, JobId, ProfileId, Revision, SchemaVersion,
    SessionId, UtcTimestamp, WorkspaceId,
};
use keith_state_store_core::{VersionedRecord, WaitRepository, WritePrecondition};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "trigger")]
pub enum WakeTrigger {
    At {
        at: UtcTimestamp,
    },
    Schedule {
        job_id: JobId,
    },
    ChildTerminal {
        child_id: ChildId,
    },
    ProcessExit {
        process_id: EntityId,
    },
    FileChanged {
        workspace_id: WorkspaceId,
        pattern: String,
    },
    RepositoryChanged {
        workspace_id: WorkspaceId,
        reference: String,
    },
    ChannelMessage {
        route_id: EntityId,
        filter: Option<String>,
    },
    ExternalCondition {
        connector: String,
        query: serde_json::Value,
    },
    UserResponse {
        session_id: SessionId,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WakeEventKind {
    Time,
    Schedule {
        job_id: JobId,
    },
    ChildTerminal {
        child_id: ChildId,
    },
    ProcessExit {
        process_id: EntityId,
    },
    FileChanged {
        workspace_id: WorkspaceId,
        path: String,
    },
    RepositoryChanged {
        workspace_id: WorkspaceId,
        reference: String,
    },
    ChannelMessage {
        route_id: EntityId,
        text: String,
    },
    ExternalCondition {
        connector: String,
        query: serde_json::Value,
    },
    UserResponse {
        session_id: SessionId,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WakeEvent {
    pub id: EntityId,
    pub occurred_at: UtcTimestamp,
    pub kind: WakeEventKind,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WaitingState {
    Armed,
    Fired,
    Resumed,
    Cancelled,
    Expired,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WaitingItem {
    pub version: SchemaVersion,
    pub id: EntityId,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub trigger: WakeTrigger,
    pub wake_id: EntityId,
    pub action_id: ActionId,
    pub reply_route: Option<ReplyRoute>,
    pub state: WaitingState,
    pub expires_at: Option<UtcTimestamp>,
    pub fired_by: Option<EntityId>,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NewWaitingItem {
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub trigger: WakeTrigger,
    pub reply_route: Option<ReplyRoute>,
    pub expires_at: Option<UtcTimestamp>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ReleaseDirective {
    pub release_model_turn: bool,
    pub kernel_evictable: bool,
    pub worker_evictable: bool,
}

impl ReleaseDirective {
    pub const WAITING: Self = Self {
        release_model_turn: true,
        kernel_evictable: true,
        worker_evictable: true,
    };
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WakeEnqueueStatus {
    Accepted,
    AlreadyPresent,
}

pub trait WakeActionSink: Send + Sync {
    /// # Errors
    ///
    /// Returns a safe error when the persistent action inbox cannot accept the resume action.
    fn enqueue_wake(
        &self,
        action: SessionAction,
        now: UtcTimestamp,
    ) -> Result<WakeEnqueueStatus, String>;
}

impl<R> WakeActionSink for PersistentActionInbox<R>
where
    R: keith_state_store_core::ActionRepository,
    R::Error: Display,
{
    fn enqueue_wake(
        &self,
        action: SessionAction,
        now: UtcTimestamp,
    ) -> Result<WakeEnqueueStatus, String> {
        if self
            .get(&action.id)
            .map_err(|error| error.to_string())?
            .is_some()
        {
            return Ok(WakeEnqueueStatus::AlreadyPresent);
        }
        match self.submit(action.clone(), now) {
            Ok(_) => Ok(WakeEnqueueStatus::Accepted),
            Err(error) => {
                if self
                    .get(&action.id)
                    .map_err(|lookup| lookup.to_string())?
                    .is_some()
                {
                    Ok(WakeEnqueueStatus::AlreadyPresent)
                } else {
                    Err(error.to_string())
                }
            }
        }
    }
}

#[derive(Debug, Error)]
pub enum WaitingError {
    #[error("waiting repository failed: {0}")]
    Repository(String),
    #[error("waiting record is corrupt: {0}")]
    Corrupt(String),
    #[error("waiting trigger is invalid")]
    InvalidTrigger,
    #[error("waiting item was not found")]
    NotFound,
    #[error("waiting state transition is invalid")]
    InvalidTransition,
    #[error("waiting state lock was poisoned")]
    LockPoisoned,
    #[error("waiting revision overflowed")]
    RevisionOverflow,
}

struct StoredWaiting {
    item: WaitingItem,
    revision: Revision,
}

pub struct WaitingService<R, S> {
    repository: Arc<R>,
    sink: Arc<S>,
    serial: Mutex<()>,
}

impl<R, S> WaitingService<R, S>
where
    R: WaitRepository,
    R::Error: Display,
    S: WakeActionSink,
{
    pub fn new(repository: Arc<R>, sink: Arc<S>) -> Self {
        Self {
            repository,
            sink,
            serial: Mutex::new(()),
        }
    }

    /// Persists a trigger before returning the directive that releases active compute.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid triggers/expiry or persistence failure.
    pub fn register(
        &self,
        request: NewWaitingItem,
        now: UtcTimestamp,
    ) -> Result<(WaitingItem, ReleaseDirective), WaitingError> {
        validate_trigger(&request.trigger)?;
        if request.expires_at.is_some_and(|expiry| expiry <= now) {
            return Err(WaitingError::InvalidTrigger);
        }
        let item = WaitingItem {
            version: CURRENT_SCHEMA_VERSION,
            id: EntityId::new(),
            profile_id: request.profile_id,
            session_id: request.session_id,
            trigger: request.trigger,
            wake_id: EntityId::new(),
            action_id: ActionId::new(),
            reply_route: request.reply_route,
            state: WaitingState::Armed,
            expires_at: request.expires_at,
            fired_by: None,
            created_at: now,
            updated_at: now,
            safe_error: None,
        };
        self.repository
            .put_wait(
                wait_record(&item, Revision::ZERO)?,
                WritePrecondition::Missing,
            )
            .map_err(repository_error)?;
        Ok((item, ReleaseDirective::WAITING))
    }

    /// Matches one real event, deduplicates fired waits, and queues stable resume actions.
    ///
    /// # Errors
    ///
    /// Returns an error for corrupt records or repository failures.
    pub fn signal(&self, event: &WakeEvent) -> Result<Vec<WaitingItem>, WaitingError> {
        let _guard = self.lock()?;
        self.expire_due_locked(event.occurred_at)?;
        let waiting = self.load_all()?;
        let mut resumed = Vec::new();
        for mut stored in waiting.into_iter().filter(|stored| {
            stored.item.state == WaitingState::Armed && trigger_matches(&stored.item.trigger, event)
        }) {
            stored.item.state = WaitingState::Fired;
            stored.item.fired_by = Some(event.id.clone());
            stored.item.updated_at = event.occurred_at;
            if self.put(&mut stored).is_err() {
                continue;
            }
            resumed.push(self.enqueue_fired(stored, event.occurred_at)?);
        }
        Ok(resumed)
    }

    /// Reconciles fired-but-not-acknowledged waits after daemon or worker restart.
    ///
    /// # Errors
    ///
    /// Returns an error for corrupt records or persistence failure.
    pub fn recover(&self, now: UtcTimestamp) -> Result<Vec<WaitingItem>, WaitingError> {
        let _guard = self.lock()?;
        self.expire_due_locked(now)?;
        let mut resumed = Vec::new();
        for stored in self
            .load_all()?
            .into_iter()
            .filter(|stored| stored.item.state == WaitingState::Fired)
        {
            resumed.push(self.enqueue_fired(stored, now)?);
        }
        Ok(resumed)
    }

    /// Cancels an armed or fired wait and prevents later wake delivery.
    ///
    /// # Errors
    ///
    /// Returns an error for missing/terminal waits or persistence failure.
    pub fn cancel(&self, id: &EntityId, now: UtcTimestamp) -> Result<WaitingItem, WaitingError> {
        self.transition(id, WaitingState::Cancelled, now)
    }

    /// Expires every armed wait whose deadline has elapsed.
    ///
    /// # Errors
    ///
    /// Returns an error for corrupt records or persistence failure.
    pub fn expire_due(&self, now: UtcTimestamp) -> Result<Vec<WaitingItem>, WaitingError> {
        let _guard = self.lock()?;
        self.expire_due_locked(now)
    }

    /// Lists exact persistent waiting state for a session.
    ///
    /// # Errors
    ///
    /// Returns an error when records cannot be read or decoded.
    pub fn list_session(&self, session_id: &SessionId) -> Result<Vec<WaitingItem>, WaitingError> {
        let mut items = self
            .load_all()?
            .into_iter()
            .filter(|stored| &stored.item.session_id == session_id)
            .map(|stored| stored.item)
            .collect::<Vec<_>>();
        items.sort_by_key(|item| item.created_at);
        Ok(items)
    }

    fn enqueue_fired(
        &self,
        mut stored: StoredWaiting,
        now: UtcTimestamp,
    ) -> Result<WaitingItem, WaitingError> {
        let action = SessionAction {
            id: stored.item.action_id.clone(),
            session_id: stored.item.session_id.clone(),
            source: ActionSource::Waiting {
                wake_id: stored.item.wake_id.clone(),
            },
            delivery: DeliveryPolicy::Immediate,
            priority: ActionPriority::Scheduled,
            created_at: now,
            not_before: None,
            deadline: None,
            limits: ActionLimits::default(),
            reply_route: stored.item.reply_route.clone(),
            payload: ActionPayload::ResumeWaiting {
                waiting_id: stored.item.id.clone(),
            },
        };
        match self.sink.enqueue_wake(action, now) {
            Ok(WakeEnqueueStatus::Accepted | WakeEnqueueStatus::AlreadyPresent) => {
                stored.item.state = WaitingState::Resumed;
                stored.item.safe_error = None;
                stored.item.updated_at = now;
                self.put(&mut stored)?;
            }
            Err(error) => {
                stored.item.safe_error = Some(safe_error(&error));
                stored.item.updated_at = now;
                self.put(&mut stored)?;
            }
        }
        Ok(stored.item)
    }

    fn transition(
        &self,
        id: &EntityId,
        next: WaitingState,
        now: UtcTimestamp,
    ) -> Result<WaitingItem, WaitingError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        let allowed = matches!(
            (stored.item.state, next),
            (
                WaitingState::Armed | WaitingState::Fired,
                WaitingState::Cancelled
            )
        );
        if !allowed {
            return Err(WaitingError::InvalidTransition);
        }
        stored.item.state = next;
        stored.item.updated_at = now;
        self.put(&mut stored)?;
        Ok(stored.item)
    }

    fn expire_due_locked(&self, now: UtcTimestamp) -> Result<Vec<WaitingItem>, WaitingError> {
        let mut expired = Vec::new();
        for mut stored in self.load_all()?.into_iter().filter(|stored| {
            stored.item.state == WaitingState::Armed
                && stored.item.expires_at.is_some_and(|expiry| expiry <= now)
        }) {
            stored.item.state = WaitingState::Expired;
            stored.item.updated_at = now;
            self.put(&mut stored)?;
            expired.push(stored.item);
        }
        Ok(expired)
    }

    fn required(&self, id: &EntityId) -> Result<StoredWaiting, WaitingError> {
        self.repository
            .get_wait(id)
            .map_err(repository_error)?
            .map(decode_wait)
            .transpose()?
            .ok_or(WaitingError::NotFound)
    }

    fn load_all(&self) -> Result<Vec<StoredWaiting>, WaitingError> {
        self.repository
            .list_waits()
            .map_err(repository_error)?
            .into_iter()
            .map(decode_wait)
            .collect()
    }

    fn put(&self, stored: &mut StoredWaiting) -> Result<(), WaitingError> {
        let revision = stored
            .revision
            .checked_next()
            .ok_or(WaitingError::RevisionOverflow)?;
        self.repository
            .put_wait(
                wait_record(&stored.item, revision)?,
                WritePrecondition::Exact(stored.revision),
            )
            .map_err(repository_error)?;
        stored.revision = revision;
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, WaitingError> {
        self.serial.lock().map_err(|_| WaitingError::LockPoisoned)
    }
}

fn validate_trigger(trigger: &WakeTrigger) -> Result<(), WaitingError> {
    let valid = match trigger {
        WakeTrigger::At { .. }
        | WakeTrigger::Schedule { .. }
        | WakeTrigger::ChildTerminal { .. }
        | WakeTrigger::ProcessExit { .. }
        | WakeTrigger::UserResponse { .. } => true,
        WakeTrigger::FileChanged { pattern, .. } => {
            !pattern.trim().is_empty() && !pattern.contains("..")
        }
        WakeTrigger::RepositoryChanged { reference, .. } => !reference.trim().is_empty(),
        WakeTrigger::ChannelMessage { filter, .. } => filter
            .as_ref()
            .is_none_or(|filter| !filter.trim().is_empty()),
        WakeTrigger::ExternalCondition { connector, query } => {
            !connector.trim().is_empty() && !query.is_null()
        }
    };
    if valid {
        Ok(())
    } else {
        Err(WaitingError::InvalidTrigger)
    }
}

fn trigger_matches(trigger: &WakeTrigger, event: &WakeEvent) -> bool {
    match (trigger, &event.kind) {
        (WakeTrigger::At { at }, WakeEventKind::Time) => event.occurred_at >= *at,
        (WakeTrigger::Schedule { job_id }, WakeEventKind::Schedule { job_id: actual }) => {
            job_id == actual
        }
        (
            WakeTrigger::ChildTerminal { child_id },
            WakeEventKind::ChildTerminal { child_id: actual },
        ) => child_id == actual,
        (
            WakeTrigger::ProcessExit { process_id },
            WakeEventKind::ProcessExit { process_id: actual },
        ) => process_id == actual,
        (
            WakeTrigger::FileChanged {
                workspace_id,
                pattern,
            },
            WakeEventKind::FileChanged {
                workspace_id: actual,
                path,
            },
        ) => workspace_id == actual && wildcard_match(pattern, path),
        (
            WakeTrigger::RepositoryChanged {
                workspace_id,
                reference,
            },
            WakeEventKind::RepositoryChanged {
                workspace_id: actual,
                reference: actual_reference,
            },
        ) => workspace_id == actual && reference == actual_reference,
        (
            WakeTrigger::ChannelMessage { route_id, filter },
            WakeEventKind::ChannelMessage {
                route_id: actual,
                text,
            },
        ) => {
            route_id == actual
                && filter
                    .as_ref()
                    .is_none_or(|filter| text.to_lowercase().contains(&filter.to_lowercase()))
        }
        (
            WakeTrigger::ExternalCondition { connector, query },
            WakeEventKind::ExternalCondition {
                connector: actual,
                query: actual_query,
            },
        ) => connector == actual && query == actual_query,
        (
            WakeTrigger::UserResponse { session_id },
            WakeEventKind::UserResponse { session_id: actual },
        ) => session_id == actual,
        _ => false,
    }
}

fn wildcard_match(pattern: &str, value: &str) -> bool {
    let parts = pattern.split('*').collect::<Vec<_>>();
    if parts.len() == 1 {
        return pattern == value;
    }
    let mut offset = 0;
    for (index, part) in parts.iter().enumerate() {
        if part.is_empty() {
            continue;
        }
        let Some(found) = value[offset..].find(part) else {
            return false;
        };
        if index == 0 && !pattern.starts_with('*') && found != 0 {
            return false;
        }
        offset += found + part.len();
    }
    pattern.ends_with('*') || parts.last().is_some_and(|part| value.ends_with(part))
}

fn wait_record(item: &WaitingItem, revision: Revision) -> Result<VersionedRecord, WaitingError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: item.id.clone(),
        revision,
        updated_at: item.updated_at,
        payload: serde_json::to_value(item)
            .map_err(|error| WaitingError::Corrupt(error.to_string()))?,
    })
}

fn decode_wait(record: VersionedRecord) -> Result<StoredWaiting, WaitingError> {
    let item = serde_json::from_value::<WaitingItem>(record.payload)
        .map_err(|error| WaitingError::Corrupt(error.to_string()))?;
    if item.id != record.id || item.version.major != CURRENT_SCHEMA_VERSION.major {
        return Err(WaitingError::Corrupt(
            "waiting identity or schema mismatch".into(),
        ));
    }
    Ok(StoredWaiting {
        item,
        revision: record.revision,
    })
}

fn repository_error(error: impl Display) -> WaitingError {
    WaitingError::Repository(safe_error(&error.to_string()))
}

fn safe_error(error: &str) -> String {
    error.chars().take(512).collect()
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use keith_action_store::{ActionInboxConfig, ActionState};
    use keith_state_store::EmbeddedStore;
    use tempfile::tempdir;

    use super::*;

    type TestWaiting = WaitingService<EmbeddedStore, PersistentActionInbox<EmbeddedStore>>;

    fn service(path: &Path, queue_limit: usize) -> TestWaiting {
        let repository = Arc::new(EmbeddedStore::open(path, None).unwrap());
        let sink = Arc::new(
            PersistentActionInbox::new(
                EmbeddedStore::open(path, None).unwrap(),
                ActionInboxConfig {
                    max_queued_per_session: queue_limit,
                    max_background_queued_per_session: queue_limit,
                },
            )
            .unwrap(),
        );
        WaitingService::new(repository, sink)
    }

    fn register(waiting: &TestWaiting, session: &SessionId, trigger: WakeTrigger) -> WaitingItem {
        let (item, directive) = waiting
            .register(
                NewWaitingItem {
                    profile_id: ProfileId::new(),
                    session_id: session.clone(),
                    trigger,
                    reply_route: Some(ReplyRoute::Session {
                        session_id: session.clone(),
                    }),
                    expires_at: None,
                },
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert_eq!(directive, ReleaseDirective::WAITING);
        item
    }

    #[test]
    fn every_trigger_wakes_once_and_duplicate_events_do_not_duplicate_actions() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let waiting = service(&path, 64);
        let session = SessionId::new();
        let job = JobId::new();
        let child = ChildId::new();
        let process = EntityId::new();
        let workspace = WorkspaceId::new();
        let route = EntityId::new();
        let triggers = vec![
            (
                WakeTrigger::At {
                    at: UtcTimestamp::from_unix_millis(10),
                },
                WakeEventKind::Time,
            ),
            (
                WakeTrigger::Schedule {
                    job_id: job.clone(),
                },
                WakeEventKind::Schedule { job_id: job },
            ),
            (
                WakeTrigger::ChildTerminal {
                    child_id: child.clone(),
                },
                WakeEventKind::ChildTerminal { child_id: child },
            ),
            (
                WakeTrigger::ProcessExit {
                    process_id: process.clone(),
                },
                WakeEventKind::ProcessExit {
                    process_id: process,
                },
            ),
            (
                WakeTrigger::FileChanged {
                    workspace_id: workspace.clone(),
                    pattern: "src/*.rs".into(),
                },
                WakeEventKind::FileChanged {
                    workspace_id: workspace.clone(),
                    path: "src/lib.rs".into(),
                },
            ),
            (
                WakeTrigger::RepositoryChanged {
                    workspace_id: workspace.clone(),
                    reference: "refs/heads/main".into(),
                },
                WakeEventKind::RepositoryChanged {
                    workspace_id: workspace,
                    reference: "refs/heads/main".into(),
                },
            ),
            (
                WakeTrigger::ChannelMessage {
                    route_id: route.clone(),
                    filter: Some("approved".into()),
                },
                WakeEventKind::ChannelMessage {
                    route_id: route,
                    text: "Approved by user".into(),
                },
            ),
            (
                WakeTrigger::ExternalCondition {
                    connector: "build".into(),
                    query: serde_json::json!({"state": "green"}),
                },
                WakeEventKind::ExternalCondition {
                    connector: "build".into(),
                    query: serde_json::json!({"state": "green"}),
                },
            ),
            (
                WakeTrigger::UserResponse {
                    session_id: session.clone(),
                },
                WakeEventKind::UserResponse {
                    session_id: session.clone(),
                },
            ),
        ];
        let mut items = Vec::new();
        for (trigger, event) in triggers {
            let item = register(&waiting, &session, trigger);
            let wake = WakeEvent {
                id: EntityId::new(),
                occurred_at: UtcTimestamp::from_unix_millis(10),
                kind: event,
            };
            assert_eq!(waiting.signal(&wake).unwrap().len(), 1);
            assert!(waiting.signal(&wake).unwrap().is_empty());
            items.push(item);
        }
        let persisted = waiting.list_session(&session).unwrap();
        assert_eq!(persisted.len(), 9);
        assert!(
            persisted
                .iter()
                .all(|item| item.state == WaitingState::Resumed)
        );
        for item in items {
            let action = waiting.sink.get(&item.action_id).unwrap().unwrap();
            assert_eq!(action.state, ActionState::Queued);
            assert!(matches!(
                action.action.source,
                ActionSource::Waiting { wake_id } if wake_id == item.wake_id
            ));
        }
    }

    #[test]
    fn fired_wait_recovers_after_restart_and_cancelled_or_expired_waits_stay_terminal() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let waiting = service(&path, 1);
        let session = SessionId::new();
        let occupying = SessionAction {
            id: ActionId::new(),
            session_id: session.clone(),
            source: ActionSource::FollowUp,
            delivery: DeliveryPolicy::Immediate,
            priority: ActionPriority::User,
            created_at: UtcTimestamp::UNIX_EPOCH,
            not_before: None,
            deadline: None,
            limits: ActionLimits::default(),
            reply_route: None,
            payload: ActionPayload::FollowUp {
                text: "occupy queue".into(),
            },
        };
        waiting
            .sink
            .submit(occupying.clone(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let fired = register(
            &waiting,
            &session,
            WakeTrigger::UserResponse {
                session_id: session.clone(),
            },
        );
        let event = WakeEvent {
            id: EntityId::new(),
            occurred_at: UtcTimestamp::from_unix_millis(1),
            kind: WakeEventKind::UserResponse {
                session_id: session.clone(),
            },
        };
        let result = waiting.signal(&event).unwrap();
        assert_eq!(result[0].state, WaitingState::Fired);
        waiting
            .sink
            .cancel(
                &occupying.id,
                UtcTimestamp::from_unix_millis(2),
                "release queue",
            )
            .unwrap();
        drop(waiting);
        let restarted = service(&path, 8);
        let recovered = restarted
            .recover(UtcTimestamp::from_unix_millis(3))
            .unwrap();
        assert_eq!(recovered[0].id, fired.id);
        assert_eq!(recovered[0].action_id, fired.action_id);
        assert_eq!(recovered[0].state, WaitingState::Resumed);

        let cancelled = register(
            &restarted,
            &session,
            WakeTrigger::At {
                at: UtcTimestamp::from_unix_millis(10),
            },
        );
        restarted
            .cancel(&cancelled.id, UtcTimestamp::from_unix_millis(4))
            .unwrap();
        let (expiring, _) = restarted
            .register(
                NewWaitingItem {
                    profile_id: ProfileId::new(),
                    session_id: session.clone(),
                    trigger: WakeTrigger::At {
                        at: UtcTimestamp::from_unix_millis(20),
                    },
                    reply_route: None,
                    expires_at: Some(UtcTimestamp::from_unix_millis(15)),
                },
                UtcTimestamp::from_unix_millis(5),
            )
            .unwrap();
        restarted
            .expire_due(UtcTimestamp::from_unix_millis(15))
            .unwrap();
        let items = restarted.list_session(&session).unwrap();
        assert_eq!(
            items
                .iter()
                .find(|item| item.id == cancelled.id)
                .unwrap()
                .state,
            WaitingState::Cancelled
        );
        assert_eq!(
            items
                .iter()
                .find(|item| item.id == expiring.id)
                .unwrap()
                .state,
            WaitingState::Expired
        );
    }
}
