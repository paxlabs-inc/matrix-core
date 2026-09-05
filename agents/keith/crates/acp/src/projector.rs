use keith_protocol::{
    DaemonEvent, EventEnvelope, MessageProjection, MessageRole, SessionSnapshot, TurnTerminalStatus,
};

use crate::{AcpUpdate, AcpUpdateKind};

pub struct AcpUpdateProjector;

impl AcpUpdateProjector {
    pub fn project_event(envelope: &EventEnvelope) -> Vec<AcpUpdate> {
        let prefix = format!(
            "{}:{}:{}",
            envelope.root_tree_id, envelope.generation.0, envelope.sequence.0
        );
        Self::project_daemon_event(&prefix, &envelope.event)
    }

    pub fn project_snapshot(snapshot: &SessionSnapshot) -> Vec<AcpUpdate> {
        let prefix = format!(
            "snapshot:{}:{}:{}",
            snapshot.session.root_tree_id, snapshot.generation.0, snapshot.through_sequence.0
        );
        let mut updates = Vec::new();
        for message in &snapshot.messages {
            if let Some(update) = project_message(&prefix, message) {
                updates.push(update);
            }
        }
        for plan in &snapshot.plans {
            updates.push(AcpUpdate {
                event_id: format!("{prefix}:plan:{}", plan.plan_id),
                kind: AcpUpdateKind::Plan {
                    plan_id: plan.plan_id.to_string(),
                    summary: plan.summary.clone(),
                    state: plan.state.clone(),
                    terminal: plan.terminal,
                },
            });
        }
        for tool in &snapshot.tools {
            updates.push(AcpUpdate {
                event_id: format!("{prefix}:tool:{}", tool.tool_call_id),
                kind: AcpUpdateKind::Tool {
                    tool_call_id: tool.tool_call_id.to_string(),
                    title: tool.tool.clone().unwrap_or_else(|| "Keith tool".to_owned()),
                    state: tool.state.clone(),
                    terminal: tool.terminal,
                },
            });
        }
        updates.push(AcpUpdate {
            event_id: format!("{prefix}:usage"),
            kind: AcpUpdateKind::Usage {
                input_tokens: snapshot.usage.input_tokens,
                output_tokens: snapshot.usage.output_tokens,
                cached_input_tokens: snapshot.usage.cached_input_tokens,
                estimated_cost_microunits: snapshot.usage.estimated_cost_microunits,
            },
        });
        if let Some(terminal) = &snapshot.terminal {
            updates.push(AcpUpdate {
                event_id: format!("{prefix}:terminal:{}", terminal.turn_id),
                kind: AcpUpdateKind::Final {
                    status: terminal.status,
                    detail: terminal.detail.clone(),
                },
            });
        }
        updates
    }

    #[allow(clippy::too_many_lines)]
    fn project_daemon_event(prefix: &str, event: &DaemonEvent) -> Vec<AcpUpdate> {
        let update = match event {
            DaemonEvent::Snapshot(snapshot) => return Self::project_snapshot(snapshot),
            DaemonEvent::AssistantDelta { message_id, text } => AcpUpdate {
                event_id: format!("{prefix}:assistant:{message_id}"),
                kind: AcpUpdateKind::AssistantMessage {
                    message_id: message_id.to_string(),
                    text: text.clone(),
                    committed: false,
                },
            },
            DaemonEvent::MessageCommitted(message) => {
                return project_message(prefix, message).into_iter().collect();
            }
            DaemonEvent::AgentActivity(activity) => {
                if let keith_protocol::AgentActivityKind::StrategyChanged { reason } =
                    &activity.kind
                {
                    AcpUpdate {
                        event_id: format!("{prefix}:thought:{}", activity.sequence),
                        kind: AcpUpdateKind::Thought {
                            text: reason.clone(),
                        },
                    }
                } else {
                    return Vec::new();
                }
            }
            DaemonEvent::PlanChanged(plan) => AcpUpdate {
                event_id: format!("{prefix}:plan:{}", plan.plan_id),
                kind: AcpUpdateKind::Plan {
                    plan_id: plan.plan_id.to_string(),
                    summary: plan.summary.clone(),
                    state: plan.state.clone(),
                    terminal: plan.terminal,
                },
            },
            DaemonEvent::ToolChanged(tool) => AcpUpdate {
                event_id: format!("{prefix}:tool:{}", tool.tool_call_id),
                kind: AcpUpdateKind::Tool {
                    tool_call_id: tool.tool_call_id.to_string(),
                    title: tool.tool.clone().unwrap_or_else(|| "Keith tool".to_owned()),
                    state: tool.state.clone(),
                    terminal: tool.terminal,
                },
            },
            DaemonEvent::UsageChanged(usage) => AcpUpdate {
                event_id: format!("{prefix}:usage"),
                kind: AcpUpdateKind::Usage {
                    input_tokens: usage.input_tokens,
                    output_tokens: usage.output_tokens,
                    cached_input_tokens: usage.cached_input_tokens,
                    estimated_cost_microunits: usage.estimated_cost_microunits,
                },
            },
            DaemonEvent::TurnTerminal(terminal) => AcpUpdate {
                event_id: format!("{prefix}:terminal:{}", terminal.turn_id),
                kind: AcpUpdateKind::Final {
                    status: terminal.status,
                    detail: terminal.detail.clone(),
                },
            },
            DaemonEvent::Warning(error) => AcpUpdate {
                event_id: format!("{prefix}:warning"),
                kind: AcpUpdateKind::Warning {
                    message: error.message.clone(),
                },
            },
            DaemonEvent::Error(error) => AcpUpdate {
                event_id: format!("{prefix}:error"),
                kind: AcpUpdateKind::Failure {
                    message: error.message.clone(),
                    retryable: error.retryable,
                },
            },
            DaemonEvent::EvolutionChanged(evolution) => {
                let mut updates = Vec::new();
                if let Some(active) = &evolution.active
                    && let Some(diff) = &active.readable_diff
                {
                    updates.push(AcpUpdate {
                        event_id: format!("{prefix}:diff:active"),
                        kind: AcpUpdateKind::Diff {
                            title: active.target.clone(),
                            patch: diff.clone(),
                        },
                    });
                }
                for entry in &evolution.ledger {
                    if let Some(diff) = &entry.readable_diff {
                        updates.push(AcpUpdate {
                            event_id: format!("{prefix}:diff:{}", entry.sequence),
                            kind: AcpUpdateKind::Diff {
                                title: entry.summary.clone(),
                                patch: diff.clone(),
                            },
                        });
                    }
                }
                return updates;
            }
            DaemonEvent::CommandRejected(error) => AcpUpdate {
                event_id: format!("{prefix}:rejected"),
                kind: AcpUpdateKind::Failure {
                    message: error.error.message.clone(),
                    retryable: error.error.retryable,
                },
            },
            DaemonEvent::CommandAccepted { .. }
            | DaemonEvent::SessionChanged(_)
            | DaemonEvent::ActionQueued(_)
            | DaemonEvent::ActionStarted(_)
            | DaemonEvent::ActionFinished(_)
            | DaemonEvent::GoalChanged(_)
            | DaemonEvent::ChildChanged(_)
            | DaemonEvent::KernelChanged(_)
            | DaemonEvent::CommitmentChanged(_)
            | DaemonEvent::ScheduleChanged(_)
            | DaemonEvent::WaitChanged(_)
            | DaemonEvent::DeliveryChanged(_)
            | DaemonEvent::MemoryChanged(_)
            | DaemonEvent::PresenceChanged(_)
            | DaemonEvent::ConfirmationRequested { .. }
            | DaemonEvent::ConfirmationResolved { .. } => return Vec::new(),
        };
        vec![update]
    }

    pub fn terminal_status(updates: &[AcpUpdate]) -> Option<TurnTerminalStatus> {
        updates.iter().rev().find_map(|update| {
            if let AcpUpdateKind::Final { status, .. } = &update.kind {
                Some(*status)
            } else {
                None
            }
        })
    }
}

fn project_message(prefix: &str, message: &MessageProjection) -> Option<AcpUpdate> {
    let kind = match message.role {
        MessageRole::Assistant => AcpUpdateKind::AssistantMessage {
            message_id: message.message_id.to_string(),
            text: message.text.clone(),
            committed: message.committed,
        },
        MessageRole::Tool => AcpUpdateKind::ToolOutput {
            message_id: message.message_id.to_string(),
            text: message.text.clone(),
        },
        MessageRole::User | MessageRole::System => return None,
    };
    Some(AcpUpdate {
        event_id: format!("{prefix}:message:{}", message.message_id),
        kind,
    })
}
