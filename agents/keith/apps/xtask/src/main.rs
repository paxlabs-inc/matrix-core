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
        Some("security-probes") => security::run_source(&workspace_root()),
        Some("security-gate") => security::run(&workspace_root()),
        Some("platform-gate") => platform::run(&workspace_root()),
        Some("release") => release(&workspace_root()),
        Some("verify-release") => verify_release_command(),
        _ => Err(
            "usage: cargo xtask <ci|clean-checkout|dependency-policy|schema-doc [--write]|protocol-doc [--write]|provider-metadata [--write]|security-probes|security-gate|platform-gate|release [OUTPUT]|verify-release PATH EXPECTED_PUBLIC_KEY_HEX>".into(),
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
    fs::create_dir_all(&bin).map_err(|error| error.to_string())?;
    fs::create_dir_all(&web).map_err(|error| error.to_string())?;
    fs::create_dir_all(&schemas).map_err(|error| error.to_string())?;
    fs::create_dir_all(&provenance).map_err(|error| error.to_string())?;
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
        "keith-agent-acp",
        "keith-composio-mcp",
        "keith-cua-runner",
        "keith-performance-runner",
    ] {
        let filename = format!("{binary}{}", env::consts::EXE_SUFFIX);
        let source = release_root.join(&filename);
        if !source.is_file() {
            return Err(format!("release binary is missing: {}", source.display()));
        }
        fs::copy(&source, bin.join(filename)).map_err(|error| error.to_string())?;
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
    write_dependency_reports(root, destination)?;
    harden_release_permissions(destination)?;

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
    let mut files = paths
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
        .collect::<Result<Vec<_>, String>>()?;
    files.sort_by(|left, right| left.path.cmp(&right.path));
    if files.windows(2).any(|pair| pair[0].path == pair[1].path) {
        return Err("release contains duplicate normalized paths".into());
    }
    Ok(files)
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

#[cfg(unix)]
fn harden_release_permissions(root: &Path) -> Result<(), String> {
    use std::os::unix::fs::PermissionsExt as _;

    fn harden(root: &Path, path: &Path) -> Result<(), String> {
        let metadata = fs::symlink_metadata(path).map_err(|error| error.to_string())?;
        if metadata.file_type().is_symlink() {
            return Err(format!(
                "release contains a symbolic link: {}",
                path.display()
            ));
        }
        if metadata.is_dir() {
            fs::set_permissions(path, fs::Permissions::from_mode(0o755))
                .map_err(|error| error.to_string())?;
            for entry in fs::read_dir(path).map_err(|error| error.to_string())? {
                harden(root, &entry.map_err(|error| error.to_string())?.path())?;
            }
            return Ok(());
        }
        if !metadata.is_file() {
            return Err(format!(
                "release contains an unsupported entry: {}",
                path.display()
            ));
        }
        let mode = if path.starts_with(root.join("bin")) {
            0o755
        } else {
            0o644
        };
        fs::set_permissions(path, fs::Permissions::from_mode(mode))
            .map_err(|error| error.to_string())
    }

    harden(root, root)
}

#[cfg(not(unix))]
const fn harden_release_permissions(_root: &Path) -> Result<(), String> {
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
    security::run_source(&root)?;
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
    require_private_packages(&manifests)?;
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
    reject_reachable_except_via(
        &graph,
        "keith-daemon-core",
        &BTreeMap::from([
            ("keith-agent-loop", BTreeSet::new()),
            ("keith-provider-adapters", BTreeSet::new()),
            (
                "keith-tool-runner-core",
                BTreeSet::from(["keith-self-evolution"]),
            ),
            ("keith-sandbox", BTreeSet::from(["keith-self-evolution"])),
            ("keith-plugin-host", BTreeSet::new()),
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
        collect_manifests(&parent, &mut result)?;
    }
    result.sort();
    Ok(result)
}

fn collect_manifests(directory: &Path, manifests: &mut Vec<PathBuf>) -> Result<(), String> {
    for entry in fs::read_dir(directory).map_err(|error| error.to_string())? {
        let entry = entry.map_err(|error| error.to_string())?;
        let path = entry.path();
        if entry
            .file_type()
            .map_err(|error| error.to_string())?
            .is_dir()
        {
            if path.file_name().is_some_and(|name| name == "target") {
                continue;
            }
            collect_manifests(&path, manifests)?;
        } else if path.file_name().is_some_and(|name| name == "Cargo.toml") {
            manifests.push(path);
        }
    }
    Ok(())
}

fn require_private_packages(manifests: &[PathBuf]) -> Result<(), String> {
    for manifest in manifests {
        let content = fs::read_to_string(manifest).map_err(|error| error.to_string())?;
        if !package_publish_is_disabled(&content) {
            return Err(format!(
                "workspace package {} must set publish = false",
                manifest.display()
            ));
        }
    }
    Ok(())
}

fn package_publish_is_disabled(content: &str) -> bool {
    let mut in_package = false;
    for line in content.lines() {
        let trimmed = line.trim();
        if trimmed.starts_with('[') {
            in_package = trimmed == "[package]";
        } else if in_package && trimmed.starts_with("publish") {
            return trimmed
                .split_once('=')
                .is_some_and(|(_, value)| value.trim() == "false");
        }
    }
    false
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

fn reject_reachable_except_via(
    graph: &BTreeMap<String, BTreeSet<String>>,
    start: &str,
    forbidden: &BTreeMap<&str, BTreeSet<&str>>,
) -> Result<(), String> {
    let mut pending = graph
        .get(start)
        .into_iter()
        .flatten()
        .map(|dependency| (dependency.as_str(), dependency.as_str()))
        .collect::<VecDeque<_>>();
    let mut visited = BTreeSet::new();
    while let Some((package, first_hop)) = pending.pop_front() {
        if !visited.insert((package, first_hop)) {
            continue;
        }
        if let Some(approved_bridges) = forbidden.get(package)
            && !approved_bridges.contains(first_hop)
        {
            return Err(format!(
                "prohibited dependency path: {start} reaches {package} via {first_hop}"
            ));
        }
        for dependency in graph.get(package).into_iter().flatten() {
            pending.push_back((dependency, first_hop));
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

    #[test]
    fn dependency_policy_requires_private_workspace_packages() {
        assert!(package_publish_is_disabled(
            "[package]\nname = \"private\"\npublish = false\n"
        ));
        assert!(!package_publish_is_disabled(
            "[package]\nname = \"public\"\npublish = true\n"
        ));
        assert!(!package_publish_is_disabled(
            "[package]\nname = \"implicit-public\"\n"
        ));
    }

    #[test]
    fn daemon_executor_access_requires_the_self_evolution_bridge() {
        let allowed = BTreeMap::from([
            (
                "keith-daemon-core".into(),
                BTreeSet::from(["keith-self-evolution".into()]),
            ),
            (
                "keith-self-evolution".into(),
                BTreeSet::from(["keith-sandbox".into(), "keith-tool-runner-core".into()]),
            ),
            ("keith-sandbox".into(), BTreeSet::new()),
            ("keith-tool-runner-core".into(), BTreeSet::new()),
        ]);
        let forbidden = BTreeMap::from([
            ("keith-sandbox", BTreeSet::from(["keith-self-evolution"])),
            (
                "keith-tool-runner-core",
                BTreeSet::from(["keith-self-evolution"]),
            ),
        ]);
        reject_reachable_except_via(&allowed, "keith-daemon-core", &forbidden).unwrap();

        let direct = BTreeMap::from([
            (
                "keith-daemon-core".into(),
                BTreeSet::from(["keith-sandbox".into()]),
            ),
            ("keith-sandbox".into(), BTreeSet::new()),
        ]);
        assert!(reject_reachable_except_via(&direct, "keith-daemon-core", &forbidden).is_err());

        let wrong_bridge = BTreeMap::from([
            (
                "keith-daemon-core".into(),
                BTreeSet::from(["keith-runtime-api".into()]),
            ),
            (
                "keith-runtime-api".into(),
                BTreeSet::from(["keith-sandbox".into()]),
            ),
            ("keith-sandbox".into(), BTreeSet::new()),
        ]);
        assert!(
            reject_reachable_except_via(&wrong_bridge, "keith-daemon-core", &forbidden).is_err()
        );
    }
}
