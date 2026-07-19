---
summary: Spec — intent-level scheduler agent that books, moves, and cancels agent working holds on Joel's calendar asynchronously.
read_when:
  - implementing or reviewing ucla-tdg-scheduler-agent
  - deciding how an agent should request time with Joel
  - changing Joel's slot-picking policy
---

# Spec: Shared Scheduler Agent (`ucla-tdg-scheduler-agent`)

Status: v2 — Codex-reviewed 2026-06-11 (11 findings incorporated; review at
`/tmp/codex-sched-review.log`, summarized in §Review log). Ready to implement.
Sanction: Joel, 2026-06-11 — full build-out approved without further input.
Authority model approved by Joel: **the scheduler may place and move agent
holds freely; it may never touch human meetings; anything involving another
person is refused with an escalate-to-Joel reply.**

Runtime auth hardening: downstream write authority stays with
`ucla-tdg-scheduler-agent`, but only upstream bus agents explicitly named in
`SCHEDULER_ALLOWED_REQUESTERS` may invoke `schedule-request`,
`schedule-move`, or `schedule-cancel`. `travel-estimate` stays read-only.

## Problem

Booking time with Joel currently requires each caller to run the mechanics
itself (events-list → pick slot → event-insert → await reply), which puts
Joel's slot-picking policy in every caller. Callers should send *intent*
("30 min with Joel this evening about X") and receive an asynchronous
confirmation ("booked 19:00") in the same bus conversation.

## Identity and runtime

- Agent id: `ucla-tdg-scheduler-agent` (historical ID; shared Joel-calendar
  scheduler, currently on UCLA bus `http://localhost:8080`, pull mode;
  passport `agent_class=orchestrator`, `mutation_class=mutate`).
- Runs on beelink as systemd user unit `ucla-tdg-scheduler-agent.service`
  (already staged in `~/.config/systemd/user/`), binary `cmd/scheduler-agent`,
  package `internal/scheduler`.
- HTTP on `:8245` (registered in manager port-allocations): `GET /health`,
  `GET /metrics` (reuse `internal/telemetry` registry like the other agents).
  Metrics include separate Outlook read/write dependency request/error counters,
  availability gauges, a writer-refusal counter, and read/write
  `schedule_calendar_*_work_blocked` counters plus gauges. A blocked-work gauge
  is set only when a scheduler request or required watcher write cannot use its
  dependency; ordinary background read failures do not page. The matching
  dependency's next successful response clears the gauge. `/health` remains
  process health.
- Env (via `~/.config/ucla-tdg-scheduler-agent.env`, already staged):
  `SCHEDULER_AGENT_SECRET`, `SCHEDULER_BUS_URL` (default
  `http://localhost:8080`), `SCHEDULER_HTTP_ADDR` (default `:8245`),
  `SCHEDULER_ALLOWED_REQUESTERS` (comma-separated caller allowlist; empty
  refuses scheduler writes),
  `SCHEDULER_CALENDAR_READ_AGENT` (default `ucla-tdg-outlook-calendar-agent`),
  `SCHEDULER_CALENDAR_WRITE_AGENT` (default
  `ucla-tdg-outlook-calendar-write-agent`), `SCHEDULER_WATCH_INTERVAL_MIN`
  (default `15`), and `SCHEDULER_WATCH_HORIZON_DAYS` (default `3`, widened in
  live deploys when Joel needs earlier travel-block visibility).
- Follow `internal/busagent` + `internal/outlookcalendarwrite/agent.go`
  patterns for registration, pull loop, acks, replies, telemetry.
- Shared scheduler bus schema lives in
  `~/Projects/shared/calendar-agents/pkg/schedulercontract`; local
  `internal/scheduler/contracts.go` should stay a thin runtime wrapper.
- Deployment step: add `ucla-tdg-scheduler-agent` to
  `OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS` on the laptop write agent.

## Companion write-agent extension (review §3, §4 — blockers)

Two narrow changes to `internal/outlookcalendarwrite`, fully covered by tests,
guard behavior otherwise untouched:

1. **Hold requesters lose access to the guard patch path.** In `handlePatch`,
   if the authenticated sender is in `OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS`,
   the existing event MUST carry the hold marker block; otherwise reply
   `error_code=not_owned` ("hold requesters may only patch their own working
   holds"). This makes move/cancel safe WITHOUT any new read surface: the
   ownership pre-check happens atomically inside the writer, which already
   reads the event. Non-hold-requester senders (the calendar guard) keep
   today's behavior exactly.
2. **Patch/cancel become idempotent.** Extend the existing
   `holdResponseCache` to cache hold patch and cancel replies by the same
   request key as inserts (sender + `meta.request_id`, 24 h). Additionally a
   cancel of an already-cancelled hold returns `status` success with the
   existing event (not an error) — cancel is naturally idempotent.

The scheduler therefore performs NO pre-check reads for move/cancel; it
forwards to the writer and maps `not_owned` through to its caller.

## Bus contract — `scheduler.v1`

Capabilities: `schedule-request`, `schedule-move`, `schedule-cancel`.

### Async flow and correlation (review §2 — blocker)

- On any scheduler.v1 request: validate shape synchronously; ack `accepted`;
  enqueue a job; return from `HandleEvent` immediately (the busagent loop
  stays sequential and non-blocking; jobs run on a worker goroutine, max 4
  concurrent, FIFO).
- Each job talks to upstream agents in a **derived conversation** whose id is
  deterministic: `sched-<idempotency key hash>`. Replies from upstreams are
  routed by the main loop to the waiting job via a channel map keyed by
  conversation id (`router`). Events in `sched-*` conversations are never
  treated as new scheduler.v1 requests.
- Upstream call timeout: 60 s, one retry, then the job replies
  `error_code=upstream_unavailable`.
- The final reply is sent in the CALLER's conversation, exactly once per
  idempotency key.

### Idempotency (review §1 — blocker)

- Canonical key: `<authenticated sender id>:<body request_id>`. `request_id`
  is required in every request body; callers reuse it on retry.
- In-memory cache key → terminal reply, 24 h TTL: duplicate requests get the
  cached reply re-sent (in the duplicate's conversation).
- All upstream write-agent requests carry `meta.request_id =
  "sched-" + <key hash> + "-" + <step>` (insert / move / cancel are distinct
  steps), so writer-side idempotency (§companion change 2) makes every
  mutating step restart-safe even when the scheduler's own cache is lost.

### `schedule-request`

```json
{
  "action": "schedule-request",
  "request_id": "<caller-unique, reused on retry>",
  "purpose": "Apple Health pipeline working session",
  "requester_label": "Fable",
  "duration_minutes": 60,
  "window": "this evening",
  "agenda": "1. Parse export baseline\n2. ...",
  "earliest": "",
  "latest": ""
}
```

- `purpose` (required): summary becomes `"Joel + <requester_label>: <purpose>"`;
  `requester_label` defaults to the authenticated sender id.
- `duration_minutes` (required): 15–120, multiple of 15.
- `agenda` (required, non-empty).
- Window resolution (review §7) — all in America/Los_Angeles, all intervals
  **half-open `[start, end)`**:
  - Grammar (exact, case-insensitive; anything else ⇒ `invalid_window`
    listing the grammar): `today` | `tomorrow` | `this morning` |
    `this afternoon` | `this evening` | `tomorrow morning` |
    `tomorrow afternoon` | `tomorrow evening` | `next N days` (1 ≤ N ≤ 7) |
    `YYYY-MM-DD`.
  - Segments: morning [07:00, 12:00), afternoon [12:00, 17:00), evening
    [17:00, 21:00), whole day [07:00, 21:00).
  - `today` / `this <segment>` clip to `now`: a segment fully in the past ⇒
    `invalid_window` with message "window already passed".
  - `next N days` = the union of whole-day windows for today .. today+N−1.
  - `YYYY-MM-DD` = that date's whole-day window.
  - `earliest`/`latest` (RFC3339 with offset): parsed, converted to LA;
    intersected with `window` if both present, or used alone if `window`
    empty. Empty intersection ⇒ `invalid_window`.
  - All outbound timestamps are RFC3339 with the LA offset valid at that
    instant (DST-safe: compute via `time.In(losAngeles)`).

Replies (one schema for every outcome — review §10):

```json
{"status":"booked|moved|cancelled|infeasible|refused|error",
 "request_id":"...",
 "event_id":"...", "start":"...", "end":"...", "summary":"...",
 "error_code":"...", "message":"...",
 "nearest_alternative":{"start":"...","end":"..."}}
```

- `booked`: event_id/start/end/summary set.
- `infeasible`: `nearest_alternative` set when a slot exists within 7 days
  past the window end (suggestion only; no negotiation state).
- `refused`: `error_code=involves_other_people`, message "escalate to Joel".
- `error`: `error_code` ∈ `invalid_window`, `invalid_request`,
  `upstream_unavailable`, `not_owned`, `booking_refused`. When the writer
  refuses, its own `error_code` is preserved as
  `message: "writer: <code>: <message>"` with `error_code=booking_refused`.

### `schedule-move`

```json
{"action":"schedule-move","request_id":"...","event_id":"<outlook id>",
 "window":"tomorrow evening","duration_minutes":60}
```

Slot selection in the new window (excluding the event being moved from the
busy set is not possible via the read agent — instead the writer's conflict
check excludes the patched event already; the scheduler's read-side selection
treats the old slot as busy, which is acceptable: worst case it picks a
different slot). Patches times via `event-patch`. `duration_minutes` optional;
defaults to 60 if the request omits it (the scheduler cannot read the current
duration — document this). `not_owned` maps through (§companion change 1).

### `schedule-cancel`

```json
{"action":"schedule-cancel","request_id":"...","event_id":"<outlook id>"}
```

Cancel patch (`[CANCELLED] ` prefix + `show_as: free`) via `event-patch`.
Idempotent end-to-end (§companion change 2): repeated cancels reply
`cancelled`.

## Slot-selection policy (the single home of "what is a good slot")

Fetch events via `events-list` for each local date the window touches, build
the busy set, then choose the EARLIEST candidate satisfying ALL of
(constants in `internal/scheduler/policy.go`, table-driven tests):

1. Candidate starts on a 15-minute boundary within the window; full duration
   fits within the window (half-open).
2. Start ≥ now + 30 minutes.
3. **Busy set** (review §8): events with `transparency == "transparent"` are
   ignored; all-day events whose summary equals `"Meeting Quota Reached"` or
   starts with `"No more meetings"` are ignored (guard blocks — Joel's
   ruling; summary-level heuristic is read-side only, see §retry). Everything
   else is busy as `[start, end)`.
4. No overlap with any busy interval.
5. No overlap with [12:00, 13:00) local (lunch), even if the calendar shows
   it free. Lunch creates no buffer.
6. 10-minute buffer AFTER timed busy events only: a candidate may not start
   before `busyEnd + 10m` for any timed (non-all-day) busy event whose end
   falls within `(candidateStart - 10m, candidateStart]`. All-day busy events
   and lunch produce no buffer (they conflict outright via rules 4–5). No
   buffer before events.

### Booking and the read/write race (review §5, §9)

After selecting a slot, send `event-insert`. If the writer replies
`error_code=conflict` (a meeting landed after our read, or a guard-summary
spoof made an exempt-looking event), add the writer-reported busy ranges to
the busy set, re-select, and retry — at most 3 attempts total, then reply
`infeasible` with the standard shape.

## Authority rules (Joel-approved, enforced in this order)

1. Requests carrying any structural attendee/invite field anywhere in the
   event payload are refused `involves_other_people` ("escalate to Joel").
   v1 does NOT attempt natural-language detection of other people in
   `purpose`/`agenda` (false-positive trap); the write agent independently
   rejects attendee fields.
2. Move/cancel of anything that is not the scheduler's own hold is refused by
   the writer (`not_owned`, §companion change 1) and mapped through.
3. Writer caps apply unchanged and are NOT refunded by cancels (review §6):
   2 successful live inserts per requester per event local date, 5 globally.
   The scheduler is ONE requester — the fleet shares 2 scheduler-booked holds
   per day. Do not raise without Joel.

## State

No new authoritative store (STATE_ARCHITECTURE.md v1.4 reviewed): Outlook
owns events; the bus carries conversations; the scheduler holds only
in-memory working state (idempotency cache, router map, job queue). Restart
safety comes from writer-side idempotency on every mutating step.

## Testing

- Unit: window grammar (every production, clipping, past-window, DST
  boundary dates 2026-03-08/2026-11-01, empty intersection); slot policy
  (lunch, buffers, guard exemption, all-day busy, 15-min boundaries,
  future-only, infeasible + nearest-alternative); authority refusals;
  idempotent replay; conflict-retry loop (busy-range injection).
- Agent-loop tests mirroring `internal/outlookcalendarwrite/agent_test.go`
  with fake upstreams: happy path, upstream timeout → `upstream_unavailable`,
  writer conflict → retry → booked, writer refusal passthrough, move, cancel,
  cancel-twice, `not_owned`, duplicate request replays cached reply.
- Write-agent extension tests: hold requester patching a guard block ⇒
  `not_owned`; guard agent patch path unchanged (existing tests untouched);
  patch/cancel idempotency cache; double-cancel succeeds.
- Live smoke (Fable, post-deploy, scripted): schedule-request "tomorrow
  evening" from `jk-fable-operator` → booked reply + event visible via
  daily-briefing; schedule-move within the window → moved; schedule-cancel →
  cancelled; repeat cancel → cancelled. Leaves one `[CANCELLED]` free-status
  artifact event — acceptable.

## Out of scope (v1)

LLM window parsing; multi-round negotiation; booking with other humans; JK
personal profile; proactive rescheduling when conflicts appear later (v2:
hold-lifecycle watcher — sketch in agent doc only); notifying Joel beyond the
calendar event itself.

Scheduler v2 (travel awareness) is specified separately in
`docs/SCHEDULER_TRAVEL_SPEC.md`: `travel-estimate` capability, `location` on
`schedule-request` with atomic travel blocks, and the reconciliation watcher.

## Review log

Codex peer review 2026-06-11: 4 blockers (idempotency key underspecified;
async correlation missing from a sequential loop; move/cancel pre-check
impossible without a read surface — solved by writer-side not_owned gate;
cancel not restart-idempotent — solved by writer patch/cancel cache), 6
should-fix (conflict retry, caps wording, window grammar pinning, buffer
semantics, guard-summary spoof, unified reply schema), 1 nit (/metrics).
All incorporated above.

## Known deviations (v1, accepted)

- Reply `start`/`end` echo the writer's Outlook timestamp format
  (`2026-06-12T17:00:00.0000000`, no offset) instead of RFC3339+LA offset.
  Times are LA-local. Live-smoke verified 2026-06-11; fix is a reformat in the
  scheduler's reply mapping — queued on the cross-project backlog.
- On `schedule-move`, the hold's current slot is treated as busy during
  re-selection (the read agent cannot exclude it), so a move within the same
  window relocates to the next free slot rather than keeping its place.
- Travel-blocks/holds asymmetry (SCHEDULER_TRAVEL_SPEC §7.6): moving an
  offsite-booked hold with `schedule-move` orphans its travel blocks — the
  watcher cancels them once their anchor is gone but does NOT recreate blocks
  at the hold's new slot (holds carry no location/category, so they are not
  offsite-detectable). Fixing this requires a location field on write-agent
  events (travel spec §12.6, v2 item).
