use std::process::ExitCode;

use keith_agent_web::{ServerArguments, WebServer};

#[tokio::main]
async fn main() -> ExitCode {
    let arguments = match ServerArguments::parse(std::env::args_os()) {
        Ok(Some(arguments)) => arguments,
        Ok(None) => return ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("agent-web: {error}");
            return ExitCode::FAILURE;
        }
    };
    let config = match arguments.load_config() {
        Ok(config) => config,
        Err(error) => {
            eprintln!("agent-web: {error}");
            return ExitCode::FAILURE;
        }
    };
    match WebServer::new(config) {
        Ok(server) => match server.run().await {
            Ok(()) => ExitCode::SUCCESS,
            Err(error) => {
                eprintln!("agent-web: {error}");
                ExitCode::FAILURE
            }
        },
        Err(error) => {
            eprintln!("agent-web: {error}");
            ExitCode::FAILURE
        }
    }
}
