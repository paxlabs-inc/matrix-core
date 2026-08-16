use std::collections::{BTreeMap, BTreeSet};
use std::sync::{Mutex, MutexGuard};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, Revision, UtcTimestamp, canonical_json_bytes,
};
use keith_state_store_core::{
    AtomicStateRepository, ClassifiedRepositoryError, RecordMutation, ResourceRepository,
    VersionedRecord, WritePrecondition,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{
    AcquireRequest, ExhaustionBehavior, ReclaimedResource, ResourceError, ResourceKind,
    ResourceLease, ResourcePolicy, ResourceProjection, ResourceScope, ScheduleOutcome, ScopePath,
    UsageDelta, UsageOutcome, WorkPriority,
};

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Account {
    id: EntityId,
    scope: ResourceScope,
    resource: ResourceKind,
    consumed: u64,
    active: u64,
    revision: Revision,
    updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "record", content = "value")]
enum StoredResource {
    Account(Account),
    Pending(Pending),
    Lease(ResourceLease),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Pending {
    request: AcquireRequest,
    revision: Revision,
}

type AccountChange = ((ResourceScope, ResourceKind), Account);
type AccountBatch = (Vec<AccountChange>, Vec<RecordMutation>);
const MAX_CONFLICT_RETRIES: usize = 8;

#[derive(Default)]
struct State {
    accounts: BTreeMap<(ResourceScope, ResourceKind), Account>,
    pending: BTreeMap<EntityId, Pending>,
    leases: BTreeMap<EntityId, ResourceLease>,
    last_tree: BTreeMap<WorkPriority, keith_agent_types::RootTreeId>,
}

pub struct ResourceGovernor<R> {
    repository: R,
    policy: ResourcePolicy,
    state: Mutex<State>,
}

impl<R> ResourceGovernor<R>
where
    R: AtomicStateRepository + ResourceRepository<Error = <R as AtomicStateRepository>::Error>,
{
    /// Restores all durable accounts, pending work, and reclaimable leases.
    ///
    /// # Errors
    ///
    /// Returns an error when durable records are corrupt or unavailable.
    pub fn open(repository: R, policy: ResourcePolicy) -> Result<Self, ResourceError> {
        let state = load_state(&repository)?;
        Ok(Self {
            repository,
            policy,
            state: Mutex::new(state),
        })
    }

    /// Adds ordered work to the durable fair scheduler.
    ///
    /// # Errors
    ///
    /// Returns an error for duplicate IDs, invalid paths/resources, or persistence failure.
    pub fn submit(&self, request: AcquireRequest) -> Result<(), ResourceError> {
        validate_acquire(&request)?;
        let mut state = self.lock()?;
        self.refresh_durable_state(&mut state)?;
        if state.pending.contains_key(&request.id)
            || state
                .leases
                .values()
                .any(|lease| lease.request.id == request.id)
        {
            return Err(ResourceError::Duplicate(request.id));
        }
        ensure_queue_capacity(&state, &self.policy, &request)?;
        let pending = Pending {
            request: request.clone(),
            revision: Revision::ZERO,
        };
        self.repository
            .transact(&[put_mutation(
                request.id.clone(),
                StoredResource::Pending(pending.clone()),
                Revision::ZERO,
                request.submitted_at,
                WritePrecondition::Missing,
            )?])
            .map_err(|error| classified_repository_error(&error))?;
        state.pending.insert(request.id.clone(), pending);
        Ok(())
    }

    /// Grants work by priority, FIFO within each session, and round-robin across trees.
    ///
    /// # Errors
    ///
    /// Returns an error when accounting cannot be committed atomically.
    pub fn schedule(
        &self,
        now: UtcTimestamp,
        max_decisions: usize,
    ) -> Result<Vec<ScheduleOutcome>, ResourceError> {
        if max_decisions == 0 {
            return Err(ResourceError::Invalid(
                "schedule decision bound must be non-zero".into(),
            ));
        }
        let mut state = self.lock()?;
        self.refresh_durable_state(&mut state)?;
        let mut outcomes = Vec::new();
        let mut attempted = BTreeSet::new();
        while outcomes.len() < max_decisions {
            let Some(request_id) = select_next(&state, &attempted) else {
                break;
            };
            attempted.insert(request_id.clone());
            let pending = state
                .pending
                .get(&request_id)
                .cloned()
                .ok_or_else(|| ResourceError::Missing(request_id.clone()))?;
            if let Some((scope, behavior)) = exhausted(
                &state,
                &self.policy,
                &pending.request.path,
                pending.request.resource,
                pending.request.units,
                true,
            )? {
                let outcome = match behavior {
                    ExhaustionBehavior::Pause => ScheduleOutcome::Paused {
                        request_id,
                        scope,
                        resource: pending.request.resource,
                    },
                    ExhaustionBehavior::Fail => {
                        self.delete_pending(&mut state, &pending)?;
                        ScheduleOutcome::Failed {
                            request_id,
                            scope,
                            resource: pending.request.resource,
                        }
                    }
                };
                outcomes.push(outcome);
                continue;
            }
            let lease = self.grant(&mut state, &pending, now)?;
            if let Some(tree) = pending.request.path.tree() {
                state
                    .last_tree
                    .insert(pending.request.priority, tree.clone());
            }
            outcomes.push(ScheduleOutcome::Granted(lease));
        }
        Ok(outcomes)
    }

    /// Grants or rejects one specific pending request without consuming work submitted by another
    /// scheduler process.
    ///
    /// # Errors
    ///
    /// Returns an error when the request is missing or accounting cannot be committed atomically.
    pub fn schedule_request(
        &self,
        request_id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<ScheduleOutcome, ResourceError> {
        let mut state = self.lock()?;
        for attempt in 0..=MAX_CONFLICT_RETRIES {
            self.refresh_durable_state(&mut state)?;
            let pending = state
                .pending
                .get(request_id)
                .cloned()
                .ok_or_else(|| ResourceError::Missing(request_id.clone()))?;
            if let Some((scope, behavior)) = exhausted(
                &state,
                &self.policy,
                &pending.request.path,
                pending.request.resource,
                pending.request.units,
                true,
            )? {
                return match behavior {
                    ExhaustionBehavior::Pause => Ok(ScheduleOutcome::Paused {
                        request_id: request_id.clone(),
                        scope,
                        resource: pending.request.resource,
                    }),
                    ExhaustionBehavior::Fail => match self.delete_pending(&mut state, &pending) {
                        Ok(()) => Ok(ScheduleOutcome::Failed {
                            request_id: request_id.clone(),
                            scope,
                            resource: pending.request.resource,
                        }),
                        Err(ResourceError::RepositoryConflict(_))
                            if attempt < MAX_CONFLICT_RETRIES =>
                        {
                            continue;
                        }
                        Err(error) => Err(error),
                    },
                };
            }
            match self.grant(&mut state, &pending, now) {
                Ok(lease) => return Ok(ScheduleOutcome::Granted(lease)),
                Err(ResourceError::RepositoryConflict(_)) if attempt < MAX_CONFLICT_RETRIES => {}
                Err(error) => return Err(error),
            }
        }
        Err(ResourceError::Invalid(
            "resource admission conflict retry bound was exhausted".into(),
        ))
    }

    /// Releases an active lease and makes capacity available to other trees.
    ///
    /// # Errors
    ///
    /// Returns an error for missing leases or persistence failure.
    pub fn release(&self, lease_id: &EntityId, now: UtcTimestamp) -> Result<(), ResourceError> {
        let mut state = self.lock()?;
        for attempt in 0..=MAX_CONFLICT_RETRIES {
            self.refresh_durable_state(&mut state)?;
            let lease = state
                .leases
                .get(lease_id)
                .cloned()
                .ok_or_else(|| ResourceError::Missing(lease_id.clone()))?;
            match self.release_locked(&mut state, &lease, now) {
                Ok(()) => return Ok(()),
                Err(ResourceError::RepositoryConflict(_)) if attempt < MAX_CONFLICT_RETRIES => {}
                Err(error) => return Err(error),
            }
        }
        Err(ResourceError::Invalid(
            "resource release conflict retry bound was exhausted".into(),
        ))
    }

    /// Refreshes a reclaimable lease without changing its accounting.
    ///
    /// # Errors
    ///
    /// Returns an error for missing leases, invalid time, or persistence failure.
    pub fn heartbeat(
        &self,
        lease_id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<ResourceLease, ResourceError> {
        let mut state = self.lock()?;
        for attempt in 0..=MAX_CONFLICT_RETRIES {
            self.refresh_durable_state(&mut state)?;
            let lease = state
                .leases
                .get(lease_id)
                .cloned()
                .ok_or_else(|| ResourceError::Missing(lease_id.clone()))?;
            if now < lease.heartbeat_at {
                return Err(ResourceError::Invalid(
                    "lease heartbeat cannot move backwards".into(),
                ));
            }
            let mut updated = lease.clone();
            updated.heartbeat_at = now;
            updated.expires_at = checked_timestamp(now, updated.request.idle_timeout_ms)?;
            updated.revision = updated
                .revision
                .checked_next()
                .ok_or_else(|| ResourceError::Invalid("lease revision exhausted".into()))?;
            match self.repository.transact(&[put_mutation(
                updated.id.clone(),
                StoredResource::Lease(updated.clone()),
                updated.revision,
                now,
                WritePrecondition::Exact(lease.revision),
            )?]) {
                Ok(_) => {
                    state.leases.insert(updated.id.clone(), updated.clone());
                    return Ok(updated);
                }
                Err(error) if error.is_conflict() && attempt < MAX_CONFLICT_RETRIES => {}
                Err(error) => return Err(repository_error(error)),
            }
        }
        Err(ResourceError::Invalid(
            "resource heartbeat conflict retry bound was exhausted".into(),
        ))
    }

    /// Reclaims each expired class independently while returning its durable recovery pointer.
    ///
    /// # Errors
    ///
    /// Returns an error when durable lease/account updates fail.
    pub fn reclaim_idle(&self, now: UtcTimestamp) -> Result<Vec<ReclaimedResource>, ResourceError> {
        let mut state = self.lock()?;
        let expired = state
            .leases
            .values()
            .filter(|lease| lease.expires_at <= now && lease.request.recovery.is_some())
            .cloned()
            .collect::<Vec<_>>();
        let mut reclaimed = Vec::new();
        for lease in expired {
            let recovery =
                lease.request.recovery.clone().ok_or_else(|| {
                    ResourceError::Invalid("recovery descriptor disappeared".into())
                })?;
            self.release_locked(&mut state, &lease, now)?;
            reclaimed.push(ReclaimedResource {
                lease_id: lease.id,
                recovery,
            });
        }
        reclaimed.sort_by(|left, right| left.lease_id.cmp(&right.lease_id));
        Ok(reclaimed)
    }

    /// Atomically records measurable goal/profile usage without crossing a ceiling.
    ///
    /// # Errors
    ///
    /// Returns an error for concurrency resources, zero deltas, or persistence failure.
    pub fn record_usage(
        &self,
        delta: &UsageDelta,
        now: UtcTimestamp,
    ) -> Result<UsageOutcome, ResourceError> {
        if delta.resource.is_concurrency() || delta.units == 0 {
            return Err(ResourceError::Invalid(
                "usage accounting requires a non-zero measurable resource".into(),
            ));
        }
        let mut state = self.lock()?;
        for attempt in 0..=MAX_CONFLICT_RETRIES {
            self.refresh_durable_state(&mut state)?;
            if let Some((scope, behavior)) = exhausted(
                &state,
                &self.policy,
                &delta.path,
                delta.resource,
                delta.units,
                false,
            )? {
                return Ok(match behavior {
                    ExhaustionBehavior::Pause => UsageOutcome::Paused {
                        scope,
                        resource: delta.resource,
                    },
                    ExhaustionBehavior::Fail => UsageOutcome::Failed {
                        scope,
                        resource: delta.resource,
                    },
                });
            }
            let (updates, mutations) =
                account_updates(&state, &delta.path, delta.resource, delta.units, false, now)?;
            match self.repository.transact(&mutations) {
                Ok(_) => {
                    apply_accounts(&mut state, updates);
                    return Ok(UsageOutcome::Recorded);
                }
                Err(error) if error.is_conflict() && attempt < MAX_CONFLICT_RETRIES => {}
                Err(error) => return Err(repository_error(error)),
            }
        }
        Err(ResourceError::Invalid(
            "resource accounting conflict retry bound was exhausted".into(),
        ))
    }

    /// Returns redacted, authoritative accounting projections.
    ///
    /// # Errors
    ///
    /// Returns an error when the governor lock is poisoned.
    pub fn projections(&self) -> Result<Vec<ResourceProjection>, ResourceError> {
        let state = self.lock()?;
        let mut projections = state
            .accounts
            .values()
            .map(|account| {
                let ceiling = self.policy.ceiling(&account.scope, account.resource);
                let used = if account.resource.is_concurrency() {
                    account.active
                } else {
                    account.consumed
                };
                ResourceProjection {
                    scope: account.scope.safe_label(),
                    resource: account.resource,
                    consumed: account.consumed,
                    active: account.active,
                    ceiling: ceiling.map(|value| value.maximum),
                    remaining: ceiling.map(|value| value.maximum.saturating_sub(used)),
                }
            })
            .collect::<Vec<_>>();
        projections.sort_by(|left, right| {
            left.scope
                .cmp(&right.scope)
                .then(left.resource.cmp(&right.resource))
        });
        Ok(projections)
    }

    /// # Errors
    ///
    /// Returns an error when the governor lock is poisoned.
    pub fn pending_count(&self) -> Result<usize, ResourceError> {
        Ok(self.lock()?.pending.len())
    }

    /// # Errors
    ///
    /// Returns an error when the governor lock is poisoned.
    pub fn lease_count(&self) -> Result<usize, ResourceError> {
        Ok(self.lock()?.leases.len())
    }

    fn grant(
        &self,
        state: &mut State,
        pending: &Pending,
        now: UtcTimestamp,
    ) -> Result<ResourceLease, ResourceError> {
        let lease = ResourceLease {
            id: EntityId::new(),
            request: pending.request.clone(),
            acquired_at: now,
            heartbeat_at: now,
            expires_at: checked_timestamp(now, pending.request.idle_timeout_ms)?,
            revision: Revision::ZERO,
        };
        let (updates, mut mutations) = account_updates(
            state,
            &pending.request.path,
            pending.request.resource,
            pending.request.units,
            true,
            now,
        )?;
        mutations.push(put_mutation(
            lease.id.clone(),
            StoredResource::Lease(lease.clone()),
            Revision::ZERO,
            now,
            WritePrecondition::Missing,
        )?);
        mutations.push(RecordMutation::Delete {
            collection: keith_state_store_core::Collection::ResourceGovernance,
            id: pending.request.id.clone(),
            precondition: WritePrecondition::Exact(pending.revision),
        });
        self.repository
            .transact(&mutations)
            .map_err(|error| classified_repository_error(&error))?;
        apply_accounts(state, updates);
        state.pending.remove(&pending.request.id);
        state.leases.insert(lease.id.clone(), lease.clone());
        Ok(lease)
    }

    fn delete_pending(&self, state: &mut State, pending: &Pending) -> Result<(), ResourceError> {
        self.repository
            .transact(&[RecordMutation::Delete {
                collection: keith_state_store_core::Collection::ResourceGovernance,
                id: pending.request.id.clone(),
                precondition: WritePrecondition::Exact(pending.revision),
            }])
            .map_err(|error| classified_repository_error(&error))?;
        state.pending.remove(&pending.request.id);
        Ok(())
    }

    fn release_locked(
        &self,
        state: &mut State,
        lease: &ResourceLease,
        now: UtcTimestamp,
    ) -> Result<(), ResourceError> {
        let mut account_changes = Vec::new();
        let mut mutations = Vec::new();
        for scope in lease.request.path.scopes() {
            let key = (scope.clone(), lease.request.resource);
            let current = state.accounts.get(&key).ok_or_else(|| {
                ResourceError::Invalid("active lease is missing its resource account".into())
            })?;
            let mut next_account = current.clone();
            next_account.active = next_account
                .active
                .checked_sub(lease.request.units)
                .ok_or_else(|| ResourceError::Invalid("resource accounting underflow".into()))?;
            next_account.revision = next_account
                .revision
                .checked_next()
                .ok_or_else(|| ResourceError::Invalid("account revision exhausted".into()))?;
            next_account.updated_at = now;
            mutations.push(put_mutation(
                next_account.id.clone(),
                StoredResource::Account(next_account.clone()),
                next_account.revision,
                now,
                WritePrecondition::Exact(current.revision),
            )?);
            account_changes.push((key, next_account));
        }
        mutations.push(RecordMutation::Delete {
            collection: keith_state_store_core::Collection::ResourceGovernance,
            id: lease.id.clone(),
            precondition: WritePrecondition::Exact(lease.revision),
        });
        self.repository
            .transact(&mutations)
            .map_err(|error| classified_repository_error(&error))?;
        apply_accounts(state, account_changes);
        state.leases.remove(&lease.id);
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, State>, ResourceError> {
        self.state.lock().map_err(|_| ResourceError::LockPoisoned)
    }

    fn refresh_durable_state(&self, state: &mut State) -> Result<(), ResourceError> {
        let last_tree = std::mem::take(&mut state.last_tree);
        let mut refreshed = load_state(&self.repository)?;
        refreshed.last_tree = last_tree;
        *state = refreshed;
        Ok(())
    }
}

fn load_state<R>(repository: &R) -> Result<State, ResourceError>
where
    R: ResourceRepository,
{
    let mut state = State::default();
    for record in repository
        .list_resource_records()
        .map_err(repository_error)?
    {
        match decode(record)? {
            StoredResource::Account(account) => {
                if state
                    .accounts
                    .insert((account.scope.clone(), account.resource), account)
                    .is_some()
                {
                    return Err(ResourceError::Invalid(
                        "duplicate durable resource account".into(),
                    ));
                }
            }
            StoredResource::Pending(pending) => {
                let id = pending.request.id.clone();
                if state.pending.insert(id, pending).is_some() {
                    return Err(ResourceError::Invalid(
                        "duplicate durable pending request".into(),
                    ));
                }
            }
            StoredResource::Lease(lease) => {
                if state.leases.insert(lease.id.clone(), lease).is_some() {
                    return Err(ResourceError::Invalid("duplicate durable lease".into()));
                }
            }
        }
    }
    Ok(state)
}

fn validate_acquire(request: &AcquireRequest) -> Result<(), ResourceError> {
    let session_required = request.resource != ResourceKind::Workers;
    if !request.resource.is_concurrency()
        || request.units == 0
        || request.idle_timeout_ms == 0
        || request.path.tree().is_none()
        || (session_required && request.path.session().is_none())
        || request.recovery.as_ref().is_some_and(|recovery| {
            recovery.class.resource() != request.resource
                || recovery.resume_marker.trim().is_empty()
                || recovery.resume_marker.len() > 1_024
        })
    {
        return Err(ResourceError::Invalid(
            "acquire requests require bounded concurrency, tree/session scope, and valid recovery"
                .into(),
        ));
    }
    Ok(())
}

fn ensure_queue_capacity(
    state: &State,
    policy: &ResourcePolicy,
    request: &AcquireRequest,
) -> Result<(), ResourceError> {
    for scope in request.path.scopes() {
        let Some(ceiling) = policy.queue_ceiling(scope) else {
            continue;
        };
        let admitted = state
            .pending
            .values()
            .filter(|pending| pending.request.path.scopes().contains(scope))
            .count();
        let maximum = if request.priority == WorkPriority::Background {
            ceiling.maximum.saturating_sub(ceiling.interactive_reserve)
        } else {
            ceiling.maximum
        };
        if admitted >= maximum {
            return Err(ResourceError::QueueFull {
                scope: scope.safe_label(),
                priority: request.priority,
            });
        }
    }
    Ok(())
}

fn exhausted(
    state: &State,
    policy: &ResourcePolicy,
    path: &ScopePath,
    resource: ResourceKind,
    units: u64,
    active: bool,
) -> Result<Option<(ResourceScope, ExhaustionBehavior)>, ResourceError> {
    for scope in path.scopes() {
        let Some(ceiling) = policy.ceiling(scope, resource) else {
            continue;
        };
        let current = state
            .accounts
            .get(&(scope.clone(), resource))
            .map_or(0, |account| {
                if active {
                    account.active
                } else {
                    account.consumed
                }
            });
        let next = current
            .checked_add(units)
            .ok_or_else(|| ResourceError::Invalid("resource counter overflow".into()))?;
        if next > ceiling.maximum {
            return Ok(Some((scope.clone(), ceiling.exhaustion)));
        }
    }
    Ok(None)
}

fn account_updates(
    state: &State,
    path: &ScopePath,
    resource: ResourceKind,
    units: u64,
    active: bool,
    now: UtcTimestamp,
) -> Result<AccountBatch, ResourceError> {
    let mut updates = Vec::new();
    let mut mutations = Vec::new();
    for scope in path.scopes() {
        let key = (scope.clone(), resource);
        let existing = state.accounts.get(&key);
        let mut account = existing.cloned().unwrap_or(Account {
            id: account_id(scope, resource)?,
            scope: scope.clone(),
            resource,
            consumed: 0,
            active: 0,
            revision: Revision::ZERO,
            updated_at: now,
        });
        let precondition = if let Some(current) = existing {
            account.revision = current
                .revision
                .checked_next()
                .ok_or_else(|| ResourceError::Invalid("account revision exhausted".into()))?;
            WritePrecondition::Exact(current.revision)
        } else {
            WritePrecondition::Missing
        };
        if active {
            account.active = account
                .active
                .checked_add(units)
                .ok_or_else(|| ResourceError::Invalid("active counter overflow".into()))?;
        } else {
            account.consumed = account
                .consumed
                .checked_add(units)
                .ok_or_else(|| ResourceError::Invalid("usage counter overflow".into()))?;
        }
        account.updated_at = now;
        mutations.push(put_mutation(
            account.id.clone(),
            StoredResource::Account(account.clone()),
            account.revision,
            now,
            precondition,
        )?);
        updates.push((key, account));
    }
    Ok((updates, mutations))
}

fn account_id(scope: &ResourceScope, resource: ResourceKind) -> Result<EntityId, ResourceError> {
    let mut hasher = Sha256::new();
    hasher.update(b"keith-resource-account-v1\0");
    hasher.update(canonical_json_bytes(&(scope, resource))?);
    let digest = hasher.finalize();
    let mut bytes = [0_u8; 16];
    bytes.copy_from_slice(&digest[..16]);
    Ok(EntityId::from_u128(u128::from_be_bytes(bytes)))
}

fn apply_accounts(state: &mut State, updates: Vec<((ResourceScope, ResourceKind), Account)>) {
    for (key, account) in updates {
        state.accounts.insert(key, account);
    }
}

fn select_next(state: &State, attempted: &BTreeSet<EntityId>) -> Option<EntityId> {
    let mut session_heads = BTreeMap::<Option<keith_agent_types::SessionId>, &Pending>::new();
    for pending in state.pending.values() {
        let session = pending.request.path.session().cloned();
        let replace = session_heads.get(&session).is_none_or(|current| {
            (pending.request.submitted_at, &pending.request.id)
                < (current.request.submitted_at, &current.request.id)
        });
        if replace {
            session_heads.insert(session, pending);
        }
    }
    let priority = session_heads
        .values()
        .filter(|pending| !attempted.contains(&pending.request.id))
        .map(|pending| pending.request.priority)
        .max()?;
    let mut candidates = session_heads
        .values()
        .filter(|pending| {
            pending.request.priority == priority && !attempted.contains(&pending.request.id)
        })
        .copied()
        .collect::<Vec<_>>();
    candidates.sort_by(|left, right| {
        left.request
            .path
            .tree()
            .cmp(&right.request.path.tree())
            .then(left.request.submitted_at.cmp(&right.request.submitted_at))
            .then(left.request.id.cmp(&right.request.id))
    });
    let selected = state.last_tree.get(&priority).and_then(|last| {
        candidates
            .iter()
            .find(|pending| pending.request.path.tree().is_some_and(|tree| tree > last))
    });
    selected
        .or_else(|| candidates.first())
        .map(|pending| pending.request.id.clone())
}

fn checked_timestamp(
    timestamp: UtcTimestamp,
    delta_ms: u64,
) -> Result<UtcTimestamp, ResourceError> {
    let delta = i64::try_from(delta_ms)
        .map_err(|_| ResourceError::Invalid("idle timeout is too large".into()))?;
    timestamp
        .unix_millis()
        .checked_add(delta)
        .map(UtcTimestamp::from_unix_millis)
        .ok_or_else(|| ResourceError::Invalid("lease expiry overflow".into()))
}

fn put_mutation(
    id: EntityId,
    resource: StoredResource,
    revision: Revision,
    updated_at: UtcTimestamp,
    precondition: WritePrecondition,
) -> Result<RecordMutation, ResourceError> {
    Ok(RecordMutation::Put {
        collection: keith_state_store_core::Collection::ResourceGovernance,
        record: VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id,
            revision,
            updated_at,
            payload: serde_json::to_value(resource)?,
        },
        precondition,
    })
}

fn decode(record: VersionedRecord) -> Result<StoredResource, ResourceError> {
    let resource: StoredResource = serde_json::from_value(record.payload)?;
    let (id, revision) = match &resource {
        StoredResource::Account(account) => (&account.id, account.revision),
        StoredResource::Pending(pending) => (&pending.request.id, pending.revision),
        StoredResource::Lease(lease) => (&lease.id, lease.revision),
    };
    if id != &record.id || revision != record.revision {
        return Err(ResourceError::Invalid(
            "resource record identity or revision mismatch".into(),
        ));
    }
    Ok(resource)
}

fn repository_error(error: impl std::error::Error) -> ResourceError {
    ResourceError::Repository(error.to_string())
}

fn classified_repository_error(error: &impl ClassifiedRepositoryError) -> ResourceError {
    if error.is_conflict() {
        ResourceError::RepositoryConflict(error.to_string())
    } else {
        ResourceError::Repository(error.to_string())
    }
}

#[cfg(test)]
mod tests {
    use std::collections::{BTreeMap, BTreeSet};
    use std::sync::{Arc, Barrier};

    use keith_agent_types::{ActionId, GoalId, ProfileId, RootTreeId, SessionId};
    use keith_state_store::EmbeddedStore;
    use tempfile::TempDir;

    use super::*;
    use crate::{
        QueueCeiling, ReclaimClass, RecoveryDescriptor, ResourceCeiling, ResourcePolicy,
        ResourceScope,
    };

    fn policy(
        overrides: impl IntoIterator<Item = ((ResourceScope, ResourceKind), ResourceCeiling)>,
    ) -> ResourcePolicy {
        let mut ceilings = ResourceKind::concurrency_kinds()
            .iter()
            .map(|resource| {
                (
                    (ResourceScope::Installation, *resource),
                    ResourceCeiling {
                        maximum: 10_000,
                        exhaustion: ExhaustionBehavior::Pause,
                    },
                )
            })
            .collect::<BTreeMap<_, _>>();
        ceilings.extend(overrides);
        ResourcePolicy::new(ceilings).unwrap()
    }

    fn path(profile: &ProfileId, tree: &RootTreeId, session: &SessionId) -> ScopePath {
        ScopePath::new(vec![
            ResourceScope::Installation,
            ResourceScope::Profile(profile.clone()),
            ResourceScope::Tree(tree.clone()),
            ResourceScope::Session(session.clone()),
            ResourceScope::Goal(GoalId::new()),
            ResourceScope::Action(ActionId::new()),
        ])
        .unwrap()
    }

    fn request(
        path: ScopePath,
        resource: ResourceKind,
        priority: WorkPriority,
        submitted_at: i64,
    ) -> AcquireRequest {
        AcquireRequest {
            id: EntityId::new(),
            path,
            resource,
            units: 1,
            priority,
            recovery: None,
            submitted_at: UtcTimestamp::from_unix_millis(submitted_at),
            idle_timeout_ms: 100,
        }
    }

    fn granted(outcomes: &[ScheduleOutcome]) -> Vec<ResourceLease> {
        outcomes
            .iter()
            .filter_map(|outcome| match outcome {
                ScheduleOutcome::Granted(lease) => Some(lease.clone()),
                _ => None,
            })
            .collect()
    }

    fn hierarchy_path(profile: &ProfileId, tree: &RootTreeId, session: &SessionId) -> ScopePath {
        ScopePath::new(vec![
            ResourceScope::Installation,
            ResourceScope::Worker(tree.clone()),
            ResourceScope::Profile(profile.clone()),
            ResourceScope::Tree(tree.clone()),
            ResourceScope::Session(session.clone()),
            ResourceScope::Goal(GoalId::new()),
            ResourceScope::Action(ActionId::new()),
            ResourceScope::ProviderAccount {
                provider: "provider".into(),
                account: "private-account".into(),
            },
            ResourceScope::Tool("shell".into()),
            ResourceScope::Child(SessionId::new()),
            ResourceScope::Kernel(SessionId::new()),
            ResourceScope::Browser(SessionId::new()),
            ResourceScope::Process(SessionId::new()),
            ResourceScope::Channel("operator".into()),
            ResourceScope::Scheduler(profile.clone()),
            ResourceScope::Background(profile.clone()),
        ])
        .unwrap()
    }

    #[test]
    fn every_declared_hierarchy_has_bounded_admission_and_interactive_reserve() {
        let profile = ProfileId::new();
        let tree = RootTreeId::new();
        let session = SessionId::new();
        let hierarchy = hierarchy_path(&profile, &tree, &session);
        for scope in hierarchy.scopes().iter().skip(1) {
            let limits = BTreeMap::from([
                (
                    ResourceScope::Installation,
                    QueueCeiling {
                        maximum: 100,
                        interactive_reserve: 10,
                    },
                ),
                (
                    scope.clone(),
                    QueueCeiling {
                        maximum: 1,
                        interactive_reserve: 0,
                    },
                ),
            ]);
            let policy = policy([]).with_queue_ceilings(limits).unwrap();
            let governor =
                ResourceGovernor::open(EmbeddedStore::open_in_memory().unwrap(), policy).unwrap();
            governor
                .submit(request(
                    hierarchy.clone(),
                    ResourceKind::ActiveSessions,
                    WorkPriority::Normal,
                    0,
                ))
                .unwrap();
            assert!(matches!(
                governor.submit(request(
                    hierarchy.clone(),
                    ResourceKind::ActiveSessions,
                    WorkPriority::Normal,
                    1,
                )),
                Err(ResourceError::QueueFull { scope: ref label, .. })
                    if label == &scope.safe_label()
            ));
        }

        let policy = policy([])
            .with_queue_ceilings(BTreeMap::from([(
                ResourceScope::Installation,
                QueueCeiling {
                    maximum: 3,
                    interactive_reserve: 1,
                },
            )]))
            .unwrap();
        let governor =
            ResourceGovernor::open(EmbeddedStore::open_in_memory().unwrap(), policy).unwrap();
        for submitted_at in 0..2 {
            governor
                .submit(request(
                    path(&profile, &tree, &session),
                    ResourceKind::ProviderRequests,
                    WorkPriority::Background,
                    submitted_at,
                ))
                .unwrap();
        }
        assert!(matches!(
            governor.submit(request(
                path(&profile, &tree, &session),
                ResourceKind::ProviderRequests,
                WorkPriority::Background,
                2,
            )),
            Err(ResourceError::QueueFull {
                priority: WorkPriority::Background,
                ..
            })
        ));
        let interactive = request(
            path(&profile, &RootTreeId::new(), &SessionId::new()),
            ResourceKind::ProviderRequests,
            WorkPriority::Interactive,
            3,
        );
        governor.submit(interactive.clone()).unwrap();
        assert_eq!(
            granted(&governor.schedule(UtcTimestamp::UNIX_EPOCH, 1).unwrap())[0]
                .request
                .id,
            interactive.id
        );
    }

    #[test]
    fn scheduler_preserves_session_order_prioritizes_interactive_and_round_robins_trees() {
        let governor =
            ResourceGovernor::open(EmbeddedStore::open_in_memory().unwrap(), policy([])).unwrap();
        let profile = ProfileId::new();
        let tree_a = RootTreeId::new();
        let tree_b = RootTreeId::new();
        let tree_c = RootTreeId::new();
        let session_a = SessionId::new();
        let session_b = SessionId::new();
        let session_c = SessionId::new();
        let background_head = request(
            path(&profile, &tree_a, &session_a),
            ResourceKind::ProviderRequests,
            WorkPriority::Background,
            0,
        );
        let interactive_behind_head = request(
            path(&profile, &tree_a, &session_a),
            ResourceKind::ProviderRequests,
            WorkPriority::Interactive,
            1,
        );
        let normal = request(
            path(&profile, &tree_b, &session_b),
            ResourceKind::ProviderRequests,
            WorkPriority::Normal,
            0,
        );
        let interactive = request(
            path(&profile, &tree_c, &session_c),
            ResourceKind::ProviderRequests,
            WorkPriority::Interactive,
            0,
        );
        for item in [
            background_head.clone(),
            interactive_behind_head.clone(),
            normal.clone(),
            interactive.clone(),
        ] {
            governor.submit(item).unwrap();
        }
        let first = governor
            .schedule(UtcTimestamp::from_unix_millis(2), 4)
            .unwrap();
        let ids = granted(&first)
            .into_iter()
            .map(|lease| lease.request.id)
            .collect::<Vec<_>>();
        assert_eq!(
            ids,
            vec![
                interactive.id,
                normal.id,
                background_head.id,
                interactive_behind_head.id
            ]
        );

        let mut trees = [tree_a, tree_b, tree_c];
        trees.sort();
        let sessions = [SessionId::new(), SessionId::new(), SessionId::new()];
        let round_robin =
            ResourceGovernor::open(EmbeddedStore::open_in_memory().unwrap(), policy([])).unwrap();
        for (tree, session) in trees.iter().zip(sessions.iter()) {
            round_robin
                .submit(request(
                    path(&profile, tree, session),
                    ResourceKind::SafeParallelTools,
                    WorkPriority::Normal,
                    0,
                ))
                .unwrap();
        }
        let order = granted(
            &round_robin
                .schedule(UtcTimestamp::from_unix_millis(1), 3)
                .unwrap(),
        )
        .into_iter()
        .map(|lease| lease.request.path.tree().cloned().unwrap())
        .collect::<Vec<_>>();
        assert_eq!(order, trees);
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn exhaustion_is_tree_local_exact_redacted_and_recovers_capacity() {
        let profile = ProfileId::new();
        let tree_a = RootTreeId::new();
        let tree_b = RootTreeId::new();
        let session_a = SessionId::new();
        let session_b = SessionId::new();
        let provider_scope = ResourceScope::ProviderAccount {
            provider: "openai".into(),
            account: "secret-account-reference".into(),
        };
        let limits = [
            (
                (ResourceScope::Tree(tree_a.clone()), ResourceKind::Kernels),
                ResourceCeiling {
                    maximum: 1,
                    exhaustion: ExhaustionBehavior::Pause,
                },
            ),
            (
                (ResourceScope::Tree(tree_b.clone()), ResourceKind::Kernels),
                ResourceCeiling {
                    maximum: 1,
                    exhaustion: ExhaustionBehavior::Pause,
                },
            ),
            (
                (provider_scope.clone(), ResourceKind::ProviderRequests),
                ResourceCeiling {
                    maximum: 1,
                    exhaustion: ExhaustionBehavior::Pause,
                },
            ),
            (
                (
                    ResourceScope::Profile(profile.clone()),
                    ResourceKind::Notifications,
                ),
                ResourceCeiling {
                    maximum: 2,
                    exhaustion: ExhaustionBehavior::Fail,
                },
            ),
        ];
        let governor =
            ResourceGovernor::open(EmbeddedStore::open_in_memory().unwrap(), policy(limits))
                .unwrap();
        let a1 = request(
            path(&profile, &tree_a, &session_a),
            ResourceKind::Kernels,
            WorkPriority::Normal,
            0,
        );
        let a2 = request(
            path(&profile, &tree_a, &SessionId::new()),
            ResourceKind::Kernels,
            WorkPriority::Normal,
            1,
        );
        let b1 = request(
            path(&profile, &tree_b, &session_b),
            ResourceKind::Kernels,
            WorkPriority::Normal,
            0,
        );
        for item in [a1, a2.clone(), b1] {
            governor.submit(item).unwrap();
        }
        let outcomes = governor
            .schedule(UtcTimestamp::from_unix_millis(2), 3)
            .unwrap();
        assert_eq!(granted(&outcomes).len(), 2);
        assert!(outcomes.iter().any(|outcome| matches!(
            outcome,
            ScheduleOutcome::Paused { request_id, .. } if request_id == &a2.id
        )));
        let lease_a = granted(&outcomes)
            .into_iter()
            .find(|lease| lease.request.path.tree() == Some(&tree_a))
            .unwrap();
        governor
            .release(&lease_a.id, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        assert_eq!(
            granted(
                &governor
                    .schedule(UtcTimestamp::from_unix_millis(4), 1)
                    .unwrap()
            )[0]
            .request
            .id,
            a2.id
        );

        let provider_path = ScopePath::new(vec![
            ResourceScope::Installation,
            ResourceScope::Profile(profile.clone()),
            ResourceScope::Tree(tree_b.clone()),
            ResourceScope::Session(SessionId::new()),
            provider_scope,
        ])
        .unwrap();
        governor
            .submit(request(
                provider_path.clone(),
                ResourceKind::ProviderRequests,
                WorkPriority::Interactive,
                5,
            ))
            .unwrap();
        governor
            .submit(request(
                provider_path,
                ResourceKind::ProviderRequests,
                WorkPriority::Interactive,
                6,
            ))
            .unwrap();
        let provider = governor
            .schedule(UtcTimestamp::from_unix_millis(7), 2)
            .unwrap();
        assert_eq!(granted(&provider).len(), 1);
        assert_eq!(
            provider
                .iter()
                .filter(|outcome| matches!(outcome, ScheduleOutcome::Paused { .. }))
                .count(),
            1
        );

        let usage_path = path(&profile, &tree_b, &session_b);
        assert_eq!(
            governor
                .record_usage(
                    &UsageDelta {
                        path: usage_path.clone(),
                        resource: ResourceKind::Notifications,
                        units: 2,
                    },
                    UtcTimestamp::from_unix_millis(8),
                )
                .unwrap(),
            UsageOutcome::Recorded
        );
        assert!(matches!(
            governor
                .record_usage(
                    &UsageDelta {
                        path: usage_path,
                        resource: ResourceKind::Notifications,
                        units: 1,
                    },
                    UtcTimestamp::from_unix_millis(9),
                )
                .unwrap(),
            UsageOutcome::Failed { .. }
        ));
        let projections = governor.projections().unwrap();
        assert!(
            projections
                .iter()
                .any(|projection| projection.scope == "provider:openai:<redacted>")
        );
        assert!(!format!("{projections:?}").contains("secret-account-reference"));
    }

    #[test]
    fn every_measurable_profile_and_goal_dimension_is_accounted() {
        let profile = ProfileId::new();
        let tree = RootTreeId::new();
        let session = SessionId::new();
        let path = path(&profile, &tree, &session);
        let governor =
            ResourceGovernor::open(EmbeddedStore::open_in_memory().unwrap(), policy([])).unwrap();
        let measurable = [
            ResourceKind::Tokens,
            ResourceKind::ModelCostMicros,
            ResourceKind::WallTimeMs,
            ResourceKind::ToolCalls,
            ResourceKind::MemoryBytes,
            ResourceKind::StorageBytes,
            ResourceKind::NetworkBytes,
            ResourceKind::Retries,
            ResourceKind::Notifications,
        ];
        for resource in measurable {
            assert_eq!(
                governor
                    .record_usage(
                        &UsageDelta {
                            path: path.clone(),
                            resource,
                            units: 1,
                        },
                        UtcTimestamp::UNIX_EPOCH,
                    )
                    .unwrap(),
                UsageOutcome::Recorded
            );
        }
        let projections = governor.projections().unwrap();
        let profile_label = ResourceScope::Profile(profile).safe_label();
        let goal_label = path
            .scopes()
            .iter()
            .find(|scope| matches!(scope, ResourceScope::Goal(_)))
            .unwrap()
            .safe_label();
        for resource in measurable {
            assert!(projections.iter().any(|projection| {
                projection.scope == profile_label
                    && projection.resource == resource
                    && projection.consumed == 1
            }));
            assert!(projections.iter().any(|projection| {
                projection.scope == goal_label
                    && projection.resource == resource
                    && projection.consumed == 1
            }));
        }
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn independent_governors_reconcile_shared_usage_and_admission_accounts() {
        let directory = TempDir::new().unwrap();
        let database = directory.path().join("state.sqlite");
        let profile = ProfileId::new();
        let first_path = path(&profile, &RootTreeId::new(), &SessionId::new());
        let second_path = path(&profile, &RootTreeId::new(), &SessionId::new());
        let first_admission_path = first_path.clone();
        let second_admission_path = second_path.clone();
        let first = Arc::new(
            ResourceGovernor::open(EmbeddedStore::open(&database, None).unwrap(), policy([]))
                .unwrap(),
        );
        let second = Arc::new(
            ResourceGovernor::open(EmbeddedStore::open(&database, None).unwrap(), policy([]))
                .unwrap(),
        );
        let barrier = Arc::new(Barrier::new(2));
        let first_barrier = Arc::clone(&barrier);
        let second_barrier = Arc::clone(&barrier);
        let first_governor = Arc::clone(&first);
        let second_governor = Arc::clone(&second);
        std::thread::scope(|scope| {
            let first_result = scope.spawn(move || {
                first_barrier.wait();
                for offset in 0..16 {
                    assert_eq!(
                        first_governor
                            .record_usage(
                                &UsageDelta {
                                    path: first_path.clone(),
                                    resource: ResourceKind::Tokens,
                                    units: 1,
                                },
                                UtcTimestamp::from_unix_millis(offset),
                            )
                            .unwrap(),
                        UsageOutcome::Recorded
                    );
                }
            });
            let second_result = scope.spawn(move || {
                second_barrier.wait();
                for offset in 16..32 {
                    assert_eq!(
                        second_governor
                            .record_usage(
                                &UsageDelta {
                                    path: second_path.clone(),
                                    resource: ResourceKind::Tokens,
                                    units: 1,
                                },
                                UtcTimestamp::from_unix_millis(offset),
                            )
                            .unwrap(),
                        UsageOutcome::Recorded
                    );
                }
            });
            first_result.join().unwrap();
            second_result.join().unwrap();
        });
        let recovered =
            ResourceGovernor::open(EmbeddedStore::open(&database, None).unwrap(), policy([]))
                .unwrap();
        let projections = recovered.projections().unwrap();
        let profile_label = ResourceScope::Profile(profile).safe_label();
        assert!(projections.iter().any(|projection| {
            projection.scope == ResourceScope::Installation.safe_label()
                && projection.resource == ResourceKind::Tokens
                && projection.consumed == 32
        }));
        assert!(projections.iter().any(|projection| {
            projection.scope == profile_label
                && projection.resource == ResourceKind::Tokens
                && projection.consumed == 32
        }));

        let first_request = request(
            first_admission_path,
            ResourceKind::ActiveSessions,
            WorkPriority::Interactive,
            32,
        );
        let second_request = request(
            second_admission_path,
            ResourceKind::ActiveSessions,
            WorkPriority::Interactive,
            33,
        );
        first.submit(first_request.clone()).unwrap();
        second.submit(second_request.clone()).unwrap();
        let barrier = Arc::new(Barrier::new(2));
        let first_governor = Arc::clone(&first);
        let second_governor = Arc::clone(&second);
        let first_barrier = Arc::clone(&barrier);
        let second_barrier = Arc::clone(&barrier);
        let (first_lease, second_lease) = std::thread::scope(|scope| {
            let first_result = scope.spawn(move || {
                first_barrier.wait();
                first_governor
                    .schedule_request(&first_request.id, UtcTimestamp::from_unix_millis(34))
                    .unwrap()
            });
            let second_result = scope.spawn(move || {
                second_barrier.wait();
                second_governor
                    .schedule_request(&second_request.id, UtcTimestamp::from_unix_millis(34))
                    .unwrap()
            });
            let ScheduleOutcome::Granted(first_lease) = first_result.join().unwrap() else {
                panic!("first admission was not granted");
            };
            let ScheduleOutcome::Granted(second_lease) = second_result.join().unwrap() else {
                panic!("second admission was not granted");
            };
            (first_lease, second_lease)
        });
        let barrier = Arc::new(Barrier::new(2));
        std::thread::scope(|scope| {
            let first_barrier = Arc::clone(&barrier);
            let second_barrier = Arc::clone(&barrier);
            let first_result = scope.spawn(move || {
                first_barrier.wait();
                first
                    .release(&first_lease.id, UtcTimestamp::from_unix_millis(35))
                    .unwrap();
            });
            let second_result = scope.spawn(move || {
                second_barrier.wait();
                second
                    .release(&second_lease.id, UtcTimestamp::from_unix_millis(35))
                    .unwrap();
            });
            first_result.join().unwrap();
            second_result.join().unwrap();
        });
        let recovered =
            ResourceGovernor::open(EmbeddedStore::open(&database, None).unwrap(), policy([]))
                .unwrap();
        assert!(recovered.projections().unwrap().iter().any(|projection| {
            projection.scope == ResourceScope::Installation.safe_label()
                && projection.resource == ResourceKind::ActiveSessions
                && projection.active == 0
        }));
    }

    #[test]
    fn idle_classes_reclaim_independently_and_recover_after_restart() {
        let directory = TempDir::new().unwrap();
        let database = directory.path().join("resources.sqlite");
        let limits = policy([]);
        let profile = ProfileId::new();
        let tree = RootTreeId::new();
        let session = SessionId::new();
        let classes = [
            (ReclaimClass::Worker, ResourceKind::Workers),
            (ReclaimClass::Child, ResourceKind::Children),
            (ReclaimClass::Kernel, ResourceKind::Kernels),
            (ReclaimClass::Browser, ResourceKind::Browsers),
            (ReclaimClass::McpSession, ResourceKind::McpSessions),
            (ReclaimClass::ToolProcess, ResourceKind::Processes),
        ];
        {
            let governor = ResourceGovernor::open(
                EmbeddedStore::open(&database, None).unwrap(),
                limits.clone(),
            )
            .unwrap();
            for (index, (class, resource)) in classes.into_iter().enumerate() {
                let mut item = request(
                    path(&profile, &tree, &session),
                    resource,
                    WorkPriority::Normal,
                    i64::try_from(index).unwrap(),
                );
                item.idle_timeout_ms = u64::try_from(index + 1).unwrap();
                item.recovery = Some(RecoveryDescriptor {
                    class,
                    durable_state_id: EntityId::new(),
                    resume_marker: format!("resume-{class:?}"),
                });
                governor.submit(item).unwrap();
            }
            assert_eq!(
                granted(
                    &governor
                        .schedule(UtcTimestamp::UNIX_EPOCH, classes.len())
                        .unwrap()
                )
                .len(),
                classes.len()
            );
            let reclaimed = governor
                .reclaim_idle(UtcTimestamp::from_unix_millis(3))
                .unwrap();
            assert_eq!(
                reclaimed
                    .iter()
                    .map(|item| item.recovery.class)
                    .collect::<BTreeSet<_>>(),
                BTreeSet::from([
                    ReclaimClass::Worker,
                    ReclaimClass::Child,
                    ReclaimClass::Kernel,
                ])
            );
            assert_eq!(governor.lease_count().unwrap(), 3);
        }
        let recovered =
            ResourceGovernor::open(EmbeddedStore::open(&database, None).unwrap(), limits).unwrap();
        assert_eq!(recovered.lease_count().unwrap(), 3);
        let reclaimed = recovered
            .reclaim_idle(UtcTimestamp::from_unix_millis(10))
            .unwrap();
        assert_eq!(reclaimed.len(), 3);
        assert!(
            reclaimed
                .iter()
                .all(|item| item.recovery.resume_marker.starts_with("resume-"))
        );
        assert_eq!(recovered.lease_count().unwrap(), 0);
    }

    #[test]
    fn saturation_is_fair_bounded_and_returns_to_normal_throughput() {
        let profile = ProfileId::new();
        let trees = (0..5).map(|_| RootTreeId::new()).collect::<Vec<_>>();
        let governor = ResourceGovernor::open(
            EmbeddedStore::open_in_memory().unwrap(),
            policy([(
                (ResourceScope::Installation, ResourceKind::ActiveSessions),
                ResourceCeiling {
                    maximum: 20,
                    exhaustion: ExhaustionBehavior::Pause,
                },
            )]),
        )
        .unwrap();
        for index in 0..100 {
            let tree = &trees[index % trees.len()];
            governor
                .submit(request(
                    path(&profile, tree, &SessionId::new()),
                    ResourceKind::ActiveSessions,
                    WorkPriority::Normal,
                    i64::try_from(index).unwrap(),
                ))
                .unwrap();
        }
        let first = governor
            .schedule(UtcTimestamp::from_unix_millis(100), 100)
            .unwrap();
        let leases = granted(&first);
        assert_eq!(leases.len(), 20);
        let mut per_tree = BTreeMap::new();
        for lease in &leases {
            *per_tree
                .entry(lease.request.path.tree().cloned().unwrap())
                .or_insert(0) += 1;
        }
        assert_eq!(
            per_tree.values().copied().collect::<BTreeSet<_>>(),
            BTreeSet::from([4])
        );
        assert_eq!(governor.pending_count().unwrap(), 80);
        for lease in leases {
            governor
                .release(&lease.id, UtcTimestamp::from_unix_millis(101))
                .unwrap();
        }
        let recovered = granted(
            &governor
                .schedule(UtcTimestamp::from_unix_millis(102), 100)
                .unwrap(),
        );
        assert_eq!(recovered.len(), 20);
        assert_eq!(governor.lease_count().unwrap(), 20);
    }

    #[test]
    fn recursive_explosion_stops_at_count_and_depth_without_losing_queue_state() {
        let profile = ProfileId::new();
        let tree = RootTreeId::new();
        let governor = ResourceGovernor::open(
            EmbeddedStore::open_in_memory().unwrap(),
            policy([
                (
                    (ResourceScope::Tree(tree.clone()), ResourceKind::Children),
                    ResourceCeiling {
                        maximum: 3,
                        exhaustion: ExhaustionBehavior::Pause,
                    },
                ),
                (
                    (
                        ResourceScope::Tree(tree.clone()),
                        ResourceKind::RecursiveDepth,
                    ),
                    ResourceCeiling {
                        maximum: 4,
                        exhaustion: ExhaustionBehavior::Fail,
                    },
                ),
            ]),
        )
        .unwrap();
        let mut excessive_depth = request(
            path(&profile, &tree, &SessionId::new()),
            ResourceKind::RecursiveDepth,
            WorkPriority::Interactive,
            0,
        );
        excessive_depth.units = 5;
        governor.submit(excessive_depth.clone()).unwrap();
        assert!(matches!(
            governor.schedule(UtcTimestamp::UNIX_EPOCH, 1).unwrap()[0],
            ScheduleOutcome::Failed { ref request_id, .. }
                if request_id == &excessive_depth.id
        ));
        for index in 0..50 {
            governor
                .submit(request(
                    path(&profile, &tree, &SessionId::new()),
                    ResourceKind::Children,
                    WorkPriority::Normal,
                    i64::from(index),
                ))
                .unwrap();
        }
        let outcomes = governor
            .schedule(UtcTimestamp::from_unix_millis(50), 50)
            .unwrap();
        assert_eq!(granted(&outcomes).len(), 3);
        assert_eq!(
            outcomes
                .iter()
                .filter(|outcome| matches!(outcome, ScheduleOutcome::Paused { .. }))
                .count(),
            47
        );
        assert_eq!(governor.pending_count().unwrap(), 47);
    }
}
