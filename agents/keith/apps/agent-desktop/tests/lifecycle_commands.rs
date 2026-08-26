use std::collections::{BTreeMap, BTreeSet};
use std::fs;
use std::path::Path;
use std::process::{Command, Output};

use keith_agent_types::{CURRENT_PROTOCOL_VERSION, CURRENT_SCHEMA_VERSION};
use keith_release::{
    BuildReport, MANIFEST_FILE, MANIFEST_FORMAT, PACKAGE_NAME, PUBLIC_KEY_FILE, ReleaseFile,
    ReleaseManifest, SIGNATURE_FILE,
};
use ring::signature::{Ed25519KeyPair, KeyPair};
use serde_json::Value;
use sha2::{Digest, Sha256};

fn desktop(arguments: &[&str]) -> Output {
    Command::new(env!("CARGO_BIN_EXE_agent-desktop"))
        .args(arguments)
        .output()
        .expect("execute packaged desktop lifecycle command")
}

fn stdout(output: Output) -> String {
    assert!(
        output.status.success(),
        "desktop command failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout)
        .expect("utf8 output")
        .trim()
        .to_owned()
}

fn path_string(path: &Path) -> String {
    path.to_str().expect("utf8 temporary path").to_owned()
}

fn signed_release(root: &Path, version: &str, payload: &[u8], seed: [u8; 32]) -> String {
    fs::create_dir(root).unwrap();
    fs::create_dir(root.join("bin")).unwrap();
    let executable = Path::new(env!("CARGO_BIN_EXE_keith-release-report-host"));
    let daemon_name = format!("agentd{}", std::env::consts::EXE_SUFFIX);
    let worker_name = format!("agent-worker{}", std::env::consts::EXE_SUFFIX);
    fs::copy(executable, root.join("bin").join(&daemon_name)).unwrap();
    fs::copy(executable, root.join("bin").join(&worker_name)).unwrap();
    fs::write(root.join("payload.bin"), payload).unwrap();
    let build_id = "desktop-command-release-test";
    let protocol_version = CURRENT_PROTOCOL_VERSION.to_string();
    let storage_schema = CURRENT_SCHEMA_VERSION.to_string();
    let report = |component: &str| BuildReport {
        component: component.into(),
        package_version: version.into(),
        build_id: build_id.into(),
        protocol_version: protocol_version.clone(),
        storage_schema: storage_schema.clone(),
        enabled_features: BTreeSet::from(["release-test".into()]),
    };
    let daemon_report = report("daemon");
    let worker_report = report("worker");
    fs::write(
        root.join("bin").join(format!("{daemon_name}.report.json")),
        serde_json::to_vec(&daemon_report).unwrap(),
    )
    .unwrap();
    fs::write(
        root.join("bin").join(format!("{worker_name}.report.json")),
        serde_json::to_vec(&worker_report).unwrap(),
    )
    .unwrap();
    let mut paths = vec![
        format!("bin/{daemon_name}"),
        format!("bin/{daemon_name}.report.json"),
        format!("bin/{worker_name}"),
        format!("bin/{worker_name}.report.json"),
        "payload.bin".to_owned(),
    ];
    paths.sort();
    let files = paths
        .into_iter()
        .map(|path| {
            let bytes = fs::read(root.join(&path)).unwrap();
            ReleaseFile {
                path,
                bytes: u64::try_from(bytes.len()).unwrap(),
                sha256: keith_release::hex_encode(&Sha256::digest(bytes)),
            }
        })
        .collect();
    let manifest = ReleaseManifest {
        format: MANIFEST_FORMAT.into(),
        package: PACKAGE_NAME.into(),
        version: version.into(),
        target: format!("{}-{}", std::env::consts::ARCH, std::env::consts::OS),
        build_id: build_id.into(),
        protocol_version,
        storage_schema,
        components: BTreeMap::from([
            ("daemon".into(), daemon_report),
            ("worker".into(), worker_report),
        ]),
        files,
    };
    let bytes = serde_json::to_vec_pretty(&manifest).unwrap();
    let key = Ed25519KeyPair::from_seed_unchecked(&seed).unwrap();
    fs::write(root.join(MANIFEST_FILE), &bytes).unwrap();
    fs::write(
        root.join(SIGNATURE_FILE),
        keith_release::hex_encode(key.sign(&bytes).as_ref()),
    )
    .unwrap();
    let public_key = keith_release::hex_encode(key.public_key().as_ref());
    fs::write(root.join(PUBLIC_KEY_FILE), &public_key).unwrap();
    public_key
}

#[test]
fn executable_backup_update_rollback_restore_and_uninstall_lifecycle() {
    let directory = tempfile::tempdir().unwrap();
    let state = directory.path().join("state");
    let data = directory.path().join("data");
    let state_string = path_string(&state);
    let data_string = path_string(&data);
    stdout(desktop(&[
        "setup",
        &state_string,
        &data_string,
        "http://127.0.0.1:7341",
    ]));

    let settings: Value = serde_json::from_str(&stdout(desktop(&["settings", &state_string])))
        .expect("settings JSON");
    let installation = settings["installation_id"].as_str().unwrap();
    fs::write(data.join("durable-session.jsonl"), b"session state\n").unwrap();

    let backup = stdout(desktop(&["backup", &state_string]));
    let restored = directory.path().join("restored-data");
    let restored_string = path_string(&restored);
    stdout(desktop(&["restore", &backup, &restored_string]));
    assert_eq!(
        fs::read(restored.join("durable-session.jsonl")).unwrap(),
        b"session state\n"
    );

    let release_one = directory.path().join("release-one");
    let release_two = directory.path().join("release-two");
    let public_key = signed_release(&release_one, "1.0.0", b"version one", [11_u8; 32]);
    assert_eq!(
        public_key,
        signed_release(&release_two, "2.0.0", b"version two", [11_u8; 32])
    );
    let release_one_string = path_string(&release_one);
    let release_two_string = path_string(&release_two);
    let verified: Value = serde_json::from_str(&stdout(desktop(&[
        "verify-release",
        &release_one_string,
        &public_key,
    ])))
    .unwrap();
    assert_eq!(verified["manifest"]["version"], "1.0.0");
    assert!(verified["manifest_sha256"].as_str().is_some());
    assert_eq!(verified["public_key_hex"], public_key);
    let active_one: Value = serde_json::from_str(&stdout(desktop(&[
        "update",
        &state_string,
        &release_one_string,
        &public_key,
    ])))
    .unwrap();
    assert_eq!(active_one["current"], "1.0.0");
    let active_two: Value = serde_json::from_str(&stdout(desktop(&[
        "update",
        &state_string,
        &release_two_string,
        &public_key,
    ])))
    .unwrap();
    assert_eq!(active_two["previous"], "1.0.0");
    let rolled: Value =
        serde_json::from_str(&stdout(desktop(&["rollback", &state_string]))).unwrap();
    assert_eq!(rolled["current"], "1.0.0");

    let plan: Value = serde_json::from_str(&stdout(desktop(&[
        "uninstall-plan",
        &state_string,
        "keep-user-data",
    ])))
    .unwrap();
    assert_eq!(plan["confirmation"], format!("REMOVE {installation}"));
    let rejected = desktop(&[
        "uninstall",
        &state_string,
        "keep-user-data",
        "wrong confirmation",
    ]);
    assert!(!rejected.status.success());
    stdout(desktop(&[
        "uninstall",
        &state_string,
        "keep-user-data",
        plan["confirmation"].as_str().unwrap(),
    ]));
    assert!(!state.join("updates").exists());
    assert!(state.join("desktop.json").exists());
    assert_eq!(
        fs::read(data.join("durable-session.jsonl")).unwrap(),
        b"session state\n"
    );
}
