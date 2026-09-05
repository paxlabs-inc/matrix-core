#![forbid(unsafe_code)]

use std::ffi::OsString;
use std::path::PathBuf;

use keith_agent_types::UtcTimestamp;
use keith_credentials::{
    CredentialOwner, CredentialRef, EncryptedCredentialStore, MasterKey, NativeMasterKeyStore,
    RestrictedMasterKeyStore, SecretValue,
};
use keith_platform::PlatformPaths;
use keith_provider_catalog::{BUILTIN_PROVIDERS, provider as provider_spec};

const HELP: &str = "Keith agent command line\n\n\
Usage:\n\
  agent-cli provider set --provider NAME --secret-env ENV [options]\n\
  agent-cli provider list [options]\n\n\
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
            return Err("expected `provider set` or `provider list`".into());
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
    }
    Ok(())
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
