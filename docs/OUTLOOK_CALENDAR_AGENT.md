---
summary: Run Joel's Windows Outlook calendar as a read-only JK bus agent.
read_when:
  - wiring calendar agenda questions to the JK bus
  - installing or debugging the Windows Outlook calendar agent
  - changing OPERATOR_GCAL_AGENT_ID or calendar bus routing
---

# Outlook Calendar Agent

`jk-outlook-calendar-agent` runs on Joel's Windows laptop, reads the primary Outlook calendar through local Outlook COM automation, and registers with the JK Pinakes bus on `beelink:8081`.

It is read-only. It answers the same `events-list` request shape that `jk-gcal-ingest` already uses, so `jk-email-operator` can use Outlook calendar context by setting:

```bash
OPERATOR_GCAL_AGENT_ID=jk-outlook-calendar-agent
```

## Status 2026-06-20: Restored

`jk-outlook-calendar-agent` is live again on the JK bus and answers read-only
`events-list` requests from the Windows laptop. It runs beside the UCLA Outlook
reader and the UCLA Outlook writer with distinct laptop health ports:

- `:8218` - `ucla-tdg-outlook-calendar-agent`
- `:8219` - `ucla-tdg-outlook-calendar-write-agent`
- `:8220` - `jk-outlook-calendar-agent`

The previous outage root cause was a port conflict: both laptop reader profiles
could default to `:8218`, so the losing task died after a fatal health-server
bind failure. The default JK reader health port is now `:8220`; UCLA reader
installs must pass `-HttpAddr ":8218"` explicitly.

Do not repoint `OPERATOR_GCAL_AGENT_ID` at `jk-gcal-ingest` as a workaround
unless the Outlook adapter is being permanently retired: the request shape is
identical but `jk-gcal-ingest` serves the personal Google calendar (different
data) and is write-capable.

### Restore Or Upgrade Runbook

Preferred path from beelink: `ssh -p 2222 joelkehle@laptop` and invoke Windows
PowerShell through `/mnt/c/Windows/System32/WindowsPowerShell/v1.0/powershell.exe`.
On the laptop directly, use PowerShell as Joel.

1. Check which tasks exist: `Get-ScheduledTask | Where-Object {$_.TaskName -match 'Outlook Calendar|outlook-calendar'}`. Expect the legacy `JK Outlook Calendar Agent` task and `ucla-tdg-outlook-calendar-agent`.
2. For a reinstall, run the installer for the JK profile with the default `:8220` health port:

   ```powershell
   .\install-outlook-calendar-agent.ps1 `
     -BinaryPath .\outlook-calendar-agent.exe `
     -AgentSecret "<existing jk-outlook-calendar-agent secret>" `
     -BusUrl "http://beelink:8081"
   ```

   This registers a task named `jk-outlook-calendar-agent` unless `-TaskName` is overridden. The current laptop still has the legacy `JK Outlook Calendar Agent` task name but launches the restored `jk-outlook-calendar-agent` profile on `:8220`.
3. If both the legacy task and a new `jk-outlook-calendar-agent` task exist, remove the duplicate so only one JK-profile task remains: `Unregister-ScheduledTask -TaskName "JK Outlook Calendar Agent" -Confirm:$false`.
4. Verify from beelink: `curl -fsS http://localhost:8081/v1/agents | jq -r '.agents[] | select(.agent_id=="jk-outlook-calendar-agent")'`.

## Bus Contract

Agent id:

- `jk-outlook-calendar-agent`

Capabilities:

- `calendar-list`
- `events-list`
- `calendar-agenda`
- `outlook-calendar`

Request:

```json
{
  "action": "events-list",
  "query": {
    "time_min": "2026-05-05T00:00:00-07:00",
    "time_max": "2026-05-06T00:00:00-07:00",
    "single_events": true,
    "order_by": "startTime",
    "max_results": 50
  }
}
```

Response uses the shared calendar read JSON shape
(`~/Projects/shared/calendar-agents/pkg/calendarreadcontract`):

```json
{
  "events": [
    {
      "id": "outlook_...",
      "status": "confirmed",
      "summary": "Design review",
      "location": "Zoom",
      "categories": ["UCLA", "Meeting"],
      "start": {"dateTime": "2026-05-05T09:00:00-07:00", "timeZone": "America/Los_Angeles"},
      "end": {"dateTime": "2026-05-05T09:30:00-07:00", "timeZone": "America/Los_Angeles"},
      "transparency": "opaque",
      "visibility": "default"
    }
  ]
}
```

Outlook comma-separated `Categories` values are returned as the event's `categories` string array when present.

Each event also carries `extendedProperties.private` metadata. Since the
location-awareness phase 1.5 change (SCHEDULER_TRAVEL_SPEC §0) this includes
`entry_id`: the RAW Outlook MAPI EntryID, present whenever Outlook supplies
one. Event `id` stays the synthetic `outlook_<hash>` and `source_entry` stays
hashed; `entry_id` exists because the write agent's patch path resolves
targets via `GetItemFromID`, which needs the raw EntryID — it is the ONLY
usable mutation handle for events the write agent did not itself insert (the
scheduler's reconciliation watcher depends on it). EntryID is an opaque
identifier, not content; per Joel's 2026-06-11 ruling it is exposed for
masked private events too.

Private Outlook appointments are visible by default because this agent runs on
Joel-owned machines with Joel's Outlook permissions. Set
`OUTLOOK_CALENDAR_INCLUDE_PRIVATE_DETAILS=false` only for a deliberately
redacted read surface; then subject becomes `Private appointment` and location
is blank.

**Scheduler-watcher deployment dependency (normative):** the travel
reconciliation watcher (SCHEDULER_TRAVEL_SPEC §7) assumes the UCLA instance
of this agent runs with `OUTLOOK_CALENDAR_INCLUDE_PRIVATE_DETAILS=true`
(Joel-equivalent calendar visibility). With explicit redaction, private
offsite meetings are only detectable via the yellow category and private
travel blocks degrade to untouchable busy events. Live-smoke item for the next
deploy session: prove a watcher-discovered travel block can be cancelled via
its `extendedProperties.private.entry_id`, and confirm the actual display name
of Joel's yellow Outlook category.

## Install On Windows Laptop

Build the Windows binary from this repo:

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o .tmp/outlook-calendar-agent.exe ./cmd/outlook-calendar-agent
```

Copy the binary and installer to the Windows laptop, then run PowerShell as Joel:

```powershell
$secret = "<openssl rand -base64 32 value>"
.\install-outlook-calendar-agent.ps1 `
  -BinaryPath .\outlook-calendar-agent.exe `
  -AgentSecret $secret `
  -BusUrl "http://beelink:8081"
```

Every laptop profile of this binary must use a distinct `-HttpAddr`; a bind conflict kills the losing agent at logon. Current laptop port map: `:8218` UCLA reader, `:8219` UCLA write agent, `:8220` JK reader.

The installer:

- copies the binary under `%LOCALAPPDATA%\calendar-agents\outlook-calendar-agent` (or `-InstallDir`)
- stores the bus HMAC secret as a DPAPI-encrypted file for the current Windows user
- creates a scheduled task named after the agent id (override with `-TaskName`)
- starts the task immediately

## Required Bus Setup

Add this id to the shared allowlist:

```text
jk-outlook-calendar-agent
```

Source of truth:

```text
~/Projects/shared/manager/ops/config/allowlist.txt
```

The JK bus hot-reloads that file.

## Verify

From beelink:

```bash
curl -fsS http://localhost:8081/v1/agents | jq -r '.agents[] | select(.agent_id=="jk-outlook-calendar-agent")'
```

Ask the operator:

```bash
curl -sS http://localhost:8205/query \
  -H 'content-type: application/json' \
  -d '{"question":"what is my agenda today?"}' | jq
```

If the laptop is asleep or Outlook COM cannot start, the agent will be absent or return a calendar extraction error. The bus stays healthy; callers should degrade to "Outlook calendar agent unavailable."
