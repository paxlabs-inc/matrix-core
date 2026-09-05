//! Cross-domain qualification of the operator corpus boundary, not candidate quality.

use std::collections::{BTreeMap, BTreeSet};
use std::fs;
use std::path::{Path, PathBuf};
use std::time::Duration;

use keith_meta_harness::{
    CandidateError, CandidatePolicy, EvaluationCase, EvaluationDataset, IndependentEvaluator,
    RedactedText, RegressionBounds,
};
use keith_provider_core::CancellationToken;
use keith_tool_runner_core::{
    IsolationRequest, OutputChunk, ProcessLimits, RestrictedProcessRunner, RunError, RunRequest,
};
use serde_json::{Value, json};

const OPERATOR_ROOT: &str = "/root/keith-evaluation/causal-intelligence/v1";

fn corpus_root() -> PathBuf {
    Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../../tests/causal-intelligence/corpus")
        .canonicalize()
        .expect("corpus root")
}

fn load(path: &Path) -> Value {
    serde_json::from_slice(&fs::read(path).expect("operator corpus must be restored separately"))
        .expect("versioned corpus JSON")
}

fn text(value: &str) -> RedactedText {
    RedactedText::parse(value).expect("synthetic bounded identifier")
}

fn dataset(path: &Path) -> EvaluationDataset {
    let source = load(path);
    let cases = source["records"]
        .as_array()
        .expect("case records")
        .iter()
        .map(|row| {
            let id = row["id"].as_str().expect("case ID");
            EvaluationCase::new(
                text(id),
                row["query"].as_str().expect("query").as_bytes().to_vec(),
                serde_json::to_vec(&row["gold_anchor_ids"]).expect("expected IDs"),
                format!("operator-private-canary-{id}").into_bytes(),
                false,
            )
            .expect("independent evaluator case")
        })
        .collect();
    EvaluationDataset::new(text(source["version"].as_str().expect("version")), cases)
        .expect("disjoint dataset")
}

#[test]
fn private_corpus_uses_existing_independent_evaluator_projection() {
    let search = dataset(&corpus_root().join("development.json"));
    let validation = dataset(&Path::new(OPERATOR_ROOT).join("validation.json"));
    let held_out = dataset(&Path::new(OPERATOR_ROOT).join("semantic-held-out.json"));
    assert_eq!(search.len(), 8);
    assert_eq!(held_out.len(), 200);
    let evaluator = IndependentEvaluator::new(
        search.clone(),
        validation.clone(),
        held_out,
        RegressionBounds::default(),
        OPERATOR_ROOT,
    )
    .expect("operator-owned evaluator");
    let view = evaluator.proposer_view();
    assert_eq!(view.search_case_count, 8);
    assert_eq!(view.validation_case_count, 1);
    let rendered = format!("{view:?}");
    assert!(!rendered.contains("semantic-held-out"));
    for row in load(&Path::new(OPERATOR_ROOT).join("semantic-held-out.json"))["records"]
        .as_array()
        .expect("held-out records")
    {
        assert!(!rendered.contains(row["query"].as_str().expect("query")));
        assert!(!rendered.contains(row["anchor_id"].as_str().expect("anchor")));
    }
    assert!(matches!(
        IndependentEvaluator::new(
            search.clone(),
            validation,
            search,
            RegressionBounds::default(),
            OPERATOR_ROOT,
        ),
        Err(CandidateError::EvaluationVersionsNotSeparated)
    ));
}

#[test]
fn candidate_export_contains_only_explicit_development_inputs() {
    let source = corpus_root();
    let manifest = load(&source.join("candidate-inputs.json"));
    let destination = tempfile::tempdir().expect("isolated candidate export");
    let files = manifest["allowed_files"].as_array().expect("allowlist");
    assert_eq!(files, &[json!("development.json")]);
    assert_eq!(manifest["external_fixture_access"], false);
    for value in files {
        let relative = value.as_str().expect("relative input");
        let path = source
            .join(relative)
            .canonicalize()
            .expect("development input");
        assert!(path.starts_with(&source));
        fs::copy(path, destination.path().join(relative)).expect("bounded development export");
    }
    assert_eq!(fs::read_dir(destination.path()).expect("export").count(), 1);
    assert!(CandidatePolicy::new([PathBuf::from("evaluator")]).is_err());
    assert!(CandidatePolicy::new([PathBuf::from("held-out")]).is_err());
    assert!(!destination.path().join("semantic-held-out.json").exists());
}

#[test]
fn restricted_candidate_cannot_read_operator_bundle() {
    let workspace = tempfile::tempdir().expect("candidate workspace");
    fs::copy(
        corpus_root().join("development.json"),
        workspace.path().join("development.json"),
    )
    .expect("development-only input");
    let private = Path::new(OPERATOR_ROOT).join("semantic-held-out.json");
    assert!(
        private.is_file(),
        "operator bundle is required, not an optional skipped proof"
    );
    #[cfg(unix)]
    std::os::unix::fs::symlink(&private, workspace.path().join("linked-private.json"))
        .expect("hostile symlink");
    let script = r"import json, pathlib, sys
assert json.loads(pathlib.Path('development.json').read_text())['records']
for target in sys.argv[1:]:
    try:
        pathlib.Path(target).read_bytes()
    except OSError:
        continue
    sys.exit(41)
print('all_private_reads_denied')
";
    let program = PathBuf::from("/usr/bin/python3");
    let runner = RestrictedProcessRunner::new(
        workspace.path(),
        [program.clone()],
        BTreeSet::new(),
        BTreeMap::new(),
    )
    .expect("existing restricted runner");
    let request = RunRequest {
        program,
        arguments: vec![
            "-c".into(),
            script.into(),
            private.display().to_string(),
            "linked-private.json".into(),
            format!("/proc/1/root{}", private.display()),
            format!("/proc/self/root{}", private.display()),
        ],
        working_directory: PathBuf::from("."),
        environment: BTreeMap::new(),
        isolation: IsolationRequest::UntrustedWorkspace,
        limits: ProcessLimits {
            timeout: Duration::from_secs(15),
            cancellation_grace: Duration::from_millis(100),
            output_bytes: 4096,
            cpu_seconds: Some(10),
            memory_bytes: Some(256 * 1024 * 1024),
            deny_network: true,
        },
    };
    let mut output = Vec::new();
    let mut sink = |chunk: &OutputChunk| output.extend_from_slice(&chunk.bytes);
    let result = runner.run(&request, &CancellationToken::default(), &mut sink);
    let executed = runner.sandbox_status().supports_untrusted();
    if executed {
        assert!(
            result.is_ok(),
            "strong sandbox must execute the bounded read-denial probe"
        );
        assert!(
            output == b"all_private_reads_denied\n",
            "private read isolation failed"
        );
    } else {
        assert!(matches!(result, Err(RunError::StrongIsolationUnavailable)));
        assert!(output.is_empty());
    }
    if let Some(directory) = std::env::var_os("KEITH_QUALIFICATION_ARTIFACT_DIR") {
        let report = json!({
            "run_id": std::env::var("KEITH_QUALIFICATION_RUN_ID").expect("run ID"),
            "case_id": std::env::var("KEITH_QUALIFICATION_CASE_ID").expect("case ID"),
            "source_digest": std::env::var("KEITH_QUALIFICATION_SOURCE_DIGEST").expect("source identity"),
            "strong_sandbox_executed": executed,
            "execution_refused_when_unavailable": !executed,
            "private_contents_exported": false,
            "scope": "evaluator isolation infrastructure, not Keith causal runtime qualification"
        });
        fs::write(
            Path::new(&directory).join("boundary.json"),
            serde_json::to_vec_pretty(&report).expect("bounded report"),
        )
        .expect("boundary evidence");
    }
}
