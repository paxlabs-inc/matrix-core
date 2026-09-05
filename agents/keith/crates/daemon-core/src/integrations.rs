use std::collections::{BTreeMap, BTreeSet};
use std::path::Path;

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, ProfileId, Revision, Sequence, UtcTimestamp,
};
use keith_configuration::{PlatformServiceGroup, ServiceEnablementConfig};
use keith_platform_contracts::{
    ActionRisk, AuditOutcome, Capability, LifecycleState, RedactedText, ResourceBounds,
};
use keith_profile::RegisteredProfile;
use keith_protocol::{
    IntegrationAvailabilityProjection, IntegrationControl, IntegrationDeletionProjection,
    IntegrationMutation, IntegrationOperation, IntegrationResourceProjection, IntegrationService,
    IntegrationServiceProjection, ProfileIntegrationsProjection,
};
use keith_runtime_api::{
    ActiveServiceOperation, ExternalServiceKind, ServiceAvailability, ServiceControl,
    ServiceRegistration,
};
use keith_state_store::{EmbeddedStore, FileBackupHook, StoreError};
use keith_state_store_core::{
    AtomicStateRepository, Collection, RecordMutation, VersionedRecord, WritePrecondition,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Debug, Error)]
pub(crate) enum IntegrationError {
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error("integration state is corrupt: {0}")]
    Corrupt(String),
    #[error("integration request is invalid: {0}")]
    Invalid(String),
    #[error("integration authority was denied: {0}")]
    Unauthorized(String),
    #[error("integration service is disabled")]
    Disabled,
    #[error("integration service is unavailable: {0}")]
    Unavailable(String),
    #[error("integration resource was not found")]
    NotFound,
    #[error("integration revision is stale")]
    Conflict,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct IntegrationAuditRecord {
    profile_id: ProfileId,
    service: ExternalServiceKind,
    resource_id: EntityId,
    operation: IntegrationOperation,
    correlation_id: keith_platform_contracts::AuditCorrelationId,
    outcome: AuditOutcome,
    occurred_at: UtcTimestamp,
}

pub(crate) enum IntegrationMutationResult {
    Resource(IntegrationResourceProjection),
    Deleted(IntegrationDeletionProjection),
}

pub(crate) struct PlatformServiceCoordinator {
    store: EmbeddedStore,
    enablement: ServiceEnablementConfig,
    unavailable: BTreeMap<IntegrationService, String>,
}

impl PlatformServiceCoordinator {
    pub(crate) fn open(
        state_path: &Path,
        enablement: ServiceEnablementConfig,
    ) -> Result<Self, IntegrationError> {
        let mut coordinator = Self {
            store: EmbeddedStore::open(state_path, Some(&FileBackupHook))?,
            enablement,
            unavailable: BTreeMap::new(),
        };
        for service in all_services() {
            if let Err(error) = coordinator.reconcile_service_after_restart(service) {
                coordinator.unavailable.insert(
                    service,
                    format!("durable service state requires repair: {error}"),
                );
            }
        }
        if let Err(error) = coordinator.reconcile_operations_after_restart() {
            for service in all_services() {
                coordinator
                    .unavailable
                    .entry(service)
                    .or_insert_with(|| format!("durable operation state requires repair: {error}"));
            }
        }
        Ok(coordinator)
    }

    pub(crate) fn list(
        &self,
        profile_id: &ProfileId,
        filter: Option<IntegrationService>,
    ) -> Result<ProfileIntegrationsProjection, IntegrationError> {
        let mut resources = Vec::new();
        let mut through_sequence = 0_u64;
        for service in all_services() {
            if filter.is_some_and(|selected| selected != service) {
                continue;
            }
            for record in self.store.list_records(collection(service))? {
                let registration = match decode_registration(&record) {
                    Ok(registration) => registration,
                    Err(_) if self.unavailable.contains_key(&service) => continue,
                    Err(error) => return Err(error),
                };
                if registration.profile_id == *profile_id {
                    through_sequence = through_sequence.max(registration.revision.get());
                    resources.push(project(registration));
                }
            }
        }
        resources.sort_by(|left, right| {
            (left.service, &left.display_label, &left.id).cmp(&(
                right.service,
                &right.display_label,
                &right.id,
            ))
        });
        let services = all_services()
            .into_iter()
            .filter(|service| filter.is_none_or(|selected| selected == *service))
            .map(|service| IntegrationServiceProjection {
                service,
                availability: self.availability(service),
            })
            .collect();
        Ok(ProfileIntegrationsProjection {
            profile_id: profile_id.clone(),
            through_sequence: Sequence::new(through_sequence),
            services,
            resources,
        })
    }

    pub(crate) fn inspect(
        &self,
        profile_id: &ProfileId,
        service: IntegrationService,
        resource_id: &EntityId,
    ) -> Result<IntegrationResourceProjection, IntegrationError> {
        let record = self
            .store
            .get_record(collection(service), resource_id)?
            .ok_or(IntegrationError::NotFound)?;
        let registration = decode_registration(&record)?;
        if registration.profile_id != *profile_id || registration.service != native(service) {
            return Err(IntegrationError::NotFound);
        }
        Ok(project(registration))
    }

    pub(crate) fn mutate(
        &self,
        mutation: IntegrationMutation,
        now: UtcTimestamp,
    ) -> Result<IntegrationMutationResult, IntegrationError> {
        Self::validate_mutation(&mutation, now)?;
        let recovery_operation = matches!(
            mutation.operation,
            IntegrationOperation::Cancel
                | IntegrationOperation::Delete
                | IntegrationOperation::Export
        );
        if !recovery_operation {
            self.require_available(mutation.service)?;
        }
        if !operation_supported(mutation.service, mutation.operation) {
            return Err(IntegrationError::Invalid(
                "operation is not supported by this integration service".into(),
            ));
        }
        self.validate_profile_policy(&mutation)?;
        if let Some(replayed) = self.idempotent_result(&mutation)? {
            return Ok(replayed);
        }
        let existing = self.load_existing_registration(&mutation)?;
        let resource_id = existing
            .as_ref()
            .map_or_else(EntityId::new, |registration| registration.id.clone());
        if matches!(mutation.operation, IntegrationOperation::Delete) {
            return self.delete(mutation, existing.ok_or(IntegrationError::NotFound)?, now);
        }
        self.persist_mutation(mutation, existing.as_ref(), resource_id, now)
    }

    fn load_existing_registration(
        &self,
        mutation: &IntegrationMutation,
    ) -> Result<Option<ServiceRegistration>, IntegrationError> {
        let existing = mutation
            .resource_id
            .as_ref()
            .map(|id| self.registration(mutation.service, id))
            .transpose()?
            .flatten();
        if let Some(registration) = &existing
            && (registration.profile_id != mutation.profile_id
                || registration.service != native(mutation.service)
                || registration.native_resource_key != mutation.native_resource_key)
        {
            return Err(IntegrationError::NotFound);
        }
        let create = existing.is_none();
        if create && !creates_resource(mutation.operation) {
            return Err(IntegrationError::NotFound);
        }
        if !create && creates_resource(mutation.operation) && mutation.resource_id.is_none() {
            return Err(IntegrationError::Invalid(
                "resource-creating operation omitted an exact resource identity".into(),
            ));
        }
        if let Some(registration) = &existing {
            let expected = mutation
                .expected_revision
                .ok_or(IntegrationError::Conflict)?;
            if expected != registration.revision {
                return Err(IntegrationError::Conflict);
            }
            if matches!(mutation.operation, IntegrationOperation::Cancel)
                && mutation.authority.cancellation_id != registration.cancellation_id
            {
                return Err(IntegrationError::Unauthorized(
                    "cancellation identity does not match the active resource".into(),
                ));
            }
        } else if mutation.expected_revision.is_some() {
            return Err(IntegrationError::Conflict);
        }
        Ok(existing)
    }

    fn persist_mutation(
        &self,
        mutation: IntegrationMutation,
        existing: Option<&ServiceRegistration>,
        resource_id: EntityId,
        now: UtcTimestamp,
    ) -> Result<IntegrationMutationResult, IntegrationError> {
        let next_revision = existing.map_or(Some(Revision::ZERO), |registration| {
            registration.revision.checked_next()
        });
        let next_revision = next_revision
            .ok_or_else(|| IntegrationError::Invalid("integration revision is exhausted".into()))?;
        let lifecycle = desired_lifecycle(mutation.operation, existing)?;
        let display_label = RedactedText::parse(mutation.display_label.clone())
            .map_err(|error| IntegrationError::Invalid(error.to_string()))?;
        let registration = ServiceRegistration {
            id: resource_id.clone(),
            profile_id: mutation.profile_id.clone(),
            owning_session_id: Some(mutation.authority.session_id.clone()),
            service: native(mutation.service),
            native_resource_key: mutation.native_resource_key.clone(),
            display_label,
            availability: ServiceAvailability::Available,
            lifecycle,
            effect: mutation.authority.external_effect.clone(),
            cancellation_id: mutation.authority.cancellation_id.clone(),
            audit_correlation: mutation.authority.audit_correlation.clone(),
            bounds: service_bounds(mutation.service),
            controls: service_controls(lifecycle),
            safe_error: None,
            revision: next_revision,
            created_at: existing.map_or(now, |record| record.created_at),
            updated_at: now,
        };
        registration.validate().map_err(IntegrationError::Invalid)?;
        let operation_id = EntityId::new();
        let (operation_lifecycle, audit_outcome) = admitted_operation_state(mutation.operation);
        let operation = ActiveServiceOperation {
            id: operation_id.clone(),
            registration_id: resource_id.clone(),
            profile_id: mutation.profile_id.clone(),
            action: mutation.authority.clone(),
            idempotency_key: mutation.idempotency_key.clone(),
            lifecycle: operation_lifecycle,
            attempt: u32::from(!matches!(operation_lifecycle, LifecycleState::Pending)),
            created_at: now,
            updated_at: now,
            safe_error: None,
        };
        operation.validate().map_err(IntegrationError::Invalid)?;
        let audit = IntegrationAuditRecord {
            profile_id: mutation.profile_id,
            service: native(mutation.service),
            resource_id,
            operation: mutation.operation,
            correlation_id: mutation.authority.audit_correlation,
            outcome: audit_outcome,
            occurred_at: now,
        };
        let precondition = existing.map_or(WritePrecondition::Missing, |record| {
            WritePrecondition::Exact(record.revision)
        });
        self.store.transact(&[
            RecordMutation::Put {
                collection: collection(mutation.service),
                record: encode_record(&registration, registration.id.clone(), next_revision, now)?,
                precondition,
            },
            RecordMutation::Put {
                collection: Collection::IntegrationOperations,
                record: encode_record(&operation, operation_id, Revision::ZERO, now)?,
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::IntegrationAudit,
                record: encode_record(&audit, EntityId::new(), Revision::ZERO, now)?,
                precondition: WritePrecondition::Missing,
            },
        ])?;
        Ok(IntegrationMutationResult::Resource(project(registration)))
    }

    fn validate_mutation(
        mutation: &IntegrationMutation,
        now: UtcTimestamp,
    ) -> Result<(), IntegrationError> {
        if mutation.profile_id != mutation.authority.profile_id {
            return Err(IntegrationError::Unauthorized(
                "action profile does not match the requested profile".into(),
            ));
        }
        if mutation.native_resource_key.is_empty()
            || mutation.native_resource_key.len() > 256
            || mutation.native_resource_key.chars().any(char::is_control)
            || mutation.idempotency_key.is_empty()
            || mutation.idempotency_key.len() > 256
            || mutation.idempotency_key.chars().any(char::is_control)
        {
            return Err(IntegrationError::Invalid(
                "resource or idempotency identity is invalid".into(),
            ));
        }
        if mutation.authority.target.as_str() != mutation.native_resource_key {
            return Err(IntegrationError::Unauthorized(
                "action target does not match the exact integration resource".into(),
            ));
        }
        let (capability, risk) = required_authority(mutation.service, mutation.operation);
        if mutation.authority.requested_capability != capability || mutation.authority.risk != risk
        {
            return Err(IntegrationError::Unauthorized(
                "action capability or risk does not match the operation".into(),
            ));
        }
        mutation
            .authority
            .approval
            .authorize(&mutation.authority, now)
            .map_err(|error| IntegrationError::Unauthorized(error.to_string()))?;
        Ok(())
    }

    fn validate_profile_policy(
        &self,
        mutation: &IntegrationMutation,
    ) -> Result<(), IntegrationError> {
        let record = self
            .store
            .get_record(Collection::Profiles, mutation.profile_id.as_entity_id())?
            .ok_or_else(|| {
                IntegrationError::Unauthorized("profile authority is unavailable".into())
            })?;
        let profile: RegisteredProfile = serde_json::from_value(record.payload)
            .map_err(|error| IntegrationError::Corrupt(error.to_string()))?;
        if profile.profile.id.as_entity_id() != &record.id || profile.revision != record.revision {
            return Err(IntegrationError::Corrupt(
                "profile authority envelope is inconsistent".into(),
            ));
        }
        if profile.profile.id != mutation.profile_id {
            return Err(IntegrationError::Unauthorized(
                "profile does not match durable authority".into(),
            ));
        }
        if matches!(
            mutation.operation,
            IntegrationOperation::Cancel
                | IntegrationOperation::Delete
                | IntegrationOperation::Export
        ) {
            return Ok(());
        }
        if !profile.enabled {
            return Err(IntegrationError::Unauthorized(
                "profile is disabled for new integration work".into(),
            ));
        }
        let key_scope = mutation
            .native_resource_key
            .split(['/', ':'])
            .next()
            .unwrap_or_default();
        let allowed = match mutation.service {
            IntegrationService::ChannelAccount => profile.authorizes_channel(key_scope),
            IntegrationService::AcpConnection | IntegrationService::HarnessRepair => true,
            IntegrationService::Plugin => profile.authorizes_plugin(key_scope),
            IntegrationService::ConnectedApp => profile.authorizes_connected_app_toolkit(key_scope),
            IntegrationService::ComputerSession | IntegrationService::ControlLease => {
                profile.authorizes_computer()
            }
            IntegrationService::Recording => profile.authorizes_recording(),
            IntegrationService::Recipe => {
                !matches!(mutation.operation, IntegrationOperation::Publish)
                    || profile.authorizes_recipe_publication()
            }
        };
        if !allowed {
            return Err(IntegrationError::Unauthorized(
                "profile policy does not authorize this integration resource".into(),
            ));
        }
        if matches!(mutation.service, IntegrationService::ComputerSession)
            && mutation.resource_id.is_none()
        {
            let current = self
                .list(
                    &mutation.profile_id,
                    Some(IntegrationService::ComputerSession),
                )?
                .resources
                .len();
            if current >= usize::from(profile.profile.service_policy.max_computers) {
                return Err(IntegrationError::Unauthorized(
                    "profile computer limit is exhausted".into(),
                ));
            }
        }
        Ok(())
    }

    fn registration(
        &self,
        service: IntegrationService,
        id: &EntityId,
    ) -> Result<Option<ServiceRegistration>, IntegrationError> {
        self.store
            .get_record(collection(service), id)?
            .map(|record| decode_registration(&record))
            .transpose()
    }

    fn idempotent_result(
        &self,
        mutation: &IntegrationMutation,
    ) -> Result<Option<IntegrationMutationResult>, IntegrationError> {
        for record in self.store.list_records(Collection::IntegrationOperations)? {
            let operation: ActiveServiceOperation = serde_json::from_value(record.payload)
                .map_err(|error| IntegrationError::Corrupt(error.to_string()))?;
            if operation.profile_id == mutation.profile_id
                && operation.idempotency_key == mutation.idempotency_key
            {
                return match self.registration(mutation.service, &operation.registration_id)? {
                    Some(registration) if registration.profile_id == mutation.profile_id => Ok(
                        Some(IntegrationMutationResult::Resource(project(registration))),
                    ),
                    None if matches!(mutation.operation, IntegrationOperation::Delete) => {
                        let (retained_operation_records, retained_audit_records) =
                            self.retained_counts(&operation.registration_id)?;
                        Ok(Some(IntegrationMutationResult::Deleted(
                            IntegrationDeletionProjection {
                                profile_id: mutation.profile_id.clone(),
                                service: mutation.service,
                                resource_id: operation.registration_id,
                                deleted_records: 1,
                                remaining_records: 0,
                                remaining_derived_indexes: None,
                                remaining_media_objects: None,
                                retained_operation_records,
                                retained_audit_records,
                                retention_reason: Some(
                                    "redacted authority audit retained for accountability".into(),
                                ),
                            },
                        )))
                    }
                    _ => Err(IntegrationError::Conflict),
                };
            }
        }
        Ok(None)
    }

    fn delete(
        &self,
        mutation: IntegrationMutation,
        registration: ServiceRegistration,
        now: UtcTimestamp,
    ) -> Result<IntegrationMutationResult, IntegrationError> {
        let operation_id = EntityId::new();
        let operation = ActiveServiceOperation {
            id: operation_id.clone(),
            registration_id: registration.id.clone(),
            profile_id: mutation.profile_id.clone(),
            action: mutation.authority.clone(),
            idempotency_key: mutation.idempotency_key,
            lifecycle: LifecycleState::Completed,
            attempt: 1,
            created_at: now,
            updated_at: now,
            safe_error: None,
        };
        operation.validate().map_err(IntegrationError::Invalid)?;
        let audit = IntegrationAuditRecord {
            profile_id: mutation.profile_id.clone(),
            service: native(mutation.service),
            resource_id: registration.id.clone(),
            operation: IntegrationOperation::Delete,
            correlation_id: mutation.authority.audit_correlation,
            outcome: AuditOutcome::Completed,
            occurred_at: now,
        };
        self.store.transact(&[
            RecordMutation::Delete {
                collection: collection(mutation.service),
                id: registration.id.clone(),
                precondition: WritePrecondition::Exact(registration.revision),
            },
            RecordMutation::Put {
                collection: Collection::IntegrationOperations,
                record: encode_record(&operation, operation_id, Revision::ZERO, now)?,
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::IntegrationAudit,
                record: encode_record(&audit, EntityId::new(), Revision::ZERO, now)?,
                precondition: WritePrecondition::Missing,
            },
        ])?;
        let remaining_records = u64::try_from(
            self.list(&mutation.profile_id, Some(mutation.service))?
                .resources
                .iter()
                .filter(|resource| resource.id == registration.id)
                .count(),
        )
        .map_err(|_| IntegrationError::Corrupt("remnant count overflowed".into()))?;
        let (retained_operation_records, retained_audit_records) =
            self.retained_counts(&registration.id)?;
        Ok(IntegrationMutationResult::Deleted(
            IntegrationDeletionProjection {
                profile_id: mutation.profile_id,
                service: mutation.service,
                resource_id: registration.id,
                deleted_records: 1,
                remaining_records,
                remaining_derived_indexes: None,
                remaining_media_objects: None,
                retained_operation_records,
                retained_audit_records,
                retention_reason: Some(
                    "redacted authority audit retained for accountability".into(),
                ),
            },
        ))
    }

    fn retained_counts(&self, resource_id: &EntityId) -> Result<(u64, u64), IntegrationError> {
        let mut operations = 0_u64;
        for record in self.store.list_records(Collection::IntegrationOperations)? {
            let operation: ActiveServiceOperation = serde_json::from_value(record.payload)
                .map_err(|error| IntegrationError::Corrupt(error.to_string()))?;
            if operation.registration_id == *resource_id {
                operations = operations.saturating_add(1);
            }
        }
        let mut audits = 0_u64;
        for record in self.store.list_records(Collection::IntegrationAudit)? {
            let audit: IntegrationAuditRecord = serde_json::from_value(record.payload)
                .map_err(|error| IntegrationError::Corrupt(error.to_string()))?;
            if audit.resource_id == *resource_id {
                audits = audits.saturating_add(1);
            }
        }
        Ok((operations, audits))
    }

    fn reconcile_service_after_restart(
        &self,
        service: IntegrationService,
    ) -> Result<(), IntegrationError> {
        let now = UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH);
        for record in self.store.list_records(collection(service))? {
            let registration = decode_registration(&record)?;
            let reconciled = registration.clone().reconcile_after_restart(now);
            if reconciled != registration {
                let revision = registration.revision.checked_next().ok_or_else(|| {
                    IntegrationError::Corrupt("integration revision is exhausted".into())
                })?;
                let mut reconciled = reconciled;
                reconciled.revision = revision;
                reconciled.validate().map_err(IntegrationError::Corrupt)?;
                self.store.transact(&[RecordMutation::Put {
                    collection: collection(service),
                    record: encode_record(&reconciled, reconciled.id.clone(), revision, now)?,
                    precondition: WritePrecondition::Exact(registration.revision),
                }])?;
            }
        }
        Ok(())
    }

    fn reconcile_operations_after_restart(&self) -> Result<(), IntegrationError> {
        let now = UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH);
        for record in self.store.list_records(Collection::IntegrationOperations)? {
            let operation: ActiveServiceOperation = serde_json::from_value(record.payload)
                .map_err(|error| IntegrationError::Corrupt(error.to_string()))?;
            let reconciled = operation.clone().reconcile_after_restart(now);
            if reconciled != operation {
                let revision = record.revision.checked_next().ok_or_else(|| {
                    IntegrationError::Corrupt("operation revision is exhausted".into())
                })?;
                reconciled.validate().map_err(IntegrationError::Corrupt)?;
                self.store.transact(&[RecordMutation::Put {
                    collection: Collection::IntegrationOperations,
                    record: encode_record(&reconciled, record.id, revision, now)?,
                    precondition: WritePrecondition::Exact(record.revision),
                }])?;
            }
        }
        Ok(())
    }

    fn availability(&self, service: IntegrationService) -> IntegrationAvailabilityProjection {
        if !self.configured(service) {
            return IntegrationAvailabilityProjection::Disabled;
        }
        self.unavailable.get(&service).map_or(
            IntegrationAvailabilityProjection::Available,
            |safe_reason| IntegrationAvailabilityProjection::Unavailable {
                safe_reason: safe_reason.clone(),
            },
        )
    }

    fn require_available(&self, service: IntegrationService) -> Result<(), IntegrationError> {
        if !self.configured(service) {
            return Err(IntegrationError::Disabled);
        }
        if let Some(reason) = self.unavailable.get(&service) {
            return Err(IntegrationError::Unavailable(reason.clone()));
        }
        Ok(())
    }

    fn configured(&self, service: IntegrationService) -> bool {
        match service {
            IntegrationService::ChannelAccount => {
                self.enablement.is_enabled(PlatformServiceGroup::Channels)
            }
            IntegrationService::AcpConnection => {
                self.enablement.is_enabled(PlatformServiceGroup::Acp)
            }
            IntegrationService::Plugin => self.enablement.is_enabled(PlatformServiceGroup::Plugins),
            IntegrationService::ConnectedApp => self
                .enablement
                .is_enabled(PlatformServiceGroup::ConnectedApps),
            IntegrationService::ComputerSession | IntegrationService::ControlLease => {
                self.enablement.is_enabled(PlatformServiceGroup::Computers)
            }
            IntegrationService::Recording | IntegrationService::Recipe => {
                self.enablement.is_enabled(PlatformServiceGroup::Teaching)
            }
            IntegrationService::HarnessRepair => true,
        }
    }
}

fn encode_record<T: Serialize>(
    value: &T,
    id: EntityId,
    revision: Revision,
    updated_at: UtcTimestamp,
) -> Result<VersionedRecord, IntegrationError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id,
        revision,
        updated_at,
        payload: serde_json::to_value(value)
            .map_err(|error| IntegrationError::Corrupt(error.to_string()))?,
    })
}

fn decode_registration(record: &VersionedRecord) -> Result<ServiceRegistration, IntegrationError> {
    let registration: ServiceRegistration = serde_json::from_value(record.payload.clone())
        .map_err(|error| IntegrationError::Corrupt(error.to_string()))?;
    if registration.id != record.id || registration.revision != record.revision {
        return Err(IntegrationError::Corrupt(
            "registration envelope does not match its durable record".into(),
        ));
    }
    registration.validate().map_err(IntegrationError::Corrupt)?;
    Ok(registration)
}

fn project(registration: ServiceRegistration) -> IntegrationResourceProjection {
    IntegrationResourceProjection {
        id: registration.id,
        profile_id: registration.profile_id,
        owning_session_id: registration.owning_session_id,
        service: protocol(registration.service),
        native_resource_key: registration.native_resource_key,
        display_label: registration.display_label.to_string(),
        lifecycle: registration.lifecycle,
        cancellation_id: registration.cancellation_id,
        audit_correlation: registration.audit_correlation,
        bounds: registration.bounds,
        controls: registration
            .controls
            .into_iter()
            .map(|control| match control {
                ServiceControl::Restart => IntegrationControl::Restart,
                ServiceControl::Cancel => IntegrationControl::Cancel,
                ServiceControl::Export => IntegrationControl::Export,
                ServiceControl::Delete => IntegrationControl::Delete,
            })
            .collect(),
        safe_error: registration.safe_error.map(|error| error.to_string()),
        revision: registration.revision,
        created_at: registration.created_at,
        updated_at: registration.updated_at,
    }
}

fn service_controls(lifecycle: LifecycleState) -> BTreeSet<ServiceControl> {
    let mut controls = [ServiceControl::Export, ServiceControl::Delete]
        .into_iter()
        .collect::<BTreeSet<_>>();
    if matches!(lifecycle, LifecycleState::Pending | LifecycleState::Paused) {
        controls.insert(ServiceControl::Restart);
    }
    if matches!(lifecycle, LifecycleState::Pending | LifecycleState::Active) {
        controls.insert(ServiceControl::Cancel);
    }
    controls
}

const fn all_services() -> [IntegrationService; 9] {
    [
        IntegrationService::ChannelAccount,
        IntegrationService::AcpConnection,
        IntegrationService::Plugin,
        IntegrationService::ConnectedApp,
        IntegrationService::ComputerSession,
        IntegrationService::ControlLease,
        IntegrationService::Recording,
        IntegrationService::Recipe,
        IntegrationService::HarnessRepair,
    ]
}

const fn native(service: IntegrationService) -> ExternalServiceKind {
    match service {
        IntegrationService::ChannelAccount => ExternalServiceKind::ChannelAccount,
        IntegrationService::AcpConnection => ExternalServiceKind::AcpConnection,
        IntegrationService::Plugin => ExternalServiceKind::Plugin,
        IntegrationService::ConnectedApp => ExternalServiceKind::ConnectedApp,
        IntegrationService::ComputerSession => ExternalServiceKind::ComputerSession,
        IntegrationService::ControlLease => ExternalServiceKind::ControlLease,
        IntegrationService::Recording => ExternalServiceKind::Recording,
        IntegrationService::Recipe => ExternalServiceKind::Recipe,
        IntegrationService::HarnessRepair => ExternalServiceKind::HarnessRepair,
    }
}

const fn protocol(service: ExternalServiceKind) -> IntegrationService {
    match service {
        ExternalServiceKind::ChannelAccount => IntegrationService::ChannelAccount,
        ExternalServiceKind::AcpConnection => IntegrationService::AcpConnection,
        ExternalServiceKind::Plugin => IntegrationService::Plugin,
        ExternalServiceKind::ConnectedApp => IntegrationService::ConnectedApp,
        ExternalServiceKind::ComputerSession => IntegrationService::ComputerSession,
        ExternalServiceKind::ControlLease => IntegrationService::ControlLease,
        ExternalServiceKind::Recording => IntegrationService::Recording,
        ExternalServiceKind::Recipe => IntegrationService::Recipe,
        ExternalServiceKind::HarnessRepair => IntegrationService::HarnessRepair,
    }
}

const fn collection(service: IntegrationService) -> Collection {
    match service {
        IntegrationService::ChannelAccount => Collection::ChannelAccounts,
        IntegrationService::AcpConnection => Collection::AcpSessions,
        IntegrationService::Plugin => Collection::PluginRegistry,
        IntegrationService::ConnectedApp => Collection::ConnectedApps,
        IntegrationService::ComputerSession => Collection::ComputerSessions,
        IntegrationService::ControlLease => Collection::ControlLeases,
        IntegrationService::Recording => Collection::Demonstrations,
        IntegrationService::Recipe => Collection::TaskRecipes,
        IntegrationService::HarnessRepair => Collection::HarnessRepairs,
    }
}

const fn creates_resource(operation: IntegrationOperation) -> bool {
    matches!(
        operation,
        IntegrationOperation::Connect
            | IntegrationOperation::Install
            | IntegrationOperation::Start
            | IntegrationOperation::TakeControl
            | IntegrationOperation::StartRecording
    )
}

const fn operation_supported(service: IntegrationService, operation: IntegrationOperation) -> bool {
    match operation {
        IntegrationOperation::Connect => matches!(
            service,
            IntegrationService::ChannelAccount
                | IntegrationService::AcpConnection
                | IntegrationService::ConnectedApp
        ),
        IntegrationOperation::Install => matches!(service, IntegrationService::Plugin),
        IntegrationOperation::Start => matches!(service, IntegrationService::ComputerSession),
        IntegrationOperation::TakeControl | IntegrationOperation::ReleaseControl => {
            matches!(service, IntegrationService::ControlLease)
        }
        IntegrationOperation::StartRecording | IntegrationOperation::StopRecording => {
            matches!(service, IntegrationService::Recording)
        }
        IntegrationOperation::Publish => matches!(service, IntegrationService::Recipe),
        IntegrationOperation::Reverse => matches!(service, IntegrationService::HarnessRepair),
        IntegrationOperation::Configure
        | IntegrationOperation::Test
        | IntegrationOperation::Pause
        | IntegrationOperation::Resume
        | IntegrationOperation::Stop
        | IntegrationOperation::Cancel
        | IntegrationOperation::Delete
        | IntegrationOperation::Export => true,
    }
}

fn desired_lifecycle(
    operation: IntegrationOperation,
    existing: Option<&ServiceRegistration>,
) -> Result<LifecycleState, IntegrationError> {
    match operation {
        IntegrationOperation::Pause | IntegrationOperation::ReleaseControl => {
            if existing.is_some_and(|record| {
                matches!(
                    record.lifecycle,
                    LifecycleState::Active | LifecycleState::Pending
                )
            }) {
                Ok(LifecycleState::Paused)
            } else {
                Err(IntegrationError::Conflict)
            }
        }
        IntegrationOperation::Stop | IntegrationOperation::StopRecording => {
            Ok(LifecycleState::Completed)
        }
        IntegrationOperation::Cancel => Ok(LifecycleState::Cancelled),
        IntegrationOperation::Export => existing
            .map(|record| record.lifecycle)
            .ok_or(IntegrationError::NotFound),
        IntegrationOperation::Delete => Err(IntegrationError::Invalid(
            "delete lifecycle is handled atomically".into(),
        )),
        IntegrationOperation::Connect
        | IntegrationOperation::Configure
        | IntegrationOperation::Test
        | IntegrationOperation::Install
        | IntegrationOperation::Start
        | IntegrationOperation::Resume
        | IntegrationOperation::TakeControl
        | IntegrationOperation::StartRecording
        | IntegrationOperation::Publish
        | IntegrationOperation::Reverse => Ok(LifecycleState::Pending),
    }
}

const fn required_authority(
    service: IntegrationService,
    operation: IntegrationOperation,
) -> (Capability, ActionRisk) {
    match operation {
        IntegrationOperation::Delete => (Capability::Delete, ActionRisk::Delete),
        IntegrationOperation::Export | IntegrationOperation::Test => {
            (Capability::Read, ActionRisk::ReadOnly)
        }
        IntegrationOperation::Connect | IntegrationOperation::Configure => match service {
            IntegrationService::AcpConnection => {
                (Capability::AcpConnect, ActionRisk::AccountChange)
            }
            _ => (Capability::AccountChange, ActionRisk::AccountChange),
        },
        IntegrationOperation::Install => (Capability::PluginInstall, ActionRisk::AccountChange),
        IntegrationOperation::TakeControl => (
            Capability::ComputerControl,
            ActionRisk::IrreversibleComputerInput,
        ),
        IntegrationOperation::StartRecording | IntegrationOperation::StopRecording => (
            Capability::DemonstrationRecord,
            ActionRisk::ReversibleLocalWrite,
        ),
        IntegrationOperation::Publish => {
            (Capability::RecipePublish, ActionRisk::ExternalCommunication)
        }
        IntegrationOperation::Reverse => {
            (Capability::HarnessReverse, ActionRisk::ReversibleLocalWrite)
        }
        IntegrationOperation::Start
        | IntegrationOperation::Pause
        | IntegrationOperation::Resume
        | IntegrationOperation::Stop
        | IntegrationOperation::Cancel
        | IntegrationOperation::ReleaseControl => {
            (Capability::LocalWrite, ActionRisk::ReversibleLocalWrite)
        }
    }
}

const fn admitted_operation_state(
    operation: IntegrationOperation,
) -> (LifecycleState, AuditOutcome) {
    match operation {
        IntegrationOperation::Cancel => (LifecycleState::Cancelled, AuditOutcome::Cancelled),
        IntegrationOperation::Pause
        | IntegrationOperation::Stop
        | IntegrationOperation::Export
        | IntegrationOperation::ReleaseControl
        | IntegrationOperation::StopRecording => {
            (LifecycleState::Completed, AuditOutcome::Completed)
        }
        IntegrationOperation::Connect
        | IntegrationOperation::Configure
        | IntegrationOperation::Test
        | IntegrationOperation::Install
        | IntegrationOperation::Start
        | IntegrationOperation::Resume
        | IntegrationOperation::Delete
        | IntegrationOperation::TakeControl
        | IntegrationOperation::StartRecording
        | IntegrationOperation::Publish
        | IntegrationOperation::Reverse => (LifecycleState::Pending, AuditOutcome::Requested),
    }
}

const fn service_bounds(_service: IntegrationService) -> ResourceBounds {
    ResourceBounds {
        max_concurrency: 4,
        max_duration_ms: 15 * 60 * 1_000,
        max_cpu_time_ms: 10 * 60 * 1_000,
        max_retries: 3,
        max_input_bytes: 16 * 1_024 * 1_024,
        max_output_bytes: 64 * 1_024 * 1_024,
        max_memory_bytes: 2 * 1_024 * 1_024 * 1_024,
        max_disk_bytes: 8 * 1_024 * 1_024 * 1_024,
        max_events_per_minute: 1_000,
    }
}
