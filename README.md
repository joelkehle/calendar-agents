# calendar-agents

Shared calendar contracts for Joel's personal and professional agent workflows.

This repo is the new home for calendar code that is neither personal-only nor
UCLA-only.

Extracted packages:

- `pkg/calendaridentity`: stable live bus IDs with explicit shared ownership.
- `pkg/calendarcontract`: shared Outlook event-class markers, summaries, and
  timezone constants.
- `pkg/calendarreadcontract`: shared calendar event/query/response schema used
  by Outlook and Google calendar agents.
- `pkg/outlookwritecontract`: shared Outlook write-agent request/response
  schema.
- `pkg/schedulercontract`: shared scheduler bus request/reply schema,
  capability/status/error constants, and schema helpers.

Current migration status:

- Live Outlook read/write and scheduler runtimes still run from
  `jk/jk-email-agents`.
- Shared calendar identity, read/write wire schemas, event-class, and scheduler
  bus contracts live here.
- Future shared scheduler and Outlook adapter implementation code should be
  added here first, then consumed by `jk` and `ucla-tdg` repos.

Validation:

```bash
go test ./...
```
