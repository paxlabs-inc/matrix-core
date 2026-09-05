#![forbid(unsafe_code)]

use keith_agent_types::ProfileId;
use keith_provider_core::CancellationToken;
use keith_web::{BrowserPolicy, BrowserRunner, NoBrowserProgress, NoFetchProgress, SafeWebClient};

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
    let url = argument_value(&arguments, "--url")?;
    let profile_id = arguments
        .iter()
        .position(|argument| argument == "--profile")
        .map(|index| {
            arguments
                .get(index.saturating_add(1))
                .ok_or_else(|| "--profile requires a value".to_owned())?
                .parse::<ProfileId>()
                .map_err(|error| error.to_string())
        })
        .transpose()?
        .unwrap_or_default();
    let browser = BrowserRunner::new(SafeWebClient::default(), BrowserPolicy::default());
    let session_id = browser
        .open_session(&profile_id)
        .map_err(|error| error.to_string())?;
    let observation = browser
        .navigate(
            &profile_id,
            &session_id,
            &url,
            &CancellationToken::default(),
            &NoFetchProgress,
            &NoBrowserProgress,
        )
        .map_err(|error| error.to_string())?;
    println!(
        "{}",
        serde_json::json!({
            "session_id": session_id,
            "profile_id": profile_id,
            "title": observation.title,
            "text": observation.text,
            "headings": observation.headings,
            "links": observation.links.into_iter().map(|link| serde_json::json!({
                "label": link.label,
                "destination": link.destination,
            })).collect::<Vec<_>>(),
            "controls": observation.controls,
            "blocked_remote_instruction_count": observation.blocked_remote_instruction_count,
            "blocked_popup_count": observation.blocked_popup_count,
            "remote_content_is_untrusted": observation.remote_content_is_untrusted,
            "truncated": observation.truncated,
        })
    );
    browser
        .close_session(&profile_id, &session_id)
        .map_err(|error| error.to_string())?;
    Ok(())
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
