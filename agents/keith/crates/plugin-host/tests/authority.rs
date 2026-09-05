use std::collections::BTreeSet;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::Command;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex, OnceLock};

use ed25519_dalek::{Signer, SigningKey};
use keith_plugin_host::{
    GrantApproval, PluginAuthorityError, PluginAuthorityHost, PluginCallOutcome,
    PluginHostCallError, PluginHostContext, PluginLifecycle, PluginPackage, PluginSafeModeReason,
    TrustedPublisher,
};
use keith_plugin_sdk::{
    HOST_API_VERSION, HostRequest, MANIFEST_FILE, MODULE_FILE, ManifestError, PayloadFormat,
    PluginDigest, PluginGrant, PluginHook, PluginKind, PluginLogLevel, PluginManifest,
    PluginMigrationContract, PluginOperation, PluginPublisher, PluginRisk, PluginSignature,
    PluginToolDescriptor, ResourceGrants,
};
use sha2::{Digest, Sha256};
use tempfile::TempDir;

const PUBLISHER_ID: &str = "authority-publisher";
const KEY_ID: &str = "authority-key";

#[derive(Default)]
struct AuthorityContext {
    deny_events: AtomicBool,
    events: Mutex<Vec<String>>,
    logs: Mutex<Vec<String>>,
}

impl PluginHostContext for AuthorityContext {
    fn credential(&self, name: &str) -> Result<Vec<u8>, PluginHostCallError> {
        if name == "provider-token" {
            Ok(b"authority-secret".to_vec())
        } else {
            Err(PluginHostCallError::Denied)
        }
    }

    fn emit_event(&self, topic: &str, _payload: &[u8]) -> Result<(), PluginHostCallError> {
        if self.deny_events.load(Ordering::SeqCst) {
            return Err(PluginHostCallError::Denied);
        }
        self.events
            .lock()
            .expect("event lock")
            .push(topic.to_owned());
        Ok(())
    }

    fn safe_log(&self, _level: PluginLogLevel, message: &str) -> Result<(), PluginHostCallError> {
        self.logs.lock().expect("log lock").push(message.to_owned());
        Ok(())
    }
}

fn signing_key() -> SigningKey {
    SigningKey::from_bytes(&[7_u8; 32])
}

fn trusted_publisher() -> TrustedPublisher {
    trusted_publisher_for(PUBLISHER_ID, KEY_ID, &signing_key())
}

fn trusted_publisher_for(publisher_id: &str, key_id: &str, key: &SigningKey) -> TrustedPublisher {
    TrustedPublisher::new(publisher_id, key_id, key.verifying_key().to_bytes())
        .expect("trusted publisher")
}

fn descriptor() -> PluginToolDescriptor {
    PluginToolDescriptor {
        name: "echo".to_owned(),
        description: "Echoes a typed message".to_owned(),
        input_schema: r#"{"type":"object","required":["message"],"properties":{"message":{"type":"string"}},"additionalProperties":false}"#.to_owned(),
        output_schema: r#"{"type":"object","required":["echo"],"properties":{"echo":{"type":"string"}},"additionalProperties":false}"#.to_owned(),
        risk: PluginRisk::ReadOnly,
        timeout_ms: 1_000,
        supports_cancellation: true,
        streaming: true,
        concurrency_limit: 1,
        required_grants: BTreeSet::from([PluginGrant::SafeLog]),
    }
}

fn grants(extra_clock: bool) -> ResourceGrants {
    ResourceGrants {
        credential_names: BTreeSet::from(["provider-token".to_owned()]),
        allow_events: true,
        allow_clock: extra_clock,
        ..ResourceGrants::default()
    }
}

fn denied_grants() -> ResourceGrants {
    ResourceGrants::default()
}

fn package(
    parent: &Path,
    id: &str,
    version: &str,
    module: &[u8],
    hooks: BTreeSet<PluginHook>,
    grants: ResourceGrants,
    key: &SigningKey,
) -> PathBuf {
    package_for_publisher(
        parent,
        id,
        version,
        module,
        hooks,
        grants,
        PUBLISHER_ID,
        KEY_ID,
        key,
    )
}

#[allow(clippy::too_many_arguments)]
fn package_for_publisher(
    parent: &Path,
    id: &str,
    version: &str,
    module: &[u8],
    hooks: BTreeSet<PluginHook>,
    grants: ResourceGrants,
    publisher_id: &str,
    key_id: &str,
    key: &SigningKey,
) -> PathBuf {
    let directory = parent.join(format!("{id}-{}", version.replace('.', "_")));
    fs::create_dir_all(&directory).expect("package directory");
    let digest = hex(&Sha256::digest(module));
    let mut manifest = PluginManifest {
        manifest_version: 2,
        id: id.to_owned(),
        name: format!("{id} plugin"),
        version: version.to_owned(),
        host_api_min: HOST_API_VERSION,
        host_api_max: HOST_API_VERSION,
        kind: PluginKind::WasiComponent,
        hooks,
        grants,
        publisher: Some(PluginPublisher {
            id: publisher_id.to_owned(),
            name: "Authority Publisher".to_owned(),
            key_id: key_id.to_owned(),
        }),
        digest: Some(PluginDigest {
            algorithm: "sha256".to_owned(),
            value: digest,
        }),
        signature: Some(PluginSignature {
            algorithm: "ed25519".to_owned(),
            key_id: key_id.to_owned(),
            value: "00".repeat(64),
        }),
        tools: vec![descriptor()],
        commands: Vec::new(),
        migration: Some(PluginMigrationContract::default()),
    };
    let payload = PluginPackage::signature_payload(&manifest).expect("signature payload");
    manifest.signature.as_mut().expect("signature").value = hex(&key.sign(&payload).to_bytes());
    fs::write(
        directory.join(MANIFEST_FILE),
        toml::to_string(&manifest).expect("manifest TOML"),
    )
    .expect("write manifest");
    fs::write(directory.join(MODULE_FILE), module).expect("write module");
    directory
}

fn approval(
    id: &str,
    from_version: Option<&str>,
    to_version: &str,
    grants: &ResourceGrants,
) -> GrantApproval {
    GrantApproval {
        plugin_id: id.to_owned(),
        from_version: from_version.map(str::to_owned),
        to_version: to_version.to_owned(),
        grants: declared_grants(grants),
        human_confirmed: true,
    }
}

fn declared_grants(grants: &ResourceGrants) -> BTreeSet<PluginGrant> {
    let mut declared = BTreeSet::new();
    declared.extend(
        grants
            .network_hosts
            .iter()
            .cloned()
            .map(PluginGrant::HttpHost),
    );
    declared.extend(
        grants
            .credential_names
            .iter()
            .cloned()
            .map(PluginGrant::Credential),
    );
    declared.extend(
        grants
            .readable_storage_namespaces
            .iter()
            .cloned()
            .map(PluginGrant::StorageRead),
    );
    declared.extend(
        grants
            .writable_storage_namespaces
            .iter()
            .cloned()
            .map(PluginGrant::StorageWrite),
    );
    if grants.allow_events {
        declared.insert(PluginGrant::EmitEvent);
    }
    if grants.allow_artifacts {
        declared.insert(PluginGrant::CreateArtifact);
    }
    if grants.allow_clock {
        declared.insert(PluginGrant::Clock);
    }
    if grants.allow_safe_logging {
        declared.insert(PluginGrant::SafeLog);
    }
    declared
}

fn request(sequence: u64) -> HostRequest {
    HostRequest {
        interface_version: HOST_API_VERSION,
        invocation_id: format!("authority-test-{sequence}"),
        operation: PluginOperation::Tool,
        target: Some("echo".to_owned()),
        payload_format: PayloadFormat::Json,
        payload: br#"{"message":"hello"}"#.to_vec(),
        cancellation_id: format!("authority-test-cancel-{sequence}"),
    }
}

fn reference_component() -> Vec<u8> {
    static COMPONENT: OnceLock<Vec<u8>> = OnceLock::new();
    COMPONENT.get_or_init(build_reference_component).clone()
}

fn build_reference_component() -> Vec<u8> {
    let fixture = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../plugin-sdk/reference-component/Cargo.toml");
    let target = std::env::var_os("CARGO_TARGET_DIR")
        .map_or_else(
            || PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("../../target"),
            PathBuf::from,
        )
        .join("reference-component-fixture");
    fs::create_dir_all(&target).expect("reference component target");
    let build = Command::new(env!("CARGO"))
        .args([
            "build",
            "--manifest-path",
            fixture.to_str().expect("UTF-8 component manifest"),
            "--lib",
            "--target",
            "wasm32-unknown-unknown",
            "--release",
            "--locked",
        ])
        .env("CARGO_INCREMENTAL", "0")
        .env("CARGO_TARGET_DIR", &target)
        .output()
        .expect("build reference component");
    assert!(
        build.status.success(),
        "reference component build failed: {}",
        String::from_utf8_lossy(&build.stderr)
    );
    let componentizer = Command::new(env!("CARGO"))
        .args([
            "build",
            "--manifest-path",
            fixture.to_str().expect("UTF-8 component manifest"),
            "--bin",
            "componentize",
            "--release",
            "--locked",
        ])
        .env("CARGO_INCREMENTAL", "0")
        .env("CARGO_TARGET_DIR", &target)
        .output()
        .expect("build component encoder");
    assert!(
        componentizer.status.success(),
        "component encoder build failed: {}",
        String::from_utf8_lossy(&componentizer.stderr)
    );
    let core = target.join("wasm32-unknown-unknown/release/keith_reference_plugin_component.wasm");
    let component = target.join("authority-reference-component.wasm");
    let encoded = Command::new(target.join("release/componentize"))
        .arg(core)
        .arg(&component)
        .output()
        .expect("componentize reference plugin");
    assert!(
        encoded.status.success(),
        "component encoding failed: {}",
        String::from_utf8_lossy(&encoded.stderr)
    );
    fs::read(component).expect("read reference component")
}

#[test]
#[allow(clippy::too_many_lines)]
fn authority_lifecycle_provenance_grants_updates_and_uninstall_are_durable() {
    let root = TempDir::new().expect("authority root");
    let packages = TempDir::new().expect("packages");
    let module = reference_component();
    let key = signing_key();
    let hooks = BTreeSet::from([
        PluginHook::Activate,
        PluginHook::Health,
        PluginHook::Tool,
        PluginHook::Migrate,
        PluginHook::Deactivate,
    ]);
    let first_grants = grants(false);
    let first = package(
        packages.path(),
        "authority",
        "1.0.0",
        &module,
        hooks.clone(),
        first_grants.clone(),
        &key,
    );
    let context = Arc::new(AuthorityContext::default());
    let mut host =
        PluginAuthorityHost::open(root.path(), false, [trusted_publisher()], context.clone())
            .expect("open authority host");
    host.discover(&first).expect("discover signed package");
    assert!(matches!(
        host.install(&first, None),
        Err(PluginAuthorityError::ApprovalRequired)
    ));
    let first_approval = approval("authority", None, "1.0.0", &first_grants);
    let installed = host
        .install(&first, Some(&first_approval))
        .expect("install approved package");
    assert_eq!(installed.lifecycle, PluginLifecycle::Active);
    assert_eq!(installed.publisher.id, PUBLISHER_ID);
    assert_eq!(installed.tools, vec![descriptor()]);
    assert!(
        installed
            .data_access
            .credential_names
            .contains("provider-token")
    );
    assert!(
        installed
            .recent_calls
            .iter()
            .any(|call| call.operation == PluginOperation::Activate)
    );
    host.health("authority").expect("component health");
    assert_eq!(
        host.invoke("authority", &request(1))
            .expect("typed tool call")
            .status,
        keith_plugin_sdk::PluginStatus::Completed
    );
    drop(host);

    let mut host =
        PluginAuthorityHost::open(root.path(), false, [trusted_publisher()], context.clone())
            .expect("restart authority host");
    assert_eq!(
        host.inspect("authority").expect("durable record").lifecycle,
        PluginLifecycle::Active
    );

    let second_grants = grants(true);
    let second = package(
        packages.path(),
        "authority",
        "2.0.0",
        &module,
        hooks,
        second_grants.clone(),
        &key,
    );
    assert!(matches!(
        host.update(&second, None),
        Err(PluginAuthorityError::ApprovalRequired)
    ));
    let widened = GrantApproval {
        plugin_id: "authority".to_owned(),
        from_version: Some("1.0.0".to_owned()),
        to_version: "2.0.0".to_owned(),
        grants: BTreeSet::from([PluginGrant::Clock]),
        human_confirmed: true,
    };
    let updated = host
        .update(&second, Some(&widened))
        .expect("approved update");
    assert_eq!(updated.version, "2.0.0");
    assert_eq!(
        updated
            .update_diff
            .as_ref()
            .expect("update diff")
            .added_grants,
        BTreeSet::from([PluginGrant::Clock])
    );
    assert_eq!(
        host.rollback("authority", "1.0.0", None)
            .expect("rollback to approved narrower grants")
            .version,
        "1.0.0"
    );
    host.disable("authority").expect("disable");
    host.activate("authority").expect("reactivate");
    assert_eq!(
        host.quarantine("authority", "operator quarantine")
            .expect("operator quarantine")
            .lifecycle,
        PluginLifecycle::Quarantined
    );
    host.activate("authority")
        .expect("explicit activation leaves quarantine");
    let removed = host.uninstall("authority").expect("complete uninstall");
    assert_eq!(removed.id, "authority");
    assert!(!root.path().join("packages/authority").exists());
    assert!(matches!(
        host.inspect("authority"),
        Err(PluginAuthorityError::NotFound)
    ));
    assert!(
        context
            .logs
            .lock()
            .expect("log lock")
            .iter()
            .all(|message| !message.contains("authority-secret"))
    );
}

#[test]
#[allow(clippy::too_many_lines)]
fn authority_hostile_packages_are_refused_and_failed_migration_rolls_back() {
    let root = TempDir::new().expect("authority root");
    let packages = TempDir::new().expect("packages");
    let module = reference_component();
    let key = signing_key();
    let lifecycle_hooks =
        BTreeSet::from([PluginHook::Activate, PluginHook::Tool, PluginHook::Migrate]);
    let context = Arc::new(AuthorityContext::default());
    let mut host =
        PluginAuthorityHost::open(root.path(), false, [trusted_publisher()], context.clone())
            .expect("open authority host");

    let invalid_signature = package(
        packages.path(),
        "bad-signature",
        "1.0.0",
        &module,
        lifecycle_hooks.clone(),
        grants(false),
        &key,
    );
    let mut manifest: PluginManifest = toml::from_str(
        &fs::read_to_string(invalid_signature.join(MANIFEST_FILE)).expect("manifest"),
    )
    .expect("decode manifest");
    manifest.signature.as_mut().expect("signature").value = "11".repeat(64);
    fs::write(
        invalid_signature.join(MANIFEST_FILE),
        toml::to_string(&manifest).expect("manifest TOML"),
    )
    .expect("write tampered signature");
    assert!(matches!(
        host.discover(&invalid_signature),
        Err(PluginAuthorityError::InvalidSignature)
    ));

    let digest_mismatch = package(
        packages.path(),
        "bad-digest",
        "1.0.0",
        &module,
        lifecycle_hooks.clone(),
        grants(false),
        &key,
    );
    fs::write(digest_mismatch.join(MODULE_FILE), b"tampered module bytes")
        .expect("tamper module digest");
    assert!(matches!(
        host.discover(&digest_mismatch),
        Err(PluginAuthorityError::DigestMismatch)
    ));

    let traversal = package(
        packages.path(),
        "traversal",
        "..",
        &module,
        lifecycle_hooks.clone(),
        grants(false),
        &key,
    );
    assert!(matches!(
        host.discover(&traversal),
        Err(PluginAuthorityError::Traversal)
    ));

    let mut ambient_network = grants(false);
    ambient_network.network_hosts = vec!["*".to_owned()];
    let network = package(
        packages.path(),
        "ambient-network",
        "1.0.0",
        &module,
        lifecycle_hooks.clone(),
        ambient_network,
        &key,
    );
    assert!(matches!(
        host.discover(&network),
        Err(PluginAuthorityError::Manifest(
            ManifestError::AmbientAuthority
        ))
    ));

    let denied = package(
        packages.path(),
        "denied",
        "1.0.0",
        &module,
        lifecycle_hooks.clone(),
        denied_grants(),
        &key,
    );
    let denied_approval = approval("denied", None, "1.0.0", &denied_grants());
    assert!(matches!(
        host.install(&denied, Some(&denied_approval)),
        Err(PluginAuthorityError::InvocationFailed)
    ));
    assert_eq!(
        host.inspect("denied").expect("denied record").lifecycle,
        PluginLifecycle::Quarantined
    );
    assert!(context.events.lock().expect("event lock").is_empty());
    assert!(context.logs.lock().expect("log lock").is_empty());

    let continuity_grants = grants(false);
    let continuity = package(
        packages.path(),
        "continuity",
        "1.0.0",
        &module,
        lifecycle_hooks.clone(),
        continuity_grants.clone(),
        &key,
    );
    host.install(
        &continuity,
        Some(&approval("continuity", None, "1.0.0", &continuity_grants)),
    )
    .expect("install continuity base");
    let other_key = SigningKey::from_bytes(&[9_u8; 32]);
    let other_publisher = trusted_publisher_for("other-publisher", "other-key", &other_key);
    let continuity_update = package_for_publisher(
        packages.path(),
        "continuity",
        "2.0.0",
        &module,
        lifecycle_hooks.clone(),
        continuity_grants.clone(),
        "other-publisher",
        "other-key",
        &other_key,
    );
    let mut continuity_host = PluginAuthorityHost::open(
        root.path(),
        false,
        [trusted_publisher(), other_publisher],
        context.clone(),
    )
    .expect("reopen with both trusted publishers");
    assert!(matches!(
        continuity_host.update(&continuity_update, None),
        Err(PluginAuthorityError::PublisherContinuity)
    ));
    drop(continuity_host);

    let mut host =
        PluginAuthorityHost::open(root.path(), false, [trusted_publisher()], context.clone())
            .expect("reopen original authority host");

    let first_grants = grants(false);
    let first = package(
        packages.path(),
        "migration",
        "1.0.0",
        &module,
        lifecycle_hooks.clone(),
        first_grants.clone(),
        &key,
    );
    host.install(
        &first,
        Some(&approval("migration", None, "1.0.0", &first_grants)),
    )
    .expect("install migration base");
    context.deny_events.store(true, Ordering::SeqCst);
    let second = package(
        packages.path(),
        "migration",
        "2.0.0",
        &module,
        lifecycle_hooks.clone(),
        first_grants.clone(),
        &key,
    );
    assert!(matches!(
        host.update(&second, None),
        Err(PluginAuthorityError::InvocationFailed)
    ));
    let inspection = host.inspect("migration").expect("migration inspection");
    assert_eq!(inspection.version, "1.0.0");
    assert!(inspection.quarantined_versions.contains_key("2.0.0"));
    assert!(
        inspection
            .update_diff
            .as_ref()
            .expect("failed diff")
            .safe_error
            .is_some()
    );
    assert_eq!(inspection.lifecycle, PluginLifecycle::Disabled);
    assert!(
        host.safe_mode()
            .reasons
            .contains(&PluginSafeModeReason::MigrationFailure {
                plugin_id: "migration".to_owned()
            })
    );
    assert_eq!(
        host.inspect("continuity")
            .expect("all third-party plugins are inspectable")
            .lifecycle,
        PluginLifecycle::Disabled
    );
}

#[test]
fn authority_crash_loop_and_corruption_enter_safe_mode_without_blocking_uninstall() {
    let root = TempDir::new().expect("authority root");
    let packages = TempDir::new().expect("packages");
    let module = reference_component();
    let key = signing_key();
    let package_grants = grants(false);
    let signed = package(
        packages.path(),
        "crashy",
        "1.0.0",
        &module,
        BTreeSet::from([PluginHook::Activate, PluginHook::Tool]),
        package_grants.clone(),
        &key,
    );
    let context = Arc::new(AuthorityContext::default());
    let mut host =
        PluginAuthorityHost::open(root.path(), false, [trusted_publisher()], context.clone())
            .expect("open authority host");
    host.install(
        &signed,
        Some(&approval("crashy", None, "1.0.0", &package_grants)),
    )
    .expect("install crash package");
    let sidecar = package(
        packages.path(),
        "sidecar",
        "1.0.0",
        &module,
        BTreeSet::from([PluginHook::Activate, PluginHook::Tool]),
        package_grants.clone(),
        &key,
    );
    host.install(
        &sidecar,
        Some(&approval("sidecar", None, "1.0.0", &package_grants)),
    )
    .expect("install non-crashing sidecar");
    context.deny_events.store(true, Ordering::SeqCst);
    for sequence in 0..3 {
        assert!(matches!(
            host.invoke("crashy", &request(sequence)),
            Err(PluginAuthorityError::InvocationFailed)
        ));
    }
    let inspection = host.inspect("crashy").expect("crash inspection");
    assert_eq!(inspection.lifecycle, PluginLifecycle::Quarantined);
    assert!(
        inspection
            .recent_calls
            .iter()
            .rev()
            .take(3)
            .all(|call| call.outcome == PluginCallOutcome::Denied)
    );
    assert!(
        host.safe_mode()
            .reasons
            .contains(&PluginSafeModeReason::CrashLoop {
                plugin_id: "crashy".to_owned()
            })
    );
    assert_eq!(
        host.inspect("sidecar")
            .expect("sidecar remains inspectable")
            .lifecycle,
        PluginLifecycle::Disabled
    );
    drop(host);

    fs::write(
        root.path().join("packages/crashy/1.0.0/plugin.wasm"),
        b"corrupt-component",
    )
    .expect("corrupt installed package");
    let mut safe = PluginAuthorityHost::open(root.path(), false, [trusted_publisher()], context)
        .expect("safe startup survives corrupt package");
    assert!(safe.safe_mode().active);
    assert!(
        safe.safe_mode()
            .reasons
            .contains(&PluginSafeModeReason::CorruptPackage {
                plugin_id: "crashy".to_owned()
            })
    );
    assert!(matches!(
        safe.activate("crashy"),
        Err(PluginAuthorityError::SafeMode)
    ));
    safe.uninstall("crashy")
        .expect("uninstall remains available in safe mode");
    assert!(!root.path().join("packages/crashy").exists());
}

#[test]
fn incompatible_api_enters_safe_mode_and_verified_rollback_remains_available() {
    let root = TempDir::new().expect("authority root");
    let packages = TempDir::new().expect("packages");
    let module = reference_component();
    let key = signing_key();
    let package_grants = grants(false);
    let hooks = BTreeSet::from([PluginHook::Activate, PluginHook::Tool, PluginHook::Migrate]);
    let first = package(
        packages.path(),
        "incompatible",
        "1.0.0",
        &module,
        hooks.clone(),
        package_grants.clone(),
        &key,
    );
    let second = package(
        packages.path(),
        "incompatible",
        "2.0.0",
        &module,
        hooks,
        package_grants.clone(),
        &key,
    );
    let context = Arc::new(AuthorityContext::default());
    let mut host =
        PluginAuthorityHost::open(root.path(), false, [trusted_publisher()], context.clone())
            .expect("open authority host");
    host.install(
        &first,
        Some(&approval("incompatible", None, "1.0.0", &package_grants)),
    )
    .expect("install compatible base");
    host.update(&second, None)
        .expect("install compatible update");
    drop(host);

    let installed_manifest = root
        .path()
        .join("packages/incompatible/2.0.0")
        .join(MANIFEST_FILE);
    let mut manifest: PluginManifest =
        toml::from_str(&fs::read_to_string(&installed_manifest).expect("installed manifest"))
            .expect("decode installed manifest");
    manifest.host_api_min = HOST_API_VERSION.saturating_add(1);
    manifest.host_api_max = HOST_API_VERSION.saturating_add(1);
    manifest.signature.as_mut().expect("signature").value = "00".repeat(64);
    let payload = PluginPackage::signature_payload(&manifest).expect("signature payload");
    manifest.signature.as_mut().expect("signature").value = hex(&key.sign(&payload).to_bytes());
    fs::write(
        &installed_manifest,
        toml::to_string(&manifest).expect("manifest TOML"),
    )
    .expect("write incompatible signed manifest");

    let mut safe = PluginAuthorityHost::open(root.path(), false, [trusted_publisher()], context)
        .expect("safe startup survives incompatible API");
    assert!(safe.safe_mode().active);
    assert!(
        safe.safe_mode()
            .reasons
            .contains(&PluginSafeModeReason::IncompatiblePackage {
                plugin_id: "incompatible".to_owned()
            })
    );
    let rolled_back = safe
        .rollback("incompatible", "1.0.0", None)
        .expect("verified rollback remains available in safe mode");
    assert_eq!(rolled_back.version, "1.0.0");
    assert_eq!(rolled_back.lifecycle, PluginLifecycle::Disabled);
    assert!(
        !safe
            .exit_safe_mode()
            .expect("validated safe-mode exit")
            .active
    );
    safe.activate("incompatible")
        .expect("explicit activation after safe-mode exit");
    safe.uninstall("incompatible")
        .expect("uninstall remains available after incompatible API");
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;

    let mut encoded = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        write!(encoded, "{byte:02x}").expect("hex encoding");
    }
    encoded
}
