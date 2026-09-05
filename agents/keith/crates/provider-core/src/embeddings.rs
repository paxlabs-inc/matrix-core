//! Provider-neutral embedding transport contracts, independent of chat models.
//!
//! These values describe and validate an encoder boundary. They do not establish
//! that a provider is configured, trained, available, indexed, or used by memory.
//! Adapters must bound transport bytes before decoding, honor cancellation and
//! the request timeout, and preserve the declared query/document role.

use std::collections::BTreeSet;
use std::fmt::{self, Debug};

use keith_agent_types::{EntityId, SchemaVersion};
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{CancellationToken, ProviderCredential, ProviderError};

pub const EMBEDDING_CONTRACT_VERSION: SchemaVersion = SchemaVersion::new(1, 0);

const MAX_IDENTITY_BYTES: usize = 512;
const MAX_DIMENSIONS: u32 = 65_536;
const MAX_BATCH_ITEMS: u32 = 4_096;
const MAX_INPUT_BYTES: u64 = 4 * 1024 * 1024;
const MAX_BATCH_BYTES: u64 = 64 * 1024 * 1024;
const MAX_VECTOR_BYTES: u64 = 64 * 1024 * 1024;
const MAX_TIMEOUT_MS: u64 = 600_000;
const UNIT_NORM_TOLERANCE: f64 = 1.0e-3;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EmbeddingRole {
    Query,
    Document,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EmbeddingDistance {
    Cosine,
    DotProduct,
    Euclidean,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EmbeddingNormalization {
    None,
    UnitL2,
}

/// Every field participates in compatibility. Equal dimensions alone do not.
///
/// `revision` identifies the encoder deployment/revision, and
/// `representation_version` identifies the input transformation. Query and
/// document encodings may share this identity only when the adapter explicitly
/// declares both roles in the same descriptor.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmbeddingSpaceIdentity {
    pub version: SchemaVersion,
    pub provider: String,
    pub model: String,
    pub revision: String,
    pub dimensions: u32,
    pub distance: EmbeddingDistance,
    pub normalization: EmbeddingNormalization,
    pub representation_version: String,
}

impl EmbeddingSpaceIdentity {
    /// # Errors
    ///
    /// Rejects unsupported versions, missing/unsafe identities and dimensions.
    pub fn validate(&self) -> Result<(), EmbeddingContractError> {
        validate_version(self.version)?;
        if ![
            &self.provider,
            &self.model,
            &self.revision,
            &self.representation_version,
        ]
        .into_iter()
        .all(|value| valid_identity(value))
            || self.dimensions == 0
            || self.dimensions > MAX_DIMENSIONS
        {
            return Err(EmbeddingContractError::InvalidIdentity);
        }
        Ok(())
    }
}

/// Adapter limits must be explicit and may only narrow the contract ceilings.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmbeddingLimits {
    pub max_batch_items: u32,
    pub max_input_bytes: u64,
    pub max_batch_bytes: u64,
    /// Bound on the complete batch's decoded f32 vector storage.
    pub max_vector_bytes: u64,
    pub max_dimensions: u32,
    pub max_timeout_ms: u64,
}

impl EmbeddingLimits {
    /// # Errors
    ///
    /// Rejects zero, inconsistent or unbounded limits.
    pub fn validate(&self) -> Result<(), EmbeddingContractError> {
        if self.max_batch_items == 0
            || self.max_batch_items > MAX_BATCH_ITEMS
            || self.max_input_bytes == 0
            || self.max_input_bytes > MAX_INPUT_BYTES
            || self.max_batch_bytes < self.max_input_bytes
            || self.max_batch_bytes > MAX_BATCH_BYTES
            || self.max_vector_bytes == 0
            || self.max_vector_bytes > MAX_VECTOR_BYTES
            || self.max_dimensions == 0
            || self.max_dimensions > MAX_DIMENSIONS
            || self.max_timeout_ms == 0
            || self.max_timeout_ms > MAX_TIMEOUT_MS
        {
            return Err(EmbeddingContractError::InvalidLimits);
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmbeddingDescriptor {
    pub space: EmbeddingSpaceIdentity,
    pub supported_roles: Vec<EmbeddingRole>,
    pub limits: EmbeddingLimits,
}

impl EmbeddingDescriptor {
    /// # Errors
    ///
    /// Rejects invalid identities, limits or duplicate/empty supported roles.
    pub fn validate(&self) -> Result<(), EmbeddingContractError> {
        self.space.validate()?;
        self.limits.validate()?;
        let roles = self.supported_roles.iter().collect::<BTreeSet<_>>();
        if roles.is_empty() || roles.len() != self.supported_roles.len() {
            return Err(EmbeddingContractError::UnsupportedRole);
        }
        if self.space.dimensions > self.limits.max_dimensions
            || u64::from(self.space.dimensions) * 4 > self.limits.max_vector_bytes
        {
            return Err(EmbeddingContractError::InvalidLimits);
        }
        Ok(())
    }
}

#[derive(Clone, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmbeddingInput {
    pub id: EntityId,
    pub text: String,
}

impl Debug for EmbeddingInput {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("EmbeddingInput")
            .field("id", &self.id)
            .field("text_bytes", &self.text.len())
            .finish_non_exhaustive()
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmbeddingRequest {
    pub version: SchemaVersion,
    pub request_id: EntityId,
    pub space: EmbeddingSpaceIdentity,
    pub role: EmbeddingRole,
    pub inputs: Vec<EmbeddingInput>,
    pub timeout_ms: u64,
}

impl EmbeddingRequest {
    /// Validate before handing input or credentials to a transport adapter.
    ///
    /// # Errors
    ///
    /// Rejects incompatible spaces/roles, duplicate IDs, empty inputs or any
    /// declared size/time limit. No input is silently truncated.
    pub fn validate(&self, descriptor: &EmbeddingDescriptor) -> Result<(), EmbeddingContractError> {
        validate_version(self.version)?;
        descriptor.validate()?;
        if self.space != descriptor.space {
            return Err(EmbeddingContractError::RequestSpaceMismatch);
        }
        if !descriptor.supported_roles.contains(&self.role) {
            return Err(EmbeddingContractError::UnsupportedRole);
        }
        if self.inputs.is_empty()
            || self.inputs.len() > descriptor.limits.max_batch_items as usize
            || self.timeout_ms == 0
            || self.timeout_ms > descriptor.limits.max_timeout_ms
        {
            return Err(EmbeddingContractError::InvalidRequest);
        }
        let vector_bytes = u64::try_from(self.inputs.len())
            .ok()
            .and_then(|count| count.checked_mul(u64::from(self.space.dimensions)))
            .and_then(|components| components.checked_mul(4))
            .ok_or(EmbeddingContractError::InvalidRequest)?;
        if vector_bytes > descriptor.limits.max_vector_bytes {
            return Err(EmbeddingContractError::InvalidRequest);
        }
        let mut identities = BTreeSet::new();
        let mut bytes = 0_u64;
        for input in &self.inputs {
            let input_bytes = u64::try_from(input.text.len())
                .map_err(|_| EmbeddingContractError::InvalidRequest)?;
            if input.text.trim().is_empty()
                || input_bytes > descriptor.limits.max_input_bytes
                || !identities.insert(&input.id)
            {
                return Err(EmbeddingContractError::InvalidRequest);
            }
            bytes = bytes
                .checked_add(input_bytes)
                .ok_or(EmbeddingContractError::InvalidRequest)?;
        }
        if bytes > descriptor.limits.max_batch_bytes {
            return Err(EmbeddingContractError::InvalidRequest);
        }
        Ok(())
    }
}

#[derive(Clone, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmbeddingVector {
    pub input_id: EntityId,
    pub values: Vec<f32>,
}

impl Debug for EmbeddingVector {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("EmbeddingVector")
            .field("input_id", &self.input_id)
            .field("dimensions", &self.values.len())
            .finish_non_exhaustive()
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmbeddingUsage {
    /// None means the provider did not report usage, rather than zero usage.
    pub input_tokens: Option<u64>,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EmbeddingResponse {
    pub version: SchemaVersion,
    pub request_id: EntityId,
    pub space: EmbeddingSpaceIdentity,
    pub role: EmbeddingRole,
    pub vectors: Vec<EmbeddingVector>,
    pub usage: EmbeddingUsage,
}

impl EmbeddingResponse {
    /// Validate every response before indexing or scoring its vectors.
    ///
    /// Output order may differ; input IDs must provide exact, unique coverage.
    /// No vector is silently padded, normalized or converted to another space.
    ///
    /// # Errors
    ///
    /// Rejects incompatible identity, incomplete coverage, invalid dimensions,
    /// nonfinite/zero vectors and a false unit-normalization declaration.
    pub fn validate(
        &self,
        request: &EmbeddingRequest,
        descriptor: &EmbeddingDescriptor,
    ) -> Result<(), EmbeddingContractError> {
        validate_version(self.version)?;
        request.validate(descriptor)?;
        if self.request_id != request.request_id
            || self.space != request.space
            || self.role != request.role
        {
            return Err(EmbeddingContractError::ResponseIdentityMismatch);
        }
        if self.vectors.len() != request.inputs.len() {
            return Err(EmbeddingContractError::ResponseCoverage);
        }
        let expected = request
            .inputs
            .iter()
            .map(|input| &input.id)
            .collect::<BTreeSet<_>>();
        let mut observed = BTreeSet::new();
        for vector in &self.vectors {
            if !expected.contains(&vector.input_id) || !observed.insert(&vector.input_id) {
                return Err(EmbeddingContractError::ResponseCoverage);
            }
            if vector.values.len() != self.space.dimensions as usize {
                return Err(EmbeddingContractError::DimensionMismatch);
            }
            if vector.values.iter().any(|value| !value.is_finite()) {
                return Err(EmbeddingContractError::InvalidVector);
            }
            let norm_squared = vector
                .values
                .iter()
                .map(|value| f64::from(*value).powi(2))
                .sum::<f64>();
            if !norm_squared.is_finite() || norm_squared <= 0.0 {
                return Err(EmbeddingContractError::InvalidVector);
            }
            if self.space.normalization == EmbeddingNormalization::UnitL2
                && (norm_squared.sqrt() - 1.0).abs() > UNIT_NORM_TOLERANCE
            {
                return Err(EmbeddingContractError::NormalizationMismatch);
            }
        }
        Ok(())
    }
}

/// A separately configured encoder; implementing this trait does not certify
/// semantic quality. Local adapters accept no credential. Hosted adapters may
/// receive an opaque credential resolved by the host, never by memory records.
pub trait EmbeddingProvider: Send + Sync {
    fn descriptor(&self) -> &EmbeddingDescriptor;

    /// # Errors
    ///
    /// Returns typed transport, authentication, cancellation or provider errors.
    /// Callers validate the request before dispatch and the response before use.
    fn embed(
        &self,
        request: &EmbeddingRequest,
        credential: Option<&ProviderCredential>,
        cancellation: &CancellationToken,
    ) -> Result<EmbeddingResponse, ProviderError>;
}

#[derive(Clone, Copy, Debug, Eq, Error, PartialEq)]
pub enum EmbeddingContractError {
    #[error("embedding contract version is unsupported")]
    UnsupportedVersion,
    #[error("embedding space identity is invalid")]
    InvalidIdentity,
    #[error("embedding limits are invalid")]
    InvalidLimits,
    #[error("embedding role is unsupported or duplicated")]
    UnsupportedRole,
    #[error("embedding request space differs from the configured encoder")]
    RequestSpaceMismatch,
    #[error("embedding request has invalid input identities or exceeds a declared bound")]
    InvalidRequest,
    #[error("embedding response identity does not match the request")]
    ResponseIdentityMismatch,
    #[error("embedding response does not uniquely cover every input")]
    ResponseCoverage,
    #[error("embedding vector dimensions differ from the declared space")]
    DimensionMismatch,
    #[error("embedding vector is nonfinite or zero")]
    InvalidVector,
    #[error("embedding vector violates its declared normalization")]
    NormalizationMismatch,
}

fn validate_version(version: SchemaVersion) -> Result<(), EmbeddingContractError> {
    if version == EMBEDDING_CONTRACT_VERSION {
        Ok(())
    } else {
        Err(EmbeddingContractError::UnsupportedVersion)
    }
}

fn valid_identity(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_IDENTITY_BYTES
        && value.trim() == value
        && !value.chars().any(char::is_control)
}
