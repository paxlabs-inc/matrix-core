use std::fs;
use std::time::Duration;

use keith_self_evolution::{CanaryRunner, CanaryVerdict};
use keith_supervisor::{InstalledImage, SupervisorOptions};
use keith_telemetry::MetricName;
use sha2::{Digest, Sha256};

fn digest(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .fold(String::new(), |mut output, byte| {
            use std::fmt::Write as _;
            let _ = write!(output, "{byte:02x}");
            output
        })
}

#[test]
fn installed_candidate_runs_all_corpus_journeys_in_a_real_isolated_generation() {
    let directory = tempfile::tempdir().unwrap();
    let executable = std::path::PathBuf::from(env!("CARGO_BIN_EXE_agent-worker"));
    let executable_sha256 = digest(&fs::read(&executable).unwrap());
    let manifest_sha256 = digest(b"process-test-candidate-manifest");
    let image = InstalledImage {
        image_id: manifest_sha256.clone(),
        build_id: "process-test-candidate".into(),
        manifest_sha256,
        source_manifest_sha256: digest(b"process-test-source"),
        executable_sha256,
        change_class: "c".into(),
        executable,
        sequence: 1,
        verified: true,
    };
    let options = SupervisorOptions {
        startup_timeout: Duration::from_secs(5),
        drain_timeout: Duration::from_secs(2),
        stale_heartbeat: Duration::from_secs(2),
        heartbeat_interval: Duration::from_millis(50),
        lease_duration: Duration::from_secs(3),
    };
    let canary_root = directory.path().join("installation-owned-canaries");
    let runner = CanaryRunner::open(&canary_root, options).unwrap();
    let evaluation = runner
        .evaluate(&image, MetricName::ModelLatency, 1_000.0, 1_000.0)
        .unwrap();

    assert_eq!(evaluation.image_id, image.image_id);
    assert!(evaluation.generation.is_some());
    assert_eq!(evaluation.measurements.len(), 7);
    assert_eq!(evaluation.verdict, CanaryVerdict::Passed);
    assert!(fs::read_dir(&canary_root).unwrap().next().is_none());
}
