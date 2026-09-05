use std::fmt::Write as _;

use keith_protocol::{MessageProjection, MessageRole, PresenceState, TurnTerminalStatus};
use ratatui::Frame;
use ratatui::layout::{Constraint, Direction, Layout, Position, Rect};
use ratatui::style::{Color, Modifier, Style};
use ratatui::text::{Line, Span, Text};
use ratatui::widgets::{Clear, List, ListItem, Paragraph, Wrap};
use unicode_width::{UnicodeWidthChar, UnicodeWidthStr};

use crate::{ColorMode, TuiApp, TuiOverlay};

#[derive(Clone, Copy)]
pub(crate) struct Palette {
    canvas: Color,
    layer: Color,
    selected: Color,
    text: Color,
    muted: Color,
    accent: Color,
    danger: Color,
}

pub fn render(frame: &mut Frame<'_>, app: &TuiApp) {
    let area = frame.area();
    let palette = palette(app.accessibility.color_mode);
    frame.render_widget(
        Paragraph::new("").style(Style::new().bg(palette.canvas)),
        area,
    );

    let activity_height = u16::from(activity_label(app).is_some());
    let header_height = u16::from(area.height >= 8);
    let completion_height = completion_height(app, area);
    let composer_height = composer_height(app, area);
    let rows = Layout::default()
        .direction(Direction::Vertical)
        .constraints([
            Constraint::Length(header_height),
            Constraint::Min(1),
            Constraint::Length(activity_height),
            Constraint::Length(completion_height),
            Constraint::Length(composer_height),
            Constraint::Length(1),
        ])
        .split(area);

    if header_height > 0 {
        render_header(frame, app, rows[0], palette);
    }
    render_conversation(frame, app, rows[1], palette);
    if let Some(activity) = activity_label(app) {
        render_activity(frame, app, activity, rows[2], palette);
    }
    if completion_height > 0 {
        render_completions(frame, app, rows[3], palette);
    }
    render_composer(frame, app, rows[4], palette);
    render_status(frame, app, rows[5], palette);
    if let Some(overlay) = app.overlay {
        render_overlay(frame, app, overlay, area, palette);
    }
}

fn render_header(frame: &mut Frame<'_>, app: &TuiApp, area: Rect, palette: Palette) {
    let conversation = app.current_session_label();
    let title = if area.width >= 52 {
        format!(" Keith   {}", terminal_safe(&conversation))
    } else {
        " Keith".into()
    };
    frame.render_widget(
        Paragraph::new(Line::from(vec![Span::styled(
            title,
            Style::new().fg(palette.text).add_modifier(Modifier::BOLD),
        )]))
        .style(Style::new().bg(palette.canvas)),
        area,
    );
}

pub(crate) fn render_conversation(
    frame: &mut Frame<'_>,
    app: &TuiApp,
    area: Rect,
    palette: Palette,
) {
    let width = usize::from(area.width.saturating_sub(2)).max(1);
    let mut chunks = Vec::new();
    let messages = app
        .reducer
        .as_ref()
        .map_or(&[][..], |reducer| reducer.snapshot().messages.as_slice());
    let mut pending = app.pending_prompts().peekable();
    for message_index in 0..=messages.len() {
        while pending.peek().is_some_and(|prompt| {
            TuiApp::pending_prompt_anchor(prompt).min(messages.len()) <= message_index
        }) {
            if let Some(prompt) = pending.next() {
                chunks.push(pending_prompt_lines(prompt, width, palette));
            }
        }
        if let Some(message) = messages.get(message_index) {
            let lines = if message.role == MessageRole::Tool && !app.tool_details_expanded() {
                compact_tool_lines(message, width, palette)
            } else {
                transcript_lines(message, width, palette)
            };
            chunks.push(lines);
        }
    }
    let mut lines = chunks.into_iter().flatten().collect::<Vec<_>>();
    if lines.is_empty() {
        let empty = if app.reducer.is_some() {
            "  Tell Keith what you want taken care of."
        } else {
            app.empty_conversation_message()
        };
        lines.push(Line::from(Span::styled(
            empty,
            Style::new().fg(palette.muted),
        )));
    }
    let visible = usize::from(area.height);
    let max_scroll = lines.len().saturating_sub(visible.min(lines.len()));
    let scroll_from_end = app.scroll_from_end.min(max_scroll);
    let end = lines.len().saturating_sub(scroll_from_end).min(lines.len());
    let start = end.saturating_sub(visible);
    frame.render_widget(
        Paragraph::new(Text::from(lines[start..end].to_vec()))
            .style(Style::new().bg(palette.canvas).fg(palette.text))
            .wrap(Wrap { trim: false }),
        area,
    );
}

fn pending_prompt_lines(
    prompt: &crate::PendingPrompt,
    width: usize,
    palette: Palette,
) -> Vec<Line<'static>> {
    let (state, state_style) = match prompt.state {
        crate::PendingPromptState::Sending => (Some("sending"), Style::new().fg(palette.muted)),
        crate::PendingPromptState::Submitted => (None, Style::new().fg(palette.muted)),
        crate::PendingPromptState::Failed => {
            (Some("failed to send"), Style::new().fg(palette.danger))
        }
    };
    prefixed_message_lines(
        &prompt.text,
        width,
        "› ",
        Style::new().fg(palette.accent).add_modifier(Modifier::BOLD),
        Style::new().fg(palette.text),
        state.map(|label| (label, state_style)),
        Some(palette.layer),
        palette,
    )
}

pub fn settled_transcript_lines(
    message: &MessageProjection,
    width: u16,
    color_mode: ColorMode,
) -> Vec<Line<'static>> {
    transcript_lines(message, usize::from(width).max(1), palette(color_mode))
}

fn transcript_lines(
    message: &MessageProjection,
    width: usize,
    palette: Palette,
) -> Vec<Line<'static>> {
    let (prefix, prefix_style, body_style, background) = match message.role {
        MessageRole::User => (
            "› ",
            Style::new().fg(palette.accent).add_modifier(Modifier::BOLD),
            Style::new().fg(palette.text),
            Some(palette.layer),
        ),
        MessageRole::Assistant => (
            "• ",
            Style::new().fg(palette.accent),
            Style::new().fg(palette.text),
            None,
        ),
        MessageRole::Tool => (
            "↳ ",
            Style::new().fg(palette.muted),
            Style::new().fg(palette.muted),
            None,
        ),
        MessageRole::System => (
            "! ",
            Style::new().fg(palette.danger).add_modifier(Modifier::BOLD),
            Style::new().fg(palette.text),
            None,
        ),
    };
    prefixed_message_lines(
        &message.text,
        width,
        prefix,
        prefix_style,
        body_style,
        None,
        background,
        palette,
    )
}

#[allow(clippy::too_many_arguments)]
fn prefixed_message_lines(
    text: &str,
    width: usize,
    prefix: &'static str,
    prefix_style: Style,
    body_style: Style,
    suffix: Option<(&'static str, Style)>,
    background: Option<Color>,
    palette: Palette,
) -> Vec<Line<'static>> {
    let body_width = width.saturating_sub(2).max(1);
    let wrapped = wrap_preserving_indentation(text, body_width);
    let mut lines = wrapped
        .into_iter()
        .enumerate()
        .map(|(index, line)| {
            let mut spans = vec![Span::styled(
                if index == 0 { prefix } else { "  " },
                prefix_style,
            )];
            spans.extend(styled_transcript_spans(&line, body_style, palette));
            if index == 0
                && let Some((label, style)) = &suffix
            {
                spans.push(Span::styled(format!("  · {label}"), *style));
            }
            let line = Line::from(spans);
            if let Some(color) = background {
                line.style(Style::new().bg(color))
            } else {
                line
            }
        })
        .collect::<Vec<_>>();
    if lines.is_empty() {
        lines.push(Line::from(Span::styled(prefix, prefix_style)));
    }
    lines.push(Line::default());
    lines
}

fn compact_tool_lines(
    message: &MessageProjection,
    width: usize,
    palette: Palette,
) -> Vec<Line<'static>> {
    let summary = tool_summary(&message.text);
    let available = width.saturating_sub(4).max(1);
    let summary = wrap_preserving_indentation(&summary, available)
        .into_iter()
        .next()
        .unwrap_or_else(|| "Tool activity".into());
    vec![Line::from(vec![
        Span::styled("  • ", Style::new().fg(palette.accent)),
        Span::styled(summary, Style::new().fg(palette.muted)),
        Span::styled("  (Ctrl-T for details)", Style::new().fg(palette.muted)),
    ])]
}

fn tool_summary(value: &str) -> String {
    let safe = terminal_safe(value);
    for key in ["tool", "name", "source_kind", "kind", "status"] {
        let needle = format!("\"{key}\"");
        let Some(start) = safe.find(&needle) else {
            continue;
        };
        let rest = &safe[start + needle.len()..];
        let Some(value) = rest.split_once(':').map(|(_, value)| value.trim_start()) else {
            continue;
        };
        if let Some(value) = value.strip_prefix('"')
            && let Some(end) = value.find('"')
        {
            return value[..end].replace('_', " ");
        }
    }
    if safe.trim_start().starts_with('{') || safe.trim_start().starts_with('[') {
        "Tool activity completed".into()
    } else {
        safe.lines()
            .find(|line| !line.trim().is_empty())
            .map(str::trim)
            .map_or_else(|| "Tool activity completed".into(), str::to_owned)
    }
}

fn styled_transcript_spans(line: &str, body_style: Style, palette: Palette) -> Vec<Span<'static>> {
    let trimmed = line.trim_start();
    if trimmed.starts_with("```") {
        return vec![Span::styled(
            line.to_owned(),
            Style::new().fg(palette.muted),
        )];
    }
    if trimmed.starts_with('#') {
        return inline_markdown_spans(
            trimmed.trim_start_matches('#').trim_start(),
            body_style.add_modifier(Modifier::BOLD),
            palette,
        );
    }
    inline_markdown_spans(line, body_style, palette)
}

fn inline_markdown_spans(value: &str, base: Style, palette: Palette) -> Vec<Span<'static>> {
    let characters = value.chars().collect::<Vec<_>>();
    let mut spans = Vec::new();
    let mut buffer = String::new();
    let mut bold = false;
    let mut italic = false;
    let mut code = false;
    let mut index = 0;
    while index < characters.len() {
        let marker = if characters[index] == '`' {
            Some((1, "`"))
        } else if characters[index] == '*'
            && characters.get(index + 1).is_some_and(|next| *next == '*')
        {
            Some((2, "**"))
        } else if characters[index] == '*' {
            Some((1, "*"))
        } else {
            None
        };
        let Some((width, marker_text)) = marker else {
            buffer.push(characters[index]);
            index += 1;
            continue;
        };
        let has_closing = characters[index + width..]
            .windows(width)
            .any(|window| window.iter().collect::<String>() == marker_text);
        let currently_open = match marker_text {
            "`" => code,
            "**" => bold,
            "*" => italic,
            _ => false,
        };
        if !currently_open && !has_closing {
            buffer.extend(characters[index..index + width].iter().copied());
            index += width;
            continue;
        }
        push_inline_span(&mut spans, &mut buffer, base, bold, italic, code, palette);
        match marker_text {
            "`" => code = !code,
            "**" => bold = !bold,
            "*" => italic = !italic,
            _ => {}
        }
        index += width;
    }
    push_inline_span(&mut spans, &mut buffer, base, bold, italic, code, palette);
    spans
}

fn push_inline_span(
    spans: &mut Vec<Span<'static>>,
    buffer: &mut String,
    base: Style,
    bold: bool,
    italic: bool,
    code: bool,
    palette: Palette,
) {
    if buffer.is_empty() {
        return;
    }
    let mut style = if code { base.fg(palette.accent) } else { base };
    if bold {
        style = style.add_modifier(Modifier::BOLD);
    }
    if italic {
        style = style.add_modifier(Modifier::ITALIC);
    }
    spans.push(Span::styled(std::mem::take(buffer), style));
}

fn wrap_preserving_indentation(value: &str, width: usize) -> Vec<String> {
    let safe = terminal_safe(value);
    let mut output = Vec::new();
    for logical in safe.split('\n') {
        if logical.is_empty() {
            output.push(String::new());
            continue;
        }
        let indentation = logical
            .chars()
            .take_while(|character| matches!(character, ' ' | '\t'))
            .collect::<String>();
        let continuation = if UnicodeWidthStr::width(indentation.as_str()) < width {
            indentation.clone()
        } else {
            String::new()
        };
        let content = logical.trim_start_matches([' ', '\t']);
        let mut current = indentation;
        for word in content.split_whitespace() {
            let separator = usize::from(!current.trim().is_empty());
            let candidate_width = UnicodeWidthStr::width(current.as_str())
                .saturating_add(separator)
                .saturating_add(UnicodeWidthStr::width(word));
            if candidate_width <= width {
                if separator > 0 {
                    current.push(' ');
                }
                current.push_str(word);
                continue;
            }
            if !current.trim().is_empty() {
                output.push(std::mem::take(&mut current));
                current.push_str(&continuation);
            }
            append_wrapped_word(&mut output, &mut current, word, width, &continuation);
        }
        if !current.is_empty() {
            output.push(current);
        }
    }
    output
}

fn append_wrapped_word(
    output: &mut Vec<String>,
    current: &mut String,
    word: &str,
    width: usize,
    continuation: &str,
) {
    for character in word.chars() {
        let character_width = UnicodeWidthChar::width(character).unwrap_or(0);
        if !current.is_empty()
            && UnicodeWidthStr::width(current.as_str()).saturating_add(character_width) > width
        {
            output.push(std::mem::take(current));
            current.push_str(continuation);
        }
        current.push(character);
    }
}

fn completion_height(app: &TuiApp, area: Rect) -> u16 {
    if area.height < 12 || area.width < 28 {
        return 0;
    }
    u16::try_from(app.slash_suggestions().len().min(6)).unwrap_or(0)
}

fn composer_height(app: &TuiApp, area: Rect) -> u16 {
    if area.height < 7 {
        return 2;
    }
    let width = usize::from(area.width.saturating_sub(3)).max(1);
    let rows = editor_lines(&app.composer, width).len();
    u16::try_from(rows.saturating_add(1).clamp(3, 8)).unwrap_or(8)
}

fn render_completions(frame: &mut Frame<'_>, app: &TuiApp, area: Rect, palette: Palette) {
    let suggestions = app.slash_suggestions();
    let items = suggestions
        .into_iter()
        .take(usize::from(area.height))
        .enumerate()
        .map(|(index, (command, label))| {
            let selected = index == app.completion_selection;
            Line::from(vec![
                Span::styled(
                    format!("  {command:<18}"),
                    Style::new()
                        .fg(if selected {
                            palette.text
                        } else {
                            palette.accent
                        })
                        .add_modifier(if selected {
                            Modifier::BOLD
                        } else {
                            Modifier::empty()
                        }),
                ),
                Span::styled(label, Style::new().fg(palette.muted)),
            ])
            .style(Style::new().bg(if selected {
                palette.selected
            } else {
                palette.layer
            }))
        })
        .collect::<Vec<_>>();
    frame.render_widget(
        Paragraph::new(items).style(Style::new().bg(palette.layer)),
        area,
    );
}

fn render_composer(frame: &mut Frame<'_>, app: &TuiApp, area: Rect, palette: Palette) {
    let enabled = app.attached_session.is_some();
    let prompt = if enabled {
        terminal_safe(&app.composer)
    } else {
        "Choose a conversation before sending a message".into()
    };
    let hint = if area.width >= 64 {
        if app.turn_is_active() {
            "Enter queue   Ctrl-K steer   Esc interrupt"
        } else {
            "Enter send   Alt-Enter newline   / commands"
        }
    } else if area.width >= 36 {
        if app.turn_is_active() {
            "Enter queue   Esc interrupt"
        } else {
            "Enter send   / commands"
        }
    } else {
        "Enter send"
    };
    let content_width = usize::from(area.width.saturating_sub(3)).max(1);
    let content_rows = editor_lines(&prompt, content_width);
    let body_height = usize::from(area.height.saturating_sub(1)).max(1);
    let (cursor_row, cursor_column) = editor_cursor(&prompt, app.cursor_byte, content_width);
    let scroll = cursor_row.saturating_sub(body_height.saturating_sub(1));
    let mut lines = content_rows
        .into_iter()
        .skip(scroll)
        .take(body_height)
        .enumerate()
        .map(|(index, line)| {
            Line::from(vec![
                Span::styled(
                    if index == 0 && scroll == 0 {
                        " › "
                    } else {
                        "   "
                    },
                    Style::new().fg(palette.accent),
                ),
                Span::styled(
                    line,
                    Style::new().fg(if enabled { palette.text } else { palette.muted }),
                ),
            ])
        })
        .collect::<Vec<_>>();
    while lines.len() < body_height {
        lines.push(Line::from("   "));
    }
    lines.push(Line::from(Span::styled(
        format!("   {hint}"),
        Style::new().fg(palette.muted),
    )));
    frame.render_widget(
        Paragraph::new(Text::from(lines)).style(Style::new().bg(palette.layer).fg(palette.text)),
        area,
    );
    if enabled && area.width > 3 && area.height > 0 {
        let x = area
            .x
            .saturating_add(3)
            .saturating_add(u16::try_from(cursor_column).unwrap_or(u16::MAX))
            .min(area.right().saturating_sub(1));
        let y = area
            .y
            .saturating_add(u16::try_from(cursor_row.saturating_sub(scroll)).unwrap_or(u16::MAX))
            .min(area.bottom().saturating_sub(2));
        frame.set_cursor_position(Position::new(x, y));
    }
}

fn editor_lines(value: &str, width: usize) -> Vec<String> {
    let mut lines = vec![String::new()];
    let mut column = 0_usize;
    for character in value.chars() {
        if character == '\n' {
            lines.push(String::new());
            column = 0;
            continue;
        }
        let character_width = UnicodeWidthChar::width(character).unwrap_or(0);
        if column > 0 && column.saturating_add(character_width) > width {
            lines.push(String::new());
            column = 0;
        }
        lines
            .last_mut()
            .expect("editor always has a line")
            .push(character);
        column = column.saturating_add(character_width);
    }
    lines
}

fn editor_cursor(value: &str, cursor_byte: usize, width: usize) -> (usize, usize) {
    let mut row = 0_usize;
    let mut column = 0_usize;
    for (index, character) in value.char_indices() {
        if index >= cursor_byte {
            break;
        }
        if character == '\n' {
            row = row.saturating_add(1);
            column = 0;
            continue;
        }
        let character_width = UnicodeWidthChar::width(character).unwrap_or(0);
        if column > 0 && column.saturating_add(character_width) > width {
            row = row.saturating_add(1);
            column = 0;
        }
        column = column.saturating_add(character_width);
    }
    (row, column)
}

fn render_status(frame: &mut Frame<'_>, app: &TuiApp, area: Rect, palette: Palette) {
    let (connection, mut color) = if app.reconnecting {
        ("Reconnecting", palette.muted)
    } else if app.connected {
        ("Connected", palette.accent)
    } else {
        ("Offline", palette.danger)
    };
    let terminal = app.reducer.as_ref().and_then(|reducer| {
        reducer
            .snapshot()
            .terminal
            .as_ref()
            .map(|terminal| match terminal.status {
                TurnTerminalStatus::Completed => "Completed",
                TurnTerminalStatus::Failed => "Could not finish",
                TurnTerminalStatus::Cancelled => "Stopped",
                TurnTerminalStatus::Exhausted => "Reached its limit",
            })
    });
    let mut right = connection.to_owned();
    if let Some(terminal) = terminal {
        let _ = write!(right, " · {terminal}");
    }
    if let Some(snapshot) = app
        .reducer
        .as_ref()
        .map(keith_ui_model::ProjectionReducer::snapshot)
    {
        if let Some(tool) = snapshot.tools.iter().rev().find(|tool| !tool.terminal) {
            let _ = write!(
                right,
                " · {} {}",
                tool.tool.as_deref().unwrap_or("tool"),
                tool.state
            );
        }
        if area.width >= 72 {
            let tokens = snapshot
                .usage
                .input_tokens
                .saturating_add(snapshot.usage.output_tokens);
            if tokens > 0 {
                let _ = write!(right, " · {tokens} tokens");
            }
        }
    }
    let left = if let Some(log) = app.latest_log()
        && (log.contains("failed") || log.contains("rejected") || log.contains("error"))
    {
        color = palette.danger;
        format!(" ! {log}")
    } else if app.scroll_from_end > 0 {
        format!(" ↑ {} lines · End to return", app.scroll_from_end)
    } else if area.width >= 74 {
        " ? shortcuts   Ctrl-P commands".to_owned()
    } else {
        " ? shortcuts".to_owned()
    };
    if area.width >= 54 {
        right.push_str(" · Ctrl-D exit");
    }
    let available = usize::from(area.width);
    let left_width = UnicodeWidthStr::width(left.as_str());
    let right_width = UnicodeWidthStr::width(right.as_str());
    let status = if left_width.saturating_add(right_width).saturating_add(1) <= available {
        format!(
            "{left}{}{right}",
            " ".repeat(available.saturating_sub(left_width + right_width))
        )
    } else {
        let fallback = format!(" {connection}");
        if available >= 22 {
            format!("{left} ·{fallback}")
        } else {
            fallback
        }
    };
    frame.render_widget(
        Paragraph::new(status).style(Style::new().bg(palette.canvas).fg(color)),
        area,
    );
}

fn render_activity(
    frame: &mut Frame<'_>,
    app: &TuiApp,
    activity: &str,
    area: Rect,
    palette: Palette,
) {
    let elapsed = app.turn_elapsed().unwrap_or_default();
    let seconds = elapsed.as_secs();
    let duration = if seconds >= 60 {
        format!("{}m {:02}s", seconds / 60, seconds % 60)
    } else {
        format!("{seconds}s")
    };
    let indicator = if app.accessibility.reduced_motion {
        "•"
    } else {
        const FRAMES: [&str; 10] = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
        let index = usize::try_from(elapsed.as_millis() / 100).unwrap_or(0) % FRAMES.len();
        FRAMES[index]
    };
    let tail = if area.width >= 42 {
        format!(" ({duration} · Esc to interrupt)")
    } else {
        format!(" · {duration}")
    };
    frame.render_widget(
        Paragraph::new(Line::from(vec![
            Span::styled(format!("  {indicator} "), Style::new().fg(palette.accent)),
            Span::styled(activity.to_owned(), Style::new().fg(palette.text)),
            Span::styled(tail, Style::new().fg(palette.muted)),
        ]))
        .style(Style::new().bg(palette.canvas)),
        area,
    );
}

fn activity_label(app: &TuiApp) -> Option<&'static str> {
    if app
        .reducer
        .as_ref()
        .is_some_and(|reducer| reducer.snapshot().terminal.is_some())
    {
        return None;
    }
    let state = app
        .reducer
        .as_ref()
        .map(|reducer| reducer.snapshot().presence.state);
    match state {
        Some(PresenceState::Thinking) => Some("Thinking"),
        Some(PresenceState::UsingTools) => Some("Using tools"),
        Some(PresenceState::WaitingChild) => Some("Waiting for delegated work"),
        Some(PresenceState::WaitingExternal) => Some("Waiting for a response"),
        Some(PresenceState::PausedForUser) => Some("Needs your input"),
        Some(PresenceState::Scheduled) => Some("Scheduled"),
        Some(PresenceState::Available | PresenceState::Completed | PresenceState::Failed)
        | None
            if app.turn_is_active() =>
        {
            Some("Working")
        }
        Some(PresenceState::Available | PresenceState::Completed | PresenceState::Failed)
        | None => None,
    }
}

fn render_overlay(
    frame: &mut Frame<'_>,
    app: &TuiApp,
    overlay: TuiOverlay,
    area: Rect,
    palette: Palette,
) {
    let overlay_area = centered(area, 76, 22);
    frame.render_widget(Clear, overlay_area);
    frame.render_widget(
        Paragraph::new("").style(Style::new().bg(palette.layer)),
        overlay_area,
    );
    let rows = Layout::default()
        .direction(Direction::Vertical)
        .margin(1)
        .constraints([
            Constraint::Length(1),
            Constraint::Length(2),
            Constraint::Min(1),
            Constraint::Length(1),
        ])
        .split(overlay_area);
    frame.render_widget(
        Paragraph::new(overlay.label()).style(
            Style::new()
                .bg(palette.layer)
                .fg(palette.text)
                .add_modifier(Modifier::BOLD),
        ),
        rows[0],
    );
    let filter = if app.overlay_query.is_empty() {
        "Type to filter".into()
    } else {
        format!("Filter: {}", terminal_safe(&app.overlay_query))
    };
    frame.render_widget(
        Paragraph::new(filter).style(Style::new().bg(palette.layer).fg(palette.muted)),
        rows[1],
    );
    let items = app.overlay_rows();
    let list = if items.is_empty() {
        List::new([ListItem::new(overlay_empty(overlay)).style(Style::new().fg(palette.muted))])
    } else {
        List::new(items.into_iter().enumerate().map(|(index, value)| {
            let style = if index == app.overlay_selection {
                Style::new()
                    .bg(palette.selected)
                    .fg(palette.text)
                    .add_modifier(Modifier::BOLD)
            } else {
                Style::new().bg(palette.layer).fg(palette.text)
            };
            ListItem::new(format!(" {}", terminal_safe(&value))).style(style)
        }))
    };
    frame.render_widget(list, rows[2]);
    let help = match overlay {
        TuiOverlay::Approvals => "Alt-A allow once   Alt-D deny   Esc close",
        TuiOverlay::Evolution => "Enter approve/revert   Tab next   Esc close",
        TuiOverlay::Help => "Type to filter   Tab next view   Esc close",
        _ => "Enter choose   Tab next   Esc close",
    };
    frame.render_widget(
        Paragraph::new(help).style(Style::new().bg(palette.layer).fg(palette.muted)),
        rows[3],
    );
}

const fn overlay_empty(overlay: TuiOverlay) -> &'static str {
    match overlay {
        TuiOverlay::Sessions => "No conversations match. Clear the filter to see everything.",
        TuiOverlay::Commands => "No commands match.",
        TuiOverlay::Models => "No models match.",
        TuiOverlay::Approvals => "Keith does not need a decision right now.",
        TuiOverlay::Work => "Nothing is in progress. Ask Keith to take care of something.",
        TuiOverlay::Memory => "No saved context is available for this conversation.",
        TuiOverlay::Diagnostics => "Attach a conversation to inspect diagnostics.",
        TuiOverlay::Evolution => "No evolution history is available.",
        TuiOverlay::Services => "No external service matches this filter.",
        TuiOverlay::Help => "No shortcut matches this filter.",
    }
}

fn centered(area: Rect, max_width: u16, max_height: u16) -> Rect {
    let width = area.width.saturating_sub(2).min(max_width).max(1);
    let height = area.height.saturating_sub(2).min(max_height).max(1);
    Rect::new(
        area.x.saturating_add(area.width.saturating_sub(width) / 2),
        area.y
            .saturating_add(area.height.saturating_sub(height) / 2),
        width,
        height,
    )
}

pub(crate) fn terminal_safe(value: &str) -> String {
    value
        .chars()
        .map(|character| {
            if character.is_control() && !matches!(character, '\n' | '\t') {
                '\u{fffd}'
            } else {
                character
            }
        })
        .collect()
}

const fn palette(mode: ColorMode) -> Palette {
    match mode {
        ColorMode::TrueColor => Palette {
            canvas: Color::Reset,
            layer: Color::Reset,
            selected: Color::DarkGray,
            text: Color::Reset,
            muted: Color::DarkGray,
            accent: Color::Cyan,
            danger: Color::Red,
        },
        ColorMode::Ansi256 => Palette {
            canvas: Color::Reset,
            layer: Color::Indexed(236),
            selected: Color::Indexed(239),
            text: Color::Indexed(255),
            muted: Color::Indexed(248),
            accent: Color::Indexed(78),
            danger: Color::Indexed(210),
        },
        ColorMode::NoColor => Palette {
            canvas: Color::Reset,
            layer: Color::Reset,
            selected: Color::Reset,
            text: Color::Reset,
            muted: Color::Reset,
            accent: Color::Reset,
            danger: Color::Reset,
        },
        ColorMode::HighContrast => Palette {
            canvas: Color::Black,
            layer: Color::DarkGray,
            selected: Color::White,
            text: Color::White,
            muted: Color::Gray,
            accent: Color::LightGreen,
            danger: Color::LightRed,
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn wrapping_prefers_words_and_only_splits_tokens_that_cannot_fit() {
        assert_eq!(
            wrap_preserving_indentation("alpha beta gamma", 10),
            ["alpha beta", "gamma"]
        );
        let long = wrap_preserving_indentation("supercalifragilistic", 6);
        assert!(
            long.iter()
                .all(|line| UnicodeWidthStr::width(line.as_str()) <= 6)
        );
        assert_eq!(long.concat(), "supercalifragilistic");
    }

    #[test]
    fn tool_json_is_collapsed_and_multiline_editor_cursor_tracks_display_cells() {
        let summary = tool_summary(r#"{"source_kind":"durable_memory","payload":{"x":1}}"#);
        assert_eq!(summary, "durable memory");
        assert!(!summary.contains('{'));

        let value = "ab界d\nsecond";
        assert_eq!(editor_lines(value, 4), ["ab界", "d", "seco", "nd"]);
        assert_eq!(editor_cursor(value, "ab界d\nsec".len(), 4), (2, 3));

        let spans = inline_markdown_spans(
            "plain *emphasis* **strong** `code`",
            Style::new(),
            palette(ColorMode::NoColor),
        );
        assert_eq!(
            spans
                .iter()
                .map(|span| span.content.as_ref())
                .collect::<String>(),
            "plain emphasis strong code"
        );
        assert!(
            spans
                .iter()
                .any(|span| span.style.add_modifier.contains(Modifier::ITALIC))
        );
        assert!(
            spans
                .iter()
                .any(|span| span.style.add_modifier.contains(Modifier::BOLD))
        );
    }
}
