---
name: handoff
description: Write a session handoff doc for the next agent
---
Write `docs/handoffs/<feature>-<date>.md` containing:
1. Active spec path + which wave/task IDs are DONE, IN PROGRESS, BLOCKED
2. Exact files changed this session
3. Test status: paste the last `go test ./... -race` and TS test output
4. Known-wrong theories already ruled out (so the next agent doesn't repeat them)
5. Next concrete action with the command to run
Do not start new implementation work while writing this.
