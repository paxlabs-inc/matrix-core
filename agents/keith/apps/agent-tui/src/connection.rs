use std::collections::{BTreeMap, BTreeSet};
use std::ffi::OsString;
use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::mpsc::{self, Receiver, SyncSender, TryRecvError, TrySendError};
use std::thread;
use std::thread::JoinHandle;
use std::time::{Duration, Instant};

use keith_agent_types::{CURRENT_PROTOCOL_VERSION, ClientId, ProtocolVersion};
use keith_connection::{AgentTransport, FramedTransport, WebSocketTransport};
use keith_platform::PlatformPaths;
use keith_protocol::{
    ClientHello, CommandEnvelope, CommandResultEnvelope, Feature, ResumeCursor, ServerHello,
    WireFormat, WireMessage,
};
use thiserror::Error;
use tungstenite::client::IntoClientRequest;
use tungstenite::http::HeaderValue;
use tungstenite::stream::MaybeTlsStream;
use url::Url;

const MAX_REMOTE_MESSAGE_BYTES: usize = 8 * 1_024 * 1_024;
const MAX_DISPATCH_EVENTS: usize = 512;
const DISPATCH_POLL_INTERVAL: Duration = Duration::from_millis(75);

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SupervisedLocalConfig {
    pub daemon_executable: PathBuf,
    pub worker_executable: PathBuf,
    pub data_root: PathBuf,
    pub socket_path: PathBuf,
    pub idle_seconds: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ConnectionMode {
    Attach { socket_path: PathBuf },
    SupervisedLocal(SupervisedLocalConfig),
    Remote { url: Url, bearer_token: String },
}

#[derive(Debug, Error)]
pub enum TuiConnectionError {
    #[error("connection setup is invalid: {0}")]
    Invalid(String),
    #[error("agent transport failed: {0}")]
    Transport(#[from] keith_connection::ConnectionError),
    #[error("supervised daemon failed to start: {0}")]
    DaemonStart(String),
    #[error("remote WebSocket failed: {0}")]
    WebSocket(#[from] tungstenite::Error),
    #[error("remote authorization header is invalid")]
    InvalidAuthorization,
    #[error("server did not return a protocol hello")]
    MissingServerHello,
    #[error("server returned a message for another command")]
    UnexpectedCommandResult,
}

type BoxedTransport = Box<dyn AgentTransport + Send>;

const MAX_DISPATCHED_COMMANDS: usize = 128;

pub struct AgentConnectionClient {
    mode: ConnectionMode,
    transport: BoxedTransport,
    supervised_daemon: Option<Child>,
    pub client_id: ClientId,
    pub server: ServerHello,
}

impl AgentConnectionClient {
    /// Opens and negotiates a bounded local, supervised-local, or authenticated remote connection.
    ///
    /// # Errors
    ///
    /// Returns an error when startup, authentication, transport, or negotiation fails.
    pub fn connect(
        mode: ConnectionMode,
        client_id: ClientId,
        resume: Option<ResumeCursor>,
        startup_timeout: Duration,
    ) -> Result<Self, TuiConnectionError> {
        let (mut transport, supervised_daemon) = open_transport(&mode, startup_timeout)?;
        transport.send(&WireMessage::ClientHello(client_hello(
            client_id.clone(),
            resume,
        )))?;
        let deadline = Instant::now() + startup_timeout;
        let server = loop {
            match transport.receive() {
                Ok(WireMessage::ServerHello(server)) => break server,
                Ok(_) => return Err(TuiConnectionError::MissingServerHello),
                Err(error) if error.is_timed_out() && Instant::now() < deadline => {}
                Err(error) => return Err(error.into()),
            }
        };
        Ok(Self {
            mode,
            transport,
            supervised_daemon,
            client_id,
            server,
        })
    }

    /// Reopens the same endpoint with the same client identity and a durable resume cursor.
    ///
    /// # Errors
    ///
    /// Returns an error when the replacement connection cannot negotiate.
    pub fn reconnect(
        &mut self,
        resume: Option<ResumeCursor>,
        startup_timeout: Duration,
    ) -> Result<(), TuiConnectionError> {
        let mut replacement = Self::connect(
            self.mode.clone(),
            self.client_id.clone(),
            resume,
            startup_timeout,
        )?;
        std::mem::swap(self, &mut replacement);
        Ok(())
    }

    /// Sends one command and consumes ordered events until its matching result arrives.
    ///
    /// # Errors
    ///
    /// Returns an error for transport closure or a mismatched result.
    pub fn execute(
        &mut self,
        mut command: CommandEnvelope,
        mut on_message: impl FnMut(WireMessage),
    ) -> Result<CommandResultEnvelope, TuiConnectionError> {
        command.protocol = self.server.protocol;
        let command_id = command.command_id.clone();
        self.transport.send(&WireMessage::Command(command))?;
        loop {
            match self.transport.receive() {
                Ok(WireMessage::CommandResult(result)) if result.command_id == command_id => {
                    return Ok(result);
                }
                Ok(WireMessage::CommandResult(_)) => {
                    return Err(TuiConnectionError::UnexpectedCommandResult);
                }
                Ok(
                    message @ (WireMessage::Event(_)
                    | WireMessage::Snapshot(_)
                    | WireMessage::Terminal(_)),
                ) => on_message(message),
                Ok(
                    WireMessage::ServerHello(_)
                    | WireMessage::ClientHello(_)
                    | WireMessage::Command(_),
                ) => {}
                Err(error) if error.is_timed_out() => {}
                Err(error) => return Err(error.into()),
            }
        }
    }

    fn send_command(&mut self, mut command: CommandEnvelope) -> Result<(), TuiConnectionError> {
        command.protocol = self.server.protocol;
        self.transport.send(&WireMessage::Command(command))?;
        Ok(())
    }

    fn receive_next(&mut self) -> Result<WireMessage, TuiConnectionError> {
        self.transport.receive().map_err(TuiConnectionError::from)
    }

    pub const fn protocol(&self) -> ProtocolVersion {
        self.server.protocol
    }
}

impl Drop for AgentConnectionClient {
    fn drop(&mut self) {
        if let Some(child) = &mut self.supervised_daemon {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

#[derive(Debug)]
pub enum DispatchEvent {
    Message(Box<WireMessage>),
    CommandFailed(String),
    Reconnecting,
    Reconnected,
    ReconnectFailed(String),
}

pub struct AgentCommandDispatcher {
    supervised_daemon: Option<Child>,
    commands: Option<SyncSender<CommandEnvelope>>,
    event_sender: SyncSender<DispatchEvent>,
    events: Receiver<DispatchEvent>,
    dispatch_mode: ConnectionMode,
    client_id: ClientId,
    startup_timeout: Duration,
    shutdown: Arc<AtomicBool>,
    worker: Option<JoinHandle<()>>,
    priority_workers: Vec<JoinHandle<()>>,
}

impl AgentCommandDispatcher {
    pub fn new(mut owner: AgentConnectionClient, startup_timeout: Duration) -> Self {
        let dispatch_mode = parallel_mode(&owner.mode);
        owner.mode.clone_from(&dispatch_mode);
        let client_id = owner.client_id.clone();
        let supervised_daemon = owner.supervised_daemon.take();
        let (command_sender, command_receiver) =
            mpsc::sync_channel::<CommandEnvelope>(MAX_DISPATCHED_COMMANDS);
        let (event_sender, event_receiver) = mpsc::sync_channel(MAX_DISPATCH_EVENTS);
        let worker_events = event_sender.clone();
        let shutdown = Arc::new(AtomicBool::new(false));
        let worker_shutdown = Arc::clone(&shutdown);
        let worker = thread::spawn(move || {
            command_worker(
                owner,
                startup_timeout,
                &command_receiver,
                &worker_events,
                &worker_shutdown,
            );
        });
        Self {
            supervised_daemon,
            commands: Some(command_sender),
            event_sender,
            events: event_receiver,
            dispatch_mode,
            client_id,
            startup_timeout,
            shutdown,
            worker: Some(worker),
            priority_workers: Vec::new(),
        }
    }

    /// Enqueues an ordinary command or starts a priority cancellation connection.
    ///
    /// # Errors
    ///
    /// Returns an error when the bounded dispatcher is full or unavailable.
    pub fn dispatch(&mut self, command: CommandEnvelope) -> Result<(), String> {
        self.reap_priority_workers();
        if matches!(command.command, keith_protocol::ClientCommand::Cancel(_)) {
            let mode = self.dispatch_mode.clone();
            let client_id = self.client_id.clone();
            let startup_timeout = self.startup_timeout;
            let events = self.event_sender.clone();
            self.priority_workers.push(thread::spawn(
                move || match AgentConnectionClient::connect(mode, client_id, None, startup_timeout)
                {
                    Ok(mut client) => {
                        if let Err(error) = forward_command(&mut client, command, &events) {
                            let _ = events.send(DispatchEvent::CommandFailed(error.to_string()));
                        }
                    }
                    Err(error) => {
                        let _ = events.send(DispatchEvent::CommandFailed(error.to_string()));
                    }
                },
            ));
            return Ok(());
        }
        let Some(commands) = &self.commands else {
            return Err("command dispatcher is closed".into());
        };
        commands.try_send(command).map_err(|error| match error {
            TrySendError::Full(_) => "command dispatcher queue is full".to_owned(),
            TrySendError::Disconnected(_) => "command dispatcher is unavailable".to_owned(),
        })
    }

    pub fn try_next(&self) -> Option<DispatchEvent> {
        self.events.try_recv().ok()
    }

    fn reap_priority_workers(&mut self) {
        let mut index = 0;
        while index < self.priority_workers.len() {
            if self.priority_workers[index].is_finished() {
                let worker = self.priority_workers.swap_remove(index);
                let _ = worker.join();
            } else {
                index += 1;
            }
        }
    }
}

impl Drop for AgentCommandDispatcher {
    fn drop(&mut self) {
        self.shutdown.store(true, Ordering::Release);
        self.commands.take();
        self.reap_priority_workers();
        if let Some(worker) = self.worker.take() {
            let _ = worker.join();
        }
        if let Some(child) = &mut self.supervised_daemon {
            let _ = child.kill();
            let _ = child.wait();
        }
    }
}

fn parallel_mode(mode: &ConnectionMode) -> ConnectionMode {
    match mode {
        ConnectionMode::SupervisedLocal(config) => ConnectionMode::Attach {
            socket_path: config.socket_path.clone(),
        },
        mode => mode.clone(),
    }
}

fn command_worker(
    mut client: AgentConnectionClient,
    startup_timeout: Duration,
    commands: &Receiver<CommandEnvelope>,
    events: &SyncSender<DispatchEvent>,
    shutdown: &AtomicBool,
) {
    let mut pending = BTreeMap::new();
    loop {
        loop {
            match commands.try_recv() {
                Ok(command) => {
                    let command_id = command.command_id.clone();
                    pending.insert(command_id.clone(), command.clone());
                    match client.send_command(command) {
                        Ok(()) => {}
                        Err(error) => {
                            let _ = events.send(DispatchEvent::CommandFailed(error.to_string()));
                            if !recover_connection(
                                &mut client,
                                startup_timeout,
                                &mut pending,
                                events,
                                shutdown,
                            ) {
                                return;
                            }
                        }
                    }
                }
                Err(TryRecvError::Empty) => break,
                Err(TryRecvError::Disconnected) => return,
            }
        }
        match client.receive_next() {
            Ok(message) => {
                if let WireMessage::CommandResult(result) = &message {
                    pending.remove(&result.command_id);
                }
                if events
                    .send(DispatchEvent::Message(Box::new(message)))
                    .is_err()
                {
                    return;
                }
            }
            Err(TuiConnectionError::Transport(error)) if error.is_timed_out() => {}
            Err(error) => {
                if !recover_connection(&mut client, startup_timeout, &mut pending, events, shutdown)
                {
                    let _ = events.send(DispatchEvent::ReconnectFailed(error.to_string()));
                    return;
                }
            }
        }
    }
}

fn recover_connection(
    client: &mut AgentConnectionClient,
    startup_timeout: Duration,
    pending: &mut BTreeMap<keith_agent_types::CommandId, CommandEnvelope>,
    events: &SyncSender<DispatchEvent>,
    shutdown: &AtomicBool,
) -> bool {
    for _ in pending.values() {
        let _ = events.send(DispatchEvent::CommandFailed(
            "Connection changed before the command result; retrying the same command identity"
                .into(),
        ));
    }
    if events.send(DispatchEvent::Reconnecting).is_err() {
        return false;
    }
    loop {
        if shutdown.load(Ordering::Acquire) {
            return false;
        }
        match client.reconnect(None, startup_timeout) {
            Ok(()) => {
                let replayed = pending
                    .values()
                    .cloned()
                    .all(|command| client.send_command(command).is_ok());
                if replayed {
                    return events.send(DispatchEvent::Reconnected).is_ok();
                }
            }
            Err(error) => {
                if events
                    .send(DispatchEvent::ReconnectFailed(error.to_string()))
                    .is_err()
                {
                    return false;
                }
                for _ in 0..10 {
                    if shutdown.load(Ordering::Acquire) {
                        return false;
                    }
                    thread::sleep(Duration::from_millis(50));
                }
            }
        }
    }
}

fn forward_command(
    client: &mut AgentConnectionClient,
    command: CommandEnvelope,
    events: &SyncSender<DispatchEvent>,
) -> Result<(), TuiConnectionError> {
    let result = client.execute(command, |message| {
        let _ = events.send(DispatchEvent::Message(Box::new(message)));
    })?;
    let _ = events.send(DispatchEvent::Message(Box::new(
        WireMessage::CommandResult(result),
    )));
    Ok(())
}

fn open_transport(
    mode: &ConnectionMode,
    startup_timeout: Duration,
) -> Result<(BoxedTransport, Option<Child>), TuiConnectionError> {
    match mode {
        ConnectionMode::Attach { socket_path } => Ok((open_local(socket_path)?, None)),
        ConnectionMode::SupervisedLocal(config) => {
            if startup_timeout.is_zero() {
                return Err(TuiConnectionError::Invalid(
                    "startup timeout must be non-zero".into(),
                ));
            }
            std::fs::create_dir_all(&config.data_root)
                .map_err(|error| TuiConnectionError::DaemonStart(error.to_string()))?;
            let mut child = Command::new(&config.daemon_executable)
                .arg("--data-root")
                .arg(&config.data_root)
                .arg("--socket")
                .arg(&config.socket_path)
                .arg("--worker-executable")
                .arg(&config.worker_executable)
                .arg("--idle-seconds")
                .arg(config.idle_seconds.to_string())
                .stdin(Stdio::null())
                .stdout(Stdio::null())
                .stderr(Stdio::null())
                .spawn()
                .map_err(|error| TuiConnectionError::DaemonStart(error.to_string()))?;
            let deadline = Instant::now() + startup_timeout;
            loop {
                match open_local(&config.socket_path) {
                    Ok(transport) => return Ok((transport, Some(child))),
                    Err(error) if Instant::now() < deadline => {
                        let _ = error;
                    }
                    Err(error) => {
                        let _ = child.kill();
                        let _ = child.wait();
                        return Err(error);
                    }
                }
                if let Some(status) = child
                    .try_wait()
                    .map_err(|error| TuiConnectionError::DaemonStart(error.to_string()))?
                {
                    return Err(TuiConnectionError::DaemonStart(format!(
                        "daemon exited with {status}"
                    )));
                }
                if Instant::now() >= deadline {
                    let _ = child.kill();
                    let _ = child.wait();
                    return Err(TuiConnectionError::DaemonStart(
                        "daemon startup timed out".into(),
                    ));
                }
                thread::sleep(Duration::from_millis(20));
            }
        }
        ConnectionMode::Remote { url, bearer_token } => {
            if !matches!(url.scheme(), "ws" | "wss")
                || bearer_token.is_empty()
                || bearer_token.chars().any(char::is_control)
            {
                return Err(TuiConnectionError::Invalid(
                    "remote mode requires ws/wss and a non-empty bearer token".into(),
                ));
            }
            let mut request = url
                .as_str()
                .into_client_request()
                .map_err(TuiConnectionError::WebSocket)?;
            let authorization = HeaderValue::from_str(&format!("Bearer {bearer_token}"))
                .map_err(|_| TuiConnectionError::InvalidAuthorization)?;
            request.headers_mut().insert("authorization", authorization);
            let (mut socket, _) = tungstenite::connect(request)?;
            match socket.get_mut() {
                MaybeTlsStream::Plain(stream) => {
                    stream
                        .set_read_timeout(Some(DISPATCH_POLL_INTERVAL))
                        .map_err(keith_connection::ConnectionError::from)?;
                }
                MaybeTlsStream::Rustls(stream) => {
                    stream
                        .sock
                        .set_read_timeout(Some(DISPATCH_POLL_INTERVAL))
                        .map_err(keith_connection::ConnectionError::from)?;
                }
                _ => {}
            }
            Ok((
                Box::new(WebSocketTransport::new(
                    socket,
                    WireFormat::Json,
                    MAX_REMOTE_MESSAGE_BYTES,
                )),
                None,
            ))
        }
    }
}

fn open_local(path: &Path) -> Result<BoxedTransport, TuiConnectionError> {
    let stream = keith_connection::connect_local(path)?;
    keith_connection::set_local_read_timeout(&stream, Some(DISPATCH_POLL_INTERVAL))
        .map_err(keith_connection::ConnectionError::from)?;
    Ok(Box::new(FramedTransport::new(stream, WireFormat::Json)))
}

fn client_hello(client_id: ClientId, resume: Option<ResumeCursor>) -> ClientHello {
    ClientHello {
        protocol: CURRENT_PROTOCOL_VERSION,
        client_id,
        client_name: "agent-tui".into(),
        client_version: env!("CARGO_PKG_VERSION").into(),
        supported_features: BTreeSet::from([
            Feature::SessionLifecycle,
            Feature::Branching,
            Feature::Steering,
            Feature::Goals,
            Feature::Children,
            Feature::Schedules,
            Feature::MemoryQueries,
            Feature::Confirmations,
            Feature::Export,
            Feature::BackgroundControls,
            Feature::SelfEvolution,
            Feature::Replay,
            Feature::Snapshots,
            Feature::FramedJson,
            Feature::WebSocket,
        ]),
        resume,
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TuiArguments {
    pub mode: ConnectionMode,
    pub session_id: Option<keith_agent_types::SessionId>,
    pub color_mode: crate::ColorMode,
    pub reduced_motion: bool,
    pub startup_timeout: Duration,
}

impl TuiArguments {
    /// # Errors
    ///
    /// Returns a safe message for unknown, missing, or incompatible arguments.
    #[allow(clippy::too_many_lines)]
    pub fn parse<I, S>(arguments: I) -> Result<Option<Self>, String>
    where
        I: IntoIterator<Item = S>,
        S: Into<OsString>,
    {
        let mut arguments = arguments.into_iter().map(Into::into);
        let program = arguments
            .next()
            .unwrap_or_else(|| OsString::from("agent-tui"));
        let mut socket = None;
        let mut remote = None;
        let mut token_env = None;
        let mut data_root = None;
        let mut daemon = None;
        let mut worker = None;
        let mut session_id = None;
        let mut color_mode = crate::ColorMode::TrueColor;
        let mut reduced_motion = false;
        let mut startup_timeout = Duration::from_secs(10);
        while let Some(argument) = arguments.next() {
            let argument = argument
                .into_string()
                .map_err(|_| "arguments must be UTF-8".to_owned())?;
            if matches!(argument.as_str(), "--version" | "-V") {
                println!("agent-tui {}", env!("CARGO_PKG_VERSION"));
                return Ok(None);
            }
            if matches!(argument.as_str(), "--help" | "-h") {
                print_help(&program);
                return Ok(None);
            }
            if argument == "--reduced-motion" {
                reduced_motion = true;
                continue;
            }
            let value = arguments
                .next()
                .ok_or_else(|| format!("missing value for {argument}"))?;
            match argument.as_str() {
                "--socket" => socket = Some(PathBuf::from(value)),
                "--remote" => {
                    remote = Some(
                        value
                            .into_string()
                            .map_err(|_| "remote URL must be UTF-8".to_owned())?,
                    );
                }
                "--token-env" => {
                    token_env = Some(
                        value
                            .into_string()
                            .map_err(|_| "token environment name must be UTF-8".to_owned())?,
                    );
                }
                "--data-root" => data_root = Some(PathBuf::from(value)),
                "--daemon-executable" => daemon = Some(PathBuf::from(value)),
                "--worker-executable" => worker = Some(PathBuf::from(value)),
                "--session" => {
                    let value = value
                        .into_string()
                        .map_err(|_| "session ID must be UTF-8".to_owned())?;
                    session_id = Some(value.parse().map_err(|_| "invalid session ID".to_owned())?);
                }
                "--color" => {
                    color_mode = match value.to_string_lossy().as_ref() {
                        "truecolor" => crate::ColorMode::TrueColor,
                        "256" => crate::ColorMode::Ansi256,
                        "none" => crate::ColorMode::NoColor,
                        "contrast" => crate::ColorMode::HighContrast,
                        _ => return Err("color must be truecolor, 256, none, or contrast".into()),
                    };
                }
                "--startup-timeout-ms" => {
                    let millis = value
                        .to_string_lossy()
                        .parse::<u64>()
                        .map_err(|_| "startup timeout must be an integer".to_owned())?;
                    startup_timeout = Duration::from_millis(millis);
                }
                _ => return Err(format!("unknown argument {argument}")),
            }
        }
        if startup_timeout.is_zero() {
            return Err("startup timeout must be non-zero".into());
        }
        let mode = if let Some(remote) = remote {
            if socket.is_some() || data_root.is_some() {
                return Err("remote mode cannot be combined with local modes".into());
            }
            let token_env =
                token_env.ok_or_else(|| "--token-env is required remotely".to_owned())?;
            let bearer_token = std::env::var(&token_env)
                .map_err(|_| format!("token environment variable {token_env} is unavailable"))?;
            ConnectionMode::Remote {
                url: Url::parse(&remote).map_err(|_| "invalid remote URL".to_owned())?,
                bearer_token,
            }
        } else if let Some(data_root) = data_root {
            let socket_path = socket.unwrap_or_else(|| data_root.join("agentd.sock"));
            let daemon_executable = daemon.unwrap_or_else(|| sibling_binary(&program, "agentd"));
            let worker_executable =
                worker.unwrap_or_else(|| sibling_binary(&program, "agent-worker"));
            ConnectionMode::SupervisedLocal(SupervisedLocalConfig {
                daemon_executable,
                worker_executable,
                data_root,
                socket_path,
                idle_seconds: 15 * 60,
            })
        } else if let Some(socket_path) = socket {
            ConnectionMode::Attach { socket_path }
        } else {
            let paths = PlatformPaths::discover().map_err(|error| error.to_string())?;
            ConnectionMode::SupervisedLocal(SupervisedLocalConfig {
                daemon_executable: daemon.unwrap_or_else(|| sibling_binary(&program, "agentd")),
                worker_executable: worker
                    .unwrap_or_else(|| sibling_binary(&program, "agent-worker")),
                data_root: paths.data_root,
                socket_path: paths.daemon_endpoint,
                idle_seconds: 15 * 60,
            })
        };
        Ok(Some(Self {
            mode,
            session_id,
            color_mode,
            reduced_motion,
            startup_timeout,
        }))
    }
}

fn print_help(program: &OsString) {
    let executable = Path::new(program)
        .file_name()
        .unwrap_or(program.as_os_str())
        .to_string_lossy();
    println!(
        "Keith terminal agent\n\nUsage: {executable} [OPTIONS]\n\nOptions:\n  --socket <PATH>                Attach to a running Keith daemon\n  --session <ID>                 Open a specific conversation\n  --data-root <PATH>             Start and supervise a local Keith daemon\n  --daemon-executable <PATH>     Override the supervised daemon binary\n  --worker-executable <PATH>     Override the supervised worker binary\n  --remote <WS_URL>              Connect to an authenticated remote daemon\n  --token-env <NAME>             Read the remote bearer token from this variable\n  --color <MODE>                 truecolor, 256, none, or contrast\n  --reduced-motion               Disable animated activity indicators\n  --startup-timeout-ms <MS>      Connection startup timeout\n  -h, --help                     Print help\n  -V, --version                  Print version"
    );
}

fn sibling_binary(program: &OsString, name: &str) -> PathBuf {
    let mut path = PathBuf::from(program);
    path.set_file_name(name);
    path
}

#[cfg(test)]
mod tests {
    use std::net::TcpListener;
    use std::sync::mpsc;
    use std::sync::{Arc, Mutex};

    use keith_agent_types::{
        CommandId, CommonError, EntityId, ErrorCode, Generation, ProfileId, RootTreeId, Sequence,
        UtcTimestamp,
    };
    use keith_protocol::{
        ClientCommand, CommandResult, DaemonEvent, EventEnvelope, ResponsePayload, SessionFilter,
        negotiate,
    };

    use super::*;

    fn server_journey(transport: &mut impl AgentTransport) {
        let WireMessage::ClientHello(hello) = transport.receive().unwrap() else {
            panic!("client hello required");
        };
        let server = negotiate(
            &hello,
            CURRENT_PROTOCOL_VERSION,
            EntityId::new(),
            &hello.supported_features,
        )
        .unwrap();
        transport.send(&WireMessage::ServerHello(server)).unwrap();
        let WireMessage::Command(command) = transport.receive().unwrap() else {
            panic!("command required");
        };
        transport
            .send(&WireMessage::Event(EventEnvelope {
                protocol: CURRENT_PROTOCOL_VERSION,
                root_tree_id: RootTreeId::new(),
                generation: Generation::new(1),
                first_sequence: Sequence::new(1),
                sequence: Sequence::new(1),
                occurred_at: UtcTimestamp::UNIX_EPOCH,
                event: DaemonEvent::Warning(CommonError::new(
                    ErrorCode::Unavailable,
                    "recovered event",
                    true,
                )),
            }))
            .unwrap();
        transport
            .send(&WireMessage::CommandResult(CommandResultEnvelope {
                protocol: CURRENT_PROTOCOL_VERSION,
                command_id: command.command_id,
                completed_at: UtcTimestamp::UNIX_EPOCH,
                result: CommandResult::Data(Box::new(ResponsePayload::Sessions(Vec::new()))),
            }))
            .unwrap();
    }

    fn list_command(client_id: ClientId) -> CommandEnvelope {
        CommandEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            client_id,
            sent_at: UtcTimestamp::UNIX_EPOCH,
            session_id: None,
            command: ClientCommand::ListSessions(SessionFilter::default()),
        }
    }

    fn negotiate_local_client(
        stream: keith_connection::LocalStream,
    ) -> FramedTransport<keith_connection::LocalStream> {
        let mut transport = FramedTransport::new(stream, WireFormat::Json);
        let WireMessage::ClientHello(hello) = transport.receive().unwrap() else {
            panic!("client hello required");
        };
        let server = negotiate(
            &hello,
            CURRENT_PROTOCOL_VERSION,
            EntityId::new(),
            &hello.supported_features,
        )
        .unwrap();
        transport.send(&WireMessage::ServerHello(server)).unwrap();
        transport
    }

    fn complete_command(transport: &mut impl AgentTransport, command_id: CommandId) {
        transport
            .send(&WireMessage::CommandResult(CommandResultEnvelope {
                protocol: CURRENT_PROTOCOL_VERSION,
                command_id,
                completed_at: UtcTimestamp::UNIX_EPOCH,
                result: CommandResult::Data(Box::new(ResponsePayload::Sessions(Vec::new()))),
            }))
            .unwrap();
    }

    #[test]
    fn local_attach_and_reconnect_run_the_real_protocol_twice() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("agent.sock");
        let listener = keith_connection::bind_permissioned_local(&path).unwrap();
        let server = thread::spawn(move || {
            for _ in 0..2 {
                let stream = keith_connection::accept_local(&listener).unwrap();
                server_journey(&mut FramedTransport::new(stream, WireFormat::Json));
            }
        });
        let client_id = ClientId::new();
        let mut client = AgentConnectionClient::connect(
            ConnectionMode::Attach { socket_path: path },
            client_id.clone(),
            None,
            Duration::from_secs(1),
        )
        .unwrap();
        let mut recovered = Vec::new();
        let result = client
            .execute(list_command(client_id.clone()), |message| {
                recovered.push(message);
            })
            .unwrap();
        assert!(matches!(
            recovered.as_slice(),
            [WireMessage::Event(EventEnvelope {
                event: DaemonEvent::Warning(error),
                ..
            })] if error.message == "recovered event"
        ));
        assert!(matches!(
            result.result,
            CommandResult::Data(payload)
                if matches!(*payload, ResponsePayload::Sessions(ref sessions) if sessions.is_empty())
        ));
        client.reconnect(None, Duration::from_secs(1)).unwrap();
        client.execute(list_command(client_id), |_| {}).unwrap();
        drop(client);
        server.join().unwrap();
    }

    #[test]
    fn local_transport_preserves_profile_scoped_integration_commands_and_projections() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("agent.sock");
        let listener = keith_connection::bind_permissioned_local(&path).unwrap();
        let profile_id = ProfileId::new();
        let expected_profile = profile_id.clone();
        let server = thread::spawn(move || {
            let stream = keith_connection::accept_local(&listener).unwrap();
            let mut transport = negotiate_local_client(stream);
            let WireMessage::Command(command) = transport.receive().unwrap() else {
                panic!("integration command required");
            };
            assert!(matches!(
                command.command,
                ClientCommand::Integration(keith_protocol::IntegrationCommand::List {
                    profile_id,
                    service: None,
                }) if profile_id == expected_profile
            ));
            transport
                .send(&WireMessage::CommandResult(CommandResultEnvelope {
                    protocol: CURRENT_PROTOCOL_VERSION,
                    command_id: command.command_id,
                    completed_at: UtcTimestamp::UNIX_EPOCH,
                    result: CommandResult::Data(Box::new(ResponsePayload::ProfileIntegrations(
                        Box::new(keith_protocol::ProfileIntegrationsProjection {
                            profile_id: expected_profile,
                            through_sequence: Sequence::new(2),
                            services: vec![keith_protocol::IntegrationServiceProjection {
                                service: keith_protocol::IntegrationService::ConnectedApp,
                                availability:
                                    keith_protocol::IntegrationAvailabilityProjection::Available,
                            }],
                            resources: Vec::new(),
                        }),
                    ))),
                }))
                .unwrap();
        });

        let client_id = ClientId::new();
        let mut client = AgentConnectionClient::connect(
            ConnectionMode::Attach { socket_path: path },
            client_id.clone(),
            None,
            Duration::from_secs(1),
        )
        .unwrap();
        let result = client
            .execute(
                CommandEnvelope {
                    protocol: CURRENT_PROTOCOL_VERSION,
                    command_id: CommandId::new(),
                    client_id,
                    sent_at: UtcTimestamp::UNIX_EPOCH,
                    session_id: None,
                    command: ClientCommand::Integration(keith_protocol::IntegrationCommand::List {
                        profile_id: profile_id.clone(),
                        service: None,
                    }),
                },
                |_| {},
            )
            .unwrap();
        assert!(matches!(
            result.result,
            CommandResult::Data(payload)
                if matches!(payload.as_ref(),
                    ResponsePayload::ProfileIntegrations(projection)
                        if projection.profile_id == profile_id
                            && projection.through_sequence == Sequence::new(2))
        ));
        drop(client);
        server.join().unwrap();
    }

    #[test]
    fn dispatcher_keeps_input_responsive_and_sends_cancel_while_a_command_is_in_flight() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("agent.sock");
        let listener = keith_connection::bind_permissioned_local(&path).unwrap();
        let (primary_received_sender, primary_received_receiver) = mpsc::channel();
        let (cancel_received_sender, cancel_received_receiver) = mpsc::channel();
        let (release_sender, release_receiver) = mpsc::channel();
        let server = thread::spawn(move || {
            let owner_stream = keith_connection::accept_local(&listener).unwrap();
            let primary = thread::spawn(move || {
                let mut transport = negotiate_local_client(owner_stream);
                let WireMessage::Command(command) = transport.receive().unwrap() else {
                    panic!("primary command required");
                };
                primary_received_sender.send(()).unwrap();
                release_receiver
                    .recv_timeout(Duration::from_secs(2))
                    .unwrap();
                complete_command(&mut transport, command.command_id);
            });

            let cancel_stream = keith_connection::accept_local(&listener).unwrap();
            let mut cancel_transport = negotiate_local_client(cancel_stream);
            let WireMessage::Command(command) = cancel_transport.receive().unwrap() else {
                panic!("cancel command required");
            };
            assert!(matches!(command.command, ClientCommand::Cancel(_)));
            cancel_received_sender.send(()).unwrap();
            complete_command(&mut cancel_transport, command.command_id);
            primary.join().unwrap();
        });

        let client_id = ClientId::new();
        let owner = AgentConnectionClient::connect(
            ConnectionMode::Attach { socket_path: path },
            client_id.clone(),
            None,
            Duration::from_secs(1),
        )
        .unwrap();
        let mut dispatcher = AgentCommandDispatcher::new(owner, Duration::from_secs(1));
        let started = Instant::now();
        dispatcher
            .dispatch(list_command(client_id.clone()))
            .unwrap();
        assert!(started.elapsed() < Duration::from_millis(250));
        primary_received_receiver
            .recv_timeout(Duration::from_secs(1))
            .unwrap();

        let session_id = keith_agent_types::SessionId::new();
        let cancel = CommandEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            client_id,
            sent_at: UtcTimestamp::UNIX_EPOCH,
            session_id: Some(session_id.clone()),
            command: ClientCommand::Cancel(keith_protocol::CancelTarget::Session(session_id)),
        };
        let started = Instant::now();
        dispatcher.dispatch(cancel).unwrap();
        assert!(started.elapsed() < Duration::from_millis(250));
        cancel_received_receiver
            .recv_timeout(Duration::from_secs(1))
            .unwrap();
        release_sender.send(()).unwrap();

        let deadline = Instant::now() + Duration::from_secs(2);
        let mut completed = 0;
        while completed < 2 && Instant::now() < deadline {
            if let Some(DispatchEvent::Message(message)) = dispatcher.try_next()
                && matches!(*message, WireMessage::CommandResult(_))
            {
                completed += 1;
            } else {
                thread::sleep(Duration::from_millis(5));
            }
        }
        assert_eq!(completed, 2);
        drop(dispatcher);
        server.join().unwrap();
    }

    #[test]
    fn dispatcher_delivers_events_while_no_command_is_in_flight() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("agent.sock");
        let listener = keith_connection::bind_permissioned_local(&path).unwrap();
        let (release_sender, release_receiver) = mpsc::channel();
        let server = thread::spawn(move || {
            let stream = keith_connection::accept_local(&listener).unwrap();
            let mut transport = negotiate_local_client(stream);
            thread::sleep(Duration::from_millis(175));
            transport
                .send(&WireMessage::Event(EventEnvelope {
                    protocol: CURRENT_PROTOCOL_VERSION,
                    root_tree_id: RootTreeId::new(),
                    generation: Generation::new(1),
                    first_sequence: Sequence::new(1),
                    sequence: Sequence::new(1),
                    occurred_at: UtcTimestamp::UNIX_EPOCH,
                    event: DaemonEvent::Warning(CommonError::new(
                        ErrorCode::Unavailable,
                        "idle event arrived",
                        true,
                    )),
                }))
                .unwrap();
            release_receiver
                .recv_timeout(Duration::from_secs(2))
                .unwrap();
        });
        let client = AgentConnectionClient::connect(
            ConnectionMode::Attach { socket_path: path },
            ClientId::new(),
            None,
            Duration::from_secs(1),
        )
        .unwrap();
        let dispatcher = AgentCommandDispatcher::new(client, Duration::from_secs(1));

        let deadline = Instant::now() + Duration::from_secs(2);
        let mut observed = false;
        while Instant::now() < deadline {
            if let Some(DispatchEvent::Message(message)) = dispatcher.try_next()
                && matches!(
                    *message,
                    WireMessage::Event(EventEnvelope {
                        event: DaemonEvent::Warning(ref warning),
                        ..
                    }) if warning.message == "idle event arrived"
                )
            {
                observed = true;
                break;
            }
            thread::sleep(Duration::from_millis(5));
        }
        assert!(observed, "idle event did not reach the TUI dispatcher");
        release_sender.send(()).unwrap();
        drop(dispatcher);
        server.join().unwrap();
    }

    #[test]
    fn remote_connection_sends_bearer_header_without_url_credentials() {
        let listener = TcpListener::bind("127.0.0.1:0").unwrap();
        let address = listener.local_addr().unwrap();
        let observed = Arc::new(Mutex::new(None));
        let server_observed = Arc::clone(&observed);
        let server = thread::spawn(move || {
            let (stream, _) = listener.accept().unwrap();
            let callback =
                |request: &tungstenite::handshake::server::Request,
                 response: tungstenite::handshake::server::Response| {
                    let header = request
                        .headers()
                        .get("authorization")
                        .unwrap()
                        .to_str()
                        .unwrap()
                        .to_owned();
                    *server_observed.lock().unwrap() = Some((request.uri().to_string(), header));
                    Ok(response)
                };
            let socket = tungstenite::accept_hdr(stream, callback).unwrap();
            let mut transport =
                WebSocketTransport::new(socket, WireFormat::Json, MAX_REMOTE_MESSAGE_BYTES);
            let WireMessage::ClientHello(hello) = transport.receive().unwrap() else {
                panic!("client hello required");
            };
            let response = negotiate(
                &hello,
                CURRENT_PROTOCOL_VERSION,
                EntityId::new(),
                &hello.supported_features,
            )
            .unwrap();
            transport.send(&WireMessage::ServerHello(response)).unwrap();
        });
        let url = Url::parse(&format!("ws://{address}/agent")).unwrap();
        let client = AgentConnectionClient::connect(
            ConnectionMode::Remote {
                url,
                bearer_token: "remote-secret".into(),
            },
            ClientId::new(),
            None,
            Duration::from_secs(1),
        )
        .unwrap();
        drop(client);
        server.join().unwrap();
        let (uri, authorization) = observed.lock().unwrap().clone().unwrap();
        assert_eq!(uri, "/agent");
        assert_eq!(authorization, "Bearer remote-secret");
        assert!(!uri.contains("remote-secret"));
    }

    #[test]
    fn argument_modes_and_accessibility_are_explicit() {
        let attached = TuiArguments::parse([
            "agent-tui",
            "--socket",
            "/tmp/keith.sock",
            "--session",
            &keith_agent_types::SessionId::new().to_string(),
            "--color",
            "none",
            "--reduced-motion",
        ])
        .unwrap()
        .unwrap();
        assert!(matches!(attached.mode, ConnectionMode::Attach { .. }));
        assert_eq!(attached.color_mode, crate::ColorMode::NoColor);
        assert!(attached.reduced_motion);

        let supervised = TuiArguments::parse([
            "agent-tui",
            "--data-root",
            "/tmp/keith-state",
            "--daemon-executable",
            "/bin/agentd",
            "--worker-executable",
            "/bin/agent-worker",
        ])
        .unwrap()
        .unwrap();
        assert!(matches!(
            supervised.mode,
            ConnectionMode::SupervisedLocal(_)
        ));
        let native = TuiArguments::parse(["agent-tui"]).unwrap().unwrap();
        let ConnectionMode::SupervisedLocal(native) = native.mode else {
            panic!("native defaults must supervise a local daemon");
        };
        assert!(native.data_root.is_absolute());
        assert!(native.socket_path.is_absolute());
        assert!(
            TuiArguments::parse(["agent-tui", "--help"])
                .unwrap()
                .is_none()
        );
        assert!(TuiArguments::parse(["agent-tui", "--remote", "ws://localhost"]).is_err());
    }
}
