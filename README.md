# calendar-agents

Shared calendar runtime and contracts for Joel's personal and professional
agent workflows.

This repo is the new home for calendar code that is neither personal-only nor
UCLA-only.

Runtime commands:

- `cmd/outlook-calendar-agent`: read-only Outlook COM adapter. The same binary
  is installed as the JK-bus reader (`jk-outlook-calendar-agent`, `:8220`) and
  UCLA-bus reader (`ucla-tdg-outlook-calendar-agent`, `:8218`).
- `cmd/outlook-calendar-write-agent`: narrow Outlook writer for guard blocks,
  working holds, and travel blocks (`ucla-tdg-outlook-calendar-write-agent`,
  `:8219`).
- `cmd/scheduler-agent`: shared scheduling and travel-time orchestrator
  (`ucla-tdg-scheduler-agent`, `:8245`).

Shared packages:

- `pkg/calendaridentity`: stable live bus IDs with explicit shared ownership.
- `pkg/calendarcontract`: shared Outlook event-class markers, summaries, and
  timezone constants.
- `pkg/calendarreadcontract`: shared calendar event/query/response schema used
  by Outlook calendar read agents.
- `pkg/outlookwritecontract`: shared Outlook write-agent request/response
  schema.
- `pkg/schedulercontract`: shared scheduler bus request/reply schema,
  capability/status/error constants, and schema helpers.

Ownership status:

- Outlook read/write and scheduler runtime source lives here.
- Historical live bus IDs remain stable during the move.
- `jk/jk-email-agents` keeps personal email, inbox, operator, and calendar guard
  policy code. It should consume these shared calendar services instead of
  owning their runtime.

Validation:

```bash
go test ./...
```

Live smoke:

```bash
CALENDAR_SMOKE_AGENT_SECRET="<jk-calendar-guard-agent bus secret>" \
  go run ./cmd/calendar-live-smoke
```

The smoke checks both Outlook read profiles, the scheduler `travel-estimate`
capability, and a non-destructive write-agent refusal. The writer check uses a
working-hold shaped request from an unallowlisted sender and expects
`not_allowlisted`, so it does not create a calendar event.

Scheduler write actions also fail closed unless
`SCHEDULER_ALLOWED_REQUESTERS` explicitly names the caller. This gate applies
to `schedule-request`, `schedule-move`, and `schedule-cancel`; the read-only
`travel-estimate` action remains available without it.
