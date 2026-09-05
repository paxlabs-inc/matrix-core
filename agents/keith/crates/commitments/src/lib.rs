#![forbid(unsafe_code)]

use std::fmt::Display;
use std::sync::{Arc, Mutex, MutexGuard};

use keith_action_store::ReplyRoute;
use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, CommitmentId, EntityId, ProfileId, Revision, SchemaVersion, SessionId,
    UtcTimestamp,
};
use keith_state_store_core::{
    CommitmentRepository, VersionedRecord, WaitRepository, WritePrecondition,
};
use keith_waiting::{
    NewWaitingItem, ReleaseDirective, WaitingError, WaitingService, WaitingState, WakeActionSink,
    WakeTrigger,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CommitmentOwner {
    User,
    Agent,
    Shared,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CommitmentState {
    Captured,
    Scheduled,
    Active,
    Waiting,
    Fulfilled,
    Blocked,
    Cancelled,
    Expired,
}

impl CommitmentState {
    pub const fn is_terminal(self) -> bool {
        matches!(self, Self::Fulfilled | Self::Cancelled | Self::Expired)
    }

    const fn allows(self, next: Self) -> bool {
        match self {
            Self::Captured => matches!(
                next,
                Self::Scheduled | Self::Active | Self::Cancelled | Self::Expired
            ),
            Self::Scheduled => matches!(
                next,
                Self::Active
                    | Self::Waiting
                    | Self::Fulfilled
                    | Self::Blocked
                    | Self::Cancelled
                    | Self::Expired
            ),
            Self::Active => matches!(
                next,
                Self::Waiting | Self::Fulfilled | Self::Blocked | Self::Cancelled | Self::Expired
            ),
            Self::Waiting => matches!(
                next,
                Self::Active | Self::Fulfilled | Self::Blocked | Self::Cancelled | Self::Expired
            ),
            Self::Blocked => matches!(next, Self::Active | Self::Cancelled | Self::Expired),
            Self::Fulfilled | Self::Cancelled | Self::Expired => false,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Commitment {
    pub version: SchemaVersion,
    pub id: CommitmentId,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub description: String,
    pub owner: CommitmentOwner,
    pub trigger: Option<WakeTrigger>,
    pub state: CommitmentState,
    pub reply_route: Option<ReplyRoute>,
    pub expires_at: Option<UtcTimestamp>,
    pub waiting_id: Option<EntityId>,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub safe_detail: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NewCommitment {
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub description: String,
    pub owner: CommitmentOwner,
    pub trigger: Option<WakeTrigger>,
    pub reply_route: Option<ReplyRoute>,
    pub expires_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CommitmentUpdate {
    pub description: Option<String>,
    pub owner: Option<CommitmentOwner>,
    pub trigger: Option<Option<WakeTrigger>>,
    pub reply_route: Option<Option<ReplyRoute>>,
    pub expires_at: Option<Option<UtcTimestamp>>,
}

#[derive(Debug, Error)]
pub enum CommitmentError {
    #[error("commitment repository failed: {0}")]
    Repository(String),
    #[error("commitment record is corrupt: {0}")]
    Corrupt(String),
    #[error("commitment is invalid")]
    Invalid,
    #[error("commitment was not found")]
    NotFound,
    #[error("commitment transition is invalid")]
    InvalidTransition,
    #[error("commitment state lock was poisoned")]
    LockPoisoned,
    #[error("commitment revision overflowed")]
    RevisionOverflow,
    #[error("commitment waiting service failed: {0}")]
    Waiting(#[from] WaitingError),
}

struct StoredCommitment {
    commitment: Commitment,
    revision: Revision,
}

/// Exact canonical commitment state with its repository revision. A caller must
/// re-read this reference before depending on it after a context change.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CommitmentSnapshot {
    pub commitment: Commitment,
    pub revision: Revision,
}

pub struct CommitmentService<R, S> {
    repository: Arc<R>,
    waiting: Arc<WaitingService<R, S>>,
    serial: Mutex<()>,
}

impl<R, S> CommitmentService<R, S>
where
    R: CommitmentRepository + WaitRepository<Error = <R as CommitmentRepository>::Error>,
    <R as CommitmentRepository>::Error: Display,
    S: WakeActionSink,
{
    pub fn new(repository: Arc<R>, sink: Arc<S>) -> Self {
        Self {
            waiting: Arc::new(WaitingService::new(Arc::clone(&repository), sink)),
            repository,
            serial: Mutex::new(()),
        }
    }

    pub fn waiting_service(&self) -> &WaitingService<R, S> {
        &self.waiting
    }

    /// Captures a user or agent commitment with optional trigger, route, and expiry.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid descriptions/expiry or persistence failure.
    pub fn create(
        &self,
        request: NewCommitment,
        now: UtcTimestamp,
    ) -> Result<Commitment, CommitmentError> {
        validate_description(&request.description)?;
        if request.expires_at.is_some_and(|expiry| expiry <= now) {
            return Err(CommitmentError::Invalid);
        }
        let state = if request.trigger.is_some() {
            CommitmentState::Scheduled
        } else {
            CommitmentState::Captured
        };
        let commitment = Commitment {
            version: CURRENT_SCHEMA_VERSION,
            id: CommitmentId::new(),
            profile_id: request.profile_id,
            session_id: request.session_id,
            description: request.description,
            owner: request.owner,
            trigger: request.trigger,
            state,
            reply_route: request.reply_route,
            expires_at: request.expires_at,
            waiting_id: None,
            created_at: now,
            updated_at: now,
            safe_detail: None,
        };
        self.repository
            .put_commitment(
                commitment_record(&commitment, Revision::ZERO)?,
                WritePrecondition::Missing,
            )
            .map_err(repository_error)?;
        Ok(commitment)
    }

    /// Persists the wake trigger before returning a directive that releases active compute.
    ///
    /// # Errors
    ///
    /// Returns an error for missing triggers, invalid states, or persistence failure.
    pub fn begin_waiting(
        &self,
        id: &CommitmentId,
        now: UtcTimestamp,
    ) -> Result<(Commitment, ReleaseDirective), CommitmentError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        if !matches!(
            stored.commitment.state,
            CommitmentState::Scheduled | CommitmentState::Active
        ) {
            return Err(CommitmentError::InvalidTransition);
        }
        let trigger = stored
            .commitment
            .trigger
            .clone()
            .ok_or(CommitmentError::Invalid)?;
        let (waiting, directive) = self.waiting.register(
            NewWaitingItem {
                profile_id: stored.commitment.profile_id.clone(),
                session_id: stored.commitment.session_id.clone(),
                trigger,
                reply_route: stored.commitment.reply_route.clone(),
                expires_at: stored.commitment.expires_at,
            },
            now,
        )?;
        stored.commitment.waiting_id = Some(waiting.id.clone());
        stored.commitment.state = CommitmentState::Waiting;
        stored.commitment.updated_at = now;
        if let Err(error) = self.put(&mut stored) {
            let _ = self.waiting.cancel(&waiting.id, now);
            return Err(error);
        }
        Ok((stored.commitment, directive))
    }

    /// Updates nonterminal commitment fields while preserving an armed wait's trigger.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid fields, waiting/terminal mutation, or persistence failure.
    pub fn update(
        &self,
        id: &CommitmentId,
        update: CommitmentUpdate,
        now: UtcTimestamp,
    ) -> Result<Commitment, CommitmentError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        if stored.commitment.state.is_terminal() {
            return Err(CommitmentError::InvalidTransition);
        }
        if let Some(description) = update.description {
            validate_description(&description)?;
            stored.commitment.description = description;
        }
        if let Some(owner) = update.owner {
            stored.commitment.owner = owner;
        }
        if let Some(trigger) = update.trigger {
            if stored.commitment.state == CommitmentState::Waiting {
                return Err(CommitmentError::InvalidTransition);
            }
            stored.commitment.trigger = trigger;
            stored.commitment.state = if stored.commitment.trigger.is_some() {
                CommitmentState::Scheduled
            } else {
                CommitmentState::Captured
            };
        }
        if let Some(reply_route) = update.reply_route {
            stored.commitment.reply_route = reply_route;
        }
        if let Some(expiry) = update.expires_at {
            if expiry.is_some_and(|expiry| expiry <= now) {
                return Err(CommitmentError::Invalid);
            }
            stored.commitment.expires_at = expiry;
        }
        stored.commitment.updated_at = now;
        self.put(&mut stored)?;
        Ok(stored.commitment)
    }

    /// Activates captured, scheduled, blocked, or resumed waiting work.
    ///
    /// # Errors
    ///
    /// Returns an error for terminal/invalid states or persistence failure.
    pub fn activate(
        &self,
        id: &CommitmentId,
        now: UtcTimestamp,
    ) -> Result<Commitment, CommitmentError> {
        self.transition(id, CommitmentState::Active, None, now)
    }

    /// Marks the commitment fulfilled and cancels any still-armed wait.
    ///
    /// # Errors
    ///
    /// Returns an error for terminal/invalid states or persistence failure.
    pub fn fulfill(
        &self,
        id: &CommitmentId,
        detail: Option<String>,
        now: UtcTimestamp,
    ) -> Result<Commitment, CommitmentError> {
        self.transition(id, CommitmentState::Fulfilled, detail, now)
    }

    /// Marks a commitment blocked with a bounded operator-visible reason.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid states/reasons or persistence failure.
    pub fn block(
        &self,
        id: &CommitmentId,
        detail: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<Commitment, CommitmentError> {
        let detail = detail.into();
        if detail.trim().is_empty() {
            return Err(CommitmentError::Invalid);
        }
        self.transition(id, CommitmentState::Blocked, Some(detail), now)
    }

    /// Cancels a commitment and any corresponding nonterminal wait.
    ///
    /// # Errors
    ///
    /// Returns an error for terminal states or persistence failure.
    pub fn cancel(
        &self,
        id: &CommitmentId,
        now: UtcTimestamp,
    ) -> Result<Commitment, CommitmentError> {
        self.transition(id, CommitmentState::Cancelled, None, now)
    }

    /// Expires due commitments and cancels their still-armed waits.
    ///
    /// # Errors
    ///
    /// Returns an error for corrupt records or persistence failure.
    pub fn expire_due(&self, now: UtcTimestamp) -> Result<Vec<Commitment>, CommitmentError> {
        let ids = self
            .load_all()?
            .into_iter()
            .filter(|stored| {
                !stored.commitment.state.is_terminal()
                    && stored
                        .commitment
                        .expires_at
                        .is_some_and(|expiry| expiry <= now)
            })
            .map(|stored| stored.commitment.id)
            .collect::<Vec<_>>();
        ids.into_iter()
            .map(|id| self.transition(&id, CommitmentState::Expired, None, now))
            .collect()
    }

    /// Lists commitments for one profile with stable creation order.
    ///
    /// # Errors
    ///
    /// Returns an error when records cannot be read or decoded.
    pub fn list_profile(&self, profile_id: &ProfileId) -> Result<Vec<Commitment>, CommitmentError> {
        let mut commitments = self
            .load_all()?
            .into_iter()
            .filter(|stored| &stored.commitment.profile_id == profile_id)
            .map(|stored| stored.commitment)
            .collect::<Vec<_>>();
        commitments.sort_by_key(|commitment| commitment.created_at);
        Ok(commitments)
    }

    /// Returns one exact commitment projection.
    ///
    /// # Errors
    ///
    /// Returns an error for missing or corrupt records.
    pub fn inspect(&self, id: &CommitmentId) -> Result<Commitment, CommitmentError> {
        Ok(self.required(id)?.commitment)
    }

    /// Resolves one commitment by exact identity within the caller's profile.
    /// Missing and other-profile identities both return `None`; no optional
    /// retrieval ranking or global list participates in this lookup.
    ///
    /// # Errors
    ///
    /// Returns an error when the canonical record cannot be read or decoded.
    pub fn inspect_scoped(
        &self,
        profile_id: &ProfileId,
        id: &CommitmentId,
    ) -> Result<Option<CommitmentSnapshot>, CommitmentError> {
        let Some(record) = self
            .repository
            .get_commitment(id.as_entity_id())
            .map_err(repository_error)?
        else {
            return Ok(None);
        };
        let stored = decode_commitment(record)?;
        if &stored.commitment.profile_id != profile_id {
            return Ok(None);
        }
        Ok(Some(CommitmentSnapshot {
            commitment: stored.commitment,
            revision: stored.revision,
        }))
    }

    fn transition(
        &self,
        id: &CommitmentId,
        next: CommitmentState,
        detail: Option<String>,
        now: UtcTimestamp,
    ) -> Result<Commitment, CommitmentError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        if !stored.commitment.state.allows(next) {
            return Err(CommitmentError::InvalidTransition);
        }
        if let Some(waiting_id) = &stored.commitment.waiting_id {
            let waits = self.waiting.list_session(&stored.commitment.session_id)?;
            if waits.iter().any(|wait| {
                &wait.id == waiting_id
                    && matches!(wait.state, WaitingState::Armed | WaitingState::Fired)
            }) {
                let _ = self.waiting.cancel(waiting_id, now);
            }
        }
        stored.commitment.state = next;
        stored.commitment.safe_detail = detail.map(|detail| detail.chars().take(512).collect());
        stored.commitment.updated_at = now;
        self.put(&mut stored)?;
        Ok(stored.commitment)
    }

    fn required(&self, id: &CommitmentId) -> Result<StoredCommitment, CommitmentError> {
        self.repository
            .get_commitment(id.as_entity_id())
            .map_err(repository_error)?
            .map(decode_commitment)
            .transpose()?
            .ok_or(CommitmentError::NotFound)
    }

    fn load_all(&self) -> Result<Vec<StoredCommitment>, CommitmentError> {
        self.repository
            .list_commitments()
            .map_err(repository_error)?
            .into_iter()
            .map(decode_commitment)
            .collect()
    }

    fn put(&self, stored: &mut StoredCommitment) -> Result<(), CommitmentError> {
        let revision = stored
            .revision
            .checked_next()
            .ok_or(CommitmentError::RevisionOverflow)?;
        self.repository
            .put_commitment(
                commitment_record(&stored.commitment, revision)?,
                WritePrecondition::Exact(stored.revision),
            )
            .map_err(repository_error)?;
        stored.revision = revision;
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, CommitmentError> {
        self.serial
            .lock()
            .map_err(|_| CommitmentError::LockPoisoned)
    }
}

fn validate_description(description: &str) -> Result<(), CommitmentError> {
    if description.trim().is_empty() || description.len() > 16 * 1_024 {
        Err(CommitmentError::Invalid)
    } else {
        Ok(())
    }
}

fn commitment_record(
    commitment: &Commitment,
    revision: Revision,
) -> Result<VersionedRecord, CommitmentError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: commitment.id.as_entity_id().clone(),
        revision,
        updated_at: commitment.updated_at,
        payload: serde_json::to_value(commitment)
            .map_err(|error| CommitmentError::Corrupt(error.to_string()))?,
    })
}

fn decode_commitment(record: VersionedRecord) -> Result<StoredCommitment, CommitmentError> {
    let commitment = serde_json::from_value::<Commitment>(record.payload)
        .map_err(|error| CommitmentError::Corrupt(error.to_string()))?;
    if commitment.id.as_entity_id() != &record.id
        || commitment.version.major != CURRENT_SCHEMA_VERSION.major
    {
        return Err(CommitmentError::Corrupt(
            "commitment identity or schema mismatch".into(),
        ));
    }
    Ok(StoredCommitment {
        commitment,
        revision: record.revision,
    })
}

fn repository_error(error: impl Display) -> CommitmentError {
    CommitmentError::Repository(error.to_string().chars().take(512).collect())
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use keith_action_store::{ActionInboxConfig, PersistentActionInbox};
    use keith_state_store::EmbeddedStore;
    use keith_waiting::{WakeEvent, WakeEventKind};
    use tempfile::tempdir;

    use super::*;

    type TestCommitments = CommitmentService<EmbeddedStore, PersistentActionInbox<EmbeddedStore>>;

    fn service(path: &Path) -> TestCommitments {
        let repository = Arc::new(EmbeddedStore::open(path, None).unwrap());
        let sink = Arc::new(
            PersistentActionInbox::new(
                EmbeddedStore::open(path, None).unwrap(),
                ActionInboxConfig::default(),
            )
            .unwrap(),
        );
        CommitmentService::new(repository, sink)
    }

    #[test]
    fn exact_scoped_revision_tracks_changes_and_restart_without_leaking_other_profiles() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let commitments = service(&path);
        let profile = ProfileId::new();
        let commitment = commitments
            .create(
                NewCommitment {
                    profile_id: profile.clone(),
                    session_id: SessionId::new(),
                    description: "Inspect the current endpoint before delivery".into(),
                    owner: CommitmentOwner::Agent,
                    trigger: None,
                    reply_route: None,
                    expires_at: None,
                },
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let original = commitments
            .inspect_scoped(&profile, &commitment.id)
            .unwrap()
            .unwrap();
        assert_eq!(original.revision, Revision::ZERO);
        assert!(
            commitments
                .inspect_scoped(&ProfileId::new(), &commitment.id)
                .unwrap()
                .is_none()
        );
        assert!(
            commitments
                .inspect_scoped(&profile, &CommitmentId::new())
                .unwrap()
                .is_none()
        );
        commitments
            .activate(&commitment.id, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        drop(commitments);
        let reopened = service(&path);
        let current = reopened
            .inspect_scoped(&profile, &commitment.id)
            .unwrap()
            .unwrap();
        assert_eq!(current.revision, Revision::new(1));
        assert_eq!(current.commitment.state, CommitmentState::Active);
        assert_eq!(current.commitment.id, original.commitment.id);
        assert_ne!(current, original);
    }

    #[test]
    fn trigger_is_armed_before_yield_then_wakes_resumes_and_survives_restart() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let commitments = service(&path);
        let profile = ProfileId::new();
        let session = SessionId::new();
        let commitment = commitments
            .create(
                NewCommitment {
                    profile_id: profile.clone(),
                    session_id: session.clone(),
                    description: "Reply after approval".into(),
                    owner: CommitmentOwner::Agent,
                    trigger: Some(WakeTrigger::UserResponse {
                        session_id: session.clone(),
                    }),
                    reply_route: Some(ReplyRoute::Session {
                        session_id: session.clone(),
                    }),
                    expires_at: None,
                },
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert_eq!(commitment.state, CommitmentState::Scheduled);
        let (waiting, directive) = commitments
            .begin_waiting(&commitment.id, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        assert_eq!(directive, ReleaseDirective::WAITING);
        let waiting_id = waiting.waiting_id.clone().unwrap();
        assert_eq!(
            commitments
                .waiting_service()
                .list_session(&session)
                .unwrap()[0]
                .id,
            waiting_id
        );
        commitments
            .waiting_service()
            .signal(&WakeEvent {
                id: EntityId::new(),
                occurred_at: UtcTimestamp::from_unix_millis(2),
                kind: WakeEventKind::UserResponse {
                    session_id: session.clone(),
                },
            })
            .unwrap();
        let active = commitments
            .activate(&commitment.id, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        assert_eq!(active.state, CommitmentState::Active);
        let fulfilled = commitments
            .fulfill(
                &commitment.id,
                Some("approval handled".into()),
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        assert_eq!(fulfilled.state, CommitmentState::Fulfilled);
        drop(commitments);
        let restarted = service(&path);
        let loaded = restarted.list_profile(&profile).unwrap();
        assert_eq!(loaded.len(), 1);
        assert_eq!(loaded[0].state, CommitmentState::Fulfilled);
        assert_eq!(loaded[0].safe_detail.as_deref(), Some("approval handled"));
    }

    #[test]
    fn edit_block_cancel_and_expiry_controls_are_validated() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let commitments = service(&path);
        let profile = ProfileId::new();
        let captured = commitments
            .create(
                NewCommitment {
                    profile_id: profile.clone(),
                    session_id: SessionId::new(),
                    description: "Finish the report".into(),
                    owner: CommitmentOwner::Shared,
                    trigger: None,
                    reply_route: None,
                    expires_at: None,
                },
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let updated = commitments
            .update(
                &captured.id,
                CommitmentUpdate {
                    description: Some("Finish and review the report".into()),
                    owner: None,
                    trigger: None,
                    reply_route: None,
                    expires_at: None,
                },
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert!(updated.description.contains("review"));
        commitments
            .activate(&captured.id, UtcTimestamp::from_unix_millis(2))
            .unwrap();
        commitments
            .block(
                &captured.id,
                "awaiting source data",
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        commitments
            .activate(&captured.id, UtcTimestamp::from_unix_millis(4))
            .unwrap();
        commitments
            .cancel(&captured.id, UtcTimestamp::from_unix_millis(5))
            .unwrap();

        let expiring = commitments
            .create(
                NewCommitment {
                    profile_id: profile.clone(),
                    session_id: SessionId::new(),
                    description: "Temporary follow-up".into(),
                    owner: CommitmentOwner::User,
                    trigger: None,
                    reply_route: None,
                    expires_at: Some(UtcTimestamp::from_unix_millis(10)),
                },
                UtcTimestamp::from_unix_millis(6),
            )
            .unwrap();
        commitments
            .expire_due(UtcTimestamp::from_unix_millis(10))
            .unwrap();
        assert_eq!(
            commitments.inspect(&expiring.id).unwrap().state,
            CommitmentState::Expired
        );
        let listed = commitments.list_profile(&profile).unwrap();
        assert_eq!(listed.len(), 2);
        assert_eq!(listed[0].state, CommitmentState::Cancelled);
        assert_eq!(listed[1].state, CommitmentState::Expired);
    }
}
