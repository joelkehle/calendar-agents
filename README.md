# calendar-agents

Shared calendar contracts for Joel's personal and professional agent workflows.

This repo is the new home for calendar code that is neither personal-only nor
UCLA-only. The first extracted package is `pkg/calendaridentity`, which keeps
the historical live bus IDs stable while making ownership explicit.

Current migration status:

- Live Outlook read/write and scheduler runtimes still run from
  `jk/jk-email-agents`.
- Shared calendar identity constants live here.
- Future shared scheduler, Outlook adapter, and event-class contracts should be
  added here first, then consumed by `jk` and `ucla-tdg` repos.

Validation:

```bash
go test ./...
```
