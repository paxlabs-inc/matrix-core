#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::ffi::OsString;
use std::fmt::Write as _;
use std::io::{Read, Write};
use std::path::{Component, Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::{Mutex, MutexGuard, mpsc};
use std::thread;
use std::time::{Duration, Instant};

use cap_fs_ext::{FollowSymlinks, OpenOptionsFollowExt};
use cap_std::ambient_authority;
use cap_std::fs::{Dir, OpenOptions};
use keith_agent_types::EntityId;
use keith_provider_core::CancellationToken;
use keith_sandbox::{IsolationLevel, SandboxStatus};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct WorkspaceLimits {
    pub max_file_bytes: usize,
    pub max_directory_entries: usize,
    pub max_search_files: usize,
    pub max_search_bytes: usize,
    pub max_search_results: usize,
}

impl Default for WorkspaceLimits {
    fn default() -> Self {
        Self {
            max_file_bytes: 16 * 1_024 * 1_024,
            max_directory_entries: 10_000,
            max_search_files: 10_000,
            max_search_bytes: 128 * 1_024 * 1_024,
            max_search_results: 10_000,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ExpectedPreimage {
    Any,
    Missing,
    Sha256(String),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChangeKind {
    Written,
    Renamed,
    Copied,
    Deleted,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChangeSummary {
    pub kind: ChangeKind,
    pub path: PathBuf,
    pub source: Option<PathBuf>,
    pub before_sha256: Option<String>,
    pub after_sha256: Option<String>,
    pub bytes: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DirectoryEntry {
    pub name: OsString,
    pub is_file: bool,
    pub is_directory: bool,
    pub bytes: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SearchMatch {
    pub path: PathBuf,
    pub line: usize,
    pub text: String,
}

#[derive(Debug, Error)]
pub enum WorkspaceError {
    #[error("workspace I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("path is not a safe workspace-relative path")]
    UnsafePath,
    #[error("symbolic links are not accepted by workspace tools")]
    Symlink,
    #[error("file or operation exceeds its configured size bound")]
    LimitExceeded,
    #[error("expected preimage does not match the current file")]
    PreimageMismatch,
    #[error("operation was cancelled")]
    Cancelled,
    #[error("workspace operation lock was poisoned")]
    LockPoisoned,
    #[error("file content is not valid UTF-8")]
    NonUtf8,
}

pub struct WorkspaceFs {
    root: Dir,
    ambient_root: PathBuf,
    limits: WorkspaceLimits,
    mutation_lock: Mutex<()>,
}

impl WorkspaceFs {
    /// # Errors
    ///
    /// Returns an error when the workspace root does not resolve to an accessible directory.
    pub fn open(root: impl AsRef<Path>, limits: WorkspaceLimits) -> Result<Self, WorkspaceError> {
        let ambient_root = std::fs::canonicalize(root.as_ref())?;
        if !ambient_root.is_dir() {
            return Err(WorkspaceError::UnsafePath);
        }
        let root = Dir::open_ambient_dir(&ambient_root, ambient_authority())?;
        Ok(Self {
            root,
            ambient_root,
            limits,
            mutation_lock: Mutex::new(()),
        })
    }

    pub fn ambient_root(&self) -> &Path {
        &self.ambient_root
    }

    /// # Errors
    ///
    /// Returns an error for unsafe paths, symlinks, inaccessible entries, or entry overflow.
    pub fn list(&self, path: impl AsRef<Path>) -> Result<Vec<DirectoryEntry>, WorkspaceError> {
        let path = safe_relative(path.as_ref())?;
        self.ensure_no_symlinks(&path, false)?;
        let mut entries = Vec::new();
        for entry in self.root.read_dir(&path)? {
            if entries.len() >= self.limits.max_directory_entries {
                return Err(WorkspaceError::LimitExceeded);
            }
            let entry = entry?;
            let file_type = entry.file_type()?;
            if file_type.is_symlink() {
                continue;
            }
            let metadata = entry.metadata()?;
            entries.push(DirectoryEntry {
                name: entry.file_name(),
                is_file: file_type.is_file(),
                is_directory: file_type.is_dir(),
                bytes: metadata.len(),
            });
        }
        entries.sort_by(|left, right| left.name.cmp(&right.name));
        Ok(entries)
    }

    /// # Errors
    ///
    /// Returns an error for unsafe paths, symlinks, cancellation, I/O, or file-size overflow.
    pub fn read(
        &self,
        path: impl AsRef<Path>,
        cancellation: &CancellationToken,
    ) -> Result<Vec<u8>, WorkspaceError> {
        cancellation_check(cancellation)?;
        let path = safe_relative(path.as_ref())?;
        self.ensure_no_symlinks(&path, true)?;
        let mut options = OpenOptions::new();
        options.read(true).follow(FollowSymlinks::No);
        let mut file = self.root.open_with(&path, &options)?;
        let length =
            usize::try_from(file.metadata()?.len()).map_err(|_| WorkspaceError::LimitExceeded)?;
        if length > self.limits.max_file_bytes {
            return Err(WorkspaceError::LimitExceeded);
        }
        let mut bytes = Vec::with_capacity(length);
        let mut buffer = [0_u8; 16 * 1_024];
        loop {
            cancellation_check(cancellation)?;
            let read = file.read(&mut buffer)?;
            if read == 0 {
                break;
            }
            if bytes.len().saturating_add(read) > self.limits.max_file_bytes {
                return Err(WorkspaceError::LimitExceeded);
            }
            bytes.extend_from_slice(&buffer[..read]);
        }
        Ok(bytes)
    }

    /// # Errors
    ///
    /// Returns an error for unsafe paths, symlinks, invalid UTF-8, cancellation, or search bounds.
    pub fn search(
        &self,
        path: impl AsRef<Path>,
        needle: &str,
        cancellation: &CancellationToken,
    ) -> Result<Vec<SearchMatch>, WorkspaceError> {
        if needle.is_empty() {
            return Err(WorkspaceError::UnsafePath);
        }
        let path = safe_relative(path.as_ref())?;
        self.ensure_no_symlinks(&path, false)?;
        let mut files = Vec::new();
        self.collect_files(&path, &mut files, cancellation)?;
        let mut searched_bytes = 0_usize;
        let mut matches = Vec::new();
        for file in files {
            let bytes = self.read(&file, cancellation)?;
            searched_bytes = searched_bytes.saturating_add(bytes.len());
            if searched_bytes > self.limits.max_search_bytes {
                return Err(WorkspaceError::LimitExceeded);
            }
            let content = std::str::from_utf8(&bytes).map_err(|_| WorkspaceError::NonUtf8)?;
            for (index, line) in content.lines().enumerate() {
                if line.contains(needle) {
                    if matches.len() >= self.limits.max_search_results {
                        return Err(WorkspaceError::LimitExceeded);
                    }
                    matches.push(SearchMatch {
                        path: file.clone(),
                        line: index + 1,
                        text: line.to_owned(),
                    });
                }
            }
        }
        Ok(matches)
    }

    /// # Errors
    ///
    /// Returns an error when validation, expected-preimage comparison, or atomic persistence fails.
    pub fn write_atomic(
        &self,
        path: impl AsRef<Path>,
        bytes: &[u8],
        expected: &ExpectedPreimage,
        cancellation: &CancellationToken,
    ) -> Result<ChangeSummary, WorkspaceError> {
        if bytes.len() > self.limits.max_file_bytes {
            return Err(WorkspaceError::LimitExceeded);
        }
        cancellation_check(cancellation)?;
        let path = safe_relative(path.as_ref())?;
        let _guard = self.lock_mutation()?;
        self.ensure_no_symlinks(&path, false)?;
        let before = self.preimage(&path)?;
        validate_preimage(expected, before.as_deref())?;
        let parent = path.parent().unwrap_or_else(|| Path::new(""));
        if !parent.as_os_str().is_empty() {
            self.root.create_dir_all(parent)?;
            self.ensure_no_symlinks(parent, true)?;
        }
        let temporary = parent.join(format!(".keith-write-{}.tmp", EntityId::new()));
        let mut options = OpenOptions::new();
        options
            .write(true)
            .create_new(true)
            .follow(FollowSymlinks::No);
        let write_result = (|| {
            let mut file = self.root.open_with(&temporary, &options)?;
            for chunk in bytes.chunks(16 * 1_024) {
                cancellation_check(cancellation)?;
                file.write_all(chunk)?;
            }
            file.sync_all()?;
            self.ensure_no_symlinks(&path, false)?;
            let current = self.preimage(&path)?;
            if current != before {
                return Err(WorkspaceError::PreimageMismatch);
            }
            self.root.rename(&temporary, &self.root, &path)?;
            self.sync_directory(parent)?;
            Ok(())
        })();
        if write_result.is_err() {
            let _ = self.root.remove_file(&temporary);
        }
        write_result?;
        Ok(ChangeSummary {
            kind: ChangeKind::Written,
            path,
            source: None,
            before_sha256: before,
            after_sha256: Some(hex_digest(bytes)),
            bytes: u64::try_from(bytes.len()).map_err(|_| WorkspaceError::LimitExceeded)?,
        })
    }

    /// # Errors
    ///
    /// Returns an error when the expected source content differs or the atomic write fails.
    pub fn edit(
        &self,
        path: impl AsRef<Path>,
        expected_bytes: &[u8],
        replacement: &[u8],
        cancellation: &CancellationToken,
    ) -> Result<ChangeSummary, WorkspaceError> {
        self.write_atomic(
            path,
            replacement,
            &ExpectedPreimage::Sha256(hex_digest(expected_bytes)),
            cancellation,
        )
    }

    /// # Errors
    ///
    /// Returns an error for unsafe paths, symlinks, target conflicts, or persistence failure.
    pub fn rename(
        &self,
        from: impl AsRef<Path>,
        to: impl AsRef<Path>,
    ) -> Result<ChangeSummary, WorkspaceError> {
        let from = safe_relative(from.as_ref())?;
        let to = safe_relative(to.as_ref())?;
        let _guard = self.lock_mutation()?;
        self.ensure_no_symlinks(&from, true)?;
        self.ensure_no_symlinks(&to, false)?;
        if self.root.try_exists(&to)? {
            return Err(WorkspaceError::PreimageMismatch);
        }
        let metadata = self.root.symlink_metadata(&from)?;
        let before = if metadata.is_dir() {
            None
        } else {
            self.preimage(&from)?
        };
        self.root.rename(&from, &self.root, &to)?;
        self.sync_directory(to.parent().unwrap_or_else(|| Path::new("")))?;
        Ok(ChangeSummary {
            kind: ChangeKind::Renamed,
            path: to,
            source: Some(from),
            before_sha256: before.clone(),
            after_sha256: before,
            bytes: metadata.len(),
        })
    }

    /// # Errors
    ///
    /// Returns an error for unsafe paths, symlinks, target conflicts, or copy limits.
    pub fn copy(
        &self,
        from: impl AsRef<Path>,
        to: impl AsRef<Path>,
    ) -> Result<ChangeSummary, WorkspaceError> {
        let from = safe_relative(from.as_ref())?;
        let to = safe_relative(to.as_ref())?;
        let _guard = self.lock_mutation()?;
        self.ensure_no_symlinks(&from, true)?;
        self.ensure_no_symlinks(&to, false)?;
        if self.root.try_exists(&to)? {
            return Err(WorkspaceError::PreimageMismatch);
        }
        let metadata = self.root.metadata(&from)?;
        if usize::try_from(metadata.len()).map_or(true, |size| size > self.limits.max_file_bytes) {
            return Err(WorkspaceError::LimitExceeded);
        }
        let bytes = self.root.copy(&from, &self.root, &to)?;
        let digest = self.preimage(&to)?;
        Ok(ChangeSummary {
            kind: ChangeKind::Copied,
            path: to,
            source: Some(from),
            before_sha256: None,
            after_sha256: digest,
            bytes,
        })
    }

    /// # Errors
    ///
    /// Returns an error for unsafe paths, symlinks, preimage mismatch, or deletion failure.
    pub fn delete(
        &self,
        path: impl AsRef<Path>,
        expected: &ExpectedPreimage,
    ) -> Result<ChangeSummary, WorkspaceError> {
        let path = safe_relative(path.as_ref())?;
        let _guard = self.lock_mutation()?;
        self.ensure_no_symlinks(&path, true)?;
        let metadata = self.root.symlink_metadata(&path)?;
        let before = if metadata.is_dir() {
            if expected != &ExpectedPreimage::Any {
                return Err(WorkspaceError::PreimageMismatch);
            }
            None
        } else {
            let before = self.preimage(&path)?;
            validate_preimage(expected, before.as_deref())?;
            before
        };
        if metadata.is_dir() {
            self.root.remove_dir(&path)?;
        } else {
            self.root.remove_file(&path)?;
        }
        self.sync_directory(path.parent().unwrap_or_else(|| Path::new("")))?;
        Ok(ChangeSummary {
            kind: ChangeKind::Deleted,
            path,
            source: None,
            before_sha256: before,
            after_sha256: None,
            bytes: metadata.len(),
        })
    }

    fn collect_files(
        &self,
        path: &Path,
        files: &mut Vec<PathBuf>,
        cancellation: &CancellationToken,
    ) -> Result<(), WorkspaceError> {
        cancellation_check(cancellation)?;
        for entry in self.root.read_dir(path)? {
            cancellation_check(cancellation)?;
            let entry = entry?;
            let file_type = entry.file_type()?;
            if file_type.is_symlink() {
                continue;
            }
            let child = path.join(entry.file_name());
            if file_type.is_dir() {
                self.collect_files(&child, files, cancellation)?;
            } else if file_type.is_file() {
                if files.len() >= self.limits.max_search_files {
                    return Err(WorkspaceError::LimitExceeded);
                }
                files.push(child);
            }
        }
        Ok(())
    }

    fn preimage(&self, path: &Path) -> Result<Option<String>, WorkspaceError> {
        match self.root.symlink_metadata(path) {
            Ok(metadata) if metadata.file_type().is_symlink() => Err(WorkspaceError::Symlink),
            Ok(metadata) if metadata.is_file() => {
                if usize::try_from(metadata.len())
                    .map_or(true, |size| size > self.limits.max_file_bytes)
                {
                    return Err(WorkspaceError::LimitExceeded);
                }
                Ok(Some(hex_digest(&self.root.read(path)?)))
            }
            Ok(_) => Err(WorkspaceError::UnsafePath),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(error.into()),
        }
    }

    fn ensure_no_symlinks(&self, path: &Path, require_final: bool) -> Result<(), WorkspaceError> {
        let components = path.components().collect::<Vec<_>>();
        let mut current = PathBuf::new();
        for (index, component) in components.iter().enumerate() {
            let Component::Normal(name) = component else {
                continue;
            };
            current.push(name);
            match self.root.symlink_metadata(&current) {
                Ok(metadata) if metadata.file_type().is_symlink() => {
                    return Err(WorkspaceError::Symlink);
                }
                Ok(_) => {}
                Err(error)
                    if error.kind() == std::io::ErrorKind::NotFound
                        && (!require_final || index + 1 == components.len()) =>
                {
                    break;
                }
                Err(error) => return Err(error.into()),
            }
        }
        Ok(())
    }

    fn lock_mutation(&self) -> Result<MutexGuard<'_, ()>, WorkspaceError> {
        self.mutation_lock
            .lock()
            .map_err(|_| WorkspaceError::LockPoisoned)
    }

    fn sync_directory(&self, path: &Path) -> Result<(), WorkspaceError> {
        std::fs::File::open(self.ambient_root.join(path))?.sync_all()?;
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum IsolationRequest {
    TrustedWorkspace,
    UntrustedWorkspace,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProcessLimits {
    pub timeout: Duration,
    pub cancellation_grace: Duration,
    pub output_bytes: usize,
    pub cpu_seconds: Option<u64>,
    pub memory_bytes: Option<u64>,
    pub deny_network: bool,
}

impl Default for ProcessLimits {
    fn default() -> Self {
        Self {
            timeout: Duration::from_secs(60),
            cancellation_grace: Duration::from_millis(250),
            output_bytes: 4 * 1_024 * 1_024,
            cpu_seconds: None,
            memory_bytes: None,
            deny_network: false,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RunRequest {
    pub program: PathBuf,
    pub arguments: Vec<String>,
    pub working_directory: PathBuf,
    pub environment: BTreeMap<String, String>,
    pub isolation: IsolationRequest,
    pub limits: ProcessLimits,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum OutputStream {
    Stdout,
    Stderr,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct OutputChunk {
    pub stream: OutputStream,
    pub bytes: Vec<u8>,
}

pub trait OutputSink {
    fn emit(&mut self, chunk: &OutputChunk);
}

impl<F> OutputSink for F
where
    F: FnMut(&OutputChunk),
{
    fn emit(&mut self, chunk: &OutputChunk) {
        self(chunk);
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RunResult {
    pub exit_code: Option<i32>,
    pub stdout: Vec<u8>,
    pub stderr: Vec<u8>,
    pub elapsed: Duration,
    pub sandbox: SandboxStatus,
}

#[derive(Debug, Error)]
pub enum RunError {
    #[error("process runner I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("program is not on the installation allowlist")]
    ProgramDenied,
    #[error("working directory is unsafe")]
    WorkingDirectory,
    #[error("environment key is not permitted")]
    EnvironmentDenied,
    #[error("process limits are invalid or unavailable: {0}")]
    LimitUnavailable(String),
    #[error("strong isolation is unavailable for an untrusted workspace")]
    StrongIsolationUnavailable,
    #[error("process timed out")]
    Timeout,
    #[error("process output exceeded its configured limit")]
    OutputLimit,
    #[error("process was cancelled")]
    Cancelled,
    #[error("output worker disconnected")]
    OutputDisconnected,
    #[error("output worker panicked")]
    OutputWorkerPanicked,
}

pub struct RestrictedProcessRunner {
    workspace_root: PathBuf,
    workspace_handle: Dir,
    allowed_programs: BTreeSet<PathBuf>,
    allowed_environment: BTreeSet<String>,
    minimal_environment: BTreeMap<String, String>,
    sandbox: SandboxStatus,
}

impl RestrictedProcessRunner {
    /// # Errors
    ///
    /// Returns an error when the workspace or an allowlisted executable cannot be resolved.
    pub fn new(
        workspace_root: impl AsRef<Path>,
        allowed_programs: impl IntoIterator<Item = PathBuf>,
        allowed_environment: BTreeSet<String>,
        minimal_environment: BTreeMap<String, String>,
    ) -> Result<Self, RunError> {
        let workspace_root = std::fs::canonicalize(workspace_root.as_ref())?;
        let workspace_handle = Dir::open_ambient_dir(&workspace_root, ambient_authority())?;
        let allowed_programs = allowed_programs
            .into_iter()
            .map(std::fs::canonicalize)
            .collect::<Result<BTreeSet<_>, _>>()?;
        Ok(Self {
            workspace_root,
            workspace_handle,
            allowed_programs,
            allowed_environment,
            minimal_environment,
            sandbox: SandboxStatus::detect(),
        })
    }

    pub fn sandbox_status(&self) -> &SandboxStatus {
        &self.sandbox
    }

    /// # Errors
    ///
    /// Returns a typed policy, limit, cancellation, timeout, output, or process error.
    #[allow(clippy::too_many_lines)]
    pub fn run(
        &self,
        request: &RunRequest,
        cancellation: &CancellationToken,
        sink: &mut dyn OutputSink,
    ) -> Result<RunResult, RunError> {
        validate_process_limits(&request.limits)?;
        let program =
            std::fs::canonicalize(&request.program).map_err(|_| RunError::ProgramDenied)?;
        if !self.allowed_programs.contains(&program) {
            return Err(RunError::ProgramDenied);
        }
        let working =
            safe_relative(&request.working_directory).map_err(|_| RunError::WorkingDirectory)?;
        let working_handle = self
            .workspace_handle
            .open_dir(&working)
            .map_err(|_| RunError::WorkingDirectory)?;
        validate_environment(&request.environment, &self.allowed_environment)?;
        if request.isolation == IsolationRequest::UntrustedWorkspace
            && !self.sandbox.supports_untrusted()
        {
            return Err(RunError::StrongIsolationUnavailable);
        }
        validate_available_limits(&request.limits, request.isolation, &self.sandbox)?;
        let (launcher, arguments) = self.launch_command(&program, request)?;
        let mut command = Command::new(launcher);
        command
            .args(arguments)
            .stdin(Stdio::null())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .env_clear();
        for (key, value) in &self.minimal_environment {
            command.env(key, value);
        }
        for (key, value) in &request.environment {
            command.env(key, value);
        }
        configure_process_group(&mut command);
        configure_working_directory(
            &mut command,
            &working_handle,
            &self.workspace_root.join(&working),
        );
        cancellation.check().map_err(|_| RunError::Cancelled)?;
        let started = Instant::now();
        let mut child = command.spawn()?;
        let stdout = child.stdout.take().ok_or(RunError::OutputDisconnected)?;
        let stderr = child.stderr.take().ok_or(RunError::OutputDisconnected)?;
        let (sender, receiver) = mpsc::sync_channel(16);
        let stdout_reader = spawn_reader(stdout, OutputStream::Stdout, sender.clone());
        let stderr_reader = spawn_reader(stderr, OutputStream::Stderr, sender);
        let mut stdout_bytes = Vec::new();
        let mut stderr_bytes = Vec::new();
        let mut eof = 0_u8;
        let status = loop {
            if cancellation.is_cancelled() {
                terminate_process_tree(&mut child, request.limits.cancellation_grace);
                drop(receiver);
                join_readers(stdout_reader, stderr_reader)?;
                return Err(RunError::Cancelled);
            }
            if started.elapsed() >= request.limits.timeout {
                terminate_process_tree(&mut child, request.limits.cancellation_grace);
                drop(receiver);
                join_readers(stdout_reader, stderr_reader)?;
                return Err(RunError::Timeout);
            }
            match receiver.recv_timeout(Duration::from_millis(5)) {
                Ok(StreamMessage::Chunk(chunk)) => {
                    let total = stdout_bytes
                        .len()
                        .saturating_add(stderr_bytes.len())
                        .saturating_add(chunk.bytes.len());
                    if total > request.limits.output_bytes {
                        terminate_process_tree(&mut child, request.limits.cancellation_grace);
                        drop(receiver);
                        join_readers(stdout_reader, stderr_reader)?;
                        return Err(RunError::OutputLimit);
                    }
                    sink.emit(&chunk);
                    match chunk.stream {
                        OutputStream::Stdout => stdout_bytes.extend_from_slice(&chunk.bytes),
                        OutputStream::Stderr => stderr_bytes.extend_from_slice(&chunk.bytes),
                    }
                }
                Ok(StreamMessage::Eof) => eof = eof.saturating_add(1),
                Err(mpsc::RecvTimeoutError::Timeout) => continue,
                Err(mpsc::RecvTimeoutError::Disconnected) if eof < 2 => {
                    terminate_process_tree(&mut child, request.limits.cancellation_grace);
                    drop(receiver);
                    join_readers(stdout_reader, stderr_reader)?;
                    return Err(RunError::OutputDisconnected);
                }
                Err(mpsc::RecvTimeoutError::Disconnected) => {}
            }
            if eof == 2 {
                if let Some(status) = child.try_wait()? {
                    break status;
                }
            } else if let Some(status) = child.try_wait()? {
                drain_messages(
                    &receiver,
                    &mut stdout_bytes,
                    &mut stderr_bytes,
                    request.limits.output_bytes,
                    sink,
                    &mut eof,
                )?;
                break status;
            }
        };
        join_readers(stdout_reader, stderr_reader)?;
        Ok(RunResult {
            exit_code: status.code(),
            stdout: stdout_bytes,
            stderr: stderr_bytes,
            elapsed: started.elapsed(),
            sandbox: self.effective_status(request),
        })
    }

    fn launch_command(
        &self,
        program: &Path,
        request: &RunRequest,
    ) -> Result<(PathBuf, Vec<OsString>), RunError> {
        #[allow(unused_mut)]
        let mut executable = program.to_path_buf();
        #[allow(unused_mut)]
        let mut arguments = request
            .arguments
            .iter()
            .map(OsString::from)
            .collect::<Vec<_>>();
        #[cfg(target_os = "linux")]
        {
            if request.limits.cpu_seconds.is_some() || request.limits.memory_bytes.is_some() {
                let prlimit = find_executable(&["/usr/bin/prlimit", "/bin/prlimit"])
                    .ok_or_else(|| RunError::LimitUnavailable("prlimit is unavailable".into()))?;
                let mut wrapped = Vec::new();
                if let Some(seconds) = request.limits.cpu_seconds {
                    wrapped.push(format!("--cpu={seconds}:{seconds}").into());
                }
                if let Some(bytes) = request.limits.memory_bytes {
                    wrapped.push(format!("--as={bytes}:{bytes}").into());
                }
                wrapped.push("--".into());
                wrapped.push(executable.into_os_string());
                wrapped.append(&mut arguments);
                executable = prlimit;
                arguments = wrapped;
            }
            if request.isolation == IsolationRequest::UntrustedWorkspace {
                let launcher = self
                    .sandbox
                    .launcher
                    .clone()
                    .ok_or(RunError::StrongIsolationUnavailable)?;
                let root = self.workspace_root.as_os_str().to_owned();
                let work = self
                    .workspace_root
                    .join(&request.working_directory)
                    .into_os_string();
                let mut wrapped = vec![
                    "--die-with-parent".into(),
                    "--new-session".into(),
                    "--unshare-all".into(),
                ];
                if !request.limits.deny_network {
                    wrapped.push("--share-net".into());
                }
                for system in ["/usr", "/bin", "/lib", "/lib64"] {
                    if Path::new(system).exists() {
                        wrapped.extend(["--ro-bind".into(), system.into(), system.into()]);
                    }
                }
                wrapped.extend([
                    "--dev".into(),
                    "/dev".into(),
                    "--proc".into(),
                    "/proc".into(),
                    "--bind".into(),
                    root.clone(),
                    root,
                    "--chdir".into(),
                    work,
                    "--".into(),
                    executable.into_os_string(),
                ]);
                wrapped.append(&mut arguments);
                executable = launcher;
                arguments = wrapped;
            }
        }
        #[cfg(target_os = "macos")]
        if request.isolation == IsolationRequest::UntrustedWorkspace {
            let launcher = self
                .sandbox
                .launcher
                .clone()
                .ok_or(RunError::StrongIsolationUnavailable)?;
            let network = if request.limits.deny_network {
                ""
            } else {
                "(allow network*)"
            };
            let profile = format!(
                "(version 1)(deny default)(allow process*)(allow file-read* (subpath \"/usr\") (subpath \"/bin\"))(allow file-read* file-write* (subpath \"{}\")){network}",
                self.workspace_root.display()
            );
            let mut wrapped = vec![
                "-p".into(),
                profile.into(),
                "--".into(),
                executable.into_os_string(),
            ];
            wrapped.append(&mut arguments);
            executable = launcher;
            arguments = wrapped;
        }
        Ok((executable, arguments))
    }

    fn effective_status(&self, request: &RunRequest) -> SandboxStatus {
        if request.isolation == IsolationRequest::UntrustedWorkspace {
            self.sandbox.clone()
        } else {
            let mut status = self.sandbox.clone();
            status.level = IsolationLevel::Reduced;
            status.filesystem_containment = false;
            status.network_isolation = false;
            status.reduced_reasons.push(
                "trusted mode intentionally runs without filesystem/network isolation".into(),
            );
            status
        }
    }
}

enum StreamMessage {
    Chunk(OutputChunk),
    Eof,
}

fn spawn_reader(
    mut reader: impl Read + Send + 'static,
    stream: OutputStream,
    sender: mpsc::SyncSender<StreamMessage>,
) -> thread::JoinHandle<Result<(), std::io::Error>> {
    thread::spawn(move || {
        let mut buffer = [0_u8; 8 * 1_024];
        loop {
            let read = reader.read(&mut buffer)?;
            if read == 0 {
                let _ = sender.send(StreamMessage::Eof);
                return Ok(());
            }
            if sender
                .send(StreamMessage::Chunk(OutputChunk {
                    stream,
                    bytes: buffer[..read].to_vec(),
                }))
                .is_err()
            {
                return Ok(());
            }
        }
    })
}

fn drain_messages(
    receiver: &mpsc::Receiver<StreamMessage>,
    stdout: &mut Vec<u8>,
    stderr: &mut Vec<u8>,
    limit: usize,
    sink: &mut dyn OutputSink,
    eof: &mut u8,
) -> Result<(), RunError> {
    while *eof < 2 {
        match receiver.recv_timeout(Duration::from_millis(50)) {
            Ok(StreamMessage::Eof) => *eof = eof.saturating_add(1),
            Ok(StreamMessage::Chunk(chunk)) => {
                if stdout
                    .len()
                    .saturating_add(stderr.len())
                    .saturating_add(chunk.bytes.len())
                    > limit
                {
                    return Err(RunError::OutputLimit);
                }
                sink.emit(&chunk);
                match chunk.stream {
                    OutputStream::Stdout => stdout.extend_from_slice(&chunk.bytes),
                    OutputStream::Stderr => stderr.extend_from_slice(&chunk.bytes),
                }
            }
            Err(mpsc::RecvTimeoutError::Timeout) => {}
            Err(mpsc::RecvTimeoutError::Disconnected) => break,
        }
    }
    Ok(())
}

fn join_readers(
    stdout: thread::JoinHandle<Result<(), std::io::Error>>,
    stderr: thread::JoinHandle<Result<(), std::io::Error>>,
) -> Result<(), RunError> {
    for handle in [stdout, stderr] {
        handle
            .join()
            .map_err(|_| RunError::OutputWorkerPanicked)??;
    }
    Ok(())
}

fn validate_process_limits(limits: &ProcessLimits) -> Result<(), RunError> {
    if limits.timeout.is_zero()
        || limits.cancellation_grace.is_zero()
        || limits.output_bytes == 0
        || limits.cpu_seconds == Some(0)
        || limits.memory_bytes == Some(0)
    {
        return Err(RunError::LimitUnavailable("limits must be non-zero".into()));
    }
    Ok(())
}

fn validate_available_limits(
    limits: &ProcessLimits,
    isolation: IsolationRequest,
    status: &SandboxStatus,
) -> Result<(), RunError> {
    if limits.cpu_seconds.is_some() && !status.cpu_limit {
        return Err(RunError::LimitUnavailable(
            "CPU limiting is unavailable".into(),
        ));
    }
    if limits.memory_bytes.is_some() && !status.memory_limit {
        return Err(RunError::LimitUnavailable(
            "memory limiting is unavailable".into(),
        ));
    }
    if limits.deny_network
        && (isolation != IsolationRequest::UntrustedWorkspace || !status.network_isolation)
    {
        return Err(RunError::LimitUnavailable(
            "network isolation is unavailable".into(),
        ));
    }
    Ok(())
}

fn validate_environment(
    environment: &BTreeMap<String, String>,
    allowed: &BTreeSet<String>,
) -> Result<(), RunError> {
    if environment.iter().any(|(key, value)| {
        !allowed.contains(key)
            || key.is_empty()
            || key.contains('=')
            || key.contains('\0')
            || value.contains('\0')
    }) {
        return Err(RunError::EnvironmentDenied);
    }
    Ok(())
}

fn safe_relative(path: &Path) -> Result<PathBuf, WorkspaceError> {
    if path.as_os_str().is_empty() {
        return Ok(PathBuf::new());
    }
    if path.is_absolute()
        || path.components().any(|component| {
            matches!(
                component,
                Component::ParentDir | Component::RootDir | Component::Prefix(_)
            )
        })
    {
        return Err(WorkspaceError::UnsafePath);
    }
    for component in path.components() {
        let Component::Normal(name) = component else {
            continue;
        };
        let name = name.to_string_lossy();
        let stem = name
            .split('.')
            .next()
            .unwrap_or_default()
            .to_ascii_uppercase();
        let device = matches!(stem.as_str(), "CON" | "PRN" | "AUX" | "NUL")
            || stem.strip_prefix("COM").is_some_and(|number| {
                matches!(number, "1" | "2" | "3" | "4" | "5" | "6" | "7" | "8" | "9")
            })
            || stem.strip_prefix("LPT").is_some_and(|number| {
                matches!(number, "1" | "2" | "3" | "4" | "5" | "6" | "7" | "8" | "9")
            });
        if device || name.contains(':') || name.contains('\0') {
            return Err(WorkspaceError::UnsafePath);
        }
    }
    Ok(path.to_path_buf())
}

fn validate_preimage(
    expected: &ExpectedPreimage,
    actual: Option<&str>,
) -> Result<(), WorkspaceError> {
    match expected {
        ExpectedPreimage::Any => Ok(()),
        ExpectedPreimage::Missing if actual.is_none() => Ok(()),
        ExpectedPreimage::Sha256(expected) if actual == Some(expected.as_str()) => Ok(()),
        ExpectedPreimage::Missing | ExpectedPreimage::Sha256(_) => {
            Err(WorkspaceError::PreimageMismatch)
        }
    }
}

fn cancellation_check(cancellation: &CancellationToken) -> Result<(), WorkspaceError> {
    if cancellation.is_cancelled() {
        Err(WorkspaceError::Cancelled)
    } else {
        Ok(())
    }
}

fn hex_digest(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    let mut encoded = String::with_capacity(digest.len() * 2);
    for byte in digest {
        write!(&mut encoded, "{byte:02x}").expect("writing to a string cannot fail");
    }
    encoded
}

#[cfg(target_os = "linux")]
fn configure_working_directory(command: &mut Command, handle: &Dir, _ambient: &Path) {
    use std::os::fd::AsRawFd;

    command.current_dir(format!("/proc/self/fd/{}", handle.as_raw_fd()));
}

#[cfg(target_os = "macos")]
fn configure_working_directory(command: &mut Command, handle: &Dir, _ambient: &Path) {
    use std::os::fd::AsRawFd;

    command.current_dir(format!("/dev/fd/{}", handle.as_raw_fd()));
}

#[cfg(not(any(target_os = "linux", target_os = "macos")))]
fn configure_working_directory(command: &mut Command, _handle: &Dir, ambient: &Path) {
    command.current_dir(ambient);
}

#[cfg(unix)]
fn configure_process_group(command: &mut Command) {
    use std::os::unix::process::CommandExt;

    command.process_group(0);
}

#[cfg(windows)]
fn configure_process_group(command: &mut Command) {
    use std::os::windows::process::CommandExt;

    const CREATE_NEW_PROCESS_GROUP: u32 = 0x0000_0200;
    command.creation_flags(CREATE_NEW_PROCESS_GROUP);
}

#[cfg(not(any(unix, windows)))]
fn configure_process_group(_command: &mut Command) {}

#[cfg(unix)]
fn terminate_process_tree(child: &mut Child, grace: Duration) {
    use nix::sys::signal::{Signal, killpg};
    use nix::unistd::Pid;

    if let Ok(pid) = i32::try_from(child.id()) {
        let process_group = Pid::from_raw(pid);
        let _ = killpg(process_group, Signal::SIGTERM);
        if wait_for_exit(child, grace) {
            return;
        }
        let _ = killpg(process_group, Signal::SIGKILL);
    }
    let _ = child.kill();
    let _ = child.wait();
}

#[cfg(windows)]
fn terminate_process_tree(child: &mut Child, grace: Duration) {
    let _ = Command::new("taskkill")
        .args(["/PID", &child.id().to_string(), "/T"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
    if wait_for_exit(child, grace) {
        return;
    }
    let _ = Command::new("taskkill")
        .args(["/PID", &child.id().to_string(), "/T", "/F"])
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .status();
    let _ = child.kill();
    let _ = child.wait();
}

#[cfg(not(any(unix, windows)))]
fn terminate_process_tree(child: &mut Child, grace: Duration) {
    if wait_for_exit(child, grace) {
        return;
    }
    let _ = child.kill();
    let _ = child.wait();
}

fn wait_for_exit(child: &mut Child, grace: Duration) -> bool {
    let started = Instant::now();
    loop {
        if child.try_wait().ok().flatten().is_some() {
            return true;
        }
        if started.elapsed() >= grace {
            return false;
        }
        thread::sleep(Duration::from_millis(2).min(grace.saturating_sub(started.elapsed())));
    }
}

#[cfg(target_os = "linux")]
fn find_executable(candidates: &[&str]) -> Option<PathBuf> {
    candidates
        .iter()
        .map(Path::new)
        .find(|path| path.is_file())
        .map(Path::to_path_buf)
}

#[cfg(test)]
mod tests {
    #[cfg(target_os = "linux")]
    use std::sync::{Arc, Mutex};

    use super::*;

    #[cfg(target_os = "linux")]
    fn empty_sink(_chunk: &OutputChunk) {}

    #[test]
    fn workspace_operations_are_bounded_atomic_and_preimage_checked() {
        let directory = tempfile::tempdir().unwrap();
        let workspace = WorkspaceFs::open(directory.path(), WorkspaceLimits::default()).unwrap();
        let cancellation = CancellationToken::default();
        let written = workspace
            .write_atomic(
                "nested/file.txt",
                b"first line\nneedle line\n",
                &ExpectedPreimage::Missing,
                &cancellation,
            )
            .unwrap();
        assert_eq!(written.kind, ChangeKind::Written);
        assert_eq!(
            workspace.read("nested/file.txt", &cancellation).unwrap(),
            b"first line\nneedle line\n"
        );
        assert!(matches!(
            workspace.write_atomic(
                "nested/file.txt",
                b"wrong",
                &ExpectedPreimage::Missing,
                &cancellation
            ),
            Err(WorkspaceError::PreimageMismatch)
        ));
        workspace
            .edit(
                "nested/file.txt",
                b"first line\nneedle line\n",
                b"replacement needle\n",
                &cancellation,
            )
            .unwrap();
        assert_eq!(
            workspace
                .search(".", "needle", &cancellation)
                .unwrap()
                .len(),
            1
        );
        workspace.copy("nested/file.txt", "copy.txt").unwrap();
        workspace.rename("copy.txt", "renamed.txt").unwrap();
        assert_eq!(workspace.list(".").unwrap().len(), 2);
        let digest = hex_digest(b"replacement needle\n");
        workspace
            .delete("renamed.txt", &ExpectedPreimage::Sha256(digest))
            .unwrap();
        assert!(!directory.path().join("renamed.txt").exists());
        std::fs::create_dir(directory.path().join("empty-dir")).unwrap();
        workspace.rename("empty-dir", "moved-dir").unwrap();
        workspace
            .delete("moved-dir", &ExpectedPreimage::Any)
            .unwrap();
    }

    #[test]
    fn traversal_device_paths_size_and_cancellation_are_rejected() {
        let directory = tempfile::tempdir().unwrap();
        let workspace = WorkspaceFs::open(
            directory.path(),
            WorkspaceLimits {
                max_file_bytes: 4,
                ..WorkspaceLimits::default()
            },
        )
        .unwrap();
        let cancellation = CancellationToken::default();
        assert!(matches!(
            workspace.read("../secret", &cancellation),
            Err(WorkspaceError::UnsafePath)
        ));
        assert!(matches!(
            workspace.write_atomic("NUL.txt", b"x", &ExpectedPreimage::Missing, &cancellation),
            Err(WorkspaceError::UnsafePath)
        ));
        assert!(matches!(
            workspace.write_atomic("large", b"12345", &ExpectedPreimage::Missing, &cancellation),
            Err(WorkspaceError::LimitExceeded)
        ));
        cancellation.cancel();
        assert!(matches!(
            workspace.read("missing", &cancellation),
            Err(WorkspaceError::Cancelled)
        ));
    }

    #[cfg(unix)]
    #[test]
    fn symlinks_and_symlink_swap_races_cannot_escape_the_capability_root() {
        use std::os::unix::fs::symlink;
        use std::sync::atomic::{AtomicBool, Ordering};

        let directory = tempfile::tempdir().unwrap();
        let outside = tempfile::tempdir().unwrap();
        let secret = outside.path().join("secret.txt");
        std::fs::write(&secret, b"outside-secret").unwrap();
        symlink(&secret, directory.path().join("link")).unwrap();
        let workspace = WorkspaceFs::open(directory.path(), WorkspaceLimits::default()).unwrap();
        assert!(matches!(
            workspace.read("link", &CancellationToken::default()),
            Err(WorkspaceError::Symlink | WorkspaceError::Io(_))
        ));

        let victim = directory.path().join("victim");
        std::fs::write(&victim, b"inside").unwrap();
        let running = Arc::new(AtomicBool::new(true));
        let worker_running = Arc::clone(&running);
        let target = secret.clone();
        let attacker = thread::spawn(move || {
            let replacement = victim.with_extension("replacement");
            while worker_running.load(Ordering::Acquire) {
                let _ = std::fs::remove_file(&victim);
                let _ = symlink(&target, &victim);
                let _ = std::fs::write(&replacement, b"inside");
                let _ = std::fs::rename(&replacement, &victim);
            }
        });
        for _ in 0..200 {
            if let Ok(bytes) = workspace.read("victim", &CancellationToken::default()) {
                assert_eq!(bytes, b"inside");
            }
        }
        running.store(false, Ordering::Release);
        attacker.join().unwrap();
        assert_eq!(std::fs::read(secret).unwrap(), b"outside-secret");
    }

    #[cfg(target_os = "linux")]
    fn runner(programs: &[&str]) -> (tempfile::TempDir, RestrictedProcessRunner) {
        let directory = tempfile::tempdir().unwrap();
        let runner = RestrictedProcessRunner::new(
            directory.path(),
            programs.iter().map(PathBuf::from).collect::<Vec<_>>(),
            BTreeSet::from(["KEITH_VISIBLE".into()]),
            BTreeMap::from([("LANG".into(), "C".into())]),
        )
        .unwrap();
        (directory, runner)
    }

    #[cfg(target_os = "linux")]
    fn request(program: &str, arguments: Vec<String>) -> RunRequest {
        RunRequest {
            program: PathBuf::from(program),
            arguments,
            working_directory: PathBuf::from("."),
            environment: BTreeMap::new(),
            isolation: IsolationRequest::TrustedWorkspace,
            limits: ProcessLimits::default(),
        }
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn argv_is_not_reparsed_and_environment_is_minimal() {
        let (directory, runner) = runner(&["/usr/bin/printf", "/usr/bin/env"]);
        let marker = directory.path().join("injected");
        let argument = format!("literal; touch {}", marker.display());
        let result = runner
            .run(
                &request("/usr/bin/printf", vec!["%s".into(), argument.clone()]),
                &CancellationToken::default(),
                &mut empty_sink,
            )
            .unwrap();
        assert_eq!(result.stdout, argument.as_bytes());
        assert!(!marker.exists());

        let mut env_request = request("/usr/bin/env", Vec::new());
        env_request
            .environment
            .insert("KEITH_VISIBLE".into(), "yes".into());
        let result = runner
            .run(&env_request, &CancellationToken::default(), &mut empty_sink)
            .unwrap();
        let output = String::from_utf8(result.stdout).unwrap();
        assert!(output.contains("LANG=C"));
        assert!(output.contains("KEITH_VISIBLE=yes"));
        assert!(!output.contains("HOME="));
        env_request.environment.insert("HOME".into(), "leak".into());
        assert!(matches!(
            runner.run(&env_request, &CancellationToken::default(), &mut empty_sink),
            Err(RunError::EnvironmentDenied)
        ));
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn output_flood_and_timeout_kill_the_process_tree() {
        let (_directory, runner) = runner(&["/usr/bin/yes", "/bin/sh"]);
        let mut flood = request("/usr/bin/yes", Vec::new());
        flood.limits.output_bytes = 16 * 1_024;
        assert!(matches!(
            runner.run(&flood, &CancellationToken::default(), &mut empty_sink),
            Err(RunError::OutputLimit)
        ));

        let mut timeout = request("/bin/sh", vec!["-c".into(), "sleep 30 & wait".into()]);
        timeout.limits.timeout = Duration::from_millis(30);
        assert!(matches!(
            runner.run(&timeout, &CancellationToken::default(), &mut empty_sink),
            Err(RunError::Timeout)
        ));
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn cancellation_streams_output_then_forces_stubborn_process_tree_after_grace() {
        let (_directory, runner) = runner(&["/bin/sh"]);
        let cancellation = CancellationToken::default();
        let trigger = cancellation.clone();
        let canceller = thread::spawn(move || {
            thread::sleep(Duration::from_millis(40));
            trigger.cancel();
        });
        let chunks = Arc::new(Mutex::new(Vec::new()));
        let captured = Arc::clone(&chunks);
        let mut sink = move |chunk: &OutputChunk| {
            captured.lock().unwrap().extend_from_slice(&chunk.bytes);
        };
        let mut run = request(
            "/bin/sh",
            vec![
                "-c".into(),
                "trap '' TERM; sleep 30 & echo $!; while :; do wait; done".into(),
            ],
        );
        run.limits.cancellation_grace = Duration::from_millis(40);
        let started = Instant::now();
        assert!(matches!(
            runner.run(&run, &cancellation, &mut sink),
            Err(RunError::Cancelled)
        ));
        assert!(started.elapsed() >= Duration::from_millis(70));
        canceller.join().unwrap();
        let output = String::from_utf8(chunks.lock().unwrap().clone()).unwrap();
        let child_pid = output.trim().parse::<u32>().unwrap();
        thread::sleep(Duration::from_millis(20));
        assert!(!PathBuf::from(format!("/proc/{child_pid}")).exists());
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn untrusted_execution_never_silently_downgrades() {
        let (_directory, runner) = runner(&["/usr/bin/printf"]);
        let mut run = request("/usr/bin/printf", vec!["ok".into()]);
        run.isolation = IsolationRequest::UntrustedWorkspace;
        if runner.sandbox_status().supports_untrusted() {
            let result = runner
                .run(&run, &CancellationToken::default(), &mut empty_sink)
                .unwrap();
            assert_eq!(result.sandbox.level, IsolationLevel::Strong);
        } else {
            assert!(matches!(
                runner.run(&run, &CancellationToken::default(), &mut empty_sink),
                Err(RunError::StrongIsolationUnavailable)
            ));
        }
    }
}
