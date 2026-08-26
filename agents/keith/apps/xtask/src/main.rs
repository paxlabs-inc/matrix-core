use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::env;
use std::ffi::OsStr;
use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Command, ExitCode};

use keith_agent_types::EntityId;
use keith_build_info::{BUILD_ID, daemon_report, worker_report};
use keith_release::{
    MANIFEST_FILE, PUBLIC_KEY_FILE, ReleaseFile, ReleaseManifest, SIGNATURE_FILE,
    decode_public_key, hex_encode, verify_packaged_build_reports,
    verify_release as verify_signed_release,
};
use ring::signature::{Ed25519KeyPair, KeyPair};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};

mod platform;
mod security;

fn main() -> ExitCode {
    let result = match env::args().nth(1).as_deref() {
        Some("ci") => ci(),
        Some("clean-checkout") => clean_checkout(),
        Some("dependency-policy") => dependency_policy(&workspace_root()),
        Some("schema-doc") => schema_document(
            &workspace_root(),
            matches!(env::args().nth(2).as_deref(), Some("--write")),
        ),
        Some("protocol-doc") => protocol_document(
            &workspace_root(),
            matches!(env::args().nth(2).as_deref(), Some("--write")),
        ),
        Some("provider-metadata") => provider_metadata_document(
            &workspace_root(),
            matches!(env::args().nth(2).as_deref(), Some("--write")),
        ),
        Some("security-gate") => security::run(&workspace_root()),
        Some("teammates-security-gate") => security::run_teammates(&workspace_root()),
        Some("teammates-release-qualify") => teammates_release_qualify(&workspace_root()),
        Some("platform-gate") => platform::run(&workspace_root()),
        Some("release") => release(&workspace_root()),
        Some("verify-release") => verify_release_command(),
        Some("compatibility") => compatibility_command(),
        Some("install") => install_command(false),
        Some("upgrade") => install_command(true),
        Some("backup") => backup_command(),
        Some("restore") => restore_command(),
        Some("rollback") => rollback_command(),
        Some("resource-report") => resource_report_command(),
        Some("uninstall") => uninstall_command(),
        _ => Err(
            "usage: cargo xtask <ci|clean-checkout|dependency-policy|schema-doc [--write]|protocol-doc [--write]|provider-metadata [--write]|security-gate|teammates-security-gate|teammates-release-qualify RELEASE PUBLIC_KEY INSTALL_ROOT DATA_ROOT BACKUP_ROOT RESTORE_ROOT PROVIDER_BINARY MODEL_CONFIG RESOURCE_LIMITS EVIDENCE_ROOT|platform-gate|release [OUTPUT]|verify-release RELEASE PUBLIC_KEY|compatibility RELEASE PUBLIC_KEY DATA_ROOT|install RELEASE PUBLIC_KEY INSTALL_ROOT DATA_ROOT|upgrade RELEASE PUBLIC_KEY INSTALL_ROOT DATA_ROOT BACKUP_ROOT|backup DATA_ROOT BACKUP_ROOT|restore BACKUP_ROOT DATA_ROOT|rollback BACKUP_ROOT INSTALL_ROOT DATA_ROOT|resource-report INSTALL_ROOT DATA_ROOT|uninstall INSTALL_ROOT [--erase-data DATA_ROOT]>".into(),
        ),
    };

    match result {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("{error}");
            ExitCode::FAILURE
        }
    }
}

#[derive(Serialize)]
#[serde(rename_all = "snake_case")]
enum LifecycleOutcome {
    Compatible,
    Installed,
    Upgraded,
    BackedUp,
    Restored,
    RolledBack,
    Reported,
    Uninstalled,
}

#[derive(Serialize)]
#[serde(deny_unknown_fields)]
struct LifecycleReport {
    schema_version: u16,
    outcome: LifecycleOutcome,
    install_root: Option<String>,
    data_root: Option<String>,
    backup_root: Option<String>,
    release_build_id: Option<String>,
    storage_schema: String,
    protocol_version: String,
    details: BTreeMap<String, Value>,
}

fn print_report(report: &LifecycleReport) -> Result<(), String> {
    println!(
        "{}",
        serde_json::to_string(report).map_err(|error| error.to_string())?
    );
    Ok(())
}

fn required_path(index: usize, name: &str) -> Result<PathBuf, String> {
    env::args_os()
        .nth(index)
        .map(PathBuf::from)
        .ok_or_else(|| format!("missing required {name}"))
}

fn trusted_release(
    index: usize,
    key_index: usize,
) -> Result<keith_release::VerifiedRelease, String> {
    let release = required_path(index, "release directory")?;
    let encoded = env::args()
        .nth(key_index)
        .ok_or_else(|| "missing trusted public key".to_owned())?;
    let key = decode_public_key(&encoded).map_err(|error| error.to_string())?;
    let verified = verify_signed_release(&release, &key).map_err(|error| error.to_string())?;
    verify_packaged_build_reports(&release, &verified.manifest)
        .map_err(|error| error.to_string())?;
    Ok(verified)
}

fn compatibility_command() -> Result<(), String> {
    let verified = trusted_release(2, 3)?;
    let data_root = required_path(4, "data root")?;
    let host_target = format!("{}-{}", env::consts::ARCH, env::consts::OS);
    if verified.manifest.target != host_target {
        return Err("release target is incompatible with this host".to_owned());
    }
    let current_schema = keith_agent_types::CURRENT_SCHEMA_VERSION.to_string();
    let existing_schema = installed_schema(&data_root)?.unwrap_or_else(|| current_schema.clone());
    if schema_number(&existing_schema)? > schema_number(&verified.manifest.storage_schema)? {
        return Err("release cannot open a newer teammate storage schema".to_owned());
    }
    print_report(&LifecycleReport {
        schema_version: 1,
        outcome: LifecycleOutcome::Compatible,
        install_root: None,
        data_root: Some(safe_path(&data_root)?),
        backup_root: None,
        release_build_id: Some(verified.manifest.build_id),
        storage_schema: verified.manifest.storage_schema,
        protocol_version: verified.manifest.protocol_version,
        details: BTreeMap::from([
            ("existing_storage_schema".into(), json!(existing_schema)),
            ("offline_installation".into(), json!(true)),
        ]),
    })
}

fn install_command(upgrade: bool) -> Result<(), String> {
    let release_root = required_path(2, "release directory")?;
    let verified = trusted_release(2, 3)?;
    let install_root = required_path(4, "install root")?;
    let data_root = required_path(5, "data root")?;
    reject_dangerous_root(&install_root)?;
    reject_dangerous_root(&data_root)?;
    let backup_root = if upgrade {
        Some(required_path(6, "backup root")?)
    } else {
        None
    };
    let existing_schema = installed_schema(&data_root)?;
    if let Some(schema) = &existing_schema
        && schema_number(schema)? > schema_number(&verified.manifest.storage_schema)?
    {
        return Err("upgrade would downgrade teammate storage".to_owned());
    }
    if upgrade {
        let backup = backup_root.as_ref().expect("upgrade backup is present");
        portable_backup(&data_root, backup)?;
        if install_root.exists() {
            copy_tree_strict(&install_root, &backup.join("installation"))?;
        }
    } else if install_root.exists() {
        return Err("install root already exists; use upgrade".to_owned());
    }
    let parent = install_root.parent().unwrap_or_else(|| Path::new("."));
    fs::create_dir_all(parent).map_err(|error| error.to_string())?;
    let staging = parent.join(format!(".keith-install-{}.tmp", EntityId::new()));
    copy_tree_strict(&release_root, &staging)?;
    write_install_metadata(&staging, &data_root, &verified.manifest)?;
    if install_root.exists() {
        let retired = parent.join(format!(".keith-retired-{}", EntityId::new()));
        fs::rename(&install_root, &retired).map_err(|error| error.to_string())?;
        if let Err(error) = fs::rename(&staging, &install_root) {
            fs::rename(&retired, &install_root).map_err(|rollback| {
                format!("upgrade promotion failed: {error}; rollback failed: {rollback}")
            })?;
            return Err(format!("upgrade promotion failed: {error}"));
        }
        fs::remove_dir_all(retired).map_err(|error| error.to_string())?;
    } else {
        fs::rename(&staging, &install_root).map_err(|error| error.to_string())?;
    }
    fs::create_dir_all(&data_root).map_err(|error| error.to_string())?;
    fs::write(
        data_root.join("storage-schema"),
        &verified.manifest.storage_schema,
    )
    .map_err(|error| error.to_string())?;
    print_report(&LifecycleReport {
        schema_version: 1,
        outcome: if upgrade {
            LifecycleOutcome::Upgraded
        } else {
            LifecycleOutcome::Installed
        },
        install_root: Some(safe_path(&install_root)?),
        data_root: Some(safe_path(&data_root)?),
        backup_root: backup_root
            .as_ref()
            .map(|path| safe_path(path))
            .transpose()?,
        release_build_id: Some(verified.manifest.build_id),
        storage_schema: verified.manifest.storage_schema,
        protocol_version: verified.manifest.protocol_version,
        details: BTreeMap::from([
            ("agentd_supervision".into(), json!(true)),
            ("browser_runner".into(), json!(true)),
            ("xvfb_chromium_supervision".into(), json!(true)),
            (
                "migration_required".into(),
                json!(existing_schema.is_some()),
            ),
        ]),
    })
}

fn backup_command() -> Result<(), String> {
    let data_root = required_path(2, "data root")?;
    let backup_root = required_path(3, "backup root")?;
    portable_backup(&data_root, &backup_root)?;
    print_simple_lifecycle(
        LifecycleOutcome::BackedUp,
        None,
        Some(&data_root),
        Some(&backup_root),
    )
}

fn restore_command() -> Result<(), String> {
    let backup_root = required_path(2, "backup root")?;
    let data_root = required_path(3, "data root")?;
    reject_dangerous_root(&data_root)?;
    if data_root.exists()
        && fs::read_dir(&data_root)
            .map_err(|error| error.to_string())?
            .next()
            .is_some()
    {
        return Err("restore data root must be absent or empty".to_owned());
    }
    copy_tree_strict(&backup_root.join("data"), &data_root)?;
    print_simple_lifecycle(
        LifecycleOutcome::Restored,
        None,
        Some(&data_root),
        Some(&backup_root),
    )
}

fn rollback_command() -> Result<(), String> {
    let backup_root = required_path(2, "backup root")?;
    let install_root = required_path(3, "install root")?;
    let data_root = required_path(4, "data root")?;
    reject_dangerous_root(&install_root)?;
    reject_dangerous_root(&data_root)?;
    if install_root.exists() || data_root.exists() {
        return Err("rollback targets must be removed explicitly before restoration".to_owned());
    }
    copy_tree_strict(&backup_root.join("installation"), &install_root)?;
    copy_tree_strict(&backup_root.join("data"), &data_root)?;
    print_simple_lifecycle(
        LifecycleOutcome::RolledBack,
        Some(&install_root),
        Some(&data_root),
        Some(&backup_root),
    )
}

fn resource_report_command() -> Result<(), String> {
    let install_root = required_path(2, "install root")?;
    let data_root = required_path(3, "data root")?;
    let mut details = BTreeMap::new();
    details.insert(
        "installation_bytes".into(),
        json!(tree_bytes(&install_root)?),
    );
    details.insert("data_bytes".into(), json!(tree_bytes(&data_root)?));
    details.insert(
        "agentd_present".into(),
        json!(install_root.join("bin/agentd").is_file()),
    );
    details.insert(
        "browser_runner_present".into(),
        json!(install_root.join("bin/browser-runner").is_file()),
    );
    details.insert(
        "chromium_available".into(),
        json!(find_on_path("chromium") || find_on_path("chromium-browser")),
    );
    details.insert("xvfb_available".into(), json!(find_on_path("Xvfb")));
    print_report(&LifecycleReport {
        schema_version: 1,
        outcome: LifecycleOutcome::Reported,
        install_root: Some(safe_path(&install_root)?),
        data_root: Some(safe_path(&data_root)?),
        backup_root: None,
        release_build_id: None,
        storage_schema: installed_schema(&data_root)?.unwrap_or_else(|| "unknown".into()),
        protocol_version: keith_agent_types::CURRENT_PROTOCOL_VERSION.to_string(),
        details,
    })
}

fn uninstall_command() -> Result<(), String> {
    let install_root = required_path(2, "install root")?;
    reject_dangerous_root(&install_root)?;
    if install_root.exists() {
        fs::remove_dir_all(&install_root).map_err(|error| error.to_string())?;
    }
    let data_root = match (env::args().nth(3).as_deref(), env::args_os().nth(4)) {
        (Some("--erase-data"), Some(path)) => {
            let path = PathBuf::from(path);
            reject_dangerous_root(&path)?;
            if path.exists() {
                fs::remove_dir_all(&path).map_err(|error| error.to_string())?;
            }
            Some(path)
        }
        (None, None) => None,
        _ => return Err("uninstall accepts only --erase-data DATA_ROOT".to_owned()),
    };
    print_simple_lifecycle(
        LifecycleOutcome::Uninstalled,
        Some(&install_root),
        data_root.as_deref(),
        None,
    )
}

fn print_simple_lifecycle(
    outcome: LifecycleOutcome,
    install_root: Option<&Path>,
    data_root: Option<&Path>,
    backup_root: Option<&Path>,
) -> Result<(), String> {
    print_report(&LifecycleReport {
        schema_version: 1,
        outcome,
        install_root: install_root.map(safe_path).transpose()?,
        data_root: data_root.map(safe_path).transpose()?,
        backup_root: backup_root.map(safe_path).transpose()?,
        release_build_id: None,
        storage_schema: data_root
            .map(installed_schema)
            .transpose()?
            .flatten()
            .unwrap_or_else(|| "unknown".into()),
        protocol_version: keith_agent_types::CURRENT_PROTOCOL_VERSION.to_string(),
        details: BTreeMap::new(),
    })
}

fn schema_number(value: &str) -> Result<u64, String> {
    value
        .split('.')
        .next()
        .unwrap_or_default()
        .parse()
        .map_err(|_| "storage schema is malformed".to_owned())
}

fn installed_schema(data_root: &Path) -> Result<Option<String>, String> {
    let path = data_root.join("storage-schema");
    match fs::read_to_string(path) {
        Ok(value) if value.len() <= 64 => Ok(Some(value.trim().to_owned())),
        Ok(_) => Err("installed storage schema marker is oversized".to_owned()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(error.to_string()),
    }
}

fn safe_path(path: &Path) -> Result<String, String> {
    path.to_str()
        .map(str::to_owned)
        .ok_or_else(|| "lifecycle paths must be UTF-8".to_owned())
}

fn reject_dangerous_root(path: &Path) -> Result<(), String> {
    if !path.is_absolute()
        || path == Path::new("/")
        || path.components().any(|component| {
            matches!(
                component,
                std::path::Component::ParentDir | std::path::Component::CurDir
            )
        })
        || path.components().count() < 3
    {
        return Err(
            "lifecycle target must be a specific absolute path below a parent directory".to_owned(),
        );
    }
    Ok(())
}

fn portable_backup(data_root: &Path, backup_root: &Path) -> Result<(), String> {
    reject_dangerous_root(data_root)?;
    reject_dangerous_root(backup_root)?;
    if !data_root.is_dir() {
        return Err("backup data root does not exist".to_owned());
    }
    if backup_root.exists() {
        return Err("backup root already exists".to_owned());
    }
    let parent = backup_root.parent().unwrap_or_else(|| Path::new("."));
    fs::create_dir_all(parent).map_err(|error| error.to_string())?;
    let staging = parent.join(format!(".keith-backup-{}.tmp", EntityId::new()));
    copy_tree_strict(data_root, &staging.join("data"))?;
    let files = release_files(&staging.join("data"))?;
    fs::write(
        staging.join("backup.json"),
        serde_json::to_vec_pretty(&json!({
            "format": "keith-portable-backup-v1",
            "storage_schema": installed_schema(data_root)?,
            "protocol_version": keith_agent_types::CURRENT_PROTOCOL_VERSION.to_string(),
            "files": files,
        }))
        .map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;
    fs::rename(staging, backup_root).map_err(|error| error.to_string())?;
    Ok(())
}

fn copy_tree_strict(source: &Path, destination: &Path) -> Result<(), String> {
    let metadata = fs::symlink_metadata(source).map_err(|error| error.to_string())?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err("portable copy source must be a real directory".to_owned());
    }
    fs::create_dir_all(destination).map_err(|error| error.to_string())?;
    for entry in fs::read_dir(source).map_err(|error| error.to_string())? {
        let entry = entry.map_err(|error| error.to_string())?;
        let file_type = entry.file_type().map_err(|error| error.to_string())?;
        let target = destination.join(entry.file_name());
        if file_type.is_dir() {
            copy_tree_strict(&entry.path(), &target)?;
        } else if file_type.is_file() {
            fs::copy(entry.path(), target).map_err(|error| error.to_string())?;
        } else {
            return Err("portable copy refuses links and special files".to_owned());
        }
    }
    Ok(())
}

fn write_install_metadata(
    install_root: &Path,
    data_root: &Path,
    manifest: &ReleaseManifest,
) -> Result<(), String> {
    let services = install_root.join("services");
    let migrations = install_root.join("migrations");
    fs::create_dir_all(&services).map_err(|error| error.to_string())?;
    fs::create_dir_all(&migrations).map_err(|error| error.to_string())?;
    let executable = install_root.join("bin/agentd");
    let browser_runner = install_root.join("bin/browser-runner");
    fs::write(
        services.join("keith-agentd.service"),
        format!(
            "[Unit]\nDescription=Keith Agent Daemon\nAfter=network.target\n[Service]\nType=simple\nExecStart={} --data-root {}\nRestart=on-failure\nNoNewPrivileges=true\n[Install]\nWantedBy=default.target\n",
            executable.display(),
            data_root.display()
        ),
    )
    .map_err(|error| error.to_string())?;
    fs::write(
        services.join("keith-browser-runner.service.template"),
        format!(
            "[Unit]\nDescription=Keith headed browser %i\n[Service]\nType=simple\nExecStart={} --profile %i\nRestart=on-failure\nNoNewPrivileges=true\n",
            browser_runner.display()
        ),
    )
    .map_err(|error| error.to_string())?;
    fs::write(
        migrations.join("teammate-schema.json"),
        serde_json::to_vec_pretty(&json!({
            "format": "keith-teammate-migration-v1",
            "target_storage_schema": manifest.storage_schema,
            "components": ["profiles", "conversations", "coordination", "sessions", "artifacts", "channel_bindings", "computers"],
            "rollback_requires_backup": true,
        }))
        .map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;
    Ok(())
}

fn tree_bytes(root: &Path) -> Result<u64, String> {
    if !root.exists() {
        return Ok(0);
    }
    let mut total = 0_u64;
    for entry in fs::read_dir(root).map_err(|error| error.to_string())? {
        let entry = entry.map_err(|error| error.to_string())?;
        let kind = entry.file_type().map_err(|error| error.to_string())?;
        if kind.is_dir() {
            total = total
                .checked_add(tree_bytes(&entry.path())?)
                .ok_or_else(|| "resource byte count overflow".to_owned())?;
        } else if kind.is_file() {
            total = total
                .checked_add(entry.metadata().map_err(|error| error.to_string())?.len())
                .ok_or_else(|| "resource byte count overflow".to_owned())?;
        } else {
            return Err("resource report refuses links and special files".to_owned());
        }
    }
    Ok(total)
}

fn find_on_path(binary: &str) -> bool {
    env::var_os("PATH")
        .is_some_and(|paths| env::split_paths(&paths).any(|path| path.join(binary).is_file()))
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct QualificationModelConfig {
    provider_boundary: String,
    model: String,
    endpoint: Option<String>,
    credential_environment: Option<String>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(deny_unknown_fields)]
struct QualificationCommandEvidence {
    program: String,
    arguments: Vec<String>,
    environment_keys: Vec<String>,
}

#[derive(Clone, Debug, Serialize)]
#[serde(deny_unknown_fields)]
struct TeammatesReleaseQualificationPlan {
    schema_version: u16,
    release_build_id: String,
    release_manifest_sha256: String,
    host_target: String,
    host_name: Option<String>,
    provider_binary: String,
    provider_binary_sha256: String,
    provider_boundary: String,
    model: String,
    model_configuration_sha256: String,
    resource_limits: Value,
    resource_limits_sha256: String,
    install_root: String,
    data_root: String,
    backup_root: String,
    restore_root: String,
    evidence_root: String,
    exact_commands: Vec<QualificationCommandEvidence>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct QualificationProfileEvidence {
    profile_id: String,
    permanent_human_dm_id: String,
    enabled: bool,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct QualificationBrowserEvidence {
    headed_chromium: bool,
    xvfb_display: bool,
    authenticated_stream: bool,
    takeover_authorized: bool,
    unauthorized_input_denied: bool,
    handback_completed: bool,
    audit_correlations: Vec<String>,
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct TeammatesDriverEvidence {
    schema_version: u16,
    release_build_id: String,
    host_target: String,
    provider_binary_sha256: String,
    model_configuration_sha256: String,
    resource_limits_sha256: String,
    used_mock_or_fake: bool,
    profiles: Vec<QualificationProfileEvidence>,
    agent_agent_dm_id: String,
    group_conversation_id: String,
    assignment_id: String,
    handoff_id: String,
    review_event_id: String,
    completion_event_id: String,
    daemon_killed_after_final_before_publication: bool,
    daemon_restarted: bool,
    final_publication_count: u64,
    reload_projection_matches: bool,
    due_routine_dm_event_id: String,
    browser: QualificationBrowserEvidence,
    resource_limits_enforced: bool,
    cross_profile_access_denied: bool,
    all_processes_stopped: bool,
    secret_scan_matches: usize,
}

#[derive(Serialize)]
#[serde(deny_unknown_fields)]
struct TeammatesReleaseQualificationReport {
    schema_version: u16,
    completed: bool,
    release_build_id: String,
    host_target: String,
    provider_binary_sha256: String,
    model_configuration_sha256: String,
    resource_limits_sha256: String,
    original_data_inventory_sha256: String,
    restored_data_inventory_sha256: String,
    adversarial_gate_evidence_sha256: String,
    codify_trace_evidence_sha256: String,
    driver_evidence: TeammatesDriverEvidence,
    exact_commands: Vec<QualificationCommandEvidence>,
}

#[allow(clippy::too_many_lines)]
fn teammates_release_qualify(root: &Path) -> Result<(), String> {
    let release_root = required_path(2, "signed release directory")?;
    let encoded_key = env::args()
        .nth(3)
        .ok_or_else(|| "missing trusted public key".to_owned())?;
    let install_root = required_path(4, "fresh install root")?;
    let data_root = required_path(5, "fresh data root")?;
    let backup_root = required_path(6, "fresh backup root")?;
    let restore_root = required_path(7, "fresh restore root")?;
    let provider_binary = required_path(8, "real provider binary")?;
    let model_configuration = required_path(9, "model configuration")?;
    let resource_limits_path = required_path(10, "resource limits")?;
    let evidence_root = required_path(11, "fresh evidence root")?;

    let key = decode_public_key(&encoded_key).map_err(|error| error.to_string())?;
    let verified = verify_signed_release(&release_root, &key).map_err(|error| error.to_string())?;
    verify_packaged_build_reports(&release_root, &verified.manifest)
        .map_err(|error| error.to_string())?;
    let host_target = format!("{}-{}", env::consts::ARCH, env::consts::OS);
    if verified.manifest.target != host_target {
        return Err("signed teammate release does not target this qualification host".into());
    }
    if verified.manifest.build_id.trim().is_empty()
        || verified.manifest.build_id.ends_with("+development")
    {
        return Err("qualification requires a non-development signed build".into());
    }

    let fresh_roots = [
        &install_root,
        &data_root,
        &backup_root,
        &restore_root,
        &evidence_root,
    ];
    validate_fresh_qualification_roots(&fresh_roots)?;
    reject_overlapping_qualification_paths(
        &fresh_roots,
        &[
            &release_root,
            &provider_binary,
            &model_configuration,
            &resource_limits_path,
        ],
    )?;
    let provider_binary = validate_real_provider_binary(&provider_binary)?;
    let model_bytes = fs::read(&model_configuration)
        .map_err(|error| format!("model configuration is unavailable: {error}"))?;
    let model: QualificationModelConfig = serde_json::from_slice(&model_bytes)
        .map_err(|error| format!("model configuration is invalid: {error}"))?;
    validate_model_configuration(&model)?;
    let resource_limit_bytes = fs::read(&resource_limits_path)
        .map_err(|error| format!("resource limit document is unavailable: {error}"))?;
    let resource_limits: Value = serde_json::from_slice(&resource_limit_bytes)
        .map_err(|error| format!("resource limit document is invalid: {error}"))?;
    if !matches!(resource_limits.as_object(), Some(limits) if !limits.is_empty()) {
        return Err("resource limit document must be a non-empty object".into());
    }

    fs::create_dir(&evidence_root)
        .map_err(|error| format!("cannot create fresh evidence root: {error}"))?;
    let current_executable = env::current_exe()
        .map_err(|error| format!("cannot resolve qualification orchestrator: {error}"))?;
    let packaged_driver = release_root
        .join("bin")
        .join(format!("agentd{}", env::consts::EXE_SUFFIX));
    if !packaged_driver.is_file() {
        return Err("signed release is missing its agentd qualification driver".into());
    }
    let driver = install_root
        .join("bin")
        .join(format!("agentd{}", env::consts::EXE_SUFFIX));
    let driver_evidence_path = evidence_root.join("driver-evidence.json");
    let adversarial_evidence_root = evidence_root.join("adversarial");
    let commands = qualification_commands(
        &current_executable,
        &driver,
        &release_root,
        &encoded_key,
        &install_root,
        &data_root,
        &backup_root,
        &restore_root,
        &provider_binary,
        &model_configuration,
        &resource_limits_path,
        &driver_evidence_path,
        &adversarial_evidence_root,
    );
    let provider_sha256 = file_sha256(&provider_binary)?;
    let model_sha256 = hex_encode(&Sha256::digest(&model_bytes));
    let resource_limits_sha256 = hex_encode(&Sha256::digest(&resource_limit_bytes));
    let plan = TeammatesReleaseQualificationPlan {
        schema_version: 1,
        release_build_id: verified.manifest.build_id.clone(),
        release_manifest_sha256: verified.manifest_sha256.clone(),
        host_target: host_target.clone(),
        host_name: env::var("HOSTNAME")
            .ok()
            .filter(|value| !value.trim().is_empty()),
        provider_binary: safe_path(&provider_binary)?,
        provider_binary_sha256: provider_sha256.clone(),
        provider_boundary: model.provider_boundary.clone(),
        model: model.model.clone(),
        model_configuration_sha256: model_sha256.clone(),
        resource_limits,
        resource_limits_sha256: resource_limits_sha256.clone(),
        install_root: safe_path(&install_root)?,
        data_root: safe_path(&data_root)?,
        backup_root: safe_path(&backup_root)?,
        restore_root: safe_path(&restore_root)?,
        evidence_root: safe_path(&evidence_root)?,
        exact_commands: commands.clone(),
    };
    fs::write(
        evidence_root.join("qualification-plan.json"),
        serde_json::to_vec_pretty(&plan).map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;

    run_qualification_process(root, &commands[0], &[], &evidence_root, 0)?;
    run_qualification_process(root, &commands[1], &[], &evidence_root, 1)?;
    let driver_evidence = read_and_validate_driver_evidence(
        &driver_evidence_path,
        &verified.manifest.build_id,
        &host_target,
        &provider_sha256,
        &model_sha256,
        &resource_limits_sha256,
    )?;
    run_qualification_process(root, &commands[2], &[], &evidence_root, 2)?;
    run_qualification_process(root, &commands[3], &[], &evidence_root, 3)?;
    let original_inventory = data_inventory_digest(&data_root)?;
    let restored_inventory = data_inventory_digest(&restore_root)?;
    if original_inventory != restored_inventory {
        return Err("portable backup and clean restore inventories differ".into());
    }
    run_qualification_process(root, &commands[4], &[], &evidence_root, 4)?;
    run_qualification_process(root, &commands[5], &[], &evidence_root, 5)?;
    let security_environment = [
        ("KEITH_SECURITY_RELEASE_PATH", safe_path(&release_root)?),
        ("KEITH_SECURITY_TRUSTED_PUBLIC_KEY", encoded_key),
        (
            "KEITH_TEAMMATES_EVIDENCE_ROOT",
            safe_path(&adversarial_evidence_root)?,
        ),
        (
            "KEITH_TEAMMATES_PROVIDER_COMMAND",
            safe_path(&provider_binary)?,
        ),
        (
            "KEITH_TEAMMATES_PROVIDER_BOUNDARY",
            model.provider_boundary.clone(),
        ),
    ];
    run_qualification_process(root, &commands[6], &security_environment, &evidence_root, 6)?;
    run_qualification_process(root, &commands[7], &[], &evidence_root, 7)?;

    let adversarial_digest =
        file_sha256(&adversarial_evidence_root.join("teammates-security-gate.json"))?;
    let codify_trace_path = evidence_root.join("07-spec.stdout.log");
    validate_codify_trace(&codify_trace_path)?;
    let codify_trace_digest = file_sha256(&codify_trace_path)?;
    let report = TeammatesReleaseQualificationReport {
        schema_version: 1,
        completed: true,
        release_build_id: verified.manifest.build_id,
        host_target,
        provider_binary_sha256: provider_sha256,
        model_configuration_sha256: model_sha256,
        resource_limits_sha256,
        original_data_inventory_sha256: original_inventory,
        restored_data_inventory_sha256: restored_inventory,
        adversarial_gate_evidence_sha256: adversarial_digest,
        codify_trace_evidence_sha256: codify_trace_digest,
        driver_evidence,
        exact_commands: commands,
    };
    fs::write(
        evidence_root.join("teammates-release-qualification.json"),
        serde_json::to_vec_pretty(&report).map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;
    println!(
        "teammates release qualification passed; evidence={}",
        evidence_root.display()
    );
    Ok(())
}

#[allow(clippy::too_many_arguments)]
fn qualification_commands(
    orchestrator: &Path,
    driver: &Path,
    release_root: &Path,
    encoded_key: &str,
    install_root: &Path,
    data_root: &Path,
    backup_root: &Path,
    restore_root: &Path,
    provider_binary: &Path,
    model_configuration: &Path,
    resource_limits: &Path,
    driver_evidence: &Path,
    adversarial_evidence_root: &Path,
) -> Vec<QualificationCommandEvidence> {
    let command = |program: &Path, arguments: Vec<String>, environment_keys: &[&str]| {
        QualificationCommandEvidence {
            program: program.display().to_string(),
            arguments,
            environment_keys: environment_keys
                .iter()
                .map(|value| (*value).to_owned())
                .collect(),
        }
    };
    vec![
        command(
            orchestrator,
            vec![
                "install".into(),
                release_root.display().to_string(),
                encoded_key.to_owned(),
                install_root.display().to_string(),
                data_root.display().to_string(),
            ],
            &[],
        ),
        command(
            driver,
            vec![
                "teammates-release-qualification".into(),
                "--data-root".into(),
                data_root.display().to_string(),
                "--provider-binary".into(),
                provider_binary.display().to_string(),
                "--model-configuration".into(),
                model_configuration.display().to_string(),
                "--resource-limits".into(),
                resource_limits.display().to_string(),
                "--evidence".into(),
                driver_evidence.display().to_string(),
                "--profile-count".into(),
                "4".into(),
                "--scenario".into(),
                "permanent-dm-a2a-group-assignment-handoff-review-completion".into(),
                "--kill-point".into(),
                "after-final-before-publication".into(),
                "--require-exactly-once-reload".into(),
                "--require-due-routine-dm".into(),
                "--require-headed-browser-stream-takeover-handback-audit".into(),
            ],
            &[],
        ),
        command(
            orchestrator,
            vec![
                "backup".into(),
                data_root.display().to_string(),
                backup_root.display().to_string(),
            ],
            &[],
        ),
        command(
            orchestrator,
            vec![
                "restore".into(),
                backup_root.display().to_string(),
                restore_root.display().to_string(),
            ],
            &[],
        ),
        command(
            orchestrator,
            vec![
                "resource-report".into(),
                install_root.display().to_string(),
                data_root.display().to_string(),
            ],
            &[],
        ),
        command(orchestrator, vec!["platform-gate".into()], &[]),
        command(
            orchestrator,
            vec!["security-gate".into()],
            &[
                "KEITH_SECURITY_RELEASE_PATH",
                "KEITH_SECURITY_TRUSTED_PUBLIC_KEY",
                "KEITH_TEAMMATES_EVIDENCE_ROOT",
                "KEITH_TEAMMATES_PROVIDER_COMMAND",
                "KEITH_TEAMMATES_PROVIDER_BOUNDARY",
            ],
        ),
        QualificationCommandEvidence {
            program: "cg".into(),
            arguments: vec!["spec".into(), "trace".into(), "9.1".into(), "--json".into()],
            environment_keys: Vec::new(),
        },
    ]
}

fn validate_fresh_qualification_roots(roots: &[&PathBuf]) -> Result<(), String> {
    for root in roots {
        reject_dangerous_root(root)?;
        reject_symlinked_path(root)?;
        if root.exists() {
            return Err(format!(
                "qualification root must not already exist: {}",
                root.display()
            ));
        }
    }
    for (index, left) in roots.iter().enumerate() {
        for right in roots.iter().skip(index + 1) {
            if left.starts_with(right) || right.starts_with(left) {
                return Err("qualification roots must be disjoint and non-nested".into());
            }
        }
    }
    Ok(())
}

fn reject_overlapping_qualification_paths(
    roots: &[&PathBuf],
    protected: &[&PathBuf],
) -> Result<(), String> {
    for path in protected {
        if !path.is_absolute()
            || path.components().any(|component| {
                matches!(
                    component,
                    std::path::Component::ParentDir | std::path::Component::CurDir
                )
            })
        {
            return Err("qualification inputs must use normalized absolute paths".into());
        }
        reject_symlinked_path(path)?;
    }
    for root in roots {
        for path in protected {
            if root.starts_with(path) || path.starts_with(root) {
                return Err("qualification roots overlap a signed or provider input".into());
            }
        }
    }
    Ok(())
}

fn reject_symlinked_path(path: &Path) -> Result<(), String> {
    for ancestor in path.ancestors() {
        match fs::symlink_metadata(ancestor) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                return Err(format!(
                    "qualification path traverses a symbolic link: {}",
                    ancestor.display()
                ));
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.to_string()),
        }
    }
    Ok(())
}

fn validate_real_provider_binary(path: &Path) -> Result<PathBuf, String> {
    let path = fs::canonicalize(path)
        .map_err(|error| format!("real provider binary is unavailable: {error}"))?;
    if !path.is_file() {
        return Err("provider command must be a real executable file".into());
    }
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        if fs::metadata(&path)
            .map_err(|error| error.to_string())?
            .permissions()
            .mode()
            & 0o111
            == 0
        {
            return Err("provider command is not executable".into());
        }
    }
    reject_mock_or_fake(path.to_string_lossy().as_ref(), "provider binary")?;
    Ok(path)
}

fn validate_model_configuration(model: &QualificationModelConfig) -> Result<(), String> {
    if !matches!(model.provider_boundary.as_str(), "hosted" | "local_model") {
        return Err("model provider boundary must be hosted or local_model".into());
    }
    if model.model.trim().is_empty() {
        return Err("qualification model must be explicit".into());
    }
    reject_mock_or_fake(&model.model, "model")?;
    if let Some(endpoint) = &model.endpoint {
        reject_mock_or_fake(endpoint, "provider endpoint")?;
        if model.provider_boundary == "hosted" && !endpoint.starts_with("https://") {
            return Err("hosted provider qualification requires an HTTPS endpoint".into());
        }
    }
    if let Some(name) = &model.credential_environment
        && (name.trim().is_empty()
            || name.contains('=')
            || name.bytes().any(|byte| byte.is_ascii_whitespace()))
    {
        return Err("credential_environment must name an environment variable, not a value".into());
    }
    Ok(())
}

fn reject_mock_or_fake(value: &str, label: &str) -> Result<(), String> {
    let normalized = value.to_ascii_lowercase();
    if ["mock", "fake", "stub", "dummy"]
        .iter()
        .any(|marker| normalized.contains(marker))
    {
        Err(format!("{label} is a mock, fake, stub, or dummy"))
    } else {
        Ok(())
    }
}

fn run_qualification_process(
    root: &Path,
    command: &QualificationCommandEvidence,
    environment: &[(&str, String)],
    evidence_root: &Path,
    index: usize,
) -> Result<(), String> {
    let label = command
        .arguments
        .first()
        .map_or("process", String::as_str)
        .replace(|character: char| !character.is_ascii_alphanumeric(), "-");
    let stdout_path = evidence_root.join(format!("{index:02}-{label}.stdout.log"));
    let stderr_path = evidence_root.join(format!("{index:02}-{label}.stderr.log"));
    let stdout = fs::File::create(&stdout_path).map_err(|error| error.to_string())?;
    let stderr = fs::File::create(&stderr_path).map_err(|error| error.to_string())?;
    let status = Command::new(&command.program)
        .args(&command.arguments)
        .envs(environment.iter().map(|(key, value)| (*key, value)))
        .current_dir(root)
        .stdin(std::process::Stdio::null())
        .stdout(std::process::Stdio::from(stdout))
        .stderr(std::process::Stdio::from(stderr))
        .status()
        .map_err(|error| format!("qualification step {index} could not start: {error}"))?;
    if status.success() {
        Ok(())
    } else {
        Err(format!(
            "qualification step {index} failed with {status}; inspect {} and {}",
            stdout_path.display(),
            stderr_path.display()
        ))
    }
}

fn read_and_validate_driver_evidence(
    path: &Path,
    build_id: &str,
    host_target: &str,
    provider_sha256: &str,
    model_sha256: &str,
    resource_limits_sha256: &str,
) -> Result<TeammatesDriverEvidence, String> {
    let bytes = fs::read(path)
        .map_err(|error| format!("packaged driver evidence is unavailable: {error}"))?;
    let evidence: TeammatesDriverEvidence = serde_json::from_slice(&bytes)
        .map_err(|error| format!("packaged driver evidence is invalid: {error}"))?;
    if evidence.schema_version != 1
        || evidence.release_build_id != build_id
        || evidence.host_target != host_target
        || evidence.provider_binary_sha256 != provider_sha256
        || evidence.model_configuration_sha256 != model_sha256
        || evidence.resource_limits_sha256 != resource_limits_sha256
    {
        return Err("packaged driver evidence does not bind the qualification plan".into());
    }
    if evidence.used_mock_or_fake || evidence.secret_scan_matches != 0 {
        return Err("packaged driver used fake infrastructure or exposed secret material".into());
    }
    let profiles = evidence
        .profiles
        .iter()
        .map(|profile| profile.profile_id.as_str())
        .collect::<BTreeSet<_>>();
    let permanent_dms = evidence
        .profiles
        .iter()
        .map(|profile| profile.permanent_human_dm_id.as_str())
        .collect::<BTreeSet<_>>();
    if evidence.profiles.len() != 4
        || profiles.len() != 4
        || permanent_dms.len() != 4
        || evidence.profiles.iter().any(|profile| !profile.enabled)
        || profiles.iter().any(|value| value.trim().is_empty())
        || permanent_dms.iter().any(|value| value.trim().is_empty())
    {
        return Err("qualification did not create four distinct profiles and permanent DMs".into());
    }
    for (label, value) in [
        ("agent-to-agent DM", evidence.agent_agent_dm_id.as_str()),
        ("group", evidence.group_conversation_id.as_str()),
        ("assignment", evidence.assignment_id.as_str()),
        ("handoff", evidence.handoff_id.as_str()),
        ("review", evidence.review_event_id.as_str()),
        ("completion", evidence.completion_event_id.as_str()),
        ("due routine DM", evidence.due_routine_dm_event_id.as_str()),
    ] {
        if value.trim().is_empty() {
            return Err(format!("qualification evidence is missing {label}"));
        }
    }
    if !evidence.daemon_killed_after_final_before_publication
        || !evidence.daemon_restarted
        || evidence.final_publication_count != 1
        || !evidence.reload_projection_matches
        || !evidence.resource_limits_enforced
        || !evidence.cross_profile_access_denied
        || !evidence.all_processes_stopped
    {
        return Err("daemon recovery, exactly-once, reload, isolation, or limits failed".into());
    }
    if !evidence.browser.headed_chromium
        || !evidence.browser.xvfb_display
        || !evidence.browser.authenticated_stream
        || !evidence.browser.takeover_authorized
        || !evidence.browser.unauthorized_input_denied
        || !evidence.browser.handback_completed
        || evidence.browser.audit_correlations.is_empty()
        || evidence
            .browser
            .audit_correlations
            .iter()
            .any(|value| value.trim().is_empty())
    {
        return Err("headed browser stream, takeover, handback, or audit proof failed".into());
    }
    Ok(evidence)
}

fn file_sha256(path: &Path) -> Result<String, String> {
    let bytes =
        fs::read(path).map_err(|error| format!("cannot hash {}: {error}", path.display()))?;
    if bytes.is_empty() {
        return Err(format!("required evidence is empty: {}", path.display()));
    }
    Ok(hex_encode(&Sha256::digest(bytes)))
}

fn data_inventory_digest(root: &Path) -> Result<String, String> {
    let inventory = release_files(root)?;
    let bytes = serde_json::to_vec(&inventory).map_err(|error| error.to_string())?;
    Ok(hex_encode(&Sha256::digest(bytes)))
}

fn validate_codify_trace(path: &Path) -> Result<(), String> {
    let trace = fs::read_to_string(path)
        .map_err(|error| format!("Codify trace evidence is unavailable: {error}"))?;
    if !trace.contains("9.1")
        || !trace.contains("teammates_release_qualify")
        || !trace.contains("apps/xtask/src/main.rs")
    {
        return Err("Codify trace does not bind task 9.1 to its declared symbol and file".into());
    }
    Ok(())
}

fn verify_release_command() -> Result<(), String> {
    let root = env::args_os()
        .nth(2)
        .map(PathBuf::from)
        .ok_or_else(|| "verify-release requires a release directory".to_owned())?;
    let encoded_key = env::args()
        .nth(3)
        .ok_or_else(|| "verify-release requires the trusted public key hex".to_owned())?;
    let key = decode_public_key(&encoded_key).map_err(|error| error.to_string())?;
    let verified = verify_signed_release(&root, &key).map_err(|error| error.to_string())?;
    let host_target = format!("{}-{}", env::consts::ARCH, env::consts::OS);
    if verified.manifest.target != host_target {
        return Err(format!(
            "release target {} does not match this host {host_target}",
            verified.manifest.target
        ));
    }
    verify_packaged_build_reports(&root, &verified.manifest).map_err(|error| error.to_string())?;
    println!(
        "verified {} signed release files for {} {} ({}) manifest_sha256={}",
        verified.manifest.files.len(),
        verified.manifest.package,
        verified.manifest.version,
        verified.manifest.build_id,
        verified.manifest_sha256
    );
    Ok(())
}

#[allow(clippy::too_many_lines)]
fn release(root: &Path) -> Result<(), String> {
    if BUILD_ID.trim().is_empty() || BUILD_ID.ends_with("+development") {
        return Err(
            "release requires KEITH_BUILD_ID to be set before compiling and running xtask".into(),
        );
    }
    let target = format!("{}-{}", env::consts::ARCH, env::consts::OS);
    let destination = env::args_os().nth(2).map_or_else(
        || {
            Ok::<_, String>(target_directory(root).join("packages").join(format!(
                "keith-agent-{}-{target}",
                env!("CARGO_PKG_VERSION")
            )))
        },
        |value| Ok(PathBuf::from(value)),
    )?;
    if destination.exists() {
        return Err(format!(
            "release destination already exists: {}",
            destination.display()
        ));
    }
    let signing_environment = "KEITH_RELEASE_SIGNING_KEY";
    let signing_seed = env::var_os(signing_environment)
        .ok_or_else(|| format!("{signing_environment} must contain a 64-character hex seed"))?;
    let signing_seed = decode_signing_seed(signing_seed.as_encoded_bytes())?;
    let signing_key = Ed25519KeyPair::from_seed_unchecked(&signing_seed)
        .map_err(|_| "release signing key is invalid".to_owned())?;

    run(
        root,
        "pnpm",
        &["--dir", "apps/agent-web/ui", "install", "--frozen-lockfile"],
    )?;
    run(
        root,
        "pnpm",
        &["--dir", "apps/agent-web/ui", "run", "build"],
    )?;
    run(
        root,
        "cargo",
        &["build", "--workspace", "--bins", "--release", "--locked"],
    )?;
    run(
        root,
        "cargo",
        &[
            "build",
            "-p",
            "keith-agent-web",
            "--lib",
            "--target",
            "wasm32-unknown-unknown",
            "--release",
            "--locked",
        ],
    )?;

    let parent = destination
        .parent()
        .filter(|path| !path.as_os_str().is_empty())
        .unwrap_or_else(|| Path::new("."));
    let name = destination
        .file_name()
        .and_then(OsStr::to_str)
        .ok_or_else(|| "release destination name must be UTF-8".to_owned())?;
    fs::create_dir_all(parent).map_err(|error| error.to_string())?;
    let staging = parent.join(format!(".{name}-{}.tmp", EntityId::new()));
    let result = assemble_release(root, &staging, target, &signing_key);
    if let Err(error) = result {
        if staging.exists() {
            fs::remove_dir_all(&staging).map_err(|cleanup| {
                format!("{error}; failed to remove staging release: {cleanup}")
            })?;
        }
        return Err(error);
    }
    if let Err(error) = fs::rename(&staging, &destination) {
        fs::remove_dir_all(&staging).map_err(|cleanup| {
            format!("failed to promote release: {error}; failed to remove staging: {cleanup}")
        })?;
        return Err(format!("failed to promote release: {error}"));
    }
    println!("release written to {}", destination.display());
    Ok(())
}

#[allow(clippy::too_many_lines)]
fn assemble_release(
    root: &Path,
    destination: &Path,
    target: String,
    signing_key: &Ed25519KeyPair,
) -> Result<(), String> {
    let bin = destination.join("bin");
    let web = destination.join("web");
    let schemas = destination.join("schemas");
    let provenance = destination.join("provenance");
    let services = destination.join("services");
    let migrations = destination.join("migrations");
    fs::create_dir_all(&bin).map_err(|error| error.to_string())?;
    fs::create_dir_all(&web).map_err(|error| error.to_string())?;
    fs::create_dir_all(&schemas).map_err(|error| error.to_string())?;
    fs::create_dir_all(&provenance).map_err(|error| error.to_string())?;
    fs::create_dir_all(&services).map_err(|error| error.to_string())?;
    fs::create_dir_all(&migrations).map_err(|error| error.to_string())?;
    let release_root = target_directory(root).join("release");
    for binary in [
        "agentd",
        "agent-worker",
        "agent-cli",
        "agent-tui",
        "channel-gateway",
        "tool-runner",
        "browser-runner",
        "kernel-runner",
        "agent-web",
        "agent-desktop",
    ] {
        let filename = format!("{binary}{}", env::consts::EXE_SUFFIX);
        let source = release_root.join(&filename);
        if !source.is_file() {
            return Err(format!("release binary is missing: {}", source.display()));
        }
        fs::copy(&source, bin.join(filename)).map_err(|error| error.to_string())?;
    }
    let wasm = target_directory(root).join("wasm32-unknown-unknown/release/keith_agent_web.wasm");
    let status = Command::new("wasm-bindgen")
        .args(["--target", "web", "--out-name", "agent_web", "--out-dir"])
        .arg(&web)
        .arg(&wasm)
        .current_dir(root)
        .status()
        .map_err(|error| format!("failed to run wasm-bindgen: {error}"))?;
    if !status.success() {
        return Err(format!("wasm-bindgen failed with {status}"));
    }
    copy_tree(&root.join("apps/agent-web/static/ui"), &web.join("ui"))?;
    copy_tree(
        &root.join("packaging/builtins"),
        &destination.join("builtins"),
    )?;
    fs::create_dir_all(destination.join("providers")).map_err(|error| error.to_string())?;
    fs::write(
        destination.join("providers/providers.json"),
        provider_metadata()?,
    )
    .map_err(|error| error.to_string())?;
    fs::create_dir_all(destination.join("docs")).map_err(|error| error.to_string())?;
    fs::copy(
        root.join("docs/installation.md"),
        destination.join("docs/installation.md"),
    )
    .map_err(|error| error.to_string())?;
    fs::copy(
        root.join("docs/release-qualification.md"),
        destination.join("docs/release-qualification.md"),
    )
    .map_err(|error| error.to_string())?;
    fs::copy(
        root.join("docs/discord.md"),
        destination.join("docs/discord.md"),
    )
    .map_err(|error| error.to_string())?;
    fs::copy(
        root.join("docs/openai-compatibility.md"),
        destination.join("docs/openai-compatibility.md"),
    )
    .map_err(|error| error.to_string())?;
    fs::copy(root.join("Cargo.lock"), provenance.join("Cargo.lock"))
        .map_err(|error| error.to_string())?;
    fs::copy(
        root.join("apps/agent-web/ui/pnpm-lock.yaml"),
        provenance.join("web-pnpm-lock.yaml"),
    )
    .map_err(|error| error.to_string())?;
    fs::write(
        schemas.join("agent-connection.md"),
        keith_protocol::schema_markdown().map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;
    fs::write(
        schemas.join("common-types.md"),
        keith_agent_types::schema_markdown().map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;
    fs::write(
        schemas.join("teammate-runtime.json"),
        serde_json::to_vec_pretty(&json!({
            "format": "keith-teammate-runtime-v1",
            "storage_schema": keith_agent_types::CURRENT_SCHEMA_VERSION.to_string(),
            "protocol_version": keith_agent_types::CURRENT_PROTOCOL_VERSION.to_string(),
            "durable_domains": ["profiles", "conversations", "coordination", "sessions", "artifacts", "channel_bindings", "computers"],
            "clients": ["agent-cli", "agent-tui", "agent-web", "agent-desktop"],
            "supervised_processes": ["agentd", "agent-worker", "browser-runner", "Xvfb", "Chromium"],
            "hosted_control_plane_required": false,
        }))
        .map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;
    fs::write(
        migrations.join("README.json"),
        serde_json::to_vec_pretty(&json!({
            "format": "keith-migration-index-v1",
            "target_storage_schema": keith_agent_types::CURRENT_SCHEMA_VERSION.to_string(),
            "backup_required": true,
            "rollback": "restore the exact portable backup before starting the prior agentd",
        }))
        .map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;
    fs::write(
        services.join("keith-agentd.service.template"),
        "[Unit]\nDescription=Keith Agent Daemon\nAfter=network.target\n[Service]\nType=simple\nExecStart={{INSTALL_ROOT}}/bin/agentd --data-root {{DATA_ROOT}}\nRestart=on-failure\nNoNewPrivileges=true\n[Install]\nWantedBy=default.target\n",
    )
    .map_err(|error| error.to_string())?;
    fs::write(
        services.join("keith-headed-browser.service.template"),
        "[Unit]\nDescription=Keith headed browser %i\n[Service]\nType=simple\nExecStart={{INSTALL_ROOT}}/bin/browser-runner --profile %i\nRestart=on-failure\nNoNewPrivileges=true\n",
    )
    .map_err(|error| error.to_string())?;
    write_dependency_reports(root, destination)?;

    let files = release_files(destination)?;
    let daemon = daemon_report();
    let worker = worker_report();
    let manifest = ReleaseManifest {
        format: keith_release::MANIFEST_FORMAT.into(),
        package: keith_release::PACKAGE_NAME.into(),
        version: env!("CARGO_PKG_VERSION").into(),
        target,
        build_id: BUILD_ID.into(),
        protocol_version: keith_agent_types::CURRENT_PROTOCOL_VERSION.to_string(),
        storage_schema: keith_agent_types::CURRENT_SCHEMA_VERSION.to_string(),
        components: BTreeMap::from([
            (daemon.component.clone(), daemon),
            (worker.component.clone(), worker),
        ]),
        files,
    };
    let manifest_bytes = serde_json::to_vec_pretty(&manifest).map_err(|error| error.to_string())?;
    fs::write(destination.join(MANIFEST_FILE), &manifest_bytes)
        .map_err(|error| error.to_string())?;
    let signature = signing_key.sign(&manifest_bytes);
    fs::write(
        destination.join(SIGNATURE_FILE),
        hex_encode(signature.as_ref()),
    )
    .map_err(|error| error.to_string())?;
    let public_key: [u8; 32] = signing_key
        .public_key()
        .as_ref()
        .try_into()
        .map_err(|_| "release signing public key has an invalid length".to_owned())?;
    fs::write(destination.join(PUBLIC_KEY_FILE), hex_encode(&public_key))
        .map_err(|error| error.to_string())?;
    let verified =
        verify_signed_release(destination, &public_key).map_err(|error| error.to_string())?;
    verify_packaged_build_reports(destination, &verified.manifest)
        .map_err(|error| error.to_string())?;
    Ok(())
}

fn provider_metadata() -> Result<Vec<u8>, String> {
    let providers = keith_provider_catalog::BUILTIN_PROVIDERS
        .iter()
        .map(|provider| {
            json!({
                "id": provider.id,
                "display_name": provider.display_name,
                "transport": provider.transport.as_str(),
                "credential_kind": provider.authentication.as_str(),
                "credential_environment": provider.credential_environment,
                "default_base_url": provider.default_base_url,
                "default_model": provider.default_model,
            })
        })
        .collect::<Vec<_>>();
    serde_json::to_vec_pretty(&json!({
        "schema_version": 1,
        "providers": providers,
    }))
    .map_err(|error| error.to_string())
}

fn target_directory(root: &Path) -> PathBuf {
    env::var_os("CARGO_TARGET_DIR").map_or_else(|| root.join("target"), PathBuf::from)
}

fn write_dependency_reports(root: &Path, destination: &Path) -> Result<(), String> {
    let output = Command::new("cargo")
        .args(["metadata", "--format-version", "1", "--locked"])
        .current_dir(root)
        .output()
        .map_err(|error| format!("failed to run cargo metadata: {error}"))?;
    if !output.status.success() {
        return Err("cargo metadata failed while generating dependency reports".into());
    }
    let metadata: Value =
        serde_json::from_slice(&output.stdout).map_err(|error| error.to_string())?;
    let mut packages = metadata["packages"]
        .as_array()
        .ok_or_else(|| "cargo metadata omitted packages".to_owned())?
        .iter()
        .map(|package| {
            let name = package["name"].as_str().unwrap_or_default();
            let version = package["version"].as_str().unwrap_or_default();
            let license = package["license"].as_str();
            let mut component = serde_json::json!({
                "type": "library",
                "name": name,
                "version": version,
                "purl": format!("pkg:cargo/{name}@{version}")
            });
            if let Some(license) = license {
                component["licenses"] = serde_json::json!([{"expression": license}]);
            }
            let report = serde_json::json!({
                "name": name,
                "version": version,
                "license": license,
                "repository": package["repository"].as_str()
            });
            (format!("{name}@{version}"), component, report)
        })
        .collect::<Vec<_>>();
    packages.sort_by(|left, right| left.0.cmp(&right.0));
    let sbom = serde_json::json!({
        "bomFormat": "CycloneDX",
        "specVersion": "1.5",
        "version": 1,
        "metadata": {"component": {"type": "application", "name": "keith-agent", "version": env!("CARGO_PKG_VERSION")}},
        "components": packages.iter().map(|(_, component, _)| component).collect::<Vec<_>>()
    });
    let licenses = serde_json::json!({
        "format": "keith-license-report-v1",
        "packages": packages.iter().map(|(_, _, report)| report).collect::<Vec<_>>()
    });
    fs::write(
        destination.join("sbom.cdx.json"),
        serde_json::to_vec_pretty(&sbom).map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;
    fs::write(
        destination.join("licenses.json"),
        serde_json::to_vec_pretty(&licenses).map_err(|error| error.to_string())?,
    )
    .map_err(|error| error.to_string())?;
    Ok(())
}

fn release_files(root: &Path) -> Result<Vec<ReleaseFile>, String> {
    let mut paths = Vec::new();
    collect_files(root, root, &mut paths)?;
    paths.sort();
    paths
        .into_iter()
        .filter(|path| {
            path != Path::new(MANIFEST_FILE)
                && path != Path::new(SIGNATURE_FILE)
                && path != Path::new(PUBLIC_KEY_FILE)
        })
        .map(|relative| {
            let path = root.join(&relative);
            let bytes = fs::read(&path).map_err(|error| error.to_string())?;
            Ok(ReleaseFile {
                path: relative.to_string_lossy().replace('\\', "/"),
                bytes: u64::try_from(bytes.len()).map_err(|error| error.to_string())?,
                sha256: hex_encode(&Sha256::digest(bytes)),
            })
        })
        .collect()
}

fn collect_files(root: &Path, directory: &Path, files: &mut Vec<PathBuf>) -> Result<(), String> {
    for entry in fs::read_dir(directory).map_err(|error| error.to_string())? {
        let entry = entry.map_err(|error| error.to_string())?;
        let path = entry.path();
        let file_type = entry.file_type().map_err(|error| error.to_string())?;
        if file_type.is_dir() {
            collect_files(root, &path, files)?;
        } else if file_type.is_file() {
            files.push(
                path.strip_prefix(root)
                    .map_err(|error| error.to_string())?
                    .to_path_buf(),
            );
        } else {
            return Err(format!(
                "release contains unsupported entry: {}",
                path.display()
            ));
        }
    }
    Ok(())
}

fn decode_signing_seed(encoded: &[u8]) -> Result<[u8; 32], String> {
    if encoded.len() != 64 {
        return Err("release signing key must be 64 hexadecimal characters".into());
    }
    let mut decoded = [0_u8; 32];
    for (target, pair) in decoded.iter_mut().zip(encoded.chunks_exact(2)) {
        *target = (hex_digit(pair[0])? << 4) | hex_digit(pair[1])?;
    }
    Ok(decoded)
}

fn hex_digit(value: u8) -> Result<u8, String> {
    match value {
        b'0'..=b'9' => Ok(value - b'0'),
        b'a'..=b'f' => Ok(value - b'a' + 10),
        b'A'..=b'F' => Ok(value - b'A' + 10),
        _ => Err("hexadecimal value is invalid".into()),
    }
}

fn workspace_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .parent()
        .and_then(Path::parent)
        .expect("xtask is nested under apps")
        .to_path_buf()
}

fn ci() -> Result<(), String> {
    let root = workspace_root();
    run(&root, "cargo", &["fmt", "--all", "--", "--check"])?;
    dependency_policy(&root)?;
    schema_document(&root, false)?;
    protocol_document(&root, false)?;
    provider_metadata_document(&root, false)?;
    security::run(&root)?;
    run(
        &root,
        "cargo",
        &["check", "--workspace", "--all-targets", "--locked"],
    )?;
    run(
        &root,
        "cargo",
        &[
            "clippy",
            "--workspace",
            "--all-targets",
            "--locked",
            "--",
            "-D",
            "warnings",
        ],
    )?;
    run(&root, "cargo", &["test", "--workspace", "--locked"])?;
    run_with_env(
        &root,
        "cargo",
        &["doc", "--workspace", "--no-deps", "--locked"],
        "RUSTDOCFLAGS",
        "-D warnings",
    )
}

fn protocol_document(root: &Path, write: bool) -> Result<(), String> {
    let path = root.join("docs/reference/agent-connection.md");
    let expected = keith_protocol::schema_markdown().map_err(|error| error.to_string())?;
    checked_generated_document(&path, expected, write)
}

fn schema_document(root: &Path, write: bool) -> Result<(), String> {
    let path = root.join("docs/reference/common-types.md");
    let expected = keith_agent_types::schema_markdown().map_err(|error| error.to_string())?;
    checked_generated_document(&path, expected, write)
}

fn provider_metadata_document(root: &Path, write: bool) -> Result<(), String> {
    let path = root.join("packaging/providers.json");
    let mut expected =
        String::from_utf8(provider_metadata()?).map_err(|error| error.to_string())?;
    expected.push('\n');
    checked_generated_document(&path, expected, write)
}

fn checked_generated_document(path: &Path, expected: String, write: bool) -> Result<(), String> {
    if write {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent).map_err(|error| error.to_string())?;
        }
        fs::write(path, expected).map_err(|error| error.to_string())?;
        return Ok(());
    }
    let actual = fs::read_to_string(path)
        .map_err(|error| format!("schema document {} is missing: {error}", path.display()))?;
    if actual == expected {
        Ok(())
    } else {
        Err(format!(
            "generated document {} is stale; run the corresponding keith-xtask document command with --write",
            path.display()
        ))
    }
}

fn clean_checkout() -> Result<(), String> {
    let root = workspace_root();
    let destination = env::temp_dir().join(format!("keith-clean-{}", std::process::id()));
    if destination.exists() {
        fs::remove_dir_all(&destination).map_err(|error| error.to_string())?;
    }
    copy_tree(&root, &destination)?;
    let result = run(
        &destination,
        "cargo",
        &["test", "--workspace", "--locked", "--offline", "--quiet"],
    );
    fs::remove_dir_all(&destination).map_err(|error| error.to_string())?;
    result
}

fn copy_tree(source: &Path, destination: &Path) -> Result<(), String> {
    fs::create_dir_all(destination).map_err(|error| error.to_string())?;
    for entry in fs::read_dir(source).map_err(|error| error.to_string())? {
        let entry = entry.map_err(|error| error.to_string())?;
        let name = entry.file_name();
        if matches!(name.to_str(), Some(".git" | ".codegraph" | "target")) {
            continue;
        }
        let source_path = entry.path();
        let destination_path = destination.join(name);
        let file_type = entry.file_type().map_err(|error| error.to_string())?;
        if file_type.is_dir() {
            copy_tree(&source_path, &destination_path)?;
        } else if file_type.is_file() {
            fs::copy(source_path, destination_path).map_err(|error| error.to_string())?;
        }
    }
    Ok(())
}

fn dependency_policy(root: &Path) -> Result<(), String> {
    let manifests = manifests(root)?;
    let graph = dependency_graph(&manifests)?;
    let forbidden_layers = BTreeSet::from([
        "keith-daemon-core",
        "keith-supervisor",
        "keith-worker-runtime",
        "keith-provider-adapters",
        "keith-channel-adapters",
        "keith-ui-model",
    ]);
    let domains = [
        "keith-session",
        "keith-goals",
        "keith-memory",
        "keith-scheduler",
        "keith-routing",
        "keith-attention",
    ];

    require_no_internal_dependencies(&graph, "keith-agent-types")?;
    for domain in domains {
        reject_reachable(&graph, domain, &forbidden_layers)?;
    }
    reject_reachable(
        &graph,
        "keith-session",
        &BTreeSet::from([
            "keith-provider-adapters",
            "keith-channel-adapters",
            "keith-tool-runner-core",
            "keith-ui-model",
        ]),
    )?;
    reject_reachable(
        &graph,
        "keith-provider-adapters",
        &BTreeSet::from(["keith-session-store"]),
    )?;
    reject_reachable(
        &graph,
        "keith-channel-adapters",
        &BTreeSet::from(["keith-worker-runtime", "keith-session"]),
    )?;
    reject_reachable(
        &graph,
        "keith-state-store",
        &BTreeSet::from([
            "keith-daemon-core",
            "keith-supervisor",
            "keith-worker-runtime",
        ]),
    )?;
    reject_reachable(
        &graph,
        "keith-daemon-core",
        &BTreeSet::from([
            "keith-agent-loop",
            "keith-provider-adapters",
            "keith-tool-runner-core",
            "keith-sandbox",
            "keith-plugin-host",
        ]),
    )?;

    println!(
        "dependency policy passed for {} workspace packages",
        graph.len()
    );
    Ok(())
}

fn manifests(root: &Path) -> Result<Vec<PathBuf>, String> {
    let mut result = Vec::new();
    for parent in [root.join("crates"), root.join("apps")] {
        for entry in fs::read_dir(parent).map_err(|error| error.to_string())? {
            let path = entry
                .map_err(|error| error.to_string())?
                .path()
                .join("Cargo.toml");
            if path.is_file() {
                result.push(path);
            }
        }
    }
    Ok(result)
}

fn dependency_graph(manifests: &[PathBuf]) -> Result<BTreeMap<String, BTreeSet<String>>, String> {
    let mut names = BTreeMap::new();
    for manifest in manifests {
        let content = fs::read_to_string(manifest).map_err(|error| error.to_string())?;
        let name = package_name(&content)
            .ok_or_else(|| format!("missing package name in {}", manifest.display()))?;
        names.insert(manifest.clone(), name);
    }
    let package_names: BTreeSet<_> = names.values().cloned().collect();
    let mut graph = BTreeMap::new();
    for (manifest, name) in names {
        let content = fs::read_to_string(manifest).map_err(|error| error.to_string())?;
        let dependencies = production_dependency_lines(&content)
            .filter_map(dependency_name)
            .filter(|dependency| package_names.contains(*dependency))
            .map(str::to_owned)
            .collect();
        graph.insert(name, dependencies);
    }
    Ok(graph)
}

fn production_dependency_lines(content: &str) -> impl Iterator<Item = &str> {
    let mut production = false;
    content.lines().filter(move |line| {
        let trimmed = line.trim();
        if trimmed.starts_with('[') {
            production = trimmed == "[dependencies]"
                || (trimmed.starts_with("[target.") && trimmed.ends_with(".dependencies]"));
            false
        } else {
            production
        }
    })
}

fn package_name(content: &str) -> Option<String> {
    let mut in_package = false;
    for line in content.lines() {
        let trimmed = line.trim();
        if trimmed.starts_with('[') {
            in_package = trimmed == "[package]";
        } else if in_package && trimmed.starts_with("name") {
            return quoted_value(trimmed).map(str::to_owned);
        }
    }
    None
}

fn dependency_name(line: &str) -> Option<&str> {
    let trimmed = line.trim();
    let (name, value) = trimmed.split_once('=')?;
    let name = name.trim();
    if name.starts_with("keith-") && value.contains("path") {
        Some(name)
    } else {
        None
    }
}

fn quoted_value(line: &str) -> Option<&str> {
    let (_, value) = line.split_once('=')?;
    value.trim().strip_prefix('"')?.strip_suffix('"')
}

fn require_no_internal_dependencies(
    graph: &BTreeMap<String, BTreeSet<String>>,
    package: &str,
) -> Result<(), String> {
    let dependencies = graph
        .get(package)
        .ok_or_else(|| format!("policy package missing: {package}"))?;
    if dependencies.is_empty() {
        Ok(())
    } else {
        Err(format!(
            "{package} must not depend on internal packages: {dependencies:?}"
        ))
    }
}

fn reject_reachable(
    graph: &BTreeMap<String, BTreeSet<String>>,
    start: &str,
    forbidden: &BTreeSet<&str>,
) -> Result<(), String> {
    let mut pending = VecDeque::from([start]);
    let mut visited = BTreeSet::new();
    while let Some(package) = pending.pop_front() {
        if !visited.insert(package) {
            continue;
        }
        for dependency in graph.get(package).into_iter().flatten() {
            if forbidden.contains(dependency.as_str()) {
                return Err(format!(
                    "prohibited dependency path: {start} reaches {dependency}"
                ));
            }
            pending.push_back(dependency);
        }
    }
    Ok(())
}

fn run(root: &Path, program: &str, args: &[&str]) -> Result<(), String> {
    let status = Command::new(program)
        .args(args)
        .current_dir(root)
        .status()
        .map_err(|error| format!("failed to run {program}: {error}"))?;
    if status.success() {
        Ok(())
    } else {
        Err(format!("{program} {} failed with {status}", args.join(" ")))
    }
}

fn run_with_env(
    root: &Path,
    program: &str,
    args: &[&str],
    key: impl AsRef<OsStr>,
    value: impl AsRef<OsStr>,
) -> Result<(), String> {
    let status = Command::new(program)
        .args(args)
        .env(key, value)
        .current_dir(root)
        .status()
        .map_err(|error| format!("failed to run {program}: {error}"))?;
    if status.success() {
        Ok(())
    } else {
        Err(format!("{program} {} failed with {status}", args.join(" ")))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn dependency_policy_excludes_test_only_edges_and_keeps_target_runtime_edges() {
        let manifest = r#"
            [dependencies]
            keith-runtime = { path = "../runtime" }
            [dev-dependencies]
            keith-test-support = { path = "../test-support" }
            [target.'cfg(windows)'.dependencies]
            keith-windows = { path = "../windows" }
        "#;
        let dependencies = production_dependency_lines(manifest)
            .filter_map(dependency_name)
            .collect::<Vec<_>>();
        assert_eq!(dependencies, ["keith-runtime", "keith-windows"]);
    }
}
