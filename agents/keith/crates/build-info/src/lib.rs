#![forbid(unsafe_code)]

use std::collections::BTreeSet;

use keith_agent_types::{CURRENT_PROTOCOL_VERSION, CURRENT_SCHEMA_VERSION};
use serde::{Deserialize, Serialize};

pub const BUILD_ID: &str = match option_env!("KEITH_BUILD_ID") {
    Some(value) => value,
    None => concat!(env!("CARGO_PKG_VERSION"), "+development"),
};

pub const DAEMON_FEATURES: &[&str] = &[
    "attachment_staging",
    "background_controls",
    "branching",
    "catalog",
    "children",
    "confirmations",
    "delivery_dispatch",
    "export",
    "framed_json",
    "goals",
    "memory_queries",
    "replay",
    "schedules",
    "scheduler_dispatch",
    "session_lifecycle",
    "snapshots",
    "steering",
    "worker_leases",
    "worker_routing",
];
pub const WORKER_FEATURES: &[&str] = &[
    "actions",
    "agent_loop",
    "anthropic",
    "artifacts",
    "attention",
    "awareness",
    "browser",
    "children",
    "commitments",
    "delivery_outbox",
    "goals",
    "kernels",
    "knowledge",
    "mcp",
    "memory",
    "openai",
    "plans",
    "plugins",
    "provider_catalog",
    "refinement",
    "resource_governor",
    "retrieval",
    "scheduler",
    "sessions",
    "skills",
    "telemetry",
    "tools",
    "waiting",
    "workspace",
];

#[derive(Clone, Debug, Eq, PartialEq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct BuildReport {
    pub component: String,
    pub package_version: String,
    pub build_id: String,
    pub protocol_version: String,
    pub storage_schema: String,
    pub enabled_features: BTreeSet<String>,
}

pub fn daemon_report() -> BuildReport {
    BuildReport::current("daemon", DAEMON_FEATURES)
}

pub fn worker_report() -> BuildReport {
    BuildReport::current("worker", WORKER_FEATURES)
}

impl BuildReport {
    pub fn current(component: &str, enabled_features: &[&str]) -> Self {
        Self {
            component: component.to_owned(),
            package_version: env!("CARGO_PKG_VERSION").to_owned(),
            build_id: BUILD_ID.to_owned(),
            protocol_version: CURRENT_PROTOCOL_VERSION.to_string(),
            storage_schema: CURRENT_SCHEMA_VERSION.to_string(),
            enabled_features: enabled_features
                .iter()
                .map(|feature| (*feature).to_owned())
                .collect(),
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn report_exposes_all_release_compatibility_dimensions_deterministically() {
        let report = BuildReport::current("worker", &["tools", "sessions", "tools"]);
        assert_eq!(report.component, "worker");
        assert!(!report.build_id.is_empty());
        assert_eq!(
            report.protocol_version,
            CURRENT_PROTOCOL_VERSION.to_string()
        );
        assert_eq!(report.storage_schema, CURRENT_SCHEMA_VERSION.to_string());
        assert_eq!(
            report.enabled_features.into_iter().collect::<Vec<_>>(),
            ["sessions", "tools"]
        );
    }
}
#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesBuildInfo {
    pub release_version: String,
    pub build_id: String,
    pub source_revision: String,
    pub target_triple: String,
    pub schema_version: u32,
    pub native_protocol_major: u16,
    pub native_protocol_minor: u16,
    pub teammates_protocol_major: u16,
    pub teammates_protocol_minor: u16,
    pub teammates_protocol_producer: String,
    pub package_manifest_sha256: String,
    pub self_hosted: bool,
    pub enabled_features: std::collections::BTreeSet<String>,
    pub external_runtime_dependencies: Vec<ExternalRuntimeDependency>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ExternalDependencyClass {
    LocalSystem,
    UserConfiguredProvider,
    OptionalExternalChannel,
    HostedControlPlane,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExternalRuntimeDependency {
    pub name: String,
    pub class: ExternalDependencyClass,
    pub required: bool,
    pub endpoint_origin: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TeammatesBuildCompatibilityError {
    Missing(&'static str),
    InvalidDigest,
    UnsupportedSchema {
        found: u32,
        minimum: u32,
        maximum: u32,
    },
    UnsupportedProtocol {
        found: u16,
        supported: u16,
    },
    WrongTarget {
        built: String,
        running: String,
    },
    NotSelfHosted,
    HostedControlPlaneDependency(String),
    RequiredProviderDependency(String),
}

impl std::fmt::Display for TeammatesBuildCompatibilityError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Missing(field) => write!(formatter, "missing build field {field}"),
            Self::InvalidDigest => formatter.write_str("invalid package manifest digest"),
            Self::UnsupportedSchema {
                found,
                minimum,
                maximum,
            } => write!(
                formatter,
                "schema {found} is outside supported range {minimum}..={maximum}"
            ),
            Self::UnsupportedProtocol { found, supported } => {
                write!(
                    formatter,
                    "protocol major {found} is incompatible with {supported}"
                )
            }
            Self::WrongTarget { built, running } => {
                write!(formatter, "package target {built} cannot run on {running}")
            }
            Self::NotSelfHosted => formatter.write_str("package is not self-hosted"),
            Self::HostedControlPlaneDependency(name) => {
                write!(
                    formatter,
                    "hosted control-plane dependency is forbidden: {name}"
                )
            }
            Self::RequiredProviderDependency(name) => {
                write!(
                    formatter,
                    "provider dependency must remain user-configured: {name}"
                )
            }
        }
    }
}

impl std::error::Error for TeammatesBuildCompatibilityError {}

impl TeammatesBuildInfo {
    pub fn verify_compatibility(
        &self,
        running_target_triple: &str,
        minimum_schema: u32,
        maximum_schema: u32,
        supported_native_protocol_major: u16,
        supported_teammates_protocol_major: u16,
    ) -> Result<(), TeammatesBuildCompatibilityError> {
        for (name, value) in [
            ("release_version", self.release_version.as_str()),
            ("build_id", self.build_id.as_str()),
            ("source_revision", self.source_revision.as_str()),
            ("target_triple", self.target_triple.as_str()),
            (
                "teammates_protocol_producer",
                self.teammates_protocol_producer.as_str(),
            ),
        ] {
            if value.trim().is_empty() {
                return Err(TeammatesBuildCompatibilityError::Missing(name));
            }
        }
        if !is_sha256(&self.package_manifest_sha256) {
            return Err(TeammatesBuildCompatibilityError::InvalidDigest);
        }
        if !self.self_hosted {
            return Err(TeammatesBuildCompatibilityError::NotSelfHosted);
        }
        if self.schema_version < minimum_schema || self.schema_version > maximum_schema {
            return Err(TeammatesBuildCompatibilityError::UnsupportedSchema {
                found: self.schema_version,
                minimum: minimum_schema,
                maximum: maximum_schema,
            });
        }
        if self.native_protocol_major != supported_native_protocol_major {
            return Err(TeammatesBuildCompatibilityError::UnsupportedProtocol {
                found: self.native_protocol_major,
                supported: supported_native_protocol_major,
            });
        }
        if self.teammates_protocol_major != supported_teammates_protocol_major {
            return Err(TeammatesBuildCompatibilityError::UnsupportedProtocol {
                found: self.teammates_protocol_major,
                supported: supported_teammates_protocol_major,
            });
        }
        if self.target_triple != running_target_triple {
            return Err(TeammatesBuildCompatibilityError::WrongTarget {
                built: self.target_triple.clone(),
                running: running_target_triple.to_owned(),
            });
        }
        for dependency in &self.external_runtime_dependencies {
            if dependency.name.trim().is_empty() {
                return Err(TeammatesBuildCompatibilityError::Missing(
                    "external dependency name",
                ));
            }
            match dependency.class {
                ExternalDependencyClass::HostedControlPlane => {
                    return Err(
                        TeammatesBuildCompatibilityError::HostedControlPlaneDependency(
                            dependency.name.clone(),
                        ),
                    );
                }
                ExternalDependencyClass::UserConfiguredProvider if dependency.required => {
                    return Err(
                        TeammatesBuildCompatibilityError::RequiredProviderDependency(
                            dependency.name.clone(),
                        ),
                    );
                }
                ExternalDependencyClass::LocalSystem
                    if dependency
                        .endpoint_origin
                        .as_deref()
                        .is_some_and(|origin| !is_local_origin(origin)) =>
                {
                    return Err(
                        TeammatesBuildCompatibilityError::HostedControlPlaneDependency(
                            dependency.name.clone(),
                        ),
                    );
                }
                ExternalDependencyClass::LocalSystem
                | ExternalDependencyClass::UserConfiguredProvider
                | ExternalDependencyClass::OptionalExternalChannel => {}
            }
        }
        Ok(())
    }
}

fn is_sha256(value: &str) -> bool {
    value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

fn is_local_origin(origin: &str) -> bool {
    let normalized = origin.to_ascii_lowercase();
    normalized.starts_with("unix:")
        || normalized.starts_with("file:")
        || normalized.starts_with("http://127.0.0.1")
        || normalized.starts_with("http://localhost")
        || normalized.starts_with("http://[::1]")
        || normalized.starts_with("https://127.0.0.1")
        || normalized.starts_with("https://localhost")
        || normalized.starts_with("https://[::1]")
}
