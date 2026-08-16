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
