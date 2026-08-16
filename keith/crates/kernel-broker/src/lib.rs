#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::{self, Debug};
use std::fs;
use std::io::{BufRead, BufReader, Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, ChildStdout, Command, Stdio};
use std::sync::atomic::{AtomicI64, Ordering};
use std::sync::mpsc::{self, RecvTimeoutError};
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant};

use keith_agent_types::{EntityId, KernelId, SessionId, UtcTimestamp};
use keith_artifacts::{OutputSpill, SpilledOutput};
use keith_kernel_protocol::{
    BridgeCapability, BridgeContext, BridgeFailure, BridgeOperation, BridgeReply, ExcludedState,
    GuestCommand, GuestEvent, GuestEventKind, GuestRequest, GuestStream, KERNEL_PROTOCOL_VERSION,
};
use keith_provider_core::CancellationToken;
use keith_sandbox::SandboxStatus;
use serde::{Deserialize, Serialize};
use thiserror::Error;

const PYTHON_GUEST: &str = r#"
import contextlib
import io
import json
import sys
import traceback
import uuid

PROTOCOL = 1
CURRENT_REQUEST = None

def emit(event, request_id=None):
    message = {"protocol": PROTOCOL, "request_id": request_id, "event": event}
    sys.__stdout__.write(json.dumps(message, separators=(",", ":")) + "\n")
    sys.__stdout__.flush()

class EventWriter(io.TextIOBase):
    def __init__(self, stream):
        self.stream = stream
    def writable(self):
        return True
    def write(self, text):
        if text:
            text = str(text)
            for offset in range(0, len(text), 8192):
                emit({"event": "output", "stream": self.stream, "text": text[offset:offset + 8192]}, CURRENT_REQUEST)
        return len(text)
    def flush(self):
        return None

def bridge(operation):
    bridge_id = "0" + uuid.uuid4().hex[:25].upper()
    emit({"event": "bridge_request", "bridge_id": bridge_id, "operation": operation}, CURRENT_REQUEST)
    line = sys.__stdin__.readline()
    if not line:
        raise RuntimeError("bridge disconnected")
    reply = json.loads(line)
    if reply.get("protocol") != PROTOCOL or reply.get("bridge_id") != bridge_id:
        raise RuntimeError("invalid bridge reply")
    if reply.get("error") is not None:
        raise RuntimeError(reply["error"].get("message", "bridge request denied"))
    return reply.get("result")

STATE = {"bridge": bridge, "__builtins__": __builtins__}

def json_value(value):
    try:
        encoded = json.dumps(value)
        if len(encoded.encode("utf-8")) <= 32768:
            return value
        return "<result omitted: exceeds kernel result limit>"
    except Exception:
        return repr(value)

emit({"event": "ready", "runtime": "python"})

for line in sys.__stdin__:
    request = None
    try:
        request = json.loads(line)
        request_id = request["request_id"]
        if request.get("protocol") != PROTOCOL:
            emit({"event": "error", "code": "protocol", "message": "unsupported protocol"}, request_id)
            continue
        command = request["command"]
        kind = command["command"]
        if kind == "execute":
            CURRENT_REQUEST = request_id
            result = None
            error = None
            try:
                code = command["code"]
                try:
                    compiled = compile(code, "<keith-kernel>", "eval")
                except SyntaxError:
                    compiled = compile(code, "<keith-kernel>", "exec")
                    with contextlib.redirect_stdout(EventWriter("stdout")), contextlib.redirect_stderr(EventWriter("stderr")):
                        exec(compiled, STATE, STATE)
                else:
                    with contextlib.redirect_stdout(EventWriter("stdout")), contextlib.redirect_stderr(EventWriter("stderr")):
                        result = json_value(eval(compiled, STATE, STATE))
            except BaseException:
                error = traceback.format_exc(limit=8)
            emit({"event": "execution_finished", "result": result, "error": error}, request_id)
            CURRENT_REQUEST = None
        elif kind == "snapshot":
            saved = {}
            excluded = []
            for name, value in STATE.items():
                if name.startswith("__") or name == "bridge":
                    continue
                try:
                    json.dumps(value)
                    saved[name] = value
                    if len(json.dumps(saved, separators=(",", ":")).encode("utf-8")) > 32768:
                        del saved[name]
                        excluded.append({"name": name, "type_name": type(value).__name__, "reason": "snapshot byte budget exceeded"})
                except Exception:
                    excluded.append({"name": name, "type_name": type(value).__name__, "reason": "not JSON serializable"})
            emit({"event": "snapshot", "state": saved, "excluded": excluded}, request_id)
        elif kind == "restore":
            for name in list(STATE):
                if not name.startswith("__") and name != "bridge":
                    del STATE[name]
            STATE.update(command["state"])
            emit({"event": "restored"}, request_id)
        elif kind == "shutdown":
            emit({"event": "shutdown"}, request_id)
            break
        else:
            emit({"event": "error", "code": "command", "message": "unknown command"}, request_id)
    except BaseException:
        emit({"event": "error", "code": "guest", "message": traceback.format_exc(limit=4)}, request.get("request_id") if isinstance(request, dict) else None)
"#;

const MAX_PROTOCOL_LINE_BYTES: u64 = 64 * 1024;
const MAX_SNAPSHOT_STATE_BYTES: usize = 48 * 1024;

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum KernelRuntime {
    Python { executable: PathBuf },
    StrictWasm { runtime: PathBuf, module: PathBuf },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum KernelIsolation {
    Untrusted,
    TrustedLocal,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum KernelNetwork {
    Denied,
    Allowed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct KernelLimits {
    pub memory_bytes: u64,
    pub cpu_seconds: u64,
    pub max_processes: u32,
    pub max_output_bytes: usize,
    pub max_inline_output_bytes: usize,
    pub max_code_bytes: usize,
    pub max_snapshot_bytes: usize,
    pub max_bridge_calls: u32,
    pub execution_timeout: Duration,
    pub cancellation_grace: Duration,
    pub max_lifetime: Duration,
    pub idle_timeout: Duration,
}

impl Default for KernelLimits {
    fn default() -> Self {
        Self {
            memory_bytes: 512 * 1024 * 1024,
            cpu_seconds: 30,
            max_processes: 8,
            max_output_bytes: 8 * 1024 * 1024,
            max_inline_output_bytes: 32 * 1024,
            max_code_bytes: 256 * 1024,
            max_snapshot_bytes: MAX_SNAPSHOT_STATE_BYTES,
            max_bridge_calls: 32,
            execution_timeout: Duration::from_secs(30),
            cancellation_grace: Duration::from_millis(250),
            max_lifetime: Duration::from_secs(8 * 60 * 60),
            idle_timeout: Duration::from_secs(30 * 60),
        }
    }
}

#[derive(Clone, Debug)]
pub struct KernelSpec {
    pub session_id: SessionId,
    pub runtime: KernelRuntime,
    pub working_directory: PathBuf,
    pub isolation: KernelIsolation,
    pub network: KernelNetwork,
    pub limits: KernelLimits,
    pub allowed_bridge: BTreeSet<BridgeCapability>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum KernelOutputStream {
    Stdout,
    Stderr,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct KernelOutputChunk {
    pub stream: KernelOutputStream,
    pub text: String,
}

pub trait KernelOutputSink: Send {
    fn emit(&mut self, chunk: &KernelOutputChunk);
}

#[derive(Debug, Default)]
pub struct NoKernelOutput;

impl KernelOutputSink for NoKernelOutput {
    fn emit(&mut self, _chunk: &KernelOutputChunk) {}
}

pub trait BridgeHandler: Send + Sync {
    /// Executes a typed bridge operation without exposing mutable session state to the guest.
    ///
    /// # Errors
    ///
    /// Returns a bounded failure that is serialized back to the guest.
    fn handle(
        &self,
        context: &BridgeContext,
        operation: &BridgeOperation,
    ) -> Result<serde_json::Value, BridgeFailure>;
}

#[derive(Debug, Default)]
pub struct DenyBridge;

impl BridgeHandler for DenyBridge {
    fn handle(
        &self,
        _context: &BridgeContext,
        _operation: &BridgeOperation,
    ) -> Result<serde_json::Value, BridgeFailure> {
        Err(BridgeFailure {
            code: "denied".into(),
            message: "kernel bridge access is denied".into(),
        })
    }
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct KernelUsage {
    pub executions: u64,
    pub bridge_calls: u64,
    pub output_bytes: u64,
    pub wall_time_ms: u64,
}

#[derive(Clone, Debug, PartialEq)]
pub struct KernelExecution {
    pub result: Option<serde_json::Value>,
    pub error: Option<String>,
    pub preview: String,
    pub total_output_bytes: usize,
    pub output_truncated: bool,
    pub spill: Option<SpilledOutput>,
    pub usage: KernelUsage,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct KernelCompatibility {
    protocol: u16,
    runtime: String,
    isolation: KernelIsolation,
    network: KernelNetwork,
    memory_bytes: u64,
    cpu_seconds: u64,
    max_processes: u32,
    workspace: String,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct KernelSnapshot {
    pub id: EntityId,
    pub source_kernel_id: KernelId,
    pub session_id: SessionId,
    pub compatibility: KernelCompatibility,
    pub state: serde_json::Value,
    pub excluded: Vec<ExcludedState>,
    pub created_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct KernelInspection {
    pub id: KernelId,
    pub session_id: SessionId,
    pub runtime: String,
    pub isolation: KernelIsolation,
    pub network: KernelNetwork,
    pub usage: KernelUsage,
    pub created_at: UtcTimestamp,
    pub last_used_at: UtcTimestamp,
}

#[derive(Debug, Error)]
pub enum KernelError {
    #[error("kernel I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("kernel protocol failed: {0}")]
    Protocol(#[from] serde_json::Error),
    #[error("kernel configuration is invalid: {0}")]
    Invalid(String),
    #[error("strong kernel isolation is unavailable")]
    StrongIsolationUnavailable,
    #[error("kernel resource limiting is unavailable")]
    ResourceLimitUnavailable,
    #[error("kernel {0} was not found")]
    NotFound(KernelId),
    #[error("kernel process exited or disconnected")]
    Crashed,
    #[error("kernel execution timed out")]
    Timeout,
    #[error("kernel execution was cancelled")]
    Cancelled,
    #[error("kernel output exceeded its configured hard limit")]
    OutputLimit,
    #[error("kernel bridge call exceeded policy or configured limits")]
    BridgeDenied,
    #[error("kernel snapshot is missing")]
    SnapshotMissing,
    #[error("kernel snapshot is incompatible with the requested sandbox")]
    IncompatibleSnapshot,
    #[error("kernel snapshot exceeded its configured byte limit")]
    SnapshotLimit,
    #[error("kernel artifact spill failed: {0}")]
    Artifact(String),
    #[error("kernel broker state lock was poisoned")]
    LockPoisoned,
}

struct ProcessIo {
    child: Child,
    stdin: ChildStdin,
    stdout: BufReader<ChildStdout>,
}

struct KernelProcess {
    id: KernelId,
    pid: u32,
    spec: KernelSpec,
    compatibility: KernelCompatibility,
    created_at: UtcTimestamp,
    last_used_ms: AtomicI64,
    usage: Mutex<KernelUsage>,
    io: Mutex<ProcessIo>,
}

impl Debug for KernelProcess {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("KernelProcess")
            .field("id", &self.id)
            .field("session_id", &self.spec.session_id)
            .field("isolation", &self.spec.isolation)
            .finish_non_exhaustive()
    }
}

impl Drop for KernelProcess {
    fn drop(&mut self) {
        if let Ok(io) = self.io.get_mut() {
            terminate_process(&mut io.child);
        }
    }
}

pub struct KernelBroker {
    root: PathBuf,
    sandbox: SandboxStatus,
    bridge: Arc<dyn BridgeHandler>,
    spill: Option<Arc<dyn OutputSpill>>,
    kernels: Mutex<BTreeMap<KernelId, Arc<KernelProcess>>>,
}

impl KernelBroker {
    /// Opens a broker-owned snapshot directory.
    ///
    /// # Errors
    ///
    /// Returns an error when the root cannot be created or restricted.
    pub fn open(
        root: impl AsRef<Path>,
        bridge: Arc<dyn BridgeHandler>,
        spill: Option<Arc<dyn OutputSpill>>,
    ) -> Result<Self, KernelError> {
        let root = root.as_ref().to_path_buf();
        fs::create_dir_all(root.join("snapshots"))?;
        restrict_directory(&root)?;
        Ok(Self {
            root,
            sandbox: SandboxStatus::detect(),
            bridge,
            spill,
            kernels: Mutex::new(BTreeMap::new()),
        })
    }

    pub fn sandbox_status(&self) -> &SandboxStatus {
        &self.sandbox
    }

    /// Starts a real persistent guest process using the configured sandbox profile.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid runtime, limits, unavailable isolation, or startup failure.
    pub fn start(&self, spec: KernelSpec, now: UtcTimestamp) -> Result<KernelId, KernelError> {
        validate_spec(&spec, &self.sandbox)?;
        let compatibility = compatibility(&spec)?;
        let id = KernelId::new();
        let mut command = build_command(&spec, &self.sandbox)?;
        command
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::null())
            .env_clear()
            .env("LANG", "C.UTF-8")
            .env("PYTHONHASHSEED", "0")
            .env("PYTHONNOUSERSITE", "1");
        configure_process_group(&mut command);
        let mut child = command.spawn()?;
        let pid = child.id();
        let stdin = child.stdin.take().ok_or(KernelError::Crashed)?;
        let stdout = child.stdout.take().ok_or(KernelError::Crashed)?;
        let mut io = ProcessIo {
            child,
            stdin,
            stdout: BufReader::new(stdout),
        };
        let ready = read_event(&mut io.stdout)?;
        if !matches!(ready.event, GuestEventKind::Ready { .. }) {
            terminate_process(&mut io.child);
            return Err(KernelError::Crashed);
        }
        let process = Arc::new(KernelProcess {
            id: id.clone(),
            pid,
            spec,
            compatibility,
            created_at: now,
            last_used_ms: AtomicI64::new(now.unix_millis()),
            usage: Mutex::new(KernelUsage::default()),
            io: Mutex::new(io),
        });
        self.kernels
            .lock()
            .map_err(|_| KernelError::LockPoisoned)?
            .insert(id.clone(), process);
        Ok(id)
    }

    /// Executes code in a persistent kernel with streaming, cancellation, bridge, and output bounds.
    ///
    /// # Errors
    ///
    /// Returns an error for a missing/crashed process, timeout, cancellation, output, or bridge failure.
    #[allow(clippy::too_many_lines)]
    pub fn execute(
        &self,
        id: &KernelId,
        code: impl Into<String>,
        cancellation: &CancellationToken,
        sink: &mut dyn KernelOutputSink,
        now: UtcTimestamp,
    ) -> Result<KernelExecution, KernelError> {
        let process = self.process(id)?;
        ensure_lifetime(&process, now)?;
        let started = Instant::now();
        let code = code.into();
        if code.len() > process.spec.limits.max_code_bytes {
            return Err(KernelError::Invalid(
                "kernel code exceeded its configured byte limit".into(),
            ));
        }
        let request_id = EntityId::new();
        let request = GuestRequest {
            protocol: KERNEL_PROTOCOL_VERSION,
            request_id: request_id.clone(),
            command: GuestCommand::Execute { code },
        };
        let mut collected = Vec::new();
        let mut preview = String::new();
        let mut bridge_calls = 0_u32;
        let mut truncated = false;
        let terminal = Self::exchange(
            &process,
            &request,
            cancellation,
            process.spec.limits.execution_timeout,
            |event, stdin| match event {
                GuestEventKind::Output { stream, text } => {
                    let bytes = text.as_bytes();
                    let remaining = process
                        .spec
                        .limits
                        .max_output_bytes
                        .saturating_sub(collected.len());
                    let accepted = bytes.len().min(remaining);
                    collected.extend_from_slice(&bytes[..accepted]);
                    if accepted < bytes.len() {
                        truncated = true;
                    }
                    if preview.len() < process.spec.limits.max_inline_output_bytes {
                        let preview_remaining = process
                            .spec
                            .limits
                            .max_inline_output_bytes
                            .saturating_sub(preview.len());
                        let safe = floor_char_boundary(&text, preview_remaining);
                        let shown = &text[..safe];
                        preview.push_str(shown);
                        if !shown.is_empty() {
                            sink.emit(&KernelOutputChunk {
                                stream: match stream {
                                    GuestStream::Stdout => KernelOutputStream::Stdout,
                                    GuestStream::Stderr => KernelOutputStream::Stderr,
                                },
                                text: shown.to_owned(),
                            });
                        }
                    }
                    Ok(None)
                }
                GuestEventKind::BridgeRequest {
                    bridge_id,
                    operation,
                } => {
                    bridge_calls = bridge_calls.saturating_add(1);
                    let permitted = bridge_calls <= process.spec.limits.max_bridge_calls
                        && process
                            .spec
                            .allowed_bridge
                            .contains(&operation.capability());
                    let handled = if permitted {
                        self.bridge.handle(
                            &BridgeContext {
                                kernel_id: process.id.clone(),
                                session_id: process.spec.session_id.clone(),
                            },
                            &operation,
                        )
                    } else {
                        Err(BridgeFailure {
                            code: "denied".into(),
                            message: "bridge capability is not allowed for this kernel".into(),
                        })
                    };
                    let (result, error) = match handled {
                        Ok(result) => (Some(result), None),
                        Err(error) => (None, Some(error)),
                    };
                    write_bridge_reply(
                        stdin,
                        &BridgeReply {
                            protocol: KERNEL_PROTOCOL_VERSION,
                            request_id: request_id.clone(),
                            bridge_id,
                            result,
                            error,
                        },
                    )?;
                    Ok(None)
                }
                GuestEventKind::ExecutionFinished { .. } | GuestEventKind::Error { .. } => {
                    Ok(Some(event))
                }
                _ => Ok(None),
            },
        )?;
        process
            .last_used_ms
            .store(now.unix_millis(), Ordering::Release);
        let elapsed = started.elapsed();
        let mut usage = process
            .usage
            .lock()
            .map_err(|_| KernelError::LockPoisoned)?;
        usage.executions = usage.executions.saturating_add(1);
        usage.bridge_calls = usage.bridge_calls.saturating_add(u64::from(bridge_calls));
        usage.output_bytes = usage
            .output_bytes
            .saturating_add(u64::try_from(collected.len()).unwrap_or(u64::MAX));
        usage.wall_time_ms = usage
            .wall_time_ms
            .saturating_add(u64::try_from(elapsed.as_millis()).unwrap_or(u64::MAX));
        let current_usage = *usage;
        drop(usage);
        let spill = if collected.len() > process.spec.limits.max_inline_output_bytes {
            self.spill
                .as_ref()
                .map(|spill| {
                    spill
                        .spill(&collected)
                        .map_err(|error| KernelError::Artifact(error.to_string()))
                })
                .transpose()?
        } else {
            None
        };
        match terminal {
            GuestEventKind::ExecutionFinished { result, error } => Ok(KernelExecution {
                result,
                error,
                preview,
                total_output_bytes: collected.len(),
                output_truncated: truncated,
                spill,
                usage: current_usage,
            }),
            GuestEventKind::Error { message, .. } => Err(KernelError::Invalid(message)),
            _ => Err(KernelError::Crashed),
        }
    }

    /// Sends an interrupt to the complete kernel process group.
    ///
    /// # Errors
    ///
    /// Returns an error when the kernel is missing or cannot be signalled.
    pub fn interrupt(&self, id: &KernelId) -> Result<(), KernelError> {
        let process = self.process(id)?;
        signal_interrupt(process_pid(&process))
    }

    /// Creates and durably stores a best-effort snapshot with excluded-state reporting.
    ///
    /// # Errors
    ///
    /// Returns an error for process, protocol, size, or persistence failures.
    pub fn snapshot(
        &self,
        id: &KernelId,
        cancellation: &CancellationToken,
        now: UtcTimestamp,
    ) -> Result<KernelSnapshot, KernelError> {
        let process = self.process(id)?;
        let request = GuestRequest {
            protocol: KERNEL_PROTOCOL_VERSION,
            request_id: EntityId::new(),
            command: GuestCommand::Snapshot,
        };
        let terminal = Self::exchange(
            &process,
            &request,
            cancellation,
            process.spec.limits.execution_timeout,
            |event, _stdin| match event {
                GuestEventKind::Snapshot { .. } | GuestEventKind::Error { .. } => Ok(Some(event)),
                _ => Ok(None),
            },
        )?;
        let GuestEventKind::Snapshot { state, excluded } = terminal else {
            return Err(KernelError::Crashed);
        };
        let snapshot = KernelSnapshot {
            id: EntityId::new(),
            source_kernel_id: process.id.clone(),
            session_id: process.spec.session_id.clone(),
            compatibility: process.compatibility.clone(),
            state,
            excluded,
            created_at: now,
        };
        let bytes = serde_json::to_vec(&snapshot)?;
        if bytes.len() > process.spec.limits.max_snapshot_bytes {
            return Err(KernelError::SnapshotLimit);
        }
        write_snapshot(&self.snapshot_path(&snapshot.id), &bytes)?;
        process
            .last_used_ms
            .store(now.unix_millis(), Ordering::Release);
        Ok(snapshot)
    }

    /// Restores a durable snapshot into a newly isolated compatible process.
    ///
    /// # Errors
    ///
    /// Returns an error for missing/incompatible snapshots or guest startup/restore failure.
    pub fn restore(
        &self,
        snapshot_id: &EntityId,
        spec: KernelSpec,
        cancellation: &CancellationToken,
        now: UtcTimestamp,
    ) -> Result<KernelId, KernelError> {
        let snapshot = self.load_snapshot(snapshot_id)?;
        if snapshot.session_id != spec.session_id || snapshot.compatibility != compatibility(&spec)?
        {
            return Err(KernelError::IncompatibleSnapshot);
        }
        let id = self.start(spec, now)?;
        let process = self.process(&id)?;
        let request = GuestRequest {
            protocol: KERNEL_PROTOCOL_VERSION,
            request_id: EntityId::new(),
            command: GuestCommand::Restore {
                state: snapshot.state,
            },
        };
        let result = Self::exchange(
            &process,
            &request,
            cancellation,
            process.spec.limits.execution_timeout,
            |event, _stdin| match event {
                GuestEventKind::Restored | GuestEventKind::Error { .. } => Ok(Some(event)),
                _ => Ok(None),
            },
        );
        if !matches!(result, Ok(GuestEventKind::Restored)) {
            let _ = self.shutdown(&id);
            return match result {
                Err(error) => Err(error),
                Ok(_) => Err(KernelError::Crashed),
            };
        }
        Ok(id)
    }

    /// Loads snapshot metadata and state from broker-owned storage.
    ///
    /// # Errors
    ///
    /// Returns an error for missing, unreadable, or corrupt snapshots.
    pub fn load_snapshot(&self, id: &EntityId) -> Result<KernelSnapshot, KernelError> {
        let bytes = fs::read(self.snapshot_path(id)).map_err(|error| {
            if error.kind() == std::io::ErrorKind::NotFound {
                KernelError::SnapshotMissing
            } else {
                KernelError::Io(error)
            }
        })?;
        serde_json::from_slice(&bytes).map_err(KernelError::from)
    }

    /// Returns a redacted inspection without process paths, environment, or guest state.
    ///
    /// # Errors
    ///
    /// Returns an error for missing or inaccessible broker state.
    pub fn inspect(&self, id: &KernelId) -> Result<KernelInspection, KernelError> {
        let process = self.process(id)?;
        Ok(KernelInspection {
            id: process.id.clone(),
            session_id: process.spec.session_id.clone(),
            runtime: runtime_name(&process.spec.runtime).into(),
            isolation: process.spec.isolation,
            network: process.spec.network,
            usage: *process
                .usage
                .lock()
                .map_err(|_| KernelError::LockPoisoned)?,
            created_at: process.created_at,
            last_used_at: UtcTimestamp::from_unix_millis(
                process.last_used_ms.load(Ordering::Acquire),
            ),
        })
    }

    /// Returns redacted inspections for every currently active kernel in stable identifier order.
    ///
    /// # Errors
    ///
    /// Returns an error when broker or usage state cannot be read.
    pub fn inspections(&self) -> Result<Vec<KernelInspection>, KernelError> {
        let ids = self
            .kernels
            .lock()
            .map_err(|_| KernelError::LockPoisoned)?
            .keys()
            .cloned()
            .collect::<Vec<_>>();
        ids.iter().map(|id| self.inspect(id)).collect()
    }

    /// Evicts kernels whose idle or total lifetime bound has elapsed.
    ///
    /// # Errors
    ///
    /// Returns an error when broker state is inaccessible.
    pub fn evict_idle(&self, now: UtcTimestamp) -> Result<Vec<KernelId>, KernelError> {
        let mut kernels = self.kernels.lock().map_err(|_| KernelError::LockPoisoned)?;
        let expired = kernels
            .iter()
            .filter(|(_, process)| idle_or_expired(process, now))
            .map(|(id, _)| id.clone())
            .collect::<Vec<_>>();
        for id in &expired {
            kernels.remove(id);
        }
        Ok(expired)
    }

    /// Stops one process and removes its live broker entry.
    ///
    /// # Errors
    ///
    /// Returns an error when the kernel is missing or broker state is inaccessible.
    pub fn shutdown(&self, id: &KernelId) -> Result<(), KernelError> {
        let process = self
            .kernels
            .lock()
            .map_err(|_| KernelError::LockPoisoned)?
            .remove(id)
            .ok_or_else(|| KernelError::NotFound(id.clone()))?;
        let mut io = process.io.lock().map_err(|_| KernelError::LockPoisoned)?;
        terminate_process(&mut io.child);
        Ok(())
    }

    fn process(&self, id: &KernelId) -> Result<Arc<KernelProcess>, KernelError> {
        self.kernels
            .lock()
            .map_err(|_| KernelError::LockPoisoned)?
            .get(id)
            .cloned()
            .ok_or_else(|| KernelError::NotFound(id.clone()))
    }

    fn snapshot_path(&self, id: &EntityId) -> PathBuf {
        self.root.join("snapshots").join(format!("{id}.json"))
    }

    fn exchange<F>(
        process: &KernelProcess,
        request: &GuestRequest,
        cancellation: &CancellationToken,
        timeout: Duration,
        mut handle: F,
    ) -> Result<GuestEventKind, KernelError>
    where
        F: FnMut(GuestEventKind, &mut ChildStdin) -> Result<Option<GuestEventKind>, KernelError>,
    {
        if cancellation.is_cancelled() {
            return Err(KernelError::Cancelled);
        }
        let mut io = process.io.lock().map_err(|_| KernelError::LockPoisoned)?;
        if io.child.try_wait()?.is_some() {
            return Err(KernelError::Crashed);
        }
        write_json_line(&mut io.stdin, request)?;
        let started = Instant::now();
        let pid = io.child.id();
        let (sender, receiver) = mpsc::sync_channel(16);
        let ProcessIo {
            child,
            stdin,
            stdout,
        } = &mut *io;
        std::thread::scope(|scope| {
            let reader = scope.spawn(move || {
                loop {
                    match read_event(stdout) {
                        Ok(event) => {
                            let terminal = event.request_id.as_ref() == Some(&request.request_id)
                                && is_exchange_terminal(&event.event);
                            if sender.send(Ok(event)).is_err() || terminal {
                                return;
                            }
                        }
                        Err(error) => {
                            let _ = sender.send(Err(error));
                            return;
                        }
                    }
                }
            });
            let mut cancellation_started = None;
            let result = loop {
                if cancellation.is_cancelled() {
                    if let Some(cancelled_at) = cancellation_started {
                        if Instant::now().duration_since(cancelled_at)
                            >= process.spec.limits.cancellation_grace
                        {
                            terminate_process(child);
                            break Err(KernelError::Cancelled);
                        }
                    } else {
                        signal_interrupt(pid)?;
                        cancellation_started = Some(Instant::now());
                    }
                }
                if started.elapsed() >= timeout {
                    signal_interrupt(pid)?;
                    terminate_process(child);
                    break Err(KernelError::Timeout);
                }
                match receiver.recv_timeout(Duration::from_millis(5)) {
                    Ok(Ok(event)) => {
                        if event.request_id.as_ref() != Some(&request.request_id) {
                            continue;
                        }
                        if cancellation_started.is_some() && is_exchange_terminal(&event.event) {
                            break Err(KernelError::Cancelled);
                        }
                        if cancellation_started.is_some() {
                            continue;
                        }
                        if let Some(done) = handle(event.event, stdin)? {
                            break Ok(done);
                        }
                    }
                    Ok(Err(error)) => {
                        terminate_process(child);
                        break if cancellation_started.is_some() {
                            Err(KernelError::Cancelled)
                        } else {
                            Err(error)
                        };
                    }
                    Err(RecvTimeoutError::Timeout) => {}
                    Err(RecvTimeoutError::Disconnected) => break Err(KernelError::Crashed),
                }
            };
            drop(receiver);
            reader.join().map_err(|_| KernelError::Crashed)?;
            result
        })
    }
}

impl Drop for KernelBroker {
    fn drop(&mut self) {
        if let Ok(kernels) = self.kernels.get_mut() {
            kernels.clear();
        }
    }
}

fn validate_spec(spec: &KernelSpec, sandbox: &SandboxStatus) -> Result<(), KernelError> {
    if spec.limits.memory_bytes < 16 * 1024 * 1024
        || spec.limits.cpu_seconds == 0
        || spec.limits.max_processes == 0
        || spec.limits.max_output_bytes == 0
        || spec.limits.max_inline_output_bytes == 0
        || spec.limits.max_inline_output_bytes > spec.limits.max_output_bytes
        || spec.limits.max_code_bytes == 0
        || spec.limits.max_snapshot_bytes == 0
        || spec.limits.max_snapshot_bytes > MAX_SNAPSHOT_STATE_BYTES
        || spec.limits.execution_timeout.is_zero()
        || spec.limits.cancellation_grace.is_zero()
        || spec.limits.max_lifetime.is_zero()
        || spec.limits.idle_timeout.is_zero()
    {
        return Err(KernelError::Invalid(
            "kernel limits must be positive and internally bounded".into(),
        ));
    }
    let working = fs::canonicalize(&spec.working_directory)?;
    if !working.is_dir() {
        return Err(KernelError::Invalid(
            "kernel working directory is not a directory".into(),
        ));
    }
    if spec.isolation == KernelIsolation::Untrusted && !sandbox.supports_untrusted() {
        return Err(KernelError::StrongIsolationUnavailable);
    }
    if spec.isolation == KernelIsolation::TrustedLocal && spec.network == KernelNetwork::Denied {
        return Err(KernelError::Invalid(
            "network denial requires a strong isolated kernel profile".into(),
        ));
    }
    if !sandbox.cpu_limit || !sandbox.memory_limit {
        return Err(KernelError::ResourceLimitUnavailable);
    }
    match &spec.runtime {
        KernelRuntime::Python { executable } => validate_executable(executable),
        KernelRuntime::StrictWasm { runtime, module } => {
            validate_executable(runtime)?;
            let module = fs::canonicalize(module)?;
            if !module.is_file() {
                return Err(KernelError::Invalid("WASM module is not a file".into()));
            }
            if spec.isolation != KernelIsolation::Untrusted {
                return Err(KernelError::Invalid(
                    "strict WASM kernels require strong isolation".into(),
                ));
            }
            Ok(())
        }
    }
}

fn validate_executable(path: &Path) -> Result<(), KernelError> {
    let path = fs::canonicalize(path)?;
    if path.is_file() {
        Ok(())
    } else {
        Err(KernelError::Invalid("kernel runtime is not a file".into()))
    }
}

fn build_command(spec: &KernelSpec, sandbox: &SandboxStatus) -> Result<Command, KernelError> {
    let working = fs::canonicalize(&spec.working_directory)?;
    let (runtime, runtime_arguments) = runtime_command(&spec.runtime, &working)?;
    let prlimit = [Path::new("/usr/bin/prlimit"), Path::new("/bin/prlimit")]
        .into_iter()
        .find(|path| path.is_file())
        .ok_or(KernelError::ResourceLimitUnavailable)?;
    let mut limited = vec![
        format!("--as={}", spec.limits.memory_bytes),
        format!("--cpu={}", spec.limits.cpu_seconds),
        format!("--nproc={}", spec.limits.max_processes),
        "--nofile=64".into(),
        "--".into(),
        runtime.to_string_lossy().into_owned(),
    ];
    limited.extend(runtime_arguments);
    if spec.isolation == KernelIsolation::TrustedLocal {
        let mut command = Command::new(prlimit);
        command.args(limited).current_dir(working);
        return Ok(command);
    }
    let launcher = sandbox
        .launcher
        .as_ref()
        .ok_or(KernelError::StrongIsolationUnavailable)?;
    let mut command = Command::new(launcher);
    command.args([
        "--die-with-parent",
        "--new-session",
        "--unshare-all",
        "--ro-bind",
        "/usr",
        "/usr",
    ]);
    for system_path in ["/lib", "/lib64"] {
        if Path::new(system_path).exists() {
            command.args(["--ro-bind", system_path, system_path]);
        }
    }
    command.args(["--bind"]).arg(&working).args([
        "/workspace",
        "--chdir",
        "/workspace",
        "--proc",
        "/proc",
        "--dev",
        "/dev",
        "--tmpfs",
        "/tmp",
    ]);
    if spec.network == KernelNetwork::Allowed {
        command.arg("--share-net");
    }
    command.arg("--").arg(prlimit).args(limited);
    Ok(command)
}

fn runtime_command(
    runtime: &KernelRuntime,
    working_directory: &Path,
) -> Result<(PathBuf, Vec<String>), KernelError> {
    match runtime {
        KernelRuntime::Python { executable } => Ok((
            fs::canonicalize(executable)?,
            vec!["-I".into(), "-u".into(), "-c".into(), PYTHON_GUEST.into()],
        )),
        KernelRuntime::StrictWasm { runtime, module } => {
            let module = fs::canonicalize(module)?;
            let relative_module = module.strip_prefix(working_directory).map_err(|_| {
                KernelError::Invalid(
                    "strict WASM modules must be inside the kernel workspace".into(),
                )
            })?;
            Ok((
                fs::canonicalize(runtime)?,
                vec![
                    "run".into(),
                    "--dir=/workspace".into(),
                    Path::new("/workspace")
                        .join(relative_module)
                        .to_string_lossy()
                        .into_owned(),
                ],
            ))
        }
    }
}

fn compatibility(spec: &KernelSpec) -> Result<KernelCompatibility, KernelError> {
    let runtime = match &spec.runtime {
        KernelRuntime::Python { executable } => {
            format!("python:{}", fs::canonicalize(executable)?.display())
        }
        KernelRuntime::StrictWasm { runtime, module } => format!(
            "wasm:{}:{}:{}",
            fs::canonicalize(runtime)?.display(),
            fs::canonicalize(module)?.display(),
            fs::metadata(module)?.len()
        ),
    };
    Ok(KernelCompatibility {
        protocol: KERNEL_PROTOCOL_VERSION,
        runtime,
        isolation: spec.isolation,
        network: spec.network,
        memory_bytes: spec.limits.memory_bytes,
        cpu_seconds: spec.limits.cpu_seconds,
        max_processes: spec.limits.max_processes,
        workspace: fs::canonicalize(&spec.working_directory)?
            .to_string_lossy()
            .into_owned(),
    })
}

fn runtime_name(runtime: &KernelRuntime) -> &'static str {
    match runtime {
        KernelRuntime::Python { .. } => "python",
        KernelRuntime::StrictWasm { .. } => "strict_wasm",
    }
}

fn ensure_lifetime(process: &KernelProcess, now: UtcTimestamp) -> Result<(), KernelError> {
    let elapsed = elapsed_duration(process.created_at, now);
    if elapsed >= process.spec.limits.max_lifetime {
        Err(KernelError::Timeout)
    } else {
        Ok(())
    }
}

fn idle_or_expired(process: &KernelProcess, now: UtcTimestamp) -> bool {
    let last = UtcTimestamp::from_unix_millis(process.last_used_ms.load(Ordering::Acquire));
    elapsed_duration(last, now) >= process.spec.limits.idle_timeout
        || elapsed_duration(process.created_at, now) >= process.spec.limits.max_lifetime
}

fn elapsed_duration(start: UtcTimestamp, now: UtcTimestamp) -> Duration {
    Duration::from_millis(
        u64::try_from(now.unix_millis().saturating_sub(start.unix_millis())).unwrap_or(0),
    )
}

fn read_event(reader: &mut BufReader<ChildStdout>) -> Result<GuestEvent, KernelError> {
    let mut line = Vec::new();
    let read = reader
        .by_ref()
        .take(MAX_PROTOCOL_LINE_BYTES + 1)
        .read_until(b'\n', &mut line)?;
    if read == 0 {
        return Err(KernelError::Crashed);
    }
    if u64::try_from(read).unwrap_or(u64::MAX) > MAX_PROTOCOL_LINE_BYTES
        || line.last() != Some(&b'\n')
    {
        return Err(KernelError::Invalid(
            "guest protocol frame exceeded its byte limit".into(),
        ));
    }
    serde_json::from_slice(&line).map_err(KernelError::from)
}

fn write_json_line(writer: &mut ChildStdin, value: &impl Serialize) -> Result<(), KernelError> {
    serde_json::to_writer(&mut *writer, value)?;
    writer.write_all(b"\n")?;
    writer.flush()?;
    Ok(())
}

fn write_bridge_reply(writer: &mut ChildStdin, reply: &BridgeReply) -> Result<(), KernelError> {
    let mut bytes = serde_json::to_vec(reply)?;
    if u64::try_from(bytes.len()).unwrap_or(u64::MAX) > MAX_PROTOCOL_LINE_BYTES {
        bytes = serde_json::to_vec(&BridgeReply {
            protocol: reply.protocol,
            request_id: reply.request_id.clone(),
            bridge_id: reply.bridge_id.clone(),
            result: None,
            error: Some(BridgeFailure {
                code: "result_too_large".into(),
                message: "bridge result exceeded its byte limit".into(),
            }),
        })?;
    }
    writer.write_all(&bytes)?;
    writer.write_all(b"\n")?;
    writer.flush()?;
    Ok(())
}

fn is_exchange_terminal(event: &GuestEventKind) -> bool {
    matches!(
        event,
        GuestEventKind::ExecutionFinished { .. }
            | GuestEventKind::Snapshot { .. }
            | GuestEventKind::Restored
            | GuestEventKind::Shutdown
            | GuestEventKind::Error { .. }
    )
}

const fn process_pid(process: &KernelProcess) -> u32 {
    process.pid
}

#[cfg(unix)]
fn configure_process_group(command: &mut Command) {
    use std::os::unix::process::CommandExt;
    command.process_group(0);
}

#[cfg(not(unix))]
fn configure_process_group(_command: &mut Command) {}

#[cfg(unix)]
fn signal_interrupt(pid: u32) -> Result<(), KernelError> {
    use nix::sys::signal::{Signal, killpg};
    use nix::unistd::Pid;
    let pid = i32::try_from(pid).map_err(|_| KernelError::Crashed)?;
    killpg(Pid::from_raw(pid), Signal::SIGINT).map_err(|_| KernelError::Crashed)
}

#[cfg(not(unix))]
fn signal_interrupt(_pid: u32) -> Result<(), KernelError> {
    Err(KernelError::ResourceLimitUnavailable)
}

#[cfg(unix)]
fn terminate_process(child: &mut Child) {
    use nix::sys::signal::{Signal, killpg};
    use nix::unistd::Pid;
    if let Ok(pid) = i32::try_from(child.id()) {
        let _ = killpg(Pid::from_raw(pid), Signal::SIGKILL);
    }
    let _ = child.wait();
}

#[cfg(not(unix))]
fn terminate_process(child: &mut Child) {
    let _ = child.kill();
    let _ = child.wait();
}

fn floor_char_boundary(value: &str, maximum: usize) -> usize {
    let mut boundary = maximum.min(value.len());
    while boundary > 0 && !value.is_char_boundary(boundary) {
        boundary -= 1;
    }
    boundary
}

fn write_snapshot(path: &Path, bytes: &[u8]) -> Result<(), KernelError> {
    let parent = path
        .parent()
        .ok_or_else(|| KernelError::Invalid("snapshot path has no parent".into()))?;
    let temporary = parent.join(format!(".{}.tmp", EntityId::new()));
    let result = (|| {
        let mut options = fs::OpenOptions::new();
        options.create_new(true).write(true);
        let mut file = options.open(&temporary)?;
        restrict_file(&file)?;
        file.write_all(bytes)?;
        file.sync_all()?;
        keith_platform::replace_file(&temporary, path)?;
        fs::File::open(parent)?.sync_all()?;
        Ok(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(temporary);
    }
    result
}

#[cfg(unix)]
fn restrict_directory(path: &Path) -> Result<(), KernelError> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o700))?;
    fs::set_permissions(path.join("snapshots"), fs::Permissions::from_mode(0o700))?;
    Ok(())
}

#[cfg(not(unix))]
fn restrict_directory(_path: &Path) -> Result<(), KernelError> {
    Ok(())
}

#[cfg(unix)]
fn restrict_file(file: &fs::File) -> Result<(), KernelError> {
    use std::os::unix::fs::PermissionsExt;
    file.set_permissions(fs::Permissions::from_mode(0o600))?;
    Ok(())
}

#[cfg(not(unix))]
fn restrict_file(_file: &fs::File) -> Result<(), KernelError> {
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicUsize, Ordering as AtomicOrdering};
    use std::thread;

    use keith_agent_types::{ProfileId, RootTreeId};
    use keith_artifacts::{
        ArtifactLimits, ArtifactReference, ArtifactScope, ArtifactService, ArtifactSource,
        RetentionPolicy,
    };
    use tempfile::TempDir;

    use super::*;

    #[derive(Default)]
    struct RecordingSink {
        chunks: Vec<KernelOutputChunk>,
    }

    impl KernelOutputSink for RecordingSink {
        fn emit(&mut self, chunk: &KernelOutputChunk) {
            self.chunks.push(chunk.clone());
        }
    }

    struct RecordingBridge {
        calls: AtomicUsize,
    }

    impl RecordingBridge {
        const fn new() -> Self {
            Self {
                calls: AtomicUsize::new(0),
            }
        }
    }

    impl BridgeHandler for RecordingBridge {
        fn handle(
            &self,
            _context: &BridgeContext,
            operation: &BridgeOperation,
        ) -> Result<serde_json::Value, BridgeFailure> {
            self.calls.fetch_add(1, AtomicOrdering::Relaxed);
            match operation {
                BridgeOperation::Compact { target_tokens } => {
                    Ok(serde_json::json!({"accepted": true, "target": target_tokens}))
                }
                _ => Err(BridgeFailure {
                    code: "unexpected".into(),
                    message: "unexpected operation in process test".into(),
                }),
            }
        }
    }

    fn python() -> PathBuf {
        fs::canonicalize("/usr/bin/python3").expect("system Python is required for process tests")
    }

    fn limits() -> KernelLimits {
        KernelLimits {
            memory_bytes: 256 * 1024 * 1024,
            cpu_seconds: 5,
            max_processes: 4,
            max_output_bytes: 64 * 1024,
            max_inline_output_bytes: 256,
            max_code_bytes: 64 * 1024,
            max_snapshot_bytes: 40 * 1024,
            max_bridge_calls: 4,
            execution_timeout: Duration::from_secs(2),
            cancellation_grace: Duration::from_millis(100),
            max_lifetime: Duration::from_secs(60),
            idle_timeout: Duration::from_secs(10),
        }
    }

    fn spec(workspace: &TempDir, session_id: &SessionId) -> KernelSpec {
        KernelSpec {
            session_id: session_id.clone(),
            runtime: KernelRuntime::Python {
                executable: python(),
            },
            working_directory: workspace.path().to_path_buf(),
            isolation: KernelIsolation::TrustedLocal,
            network: KernelNetwork::Allowed,
            limits: limits(),
            allowed_bridge: BTreeSet::new(),
        }
    }

    fn broker(root: &TempDir, bridge: Arc<dyn BridgeHandler>) -> KernelBroker {
        KernelBroker::open(root.path(), bridge, None).unwrap()
    }

    #[test]
    fn persistent_python_streams_and_retains_state_across_executions() {
        let root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        let session = SessionId::new();
        let broker = broker(&root, Arc::new(DenyBridge));
        let id = broker
            .start(spec(&workspace, &session), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let mut sink = RecordingSink::default();
        let first = broker
            .execute(
                &id,
                "x = 40\nprint('streamed')",
                &CancellationToken::default(),
                &mut sink,
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert!(first.error.is_none());
        assert!(
            sink.chunks
                .iter()
                .any(|chunk| chunk.text.contains("streamed"))
        );
        let second = broker
            .execute(
                &id,
                "x + 2",
                &CancellationToken::default(),
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert_eq!(second.result, Some(serde_json::json!(42)));
        assert_eq!(second.usage.executions, 2);
    }

    #[test]
    fn interrupt_timeout_cancellation_and_process_crash_are_contained() {
        let root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        let session = SessionId::new();
        let broker = Arc::new(broker(&root, Arc::new(DenyBridge)));
        let id = broker
            .start(spec(&workspace, &session), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let executing_broker = Arc::clone(&broker);
        let executing_id = id.clone();
        let execution = thread::spawn(move || {
            executing_broker.execute(
                &executing_id,
                "while True: pass",
                &CancellationToken::default(),
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(1),
            )
        });
        thread::sleep(Duration::from_millis(50));
        broker.interrupt(&id).unwrap();
        let interrupted = execution.join().unwrap().unwrap();
        assert!(interrupted.error.unwrap().contains("KeyboardInterrupt"));

        let live_cancellation = CancellationToken::default();
        let execution_cancellation = live_cancellation.clone();
        let cancelling_broker = Arc::clone(&broker);
        let cancelling_id = id.clone();
        let cancelling_execution = thread::spawn(move || {
            cancelling_broker.execute(
                &cancelling_id,
                "while True: pass",
                &execution_cancellation,
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(2),
            )
        });
        thread::sleep(Duration::from_millis(50));
        live_cancellation.cancel();
        assert!(matches!(
            cancelling_execution.join().unwrap(),
            Err(KernelError::Cancelled)
        ));
        assert_eq!(
            broker
                .execute(
                    &id,
                    "3 + 4",
                    &CancellationToken::default(),
                    &mut NoKernelOutput,
                    UtcTimestamp::from_unix_millis(3),
                )
                .unwrap()
                .result,
            Some(serde_json::json!(7))
        );

        let cancelled = CancellationToken::default();
        cancelled.cancel();
        assert!(matches!(
            broker.execute(
                &id,
                "1 + 1",
                &cancelled,
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(4)
            ),
            Err(KernelError::Cancelled)
        ));

        let crashed = broker.execute(
            &id,
            "import os; os._exit(17)",
            &CancellationToken::default(),
            &mut NoKernelOutput,
            UtcTimestamp::from_unix_millis(5),
        );
        assert!(matches!(crashed, Err(KernelError::Crashed)));

        let mut short = spec(&workspace, &session);
        short.limits.execution_timeout = Duration::from_millis(50);
        let timed = broker
            .start(short, UtcTimestamp::from_unix_millis(6))
            .unwrap();
        assert!(matches!(
            broker.execute(
                &timed,
                "while True: pass",
                &CancellationToken::default(),
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(5)
            ),
            Err(KernelError::Timeout)
        ));
    }

    #[test]
    fn snapshot_restart_restore_reports_exclusions_and_checks_compatibility() {
        let root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        let session = SessionId::new();
        let snapshot_id;
        {
            let broker = broker(&root, Arc::new(DenyBridge));
            let id = broker
                .start(spec(&workspace, &session), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            broker
                .execute(
                    &id,
                    "answer = 41\nhandle = open('/dev/null', 'w')",
                    &CancellationToken::default(),
                    &mut NoKernelOutput,
                    UtcTimestamp::from_unix_millis(1),
                )
                .unwrap();
            let snapshot = broker
                .snapshot(
                    &id,
                    &CancellationToken::default(),
                    UtcTimestamp::from_unix_millis(2),
                )
                .unwrap();
            assert!(snapshot.excluded.iter().any(|item| item.name == "handle"));
            snapshot_id = snapshot.id;
        }
        let broker = broker(&root, Arc::new(DenyBridge));
        let restored = broker
            .restore(
                &snapshot_id,
                spec(&workspace, &session),
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        let result = broker
            .execute(
                &restored,
                "answer + 1",
                &CancellationToken::default(),
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        assert_eq!(result.result, Some(serde_json::json!(42)));
        let mut incompatible = spec(&workspace, &session);
        incompatible.limits.memory_bytes *= 2;
        assert!(matches!(
            broker.restore(
                &snapshot_id,
                incompatible,
                &CancellationToken::default(),
                UtcTimestamp::from_unix_millis(5)
            ),
            Err(KernelError::IncompatibleSnapshot)
        ));
    }

    #[test]
    fn output_flood_spills_through_the_real_artifact_service() {
        let root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        let artifact_root = TempDir::new().unwrap();
        let session = SessionId::new();
        let scope = ArtifactScope {
            root_tree_id: RootTreeId::new(),
            session_id: session.clone(),
            profile_id: ProfileId::new(),
        };
        let artifacts = Arc::new(
            ArtifactService::open(
                artifact_root.path(),
                ArtifactLimits {
                    max_artifact_bytes: 128 * 1024,
                    max_preview_bytes: 128,
                    max_artifacts_per_tree: 8,
                },
            )
            .unwrap(),
        );
        let spill = artifacts.scoped_spill(
            scope.clone(),
            ArtifactSource::Kernel,
            "text/plain",
            RetentionPolicy::Retain,
        );
        let broker =
            KernelBroker::open(root.path(), Arc::new(DenyBridge), Some(Arc::new(spill))).unwrap();
        let mut kernel_spec = spec(&workspace, &session);
        kernel_spec.limits.max_inline_output_bytes = 64;
        kernel_spec.limits.max_output_bytes = 16 * 1024;
        let id = broker.start(kernel_spec, UtcTimestamp::UNIX_EPOCH).unwrap();
        let execution = broker
            .execute(
                &id,
                "print('x' * 100000)",
                &CancellationToken::default(),
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert_eq!(execution.preview.len(), 64);
        assert_eq!(execution.total_output_bytes, 16 * 1024);
        assert!(execution.output_truncated);
        let spilled = execution.spill.unwrap();
        let reference = ArtifactReference {
            id: spilled.artifact_id,
            root_tree_id: scope.root_tree_id.clone(),
            profile_id: scope.profile_id.clone(),
        };
        let bytes = artifacts.download(&scope, &reference).unwrap();
        assert_eq!(bytes.len(), execution.total_output_bytes);
        assert!(bytes.len() > execution.preview.len());
    }

    #[test]
    fn typed_bridge_enforces_capabilities_before_the_handler() {
        let root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        let session = SessionId::new();
        let handler = Arc::new(RecordingBridge::new());
        let broker = KernelBroker::open(root.path(), handler.clone(), None).unwrap();
        let mut allowed = spec(&workspace, &session);
        allowed.allowed_bridge.insert(BridgeCapability::Compaction);
        let id = broker.start(allowed, UtcTimestamp::UNIX_EPOCH).unwrap();
        let result = broker
            .execute(
                &id,
                "bridge({'kind':'compact','target_tokens':128})['accepted']",
                &CancellationToken::default(),
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert_eq!(result.result, Some(serde_json::json!(true)));
        assert_eq!(handler.calls.load(AtomicOrdering::Relaxed), 1);

        let denied_id = broker
            .start(
                spec(&workspace, &session),
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        let denied = broker
            .execute(
                &denied_id,
                "bridge({'kind':'compact','target_tokens':128})",
                &CancellationToken::default(),
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        assert!(denied.error.unwrap().contains("not allowed"));
        assert_eq!(handler.calls.load(AtomicOrdering::Relaxed), 1);
    }

    #[test]
    fn isolation_resource_profiles_and_idle_reclamation_fail_closed() {
        let root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        let session = SessionId::new();
        let broker = broker(&root, Arc::new(DenyBridge));
        let mut untrusted = spec(&workspace, &session);
        untrusted.isolation = KernelIsolation::Untrusted;
        if !broker.sandbox_status().supports_untrusted() {
            assert!(matches!(
                broker.start(untrusted, UtcTimestamp::UNIX_EPOCH),
                Err(KernelError::StrongIsolationUnavailable)
            ));
        }
        let mut invalid = spec(&workspace, &session);
        invalid.limits.memory_bytes = 1;
        assert!(matches!(
            broker.start(invalid, UtcTimestamp::UNIX_EPOCH),
            Err(KernelError::Invalid(_))
        ));
        let resource_id = broker
            .start(
                spec(&workspace, &session),
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        let memory_limited = broker
            .execute(
                &resource_id,
                "bytearray(512 * 1024 * 1024)",
                &CancellationToken::default(),
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert!(memory_limited.error.unwrap().contains("MemoryError"));
        let environment = broker
            .execute(
                &resource_id,
                "sorted(__import__('os').environ)",
                &CancellationToken::default(),
                &mut NoKernelOutput,
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        let keys = environment.result.unwrap();
        assert!(!keys.as_array().unwrap().iter().any(|key| key == "HOME"));
        let mut idle = spec(&workspace, &session);
        idle.limits.idle_timeout = Duration::from_millis(5);
        let id = broker.start(idle, UtcTimestamp::UNIX_EPOCH).unwrap();
        let evicted = broker
            .evict_idle(UtcTimestamp::from_unix_millis(5))
            .unwrap();
        assert_eq!(evicted.as_slice(), std::slice::from_ref(&id));
        assert!(matches!(broker.inspect(&id), Err(KernelError::NotFound(_))));

        let strict_wasm = KernelSpec {
            runtime: KernelRuntime::StrictWasm {
                runtime: PathBuf::from("/missing/wasmtime"),
                module: PathBuf::from("/missing/kernel.wasm"),
            },
            isolation: KernelIsolation::Untrusted,
            ..spec(&workspace, &session)
        };
        assert!(broker.start(strict_wasm, UtcTimestamp::UNIX_EPOCH).is_err());
    }
}
