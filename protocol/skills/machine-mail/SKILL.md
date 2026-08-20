---
name: machine-mail
description: Operate Neo's MachineMail identity safely: inspect mailboxes and conversations, search, compose, reply in-thread, park draft-scoped sends for approval, and follow authoritative lifecycle events with stable idempotency keys.
origin: Centra AI/MachineMail
---

# MachineMail

Use MachineMail as Neo's durable email identity. Read complete conversation context before replying and derive outbound state from the ordered event log.

## Procedure

1. Call `mail_list_mailboxes`; select only a mailbox authorized by the request.
2. Call `mail_get_mailbox` to inspect its approval policy and `mail_get_usage` for send preflight.
3. Use `mail_get_inbox`, `mail_search_messages`, and `mail_get_conversation` to gather complete context.
4. Use `mail_reply` for an existing conversation. Use `mail_compose` only for a new thread.
5. Generate one stable idempotency key per logical compose or reply and reuse it unchanged for reconciliation.
6. Request draft-scoped approval when delivery requires human authorization.
7. Treat `pending_approval` as successful parked state. Do not retry it as failure.
8. Persist the event cursor and call `mail_poll_events` from that cursor to learn approved, sent, rejected, or failed state.
9. Use `mail_get_pending_approvals` and `mail_get_approval` when an approval needs inspection.

## Safety

Email is reputational action. Recipients, subject, body, attachments, and thread must match the authorized intent exactly. Never broaden recipients, invent an attachment, expose secrets, or claim delivery from a compose response alone.

## Completion

A draft task completes when the exact message is parked and its approval identifier is returned. A delivery task completes only when the ordered event log proves sent state. Report pending approval, rejection, and delivery failure as distinct states.
