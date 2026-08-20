#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};

use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const HOST_API_VERSION: u16 = 1;
pub const MANIFEST_FILE: &str = "plugin.toml";
pub const MODULE_FILE: &str = "plugin.wasm";

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

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginKind {
    WasiComponent,
    SeparateProcess,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResourceGrants {
    pub max_memory_bytes: usize,
    pub max_fuel: u64,
    pub max_input_bytes: usize,
    pub max_output_bytes: usize,
    #[serde(default)]
    pub readable_roots: Vec<String>,
    #[serde(default)]
    pub writable_roots: Vec<String>,
    #[serde(default)]
    pub network_hosts: Vec<String>,
    #[serde(default)]
    pub environment: BTreeMap<String, String>,
    #[serde(default)]
    pub credential_names: BTreeSet<String>,
    #[serde(default)]
    pub allow_clock: bool,
    #[serde(default)]
    pub allow_processes: bool,
}

impl Default for ResourceGrants {
    fn default() -> Self {
        Self {
            max_memory_bytes: 16 * 1_024 * 1_024,
            max_fuel: 1_000_000,
            max_input_bytes: 64 * 1_024,
            max_output_bytes: 64 * 1_024,
            readable_roots: Vec::new(),
            writable_roots: Vec::new(),
            network_hosts: Vec::new(),
            environment: BTreeMap::new(),
            credential_names: BTreeSet::new(),
            allow_clock: false,
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
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HostRequest {
    pub interface_version: u16,
    pub operation: String,
    pub payload: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HostResponse {
    pub status: u16,
    pub payload: Vec<u8>,
    pub safe_error: Option<String>,
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
    #[error("separate-process plugins are not accepted by the embedded WASI host")]
    UnsupportedKind,
    #[error("plugin manifest exceeds the configured compatibility bound")]
    TooLarge,
}

impl PluginManifest {
    /// Parses and validates a third-party plugin manifest.
    ///
    /// # Errors
    ///
    /// Returns a bounded, safe validation error for malformed or incompatible input.
    pub fn parse(input: &str) -> Result<Self, ManifestError> {
        Self::parse_bounded(input, 64 * 1_024)
    }

    /// Parses a supported manifest version under an explicit compatibility byte bound.
    ///
    /// # Errors
    ///
    /// Returns a bounded, safe validation error for oversized, malformed, or incompatible input.
    pub fn parse_bounded(input: &str, max_bytes: usize) -> Result<Self, ManifestError> {
        if max_bytes == 0 || input.len() > max_bytes {
            return Err(ManifestError::TooLarge);
        }
        let manifest: Self =
            toml::from_str(input).map_err(|error| ManifestError::Toml(error.to_string()))?;
        manifest.validate()?;
        Ok(manifest)
    }

    /// Validates identity, compatibility, execution kind, and absence of ambient grants.
    ///
    /// # Errors
    ///
    /// Returns the first manifest policy violation.
    pub fn validate(&self) -> Result<(), ManifestError> {
        let valid_id = !self.id.is_empty()
            && self.id.len() <= 128
            && self
                .id
                .bytes()
                .all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-');
        let valid_version = !self.version.is_empty()
            && self
                .version
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'-' | b'+'));
        if !valid_id || self.name.trim().is_empty() || !valid_version {
            return Err(ManifestError::InvalidIdentity);
        }
        if self.manifest_version != 1
            || self.host_api_min > HOST_API_VERSION
            || self.host_api_max < HOST_API_VERSION
        {
            return Err(ManifestError::Incompatible);
        }
        if self.grants.max_memory_bytes == 0
            || self.grants.max_fuel == 0
            || self.grants.max_input_bytes == 0
            || self.grants.max_output_bytes == 0
        {
            return Err(ManifestError::InvalidLimit);
        }
        if self.grants.has_ambient_authority() {
            return Err(ManifestError::AmbientAuthority);
        }
        if self.kind != PluginKind::WasiComponent {
            return Err(ManifestError::UnsupportedKind);
        }
        Ok(())
    }
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

    fn manifest() -> PluginManifest {
        PluginManifest {
            manifest_version: 1,
            id: "sample-plugin".to_owned(),
            name: "Sample".to_owned(),
            version: "1.0.0".to_owned(),
            host_api_min: 1,
            host_api_max: 1,
            kind: PluginKind::WasiComponent,
            hooks: BTreeSet::from([PluginHook::Activate, PluginHook::Health]),
            grants: ResourceGrants::default(),
        }
    }

    #[test]
    fn rejects_incompatible_and_ambient_manifests() {
        assert!(manifest().validate().is_ok());
        let mut ambient = manifest();
        ambient.grants.network_hosts.push("*".to_owned());
        assert_eq!(ambient.validate(), Err(ManifestError::AmbientAuthority));
        let mut incompatible = manifest();
        incompatible.host_api_min = 2;
        assert_eq!(incompatible.validate(), Err(ManifestError::Incompatible));
    }
}
