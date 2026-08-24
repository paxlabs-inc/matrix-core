use std::fmt::Display;

use keith_agent_types::{ComputerId, ProfileId, Revision, StableKey, UtcTimestamp};
use keith_credentials::{CredentialOwner, CredentialRef, EncryptedCredentialStore};
use serde::{Deserialize, Serialize};
use thiserror::Error;

const MAX_ORIGIN_BYTES: usize = 2_048;
const MAX_FIELD_ID_BYTES: usize = 256;
const MAX_BROKER_ID_BYTES: usize = 256;
const MAX_INJECTION_BYTES: usize = 64 * 1_024;

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum SecretInjectionTarget {
    FocusedField {
        exact_origin: String,
        frame_origin: String,
        field_id: String,
        focus_revision: Revision,
    },
    CredentialBroker {
        exact_origin: String,
        broker_id: String,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SecretInjectionTargetKind {
    FocusedField,
    CredentialBroker,
}

impl SecretInjectionTarget {
    const fn kind(&self) -> SecretInjectionTargetKind {
        match self {
            Self::FocusedField { .. } => SecretInjectionTargetKind::FocusedField,
            Self::CredentialBroker { .. } => SecretInjectionTargetKind::CredentialBroker,
        }
    }

    fn exact_origin(&self) -> &str {
        match self {
            Self::FocusedField { exact_origin, .. }
            | Self::CredentialBroker { exact_origin, .. } => exact_origin,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SecretInjectionRequest {
    pub operation_key: StableKey,
    pub claimed_profile_id: ProfileId,
    pub computer_id: ComputerId,
    pub task_key: StableKey,
    pub task_fencing_token: u64,
    pub computer_revision: Revision,
    pub policy_revision: Revision,
    pub credential_ref: CredentialRef,
    pub target: SecretInjectionTarget,
    pub owner_approved: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SecretInjectionAuthority {
    pub profile_id: ProfileId,
    pub computer_id: ComputerId,
    pub task_key: StableKey,
    pub task_fencing_token: u64,
    pub computer_revision: Revision,
    pub policy_revision: Revision,
    pub credential_ref: CredentialRef,
    pub credential_owner: CredentialOwner,
    pub target: SecretInjectionTarget,
    pub enabled: bool,
    pub allow_secret_injection: bool,
    pub requires_owner_approval: bool,
    pub recording_active: bool,
    pub max_secret_bytes: usize,
}

pub trait SecretInjectionAuthorityResolver: Send + Sync {
    type Error: Display;

    fn resolve_current(
        &self,
        profile_id: &ProfileId,
        computer_id: &ComputerId,
        task_key: &StableKey,
    ) -> Result<SecretInjectionAuthority, Self::Error>;
}

/// A privileged browser-control implementation. Implementations must send bytes directly to the
/// focused renderer field or credential broker and must never retain, inspect, log, serialize, or
/// echo them.
pub trait FocusedSecretWriter {
    type Error: Display;

    fn write_focused_field(
        &mut self,
        exact_origin: &str,
        frame_origin: &str,
        field_id: &str,
        secret: &[u8],
    ) -> Result<(), Self::Error>;

    fn write_credential_broker(
        &mut self,
        exact_origin: &str,
        broker_id: &str,
        secret: &[u8],
    ) -> Result<(), Self::Error>;
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SecretInjectionReceipt {
    pub operation_key: StableKey,
    pub profile_id: ProfileId,
    pub computer_id: ComputerId,
    pub task_key: StableKey,
    pub task_fencing_token: u64,
    pub computer_revision: Revision,
    pub policy_revision: Revision,
    pub target_kind: SecretInjectionTargetKind,
    pub injected_at: UtcTimestamp,
}

#[derive(Debug, Error)]
pub enum SecretInjectionError {
    #[error("secret injection actor is not authorized")]
    Unauthorized,
    #[error("secret injection authority is stale or revoked")]
    StaleAuthority,
    #[error("secret injection focus, origin, or broker target is invalid")]
    InvalidTarget,
    #[error("secret injection is disabled by current policy")]
    PolicyDenied,
    #[error("secret injection requires explicit owner approval")]
    ApprovalRequired,
    #[error("secret injection is unavailable while recording is active")]
    RecordingActive,
    #[error("credential resolution failed")]
    CredentialUnavailable,
    #[error("focused secret write failed")]
    WriteFailed,
    #[error("secret injection clock failed")]
    Clock,
}

pub struct SecureSecretInjection<'a, R, W> {
    credentials: &'a EncryptedCredentialStore,
    authority: R,
    writer: W,
}

impl<'a, R, W> SecureSecretInjection<'a, R, W>
where
    R: SecretInjectionAuthorityResolver,
    W: FocusedSecretWriter,
{
    pub const fn new(credentials: &'a EncryptedCredentialStore, authority: R, writer: W) -> Self {
        Self {
            credentials,
            authority,
            writer,
        }
    }

    pub fn inject(
        &mut self,
        authenticated_profile_id: &ProfileId,
        request: SecretInjectionRequest,
        now: UtcTimestamp,
    ) -> Result<SecretInjectionReceipt, SecretInjectionError> {
        if authenticated_profile_id != &request.claimed_profile_id {
            return Err(SecretInjectionError::Unauthorized);
        }
        validate_target(&request.target)?;
        let current = self
            .authority
            .resolve_current(
                authenticated_profile_id,
                &request.computer_id,
                &request.task_key,
            )
            .map_err(|_| SecretInjectionError::StaleAuthority)?;
        if current.profile_id != *authenticated_profile_id
            || current.computer_id != request.computer_id
            || current.task_key != request.task_key
            || current.task_fencing_token != request.task_fencing_token
            || current.computer_revision != request.computer_revision
            || current.policy_revision != request.policy_revision
            || current.credential_ref != request.credential_ref
            || current.credential_owner != request.credential_ref.owner
            || current.target != request.target
        {
            return Err(SecretInjectionError::StaleAuthority);
        }
        if !current.enabled || !current.allow_secret_injection {
            return Err(SecretInjectionError::PolicyDenied);
        }
        if current.requires_owner_approval && !request.owner_approved {
            return Err(SecretInjectionError::ApprovalRequired);
        }
        if current.recording_active {
            return Err(SecretInjectionError::RecordingActive);
        }
        if current.max_secret_bytes == 0 || current.max_secret_bytes > MAX_INJECTION_BYTES {
            return Err(SecretInjectionError::PolicyDenied);
        }
        let target = request.target.clone();
        let maximum = current.max_secret_bytes;
        self.credentials
            .consume_write_only(
                &request.credential_ref,
                &current.credential_owner,
                |secret| {
                    if secret.is_empty() || secret.len() > maximum {
                        return Err(SecretInjectionError::PolicyDenied);
                    }
                    let mut buffer = ZeroingInjectionBuffer(secret.to_vec());
                    let result = match &target {
                        SecretInjectionTarget::FocusedField {
                            exact_origin,
                            frame_origin,
                            field_id,
                            ..
                        } => self
                            .writer
                            .write_focused_field(exact_origin, frame_origin, field_id, &buffer.0)
                            .map_err(|_| SecretInjectionError::WriteFailed),
                        SecretInjectionTarget::CredentialBroker {
                            exact_origin,
                            broker_id,
                        } => self
                            .writer
                            .write_credential_broker(exact_origin, broker_id, &buffer.0)
                            .map_err(|_| SecretInjectionError::WriteFailed),
                    };
                    buffer.0.fill(0);
                    result
                },
            )
            .map_err(|_| SecretInjectionError::CredentialUnavailable)??;
        Ok(SecretInjectionReceipt {
            operation_key: request.operation_key,
            profile_id: request.claimed_profile_id,
            computer_id: request.computer_id,
            task_key: request.task_key,
            task_fencing_token: request.task_fencing_token,
            computer_revision: request.computer_revision,
            policy_revision: request.policy_revision,
            target_kind: request.target.kind(),
            injected_at: now,
        })
    }
}

struct ZeroingInjectionBuffer(Vec<u8>);

impl Drop for ZeroingInjectionBuffer {
    fn drop(&mut self) {
        self.0.fill(0);
    }
}

fn validate_target(target: &SecretInjectionTarget) -> Result<(), SecretInjectionError> {
    if !valid_origin(target.exact_origin()) {
        return Err(SecretInjectionError::InvalidTarget);
    }
    match target {
        SecretInjectionTarget::FocusedField {
            exact_origin,
            frame_origin,
            field_id,
            ..
        } => {
            if frame_origin != exact_origin
                || field_id.is_empty()
                || field_id.len() > MAX_FIELD_ID_BYTES
                || !field_id
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
            {
                return Err(SecretInjectionError::InvalidTarget);
            }
        }
        SecretInjectionTarget::CredentialBroker { broker_id, .. } => {
            if broker_id.is_empty()
                || broker_id.len() > MAX_BROKER_ID_BYTES
                || !broker_id
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.'))
            {
                return Err(SecretInjectionError::InvalidTarget);
            }
        }
    }
    Ok(())
}

fn valid_origin(origin: &str) -> bool {
    let Some(authority) = origin.strip_prefix("https://") else {
        return false;
    };
    !authority.is_empty()
        && origin.len() <= MAX_ORIGIN_BYTES
        && !authority.contains(['/', '?', '#', '@'])
        && !authority.chars().any(char::is_whitespace)
}
