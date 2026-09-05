#![forbid(unsafe_code)]

use std::ffi::OsString;
use std::fs::{self, File, OpenOptions};
use std::io::{self, BufRead, Write};
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::thread;
use std::time::Duration;

use keith_agent_types::{CommandId, EntityId, ProfileId, SessionId, UtcTimestamp};
use keith_channel_adapters::{DiscordAdapter, DiscordConfig, DiscordCursor, DiscordUpload};
use keith_channel_core::{
    AdapterEvent, AdapterFailure, AgentConnection, ChannelAdapter, EnqueueOutcome, GatewayLimits,
    GatewayQueue, OutboundMessage, ReconnectPolicy, ReplyRoute, RetryClass, RoutedInbound,
    SessionAction,
};
use keith_connection::{FramedTransport, LocalStream, connect_local};
use keith_credentials::SecretValue;
use keith_protocol::{
    ClientCommand, CommandResult, DeliveryAcknowledgement, DeliveryFailure, DeliveryFailureClass,
    ResponsePayload, StagedAttachment, WireFormat,
};
use serde::Serialize;
use sha2::{Digest, Sha256};
use signal_hook::consts::{SIGINT, SIGTERM};

const MAX_ATTACHMENT_BYTES: u64 = 25 * 1_024 * 1_024;

struct Arguments {
    socket: PathBuf,
    reconnect: ReconnectPolicy,
    mode: GatewayMode,
}

enum GatewayMode {
    StandardInput,
    Discord(DiscordArguments),
}

struct DiscordArguments {
    token_environment: String,
    bot_user_id: String,
    intents: u64,
    profile_id: ProfileId,
    session_id: SessionId,
    cursor: PathBuf,
    attachment_root: PathBuf,
}

impl Arguments {
    fn parse<I, S>(arguments: I) -> Result<Option<Self>, String>
    where
        I: IntoIterator<Item = S>,
        S: Into<OsString>,
    {
        let mut arguments = arguments.into_iter().map(Into::into);
        let _program = arguments.next();
        let mut socket = None;
        let mut reconnect = ReconnectPolicy::default();
        let mut discord_token_environment = None;
        let mut discord_bot_user_id = None;
        let mut discord_intents = None;
        let mut discord_profile_id = None;
        let mut discord_session_id = None;
        let mut discord_cursor = None;
        let mut attachment_root = None;
        while let Some(argument) = arguments.next() {
            let argument = argument
                .into_string()
                .map_err(|_| "arguments must be UTF-8".to_owned())?;
            if matches!(argument.as_str(), "--version" | "-V") {
                println!("{} {}", env!("CARGO_BIN_NAME"), env!("CARGO_PKG_VERSION"));
                return Ok(None);
            }
            let value = arguments
                .next()
                .ok_or_else(|| format!("missing value for {argument}"))?
                .into_string()
                .map_err(|_| format!("value for {argument} must be UTF-8"))?;
            match argument.as_str() {
                "--socket" => socket = Some(PathBuf::from(value)),
                "--reconnect-attempts" => {
                    reconnect.max_attempts = value
                        .parse()
                        .map_err(|_| "reconnect attempts must be an integer".to_owned())?;
                }
                "--discord-token-env" => discord_token_environment = Some(value),
                "--discord-bot-user-id" => discord_bot_user_id = Some(value),
                "--discord-intents" => {
                    discord_intents = Some(
                        value
                            .parse()
                            .map_err(|_| "Discord intents must be an integer".to_owned())?,
                    );
                }
                "--discord-profile-id" => {
                    discord_profile_id = Some(
                        value
                            .parse()
                            .map_err(|_| "Discord profile ID is invalid".to_owned())?,
                    );
                }
                "--discord-session-id" => {
                    discord_session_id = Some(
                        value
                            .parse()
                            .map_err(|_| "Discord session ID is invalid".to_owned())?,
                    );
                }
                "--discord-cursor" => discord_cursor = Some(PathBuf::from(value)),
                "--attachment-root" => attachment_root = Some(PathBuf::from(value)),
                _ => return Err(format!("unknown argument {argument}")),
            }
        }
        let mode = match discord_token_environment {
            Some(token_environment) => GatewayMode::Discord(DiscordArguments {
                token_environment,
                bot_user_id: discord_bot_user_id
                    .ok_or_else(|| "--discord-bot-user-id is required".to_owned())?,
                intents: discord_intents
                    .ok_or_else(|| "--discord-intents is required".to_owned())?,
                profile_id: discord_profile_id
                    .ok_or_else(|| "--discord-profile-id is required".to_owned())?,
                session_id: discord_session_id
                    .ok_or_else(|| "--discord-session-id is required".to_owned())?,
                cursor: discord_cursor.ok_or_else(|| "--discord-cursor is required".to_owned())?,
                attachment_root: attachment_root
                    .ok_or_else(|| "--attachment-root is required".to_owned())?,
            }),
            None if discord_bot_user_id.is_none()
                && discord_intents.is_none()
                && discord_profile_id.is_none()
                && discord_session_id.is_none()
                && discord_cursor.is_none()
                && attachment_root.is_none() =>
            {
                GatewayMode::StandardInput
            }
            None => {
                return Err(
                    "--discord-token-env is required when Discord options are present".into(),
                );
            }
        };
        Ok(Some(Self {
            socket: socket.ok_or_else(|| "--socket is required".to_owned())?,
            reconnect,
            mode,
        }))
    }
}

#[derive(Serialize)]
#[serde(deny_unknown_fields)]
struct GatewayReport {
    message_id: String,
    outcome: &'static str,
    safe_error: Option<String>,
}

type LocalAgentConnection = AgentConnection<FramedTransport<LocalStream>>;

fn connect(socket: &Path) -> Result<LocalAgentConnection, String> {
    let stream = connect_local(socket).map_err(|error| error.to_string())?;
    AgentConnection::connect(FramedTransport::new(stream, WireFormat::Json))
        .map_err(|error| error.to_string())
}

fn submit_with_reconnect(
    connection: &mut Option<LocalAgentConnection>,
    socket: &Path,
    policy: ReconnectPolicy,
    action: &SessionAction,
) -> Result<CommandResult, String> {
    let mut attempt = 0;
    loop {
        if connection.is_none() {
            match connect(socket) {
                Ok(connected) => *connection = Some(connected),
                Err(error) => {
                    let Some(delay) = policy.delay_ms(attempt) else {
                        return Err(error);
                    };
                    attempt = attempt.saturating_add(1);
                    thread::sleep(Duration::from_millis(delay));
                    continue;
                }
            }
        }
        let now = UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH);
        match connection
            .as_mut()
            .expect("connection initialized")
            .submit(action, now)
        {
            Ok(result) => return Ok(result),
            Err(error) => {
                *connection = None;
                let Some(delay) = policy.delay_ms(attempt) else {
                    return Err(error.to_string());
                };
                attempt = attempt.saturating_add(1);
                thread::sleep(Duration::from_millis(delay));
            }
        }
    }
}

fn write_report(report: &GatewayReport) -> Result<(), String> {
    let stdout = io::stdout();
    let mut output = stdout.lock();
    serde_json::to_writer(&mut output, report).map_err(|error| error.to_string())?;
    output.write_all(b"\n").map_err(|error| error.to_string())?;
    output.flush().map_err(|error| error.to_string())
}

fn execute_with_reconnect(
    connection: &mut Option<LocalAgentConnection>,
    socket: &Path,
    policy: ReconnectPolicy,
    command: &ClientCommand,
) -> Result<CommandResult, String> {
    let mut attempt = 0;
    let command_id = CommandId::new();
    loop {
        if connection.is_none() {
            match connect(socket) {
                Ok(connected) => *connection = Some(connected),
                Err(error) => {
                    let Some(delay) = policy.delay_ms(attempt) else {
                        return Err(error);
                    };
                    attempt = attempt.saturating_add(1);
                    thread::sleep(Duration::from_millis(delay));
                    continue;
                }
            }
        }
        let now = UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH);
        match connection
            .as_mut()
            .expect("connection initialized")
            .execute_idempotent(&command_id, command.clone(), None, now)
        {
            Ok(result) => return Ok(result),
            Err(error) => {
                *connection = None;
                let Some(delay) = policy.delay_ms(attempt) else {
                    return Err(error.to_string());
                };
                attempt = attempt.saturating_add(1);
                thread::sleep(Duration::from_millis(delay));
            }
        }
    }
}

fn stage_inbound_attachments(
    connection: &mut Option<LocalAgentConnection>,
    socket: &Path,
    reconnect: ReconnectPolicy,
    attachment_root: &Path,
    routed: &mut RoutedInbound,
) -> Result<(), String> {
    for attachment in &mut routed.message.attachments {
        if attachment.artifact_id.is_some() {
            continue;
        }
        let url = attachment
            .download_url
            .as_deref()
            .ok_or_else(|| "Discord attachment download location is missing".to_owned())?;
        let (staging_file, sha256) =
            download_attachment(attachment_root, url, attachment.byte_length)?;
        attachment.staging_file = Some(staging_file.clone());
        attachment.sha256 = Some(sha256.clone());
        let command = ClientCommand::StageAttachment(StagedAttachment {
            session_id: routed.session_id.clone(),
            staging_file: staging_file.clone(),
            file_name: attachment.file_name.clone(),
            media_type: attachment.media_type.clone(),
            byte_length: attachment.byte_length,
            sha256,
        });
        let result = execute_with_reconnect(connection, socket, reconnect, &command);
        match result {
            Ok(CommandResult::Data(payload)) => {
                if let ResponsePayload::Artifact(artifact_id) = *payload {
                    attachment.artifact_id = Some(artifact_id);
                    attachment.download_url = None;
                } else {
                    remove_inbound_staging(attachment_root, &staging_file);
                    return Err("daemon returned an unexpected attachment response".into());
                }
            }
            Ok(CommandResult::Rejected(rejection)) => {
                remove_inbound_staging(attachment_root, &staging_file);
                return Err(rejection.error.message);
            }
            Ok(CommandResult::Accepted { .. }) => {
                remove_inbound_staging(attachment_root, &staging_file);
                return Err("daemon accepted attachment staging without an artifact ID".into());
            }
            Err(error) => {
                remove_inbound_staging(attachment_root, &staging_file);
                return Err(error);
            }
        }
    }
    Ok(())
}

fn remove_inbound_staging(attachment_root: &Path, staging_file: &str) {
    let _ = fs::remove_file(attachment_root.join("inbound").join(staging_file));
}

fn download_attachment(
    attachment_root: &Path,
    url: &str,
    expected_bytes: u64,
) -> Result<(String, String), String> {
    if expected_bytes == 0 || expected_bytes > MAX_ATTACHMENT_BYTES {
        return Err("Discord attachment size is outside the supported limit".into());
    }
    if !(url.starts_with("https://cdn.discordapp.com/attachments/")
        || url.starts_with("https://media.discordapp.net/attachments/"))
    {
        return Err("Discord attachment location is not an approved CDN endpoint".into());
    }
    let agent: ureq::Agent = ureq::Agent::config_builder()
        .timeout_global(Some(Duration::from_secs(30)))
        .http_status_as_error(false)
        .max_redirects(0)
        .build()
        .into();
    let mut response = agent
        .get(url)
        .call()
        .map_err(|_| "Discord attachment download failed".to_owned())?;
    if response.status().as_u16() != 200 {
        return Err("Discord attachment download was rejected".into());
    }
    let limit = expected_bytes.saturating_add(1);
    let bytes = response
        .body_mut()
        .with_config()
        .limit(limit)
        .read_to_vec()
        .map_err(|_| "Discord attachment body could not be read".to_owned())?;
    if u64::try_from(bytes.len()).ok() != Some(expected_bytes) {
        return Err("Discord attachment length changed during download".into());
    }
    let inbound_root = attachment_root.join("inbound");
    fs::create_dir_all(&inbound_root).map_err(|error| error.to_string())?;
    let staging_file = EntityId::new().to_string();
    let path = inbound_root.join(&staging_file);
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&path)
        .map_err(|error| error.to_string())?;
    if let Err(error) = file.write_all(&bytes).and_then(|()| file.sync_all()) {
        let _ = fs::remove_file(&path);
        return Err(error.to_string());
    }
    Ok((staging_file, hex_sha256(&bytes)))
}

fn hex_sha256(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let digest = Sha256::digest(bytes);
    let mut encoded = String::with_capacity(64);
    for byte in digest {
        encoded.push(char::from(HEX[usize::from(byte >> 4)]));
        encoded.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    encoded
}

fn dispatch_available(
    connection: &mut Option<LocalAgentConnection>,
    socket: &Path,
    reconnect: ReconnectPolicy,
    channel: &str,
    external_account: &str,
    attachment_root: &Path,
    adapter: &mut DiscordAdapter,
) -> Result<usize, String> {
    let mut sent = 0_usize;
    for _ in 0..64 {
        let claim = match execute_with_reconnect(
            connection,
            socket,
            reconnect,
            &ClientCommand::ClaimDelivery {
                channel: channel.to_owned(),
                external_account: external_account.to_owned(),
            },
        )? {
            CommandResult::Data(payload) => match *payload {
                ResponsePayload::DeliveryClaim(claim) => claim,
                _ => return Err("daemon returned an unexpected delivery response".into()),
            },
            CommandResult::Rejected(rejection) => return Err(rejection.error.message),
            CommandResult::Accepted { .. } => {
                return Err("daemon accepted delivery claim without returning it".into());
            }
        };
        let Some(claim) = claim else {
            break;
        };
        if claim.route.channel != channel || claim.route.external_account != external_account {
            return Err("daemon returned a delivery claim for another channel account".to_owned());
        }
        let staged = load_delivery_artifacts(
            attachment_root,
            &claim.artifacts,
            &claim.staged_artifacts,
            adapter,
        );
        let outcome = match staged {
            Ok(artifact_ids) => {
                let outbound = OutboundMessage {
                    route: ReplyRoute {
                        channel: claim.route.channel.clone(),
                        external_account: claim.route.external_account.clone(),
                        conversation: claim.route.conversation.clone(),
                        thread: claim.route.thread.clone(),
                        reply_to_message: claim.route.reply_to_message.clone(),
                    },
                    idempotency_key: claim.idempotency_key.clone(),
                    text: claim.text.clone(),
                    artifacts: artifact_ids,
                };
                match adapter.send(&outbound) {
                    Ok(receipt) => ClientCommand::AcknowledgeDelivery(DeliveryAcknowledgement {
                        delivery_id: claim.delivery_id.clone(),
                        claim_token: claim.claim_token.clone(),
                        platform_message_id: receipt.platform_message_id,
                        accepted_at: receipt.accepted_at,
                        duplicate_possible: receipt.duplicate_possible,
                    }),
                    Err(failure) => ClientCommand::FailDelivery(DeliveryFailure {
                        delivery_id: claim.delivery_id.clone(),
                        claim_token: claim.claim_token.clone(),
                        class: delivery_failure_class(failure.class),
                        safe_message: failure.safe_message,
                        retry_after_ms: failure.retry_after_ms,
                    }),
                }
            }
            Err(safe_message) => ClientCommand::FailDelivery(DeliveryFailure {
                delivery_id: claim.delivery_id.clone(),
                claim_token: claim.claim_token.clone(),
                class: DeliveryFailureClass::Retryable,
                safe_message,
                retry_after_ms: Some(1_000),
            }),
        };
        let acknowledged = matches!(outcome, ClientCommand::AcknowledgeDelivery(_));
        match execute_with_reconnect(connection, socket, reconnect, &outcome)? {
            CommandResult::Accepted { .. } | CommandResult::Data(_) => {}
            CommandResult::Rejected(rejection) => return Err(rejection.error.message),
        }
        if acknowledged {
            remove_delivery_staging(attachment_root, &claim.staged_artifacts);
        }
        sent = sent.saturating_add(1);
    }
    Ok(sent)
}

fn load_delivery_artifacts(
    attachment_root: &Path,
    expected_artifacts: &[keith_agent_types::ArtifactId],
    artifacts: &[keith_protocol::StagedDeliveryArtifact],
    adapter: &mut DiscordAdapter,
) -> Result<Vec<keith_agent_types::ArtifactId>, String> {
    if expected_artifacts.len() != artifacts.len()
        || !expected_artifacts
            .iter()
            .zip(artifacts)
            .all(|(expected, staged)| expected == &staged.artifact_id)
    {
        return Err("delivery artifact staging is incomplete".into());
    }
    let outbound_root = attachment_root.join("outbound");
    let mut artifact_ids = Vec::with_capacity(artifacts.len());
    for artifact in artifacts {
        let _: EntityId = artifact
            .staging_file
            .parse()
            .map_err(|_| "delivery staging token is invalid".to_owned())?;
        let path = outbound_root.join(&artifact.staging_file);
        let metadata = fs::symlink_metadata(&path)
            .map_err(|_| "delivery staging file is unavailable".to_owned())?;
        if metadata.file_type().is_symlink()
            || !metadata.is_file()
            || metadata.len() != artifact.byte_length
        {
            return Err("delivery staging file metadata changed".into());
        }
        let bytes =
            fs::read(&path).map_err(|_| "delivery staging file could not be read".to_owned())?;
        if u64::try_from(bytes.len()).ok() != Some(artifact.byte_length)
            || hex_sha256(&bytes) != artifact.sha256
        {
            return Err("delivery staging file digest changed".into());
        }
        adapter
            .stage_artifact(
                artifact.artifact_id.clone(),
                DiscordUpload {
                    file_name: artifact.file_name.clone(),
                    media_type: artifact.media_type.clone(),
                    bytes,
                },
            )
            .map_err(|failure| failure.safe_message)?;
        artifact_ids.push(artifact.artifact_id.clone());
    }
    Ok(artifact_ids)
}

fn remove_delivery_staging(
    attachment_root: &Path,
    artifacts: &[keith_protocol::StagedDeliveryArtifact],
) {
    let outbound_root = attachment_root.join("outbound");
    for artifact in artifacts {
        let _ = fs::remove_file(outbound_root.join(&artifact.staging_file));
    }
}

const fn delivery_failure_class(class: RetryClass) -> DeliveryFailureClass {
    match class {
        RetryClass::Retryable => DeliveryFailureClass::Retryable,
        RetryClass::RateLimited => DeliveryFailureClass::RateLimited,
        RetryClass::Reconnect => DeliveryFailureClass::Reconnect,
        RetryClass::Permanent => DeliveryFailureClass::Permanent,
    }
}

fn discord_token(environment: &str) -> Result<SecretValue, String> {
    let value = std::env::var_os(environment)
        .ok_or_else(|| format!("{environment} is unavailable"))?
        .into_encoded_bytes();
    SecretValue::new(value).map_err(|error| error.to_string())
}

fn load_cursor(path: &Path) -> Result<DiscordCursor, String> {
    match fs::read(path) {
        Ok(bytes) => serde_json::from_slice(&bytes).map_err(|error| error.to_string()),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(DiscordCursor::default()),
        Err(error) => Err(error.to_string()),
    }
}

fn save_cursor(path: &Path, cursor: &DiscordCursor) -> Result<(), String> {
    if let Some(parent) = path.parent() {
        fs::create_dir_all(parent).map_err(|error| error.to_string())?;
    }
    let temporary = path.with_extension(format!("{}.tmp", std::process::id()));
    let bytes = serde_json::to_vec(cursor).map_err(|error| error.to_string())?;
    fs::write(&temporary, bytes).map_err(|error| error.to_string())?;
    File::open(&temporary)
        .and_then(|file| file.sync_all())
        .map_err(|error| error.to_string())?;
    keith_platform::replace_file(&temporary, path).map_err(|error| error.to_string())
}

#[allow(clippy::too_many_lines)]
fn run_discord(
    socket: &Path,
    reconnect: ReconnectPolicy,
    arguments: &DiscordArguments,
) -> Result<(), String> {
    let shutdown = Arc::new(AtomicBool::new(false));
    signal_hook::flag::register(SIGTERM, Arc::clone(&shutdown))
        .map_err(|error| error.to_string())?;
    signal_hook::flag::register(SIGINT, Arc::clone(&shutdown))
        .map_err(|error| error.to_string())?;
    let config = DiscordConfig::production(&arguments.bot_user_id, arguments.intents);
    let mut inbound = DiscordAdapter::new(
        config.clone(),
        discord_token(&arguments.token_environment)?,
        load_cursor(&arguments.cursor)?,
    )
    .map_err(|failure| failure.safe_message)?;
    let outbound_shutdown = Arc::clone(&shutdown);
    let outbound_socket = socket.to_path_buf();
    let outbound_token_environment = arguments.token_environment.clone();
    let outbound_external_account = arguments.bot_user_id.clone();
    let outbound_attachment_root = arguments.attachment_root.clone();
    let mut outbound_adapter = DiscordAdapter::new(
        config,
        discord_token(&outbound_token_environment)?,
        DiscordCursor::default(),
    )
    .map_err(|failure| failure.safe_message)?;
    let outbound = thread::spawn(move || -> Result<(), String> {
        let mut connection = None;
        while !outbound_shutdown.load(Ordering::Acquire) {
            match dispatch_available(
                &mut connection,
                &outbound_socket,
                reconnect,
                "discord",
                &outbound_external_account,
                &outbound_attachment_root,
                &mut outbound_adapter,
            ) {
                Ok(0) => thread::sleep(Duration::from_millis(100)),
                Ok(_) => {}
                Err(_) => {
                    connection = None;
                    thread::sleep(Duration::from_millis(reconnect.initial_delay_ms.max(1)));
                }
            }
        }
        Ok(())
    });
    let mut queue =
        GatewayQueue::new(GatewayLimits::default()).map_err(|error| error.to_string())?;
    let mut connection = None;
    while !shutdown.load(Ordering::Acquire) {
        match inbound.receive() {
            Ok(AdapterEvent::Inbound(message)) => {
                let routed = RoutedInbound {
                    profile_id: arguments.profile_id.clone(),
                    session_id: arguments.session_id.clone(),
                    message: *message,
                };
                if queue.enqueue(routed).is_err() {
                    continue;
                }
                while let Some(mut ready) = queue.take_ready() {
                    let session_id = ready.session_id.clone();
                    stage_inbound_attachments(
                        &mut connection,
                        socket,
                        reconnect,
                        &arguments.attachment_root,
                        &mut ready,
                    )?;
                    let action = SessionAction::from(ready);
                    let mut settled = false;
                    while !shutdown.load(Ordering::Acquire) {
                        match submit_with_reconnect(&mut connection, socket, reconnect, &action) {
                            Ok(
                                CommandResult::Accepted { .. }
                                | CommandResult::Data(_)
                                | CommandResult::Rejected(_),
                            ) => {
                                settled = true;
                                break;
                            }
                            Err(_) => {
                                connection = None;
                                thread::sleep(Duration::from_millis(
                                    reconnect.initial_delay_ms.max(1),
                                ));
                            }
                        }
                    }
                    if !settled {
                        return Ok(());
                    }
                    save_cursor(&arguments.cursor, inbound.cursor())?;
                    queue
                        .complete(&session_id)
                        .map_err(|error| error.to_string())?;
                }
            }
            Ok(AdapterEvent::RateLimited { retry_after_ms }) => {
                thread::sleep(Duration::from_millis(retry_after_ms.min(30_000)));
            }
            Ok(AdapterEvent::Disconnected { .. }) => {
                inbound
                    .reconnect()
                    .map_err(|failure| failure.safe_message)?;
            }
            Err(AdapterFailure {
                class: RetryClass::Reconnect | RetryClass::Retryable,
                ..
            }) => {
                thread::sleep(Duration::from_millis(reconnect.initial_delay_ms.max(1)));
                inbound
                    .reconnect()
                    .map_err(|failure| failure.safe_message)?;
            }
            Err(failure) => return Err(failure.safe_message),
        }
    }
    outbound
        .join()
        .map_err(|_| "Discord delivery worker panicked".to_owned())?
}

fn run_standard_input(socket: &Path, reconnect: ReconnectPolicy) -> Result<(), String> {
    let stdin = io::stdin();
    let mut queue =
        GatewayQueue::new(GatewayLimits::default()).map_err(|error| error.to_string())?;
    let mut connection = None;
    for line in stdin.lock().lines() {
        let line = line.map_err(|error| error.to_string())?;
        let Ok(routed) = serde_json::from_str::<RoutedInbound>(&line) else {
            write_report(&GatewayReport {
                message_id: String::new(),
                outcome: "rejected",
                safe_error: Some("malformed normalized inbound message".to_owned()),
            })?;
            continue;
        };
        let message_id = routed.message.message_id.clone();
        match queue.enqueue(routed) {
            Ok(EnqueueOutcome::Duplicate) => {
                write_report(&GatewayReport {
                    message_id,
                    outcome: "duplicate",
                    safe_error: None,
                })?;
                continue;
            }
            Ok(EnqueueOutcome::Queued) => {}
            Err(error) => {
                write_report(&GatewayReport {
                    message_id,
                    outcome: "rejected",
                    safe_error: Some(error.to_string()),
                })?;
                continue;
            }
        }
        while let Some(ready) = queue.take_ready() {
            let session_id = ready.session_id.clone();
            let action = SessionAction::from(ready);
            let report = match submit_with_reconnect(&mut connection, socket, reconnect, &action) {
                Ok(CommandResult::Accepted { .. } | CommandResult::Data(_)) => GatewayReport {
                    message_id: action.message_id,
                    outcome: "submitted",
                    safe_error: None,
                },
                Ok(CommandResult::Rejected(rejection)) => GatewayReport {
                    message_id: action.message_id,
                    outcome: "rejected",
                    safe_error: Some(rejection.error.message),
                },
                Err(error) => GatewayReport {
                    message_id: action.message_id,
                    outcome: "connection_failed",
                    safe_error: Some(error),
                },
            };
            queue
                .complete(&session_id)
                .map_err(|error| error.to_string())?;
            write_report(&report)?;
        }
    }
    Ok(())
}

fn run() -> Result<(), String> {
    let Some(arguments) = Arguments::parse(std::env::args_os())? else {
        return Ok(());
    };
    match &arguments.mode {
        GatewayMode::StandardInput => run_standard_input(&arguments.socket, arguments.reconnect),
        GatewayMode::Discord(discord) => {
            run_discord(&arguments.socket, arguments.reconnect, discord)
        }
    }
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn arguments_require_socket_and_bound_reconnect_attempts() {
        let parsed = Arguments::parse([
            "channel-gateway",
            "--socket",
            "/tmp/agent.sock",
            "--reconnect-attempts",
            "3",
        ])
        .expect("arguments")
        .expect("run");
        assert_eq!(parsed.socket, PathBuf::from("/tmp/agent.sock"));
        assert_eq!(parsed.reconnect.max_attempts, 3);
        assert!(Arguments::parse(["channel-gateway"]).is_err());
    }
}
