#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};

use serde::{Deserialize, Serialize};
use serde_json::Value;
use thiserror::Error;

pub const HOST_API_MIN_VERSION: u16 = 1;
pub const HOST_API_VERSION: u16 = 2;
pub const MANIFEST_VERSION: u16 = 2;
pub const MANIFEST_FILE: &str = "plugin.toml";
pub const MODULE_FILE: &str = "plugin.wasm";
pub const WIT_PACKAGE: &str = "keith:plugin@1.0.0";

pub mod bindings {
    pub use super::{
        HostRequest, HostResponse, PayloadFormat, PluginHostCall, PluginHostCallResult,
        PluginHttpRequest, PluginHttpResponse, PluginLogLevel, PluginOperation, PluginRisk,
        PluginStatus, PluginStorageRequest, PluginStreamFrame, PluginToolDescriptor,
    };

    pub trait Guest {
        fn describe_tools() -> Vec<PluginToolDescriptor>;
        fn describe_commands() -> Vec<PluginToolDescriptor>;
        fn invoke(request: HostRequest) -> HostResponse;
    }

    pub trait Host {
        type Error;

        /// # Errors
        ///
        /// Returns a host-defined error when the requested capability is denied or fails.
        fn call(request: PluginHostCall) -> Result<PluginHostCallResult, Self::Error>;
        fn cancelled(cancellation_id: &str) -> bool;
    }
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginHook {
    Activate,
    Health,
    Command,
    Tool,
    Migrate,
    Deactivate,
}

impl PluginHook {
    pub const fn export_name(self) -> &'static str {
        match self {
            Self::Activate => "keith_activate",
            Self::Health => "keith_health",
            Self::Command => "keith_command",
            Self::Tool => "keith_tool",
            Self::Migrate => "keith_migrate",
            Self::Deactivate => "keith_deactivate",
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginOperation {
    Activate,
    Health,
    Command,
    Tool,
    Migrate,
    Deactivate,
}

impl PluginOperation {
    pub const fn hook(self) -> PluginHook {
        match self {
            Self::Activate => PluginHook::Activate,
            Self::Health => PluginHook::Health,
            Self::Command => PluginHook::Command,
            Self::Tool => PluginHook::Tool,
            Self::Migrate => PluginHook::Migrate,
            Self::Deactivate => PluginHook::Deactivate,
        }
    }

    pub const fn needs_target(self) -> bool {
        matches!(self, Self::Command | Self::Tool)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginKind {
    WasiComponent,
    SeparateProcess,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginRisk {
    ReadOnly,
    Reversible,
    Consequential,
    Irreversible,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PayloadFormat {
    Json,
    Bytes,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginHttpRequest {
    pub method: String,
    pub url: String,
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
    #[serde(default)]
    pub body: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginHttpResponse {
    pub status: u16,
    #[serde(default)]
    pub headers: BTreeMap<String, String>,
    #[serde(default)]
    pub body: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(tag = "operation", rename_all = "snake_case")]
pub enum PluginStorageRequest {
    Get {
        namespace: String,
        key: String,
    },
    Put {
        namespace: String,
        key: String,
        value: Vec<u8>,
    },
    Delete {
        namespace: String,
        key: String,
    },
}

impl PluginStorageRequest {
    pub fn namespace(&self) -> &str {
        match self {
            Self::Get { namespace, .. }
            | Self::Put { namespace, .. }
            | Self::Delete { namespace, .. } => namespace,
        }
    }

    pub const fn requires_write(&self) -> bool {
        matches!(self, Self::Put { .. } | Self::Delete { .. })
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginLogLevel {
    Error,
    Warn,
    Info,
    Debug,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(tag = "call", rename_all = "snake_case")]
pub enum PluginHostCall {
    Http {
        request: PluginHttpRequest,
    },
    Credential {
        name: String,
    },
    Storage {
        request: PluginStorageRequest,
    },
    EmitEvent {
        topic: String,
        payload: Vec<u8>,
    },
    CreateArtifact {
        name: String,
        media_type: String,
        payload: Vec<u8>,
    },
    Clock,
    SafeLog {
        level: PluginLogLevel,
        message: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(tag = "result", content = "value", rename_all = "snake_case")]
pub enum PluginHostCallResult {
    Http(PluginHttpResponse),
    Credential(Vec<u8>),
    Storage(Option<Vec<u8>>),
    Artifact(String),
    Clock(u64),
    Empty,
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(tag = "kind", content = "scope", rename_all = "snake_case")]
pub enum PluginGrant {
    HttpHost(String),
    Credential(String),
    StorageRead(String),
    StorageWrite(String),
    EmitEvent,
    CreateArtifact,
    Clock,
    SafeLog,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)]
pub struct ResourceGrants {
    pub max_memory_bytes: usize,
    pub max_fuel: u64,
    pub max_input_bytes: usize,
    pub max_output_bytes: usize,
    #[serde(default = "default_wall_time_ms")]
    pub max_wall_time_ms: u64,
    #[serde(default = "default_concurrent_calls")]
    pub max_concurrent_calls: u32,
    #[serde(default = "default_host_call_bytes")]
    pub max_host_call_bytes: usize,
    #[serde(default = "default_storage_bytes")]
    pub max_storage_bytes: usize,
    #[serde(default)]
    pub readable_roots: Vec<String>,
    #[serde(default)]
    pub writable_roots: Vec<String>,
    #[serde(default)]
    pub network_hosts: Vec<String>,
    #[serde(default)]
    pub readable_storage_namespaces: BTreeSet<String>,
    #[serde(default)]
    pub writable_storage_namespaces: BTreeSet<String>,
    #[serde(default)]
    pub environment: BTreeMap<String, String>,
    #[serde(default)]
    pub credential_names: BTreeSet<String>,
    #[serde(default)]
    pub allow_events: bool,
    #[serde(default)]
    pub allow_artifacts: bool,
    #[serde(default)]
    pub allow_clock: bool,
    #[serde(default = "default_true")]
    pub allow_safe_logging: bool,
    #[serde(default)]
    pub allow_processes: bool,
}

const fn default_wall_time_ms() -> u64 {
    30_000
}

const fn default_concurrent_calls() -> u32 {
    1
}

const fn default_host_call_bytes() -> usize {
    64 * 1_024
}

const fn default_storage_bytes() -> usize {
    1024 * 1_024
}

const fn default_true() -> bool {
    true
}

impl Default for ResourceGrants {
    fn default() -> Self {
        Self {
            max_memory_bytes: 16 * 1_024 * 1_024,
            max_fuel: 1_000_000,
            max_input_bytes: 64 * 1_024,
            max_output_bytes: 64 * 1_024,
            max_wall_time_ms: default_wall_time_ms(),
            max_concurrent_calls: default_concurrent_calls(),
            max_host_call_bytes: default_host_call_bytes(),
            max_storage_bytes: default_storage_bytes(),
            readable_roots: Vec::new(),
            writable_roots: Vec::new(),
            network_hosts: Vec::new(),
            readable_storage_namespaces: BTreeSet::new(),
            writable_storage_namespaces: BTreeSet::new(),
            environment: BTreeMap::new(),
            credential_names: BTreeSet::new(),
            allow_events: false,
            allow_artifacts: false,
            allow_clock: false,
            allow_safe_logging: true,
            allow_processes: false,
        }
    }
}

impl ResourceGrants {
    pub fn has_ambient_authority(&self) -> bool {
        self.readable_roots.iter().any(|root| root == "/")
            || self.writable_roots.iter().any(|root| root == "/")
            || self.network_hosts.iter().any(|host| host == "*")
            || self.environment.keys().any(|name| name == "*")
            || self.credential_names.contains("*")
            || self.readable_storage_namespaces.contains("*")
            || self.writable_storage_namespaces.contains("*")
    }

    pub fn allows(&self, grant: &PluginGrant) -> bool {
        match grant {
            PluginGrant::HttpHost(host) => self.network_hosts.contains(host),
            PluginGrant::Credential(name) => self.credential_names.contains(name),
            PluginGrant::StorageRead(namespace) => {
                self.readable_storage_namespaces.contains(namespace)
                    || self.writable_storage_namespaces.contains(namespace)
            }
            PluginGrant::StorageWrite(namespace) => {
                self.writable_storage_namespaces.contains(namespace)
            }
            PluginGrant::EmitEvent => self.allow_events,
            PluginGrant::CreateArtifact => self.allow_artifacts,
            PluginGrant::Clock => self.allow_clock,
            PluginGrant::SafeLog => self.allow_safe_logging,
        }
    }

    fn validate(&self, manifest_version: u16) -> Result<(), ManifestError> {
        if self.max_memory_bytes == 0
            || self.max_fuel == 0
            || self.max_input_bytes == 0
            || self.max_output_bytes == 0
            || self.max_wall_time_ms == 0
            || self.max_concurrent_calls == 0
            || self.max_host_call_bytes == 0
            || self.max_storage_bytes == 0
        {
            return Err(ManifestError::InvalidLimit);
        }
        if self.has_ambient_authority() {
            return Err(ManifestError::AmbientAuthority);
        }
        if manifest_version >= MANIFEST_VERSION
            && (!self.readable_roots.is_empty()
                || !self.writable_roots.is_empty()
                || !self.environment.is_empty()
                || self.allow_processes)
        {
            return Err(ManifestError::AmbientAuthority);
        }
        if self
            .network_hosts
            .iter()
            .chain(self.readable_storage_namespaces.iter())
            .chain(self.writable_storage_namespaces.iter())
            .chain(self.credential_names.iter())
            .any(|scope| !valid_scope(scope))
        {
            return Err(ManifestError::InvalidGrant);
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginPublisher {
    pub id: String,
    pub name: String,
    pub key_id: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginDigest {
    pub algorithm: String,
    pub value: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginSignature {
    pub algorithm: String,
    pub key_id: String,
    pub value: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginMigrationContract {
    pub state_schema_version: u32,
    #[serde(default)]
    pub accepts_plugin_versions: BTreeSet<String>,
}

impl Default for PluginMigrationContract {
    fn default() -> Self {
        Self {
            state_schema_version: 1,
            accepts_plugin_versions: BTreeSet::new(),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginToolDescriptor {
    pub name: String,
    pub description: String,
    pub input_schema: String,
    pub output_schema: String,
    pub risk: PluginRisk,
    pub timeout_ms: u64,
    pub supports_cancellation: bool,
    pub streaming: bool,
    pub concurrency_limit: u32,
    #[serde(default)]
    pub required_grants: BTreeSet<PluginGrant>,
}

pub type PluginCommandDescriptor = PluginToolDescriptor;

impl PluginToolDescriptor {
    /// # Errors
    ///
    /// Returns an error when metadata, schemas, bounds, or required grants are invalid.
    pub fn validate(&self, grants: &ResourceGrants) -> Result<(), ManifestError> {
        if !valid_name(&self.name)
            || self.description.trim().is_empty()
            || self.description.len() > 4_096
            || self.timeout_ms == 0
            || self.timeout_ms > grants.max_wall_time_ms
            || self.concurrency_limit == 0
            || self.concurrency_limit > grants.max_concurrent_calls
            || validate_json_schema(&self.input_schema).is_err()
            || validate_json_schema(&self.output_schema).is_err()
        {
            return Err(ManifestError::InvalidDescriptor);
        }
        if self
            .required_grants
            .iter()
            .any(|grant| !grants.allows(grant))
        {
            return Err(ManifestError::InvalidGrant);
        }
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when the payload is not JSON satisfying the declared input schema.
    pub fn validate_input(&self, payload: &[u8]) -> Result<(), PayloadError> {
        validate_json_payload(&self.input_schema, payload)
    }

    /// # Errors
    ///
    /// Returns an error when the payload is not JSON satisfying the declared output schema.
    pub fn validate_output(&self, payload: &[u8]) -> Result<(), PayloadError> {
        validate_json_payload(&self.output_schema, payload)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginManifest {
    pub manifest_version: u16,
    pub id: String,
    pub name: String,
    pub version: String,
    pub host_api_min: u16,
    pub host_api_max: u16,
    pub kind: PluginKind,
    pub hooks: BTreeSet<PluginHook>,
    #[serde(default)]
    pub grants: ResourceGrants,
    #[serde(default)]
    pub publisher: Option<PluginPublisher>,
    #[serde(default)]
    pub digest: Option<PluginDigest>,
    #[serde(default)]
    pub signature: Option<PluginSignature>,
    #[serde(default)]
    pub tools: Vec<PluginToolDescriptor>,
    #[serde(default)]
    pub commands: Vec<PluginCommandDescriptor>,
    #[serde(default)]
    pub migration: Option<PluginMigrationContract>,
}

impl PluginManifest {
    /// # Errors
    ///
    /// Returns a bounded validation error for malformed or unsupported manifest input.
    pub fn parse(input: &str) -> Result<Self, ManifestError> {
        Self::parse_bounded(input, 64 * 1_024)
    }

    /// # Errors
    ///
    /// Returns an error for oversized, malformed, incompatible, or unsafe manifest input.
    pub fn parse_bounded(input: &str, max_bytes: usize) -> Result<Self, ManifestError> {
        if max_bytes == 0 || input.len() > max_bytes {
            return Err(ManifestError::TooLarge);
        }
        let manifest: Self =
            toml::from_str(input).map_err(|error| ManifestError::Toml(error.to_string()))?;
        manifest.validate()?;
        Ok(manifest)
    }

    /// # Errors
    ///
    /// Returns the first identity, compatibility, authority, descriptor, or provenance failure.
    pub fn validate(&self) -> Result<(), ManifestError> {
        if !valid_id(&self.id) || self.name.trim().is_empty() || !valid_version(&self.version) {
            return Err(ManifestError::InvalidIdentity);
        }
        if !(1..=MANIFEST_VERSION).contains(&self.manifest_version)
            || self.host_api_min > self.host_api_max
            || self.host_api_min > HOST_API_VERSION
            || self.host_api_max < HOST_API_MIN_VERSION
        {
            return Err(ManifestError::Incompatible);
        }
        if self.kind != PluginKind::WasiComponent {
            return Err(ManifestError::UnsupportedKind);
        }
        self.grants.validate(self.manifest_version)?;
        if self.manifest_version >= MANIFEST_VERSION {
            self.validate_v2_identity()?;
        }
        let mut names = BTreeSet::new();
        for descriptor in self.tools.iter().chain(&self.commands) {
            descriptor.validate(&self.grants)?;
            if !names.insert(&descriptor.name) {
                return Err(ManifestError::DuplicateDescriptor);
            }
        }
        if !self.tools.is_empty() && !self.hooks.contains(&PluginHook::Tool) {
            return Err(ManifestError::InvalidDescriptor);
        }
        if !self.commands.is_empty() && !self.hooks.contains(&PluginHook::Command) {
            return Err(ManifestError::InvalidDescriptor);
        }
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when the manifest and host API ranges do not overlap safely.
    pub fn negotiated_api_version(&self) -> Result<u16, ManifestError> {
        self.validate()?;
        Ok(self.host_api_max.min(HOST_API_VERSION))
    }

    pub fn descriptor(
        &self,
        operation: PluginOperation,
        target: &str,
    ) -> Option<&PluginToolDescriptor> {
        let descriptors = match operation {
            PluginOperation::Tool => &self.tools,
            PluginOperation::Command => &self.commands,
            _ => return None,
        };
        descriptors
            .iter()
            .find(|descriptor| descriptor.name == target)
    }

    fn validate_v2_identity(&self) -> Result<(), ManifestError> {
        let publisher = self
            .publisher
            .as_ref()
            .ok_or(ManifestError::MissingProvenance)?;
        let digest = self
            .digest
            .as_ref()
            .ok_or(ManifestError::MissingProvenance)?;
        let signature = self
            .signature
            .as_ref()
            .ok_or(ManifestError::MissingProvenance)?;
        if !valid_id(&publisher.id)
            || publisher.name.trim().is_empty()
            || !valid_scope(&publisher.key_id)
            || digest.algorithm != "sha256"
            || digest.value.len() != 64
            || !digest.value.bytes().all(|byte| byte.is_ascii_hexdigit())
            || signature.algorithm != "ed25519"
            || signature.key_id != publisher.key_id
            || signature.value.trim().is_empty()
            || self.migration.is_none()
        {
            return Err(ManifestError::MissingProvenance);
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginInvocation {
    pub interface_version: u16,
    pub invocation_id: String,
    pub operation: PluginOperation,
    pub target: Option<String>,
    pub payload_format: PayloadFormat,
    pub payload: Vec<u8>,
    pub cancellation_id: String,
}

impl PluginInvocation {
    /// # Errors
    ///
    /// Returns an error when the envelope, target, payload, schema, or version is invalid.
    pub fn validate(&self, manifest: &PluginManifest) -> Result<(), PayloadError> {
        if self.interface_version < manifest.host_api_min
            || self.interface_version > manifest.host_api_max
            || self.interface_version > HOST_API_VERSION
        {
            return Err(PayloadError::Incompatible);
        }
        if !valid_scope(&self.invocation_id)
            || !valid_scope(&self.cancellation_id)
            || self.payload.len() > manifest.grants.max_input_bytes
        {
            return Err(PayloadError::InvalidEnvelope);
        }
        if self.operation.needs_target() {
            let target = self
                .target
                .as_deref()
                .ok_or(PayloadError::InvalidEnvelope)?;
            let descriptor = manifest
                .descriptor(self.operation, target)
                .ok_or(PayloadError::UnknownTarget)?;
            if self.payload_format != PayloadFormat::Json {
                return Err(PayloadError::Schema);
            }
            descriptor.validate_input(&self.payload)?;
        } else if self.target.is_some() {
            return Err(PayloadError::InvalidEnvelope);
        }
        Ok(())
    }
}

pub type HostRequest = PluginInvocation;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginStatus {
    Completed,
    Cancelled,
    Denied,
    Failed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginStreamFrame {
    pub sequence: u64,
    pub payload_format: PayloadFormat,
    pub payload: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HostResponse {
    pub interface_version: u16,
    pub invocation_id: String,
    pub status: PluginStatus,
    pub payload_format: PayloadFormat,
    pub payload: Vec<u8>,
    #[serde(default)]
    pub stream: Vec<PluginStreamFrame>,
    pub safe_error: Option<String>,
}

impl HostResponse {
    /// # Errors
    ///
    /// Returns an error when the response does not match the request, schema, or output bounds.
    pub fn validate(
        &self,
        request: &HostRequest,
        manifest: &PluginManifest,
    ) -> Result<(), PayloadError> {
        let stream_bytes = self.stream.iter().try_fold(0_usize, |total, frame| {
            total.checked_add(frame.payload.len())
        });
        if self.interface_version != request.interface_version
            || self.invocation_id != request.invocation_id
            || stream_bytes
                .and_then(|stream| stream.checked_add(self.payload.len()))
                .is_none_or(|total| total > manifest.grants.max_output_bytes)
            || self
                .safe_error
                .as_ref()
                .is_some_and(|error| error.len() > 4_096)
            || self
                .stream
                .iter()
                .enumerate()
                .any(|(index, frame)| frame.sequence != index as u64)
        {
            return Err(PayloadError::InvalidEnvelope);
        }
        if request.operation.needs_target() && self.status == PluginStatus::Completed {
            let descriptor = manifest
                .descriptor(
                    request.operation,
                    request
                        .target
                        .as_deref()
                        .ok_or(PayloadError::InvalidEnvelope)?,
                )
                .ok_or(PayloadError::UnknownTarget)?;
            if self.payload_format != PayloadFormat::Json {
                return Err(PayloadError::Schema);
            }
            descriptor.validate_output(&self.payload)?;
            if !descriptor.streaming && !self.stream.is_empty() {
                return Err(PayloadError::InvalidEnvelope);
            }
        }
        Ok(())
    }
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum ManifestError {
    #[error("manifest TOML is invalid: {0}")]
    Toml(String),
    #[error("plugin identifier, name, or version is invalid")]
    InvalidIdentity,
    #[error("plugin manifest or host API version is incompatible")]
    Incompatible,
    #[error("plugin resource limits must be positive")]
    InvalidLimit,
    #[error("plugin requests ambient authority")]
    AmbientAuthority,
    #[error("plugin grant declaration is invalid or exceeds package grants")]
    InvalidGrant,
    #[error("plugin tool or command descriptor is invalid")]
    InvalidDescriptor,
    #[error("plugin tool and command names must be unique")]
    DuplicateDescriptor,
    #[error("plugin v2 publisher, digest, signature, or migration identity is incomplete")]
    MissingProvenance,
    #[error("separate-process plugins are not accepted by the embedded WASI host")]
    UnsupportedKind,
    #[error("plugin manifest exceeds the configured compatibility bound")]
    TooLarge,
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum PayloadError {
    #[error("plugin payload envelope is invalid or exceeds its bound")]
    InvalidEnvelope,
    #[error("plugin interface version is incompatible")]
    Incompatible,
    #[error("plugin invocation target is not declared")]
    UnknownTarget,
    #[error("plugin payload does not satisfy its JSON schema")]
    Schema,
    #[error("plugin payload encoding failed: {0}")]
    Encoding(String),
}

/// # Errors
///
/// Returns an error when serialization fails or the encoded envelope exceeds `max_bytes`.
pub fn encode_request(request: &HostRequest, max_bytes: usize) -> Result<Vec<u8>, PayloadError> {
    encode_bounded(request, max_bytes)
}

/// # Errors
///
/// Returns an error when decoding fails or the encoded envelope exceeds `max_bytes`.
pub fn decode_request(bytes: &[u8], max_bytes: usize) -> Result<HostRequest, PayloadError> {
    decode_bounded(bytes, max_bytes)
}

/// # Errors
///
/// Returns an error when serialization fails or the encoded envelope exceeds `max_bytes`.
pub fn encode_response(response: &HostResponse, max_bytes: usize) -> Result<Vec<u8>, PayloadError> {
    encode_bounded(response, max_bytes)
}

/// # Errors
///
/// Returns an error when decoding fails or the encoded envelope exceeds `max_bytes`.
pub fn decode_response(bytes: &[u8], max_bytes: usize) -> Result<HostResponse, PayloadError> {
    decode_bounded(bytes, max_bytes)
}

fn encode_bounded<T: Serialize>(value: &T, max_bytes: usize) -> Result<Vec<u8>, PayloadError> {
    let bytes =
        serde_json::to_vec(value).map_err(|error| PayloadError::Encoding(error.to_string()))?;
    if max_bytes == 0 || bytes.len() > max_bytes {
        return Err(PayloadError::InvalidEnvelope);
    }
    Ok(bytes)
}

fn decode_bounded<T: for<'de> Deserialize<'de>>(
    bytes: &[u8],
    max_bytes: usize,
) -> Result<T, PayloadError> {
    if max_bytes == 0 || bytes.len() > max_bytes {
        return Err(PayloadError::InvalidEnvelope);
    }
    serde_json::from_slice(bytes).map_err(|error| PayloadError::Encoding(error.to_string()))
}

fn validate_json_payload(schema: &str, payload: &[u8]) -> Result<(), PayloadError> {
    let schema: Value = serde_json::from_str(schema).map_err(|_| PayloadError::Schema)?;
    let value: Value = serde_json::from_slice(payload).map_err(|_| PayloadError::Schema)?;
    if value_matches_schema(&value, &schema, 0) {
        Ok(())
    } else {
        Err(PayloadError::Schema)
    }
}

fn validate_json_schema(schema: &str) -> Result<(), PayloadError> {
    if schema.len() > 64 * 1_024 {
        return Err(PayloadError::Schema);
    }
    let value: Value = serde_json::from_str(schema).map_err(|_| PayloadError::Schema)?;
    if !value.is_object() || !schema_shape_is_bounded(&value, 0) {
        return Err(PayloadError::Schema);
    }
    Ok(())
}

fn schema_shape_is_bounded(schema: &Value, depth: usize) -> bool {
    if depth > 16 {
        return false;
    }
    let Some(object) = schema.as_object() else {
        return false;
    };
    if let Some(kind) = object.get("type") {
        let valid = kind.as_str().is_some_and(|kind| {
            matches!(
                kind,
                "object" | "array" | "string" | "number" | "integer" | "boolean" | "null"
            )
        });
        if !valid {
            return false;
        }
    }
    if let Some(properties) = object.get("properties") {
        let Some(properties) = properties.as_object() else {
            return false;
        };
        if properties.len() > 128
            || properties
                .values()
                .any(|child| !schema_shape_is_bounded(child, depth + 1))
        {
            return false;
        }
    }
    if let Some(items) = object.get("items")
        && !schema_shape_is_bounded(items, depth + 1)
    {
        return false;
    }
    object.get("required").is_none_or(|required| {
        required.as_array().is_some_and(|items| {
            items.len() <= 128
                && items
                    .iter()
                    .all(|item| item.as_str().is_some_and(valid_name))
        })
    })
}

fn value_matches_schema(value: &Value, schema: &Value, depth: usize) -> bool {
    if depth > 16 {
        return false;
    }
    let Some(object) = schema.as_object() else {
        return false;
    };
    if let Some(allowed) = object.get("enum").and_then(Value::as_array)
        && !allowed.contains(value)
    {
        return false;
    }
    if let Some(kind) = object.get("type").and_then(Value::as_str) {
        let type_matches = match kind {
            "object" => value.is_object(),
            "array" => value.is_array(),
            "string" => value.is_string(),
            "number" => value.is_number(),
            "integer" => value.as_i64().is_some() || value.as_u64().is_some(),
            "boolean" => value.is_boolean(),
            "null" => value.is_null(),
            _ => false,
        };
        if !type_matches {
            return false;
        }
    }
    if let Some(properties) = object.get("properties").and_then(Value::as_object) {
        let Some(instance) = value.as_object() else {
            return false;
        };
        if object
            .get("required")
            .and_then(Value::as_array)
            .is_some_and(|required| {
                required
                    .iter()
                    .filter_map(Value::as_str)
                    .any(|name| !instance.contains_key(name))
            })
        {
            return false;
        }
        if properties.iter().any(|(name, child)| {
            instance
                .get(name)
                .is_some_and(|value| !value_matches_schema(value, child, depth + 1))
        }) {
            return false;
        }
        if object.get("additionalProperties") == Some(&Value::Bool(false))
            && instance.keys().any(|name| !properties.contains_key(name))
        {
            return false;
        }
    }
    if let Some(items) = object.get("items")
        && value.as_array().is_some_and(|values| {
            values
                .iter()
                .any(|value| !value_matches_schema(value, items, depth + 1))
        })
    {
        return false;
    }
    true
}

fn valid_id(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
}

fn valid_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value.bytes().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || matches!(byte, b'-' | b'_' | b'.')
        })
}

fn valid_scope(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 256
        && !value.contains('*')
        && !value.contains("..")
        && !value.chars().any(char::is_control)
}

fn valid_version(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'-' | b'+'))
}

pub trait FirstPartyExtension: Send + Sync {
    fn id(&self) -> &'static str;
    fn hooks(&self) -> &'static [PluginHook];
}

#[derive(Default)]
pub struct NativeExtensionRegistry {
    extensions: BTreeMap<&'static str, Box<dyn FirstPartyExtension>>,
}

impl NativeExtensionRegistry {
    pub fn register(&mut self, extension: impl FirstPartyExtension + 'static) -> bool {
        self.extensions
            .insert(extension.id(), Box::new(extension))
            .is_none()
    }

    pub fn get(&self, id: &str) -> Option<&dyn FirstPartyExtension> {
        self.extensions.get(id).map(Box::as_ref)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn descriptor(name: &str, streaming: bool) -> PluginToolDescriptor {
        PluginToolDescriptor {
            name: name.to_owned(),
            description: "Echoes one bounded message".to_owned(),
            input_schema: r#"{"type":"object","required":["message"],"properties":{"message":{"type":"string"}},"additionalProperties":false}"#.to_owned(),
            output_schema: r#"{"type":"object","required":["echo"],"properties":{"echo":{"type":"string"}},"additionalProperties":false}"#.to_owned(),
            risk: PluginRisk::ReadOnly,
            timeout_ms: 1_000,
            supports_cancellation: true,
            streaming,
            concurrency_limit: 1,
            required_grants: BTreeSet::from([PluginGrant::SafeLog]),
        }
    }

    fn manifest() -> PluginManifest {
        PluginManifest {
            manifest_version: MANIFEST_VERSION,
            id: "sample-plugin".to_owned(),
            name: "Sample".to_owned(),
            version: "1.0.0".to_owned(),
            host_api_min: HOST_API_VERSION,
            host_api_max: HOST_API_VERSION,
            kind: PluginKind::WasiComponent,
            hooks: BTreeSet::from([PluginHook::Activate, PluginHook::Tool]),
            grants: ResourceGrants::default(),
            publisher: Some(PluginPublisher {
                id: "sample-publisher".to_owned(),
                name: "Sample Publisher".to_owned(),
                key_id: "publisher-key-1".to_owned(),
            }),
            digest: Some(PluginDigest {
                algorithm: "sha256".to_owned(),
                value: "ab".repeat(32),
            }),
            signature: Some(PluginSignature {
                algorithm: "ed25519".to_owned(),
                key_id: "publisher-key-1".to_owned(),
                value: "signed-package".to_owned(),
            }),
            tools: vec![descriptor("echo", false)],
            commands: Vec::new(),
            migration: Some(PluginMigrationContract::default()),
        }
    }

    #[test]
    fn abi_manifest_requires_precise_provenance_descriptors_and_grants() {
        assert!(manifest().validate().is_ok());
        let mut ambient = manifest();
        ambient.grants.network_hosts.push("*".to_owned());
        assert_eq!(ambient.validate(), Err(ManifestError::AmbientAuthority));
        let mut unsigned = manifest();
        unsigned.signature = None;
        assert_eq!(unsigned.validate(), Err(ManifestError::MissingProvenance));
        let mut undeclared = manifest();
        undeclared.tools[0]
            .required_grants
            .insert(PluginGrant::Clock);
        assert_eq!(undeclared.validate(), Err(ManifestError::InvalidGrant));
    }

    #[test]
    fn abi_request_response_round_trip_and_schema_validation_are_bounded() {
        let manifest = manifest();
        let request = HostRequest {
            interface_version: HOST_API_VERSION,
            invocation_id: "invocation-1".to_owned(),
            operation: PluginOperation::Tool,
            target: Some("echo".to_owned()),
            payload_format: PayloadFormat::Json,
            payload: br#"{"message":"hello"}"#.to_vec(),
            cancellation_id: "cancel-1".to_owned(),
        };
        request.validate(&manifest).expect("request validates");
        let encoded = encode_request(&request, 4_096).expect("encode request");
        assert_eq!(decode_request(&encoded, 4_096), Ok(request.clone()));

        let response = HostResponse {
            interface_version: HOST_API_VERSION,
            invocation_id: request.invocation_id.clone(),
            status: PluginStatus::Completed,
            payload_format: PayloadFormat::Json,
            payload: br#"{"echo":"hello"}"#.to_vec(),
            stream: Vec::new(),
            safe_error: None,
        };
        response
            .validate(&request, &manifest)
            .expect("response validates");

        let mut invalid = request;
        invalid.payload = br#"{"unexpected":true}"#.to_vec();
        assert_eq!(invalid.validate(&manifest), Err(PayloadError::Schema));
    }

    #[test]
    fn abi_v1_manifest_remains_compatible_without_claiming_v2_provenance() {
        let legacy = PluginManifest {
            manifest_version: 1,
            id: "legacy".to_owned(),
            name: "Legacy".to_owned(),
            version: "1.0.0".to_owned(),
            host_api_min: 1,
            host_api_max: 1,
            kind: PluginKind::WasiComponent,
            hooks: BTreeSet::new(),
            grants: ResourceGrants::default(),
            publisher: None,
            digest: None,
            signature: None,
            tools: Vec::new(),
            commands: Vec::new(),
            migration: None,
        };
        assert_eq!(legacy.negotiated_api_version(), Ok(1));
    }
}
