#![forbid(unsafe_code)]

use std::fmt::Write as _;
use std::fs;
use std::io::Read;
use std::path::PathBuf;
use std::process::{Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::ArtifactId;
use keith_planner::Plan;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ArtifactRef {
    pub id: ArtifactId,
    pub path: PathBuf,
    pub media_type: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "check")]
pub enum CheckSpec {
    File {
        path: PathBuf,
        must_exist: bool,
        minimum_bytes: Option<u64>,
        sha256: Option<String>,
    },
    Command {
        program: PathBuf,
        arguments: Vec<String>,
        working_directory: PathBuf,
        timeout_ms: u64,
        expected_exit: i32,
        output_limit_bytes: usize,
    },
    Schema {
        value: serde_json::Value,
        schema: serde_json::Value,
    },
    Content {
        content: String,
        required: Vec<String>,
        forbidden: Vec<String>,
    },
    ExternalQuery {
        source: String,
        query: String,
        expected: String,
    },
    Reviewer,
    User {
        question: String,
        expected: String,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CheckStatus {
    Passed,
    Failed,
    NeedsUser,
    Error,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CheckResult {
    pub index: usize,
    pub kind: String,
    pub status: CheckStatus,
    pub detail: String,
    pub elapsed_ms: u64,
}

pub trait ExternalQueryRunner: Send + Sync {
    /// # Errors
    ///
    /// Returns an error when the named external source cannot answer the query.
    fn query(&self, source: &str, query: &str) -> Result<String, CheckError>;
}

impl<F> ExternalQueryRunner for F
where
    F: Fn(&str, &str) -> Result<String, CheckError> + Send + Sync,
{
    fn query(&self, source: &str, query: &str) -> Result<String, CheckError> {
        self(source, query)
    }
}

pub trait UserDecisionSource: Send + Sync {
    fn answer(&self, question: &str) -> Option<String>;
}

impl<F> UserDecisionSource for F
where
    F: Fn(&str) -> Option<String> + Send + Sync,
{
    fn answer(&self, question: &str) -> Option<String> {
        self(question)
    }
}

#[derive(Debug, Error)]
pub enum CheckError {
    #[error("check I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("external query failed: {0}")]
    External(String),
    #[error("check configuration is invalid: {0}")]
    Invalid(String),
}

pub struct DeterministicChecker<'a> {
    external: &'a dyn ExternalQueryRunner,
    user: &'a dyn UserDecisionSource,
}

impl<'a> DeterministicChecker<'a> {
    pub const fn new(
        external: &'a dyn ExternalQueryRunner,
        user: &'a dyn UserDecisionSource,
    ) -> Self {
        Self { external, user }
    }

    pub fn run(&self, checks: &[CheckSpec]) -> Vec<CheckResult> {
        checks
            .iter()
            .enumerate()
            .filter(|(_, check)| !matches!(check, CheckSpec::Reviewer))
            .map(|(index, check)| self.run_one(index, check))
            .collect()
    }

    fn run_one(&self, index: usize, check: &CheckSpec) -> CheckResult {
        let started = Instant::now();
        let kind = check_kind(check).to_owned();
        let result = match check {
            CheckSpec::File {
                path,
                must_exist,
                minimum_bytes,
                sha256,
            } => check_file(path, *must_exist, *minimum_bytes, sha256.as_deref()),
            CheckSpec::Command {
                program,
                arguments,
                working_directory,
                timeout_ms,
                expected_exit,
                output_limit_bytes,
            } => check_command(
                program,
                arguments,
                working_directory,
                *timeout_ms,
                *expected_exit,
                *output_limit_bytes,
            ),
            CheckSpec::Schema { value, schema } => validate_schema(schema, value)
                .map(|()| "schema matched".into())
                .map_err(CheckFailure::Failed),
            CheckSpec::Content {
                content,
                required,
                forbidden,
            } => check_content(content, required, forbidden),
            CheckSpec::ExternalQuery {
                source,
                query,
                expected,
            } => self
                .external
                .query(source, query)
                .map_err(|error| CheckFailure::Error(error.to_string()))
                .and_then(|actual| {
                    if actual == *expected {
                        Ok("external query matched".into())
                    } else {
                        Err(CheckFailure::Failed(format!(
                            "external value differed: {actual}"
                        )))
                    }
                }),
            CheckSpec::User { question, expected } => match self.user.answer(question) {
                Some(answer) if answer == *expected => Ok("user approved".into()),
                Some(answer) => Err(CheckFailure::Failed(format!("user answered {answer:?}"))),
                None => Err(CheckFailure::NeedsUser(question.clone())),
            },
            CheckSpec::Reviewer => unreachable!("reviewer checks are filtered"),
        };
        let (status, detail) = match result {
            Ok(detail) => (CheckStatus::Passed, detail),
            Err(CheckFailure::Failed(detail)) => (CheckStatus::Failed, detail),
            Err(CheckFailure::NeedsUser(detail)) => (CheckStatus::NeedsUser, detail),
            Err(CheckFailure::Error(detail)) => (CheckStatus::Error, detail),
        };
        CheckResult {
            index,
            kind,
            status,
            detail,
            elapsed_ms: elapsed_millis(started),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReviewSubject {
    pub original_request: String,
    pub plan: Option<Plan>,
    pub final_response: String,
    pub artifacts: Vec<ArtifactRef>,
    pub checks: Vec<CheckSpec>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReviewerInput {
    pub original_request: String,
    pub plan: Option<Plan>,
    pub final_response: String,
    pub artifacts: Vec<ArtifactRef>,
    pub check_results: Vec<CheckResult>,
    pub pass: u32,
    pub remaining_passes: u32,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "decision")]
pub enum ReviewDecision {
    Accept { rationale: String },
    Revise { instructions: String },
    AskUser { question: String },
    Stop { reason: String },
}

pub trait IndependentReviewer: Send + Sync {
    /// # Errors
    ///
    /// Returns an error when the independent review cannot complete.
    fn review(&self, input: &ReviewerInput) -> Result<ReviewDecision, ReviewError>;
}

impl<F> IndependentReviewer for F
where
    F: Fn(&ReviewerInput) -> Result<ReviewDecision, ReviewError> + Send + Sync,
{
    fn review(&self, input: &ReviewerInput) -> Result<ReviewDecision, ReviewError> {
        self(input)
    }
}

pub trait RevisionProducer: Send + Sync {
    /// # Errors
    ///
    /// Returns an error when the requested revision cannot be produced.
    fn revise(
        &self,
        subject: &ReviewSubject,
        instructions: &str,
    ) -> Result<ReviewSubject, ReviewError>;
}

impl<F> RevisionProducer for F
where
    F: Fn(&ReviewSubject, &str) -> Result<ReviewSubject, ReviewError> + Send + Sync,
{
    fn revise(
        &self,
        subject: &ReviewSubject,
        instructions: &str,
    ) -> Result<ReviewSubject, ReviewError> {
        self(subject, instructions)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ReviewConfig {
    pub max_passes: u32,
}

impl Default for ReviewConfig {
    fn default() -> Self {
        Self { max_passes: 2 }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ReviewRun {
    Accepted {
        subject: ReviewSubject,
        checks: Vec<CheckResult>,
        passes: u32,
        rationale: String,
    },
    RevisionBudgetExhausted {
        subject: ReviewSubject,
        checks: Vec<CheckResult>,
        passes: u32,
    },
    AskUser {
        question: String,
        subject: ReviewSubject,
        checks: Vec<CheckResult>,
        passes: u32,
    },
    Stopped {
        reason: String,
        subject: ReviewSubject,
        checks: Vec<CheckResult>,
        passes: u32,
    },
    DeterministicFailure {
        subject: ReviewSubject,
        checks: Vec<CheckResult>,
    },
}

#[derive(Debug, Error)]
pub enum ReviewError {
    #[error("reviewer failed: {0}")]
    Reviewer(String),
    #[error("revision failed: {0}")]
    Revision(String),
    #[error("review pass budget must be non-zero")]
    InvalidBudget,
}

pub struct ReviewEngine<'a> {
    checker: DeterministicChecker<'a>,
    reviewer: &'a dyn IndependentReviewer,
    reviser: &'a dyn RevisionProducer,
    config: ReviewConfig,
}

impl<'a> ReviewEngine<'a> {
    pub const fn new(
        checker: DeterministicChecker<'a>,
        reviewer: &'a dyn IndependentReviewer,
        reviser: &'a dyn RevisionProducer,
        config: ReviewConfig,
    ) -> Self {
        Self {
            checker,
            reviewer,
            reviser,
            config,
        }
    }

    /// # Errors
    ///
    /// Returns an error when the review budget is invalid or review/revision execution fails.
    pub fn run(&self, mut subject: ReviewSubject) -> Result<ReviewRun, ReviewError> {
        if self.config.max_passes == 0 {
            return Err(ReviewError::InvalidBudget);
        }
        let mut checks = self.checker.run(&subject.checks);
        if let Some(question) = needs_user(&checks) {
            return Ok(ReviewRun::AskUser {
                question,
                subject,
                checks,
                passes: 0,
            });
        }
        if has_failure(&checks) {
            return Ok(ReviewRun::DeterministicFailure { subject, checks });
        }
        if !subject
            .checks
            .iter()
            .any(|check| matches!(check, CheckSpec::Reviewer))
        {
            return Ok(ReviewRun::Accepted {
                subject,
                checks,
                passes: 0,
                rationale: "deterministic checks passed".into(),
            });
        }
        for pass in 1..=self.config.max_passes {
            let input = ReviewerInput {
                original_request: subject.original_request.clone(),
                plan: subject.plan.clone(),
                final_response: subject.final_response.clone(),
                artifacts: subject.artifacts.clone(),
                check_results: checks.clone(),
                pass,
                remaining_passes: self.config.max_passes - pass,
            };
            match self.reviewer.review(&input)? {
                ReviewDecision::Accept { rationale } => {
                    return Ok(ReviewRun::Accepted {
                        subject,
                        checks,
                        passes: pass,
                        rationale,
                    });
                }
                ReviewDecision::AskUser { question } => {
                    return Ok(ReviewRun::AskUser {
                        question,
                        subject,
                        checks,
                        passes: pass,
                    });
                }
                ReviewDecision::Stop { reason } => {
                    return Ok(ReviewRun::Stopped {
                        reason,
                        subject,
                        checks,
                        passes: pass,
                    });
                }
                ReviewDecision::Revise { instructions } => {
                    if pass == self.config.max_passes {
                        return Ok(ReviewRun::RevisionBudgetExhausted {
                            subject,
                            checks,
                            passes: pass,
                        });
                    }
                    subject = self.reviser.revise(&subject, &instructions)?;
                    checks = self.checker.run(&subject.checks);
                    if let Some(question) = needs_user(&checks) {
                        return Ok(ReviewRun::AskUser {
                            question,
                            subject,
                            checks,
                            passes: pass,
                        });
                    }
                    if has_failure(&checks) {
                        return Ok(ReviewRun::DeterministicFailure { subject, checks });
                    }
                }
            }
        }
        unreachable!("the configured pass range is non-empty")
    }
}

fn check_kind(check: &CheckSpec) -> &'static str {
    match check {
        CheckSpec::File { .. } => "file",
        CheckSpec::Command { .. } => "command",
        CheckSpec::Schema { .. } => "schema",
        CheckSpec::Content { .. } => "content",
        CheckSpec::ExternalQuery { .. } => "external_query",
        CheckSpec::Reviewer => "reviewer",
        CheckSpec::User { .. } => "user",
    }
}

enum CheckFailure {
    Failed(String),
    NeedsUser(String),
    Error(String),
}

fn check_file(
    path: &PathBuf,
    must_exist: bool,
    minimum_bytes: Option<u64>,
    expected_sha256: Option<&str>,
) -> Result<String, CheckFailure> {
    let metadata = match fs::metadata(path) {
        Ok(metadata) => metadata,
        Err(error) if error.kind() == std::io::ErrorKind::NotFound && !must_exist => {
            return Ok("file is absent as required".into());
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            return Err(CheckFailure::Failed("required file is absent".into()));
        }
        Err(error) => return Err(CheckFailure::Error(error.to_string())),
    };
    if !must_exist {
        return Err(CheckFailure::Failed(
            "file exists but must be absent".into(),
        ));
    }
    if minimum_bytes.is_some_and(|minimum| metadata.len() < minimum) {
        return Err(CheckFailure::Failed("file is smaller than required".into()));
    }
    if let Some(expected) = expected_sha256 {
        let bytes = fs::read(path).map_err(|error| CheckFailure::Error(error.to_string()))?;
        let actual = hex_digest(&bytes);
        if actual != expected {
            return Err(CheckFailure::Failed("file digest differs".into()));
        }
    }
    Ok("file check passed".into())
}

fn check_command(
    program: &PathBuf,
    arguments: &[String],
    working_directory: &PathBuf,
    timeout_ms: u64,
    expected_exit: i32,
    output_limit_bytes: usize,
) -> Result<String, CheckFailure> {
    if timeout_ms == 0 || output_limit_bytes == 0 || !working_directory.is_dir() {
        return Err(CheckFailure::Error(
            "invalid command check limits or directory".into(),
        ));
    }
    let mut child = Command::new(program)
        .args(arguments)
        .current_dir(working_directory)
        .stdin(Stdio::null())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|error| CheckFailure::Error(error.to_string()))?;
    let stdout = child
        .stdout
        .take()
        .ok_or_else(|| CheckFailure::Error("command stdout was not captured".into()))?;
    let stderr = child
        .stderr
        .take()
        .ok_or_else(|| CheckFailure::Error("command stderr was not captured".into()))?;
    let stdout_reader = thread::spawn(move || count_stream(stdout));
    let stderr_reader = thread::spawn(move || count_stream(stderr));
    let started = Instant::now();
    let status = loop {
        match child.try_wait() {
            Ok(Some(status)) => break status,
            Ok(None) if started.elapsed() >= Duration::from_millis(timeout_ms) => {
                let _ = child.kill();
                let _ = child.wait();
                let _ = stdout_reader.join();
                let _ = stderr_reader.join();
                return Err(CheckFailure::Failed("command timed out".into()));
            }
            Ok(None) => thread::sleep(Duration::from_millis(2)),
            Err(error) => return Err(CheckFailure::Error(error.to_string())),
        }
    };
    let stdout_bytes = stdout_reader
        .join()
        .map_err(|_| CheckFailure::Error("stdout reader panicked".into()))?
        .map_err(|error| CheckFailure::Error(error.to_string()))?;
    let stderr_bytes = stderr_reader
        .join()
        .map_err(|_| CheckFailure::Error("stderr reader panicked".into()))?
        .map_err(|error| CheckFailure::Error(error.to_string()))?;
    if stdout_bytes.saturating_add(stderr_bytes) > output_limit_bytes {
        return Err(CheckFailure::Failed(
            "command output exceeded its limit".into(),
        ));
    }
    if status.code() != Some(expected_exit) {
        return Err(CheckFailure::Failed(format!(
            "command exited with {:?}",
            status.code()
        )));
    }
    Ok("command check passed".into())
}

fn count_stream(mut stream: impl Read) -> Result<usize, std::io::Error> {
    let mut total = 0_usize;
    let mut buffer = [0_u8; 8 * 1_024];
    loop {
        let read = stream.read(&mut buffer)?;
        if read == 0 {
            return Ok(total);
        }
        total = total.saturating_add(read);
    }
}

fn check_content(
    content: &str,
    required: &[String],
    forbidden: &[String],
) -> Result<String, CheckFailure> {
    if let Some(missing) = required
        .iter()
        .find(|needle| !content.contains(needle.as_str()))
    {
        return Err(CheckFailure::Failed(format!(
            "required content is missing: {missing}"
        )));
    }
    if let Some(found) = forbidden
        .iter()
        .find(|needle| content.contains(needle.as_str()))
    {
        return Err(CheckFailure::Failed(format!(
            "forbidden content is present: {found}"
        )));
    }
    Ok("content check passed".into())
}

fn validate_schema(schema: &serde_json::Value, value: &serde_json::Value) -> Result<(), String> {
    if let Some(expected) = schema.get("type").and_then(serde_json::Value::as_str) {
        let matches = match expected {
            "object" => value.is_object(),
            "array" => value.is_array(),
            "string" => value.is_string(),
            "integer" => value.as_i64().is_some() || value.as_u64().is_some(),
            "number" => value.is_number(),
            "boolean" => value.is_boolean(),
            "null" => value.is_null(),
            _ => return Err(format!("unsupported schema type {expected}")),
        };
        if !matches {
            return Err(format!("expected {expected}"));
        }
    }
    if let (Some(required), Some(object)) = (
        schema.get("required").and_then(serde_json::Value::as_array),
        value.as_object(),
    ) {
        for field in required {
            let field = field
                .as_str()
                .ok_or_else(|| "required fields must be strings".to_owned())?;
            if !object.contains_key(field) {
                return Err(format!("missing required field {field}"));
            }
        }
    }
    Ok(())
}

fn hex_digest(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    let mut encoded = String::with_capacity(digest.len() * 2);
    for byte in digest {
        write!(&mut encoded, "{byte:02x}").expect("writing to a string cannot fail");
    }
    encoded
}

fn has_failure(checks: &[CheckResult]) -> bool {
    checks
        .iter()
        .any(|result| matches!(result.status, CheckStatus::Failed | CheckStatus::Error))
}

fn needs_user(checks: &[CheckResult]) -> Option<String> {
    checks
        .iter()
        .find(|result| result.status == CheckStatus::NeedsUser)
        .map(|result| result.detail.clone())
}

fn elapsed_millis(started: Instant) -> u64 {
    u64::try_from(started.elapsed().as_millis()).unwrap_or(u64::MAX)
}

#[cfg(test)]
mod tests {
    use std::collections::VecDeque;
    use std::sync::{Arc, Mutex};

    use serde_json::json;

    use super::*;

    fn external(source: &str, query: &str) -> Result<String, CheckError> {
        if source == "local-catalog" && query == "version" {
            Ok("1.0".into())
        } else {
            Err(CheckError::External("unknown query".into()))
        }
    }

    fn no_user(_question: &str) -> Option<String> {
        None
    }

    fn subject(checks: Vec<CheckSpec>) -> ReviewSubject {
        ReviewSubject {
            original_request: "Produce a checked answer".into(),
            plan: None,
            final_response: "draft answer".into(),
            artifacts: Vec::new(),
            checks,
        }
    }

    #[test]
    fn file_command_schema_content_and_external_checks_run_in_order() {
        let directory = tempfile::tempdir().unwrap();
        let file = directory.path().join("artifact.txt");
        fs::write(&file, b"verified content").unwrap();
        let checks = vec![
            CheckSpec::File {
                path: file,
                must_exist: true,
                minimum_bytes: Some(8),
                sha256: Some(hex_digest(b"verified content")),
            },
            CheckSpec::Command {
                program: PathBuf::from("/bin/sh"),
                arguments: vec!["-c".into(), "printf checked".into()],
                working_directory: directory.path().into(),
                timeout_ms: 1_000,
                expected_exit: 0,
                output_limit_bytes: 64,
            },
            CheckSpec::Schema {
                value: json!({"answer": "yes"}),
                schema: json!({"type": "object", "required": ["answer"]}),
            },
            CheckSpec::Content {
                content: "safe verified answer".into(),
                required: vec!["verified".into()],
                forbidden: vec!["secret".into()],
            },
            CheckSpec::ExternalQuery {
                source: "local-catalog".into(),
                query: "version".into(),
                expected: "1.0".into(),
            },
        ];
        let checker = DeterministicChecker::new(&external, &no_user);
        let results = checker.run(&checks);
        assert_eq!(results.len(), checks.len());
        assert!(
            results
                .iter()
                .all(|result| result.status == CheckStatus::Passed)
        );
        assert_eq!(
            results
                .iter()
                .map(|result| result.index)
                .collect::<Vec<_>>(),
            vec![0, 1, 2, 3, 4]
        );
    }

    #[test]
    fn deterministic_failure_prevents_a_false_positive_model_acceptance() {
        let review_calls = Arc::new(Mutex::new(0_u32));
        let calls = Arc::clone(&review_calls);
        let reviewer = move |_: &ReviewerInput| {
            *calls.lock().unwrap() += 1;
            Ok(ReviewDecision::Accept {
                rationale: "looks fine".into(),
            })
        };
        let reviser = |subject: &ReviewSubject, _: &str| Ok(subject.clone());
        let checker = DeterministicChecker::new(&external, &no_user);
        let engine = ReviewEngine::new(checker, &reviewer, &reviser, ReviewConfig::default());
        let result = engine
            .run(subject(vec![
                CheckSpec::Content {
                    content: "incomplete".into(),
                    required: vec!["verified".into()],
                    forbidden: Vec::new(),
                },
                CheckSpec::Reviewer,
            ]))
            .unwrap();
        assert!(matches!(result, ReviewRun::DeterministicFailure { .. }));
        assert_eq!(*review_calls.lock().unwrap(), 0);
    }

    struct QueueReviewer {
        decisions: Mutex<VecDeque<ReviewDecision>>,
        inputs: Arc<Mutex<Vec<ReviewerInput>>>,
    }

    impl IndependentReviewer for QueueReviewer {
        fn review(&self, input: &ReviewerInput) -> Result<ReviewDecision, ReviewError> {
            self.inputs.lock().unwrap().push(input.clone());
            self.decisions
                .lock()
                .unwrap()
                .pop_front()
                .ok_or_else(|| ReviewError::Reviewer("decision queue exhausted".into()))
        }
    }

    #[test]
    fn false_positive_revision_is_bounded_and_reviewer_receives_complete_context() {
        let inputs = Arc::new(Mutex::new(Vec::new()));
        let reviewer = QueueReviewer {
            decisions: Mutex::new(
                vec![
                    ReviewDecision::Revise {
                        instructions: "clarify the answer".into(),
                    },
                    ReviewDecision::Accept {
                        rationale: "clear after inspection".into(),
                    },
                ]
                .into(),
            ),
            inputs: Arc::clone(&inputs),
        };
        let reviser = |subject: &ReviewSubject, instructions: &str| {
            let mut revised = subject.clone();
            revised.final_response = format!("{}; {instructions}", revised.final_response);
            Ok(revised)
        };
        let checker = DeterministicChecker::new(&external, &no_user);
        let engine =
            ReviewEngine::new(checker, &reviewer, &reviser, ReviewConfig { max_passes: 2 });
        let mut review_subject = subject(vec![CheckSpec::Reviewer]);
        let directory = tempfile::tempdir().unwrap();
        let artifact_path = directory.path().join("review.txt");
        fs::write(&artifact_path, b"artifact").unwrap();
        review_subject.artifacts.push(ArtifactRef {
            id: ArtifactId::new(),
            path: artifact_path,
            media_type: "text/plain".into(),
        });
        let result = engine.run(review_subject).unwrap();
        assert!(matches!(result, ReviewRun::Accepted { passes: 2, .. }));
        let captured = inputs.lock().unwrap();
        assert_eq!(captured.len(), 2);
        assert_eq!(captured[0].original_request, "Produce a checked answer");
        assert_eq!(captured[0].artifacts.len(), 1);
        assert_eq!(captured[0].pass, 1);
        assert_eq!(captured[1].remaining_passes, 0);
        assert!(captured[1].final_response.contains("clarify"));
    }

    #[test]
    fn user_decisions_and_reviewer_ask_user_are_explicit() {
        let checker = DeterministicChecker::new(&external, &no_user);
        let reviewer = |_: &ReviewerInput| {
            Ok(ReviewDecision::Accept {
                rationale: "unused".into(),
            })
        };
        let reviser = |subject: &ReviewSubject, _: &str| Ok(subject.clone());
        let engine = ReviewEngine::new(checker, &reviewer, &reviser, ReviewConfig::default());
        assert!(matches!(
            engine
                .run(subject(vec![CheckSpec::User {
                    question: "Publish?".into(),
                    expected: "yes".into()
                }]))
                .unwrap(),
            ReviewRun::AskUser { passes: 0, .. }
        ));

        let ask = |_: &ReviewerInput| {
            Ok(ReviewDecision::AskUser {
                question: "Choose format".into(),
            })
        };
        let checker = DeterministicChecker::new(&external, &no_user);
        let engine = ReviewEngine::new(checker, &ask, &reviser, ReviewConfig::default());
        assert!(matches!(
            engine.run(subject(vec![CheckSpec::Reviewer])).unwrap(),
            ReviewRun::AskUser { passes: 1, .. }
        ));
    }

    #[test]
    fn repeated_revision_stops_at_the_review_budget_and_stop_is_terminal() {
        let revise = |_: &ReviewerInput| {
            Ok(ReviewDecision::Revise {
                instructions: "revise again".into(),
            })
        };
        let reviser = |subject: &ReviewSubject, _: &str| Ok(subject.clone());
        let checker = DeterministicChecker::new(&external, &no_user);
        let engine = ReviewEngine::new(checker, &revise, &reviser, ReviewConfig { max_passes: 1 });
        assert!(matches!(
            engine.run(subject(vec![CheckSpec::Reviewer])).unwrap(),
            ReviewRun::RevisionBudgetExhausted { passes: 1, .. }
        ));

        let stop = |_: &ReviewerInput| {
            Ok(ReviewDecision::Stop {
                reason: "unsafe result".into(),
            })
        };
        let checker = DeterministicChecker::new(&external, &no_user);
        let engine = ReviewEngine::new(checker, &stop, &reviser, ReviewConfig::default());
        assert!(matches!(
            engine.run(subject(vec![CheckSpec::Reviewer])).unwrap(),
            ReviewRun::Stopped { passes: 1, .. }
        ));
    }
}
