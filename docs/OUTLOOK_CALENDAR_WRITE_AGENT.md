---
summary: Run Joel's Windows Outlook calendar as a narrow writable UCLA bus agent for guard blocks, working holds, and travel blocks.
read_when:
  - installing or debugging the UCLA Outlook calendar write agent
  - wiring calendar guard blocks to the UCLA Pinakes bus
  - changing OUTLOOK_CALENDAR_WRITE_* env vars
---

# Outlook Calendar Write Agent

`ucla-tdg-outlook-calendar-write-agent` runs on Joel's Windows laptop and registers with the UCLA Pinakes bus at `http://beelink:8080`.

It is intentionally narrow. It supports calendar guard-block creation/patching,
sanctioned working holds with Joel, and travel blocks:

- capabilities: `event-insert`, `event-patch`
- bus passport: `agent_class=worker`, `mutation_class=mutate`, `mode=pull`
- default mode: dry-run unless `OUTLOOK_CALENDAR_WRITE_DRY_RUN=false`

Shared event-class markers and summary prefixes live in
`~/Projects/shared/calendar-agents/pkg/calendarcontract`. The write-agent
request/response schema lives in
`~/Projects/shared/calendar-agents/pkg/outlookwritecontract`.

It does not delete events, send email, invite attendees, or create recurring events.
It may create a one-day all-day busy event for the calendar guard's
`Meeting Quota Reached` block, a timed busy working hold whose summary starts
`Joel + `, or a timed busy travel block whose summary starts `Travel: `, the
latter two only when the requesting bus agent is allowlisted.

The writer processes at most one mutation at a time: `busagent.Loop` dispatches
events sequentially from a single goroutine. This serialization is a contract
invariant — quota Allow/Record and the PowerShell conflict-check/save sequences
are only race-free under it. Do not parallelize the event loop, and run exactly
one writer instance per calendar.

## Ownership Marker

Created and patched Outlook event bodies must retain:

```text
managed_by=jk-calendar-guard-agent
owner_agent=ucla-tdg-outlook-calendar-write-agent
```

Patch requests first read the existing Outlook event and refuse to mutate it unless both marker lines are already present.

Working holds append an agent-generated marker block. Request bodies must not
provide any `managed_by=`, `owner_agent=`, or `hold_class=` lines:

```text
managed_by=<authenticated bus sender id>
owner_agent=ucla-tdg-outlook-calendar-write-agent
hold_class=working-hold
```

Hold patches are allowed only from the same authenticated sender recorded in
`managed_by`.

Travel blocks use the same marker scheme with a travel class line:

```text
managed_by=<authenticated bus sender id>
owner_agent=ucla-tdg-outlook-calendar-write-agent
hold_class=travel-block
```

`managed_by` is always the authenticated envelope sender, never request data.
Travel patches are allowed only from the same authenticated sender recorded in
`managed_by`.

**Marker-strip rule for patches:** the reserved-key refusal substring-matches
anywhere in the description, so patching back a description that was READ from
Outlook is always refused — stored bodies contain the marker block. Patch
descriptions must be the agenda text only: strip the 3-line marker block
(starting at the `managed_by=` line) before re-sending a body read from
Outlook; the writer re-appends the marker.

**`travel_for=` convention (recommended, not validated):** travel-block
descriptions should include a `travel_for=<parent Outlook EntryID>` line plus a
human route line so the reconciliation watcher can associate a block with its
parent meeting. The writer treats these as opaque body text. Anything read back
from an event body — including `travel_for=` and `managed_by=` lines — is
untrusted input to downstream consumers; never act destructively on body
content alone.

## Request Contract

Insert:

```json
{
  "action": "event-insert",
  "calendar_id": "default",
  "event": {
    "summary": "Meeting Quota Reached",
    "description": "Inbox recovery day\n\nmanaged_by=jk-calendar-guard-agent\nowner_agent=ucla-tdg-outlook-calendar-write-agent",
    "start": {"date": "2026-05-07", "time_zone": "America/Los_Angeles"},
    "end": {"date": "2026-05-08", "time_zone": "America/Los_Angeles"},
    "show_as": "busy"
  }
}
```

Patch:

```json
{
  "action": "event-patch",
  "calendar_id": "default",
  "event_id": "<Outlook EntryID returned by insert>",
  "event": {
    "summary": "Meeting Quota Reached"
  }
}
```

Working hold insert:

```json
{
  "action": "event-insert",
  "calendar_id": "default",
  "event": {
    "summary": "Joel + disclosure strategy working session",
    "description": "Review next filing path and action plan.",
    "start": {"date_time": "2026-06-15T10:00:00-07:00", "time_zone": "America/Los_Angeles"},
    "end": {"date_time": "2026-06-15T11:00:00-07:00", "time_zone": "America/Los_Angeles"},
    "show_as": "busy"
  }
}
```

Travel block insert (`meta.request_id` MUST be unique per mutation attempt,
across all event classes and actions — see Safety Rules):

```json
{
  "action": "event-insert",
  "calendar_id": "default",
  "event": {
    "summary": "Travel: Alto Cedro → 200 Medical Plaza",
    "description": "travel_for=00000000ABCD...\nDestination: 200 Medical Plaza, Los Angeles, CA 90024\nroute: drive ~25 min + 10 min parking (200 Medical Plaza structure)",
    "location": "200 Medical Plaza, Los Angeles, CA 90024",
    "start": {"date_time": "2026-06-15T08:20:00-07:00", "time_zone": "America/Los_Angeles"},
    "end":   {"date_time": "2026-06-15T08:55:00-07:00", "time_zone": "America/Los_Angeles"},
    "show_as": "busy"
  }
}
```

Travel block cancel (only summary + show_as; everything else refused):

```json
{
  "action": "event-patch",
  "calendar_id": "default",
  "event_id": "<Outlook EntryID>",
  "event": {
    "summary": "[CANCELLED] Travel: Alto Cedro → 200 Medical Plaza",
    "show_as": "free"
  }
}
```

Dry-run responses return:

```json
{
  "dry_run": true,
  "would_write": {
    "summary": "Meeting Quota Reached",
    "description": "...",
    "start": {"date": "2026-05-07", "time_zone": "America/Los_Angeles"},
    "end": {"date": "2026-05-08", "time_zone": "America/Los_Angeles"},
    "show_as": "busy"
  }
}
```

## Safety Rules

- `calendar_id` must be `default` or `outlook-primary`.
- Guard `event.summary` must start with `No more meetings` or equal `Meeting Quota Reached`.
- Working-hold `event.summary` must start with `Joel + `.
- `event.show_as` must be `busy`.
- Guard timed events must use RFC3339 `event.start.date_time` and `event.end.date_time`, fall on the same local date, and last no more than 4 hours.
- Guard all-day events must use `event.start.date` and `event.end.date` only and must span exactly one local day.
- Working holds are timed only, last 15 minutes to 2 hours, start in the next 30 days, start between 07:00 and 21:00 local time, and must not overlap existing non-free Outlook events.
- Working-hold `time_zone` must be absent or `America/Los_Angeles`; timestamp offsets must match Los Angeles at that instant.
- Working-hold live inserts are capped at 2 per requester per event local date and 5 globally per event local date. Duplicate request ids are idempotent for 24 hours.
- Travel-block `event.summary` must start with `Travel: ` (recommended form `Travel: <origin> → <destination>`).
- Travel-block `event.location` is optional and, when present, is copied to
  Outlook's visible Location field. Scheduler-created travel blocks should use
  the visible endpoint for that leg: verified venue name + address before the
  meeting, and verified return/next target label + address after the meeting.
  The scheduler may patch stale attached travel blocks to repair this visible
  endpoint or old summary grammar without changing the live time bounds.
- Travel blocks are timed only, last 10 minutes to 2 hours, start in the next 30 days, start between 05:00 and 23:00 local time, and must not overlap existing non-free Outlook events (guard quota blocks exempt; exact-summary `Meeting Buffer` placeholders exempt so the scheduler can replace vague buffer time with a visible travel card; abutting the parent meeting is permitted by construction).
- Travel-block descriptions must not exceed 4096 bytes (before the marker is appended) and must have a non-empty agenda after marker stripping.
- Travel-block live inserts are capped at 8 per requester per event local date and 20 globally per event local date — a separate quota from working holds; neither class consumes the other's quota.
- Travel writes are gated by the same `OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS` allowlist and the same host-timezone probe as working holds. Invalid travel input returns `error_code: "invalid_travel"`.
- Travel idempotency cache keys are class/action-qualified (`<sender>/event-insert/travel-block/<request_id>`), so a request id reused across classes never replays the other class's cached response. **Requester contract:** `meta.request_id` MUST be unique per mutation attempt, across all event classes and actions; reuse within the same class+action returns the original cached response without re-validating that the new request resembles it. Multi-leg bookings must give each travel leg a distinct id (e.g. `-travel-1`, `-travel-2`).
- Live travel patches (including cancels) are stale-snapshot-checked: if the event's live start/end moved between the read and the write, the patch fails with `error_code: "conflict"` (`conflict: stale snapshot ...`). Treat that as "re-read and re-plan from fresh state", never retry-as-is. Hold patches keep today's unchecked behavior. Travel cancel patches may set only `summary` and `show_as`; `location` is refused on cancel.
- **Replay visibility (SCHEDULER_TRAVEL_SPEC §3.7):** every response served from the 24-hour idempotency cache (hold and travel classes alike) carries `"replayed": true`; fresh responses omit the field. The cached event reflects the event AS INSERTED, not its current state — a replayed success says nothing about whether the event has since been cancelled. Callers composing multi-step writes MUST verify replayed prerequisites (the scheduler's offsite-booking replay guard does this via events-list).
- **Budget release on cancel (SCHEDULER_TRAVEL_SPEC §3.6):** a LIVE travel patch that transitions a block into the cancelled state returns one unit of the cancelling sender's travel insert budget for the block's event local date (metric `event_travel_budget_released`). Double-cancels release nothing. The HOLD quota deliberately has no release — hold-quota burn under failed-booking compensation is accepted v1 behavior (travel spec §3.6).
- `OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS` is a comma-separated allowlist. Unset or empty refuses hold and travel writes and leaves guard writes unaffected.
- `attendees`, `conferenceData`, and recurrence fields are rejected.
- Private calendar details and request bodies are not written to logs.

## Build

The `timetzdata` tag is REQUIRED: working-hold validation calls
`time.LoadLocation("America/Los_Angeles")`, which fails on Windows without the
embedded zone database (live failure observed 2026-06-11: every hold insert
returned `invalid_hold: unknown time zone America/Los_Angeles`).

```powershell
$env:GOOS = "windows"
$env:GOARCH = "amd64"
$env:CGO_ENABLED = "0"
go build -tags timetzdata -o .tmp\outlook-calendar-write-agent.exe .\cmd\outlook-calendar-write-agent
```

Or cross-compile from beelink:

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -tags timetzdata -o /tmp/outlook-calendar-write-agent.exe ./cmd/outlook-calendar-write-agent
```

## Install On Windows Laptop

Add the agent id to the UCLA bus allowlist before installing:

```text
ucla-tdg-outlook-calendar-write-agent
```

Install as a scheduled task in dry-run mode:

```powershell
.\scripts\install-outlook-calendar-write-agent.ps1 `
  -BinaryPath .\.tmp\outlook-calendar-write-agent.exe `
  -AgentSecret "<openssl rand -base64 32 value>" `
  -BusUrl "http://beelink:8080" `
  -AgentId "ucla-tdg-outlook-calendar-write-agent" `
  -TaskName "ucla-tdg-outlook-calendar-write-agent" `
  -HoldRequesters "jk-fable-operator,ucla-tdg-scheduler-agent"
```

Current UCLA scheduled-task value:

```text
OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS=jk-fable-operator,ucla-tdg-scheduler-agent
```

To explicitly allow live Outlook mutation after human approval:

```powershell
.\scripts\install-outlook-calendar-write-agent.ps1 `
  -BinaryPath .\.tmp\outlook-calendar-write-agent.exe `
  -AgentSecret "<openssl rand -base64 32 value>" `
  -BusUrl "http://beelink:8080" `
  -AgentId "ucla-tdg-outlook-calendar-write-agent" `
  -TaskName "ucla-tdg-outlook-calendar-write-agent" `
  -DryRun "false" `
  -HoldRequesters "jk-fable-operator,ucla-tdg-scheduler-agent"
```

## Verify

```powershell
Invoke-RestMethod http://localhost:8219/health
Invoke-RestMethod http://beelink:8080/v1/agents |
  Select-Object -ExpandProperty agents |
  Where-Object agent_id -eq "ucla-tdg-outlook-calendar-write-agent"
```

Do only dry-run insert smoke tests unless Joel explicitly approves a live Outlook mutation.
