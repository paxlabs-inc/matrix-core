#![forbid(unsafe_code)]
#![allow(clippy::missing_errors_doc)]

use std::collections::BTreeSet;
use std::path::{Component, Path, PathBuf};
use std::{fs, io};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, ComputerId, GrantId, ProfileId, Revision, SchemaVersion, StableKey,
    UtcTimestamp,
};
pub use keith_configuration::{
    AgentProfile, AgentProfilePresentation, AutonomyMode, ComputerPolicy, ModelRoute,
    ModelSelection, NotificationSettings, ProfileAutonomy, ProfileLifecycleState,
    RefinementSettings, ThinkingLevel, ToolPermission,
};
use keith_state_store_core::{
    Collection, ProfileRepository, RecordMutation, StateRecordRepository, VersionedRecord,
    WritePrecondition,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileResources {
    pub workspace_root: PathBuf,
    pub memory_root: PathBuf,
    pub schedule_root: PathBuf,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RegisteredProfile {
    pub profile: AgentProfile,
    pub resources: ProfileResources,
    pub enabled: bool,
    pub authorized_callers: BTreeSet<String>,
    pub revision: Revision,
    pub updated_at: UtcTimestamp,
}

impl RegisteredProfile {
    pub fn id(&self) -> &ProfileId {
        &self.profile.id
    }

    /// Profile, client, channel, plugin, MCP, skill, kernel, and model state is
    /// never installation authority for self-evolution.
    #[must_use]
    pub const fn can_enable_self_evolution(&self) -> bool {
        false
    }
}

pub const MAX_COMPUTER_CREDENTIAL_GRANTS_PER_PROFILE: usize = 256;
pub const MAX_COMPUTER_CREDENTIAL_TASKS: usize = 128;
pub const MAX_COMPUTER_CREDENTIAL_AUTHORITIES: usize = 8;
pub const MAX_COMPUTER_CREDENTIAL_LABEL_BYTES: usize = 256;
pub const MAX_COMPUTER_CREDENTIAL_ORIGIN_BYTES: usize = 2_048;
const COMPUTER_CREDENTIAL_GRANT_RECORD_KIND: &str = "computer_credential_grant_v1";

/// Stable, non-secret identity of a credential managed by the credential store. It deliberately
/// mirrors only `CredentialRef` metadata; credential bytes cannot be represented here.
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerCredentialReference {
    pub id: StableKey,
    pub name: String,
    pub owner_kind: String,
    pub owner_id: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "scope", content = "computer_id")]
pub enum ComputerCredentialComputerScope {
    Exact(ComputerId),
    ProfileComputer,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ComputerCredentialTargetKind {
    FocusedField,
    CredentialBroker,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ComputerCredentialUseAuthority {
    Owner,
    ProfileAgent,
    OwnedRoutine,
    OwnedChild,
}

/// A set-valued ceiling is used because actor categories are not safely linearly ordered.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerCredentialAuthorityCeiling {
    pub allowed: BTreeSet<ComputerCredentialUseAuthority>,
    pub allowed_task_keys: BTreeSet<StableKey>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerCredentialGrant {
    pub version: SchemaVersion,
    pub grant_id: GrantId,
    pub owner_profile_id: ProfileId,
    pub credential: ComputerCredentialReference,
    pub computer: ComputerCredentialComputerScope,
    pub target_kind: ComputerCredentialTargetKind,
    pub target_id: Option<String>,
    pub https_origin: String,
    pub frame_https_origin: String,
    pub authority: ComputerCredentialAuthorityCeiling,
    pub owner_approval_id: StableKey,
    pub owner_approved_by: String,
    pub owner_approved_at: UtcTimestamp,
    pub expires_at: Option<UtcTimestamp>,
    pub revoked_at: Option<UtcTimestamp>,
    pub revoked_by: Option<String>,
    pub revocation_reason: Option<String>,
    pub revision: Revision,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerCredentialGrantLookup {
    pub grant_id: GrantId,
    pub expected_revision: Revision,
    pub owner_profile_id: ProfileId,
    pub computer_id: ComputerId,
    pub credential: ComputerCredentialReference,
    pub target_kind: ComputerCredentialTargetKind,
    pub target_id: Option<String>,
    pub https_origin: String,
    pub frame_https_origin: String,
    pub task_key: StableKey,
    pub authority: ComputerCredentialUseAuthority,
    pub now: UtcTimestamp,
}

/// Non-widening result safe to pass to computer and credential boundaries. It contains identity
/// and current authorization evidence only, never a credential value.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerCredentialGrantProjection {
    pub grant_id: GrantId,
    pub grant_revision: Revision,
    pub owner_profile_id: ProfileId,
    pub credential: ComputerCredentialReference,
    pub computer_id: ComputerId,
    pub target_kind: ComputerCredentialTargetKind,
    pub target_id: Option<String>,
    pub https_origin: String,
    pub frame_https_origin: String,
    pub task_key: StableKey,
    pub authority: ComputerCredentialUseAuthority,
    pub owner_approval_id: StableKey,
    pub owner_approved_at: UtcTimestamp,
    pub expires_at: Option<UtcTimestamp>,
}

/// Resolves the current profile-to-computer binding from authoritative computer state. Grant
/// callers cannot assert that an arbitrary computer is a profile's current computer.
pub trait ComputerCredentialComputerAuthorizer {
    fn is_current_profile_computer(
        &self,
        owner_profile_id: &ProfileId,
        computer_id: &ComputerId,
        now: UtcTimestamp,
    ) -> bool;
}

/// Fail-closed resolver for callers that have not wired authoritative computer state.
pub struct DenyAllProfileComputers;

impl ComputerCredentialComputerAuthorizer for DenyAllProfileComputers {
    fn is_current_profile_computer(
        &self,
        _owner_profile_id: &ProfileId,
        _computer_id: &ComputerId,
        _now: UtcTimestamp,
    ) -> bool {
        false
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct StoredComputerCredentialGrant {
    kind: String,
    grant: ComputerCredentialGrant,
}

#[derive(Debug, Error)]
pub enum ComputerCredentialGrantError {
    #[error("computer credential grant is invalid: {0}")]
    Invalid(&'static str),
    #[error("computer credential grant repository failed: {0}")]
    Repository(String),
    #[error("computer credential grant encoding failed: {0}")]
    Serialize(#[from] serde_json::Error),
    #[error("computer credential grant was not found")]
    Missing,
    #[error("computer credential grant already exists")]
    AlreadyExists,
    #[error("computer credential grant revision is stale")]
    Stale,
    #[error("computer credential grant is revoked, expired, or does not authorize this use")]
    Denied,
    #[error("computer credential grant collection exceeds its profile bound")]
    LimitExceeded,
}

pub struct ComputerCredentialGrantRegistry<R> {
    repository: R,
}

impl<R> ComputerCredentialGrantRegistry<R>
where
    R: StateRecordRepository,
    R::Error: std::fmt::Display,
{
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    pub fn create(
        &self,
        grant: ComputerCredentialGrant,
    ) -> Result<ComputerCredentialGrant, ComputerCredentialGrantError> {
        validate_computer_credential_grant(&grant)?;
        if grant.revision != Revision::ZERO
            || grant.updated_at != grant.created_at
            || grant.owner_approved_at != grant.created_at
            || grant.revoked_at.is_some()
        {
            return Err(ComputerCredentialGrantError::Invalid(
                "new grant must begin active at revision zero with contemporaneous approval",
            ));
        }
        if self.get(&grant.grant_id)?.is_some() {
            return Err(ComputerCredentialGrantError::AlreadyExists);
        }
        if self.list_for_profile(&grant.owner_profile_id)?.len()
            >= MAX_COMPUTER_CREDENTIAL_GRANTS_PER_PROFILE
        {
            return Err(ComputerCredentialGrantError::LimitExceeded);
        }
        self.repository
            .transact(&[RecordMutation::Put {
                collection: Collection::ResourceGovernance,
                record: encode_computer_credential_grant(&grant)?,
                precondition: WritePrecondition::Missing,
            }])
            .map_err(computer_credential_repository_error)?;
        Ok(grant)
    }

    pub fn replace(
        &self,
        mut replacement: ComputerCredentialGrant,
        expected_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<ComputerCredentialGrant, ComputerCredentialGrantError> {
        let current = self
            .get(&replacement.grant_id)?
            .ok_or(ComputerCredentialGrantError::Missing)?;
        if current.revision != expected_revision
            || replacement.owner_profile_id != current.owner_profile_id
            || replacement.credential != current.credential
            || replacement.created_at != current.created_at
            || replacement.owner_approval_id != current.owner_approval_id
            || replacement.owner_approved_by != current.owner_approved_by
            || replacement.owner_approved_at != current.owner_approved_at
            || replacement.version != current.version
            || now < current.updated_at
            || current.revoked_at.is_some()
            || !computer_credential_replacement_is_non_widening(&current, &replacement)
        {
            return Err(ComputerCredentialGrantError::Stale);
        }
        replacement.revision = expected_revision
            .checked_next()
            .ok_or(ComputerCredentialGrantError::Stale)?;
        replacement.updated_at = now;
        validate_computer_credential_grant(&replacement)?;
        self.repository
            .transact(&[RecordMutation::Put {
                collection: Collection::ResourceGovernance,
                record: encode_computer_credential_grant(&replacement)?,
                precondition: WritePrecondition::Exact(expected_revision),
            }])
            .map_err(computer_credential_repository_error)?;
        Ok(replacement)
    }

    pub fn revoke(
        &self,
        grant_id: &GrantId,
        owner_profile_id: &ProfileId,
        expected_revision: Revision,
        revoked_by: String,
        reason: String,
        now: UtcTimestamp,
    ) -> Result<ComputerCredentialGrant, ComputerCredentialGrantError> {
        validate_computer_credential_label(&revoked_by)?;
        validate_computer_credential_label(&reason)?;
        let mut grant = self
            .get(grant_id)?
            .ok_or(ComputerCredentialGrantError::Missing)?;
        if &grant.owner_profile_id != owner_profile_id {
            return Err(ComputerCredentialGrantError::Denied);
        }
        if grant.revision != expected_revision
            || grant.revoked_at.is_some()
            || now < grant.updated_at
        {
            return Err(ComputerCredentialGrantError::Stale);
        }
        grant.revision = expected_revision
            .checked_next()
            .ok_or(ComputerCredentialGrantError::Stale)?;
        grant.revoked_at = Some(now);
        grant.revoked_by = Some(revoked_by);
        grant.revocation_reason = Some(reason);
        grant.updated_at = now;
        validate_computer_credential_grant(&grant)?;
        self.repository
            .transact(&[RecordMutation::Put {
                collection: Collection::ResourceGovernance,
                record: encode_computer_credential_grant(&grant)?,
                precondition: WritePrecondition::Exact(expected_revision),
            }])
            .map_err(computer_credential_repository_error)?;
        Ok(grant)
    }

    pub fn get(
        &self,
        grant_id: &GrantId,
    ) -> Result<Option<ComputerCredentialGrant>, ComputerCredentialGrantError> {
        let Some(record) = self
            .repository
            .get_record(Collection::ResourceGovernance, grant_id.as_entity_id())
            .map_err(computer_credential_repository_error)?
        else {
            return Ok(None);
        };
        if record
            .payload
            .get("kind")
            .and_then(serde_json::Value::as_str)
            != Some(COMPUTER_CREDENTIAL_GRANT_RECORD_KIND)
        {
            return Ok(None);
        }
        decode_computer_credential_grant(record).map(Some)
    }

    pub fn list_for_profile(
        &self,
        profile_id: &ProfileId,
    ) -> Result<Vec<ComputerCredentialGrant>, ComputerCredentialGrantError> {
        let mut grants = Vec::new();
        for record in self
            .repository
            .list_records(Collection::ResourceGovernance)
            .map_err(computer_credential_repository_error)?
        {
            if record
                .payload
                .get("kind")
                .and_then(serde_json::Value::as_str)
                != Some(COMPUTER_CREDENTIAL_GRANT_RECORD_KIND)
            {
                continue;
            }
            let grant = decode_computer_credential_grant(record)?;
            if &grant.owner_profile_id == profile_id {
                grants.push(grant);
            }
        }
        if grants.len() > MAX_COMPUTER_CREDENTIAL_GRANTS_PER_PROFILE {
            return Err(ComputerCredentialGrantError::LimitExceeded);
        }
        grants.sort_by(|left, right| left.grant_id.cmp(&right.grant_id));
        Ok(grants)
    }

    /// Resolves one exact current grant. Missing grants and every mismatch deny; no profile or
    /// computer policy is interpreted as an implicit grant.
    pub fn authorize<A>(
        &self,
        request: &ComputerCredentialGrantLookup,
        computer_authorizer: &A,
    ) -> Result<ComputerCredentialGrantProjection, ComputerCredentialGrantError>
    where
        A: ComputerCredentialComputerAuthorizer,
    {
        validate_computer_credential_lookup(request)?;
        let grant = self
            .get(&request.grant_id)?
            .ok_or(ComputerCredentialGrantError::Denied)?;
        let computer_matches = match &grant.computer {
            ComputerCredentialComputerScope::Exact(computer_id) => {
                computer_id == &request.computer_id
            }
            ComputerCredentialComputerScope::ProfileComputer => computer_authorizer
                .is_current_profile_computer(
                    &request.owner_profile_id,
                    &request.computer_id,
                    request.now,
                ),
        };
        let active = grant.revoked_at.is_none()
            && grant
                .expires_at
                .is_none_or(|expires_at| request.now < expires_at);
        let exact = grant.revision == request.expected_revision
            && grant.owner_profile_id == request.owner_profile_id
            && grant.credential == request.credential
            && computer_matches
            && grant.target_kind == request.target_kind
            && grant.target_id == request.target_id
            && grant.https_origin == request.https_origin
            && grant.frame_https_origin == request.frame_https_origin
            && grant.authority.allowed.contains(&request.authority)
            && grant
                .authority
                .allowed_task_keys
                .contains(&request.task_key);
        if !active || !exact {
            return Err(ComputerCredentialGrantError::Denied);
        }
        Ok(ComputerCredentialGrantProjection {
            grant_id: grant.grant_id,
            grant_revision: grant.revision,
            owner_profile_id: grant.owner_profile_id,
            credential: grant.credential,
            computer_id: request.computer_id.clone(),
            target_kind: grant.target_kind,
            target_id: grant.target_id,
            https_origin: grant.https_origin,
            frame_https_origin: grant.frame_https_origin,
            task_key: request.task_key.clone(),
            authority: request.authority,
            owner_approval_id: grant.owner_approval_id,
            owner_approved_at: grant.owner_approved_at,
            expires_at: grant.expires_at,
        })
    }
}

fn validate_computer_credential_grant(
    grant: &ComputerCredentialGrant,
) -> Result<(), ComputerCredentialGrantError> {
    if grant.version.major != CURRENT_SCHEMA_VERSION.major
        || grant.version.minor > CURRENT_SCHEMA_VERSION.minor
        || grant.created_at > grant.owner_approved_at
        || grant.owner_approved_at > grant.updated_at
        || grant
            .expires_at
            .is_some_and(|expires_at| expires_at <= grant.created_at)
        || grant.authority.allowed.is_empty()
        || grant.authority.allowed.len() > MAX_COMPUTER_CREDENTIAL_AUTHORITIES
        || grant.authority.allowed_task_keys.is_empty()
        || grant.authority.allowed_task_keys.len() > MAX_COMPUTER_CREDENTIAL_TASKS
    {
        return Err(ComputerCredentialGrantError::Invalid(
            "grant schema, time, authority, or task bounds are invalid",
        ));
    }
    validate_computer_credential_reference(&grant.credential)?;
    validate_computer_credential_origin(&grant.https_origin)?;
    validate_computer_credential_origin(&grant.frame_https_origin)?;
    validate_optional_computer_credential_label(grant.target_id.as_deref())?;
    validate_computer_credential_label(&grant.owner_approved_by)?;
    let revoked = grant.revoked_at.is_some();
    if revoked != grant.revoked_by.is_some()
        || revoked != grant.revocation_reason.is_some()
        || grant
            .revoked_at
            .is_some_and(|revoked_at| revoked_at < grant.created_at)
    {
        return Err(ComputerCredentialGrantError::Invalid(
            "grant revocation metadata is inconsistent",
        ));
    }
    if let Some(revoked_by) = &grant.revoked_by {
        validate_computer_credential_label(revoked_by)?;
    }
    if let Some(reason) = &grant.revocation_reason {
        validate_computer_credential_label(reason)?;
    }
    Ok(())
}

fn validate_computer_credential_lookup(
    request: &ComputerCredentialGrantLookup,
) -> Result<(), ComputerCredentialGrantError> {
    validate_computer_credential_reference(&request.credential)?;
    validate_computer_credential_origin(&request.https_origin)?;
    validate_computer_credential_origin(&request.frame_https_origin)?;
    validate_optional_computer_credential_label(request.target_id.as_deref())
}

fn validate_computer_credential_reference(
    reference: &ComputerCredentialReference,
) -> Result<(), ComputerCredentialGrantError> {
    for value in [&reference.name, &reference.owner_kind, &reference.owner_id] {
        validate_computer_credential_label(value)?;
    }
    Ok(())
}

fn validate_computer_credential_label(value: &str) -> Result<(), ComputerCredentialGrantError> {
    if value.trim().is_empty()
        || value.len() > MAX_COMPUTER_CREDENTIAL_LABEL_BYTES
        || value.contains('\0')
        || value.chars().any(char::is_control)
    {
        return Err(ComputerCredentialGrantError::Invalid(
            "grant label is empty, malformed, or over bound",
        ));
    }
    Ok(())
}

fn validate_optional_computer_credential_label(
    value: Option<&str>,
) -> Result<(), ComputerCredentialGrantError> {
    if let Some(value) = value {
        validate_computer_credential_label(value)?;
    }
    Ok(())
}

fn validate_computer_credential_origin(origin: &str) -> Result<(), ComputerCredentialGrantError> {
    let Some(authority) = origin.strip_prefix("https://") else {
        return Err(ComputerCredentialGrantError::Invalid(
            "credential origin must use canonical HTTPS",
        ));
    };
    if authority.is_empty()
        || origin.len() > MAX_COMPUTER_CREDENTIAL_ORIGIN_BYTES
        || !origin.is_ascii()
        || authority.contains(['/', '?', '#', '@'])
        || authority.bytes().any(|byte| {
            !(byte.is_ascii_lowercase()
                || byte.is_ascii_digit()
                || matches!(byte, b'.' | b'-' | b':' | b'[' | b']'))
        })
    {
        return Err(ComputerCredentialGrantError::Invalid(
            "credential origin is not an exact bounded HTTPS origin",
        ));
    }
    let host_and_port_valid = if let Some(ipv6) = authority.strip_prefix('[') {
        ipv6.split_once(']').is_some_and(|(host, remainder)| {
            !host.is_empty()
                && host
                    .bytes()
                    .all(|byte| byte.is_ascii_hexdigit() || byte == b':')
                && valid_origin_port(remainder)
        })
    } else {
        let (host, port) = authority
            .split_once(':')
            .map_or((authority, ""), |(host, port)| (host, port));
        !host.is_empty()
            && !matches!(host.as_bytes().first().copied(), Some(b'.' | b'-'))
            && !matches!(host.as_bytes().last().copied(), Some(b'.' | b'-'))
            && !host.contains("..")
            && !host
                .split('.')
                .any(|label| label.is_empty() || label.starts_with('-') || label.ends_with('-'))
            && (port.is_empty() || valid_origin_port(&format!(":{port}")))
            && !port.contains(':')
    };
    if !host_and_port_valid {
        return Err(ComputerCredentialGrantError::Invalid(
            "credential origin authority is malformed",
        ));
    }
    Ok(())
}

fn valid_origin_port(value: &str) -> bool {
    if value.is_empty() {
        return true;
    }
    value
        .strip_prefix(':')
        .filter(|port| !port.is_empty() && port.len() <= 5)
        .and_then(|port| port.parse::<u16>().ok())
        .is_some_and(|port| port != 0)
}

fn computer_credential_replacement_is_non_widening(
    current: &ComputerCredentialGrant,
    replacement: &ComputerCredentialGrant,
) -> bool {
    let expiry_not_wider = match (current.expires_at, replacement.expires_at) {
        (None, _) => true,
        (Some(current), Some(replacement)) => replacement <= current,
        (Some(_), None) => false,
    };
    replacement.computer == current.computer
        && replacement.version == current.version
        && replacement.target_kind == current.target_kind
        && replacement.target_id == current.target_id
        && replacement.https_origin == current.https_origin
        && replacement.frame_https_origin == current.frame_https_origin
        && replacement
            .authority
            .allowed
            .is_subset(&current.authority.allowed)
        && replacement
            .authority
            .allowed_task_keys
            .is_subset(&current.authority.allowed_task_keys)
        && expiry_not_wider
        && replacement.revoked_at.is_none()
        && replacement.revoked_by.is_none()
        && replacement.revocation_reason.is_none()
}

fn encode_computer_credential_grant(
    grant: &ComputerCredentialGrant,
) -> Result<VersionedRecord, ComputerCredentialGrantError> {
    validate_computer_credential_grant(grant)?;
    Ok(VersionedRecord {
        version: grant.version,
        id: grant.grant_id.as_entity_id().clone(),
        revision: grant.revision,
        updated_at: grant.updated_at,
        payload: serde_json::to_value(StoredComputerCredentialGrant {
            kind: COMPUTER_CREDENTIAL_GRANT_RECORD_KIND.into(),
            grant: grant.clone(),
        })?,
    })
}

fn decode_computer_credential_grant(
    record: VersionedRecord,
) -> Result<ComputerCredentialGrant, ComputerCredentialGrantError> {
    let stored: StoredComputerCredentialGrant = serde_json::from_value(record.payload)?;
    if stored.kind != COMPUTER_CREDENTIAL_GRANT_RECORD_KIND {
        return Err(ComputerCredentialGrantError::Invalid(
            "resource-governance record kind is not a credential grant",
        ));
    }
    validate_computer_credential_grant(&stored.grant)?;
    if record.version != stored.grant.version
        || &record.id != stored.grant.grant_id.as_entity_id()
        || record.revision != stored.grant.revision
        || record.updated_at != stored.grant.updated_at
    {
        return Err(ComputerCredentialGrantError::Invalid(
            "grant record envelope does not match its strict payload",
        ));
    }
    Ok(stored.grant)
}

fn computer_credential_repository_error(
    error: impl std::fmt::Display,
) -> ComputerCredentialGrantError {
    ComputerCredentialGrantError::Repository(error.to_string())
}

#[derive(Debug, Error)]
pub enum ProfileError {
    #[error("profile {0} already exists")]
    AlreadyExists(ProfileId),
    #[error("profile {0} does not exist")]
    Missing(ProfileId),
    #[error("profile revision is stale")]
    Stale,
    #[error("profile is invalid: {0}")]
    Invalid(String),
    #[error("profile resource I/O failed: {0}")]
    Io(#[from] io::Error),
    #[error("profile repository failed: {0}")]
    Repository(String),
    #[error("profile serialization failed: {0}")]
    Serialize(#[from] serde_json::Error),
    #[error("profile lifecycle transition is invalid")]
    InvalidTransition,
    #[error("profile deletion was not explicitly confirmed for the current profile")]
    DeleteNotConfirmed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum OwnedWorkDisposition {
    Cancel,
    TransferTo(ProfileId),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentLifecycleAudit {
    pub sequence: u64,
    pub actor: String,
    pub action: String,
    pub revision: Revision,
    pub occurred_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentLifecycleRecord {
    pub profile: RegisteredProfile,
    pub presentation: AgentProfilePresentation,
    pub audit: Vec<AgentLifecycleAudit>,
    pub deletion: Option<AgentDeletionTombstone>,
}

pub const MAX_PEER_AUTHORITY_ENTRIES: usize = 128;
pub const MAX_PEER_AUTHORITY_LABEL_BYTES: usize = 128;
pub const MAX_PEER_AUTONOMY_BOUND: u16 = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerApprovalRequirement {
    Always,
    ConsequentialActions,
    None,
}

impl PeerApprovalRequirement {
    const fn intersect(self, other: Self) -> Self {
        match (self, other) {
            (Self::Always, _) | (_, Self::Always) => Self::Always,
            (Self::ConsequentialActions, _) | (_, Self::ConsequentialActions) => {
                Self::ConsequentialActions
            }
            (Self::None, Self::None) => Self::None,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerAutonomyCeiling {
    pub enabled: bool,
    pub max_children: u16,
    pub max_depth: u16,
    pub max_parallel_actions: u16,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerComputerCeiling {
    pub enabled: bool,
    pub allow_downloads: bool,
    pub allow_uploads: bool,
    pub allow_consequential_actions: bool,
    pub max_idle_seconds: u32,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerAuthorityPolicy {
    pub profile_id: ProfileId,
    pub revision: Revision,
    pub enabled: bool,
    pub tools: BTreeSet<String>,
    pub models: BTreeSet<String>,
    pub network_scopes: BTreeSet<String>,
    pub filesystem_scopes: BTreeSet<String>,
    pub allow_credentials: bool,
    pub approvals: PeerApprovalRequirement,
    pub autonomy: PeerAutonomyCeiling,
    pub computer: PeerComputerCeiling,
    pub allow_self_evolution: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerAuthorityProjection {
    pub sender_profile_id: ProfileId,
    pub receiver_profile_id: ProfileId,
    pub sender_revision: Revision,
    pub receiver_revision: Revision,
    pub tools: BTreeSet<String>,
    pub models: BTreeSet<String>,
    pub network_scopes: BTreeSet<String>,
    pub filesystem_scopes: BTreeSet<String>,
    pub allow_credentials: bool,
    pub approvals: PeerApprovalRequirement,
    pub autonomy: PeerAutonomyCeiling,
    pub computer: PeerComputerCeiling,
    pub allow_self_evolution: bool,
}

impl PeerAuthorityProjection {
    pub fn positive_capability_labels(&self) -> BTreeSet<String> {
        let mut labels = BTreeSet::new();
        labels.extend(self.tools.iter().map(|value| format!("tool:{value}")));
        labels.extend(self.models.iter().map(|value| format!("model:{value}")));
        labels.extend(
            self.network_scopes
                .iter()
                .map(|value| format!("network:{value}")),
        );
        labels.extend(
            self.filesystem_scopes
                .iter()
                .map(|value| format!("filesystem:{value}")),
        );
        if self.allow_credentials {
            labels.insert("credentials:delegated_use".to_owned());
        }
        if self.computer.enabled {
            labels.insert("computer:use".to_owned());
        }
        if self.allow_self_evolution {
            labels.insert("self_evolution:peer_use".to_owned());
        }
        labels
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum PeerAuthorityProjectionError {
    SameProfile,
    SenderDisabled(ProfileId),
    ReceiverDisabled(ProfileId),
    TooManyEntries,
    InvalidCapabilityLabel,
    InvalidAutonomyBound,
    InvalidComputerBound,
}

impl std::fmt::Display for PeerAuthorityProjectionError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::SameProfile => formatter.write_str("peer authority requires distinct profiles"),
            Self::SenderDisabled(profile_id) => {
                write!(formatter, "peer authority sender {profile_id} is disabled")
            }
            Self::ReceiverDisabled(profile_id) => {
                write!(
                    formatter,
                    "peer authority receiver {profile_id} is disabled"
                )
            }
            Self::TooManyEntries => {
                formatter.write_str("peer authority policy exceeds its entry bound")
            }
            Self::InvalidCapabilityLabel => {
                formatter.write_str("peer authority policy contains an invalid capability label")
            }
            Self::InvalidAutonomyBound => {
                formatter.write_str("peer authority autonomy bound is invalid")
            }
            Self::InvalidComputerBound => {
                formatter.write_str("peer authority computer bound is invalid")
            }
        }
    }
}

impl std::error::Error for PeerAuthorityProjectionError {}

pub fn project_peer_authority(
    sender: &PeerAuthorityPolicy,
    receiver: &PeerAuthorityPolicy,
) -> Result<PeerAuthorityProjection, PeerAuthorityProjectionError> {
    validate_peer_authority_policy(sender)?;
    validate_peer_authority_policy(receiver)?;
    if sender.profile_id == receiver.profile_id {
        return Err(PeerAuthorityProjectionError::SameProfile);
    }
    if !sender.enabled {
        return Err(PeerAuthorityProjectionError::SenderDisabled(
            sender.profile_id.clone(),
        ));
    }
    if !receiver.enabled {
        return Err(PeerAuthorityProjectionError::ReceiverDisabled(
            receiver.profile_id.clone(),
        ));
    }

    let autonomy_enabled = sender.autonomy.enabled && receiver.autonomy.enabled;
    let computer_enabled = sender.computer.enabled && receiver.computer.enabled;
    Ok(PeerAuthorityProjection {
        sender_profile_id: sender.profile_id.clone(),
        receiver_profile_id: receiver.profile_id.clone(),
        sender_revision: sender.revision.clone(),
        receiver_revision: receiver.revision.clone(),
        tools: intersect_labels(&sender.tools, &receiver.tools),
        models: intersect_labels(&sender.models, &receiver.models),
        network_scopes: intersect_labels(&sender.network_scopes, &receiver.network_scopes),
        filesystem_scopes: intersect_labels(&sender.filesystem_scopes, &receiver.filesystem_scopes),
        allow_credentials: sender.allow_credentials && receiver.allow_credentials,
        approvals: sender.approvals.intersect(receiver.approvals),
        autonomy: PeerAutonomyCeiling {
            enabled: autonomy_enabled,
            max_children: if autonomy_enabled {
                sender
                    .autonomy
                    .max_children
                    .min(receiver.autonomy.max_children)
            } else {
                0
            },
            max_depth: if autonomy_enabled {
                sender.autonomy.max_depth.min(receiver.autonomy.max_depth)
            } else {
                0
            },
            max_parallel_actions: if autonomy_enabled {
                sender
                    .autonomy
                    .max_parallel_actions
                    .min(receiver.autonomy.max_parallel_actions)
            } else {
                0
            },
        },
        computer: PeerComputerCeiling {
            enabled: computer_enabled,
            allow_downloads: computer_enabled
                && sender.computer.allow_downloads
                && receiver.computer.allow_downloads,
            allow_uploads: computer_enabled
                && sender.computer.allow_uploads
                && receiver.computer.allow_uploads,
            allow_consequential_actions: computer_enabled
                && sender.computer.allow_consequential_actions
                && receiver.computer.allow_consequential_actions,
            max_idle_seconds: if computer_enabled {
                sender
                    .computer
                    .max_idle_seconds
                    .min(receiver.computer.max_idle_seconds)
            } else {
                0
            },
        },
        allow_self_evolution: sender.allow_self_evolution && receiver.allow_self_evolution,
    })
}

fn intersect_labels(left: &BTreeSet<String>, right: &BTreeSet<String>) -> BTreeSet<String> {
    left.intersection(right).cloned().collect()
}

fn validate_peer_authority_policy(
    policy: &PeerAuthorityPolicy,
) -> Result<(), PeerAuthorityProjectionError> {
    let sets = [
        (&policy.tools, false),
        (&policy.models, false),
        (&policy.network_scopes, true),
        (&policy.filesystem_scopes, true),
    ];
    for (values, safe_scope) in sets {
        if values.len() > MAX_PEER_AUTHORITY_ENTRIES {
            return Err(PeerAuthorityProjectionError::TooManyEntries);
        }
        if values.iter().any(|value| {
            value.is_empty()
                || value.len() > MAX_PEER_AUTHORITY_LABEL_BYTES
                || value.chars().any(char::is_control)
                || (safe_scope
                    && !value.bytes().all(|byte| {
                        byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.')
                    }))
        }) {
            return Err(PeerAuthorityProjectionError::InvalidCapabilityLabel);
        }
    }
    if policy.autonomy.max_children > MAX_PEER_AUTONOMY_BOUND
        || policy.autonomy.max_depth > MAX_PEER_AUTONOMY_BOUND
        || policy.autonomy.max_parallel_actions > MAX_PEER_AUTONOMY_BOUND
        || (!policy.autonomy.enabled
            && (policy.autonomy.max_children != 0
                || policy.autonomy.max_depth != 0
                || policy.autonomy.max_parallel_actions != 0))
    {
        return Err(PeerAuthorityProjectionError::InvalidAutonomyBound);
    }
    if (!policy.computer.enabled
        && (policy.computer.allow_downloads
            || policy.computer.allow_uploads
            || policy.computer.allow_consequential_actions
            || policy.computer.max_idle_seconds != 0))
        || (policy.computer.enabled && policy.computer.max_idle_seconds == 0)
    {
        return Err(PeerAuthorityProjectionError::InvalidComputerBound);
    }
    Ok(())
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentRosterEntry {
    pub profile_id: ProfileId,
    pub name: String,
    pub role: String,
    pub avatar: Option<String>,
    pub lifecycle: ProfileLifecycleState,
    pub hidden: bool,
    pub enabled: bool,
    pub revision: Revision,
}

impl From<&AgentLifecycleRecord> for AgentRosterEntry {
    fn from(value: &AgentLifecycleRecord) -> Self {
        Self {
            profile_id: value.profile.profile.id.clone(),
            name: value.profile.profile.display_name.clone(),
            role: value.presentation.role.clone(),
            avatar: value.presentation.avatar.clone(),
            lifecycle: value.presentation.lifecycle,
            hidden: value.presentation.hidden,
            enabled: value.profile.enabled,
            revision: value.profile.revision,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DuplicateSelection {
    pub model_route: bool,
    pub skills: bool,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentLifecycleEdit {
    pub display_name: Option<String>,
    pub role: Option<String>,
    pub description: Option<String>,
    pub avatar: Option<Option<String>>,
    pub model_route: Option<ModelRoute>,
    pub computer_policy: Option<ComputerPolicy>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeletePlan {
    pub profile_id: ProfileId,
    pub expected_revision: Revision,
    pub confirmed_profile_id: Option<ProfileId>,
    pub owned_work: OwnedWorkDisposition,
    pub revoke_active_leases: bool,
    pub retained_shared_remnants: Vec<String>,
    pub externally_controlled_remnants: Vec<String>,
    pub saga_proof: DeleteSagaProof,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)]
pub struct DeleteSagaProof {
    correlation_key: String,
    owned_work_terminal: bool,
    active_leases_revoked: bool,
    private_resources_terminal: bool,
    shared_data_classified: bool,
}

impl DeleteSagaProof {
    #[allow(clippy::fn_params_excessive_bools)]
    pub fn verified(
        correlation_key: impl Into<String>,
        owned_work_terminal: bool,
        active_leases_revoked: bool,
        private_resources_terminal: bool,
        shared_data_classified: bool,
    ) -> Result<Self, ProfileError> {
        let proof = Self {
            correlation_key: correlation_key.into(),
            owned_work_terminal,
            active_leases_revoked,
            private_resources_terminal,
            shared_data_classified,
        };
        if proof.correlation_key.trim().is_empty()
            || proof.correlation_key.len() > 256
            || !proof.owned_work_terminal
            || !proof.active_leases_revoked
            || !proof.private_resources_terminal
            || !proof.shared_data_classified
        {
            return Err(ProfileError::Invalid("delete saga is not terminal".into()));
        }
        Ok(proof)
    }

    pub fn correlation_key(&self) -> &str {
        &self.correlation_key
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeletionTombstone {
    pub correlation_key: String,
    pub owned_work: OwnedWorkDisposition,
    pub retained_shared_remnants: Vec<String>,
    pub externally_controlled_remnants: Vec<String>,
    pub deleted_at: UtcTimestamp,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentLifecycleMutationPlan {
    pub record: AgentLifecycleRecord,
    pub mutation: Option<RecordMutation>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteMutationPlan {
    pub lifecycle: AgentLifecycleMutationPlan,
    pub report: AgentDeleteReport,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteReport {
    pub profile_id: ProfileId,
    pub owned_work: OwnedWorkDisposition,
    pub active_leases_revoked: bool,
    pub retained_shared_remnants: Vec<String>,
    pub externally_controlled_remnants: Vec<String>,
    pub audit: AgentLifecycleAudit,
}

pub struct ProfileRegistry<R> {
    repository: R,
}

impl<R> ProfileRegistry<R>
where
    R: ProfileRepository,
{
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    /// Registers a new durable profile after validating its real resource roots.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid resources, duplicate IDs, or persistence failure.
    pub fn register(
        &self,
        mut profile: RegisteredProfile,
    ) -> Result<RegisteredProfile, ProfileError> {
        if self.get(profile.id())?.is_some() {
            return Err(ProfileError::AlreadyExists(profile.profile.id.clone()));
        }
        profile.revision = Revision::ZERO;
        normalize_and_validate(&mut profile)?;
        self.repository
            .put_profile(encode(&profile)?, WritePrecondition::Missing)
            .map_err(repository_error)?;
        Ok(profile)
    }

    /// Replaces a profile only at the caller-observed revision.
    ///
    /// # Errors
    ///
    /// Returns an error for missing/stale profiles, invalid resources, or persistence failure.
    pub fn update(
        &self,
        mut profile: RegisteredProfile,
        expected: Revision,
    ) -> Result<RegisteredProfile, ProfileError> {
        let raw = self
            .repository
            .get_profile(profile.id().as_entity_id())
            .map_err(repository_error)?
            .ok_or_else(|| ProfileError::Missing(profile.profile.id.clone()))?;
        if decode_lifecycle(raw.clone())?.presentation.lifecycle == ProfileLifecycleState::Deleted {
            return Err(ProfileError::InvalidTransition);
        }
        let sidecar = raw.payload.get("teammate").cloned();
        let current = decode(raw)?;
        if current.revision != expected || profile.revision != expected {
            return Err(ProfileError::Stale);
        }
        profile.revision = expected.checked_next().ok_or(ProfileError::Stale)?;
        normalize_and_validate(&mut profile)?;
        let mut encoded = encode(&profile)?;
        if let Some(sidecar) = sidecar {
            encoded
                .payload
                .as_object_mut()
                .ok_or_else(|| ProfileError::Invalid("profile payload must be an object".into()))?
                .insert("teammate".into(), sidecar);
        }
        self.repository
            .put_profile(encoded, WritePrecondition::Exact(expected))
            .map_err(repository_error)?;
        Ok(profile)
    }

    /// # Errors
    ///
    /// Returns an error when the durable record cannot be loaded or decoded.
    pub fn get(&self, id: &ProfileId) -> Result<Option<RegisteredProfile>, ProfileError> {
        self.repository
            .get_profile(id.as_entity_id())
            .map_err(repository_error)?
            .map(decode)
            .transpose()
    }

    /// # Errors
    ///
    /// Returns an error when durable records cannot be loaded or decoded.
    pub fn list(&self) -> Result<Vec<RegisteredProfile>, ProfileError> {
        let mut profiles = self
            .repository
            .list_profiles()
            .map_err(repository_error)?
            .into_iter()
            .map(decode)
            .collect::<Result<Vec<_>, _>>()?;
        profiles.sort_by(|left, right| left.profile.id.cmp(&right.profile.id));
        Ok(profiles)
    }
}

pub struct AgentLifecycleService<R> {
    repository: R,
}

impl<R> AgentLifecycleService<R>
where
    R: ProfileRepository,
{
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    /// # Errors
    /// Returns an error when the draft is invalid, already exists, or cannot be persisted.
    pub fn create(
        &self,
        profile: RegisteredProfile,
        presentation: AgentProfilePresentation,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleRecord, ProfileError> {
        let plan = self.plan_create(profile, presentation, actor, now)?;
        self.commit_plan(&plan)?;
        Ok(plan.record)
    }

    pub fn plan_create(
        &self,
        mut profile: RegisteredProfile,
        mut presentation: AgentProfilePresentation,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleMutationPlan, ProfileError> {
        validate_actor(actor)?;
        if self.get(profile.id())?.is_some() {
            return Err(ProfileError::AlreadyExists(profile.profile.id.clone()));
        }
        profile.enabled = false;
        profile.revision = Revision::ZERO;
        profile.updated_at = now;
        presentation.lifecycle = ProfileLifecycleState::Draft;
        presentation.hidden = false;
        presentation
            .validate()
            .map_err(|error| ProfileError::Invalid(error.to_string()))?;
        normalize_and_validate(&mut profile)?;
        let record = AgentLifecycleRecord {
            profile,
            presentation,
            audit: vec![audit(1, actor, "create", Revision::ZERO, now)],
            deletion: None,
        };
        let encoded = encode_lifecycle(&record)?;
        Ok(AgentLifecycleMutationPlan {
            record,
            mutation: Some(RecordMutation::Put {
                collection: keith_state_store_core::Collection::Profiles,
                record: encoded,
                precondition: WritePrecondition::Missing,
            }),
        })
    }

    pub fn commit_plan(&self, plan: &AgentLifecycleMutationPlan) -> Result<(), ProfileError> {
        let Some(RecordMutation::Put {
            record,
            precondition,
            ..
        }) = &plan.mutation
        else {
            return Ok(());
        };
        self.repository
            .put_profile(record.clone(), *precondition)
            .map_err(repository_error)?;
        Ok(())
    }

    /// # Errors
    /// Returns an error for a missing profile, stale revision, malformed edit, or persistence failure.
    pub fn edit(
        &self,
        id: &ProfileId,
        expected: Revision,
        edit: AgentLifecycleEdit,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleRecord, ProfileError> {
        self.mutate(id, expected, actor, "edit", now, |record| {
            if let Some(value) = edit.display_name {
                record.profile.profile.display_name = value;
            }
            if let Some(value) = edit.role {
                record.presentation.role = value;
            }
            if let Some(value) = edit.description {
                record.presentation.description = value;
            }
            if let Some(value) = edit.avatar {
                record.presentation.avatar = value;
            }
            if let Some(value) = edit.model_route {
                record.profile.profile.model_route = value;
            }
            if let Some(value) = edit.computer_policy {
                record.presentation.computer_policy = value;
            }
            Ok(())
        })
    }

    pub fn enable(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleRecord, ProfileError> {
        let plan = self.plan_enable(id, expected, actor, now)?;
        self.commit_plan(&plan)?;
        Ok(plan.record)
    }

    pub fn plan_enable(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleMutationPlan, ProfileError> {
        self.plan_transition(
            id,
            expected,
            actor,
            now,
            ProfileLifecycleState::Enabled,
            "enable",
        )
    }

    pub fn disable(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleRecord, ProfileError> {
        let plan = self.plan_disable(id, expected, actor, now)?;
        self.commit_plan(&plan)?;
        Ok(plan.record)
    }

    pub fn plan_disable(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleMutationPlan, ProfileError> {
        self.plan_transition(
            id,
            expected,
            actor,
            now,
            ProfileLifecycleState::Disabled,
            "disable",
        )
    }

    pub fn archive(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleRecord, ProfileError> {
        let plan = self.plan_archive(id, expected, actor, now)?;
        self.commit_plan(&plan)?;
        Ok(plan.record)
    }

    pub fn plan_archive(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleMutationPlan, ProfileError> {
        self.plan_transition(
            id,
            expected,
            actor,
            now,
            ProfileLifecycleState::Archived,
            "archive",
        )
    }

    pub fn unarchive(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleRecord, ProfileError> {
        let plan = self.plan_unarchive(id, expected, actor, now)?;
        self.commit_plan(&plan)?;
        Ok(plan.record)
    }

    pub fn plan_unarchive(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleMutationPlan, ProfileError> {
        self.plan_transition(
            id,
            expected,
            actor,
            now,
            ProfileLifecycleState::Disabled,
            "unarchive",
        )
    }

    pub fn hide(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleRecord, ProfileError> {
        self.set_hidden(id, expected, actor, now, true, "hide")
    }

    pub fn unhide(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleRecord, ProfileError> {
        self.set_hidden(id, expected, actor, now, false, "unhide")
    }

    /// # Errors
    /// Returns an error for missing source, unsafe target resources, duplicate identity, or persistence failure.
    pub fn duplicate(
        &self,
        source_id: &ProfileId,
        mut target: RegisteredProfile,
        selection: &DuplicateSelection,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentLifecycleRecord, ProfileError> {
        let source = self.get_required(source_id)?;
        if target.profile.id == source.profile.profile.id
            || target.profile.workspace_id == source.profile.profile.workspace_id
        {
            return Err(ProfileError::Invalid(
                "duplication requires new profile and workspace identities".into(),
            ));
        }
        if selection.model_route {
            target.profile.model_route = source.profile.profile.model_route.clone();
        }
        target.profile.model_route.credential_ref = None;
        if selection.skills {
            target
                .profile
                .enabled_skills
                .clone_from(&source.profile.profile.enabled_skills);
        }
        let presentation = AgentProfilePresentation {
            role: source.presentation.role,
            description: source.presentation.description,
            avatar: source.presentation.avatar,
            lifecycle: ProfileLifecycleState::Draft,
            hidden: false,
            computer_policy: ComputerPolicy::default(),
        };
        self.create(target, presentation, actor, now)
    }

    /// # Errors
    /// Returns an error unless the profile, current revision, explicit confirmation, and repository delete all agree.
    pub fn confirmed_delete(
        &self,
        plan: AgentDeletePlan,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentDeleteReport, ProfileError> {
        let planned = self.plan_confirmed_delete(plan, actor, now)?;
        self.commit_plan(&planned.lifecycle)?;
        Ok(planned.report)
    }

    #[allow(clippy::too_many_lines)]
    pub fn plan_confirmed_delete(
        &self,
        plan: AgentDeletePlan,
        actor: &str,
        now: UtcTimestamp,
    ) -> Result<AgentDeleteMutationPlan, ProfileError> {
        validate_actor(actor)?;
        if plan.confirmed_profile_id.as_ref() != Some(&plan.profile_id) {
            return Err(ProfileError::DeleteNotConfirmed);
        }
        if !plan.revoke_active_leases {
            return Err(ProfileError::Invalid(
                "delete must revoke active leases".into(),
            ));
        }
        let current = self.get_required(&plan.profile_id)?;
        let proof = DeleteSagaProof::verified(
            plan.saga_proof.correlation_key,
            plan.saga_proof.owned_work_terminal,
            plan.saga_proof.active_leases_revoked,
            plan.saga_proof.private_resources_terminal,
            plan.saga_proof.shared_data_classified,
        )?;
        if current.presentation.lifecycle == ProfileLifecycleState::Deleted {
            let tombstone = current
                .deletion
                .as_ref()
                .ok_or_else(|| ProfileError::Invalid("deleted profile lacks tombstone".into()))?;
            if tombstone.correlation_key != proof.correlation_key {
                return Err(ProfileError::InvalidTransition);
            }
            let audit = current
                .audit
                .last()
                .cloned()
                .ok_or_else(|| ProfileError::Invalid("deleted profile lacks audit".into()))?;
            return Ok(AgentDeleteMutationPlan {
                report: AgentDeleteReport {
                    profile_id: plan.profile_id,
                    owned_work: tombstone.owned_work.clone(),
                    active_leases_revoked: true,
                    retained_shared_remnants: tombstone.retained_shared_remnants.clone(),
                    externally_controlled_remnants: tombstone
                        .externally_controlled_remnants
                        .clone(),
                    audit,
                },
                lifecycle: AgentLifecycleMutationPlan {
                    record: current,
                    mutation: None,
                },
            });
        }
        if current.profile.revision != plan.expected_revision {
            return Err(ProfileError::Stale);
        }
        let owned_work = plan.owned_work.clone();
        let shared = plan.retained_shared_remnants.clone();
        let external = plan.externally_controlled_remnants.clone();
        let correlation = proof.correlation_key.clone();
        let mut terminal = current;
        let next = plan
            .expected_revision
            .checked_next()
            .ok_or(ProfileError::Stale)?;
        terminal.profile.enabled = false;
        terminal.profile.revision = next;
        terminal.profile.updated_at = now;
        terminal.presentation.lifecycle = ProfileLifecycleState::Deleted;
        terminal.presentation.hidden = true;
        terminal.deletion = Some(AgentDeletionTombstone {
            correlation_key: correlation,
            owned_work,
            retained_shared_remnants: shared,
            externally_controlled_remnants: external,
            deleted_at: now,
        });
        let sequence = u64::try_from(terminal.audit.len())
            .map_err(|_| ProfileError::Invalid("audit is too large".into()))?
            .checked_add(1)
            .ok_or_else(|| ProfileError::Invalid("audit is too large".into()))?;
        if sequence > 4_096 {
            return Err(ProfileError::Invalid("audit is too large".into()));
        }
        terminal
            .audit
            .push(audit(sequence, actor, "confirmed_delete", next, now));
        let encoded = encode_lifecycle(&terminal)?;
        let lifecycle = AgentLifecycleMutationPlan {
            record: terminal,
            mutation: Some(RecordMutation::Put {
                collection: keith_state_store_core::Collection::Profiles,
                record: encoded,
                precondition: WritePrecondition::Exact(plan.expected_revision),
            }),
        };
        let tombstone = lifecycle
            .record
            .deletion
            .as_ref()
            .ok_or_else(|| ProfileError::Invalid("delete plan lacks tombstone".into()))?;
        let delete_audit = lifecycle
            .record
            .audit
            .last()
            .cloned()
            .ok_or_else(|| ProfileError::Invalid("delete plan lacks audit".into()))?;
        Ok(AgentDeleteMutationPlan {
            report: AgentDeleteReport {
                profile_id: plan.profile_id,
                owned_work: tombstone.owned_work.clone(),
                active_leases_revoked: true,
                retained_shared_remnants: tombstone.retained_shared_remnants.clone(),
                externally_controlled_remnants: tombstone.externally_controlled_remnants.clone(),
                audit: delete_audit,
            },
            lifecycle,
        })
    }

    pub fn get(&self, id: &ProfileId) -> Result<Option<AgentLifecycleRecord>, ProfileError> {
        self.repository
            .get_profile(id.as_entity_id())
            .map_err(repository_error)?
            .map(decode_lifecycle)
            .transpose()
    }

    pub fn roster(&self) -> Result<Vec<AgentRosterEntry>, ProfileError> {
        let mut entries = self
            .repository
            .list_profiles()
            .map_err(repository_error)?
            .into_iter()
            .map(decode_lifecycle)
            .collect::<Result<Vec<_>, _>>()?
            .iter()
            .filter(|record| record.presentation.lifecycle != ProfileLifecycleState::Deleted)
            .map(AgentRosterEntry::from)
            .collect::<Vec<_>>();
        entries.sort_by(|left, right| left.profile_id.cmp(&right.profile_id));
        Ok(entries)
    }

    fn get_required(&self, id: &ProfileId) -> Result<AgentLifecycleRecord, ProfileError> {
        self.get(id)?
            .ok_or_else(|| ProfileError::Missing(id.clone()))
    }

    fn plan_transition(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
        target: ProfileLifecycleState,
        action: &'static str,
    ) -> Result<AgentLifecycleMutationPlan, ProfileError> {
        let current = self.get_required(id)?;
        if current.presentation.lifecycle == target
            && current.profile.revision == expected.checked_next().ok_or(ProfileError::Stale)?
            && current
                .audit
                .last()
                .is_some_and(|entry| entry.action == action)
        {
            return Ok(AgentLifecycleMutationPlan {
                record: current,
                mutation: None,
            });
        }
        self.plan_mutate(id, expected, actor, action, now, |record| {
            let source = record.presentation.lifecycle;
            let allowed = match action {
                "enable" => {
                    matches!(
                        source,
                        ProfileLifecycleState::Draft | ProfileLifecycleState::Disabled
                    ) && target == ProfileLifecycleState::Enabled
                }
                "disable" => {
                    source == ProfileLifecycleState::Enabled
                        && target == ProfileLifecycleState::Disabled
                }
                "archive" => {
                    matches!(
                        source,
                        ProfileLifecycleState::Draft
                            | ProfileLifecycleState::Enabled
                            | ProfileLifecycleState::Disabled
                    ) && target == ProfileLifecycleState::Archived
                }
                "unarchive" => {
                    source == ProfileLifecycleState::Archived
                        && target == ProfileLifecycleState::Disabled
                }
                _ => false,
            };
            if !allowed {
                return Err(ProfileError::InvalidTransition);
            }
            record.presentation.lifecycle = target;
            record.profile.enabled = target == ProfileLifecycleState::Enabled;
            Ok(())
        })
    }

    fn set_hidden(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        now: UtcTimestamp,
        hidden: bool,
        action: &'static str,
    ) -> Result<AgentLifecycleRecord, ProfileError> {
        self.mutate(id, expected, actor, action, now, |record| {
            record.presentation.hidden = hidden;
            Ok(())
        })
    }

    fn mutate<F>(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        action: &'static str,
        now: UtcTimestamp,
        change: F,
    ) -> Result<AgentLifecycleRecord, ProfileError>
    where
        F: FnOnce(&mut AgentLifecycleRecord) -> Result<(), ProfileError>,
    {
        let plan = self.plan_mutate(id, expected, actor, action, now, change)?;
        self.commit_plan(&plan)?;
        Ok(plan.record)
    }

    fn plan_mutate<F>(
        &self,
        id: &ProfileId,
        expected: Revision,
        actor: &str,
        action: &'static str,
        now: UtcTimestamp,
        change: F,
    ) -> Result<AgentLifecycleMutationPlan, ProfileError>
    where
        F: FnOnce(&mut AgentLifecycleRecord) -> Result<(), ProfileError>,
    {
        validate_actor(actor)?;
        let mut record = self.get_required(id)?;
        if record.presentation.lifecycle == ProfileLifecycleState::Deleted {
            return Err(ProfileError::InvalidTransition);
        }
        if record.profile.revision != expected {
            return Err(ProfileError::Stale);
        }
        change(&mut record)?;
        let next = expected.checked_next().ok_or(ProfileError::Stale)?;
        record.profile.revision = next;
        record.profile.updated_at = now;
        record
            .presentation
            .validate()
            .map_err(|error| ProfileError::Invalid(error.to_string()))?;
        normalize_and_validate(&mut record.profile)?;
        let sequence = u64::try_from(record.audit.len())
            .map_err(|_| ProfileError::Invalid("audit is too large".into()))?
            .checked_add(1)
            .ok_or_else(|| ProfileError::Invalid("audit is too large".into()))?;
        if sequence > 4_096 {
            return Err(ProfileError::Invalid("audit is too large".into()));
        }
        record.audit.push(audit(sequence, actor, action, next, now));
        let encoded = encode_lifecycle(&record)?;
        Ok(AgentLifecycleMutationPlan {
            record,
            mutation: Some(RecordMutation::Put {
                collection: keith_state_store_core::Collection::Profiles,
                record: encoded,
                precondition: WritePrecondition::Exact(expected),
            }),
        })
    }
}

fn normalize_and_validate(profile: &mut RegisteredProfile) -> Result<(), ProfileError> {
    if profile.profile.version.major != CURRENT_SCHEMA_VERSION.major
        || profile.profile.version.minor > CURRENT_SCHEMA_VERSION.minor
    {
        return Err(ProfileError::Invalid("unsupported profile version".into()));
    }
    if profile.profile.display_name.trim().is_empty()
        || profile.profile.model_route.provider.trim().is_empty()
        || profile.profile.model_route.model.trim().is_empty()
        || profile.authorized_callers.is_empty()
        || profile
            .authorized_callers
            .iter()
            .any(|caller| caller.trim().is_empty())
    {
        return Err(ProfileError::Invalid(
            "identity, model route, and authorized callers must be non-empty".into(),
        ));
    }
    validate_unique_nonempty("skills", &profile.profile.enabled_skills)?;
    validate_unique_nonempty("MCP servers", &profile.profile.enabled_mcp_servers)?;
    validate_unique_nonempty("plugins", &profile.profile.enabled_plugins)?;
    validate_unique_nonempty("channels", &profile.profile.channels)?;
    if profile
        .profile
        .tool_rules
        .keys()
        .any(|name| name.trim().is_empty())
        || profile.profile.autonomy.max_children == 0
        || profile.profile.autonomy.max_depth == 0
        || profile.profile.notifications.daily_limit == 0
    {
        return Err(ProfileError::Invalid(
            "tool names and profile ceilings must be valid".into(),
        ));
    }
    let workspace = canonical_directory(&profile.resources.workspace_root)?;
    let memory = canonical_directory(&profile.resources.memory_root)?;
    let schedules = canonical_directory(&profile.resources.schedule_root)?;
    if !memory.starts_with(&workspace) || !schedules.starts_with(&workspace) || memory == schedules
    {
        return Err(ProfileError::Invalid(
            "memory and schedule roots must be distinct workspace descendants".into(),
        ));
    }
    validate_profile_file(&workspace, &profile.profile.persona_file)?;
    validate_profile_file(&workspace, &profile.profile.user_file)?;
    for rules in &profile.profile.rule_files {
        validate_profile_file(&workspace, rules)?;
    }
    profile.resources.workspace_root = workspace;
    profile.resources.memory_root = memory;
    profile.resources.schedule_root = schedules;
    Ok(())
}

fn validate_actor(actor: &str) -> Result<(), ProfileError> {
    if actor.trim().is_empty() || actor.len() > 256 {
        Err(ProfileError::Invalid("lifecycle actor is invalid".into()))
    } else {
        Ok(())
    }
}

fn audit(
    sequence: u64,
    actor: &str,
    action: &str,
    revision: Revision,
    occurred_at: UtcTimestamp,
) -> AgentLifecycleAudit {
    AgentLifecycleAudit {
        sequence,
        actor: actor.into(),
        action: action.into(),
        revision,
        occurred_at,
    }
}

fn encode_lifecycle(record: &AgentLifecycleRecord) -> Result<VersionedRecord, ProfileError> {
    let mut encoded = encode(&record.profile)?;
    let payload = encoded
        .payload
        .as_object_mut()
        .ok_or_else(|| ProfileError::Invalid("profile payload must be an object".into()))?;
    payload.insert(
        "teammate".into(),
        serde_json::json!({ "presentation": record.presentation, "audit": record.audit, "deletion": record.deletion }),
    );
    Ok(encoded)
}

fn decode_lifecycle(record: VersionedRecord) -> Result<AgentLifecycleRecord, ProfileError> {
    let mut payload = record.payload.clone();
    let sidecar = payload
        .as_object_mut()
        .and_then(|object| object.remove("teammate"));
    let profile = decode(VersionedRecord { payload, ..record })?;
    if let Some(sidecar) = sidecar {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Sidecar {
            presentation: AgentProfilePresentation,
            audit: Vec<AgentLifecycleAudit>,
            #[serde(default)]
            deletion: Option<AgentDeletionTombstone>,
        }
        let sidecar: Sidecar = serde_json::from_value(sidecar)?;
        sidecar
            .presentation
            .validate()
            .map_err(|error| ProfileError::Invalid(error.to_string()))?;
        if sidecar.audit.len() > 4_096 {
            return Err(ProfileError::Invalid("audit is too large".into()));
        }
        Ok(AgentLifecycleRecord {
            profile,
            presentation: sidecar.presentation,
            audit: sidecar.audit,
            deletion: sidecar.deletion,
        })
    } else {
        Ok(AgentLifecycleRecord {
            presentation: AgentProfilePresentation {
                role: "Assistant".into(),
                description: String::new(),
                avatar: None,
                lifecycle: if profile.enabled {
                    ProfileLifecycleState::Enabled
                } else {
                    ProfileLifecycleState::Disabled
                },
                hidden: false,
                computer_policy: ComputerPolicy::default(),
            },
            profile,
            audit: Vec::new(),
            deletion: None,
        })
    }
}

fn validate_unique_nonempty(field: &str, values: &[String]) -> Result<(), ProfileError> {
    let unique = values.iter().collect::<BTreeSet<_>>();
    if unique.len() != values.len() || values.iter().any(|value| value.trim().is_empty()) {
        Err(ProfileError::Invalid(format!(
            "{field} must be unique and non-empty"
        )))
    } else {
        Ok(())
    }
}

fn canonical_directory(path: &Path) -> Result<PathBuf, ProfileError> {
    let canonical = fs::canonicalize(path)?;
    if !canonical.is_dir() {
        return Err(ProfileError::Invalid(format!(
            "{} is not a directory",
            path.display()
        )));
    }
    Ok(canonical)
}

fn validate_profile_file(workspace: &Path, relative: &Path) -> Result<(), ProfileError> {
    if relative.as_os_str().is_empty()
        || relative.is_absolute()
        || relative.components().any(|component| {
            matches!(
                component,
                Component::ParentDir | Component::RootDir | Component::Prefix(_)
            )
        })
    {
        return Err(ProfileError::Invalid(
            "profile file path escapes workspace".into(),
        ));
    }
    let canonical = fs::canonicalize(workspace.join(relative))?;
    if !canonical.starts_with(workspace) || !canonical.is_file() {
        return Err(ProfileError::Invalid(
            "profile file must be a regular workspace file".into(),
        ));
    }
    Ok(())
}

fn encode(profile: &RegisteredProfile) -> Result<VersionedRecord, ProfileError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: profile.profile.id.as_entity_id().clone(),
        revision: profile.revision,
        updated_at: profile.updated_at,
        payload: serde_json::to_value(profile)?,
    })
}

fn decode(record: VersionedRecord) -> Result<RegisteredProfile, ProfileError> {
    let mut payload = record.payload;
    if let Some(object) = payload.as_object_mut() {
        object.remove("teammate");
    }
    let profile: RegisteredProfile = serde_json::from_value(payload)?;
    if profile.profile.id.as_entity_id() != &record.id || profile.revision != record.revision {
        return Err(ProfileError::Invalid(
            "profile record identity or revision mismatch".into(),
        ));
    }
    Ok(profile)
}

fn repository_error(error: impl std::error::Error) -> ProfileError {
    ProfileError::Repository(error.to_string())
}

#[cfg(test)]
mod tests {
    use std::collections::{BTreeMap, BTreeSet};
    use std::fs;

    use keith_agent_types::{ProfileId, TimeZoneName, WorkspaceId};
    use keith_state_store::EmbeddedStore;
    use tempfile::TempDir;

    use super::*;

    fn registered(root: &TempDir) -> RegisteredProfile {
        fs::write(root.path().join("PERSONA.md"), "precise").unwrap();
        fs::write(root.path().join("USER.md"), "operator").unwrap();
        fs::write(root.path().join("RULES.md"), "stay scoped").unwrap();
        fs::create_dir(root.path().join("memory")).unwrap();
        fs::create_dir(root.path().join("schedules")).unwrap();
        RegisteredProfile {
            profile: AgentProfile {
                version: CURRENT_SCHEMA_VERSION,
                id: ProfileId::new(),
                display_name: "work".into(),
                workspace_id: WorkspaceId::new(),
                persona_file: "PERSONA.md".into(),
                user_file: "USER.md".into(),
                rule_files: vec!["RULES.md".into()],
                model_route: ModelRoute {
                    provider: "openai".into(),
                    model: "model-a".into(),
                    fallbacks: vec![],
                    credential_ref: Some("credential-a".into()),
                },
                thinking: ThinkingLevel::High,
                tool_rules: BTreeMap::from([("read".into(), ToolPermission::Allow)]),
                enabled_skills: vec!["research".into()],
                enabled_mcp_servers: vec!["codegraph".into()],
                enabled_plugins: vec!["source".into()],
                channels: vec!["terminal".into()],
                autonomy: ProfileAutonomy {
                    mode: AutonomyMode::Bounded,
                    max_children: 2,
                    max_depth: 2,
                    daily_token_budget: 10_000,
                },
                notifications: NotificationSettings {
                    quiet_hours_start: "22:00".into(),
                    quiet_hours_end: "08:00".into(),
                    time_zone: TimeZoneName::parse("Europe/Berlin").unwrap(),
                    daily_limit: 4,
                },
                refinement: RefinementSettings {
                    enabled: true,
                    require_confirmation: true,
                    editable_targets: BTreeSet::from(["persona".into()]),
                },
            },
            resources: ProfileResources {
                workspace_root: root.path().into(),
                memory_root: root.path().join("memory"),
                schedule_root: root.path().join("schedules"),
            },
            enabled: true,
            authorized_callers: BTreeSet::from(["operator-a".into()]),
            revision: Revision::ZERO,
            updated_at: UtcTimestamp::UNIX_EPOCH,
        }
    }

    #[test]
    fn durable_registry_validates_resources_and_rejects_stale_updates() {
        let root = TempDir::new().unwrap();
        let store = EmbeddedStore::open_in_memory().unwrap();
        let registry = ProfileRegistry::new(store);
        let profile = registry.register(registered(&root)).unwrap();
        let mut updated = profile.clone();
        updated.profile.display_name = "updated".into();
        updated.updated_at = UtcTimestamp::from_unix_millis(1);
        let updated = registry.update(updated, Revision::ZERO).unwrap();
        assert_eq!(updated.revision, Revision::new(1));
        assert!(matches!(
            registry.update(profile, Revision::ZERO),
            Err(ProfileError::Stale)
        ));
        assert_eq!(registry.list().unwrap(), vec![updated]);
    }

    #[test]
    fn profile_capabilities_never_grant_self_evolution_authority() {
        let root = TempDir::new().unwrap();
        let mut profile = registered(&root);
        profile.profile.enabled_skills.push("self-evolution".into());
        profile
            .profile
            .enabled_mcp_servers
            .push("self-evolution".into());
        profile
            .profile
            .enabled_plugins
            .push("self-evolution".into());
        profile.profile.channels.push("self-evolution".into());
        assert!(!profile.can_enable_self_evolution());
    }

    fn presentation() -> AgentProfilePresentation {
        AgentProfilePresentation {
            role: "Researcher".into(),
            description: "Finds grounded evidence".into(),
            avatar: Some("avatar://researcher".into()),
            lifecycle: ProfileLifecycleState::Enabled,
            hidden: true,
            computer_policy: ComputerPolicy {
                enabled: true,
                ..ComputerPolicy::default()
            },
        }
    }

    #[test]
    fn agent_lifecycle_is_revisioned_audited_and_roster_backed() {
        let root = TempDir::new().unwrap();
        let service = AgentLifecycleService::new(EmbeddedStore::open_in_memory().unwrap());
        let created = service
            .create(
                registered(&root),
                presentation(),
                "owner",
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert_eq!(created.presentation.lifecycle, ProfileLifecycleState::Draft);
        assert!(!created.profile.enabled);
        assert!(!created.presentation.hidden);
        let id = created.profile.profile.id.clone();
        let enabled = service
            .enable(
                &id,
                Revision::ZERO,
                "owner",
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert!(enabled.profile.enabled);
        assert_eq!(enabled.audit.len(), 2);
        assert!(matches!(
            service.hide(
                &id,
                Revision::ZERO,
                "owner",
                UtcTimestamp::from_unix_millis(2)
            ),
            Err(ProfileError::Stale)
        ));
        let archived = service
            .archive(
                &id,
                enabled.profile.revision,
                "owner",
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert!(!archived.profile.enabled);
        assert_eq!(service.roster().unwrap()[0].profile_id, id);
    }

    #[test]
    fn agent_lifecycle_disable_archive_and_unarchive_plans_are_atomic_and_replay_safe() {
        let root = TempDir::new().unwrap();
        let service = AgentLifecycleService::new(EmbeddedStore::open_in_memory().unwrap());
        let created = service
            .create(
                registered(&root),
                presentation(),
                "owner",
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let id = created.profile.profile.id.clone();
        let enabled = service
            .enable(
                &id,
                Revision::ZERO,
                "owner",
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();

        let disable = service
            .plan_disable(
                &id,
                enabled.profile.revision,
                "owner",
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert!(disable.mutation.is_some());
        assert_eq!(
            service.get(&id).unwrap().unwrap().presentation.lifecycle,
            ProfileLifecycleState::Enabled
        );
        service.commit_plan(&disable).unwrap();
        let disable_replay = service
            .plan_disable(
                &id,
                enabled.profile.revision,
                "owner",
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        assert!(disable_replay.mutation.is_none());
        assert_eq!(disable_replay.record.audit, disable.record.audit);

        let archived = service
            .plan_archive(
                &id,
                disable.record.profile.revision,
                "owner",
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        service.commit_plan(&archived).unwrap();
        assert!(matches!(
            service.plan_enable(
                &id,
                archived.record.profile.revision,
                "owner",
                UtcTimestamp::from_unix_millis(5)
            ),
            Err(ProfileError::InvalidTransition)
        ));
        let unarchived = service
            .plan_unarchive(
                &id,
                archived.record.profile.revision,
                "owner",
                UtcTimestamp::from_unix_millis(6),
            )
            .unwrap();
        service.commit_plan(&unarchived).unwrap();
        assert_eq!(
            unarchived.record.presentation.lifecycle,
            ProfileLifecycleState::Disabled
        );
        let unarchive_replay = service
            .plan_unarchive(
                &id,
                archived.record.profile.revision,
                "owner",
                UtcTimestamp::from_unix_millis(7),
            )
            .unwrap();
        assert!(unarchive_replay.mutation.is_none());
        assert_eq!(unarchive_replay.record.audit, unarchived.record.audit);
    }

    #[test]
    fn agent_lifecycle_duplicate_copies_only_selected_config_and_skills() {
        let source_root = TempDir::new().unwrap();
        let target_root = TempDir::new().unwrap();
        let service = AgentLifecycleService::new(EmbeddedStore::open_in_memory().unwrap());
        let source = service
            .create(
                registered(&source_root),
                presentation(),
                "owner",
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let mut target = registered(&target_root);
        target.profile.id = ProfileId::new();
        target.profile.workspace_id = WorkspaceId::new();
        target.profile.enabled_skills.clear();
        target.profile.tool_rules = BTreeMap::from([("read".into(), ToolPermission::Deny)]);
        target.profile.channels = vec!["local-only".into()];
        target.profile.autonomy = ProfileAutonomy {
            mode: AutonomyMode::Suggest,
            max_children: 1,
            max_depth: 1,
            daily_token_budget: 100,
        };
        target.profile.notifications.daily_limit = 1;
        let duplicate = service
            .duplicate(
                &source.profile.profile.id,
                target,
                &DuplicateSelection {
                    model_route: true,
                    skills: true,
                },
                "owner",
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert_eq!(duplicate.profile.profile.enabled_skills, vec!["research"]);
        assert_eq!(
            duplicate.profile.profile.tool_rules,
            BTreeMap::from([("read".into(), ToolPermission::Deny)])
        );
        assert_eq!(duplicate.profile.profile.channels, vec!["local-only"]);
        assert_eq!(duplicate.profile.profile.autonomy.max_children, 1);
        assert_eq!(duplicate.profile.profile.notifications.daily_limit, 1);
        assert!(
            duplicate
                .profile
                .profile
                .model_route
                .credential_ref
                .is_none()
        );
        assert!(!duplicate.presentation.computer_policy.enabled);
        let id = duplicate.profile.profile.id.clone();
        let mut plan = AgentDeletePlan {
            profile_id: id.clone(),
            expected_revision: Revision::ZERO,
            confirmed_profile_id: None,
            owned_work: OwnedWorkDisposition::Cancel,
            revoke_active_leases: true,
            retained_shared_remnants: vec!["shared:report".into()],
            externally_controlled_remnants: vec!["external:email".into()],
            saga_proof: DeleteSagaProof::verified("delete:duplicate", true, true, true, true)
                .unwrap(),
        };
        assert!(matches!(
            service.confirmed_delete(plan.clone(), "owner", UtcTimestamp::from_unix_millis(2)),
            Err(ProfileError::DeleteNotConfirmed)
        ));
        plan.confirmed_profile_id = Some(id.clone());
        fs::remove_dir_all(target_root.path()).unwrap();
        assert!(!duplicate.profile.resources.workspace_root.exists());
        let mut stale = plan.clone();
        stale.expected_revision = Revision::new(1);
        assert!(matches!(
            service.confirmed_delete(stale, "owner", UtcTimestamp::from_unix_millis(2)),
            Err(ProfileError::Stale)
        ));
        let report = service
            .confirmed_delete(plan.clone(), "owner", UtcTimestamp::from_unix_millis(2))
            .unwrap();
        assert_eq!(report.retained_shared_remnants, vec!["shared:report"]);
        let replay = service
            .confirmed_delete(plan, "owner", UtcTimestamp::from_unix_millis(3))
            .unwrap();
        assert_eq!(replay.audit, report.audit);
        let tombstone = service.get(&id).unwrap().unwrap();
        assert_eq!(
            tombstone.presentation.lifecycle,
            ProfileLifecycleState::Deleted
        );
        assert!(tombstone.deletion.is_some());
        assert!(
            service
                .roster()
                .unwrap()
                .iter()
                .all(|entry| entry.profile_id != id)
        );
    }

    #[test]
    fn agent_lifecycle_duplicate_defaults_copy_no_source_authority_or_skills() {
        let source_root = TempDir::new().unwrap();
        let target_root = TempDir::new().unwrap();
        let service = AgentLifecycleService::new(EmbeddedStore::open_in_memory().unwrap());
        let source = service
            .create(
                registered(&source_root),
                presentation(),
                "owner",
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let mut target = registered(&target_root);
        target.profile.id = ProfileId::new();
        target.profile.workspace_id = WorkspaceId::new();
        target.profile.enabled_skills.clear();
        target.profile.tool_rules.clear();
        target.profile.channels.clear();
        target.profile.model_route.credential_ref = Some("target-credential".into());
        let duplicate = service
            .duplicate(
                &source.profile.profile.id,
                target,
                &DuplicateSelection::default(),
                "owner",
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert!(duplicate.profile.profile.enabled_skills.is_empty());
        assert!(duplicate.profile.profile.tool_rules.is_empty());
        assert!(duplicate.profile.profile.channels.is_empty());
        assert!(
            duplicate
                .profile
                .profile
                .model_route
                .credential_ref
                .is_none()
        );
        assert!(!duplicate.presentation.computer_policy.enabled);
    }
}
