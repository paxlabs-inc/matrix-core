use std::collections::BTreeMap;
use std::fs;
use std::sync::Arc;

use keith_agent_types::ProfileId;
use keith_composio::{
    ComposioConfig, ComposioConnector, ComposioError, ComposioLimits, ProfileAppPolicy,
};
use keith_credentials::{CredentialOwner, CredentialRef, EncryptedCredentialStore, MasterKey};
use keith_platform_contracts::ActionRisk;
use serde_json::Value;
use tempfile::TempDir;

fn limits() -> ComposioLimits {
    ComposioLimits {
        timeout_ms: 1_000,
        session_ttl_ms: 5_000,
        max_profiles: 2,
        max_accounts_per_profile: 4,
        max_toolkits_per_profile: 3,
        max_tools_per_profile: 8,
        max_schema_bytes: 16_384,
        max_argument_bytes: 4_096,
        max_result_bytes: 16_384,
    }
}

fn credential_store(root: &TempDir) -> Arc<EncryptedCredentialStore> {
    Arc::new(
        EncryptedCredentialStore::open(root.path(), MasterKey::from_bytes([19; 32]))
            .expect("credential store"),
    )
}

fn config(api_base: &str, owner: CredentialOwner) -> ComposioConfig {
    ComposioConfig {
        api_base: api_base.to_owned(),
        api_credential: CredentialRef::new("project-api-key", owner).expect("credential reference"),
        limits: limits(),
    }
}

#[test]
fn control_plane_and_mcp_endpoint_policy_blocks_credential_ssrf() {
    let invalid = [
        "http://169.254.169.254/latest/meta-data/",
        "https://127.0.0.1/",
        "https://composio.dev.evil.example/",
        "https://user:password@backend.composio.dev/",
    ];
    for endpoint in invalid {
        let state = TempDir::new().expect("state root");
        let credentials = TempDir::new().expect("credential root");
        assert!(matches!(
            ComposioConnector::open(
                state.path(),
                config(
                    endpoint,
                    CredentialOwner::Tool("composio-control-plane".to_owned()),
                ),
                credential_store(&credentials),
            ),
            Err(ComposioError::InvalidConfiguration)
        ));
    }

    let state = TempDir::new().expect("state root");
    let credentials = TempDir::new().expect("credential root");
    assert!(
        ComposioConnector::open(
            state.path(),
            config(
                "https://backend.composio.dev/",
                CredentialOwner::Tool("composio-control-plane".to_owned()),
            ),
            credential_store(&credentials),
        )
        .is_ok()
    );

    let state = TempDir::new().expect("state root");
    let credentials = TempDir::new().expect("credential root");
    assert!(matches!(
        ComposioConnector::open(
            state.path(),
            config(
                "https://backend.composio.dev/",
                CredentialOwner::Provider("wrong-owner".to_owned()),
            ),
            credential_store(&credentials),
        ),
        Err(ComposioError::InvalidConfiguration)
    ));
}

#[cfg(unix)]
#[test]
fn durable_state_and_atomic_temporary_files_refuse_symlinks() {
    use std::os::unix::fs::symlink;

    let external = TempDir::new().expect("external root");
    let external_state = external.path().join("outside.json");
    fs::write(&external_state, b"{}\n").expect("external state");
    let state = TempDir::new().expect("state root");
    symlink(&external_state, state.path().join("composio-state.json")).expect("state symlink");
    let credentials = TempDir::new().expect("credential root");
    assert!(matches!(
        ComposioConnector::open(
            state.path(),
            config(
                "https://backend.composio.dev/",
                CredentialOwner::Tool("composio-control-plane".to_owned()),
            ),
            credential_store(&credentials),
        ),
        Err(ComposioError::InvalidConfiguration)
    ));

    let state = TempDir::new().expect("state root");
    let credentials = TempDir::new().expect("credential root");
    let mut connector = ComposioConnector::open(
        state.path(),
        config(
            "https://backend.composio.dev/",
            CredentialOwner::Tool("composio-control-plane".to_owned()),
        ),
        credential_store(&credentials),
    )
    .expect("connector");
    let external_temporary = external.path().join("outside.tmp");
    fs::write(&external_temporary, b"do not overwrite\n").expect("external temporary");
    symlink(
        &external_temporary,
        state.path().join(".composio-state.json.tmp"),
    )
    .expect("temporary symlink");
    let policy = ProfileAppPolicy {
        tools: BTreeMap::from([(
            "github".to_owned(),
            BTreeMap::from([("GITHUB_LIST_REPOS".to_owned(), ActionRisk::ReadOnly)]),
        )]),
        max_context_schema_bytes: 4_096,
    };
    assert!(matches!(
        connector.set_profile_policy(ProfileId::new(), policy),
        Err(ComposioError::InvalidConfiguration)
    ));
    assert_eq!(
        fs::read(&external_temporary).expect("external temporary remains"),
        b"do not overwrite\n"
    );
}

#[test]
fn qualification_manifest_keeps_external_accounts_and_secret_access_truthful() {
    let path = std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../evidence/composio/qualification.json");
    let evidence: Value = serde_json::from_slice(&fs::read(path).expect("qualification evidence"))
        .expect("qualification JSON");
    assert_eq!(
        evidence["qualification_result"],
        "local_conformance_passed_external_accounts_blocked"
    );
    assert_eq!(
        evidence["credential_inventory"]["secret_values_read"],
        false
    );
    let applications = evidence["external_applications"]
        .as_array()
        .expect("external application inventory");
    assert_eq!(applications.len(), 3);
    assert!(applications.iter().all(|application| {
        application["real_account_status"] == "blocked_external_credentials_and_owner_authorization"
    }));
}
