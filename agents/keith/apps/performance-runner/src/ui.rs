use std::time::Instant;

use keith_agent_tui::{Accessibility, TuiApp, render};
use keith_agent_types::{MessageId, Revision, Sequence};
use keith_agent_web::bootstrap_payload;
use keith_protocol::{
    DaemonEvent, EventEnvelope, MessageProjection, MessageRole, SessionSnapshot, WireMessage,
};
use ratatui::Terminal;
use ratatui::backend::TestBackend;

use crate::report::Measurements;

pub fn benchmark(snapshot: &SessionSnapshot, iterations: usize) -> Result<Measurements, String> {
    let mut measurements = Measurements::default();
    let mut long = snapshot.clone();
    for index in 0..10_000_u64 {
        long.messages.push(MessageProjection {
            message_id: MessageId::new(),
            final_id: None,
            role: if index % 2 == 0 {
                MessageRole::User
            } else {
                MessageRole::Assistant
            },
            text: format!("performance history message {index} {}", "x".repeat(96)),
            committed: true,
        });
    }
    long.revision = Revision::new(long.revision.get().saturating_add(1));
    long.through_sequence = Sequence::new(long.through_sequence.get().saturating_add(1));

    let mut app = TuiApp::new(Accessibility::default());
    let envelope = EventEnvelope {
        protocol: keith_agent_types::CURRENT_PROTOCOL_VERSION,
        root_tree_id: long.session.root_tree_id.clone(),
        generation: long.generation,
        first_sequence: long.through_sequence,
        sequence: long.through_sequence,
        occurred_at: keith_agent_types::UtcTimestamp::now().map_err(|error| error.to_string())?,
        event: DaemonEvent::Snapshot(Box::new(long.clone())),
    };
    let started = Instant::now();
    app.apply_wire_message(WireMessage::Event(envelope));
    measurements.record(
        "tui_snapshot_projection",
        u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
    );

    let backend = TestBackend::new(120, 40);
    let mut terminal = Terminal::new(backend).map_err(|error| error.to_string())?;
    for _ in 0..5 {
        terminal
            .draw(|frame| render(frame, &app))
            .map_err(|error| error.to_string())?;
    }
    for _ in 0..iterations {
        let started = Instant::now();
        terminal
            .draw(|frame| render(frame, &app))
            .map_err(|error| error.to_string())?;
        measurements.record(
            "tui_render_long_history",
            u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
        );
    }

    let profiles = [keith_protocol::ProfileSummary {
        id: long.session.profile_id.clone(),
        workspace_id: keith_agent_types::WorkspaceId::new(),
        display_name: "Performance profile".into(),
        enabled: true,
    }];
    let sessions = (0..1_000)
        .map(|index| {
            let mut session = long.session.clone();
            session.title = Some(format!("Long session {index} {}", "y".repeat(64)));
            session
        })
        .collect::<Vec<_>>();
    for _ in 0..iterations {
        let started = Instant::now();
        let mut catalog = sessions.clone();
        let payload = bootstrap_payload("performance-csrf", &profiles, &mut catalog, None);
        if payload["sessions"].as_array().map_or(0, Vec::len) != sessions.len() {
            return Err("web bootstrap projection dropped sessions".into());
        }
        measurements.record(
            "web_bootstrap_render_1000_sessions",
            u64::try_from(started.elapsed().as_micros()).unwrap_or(u64::MAX),
        );
    }
    Ok(measurements)
}
