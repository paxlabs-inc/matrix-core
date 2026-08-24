use std::fs;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use keith_self_evolution::{BuildCheckpoint, BuildCheckpointJournal};

const BASE: &str = "1111111111111111111111111111111111111111";
const SOURCE_DIGEST: &str = "2222222222222222222222222222222222222222222222222222222222222222";
const INVARIANTS: [(&str, &[u8]); 5] = [
    ("live/source.rs", b"known-good source"),
    ("images/installed", b"installed-image"),
    ("images/current", b"current-image"),
    ("images/pinned", b"pinned-image"),
    ("ledger/events", b"signed-ledger-prefix"),
];

#[test]
fn build_crash_boundary_matrix_keeps_external_state_exact_and_recoverable() {
    if std::env::var_os("KEITH_BUILD_CHILD_ROOT").is_some() {
        return;
    }
    for checkpoint in [
        BuildCheckpoint::Prepared,
        BuildCheckpoint::ToolchainIdentified,
        BuildCheckpoint::GatesPassed,
        BuildCheckpoint::ArtifactRead,
        BuildCheckpoint::ImageSigned,
    ] {
        let temporary = tempfile::tempdir().unwrap();
        seed_invariants(temporary.path());
        let mut child = Command::new(std::env::current_exe().unwrap())
            .args(["--exact", "build_crash_child", "--nocapture"])
            .env("KEITH_BUILD_CHILD_ROOT", temporary.path())
            .env("KEITH_BUILD_PAUSE_AT", checkpoint_name(checkpoint))
            .stdin(Stdio::null())
            .stdout(Stdio::null())
            .stderr(Stdio::null())
            .spawn()
            .unwrap();
        wait_for_checkpoint(temporary.path(), checkpoint);
        child.kill().unwrap();
        let status = child.wait().unwrap();
        assert!(!status.success());

        assert_invariants(temporary.path());
        assert_eq!(
            BuildCheckpointJournal::recover(temporary.path().join("build-journal")).unwrap(),
            Some(checkpoint)
        );
        assert!(
            !temporary
                .path()
                .join("build-journal/transaction.json")
                .exists()
        );
        assert_invariants(temporary.path());
    }
}

#[test]
fn build_crash_child() {
    let Some(root) = std::env::var_os("KEITH_BUILD_CHILD_ROOT") else {
        return;
    };
    let mut journal = BuildCheckpointJournal::open(
        PathBuf::from(root).join("build-journal"),
        "evolution-process-matrix",
        BASE,
        SOURCE_DIGEST,
    )
    .unwrap();
    for checkpoint in [
        BuildCheckpoint::ToolchainIdentified,
        BuildCheckpoint::GatesPassed,
        BuildCheckpoint::ArtifactRead,
        BuildCheckpoint::ImageSigned,
        BuildCheckpoint::Committed,
    ] {
        journal.checkpoint(checkpoint).unwrap();
    }
}

fn checkpoint_name(checkpoint: BuildCheckpoint) -> &'static str {
    match checkpoint {
        BuildCheckpoint::Prepared => "prepared",
        BuildCheckpoint::ToolchainIdentified => "toolchain_identified",
        BuildCheckpoint::GatesPassed => "gates_passed",
        BuildCheckpoint::ArtifactRead => "artifact_read",
        BuildCheckpoint::ImageSigned => "image_signed",
        BuildCheckpoint::Committed => "committed",
    }
}

fn wait_for_checkpoint(root: &Path, checkpoint: BuildCheckpoint) {
    let journal = root.join("build-journal/transaction.json");
    let needle = format!("\"checkpoint\":\"{}\"", checkpoint_name(checkpoint));
    let deadline = Instant::now() + Duration::from_secs(10);
    while Instant::now() < deadline {
        if fs::read_to_string(&journal).is_ok_and(|bytes| bytes.contains(&needle)) {
            return;
        }
        thread::sleep(Duration::from_millis(10));
    }
    panic!(
        "child did not durably reach {}",
        checkpoint_name(checkpoint)
    );
}

fn seed_invariants(root: &Path) {
    for (relative, bytes) in INVARIANTS {
        let path = root.join(relative);
        fs::create_dir_all(path.parent().unwrap()).unwrap();
        fs::write(path, bytes).unwrap();
    }
}

fn assert_invariants(root: &Path) {
    for (relative, bytes) in INVARIANTS {
        assert_eq!(fs::read(root.join(relative)).unwrap(), bytes, "{relative}");
    }
}
