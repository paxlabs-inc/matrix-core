#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::path::PathBuf;

use keith_provider_core::CancellationToken;
use keith_tool_runner_core::{
    IsolationRequest, ProcessLimits, RestrictedProcessRunner, RunRequest,
};

fn argument_value(arguments: &[String], name: &str) -> Result<String, String> {
    let index = arguments
        .iter()
        .position(|argument| argument == name)
        .ok_or_else(|| format!("{name} is required"))?;
    arguments
        .get(index.saturating_add(1))
        .cloned()
        .ok_or_else(|| format!("{name} requires a value"))
}

fn run() -> Result<(), String> {
    let arguments = std::env::args().skip(1).collect::<Vec<_>>();
    if arguments
        .iter()
        .any(|argument| matches!(argument.as_str(), "--version" | "-V"))
    {
        println!("{} {}", env!("CARGO_BIN_NAME"), env!("CARGO_PKG_VERSION"));
        return Ok(());
    }
    let workspace = PathBuf::from(argument_value(&arguments, "--workspace")?);
    let program = PathBuf::from(argument_value(&arguments, "--program")?);
    let passthrough = arguments
        .iter()
        .position(|argument| argument == "--")
        .map_or_else(Vec::new, |index| {
            arguments[index.saturating_add(1)..].to_vec()
        });
    let runner = RestrictedProcessRunner::new(
        &workspace,
        [program.clone()],
        BTreeSet::new(),
        BTreeMap::new(),
    )
    .map_err(|error| error.to_string())?;
    let mut chunks = Vec::new();
    let result = runner
        .run(
            &RunRequest {
                program,
                arguments: passthrough,
                working_directory: PathBuf::from("."),
                environment: BTreeMap::new(),
                isolation: if arguments.iter().any(|argument| argument == "--untrusted") {
                    IsolationRequest::UntrustedWorkspace
                } else {
                    IsolationRequest::TrustedWorkspace
                },
                limits: ProcessLimits::default(),
            },
            &CancellationToken::default(),
            &mut |chunk: &keith_tool_runner_core::OutputChunk| {
                chunks.push(serde_json::json!({
                    "stream": chunk.stream,
                    "bytes": chunk.bytes,
                }));
            },
        )
        .map_err(|error| error.to_string())?;
    println!(
        "{}",
        serde_json::json!({
            "exit_code": result.exit_code,
            "stdout": result.stdout,
            "stderr": result.stderr,
            "elapsed_ms": u64::try_from(result.elapsed.as_millis()).unwrap_or(u64::MAX),
            "sandbox": result.sandbox,
            "chunks": chunks,
        })
    );
    Ok(())
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
