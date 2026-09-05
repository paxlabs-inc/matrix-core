#![forbid(unsafe_code)]

use std::fs;
use std::io::{self, IsTerminal, stdout};
use std::process::Command;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use crossterm::event::{self, DisableBracketedPaste, EnableBracketedPaste, Event, KeyEventKind};
use crossterm::execute;
use keith_agent_tui::{
    Accessibility, AgentCommandDispatcher, AgentConnectionClient, AppAction, DispatchEvent, TuiApp,
    TuiArguments, render,
};
use keith_protocol::WireMessage;
use signal_hook::consts::{SIGINT, SIGTERM};

enum LoopExit {
    Quit,
    ExternalEditor,
}

fn run() -> Result<(), String> {
    let Some(arguments) = TuiArguments::parse(std::env::args_os())? else {
        return Ok(());
    };
    if !io::stdin().is_terminal() || !io::stdout().is_terminal() {
        return Err("agent-tui requires an interactive terminal".into());
    }
    let shutdown = Arc::new(AtomicBool::new(false));
    signal_hook::flag::register(SIGTERM, Arc::clone(&shutdown))
        .map_err(|error| format!("failed to register SIGTERM: {error}"))?;
    signal_hook::flag::register(SIGINT, Arc::clone(&shutdown))
        .map_err(|error| format!("failed to register SIGINT: {error}"))?;
    let accessibility = Accessibility {
        color_mode: arguments.color_mode,
        reduced_motion: arguments.reduced_motion,
    };
    let mut app = TuiApp::new(accessibility);
    let client = AgentConnectionClient::connect(
        arguments.mode,
        app.client_id.clone(),
        None,
        arguments.startup_timeout,
    )
    .map_err(|error| error.to_string())?;
    let mut dispatcher = AgentCommandDispatcher::new(client, arguments.startup_timeout);
    app.connected = true;
    app.list_profiles();
    app.list_sessions();
    if let Some(session_id) = arguments.session_id {
        app.attach(session_id);
    }

    loop {
        let mut terminal = init_terminal().map_err(|error| error.to_string())?;
        let result = event_loop(&mut terminal, &mut app, &mut dispatcher, &shutdown);
        let restored = restore_terminal();
        let exit = result.map_err(|error| error.to_string())?;
        restored.map_err(|error| error.to_string())?;
        match exit {
            LoopExit::Quit => return Ok(()),
            LoopExit::ExternalEditor => edit_composer(&mut app)?,
        }
    }
}

fn init_terminal() -> io::Result<ratatui::DefaultTerminal> {
    let mut terminal = ratatui::try_init()?;
    if let Err(error) = execute!(stdout(), EnableBracketedPaste) {
        let _ = ratatui::try_restore();
        return Err(error);
    }
    if let Err(error) = terminal.clear() {
        let _ = execute!(stdout(), DisableBracketedPaste);
        let _ = ratatui::try_restore();
        return Err(error);
    }
    Ok(terminal)
}

fn restore_terminal() -> io::Result<()> {
    let paste_result = execute!(stdout(), DisableBracketedPaste);
    let terminal_result = ratatui::try_restore();
    paste_result.and(terminal_result)
}

fn event_loop(
    terminal: &mut ratatui::DefaultTerminal,
    app: &mut TuiApp,
    dispatcher: &mut AgentCommandDispatcher,
    shutdown: &AtomicBool,
) -> io::Result<LoopExit> {
    let mut dirty = true;
    while !shutdown.load(Ordering::Acquire) && !app.quit {
        while let Some(event) = dispatcher.try_next() {
            dirty = true;
            match event {
                DispatchEvent::Message(message) => {
                    let message = *message;
                    if matches!(message, WireMessage::CommandResult(_)) {
                        app.command_finished();
                    }
                    app.apply_wire_message(message);
                }
                DispatchEvent::CommandFailed(error) => {
                    app.command_finished();
                    app.report_command_failure(None, error);
                }
                DispatchEvent::Reconnecting => app.report_reconnecting(),
                DispatchEvent::Reconnected => {
                    app.report_reconnected();
                    app.resume_attached_session();
                }
                DispatchEvent::ReconnectFailed(error) => app.report_reconnect_failure(error),
            }
        }
        while let Some(command) = app.next_command() {
            dirty = true;
            let envelope = app.command_envelope(command);
            let command_id = envelope.command_id.clone();
            match dispatcher.dispatch(envelope) {
                Ok(()) => app.command_dispatched(&command_id),
                Err(error) => app.report_command_failure(Some(&command_id), error),
            }
        }
        if dirty {
            terminal.draw(|frame| render(frame, app))?;
            dirty = false;
        }
        if !event::poll(Duration::from_millis(100))? {
            if app.turn_is_active() {
                dirty = true;
            }
            continue;
        }
        match event::read()? {
            Event::Key(key) if key.kind == KeyEventKind::Press => match app.handle_key(key) {
                AppAction::Quit => return Ok(LoopExit::Quit),
                AppAction::OpenExternalEditor => return Ok(LoopExit::ExternalEditor),
                AppAction::Redraw => dirty = true,
                AppAction::None => {}
            },
            Event::Paste(content) => {
                app.handle_paste(&content);
                dirty = true;
            }
            Event::Resize(_, _) | Event::FocusGained => dirty = true,
            Event::FocusLost | Event::Mouse(_) | Event::Key(_) => {}
        }
    }
    Ok(LoopExit::Quit)
}

fn edit_composer(app: &mut TuiApp) -> Result<(), String> {
    let editor = std::env::var_os("VISUAL")
        .or_else(|| std::env::var_os("EDITOR"))
        .ok_or_else(|| "VISUAL or EDITOR must name an external editor".to_owned())?;
    let directory = tempfile::tempdir().map_err(|error| error.to_string())?;
    let path = directory.path().join("message.txt");
    fs::write(&path, &app.composer).map_err(|error| error.to_string())?;
    let status = Command::new(editor)
        .arg(&path)
        .status()
        .map_err(|error| error.to_string())?;
    if !status.success() {
        return Err(format!("external editor exited with {status}"));
    }
    let content = fs::read_to_string(path).map_err(|error| error.to_string())?;
    app.replace_composer(content);
    Ok(())
}

fn main() {
    if let Err(error) = run() {
        eprintln!("{error}");
        std::process::exit(1);
    }
}
