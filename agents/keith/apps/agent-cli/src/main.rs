#![forbid(unsafe_code)]

use std::collections::BTreeSet;
use std::ffi::OsString;
use std::path::PathBuf;
use std::time::Duration;

use keith_agent_types::{
    CURRENT_PROTOCOL_VERSION, ClientId, CommandId, ComputerId, EntityId, ProfileId, Revision,
    StableKey, UtcTimestamp,
};
use keith_connection::{AgentTransport, FramedTransport};
use keith_credentials::{
    CredentialOwner, CredentialRef, EncryptedCredentialStore, MasterKey, NativeMasterKeyStore,
    RestrictedMasterKeyStore, SecretValue,
};
use keith_platform::PlatformPaths;
use keith_protocol::{
    AgentLifecycleCommand, AssignmentStateProjection, AuditCommand, ClientCommand, ClientHello,
    CommandEnvelope, ComputerAction, ComputerCommand, ComputerProtocolCommand,
    ComputerStreamOpenCommand, ConversationCommand, ConversationListRequest,
    ConversationResumeCursor, CreateAssignmentCommand, DeliveryAdminAction, DeliveryAdminCommand,
    Feature, HandoffWorkCommand, PeerMessageCommand, ReportAssignmentCommand,
    ResumeConversationEventsCommand, TeammatesCommand, WireFormat, WireMessage,
};
use keith_provider_catalog::{BUILTIN_PROVIDERS, provider as provider_spec};

const HELP: &str = "Keith agent command line\n\n\
Usage:\n\
  agent-cli provider set --provider NAME --secret-env ENV [options]\n\
  agent-cli provider list [options]\n\
  agent-cli agents | conversations | assignments | delivery-failures | routines | computers\n\
  agent-cli peer-message RECIPIENT CONVERSATION SENDER SESSION CONVERSATION_REV POLICY_REV MESSAGE\n\
  agent-cli assign-work OWNER CONVERSATION SOURCE_EVENT PRIORITY OBJECTIVE\n\
  agent-cli handoff-work ASSIGNMENT REVISION NEW_OWNER REASON\n\
  agent-cli report-assignment ASSIGNMENT REVISION STATE RESULT_EVENT|- SUMMARY\n\
  agent-cli delivery-retry DELIVERY REVISION\n\
  agent-cli computer-start COMPUTER PROFILE [REVISION]\n\
  agent-cli computer-stop COMPUTER PROFILE [REVISION]\n\
  agent-cli computer-open PROFILE COMPUTER\n\
  agent-cli audit [PROFILE]\n\n\
Options:\n\
  --name NAME                         Credential reference (default: default)\n\
  --data-root PATH                    Keith data directory\n\
  --credential-root PATH              Encrypted credential directory\n\
  --credential-key-env ENV            Read a 64-character hex master key from ENV\n\
  --credential-key-native-account ID  Native keyring account\n\n\
Secrets are accepted only through the named environment variable and are never command arguments.";

enum ProviderCommand {
    Set {
        provider: String,
        secret_env: String,
    },
    List,
    Protocol(ClientCommand),
}

enum KeySource {
    Environment(String),
    Native(String),
    Restricted(PathBuf),
}

struct Arguments {
    command: ProviderCommand,
    name: String,
    data_root: PathBuf,
    credential_root: PathBuf,
    key_source: KeySource,
}

impl Arguments {
    fn parse<I, S>(arguments: I) -> Result<Option<Self>, String>
    where
        I: IntoIterator<Item = S>,
        S: Into<OsString>,
    {
        let mut arguments = arguments.into_iter().map(Into::into);
        let _program = arguments.next();
        let Some(command) = arguments.next() else {
            return Ok(None);
        };
        if matches!(command.to_str(), Some("--version" | "-V")) {
            println!("{} {}", env!("CARGO_BIN_NAME"), env!("CARGO_PKG_VERSION"));
            return Ok(None);
        }
        if matches!(command.to_str(), Some("--help" | "-h")) {
            println!("{HELP}");
            return Ok(None);
        }
        if command != "provider" {
            let command = command
                .into_string()
                .map_err(|_| "command must be UTF-8".to_owned())?;
            let protocol = parse_protocol_command(&command, arguments)?;
            let paths = PlatformPaths::discover().map_err(|error| error.to_string())?;
            let credential_root = paths.data_root.join("credentials");
            return Ok(Some(Self {
                command: ProviderCommand::Protocol(protocol),
                name: "default".into(),
                data_root: paths.data_root,
                credential_root: credential_root.clone(),
                key_source: KeySource::Restricted(credential_root),
            }));
        }
        let action = arguments
            .next()
            .ok_or_else(|| "expected `provider set` or `provider list`".to_owned())?;
        let mut provider = None;
        let mut secret_env = None;
        let mut name = "default".to_owned();
        let mut data_root = None;
        let mut credential_root = None;
        let mut key_source = None;
        while let Some(argument) = arguments.next() {
            let argument = argument
                .into_string()
                .map_err(|_| "arguments must be UTF-8".to_owned())?;
            if matches!(argument.as_str(), "--help" | "-h") {
                println!("{HELP}");
                return Ok(None);
            }
            let value = arguments
                .next()
                .ok_or_else(|| format!("missing value for {argument}"))?;
            match argument.as_str() {
                "--provider" => provider = Some(utf8(value, "provider")?),
                "--secret-env" => secret_env = Some(utf8(value, "secret environment")?),
                "--name" => name = utf8(value, "credential name")?,
                "--data-root" => data_root = Some(PathBuf::from(value)),
                "--credential-root" => credential_root = Some(PathBuf::from(value)),
                "--credential-key-env" => {
                    key_source = Some(KeySource::Environment(utf8(
                        value,
                        "credential key environment",
                    )?));
                }
                "--credential-key-native-account" => {
                    key_source = Some(KeySource::Native(utf8(value, "native key account")?));
                }
                _ => return Err(format!("unknown argument {argument}")),
            }
        }
        let data_root = match data_root {
            Some(root) => root,
            None => {
                PlatformPaths::discover()
                    .map_err(|error| error.to_string())?
                    .data_root
            }
        };
        let credential_root = credential_root.unwrap_or_else(|| data_root.join("credentials"));
        let key_source =
            key_source.unwrap_or_else(|| KeySource::Restricted(credential_root.clone()));
        let command = match action.to_str() {
            Some("set") => ProviderCommand::Set {
                provider: provider.ok_or_else(|| "--provider is required".to_owned())?,
                secret_env: secret_env.ok_or_else(|| "--secret-env is required".to_owned())?,
            },
            Some("list") => {
                if provider.is_some() || secret_env.is_some() {
                    return Err("provider list does not accept --provider or --secret-env".into());
                }
                ProviderCommand::List
            }
            _ => return Err("expected `provider set` or `provider list`".into()),
        };
        Ok(Some(Self {
            command,
            name,
            data_root,
            credential_root,
            key_source,
        }))
    }

    fn master_key(&self) -> Result<MasterKey, String> {
        match &self.key_source {
            KeySource::Environment(environment) => {
                let value = std::env::var_os(environment)
                    .ok_or_else(|| format!("{environment} is unavailable"))?;
                decode_key(value.as_encoded_bytes()).map(MasterKey::from_bytes)
            }
            KeySource::Native(account) => NativeMasterKeyStore::new("keith-agent", account.clone())
                .and_then(|store| store.load_or_create())
                .map_err(|error| error.to_string()),
            KeySource::Restricted(root) => RestrictedMasterKeyStore::open(root)
                .and_then(|store| store.load_or_create())
                .map_err(|error| error.to_string()),
        }
    }
}

fn parse_protocol_command(
    command: &str,
    arguments: impl Iterator<Item = OsString>,
) -> Result<ClientCommand, String> {
    let arguments = arguments
        .map(|argument| {
            argument
                .into_string()
                .map_err(|_| "command arguments must be UTF-8".to_owned())
        })
        .collect::<Result<Vec<_>, _>>()?;
    let teammates = |command| ClientCommand::Conversation(ConversationCommand::Teammates(command));
    match command {
        "agents" if arguments.is_empty() => {
            Ok(ClientCommand::AgentLifecycle(AgentLifecycleCommand::List))
        }
        "conversations" if arguments.is_empty() => Ok(ClientCommand::Conversation(
            ConversationCommand::List(ConversationListRequest {
                include_archived: true,
                after_conversation_id: None,
                limit: 100,
            }),
        )),
        "assignments" | "delivery-failures" | "routines" | "computers" if arguments.is_empty() => {
            Ok(teammates(TeammatesCommand::Resume(
                ResumeConversationEventsCommand {
                    request_id: EntityId::new(),
                    cursor: ConversationResumeCursor {
                        generation: 0,
                        sequence: 0,
                    },
                    profile_id: None,
                    conversation_id: None,
                },
            )))
        }
        "peer-message" if arguments.len() >= 7 => {
            let (request_id, operation_key) = cli_operation("message");
            let now = UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH);
            Ok(teammates(TeammatesCommand::PeerMessage(
                PeerMessageCommand {
                    request_id,
                    operation_key,
                    recipient_profile_id: parse(&arguments[0], "recipient profile")?,
                    conversation_id: parse(&arguments[1], "conversation")?,
                    sender_profile_id: parse(&arguments[2], "sender profile")?,
                    participant_session_id: parse(&arguments[3], "participant session")?,
                    expected_conversation_revision: parse_revision(
                        &arguments[4],
                        "conversation revision",
                    )?,
                    expected_policy_revision: parse_revision(&arguments[5], "policy revision")?,
                    content: arguments[6..].join(" "),
                    deadline: UtcTimestamp::from_unix_millis(
                        now.unix_millis().saturating_add(300_000),
                    ),
                },
            )))
        }
        "assign-work" if arguments.len() >= 5 => {
            let (request_id, operation_key) = cli_operation("assign");
            Ok(teammates(TeammatesCommand::CreateAssignment(
                CreateAssignmentCommand {
                    request_id,
                    operation_key,
                    assignment_id: EntityId::new(),
                    owner_profile_id: parse(&arguments[0], "owner profile")?,
                    conversation_id: parse(&arguments[1], "conversation")?,
                    source_event_id: parse(&arguments[2], "source event")?,
                    priority: parse(&arguments[3], "priority")?,
                    objective: arguments[4..].join(" "),
                    dependency_ids: Vec::new(),
                    due_at: None,
                },
            )))
        }
        "handoff-work" if arguments.len() >= 4 => {
            let (request_id, operation_key) = cli_operation("handoff");
            Ok(teammates(TeammatesCommand::HandoffWork(
                HandoffWorkCommand {
                    request_id,
                    operation_key,
                    assignment_id: parse(&arguments[0], "assignment")?,
                    expected_assignment_revision: parse_revision(
                        &arguments[1],
                        "assignment revision",
                    )?,
                    new_owner_profile_id: parse(&arguments[2], "new owner profile")?,
                    reason: arguments[3..].join(" "),
                },
            )))
        }
        "report-assignment" if arguments.len() >= 5 => {
            let state = match arguments[2].as_str() {
                "active" => AssignmentStateProjection::Active,
                "blocked" => AssignmentStateProjection::Blocked,
                "completed" => AssignmentStateProjection::Completed,
                _ => return Err("assignment state must be active, blocked, or completed".into()),
            };
            let result_event_id = if arguments[3] == "-" {
                None
            } else {
                Some(parse(&arguments[3], "result event")?)
            };
            if state == AssignmentStateProjection::Completed && result_event_id.is_none() {
                return Err("completed assignment reports require a result event".into());
            }
            let (request_id, operation_key) = cli_operation("report");
            Ok(teammates(TeammatesCommand::ReportAssignment(
                ReportAssignmentCommand {
                    request_id,
                    operation_key,
                    assignment_id: parse(&arguments[0], "assignment")?,
                    expected_assignment_revision: parse_revision(
                        &arguments[1],
                        "assignment revision",
                    )?,
                    state,
                    summary: arguments[4..].join(" "),
                    result_event_id,
                },
            )))
        }
        "delivery-retry" if arguments.len() == 2 => {
            let (request_id, operation_key) = cli_operation("delivery");
            Ok(teammates(TeammatesCommand::DeliveryAdmin(
                DeliveryAdminCommand {
                    request_id,
                    operation_key,
                    delivery_id: parse(&arguments[0], "delivery")?,
                    expected_revision: parse_revision(&arguments[1], "delivery revision")?,
                    action: DeliveryAdminAction::Retry,
                    safe_reason: None,
                },
            )))
        }
        "computer-start" | "computer-stop" if (2..=3).contains(&arguments.len()) => {
            let (request_id, operation_key) = cli_operation("computer");
            Ok(teammates(TeammatesCommand::Computer(ComputerCommand {
                request_id,
                operation_key,
                action: if command == "computer-start" {
                    ComputerAction::Start
                } else {
                    ComputerAction::Stop
                },
                computer_id: parse(&arguments[0], "computer")?,
                profile_id: parse(&arguments[1], "profile")?,
                expected_revision: arguments
                    .get(2)
                    .map(|revision| parse_revision(revision, "computer revision"))
                    .transpose()?,
                lease_token: None,
                bounded_input: None,
            })))
        }
        "computer-open" if arguments.len() == 2 => Ok(ClientCommand::Computer(
            ComputerProtocolCommand::Open(ComputerStreamOpenCommand {
                request_id: EntityId::new(),
                profile_id: parse::<ProfileId>(&arguments[0], "profile")?,
                computer_id: parse::<ComputerId>(&arguments[1], "computer")?,
                resume: None,
            }),
        )),
        "audit" if arguments.len() <= 1 => Ok(teammates(TeammatesCommand::Audit(AuditCommand {
            request_id: EntityId::new(),
            profile_id: arguments
                .first()
                .map(|profile| parse(profile, "profile"))
                .transpose()?,
            conversation_id: None,
            after_audit_id: None,
            limit: 100,
        }))),
        _ => Err(format!("invalid {command} arguments; run agent-cli --help")),
    }
}

fn parse<T>(value: &str, label: &str) -> Result<T, String>
where
    T: std::str::FromStr,
{
    value.parse().map_err(|_| format!("invalid {label}"))
}

fn parse_revision(value: &str, label: &str) -> Result<Revision, String> {
    value
        .parse::<u64>()
        .map(Revision::new)
        .map_err(|_| format!("invalid {label}"))
}

fn cli_operation(kind: &str) -> (EntityId, StableKey) {
    let request_id = EntityId::new();
    let operation_key = StableKey::parse(format!("cli:{kind}:{request_id}"))
        .expect("generated CLI operation keys are canonical");
    (request_id, operation_key)
}

fn utf8(value: OsString, label: &str) -> Result<String, String> {
    value
        .into_string()
        .map_err(|_| format!("{label} must be UTF-8"))
}

fn run() -> Result<(), String> {
    let Some(arguments) = Arguments::parse(std::env::args_os())? else {
        if std::env::args_os().nth(1).is_none() {
            println!("{HELP}");
        }
        return Ok(());
    };
    if let ProviderCommand::Protocol(command) = &arguments.command {
        return execute_protocol_command(&arguments.data_root, command.clone());
    }
    std::fs::create_dir_all(&arguments.data_root).map_err(|error| error.to_string())?;
    let store = EncryptedCredentialStore::open(&arguments.credential_root, arguments.master_key()?)
        .map_err(|error| error.to_string())?;
    match arguments.command {
        ProviderCommand::Set {
            provider,
            secret_env,
        } => {
            if provider_spec(&provider).is_none() {
                return Err(format!(
                    "unknown provider {provider}; run `agent-cli provider list` for supported IDs"
                ));
            }
            let secret = std::env::var_os(&secret_env)
                .ok_or_else(|| format!("{secret_env} is unavailable"))?;
            let reference = CredentialRef::new(
                arguments.name.clone(),
                CredentialOwner::Provider(provider.clone()),
            )
            .map_err(|error| error.to_string())?;
            store
                .put(
                    reference,
                    SecretValue::new(secret.into_encoded_bytes())
                        .map_err(|error| error.to_string())?,
                    UtcTimestamp::now().map_err(|error| error.to_string())?,
                )
                .map_err(|error| error.to_string())?;
            println!(
                "configured provider {provider} with credential reference {}",
                arguments.name
            );
        }
        ProviderCommand::List => {
            let inspections = store.inspect().map_err(|error| error.to_string())?;
            for provider in BUILTIN_PROVIDERS {
                println!(
                    "{}\t{}\ttransport={}\tauth={}\tenv={}",
                    provider.id,
                    provider.display_name,
                    provider.transport.as_str(),
                    provider.authentication.as_str(),
                    if provider.credential_environment.is_empty() {
                        "interactive"
                    } else {
                        provider.credential_environment
                    }
                );
            }
            println!("configured credential records={}", inspections.len());
        }
        ProviderCommand::Protocol(_) => unreachable!("protocol commands return before store setup"),
    }
    Ok(())
}

fn execute_protocol_command(
    data_root: &std::path::Path,
    command: ClientCommand,
) -> Result<(), String> {
    let daemon_endpoint = data_root.join("agentd.sock");
    let stream =
        keith_connection::connect_local(&daemon_endpoint).map_err(|error| error.to_string())?;
    keith_connection::set_local_read_timeout(&stream, Some(Duration::from_secs(30)))
        .map_err(|error| error.to_string())?;
    let mut transport = FramedTransport::new(stream, WireFormat::Json);
    let client_id = ClientId::new();
    transport
        .send(&WireMessage::ClientHello(ClientHello {
            protocol: CURRENT_PROTOCOL_VERSION,
            client_id: client_id.clone(),
            client_name: "agent-cli".into(),
            client_version: env!("CARGO_PKG_VERSION").into(),
            supported_features: BTreeSet::from([
                Feature::AgentLifecycle,
                Feature::Conversations,
                Feature::TeammatesProtocol,
                Feature::ComputerStreaming,
                Feature::Replay,
                Feature::Snapshots,
                Feature::FramedJson,
                Feature::LocalBinary,
            ]),
            resume: None,
        }))
        .map_err(|error| error.to_string())?;
    let negotiated = loop {
        match transport.receive().map_err(|error| error.to_string())? {
            WireMessage::ServerHello(server) => break server.protocol,
            WireMessage::Event(_)
            | WireMessage::Snapshot(_)
            | WireMessage::Terminal(_)
            | WireMessage::CommandResult(_) => {}
            WireMessage::ClientHello(_) | WireMessage::Command(_) => {
                return Err("daemon returned an invalid handshake message".into());
            }
        }
    };
    let command_id = CommandId::new();
    transport
        .send(&WireMessage::Command(CommandEnvelope {
            protocol: negotiated,
            command_id: command_id.clone(),
            client_id,
            sent_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
            session_id: None,
            command,
        }))
        .map_err(|error| error.to_string())?;
    loop {
        match transport.receive().map_err(|error| error.to_string())? {
            WireMessage::CommandResult(result) if result.command_id == command_id => {
                println!("{:#?}", result.result);
                return Ok(());
            }
            WireMessage::Event(_)
            | WireMessage::Snapshot(_)
            | WireMessage::Terminal(_)
            | WireMessage::CommandResult(_) => {}
            WireMessage::ClientHello(_) | WireMessage::ServerHello(_) | WireMessage::Command(_) => {
                return Err("daemon returned an invalid command response".into());
            }
        }
    }
}

fn decode_key(encoded: &[u8]) -> Result<[u8; 32], String> {
    if encoded.len() != 64 {
        return Err("credential key must be 64 hexadecimal characters".into());
    }
    let mut decoded = [0_u8; 32];
    for (target, pair) in decoded.iter_mut().zip(encoded.chunks_exact(2)) {
        *target = (hex_digit(pair[0])? << 4) | hex_digit(pair[1])?;
    }
    Ok(decoded)
}

fn hex_digit(value: u8) -> Result<u8, String> {
    match value {
        b'0'..=b'9' => Ok(value - b'0'),
        b'a'..=b'f' => Ok(value - b'a' + 10),
        b'A'..=b'F' => Ok(value - b'A' + 10),
        _ => Err("credential key must be hexadecimal".into()),
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
    use super::{Arguments, HELP, ProviderCommand};

    #[test]
    fn provider_secret_is_environment_only() {
        assert!(!HELP.contains("--secret "));
        let error = Arguments::parse([
            "agent-cli",
            "provider",
            "set",
            "--provider",
            "openai",
            "--secret",
            "forbidden",
        ])
        .err()
        .expect("secret argument must be rejected");
        assert!(error.contains("unknown argument --secret"));
    }

    #[test]
    fn provider_set_requires_provider_and_secret_environment() {
        let error = Arguments::parse(["agent-cli", "provider", "set"])
            .err()
            .expect("missing provider must fail");
        assert_eq!(error, "--provider is required");
        let parsed = Arguments::parse([
            "agent-cli",
            "provider",
            "set",
            "--provider",
            "anthropic",
            "--secret-env",
            "ANTHROPIC_API_KEY",
            "--data-root",
            "/tmp/keith-cli-test",
            "--credential-key-env",
            "KEITH_CREDENTIAL_KEY",
        ])
        .expect("arguments must parse")
        .expect("command must be present");
        assert!(matches!(parsed.command, ProviderCommand::Set { .. }));
    }
}
