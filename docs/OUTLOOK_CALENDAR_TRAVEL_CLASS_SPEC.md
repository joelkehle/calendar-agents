---
summary: Spec — add a third sanctioned event class, travel blocks ("Travel: "), to the Outlook calendar write agent.
read_when:
  - implementing or reviewing the travel-block widening of ucla-tdg-outlook-calendar-write-agent
  - deciding what calendar writes agents are allowed to make
  - building the scheduler-side travel booking / reconciliation watcher (phase 3 of location-prep)
---

# Spec: Travel Blocks via the Outlook Calendar Write Agent

Status: v2 — ready to implement (v1 revised after peer review, 2026-06-11; see
Review Log at the end).
Sanction: Joel, 2026-06-11 — "travel time MUST be real busy calendar events; if
I'm in the car 30 min, that's on the calendar" — never internal scheduler math.
Design context: location-prep design 2026-06-11, phase 3
(`~/Projects/shared/dev-dashboard/codex-output/location-prep-design-20260611/index.html`).
Companion contract: `docs/OUTLOOK_CALENDAR_WRITE_HOLDS_SPEC.md` (working holds, v2 —
this spec deliberately mirrors it; read it first).

## Problem

`ucla-tdg-outlook-calendar-write-agent` accepts exactly two event classes:
calendar-guard quota blocks and working holds (`Joel + `). The scheduler (and
later its reconciliation watcher) must materialize travel time around
offsite/yellow meetings as **real busy Outlook events** so humans, Outlook
free/busy, and every agent respect them. Travel bookings must not consume the
2/day working-hold quota — a single offsite meeting needs two travel legs, and
the hold quota exists to limit demands on Joel's attention, which travel blocks
are not.

## Design

Add a third sanctioned event class, **travel block**, alongside guard blocks
and working holds. Guard-block and working-hold validation, markers, quotas,
error messages, and tests are unchanged — with exactly two narrowly-scoped
guard-path hardenings mandated by review (§Forgery resistance), both invisible
to every existing test and to the live guard agent's actual payloads. Every
other widening below applies only to the new travel path. The implementation
mirrors the working-hold class structurally — same marker scheme, same
idempotency cache shape, same conflict check, same cancel state machine — with
travel-specific summary prefix, marker class, duration bounds, local-start
window, quota, and idempotency-key qualification.

**Serialization invariant (load-bearing, previously implicit):** the writer
processes at most one mutation at a time — `busagent.Loop` dispatches
`HandleEvent` sequentially from a single goroutine
(`internal/busagent/loop.go`). The quota Allow→Record sequence and the
PowerShell conflict-check→Save sequence are only race-free under this
serialization. This is a contract invariant: do not parallelize the event
loop, and exactly one writer instance may run per calendar. With travel, two
independent drivers (scheduler request path + phase-3 watcher) will both send
mutations here; they share the one serialized writer.

### Class summary (delta vs working holds)

| Property | Working hold | Travel block |
|---|---|---|
| Summary prefix | `Joel + ` | `Travel: ` |
| Marker class line | `hold_class=working-hold` | `hold_class=travel-block` |
| Duration | 15 min – 2 h | **10 min – 2 h** |
| Local start window | 07:00–21:00 | **05:00–23:00** (spec ruling, below) |
| Quota (live inserts, per event local date) | 2/requester, 5 global | **8/requester, 20 global** (separate limiter instance) |
| Allowlist | `OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS` | same env, same set |
| Conflict check | yes, guard blocks exempt | identical, plus stale-snapshot check on patch (below) |
| Idempotency | 24 h response cache | same cache instance, **class/action-qualified key** |
| Cancel | `[CANCELLED] ` + `show_as: free` | identical semantics |
| Invalid-input error code | `invalid_hold` | `invalid_travel` |
| Description size | unbounded (frozen) | **≤ 4096 bytes** (pre-marker) |

Spec rulings made here (do not re-litigate during implementation):

1. **Local start window 05:00–23:00.** Travel precedes early meetings and
   follows late ones; the holds window (07:00–21:00) would refuse the drive to
   a 07:30 offsite. End must still land on the same local date as start.
2. **Global cap 20/event-date.** Mirrors the holds structure (global cap >
   per-requester cap); 8/requester ≈ four offsite meetings × two legs.
3. **Same allowlist env.** Joel's instruction: created/patched/cancelled ONLY
   by allowlisted hold requesters. No new env var.
4. **Description agenda required non-empty** after marker stripping (pattern
   parity with holds). Convention (recommended, NOT validated, NOT reserved):
   the requester includes a `travel_for=<parent Outlook EntryID>` line plus a
   human route line, so the phase-3 reconciliation watcher can associate a
   block with its parent meeting. The writer treats these as opaque body text.
   **Anything read back from an event body — including `travel_for=` and
   `managed_by=` lines — is untrusted input to downstream consumers** (see
   §Forgery resistance); the watcher must never act destructively on body
   content alone without the ownership checks this agent enforces.
5. **Description cap 4096 bytes (travel only).** The event JSON is shipped to
   PowerShell via the `JK_OUTLOOK_WRITE_EVENT_JSON` environment variable
   (`service.go`), where Windows' ~32 KB env-block limit turns oversized
   bodies into late, raw spawn failures with an empty `error_code`. Travel
   descriptions are refused early (`invalid_travel`,
   `"event.description must not exceed 4096 characters"`) when the inbound
   description exceeds 4096 bytes before marker append. Hold/guard paths are
   frozen and keep today's behavior.

### Forgery resistance and classification precedence (blocker fix)

The guard insert path is unauthenticated by design (any bus agent may insert
quota blocks, and `TestAgentGuardInsertSucceedsWithHoldRequestersUnset`
freezes that). Guard `BuildInsert` today performs no reserved-marker-key check,
so without the rulings below, any bus agent could insert a guard-summary event
whose description embeds a forged 3-line hold/travel marker
(`managed_by=<victim>` / `owner_agent=...` / `hold_class=travel-block`);
`ensureOwnershipMarker` would append the guard marker alongside it, the stored
event would satisfy `HasTravelMarker`, `handlePatch` would route it to the
travel path, and the phase-3 watcher could be induced to adopt/move/cancel
events it never created. Two rulings close this:

1. **Patch classification precedence — guard ownership wins.** `handlePatch`
   classifies the EXISTING event in this order: (a) body satisfies the guard
   ownership check (`HasOwnershipMarker` — exact substrings
   `managed_by=jk-calendar-guard-agent` + `owner_agent=ucla-tdg-outlook-calendar-write-agent`)
   → ALWAYS guard class, regardless of any embedded hold/travel marker;
   (b) else exact 3-line hold marker → hold path; (c) else exact 3-line travel
   marker → travel path; (d) else the existing fallback: allowlisted requester
   → `not_owned` with the EXISTING message
   `"hold requesters may only patch their own working holds"` (byte-for-byte —
   `TestAgentHoldRequesterPatchingGuardBlockReturnsNotOwned` asserts this
   string); otherwise guard `BuildPatch`, which refuses unmarked events as
   today. Legitimate holds/travel blocks carry `managed_by=<requester>`, never
   the guard `managed_by=jk-calendar-guard-agent` line, so step (a) never
   captures them; every existing hold and guard test passes unmodified
   (verified: no existing test exercises dual markers). An event that was
   forged through the guard path is therefore permanently guard-class and can
   never be managed via the hold/travel paths.
2. **Guard inserts refuse `hold_class=`.** Guard `BuildInsert` gains exactly
   one new refusal: a description containing the substring `hold_class=`
   (case-insensitive, same lowercasing as `containsReservedHoldMarkerKey`) is
   rejected with `"event.description must not contain reserved ownership marker keys"`.
   This kills marker forgery at creation, because both `holdMarkerRequester`
   and `travelMarkerRequester` require the `hold_class=...` line as the third
   anchored marker line. **Deliberately NOT the full reserved-key set**: the
   live `jk-calendar-guard-agent` legitimately includes
   `managed_by=jk-calendar-guard-agent` and `owner_agent=<writer id>` lines in
   its insert descriptions (`internal/calendarguard/agent.go`,
   `writeRequestForBlock`), so rejecting `managed_by=`/`owner_agent=` would
   break the live guard agent. It never sends `hold_class=`. Guard `BuildPatch`
   is untouched — `ensureOwnershipMarker` legitimately tolerates round-tripped
   guard markers, and ruling 1 already makes forged-marker patches guard-class.

These are the only two deviations from "guard path byte-for-byte". Both leave
every existing guard test passing unmodified (no existing guard test sends
`hold_class=` or dual markers — verified against `validation_test.go`,
`agent_test.go`, `hold_agent_test.go`, `hold_agent_extension_test.go`) and do
not change behavior for the live guard agent's actual payloads. They are
required to make this spec's safety claims ("created/patched/cancelled ONLY by
allowlisted requesters"; "`managed_by` is ALWAYS the authenticated sender")
true, which is why they are in scope despite the general guard freeze.

### Classification first

`handleInsert` classifies BEFORE any class-specific checks:

- summary starts with `HoldSummaryPrefix = "Joel + "` → working-hold path (unchanged);
- else summary starts with `TravelSummaryPrefix = "Travel: "` → travel path (new);
- else → guard path, exactly as today (plus the `hold_class=` refusal above),
  including when the allowlist env is unset.

The two prefixes are disjoint, so order is immaterial; check hold first to keep
the existing code shape. Note the classification trims the summary first
(mirroring `IsHoldInsert`): a summary of exactly `"Travel:"` or `"Travel: "`
trims to `"Travel:"`, does NOT match the prefix, and falls through to the
guard path, where it gets the guard summary error. The prefix must be followed
by non-whitespace text or the request is treated as a guard write (same
long-standing edge as `"Joel + "`; documented, not changed).

`handlePatch` classifies by the EXISTING event's marker block (never by request
data), in the precedence order fixed in §Forgery resistance: guard ownership
marker → hold marker → travel marker → `not_owned`/guard fallback.

### Ownership marker

Inbound `event.description` for travel inserts/patches MUST NOT contain any
reserved key (`managed_by=`, `owner_agent=`, `hold_class=` — the existing
`ReservedHoldMarkerKeys` list already covers all three; reuse
`containsReservedHoldMarkerKey` verbatim). The agent appends exactly:

```text
managed_by=<authenticated bus sender id (evt.From)>
owner_agent=ucla-tdg-outlook-calendar-write-agent
hold_class=travel-block
```

`managed_by` is ALWAYS the authenticated envelope sender, never request data.
Marker matching is anchored full-line, 3 lines in sequence, after the same
CR/LF + trim normalization as `descriptionLines` (Outlook trailing-whitespace
round-trip). Because the third line differs, `HasHoldMarker` never matches a
travel block and `HasTravelMarker` never matches a hold (test required, both
directions). A travel patch additionally requires the existing event's
`managed_by` to equal the requesting sender — agents can only patch their own
travel blocks. Guard `HasOwnershipMarker` is untouched.

**Round-trip warning (requester contract):** because the reserved-key refusal
substring-matches anywhere in the description, patching back a description
that was READ from Outlook is always refused — stored bodies contain the
marker block. Patch descriptions must be the agenda text only: strip the
marker block (the 3 lines starting at the `managed_by=` line, i.e. the
`stripTravelMarker` semantics) before re-sending a body read from Outlook; the
writer re-appends the marker. This applies to the phase-3 watcher's natural
read-tweak-write loop.

### Validation (insert and re-validated on every non-cancel patch)

- `event.summary` required; after trim, must start with `"Travel: "`.
  Recommended (not enforced) form: `Travel: <origin> → <destination>`.
- `event.description` required; ≤ 4096 bytes; must not contain reserved marker
  keys; agenda must be non-empty after marker stripping (`stripTravelMarker`).
- Timed only — `start.date_time`/`end.date_time` required; any `date` form
  rejected with `"travel blocks must be timed events"`.
- Time zone: `time_zone` absent or exactly `America/Los_Angeles`, AND each
  `date_time`'s numeric offset must equal that zone's offset at that instant.
  Mechanics shared with holds via extracted label-parameterized helpers (see
  the error-string ruling below) — hold error strings stay byte-identical.
- Start and end on the same local date (`sameLocalDate`, `DefaultTimeZone`).
- Duration ≥ 10 minutes and ≤ 2 hours
  (`minTravelDuration = 10 * time.Minute`, `maxTravelDuration = 2 * time.Hour`);
  error `"travel-block duration must be at least 10 minutes and no more than 2 hours"`.
- Start in the future; within the next 30 days.
- Local start between 05:00 and 23:00 inclusive
  (`startMinutes < 5*60 || startMinutes > 23*60` refused);
  error `"travel-block local start must be between 05:00 and 23:00"`.
- `show_as` must normalize to `busy` (cancel is the only path to `free`).
- Optional `location` is copied to Outlook's visible Location field. It is
  display-only; ownership and reconciliation still come from markers, summary
  grammar, and live start/end.
- Attendees / recurrence / conference fields rejected via `ProhibitedFields`
  (unchanged mechanism).
- Host-timezone gate: travel writes (inserts AND patches) are refused with
  `tz_mismatch` whenever `HoldTimeZoneOK` is false — same config field, same
  probe, same message fallback as holds (travel events are timed, so the gate
  applies identically).

#### Travel error strings (normative)

Rule: every class-labelled hold validation message is reused with
`working-hold` → `travel-block` and `working holds` → `travel blocks`
substituted; class-neutral messages are reused byte-identically. Enumerated:

| Travel message | Derivation |
|---|---|
| `event.summary must start with "Travel: "` | hold prefix message, `%q` = `TravelSummaryPrefix` |
| `event.description must contain travel-block ownership marker` | substituted |
| `event.description agenda is required` | neutral, identical |
| `event.description must not contain reserved ownership marker keys` | neutral, identical |
| `event.description must not exceed 4096 characters` | new, travel-only |
| `travel blocks must be timed events` | substituted |
| `event.start.date_time and event.end.date_time are required` | neutral, identical |
| `event.start.time_zone must be absent or "America/Los_Angeles"` (and `.end`) | neutral, identical |
| `invalid event.start.date_time: …` / `invalid event.end.date_time: …` | neutral, identical |
| `event.start.date_time offset must match America/Los_Angeles` (and `.end`) | neutral, identical |
| `event.start and event.end must be on the same local date` | neutral, identical |
| `travel-block duration must be at least 10 minutes and no more than 2 hours` | substituted + new bounds |
| `travel-block start must be in the future` | substituted |
| `travel-block start must be within the next 30 days` | substituted |
| `travel-block local start must be between 05:00 and 23:00` | substituted + new window |
| `event.show_as must be busy` | neutral, identical |
| `requester is required` | neutral, identical |
| `refusing to patch event without travel-block ownership marker` | substituted |
| `travel blocks can only be patched by their creating agent` | substituted |
| `travel-block cancel patch may only set summary and show_as` | substituted |
| `travel-block cancel summary must gain the cancelled prefix` | substituted |
| `travel-block cancel patch must set show_as to free` | substituted |
| `cancelled travel blocks cannot be patched` | substituted |

**Helper-sharing ruling:** `validateHoldTimes` hard-codes
`"working holds must be timed events"`, so "reuse it directly" is forbidden on
the travel path — it would ship the hold-labelled message. Extract a
label-parameterized helper (e.g. `validateClassTimes(start, end EventDateTime,
timedMessage string)`), with the hold caller passing the existing string so
`"working holds must be timed events"` stays byte-identical and the travel
caller passing `"travel blocks must be timed events"`. `validateHoldDateTime`'s
messages are class-neutral and may be reused directly.

### Conflict check (writer-enforced, in the PowerShell COM script)

Identical rules to working holds, byte-for-byte shared implementation
(`Assert-NoBusyConflict` is reused unmodified):

- A travel insert (and any non-cancel patch, excluding the patched event
  itself) is refused if it overlaps any existing event with
  `BusyStatus != olFree`, **except calendar-guard quota blocks** (exact
  guard-marker-lines exemption — Joel's 2026-06-11 ruling; summaries are never
  trusted).
- Overlap is strict (`[End] > start AND [Start] < end`), so **abutment is
  permitted by construction**: a travel block ending exactly when its parent
  meeting starts does not conflict — that is the intended shape.
- Everything else busy still conflicts: real meetings, private appointments,
  working holds, and **other travel blocks**. Travel blocks get no exemption
  for each other or for holds.
- Conflict failures carry only overlapping time ranges, never subjects
  (privacy), and map to error code `conflict` via the existing
  `holdServiceErrorCode` substring mapping.

#### Stale-snapshot check on travel patches (lost-update race)

`handlePatch` is read-modify-write: GetEvent → Build*Patch (merging into the
snapshot) → PatchEvent, and the PowerShell patch unconditionally rewrites
Subject, Body, Start, End, BusyStatus from the merged snapshot. If Joel drags
the event in Outlook between the Go read and the COM write, a hold patch today
silently reverts his change. That window was tolerable for rare manual hold
operations; this spec's design context is an autonomous watcher patching
travel blocks every ~15 minutes around meetings Joel actively rearranges, so
the race becomes routine. The self-exclusion in the conflict check
(`$excludeEntryId`) means it cannot catch this. Travel-path fix, in scope:

- New optional capability interface in `contracts.go`:

  ```go
  type SnapshotCheckedService interface {
      PatchEventExpecting(ctx context.Context, calendarID, eventID string,
          expectedStart, expectedEnd EventDateTime, event StoredEvent) (StoredEvent, error)
  }
  ```

- Every LIVE travel patch (including cancel — a stale cancel would rewrite
  times/body from stale data too) type-asserts the service; when implemented,
  it calls `PatchEventExpecting` with the GetEvent snapshot's `Start`/`End`;
  otherwise it falls back to plain `PatchEvent` (so frozen test fakes keep
  compiling and passing unmodified).
- `PowerShellService` implements it by additionally setting
  `JK_OUTLOOK_WRITE_EXPECT_START` / `JK_OUTLOOK_WRITE_EXPECT_END`; the PS patch
  branch, when both env vars are non-empty, compares them (via
  `Parse-EventTime`) against the live `$item.Start`/`$item.End` and throws
  `"conflict: stale snapshot " + <live start>/<live end>` on mismatch — which
  maps to the existing `conflict` error code via `holdServiceErrorCode`
  (ranges only, no subjects). Empty env vars (hold/guard paths, plain
  `PatchEvent`) skip the check entirely, so hold and guard behavior is
  byte-identical.
- The hold path keeps today's race (frozen); this spec documents it, and the
  phase-3 watcher MUST re-read an event immediately before any destructive
  patch and treat `conflict` as "re-plan from fresh state", never retry-as-is.

### Patch and cancel

State machine identical to holds:

1. **Active travel block** → *modify*: patch summary (must keep
   `"Travel: "` prefix), description, times (re-validated under all insert
   rules + conflict check + stale-snapshot check). Typical use: the
   reconciliation watcher moving a block when the parent meeting moves.
2. **Active travel block** → *cancel*: a patch whose ONLY changes are summary
   gaining the literal prefix `[CANCELLED] ` (either bare `[CANCELLED]`/
   `[CANCELLED] ` or `[CANCELLED] ` + the exact existing summary) and
   `show_as` becoming `free`. No other field may be present in the patch.
   Time/window rules not re-enforced; no busy-conflict check (it is becoming
   free); stale-snapshot check still applies on the live write.
3. **Cancelled travel block** (summary starts `[CANCELLED] `): all further
   patches refused, except an idempotent re-cancel (same shape) which returns
   the existing event without calling the mutation service.

No delete capability. `show_as: free` → `BusyStatus = 0` (olFree), unchanged
PowerShell mapping. Cancel-shape errors are the hold messages with the label
swapped (see the normative error-string table). Implementation: extract
`buildClassCancelPatch(existing StoredEvent, patch EventInput, label string)`
with `buildHoldCancelPatch` delegating with label `"working-hold"` so all hold
messages stay byte-identical, and the travel caller passing `"travel-block"`.

### Authorization, quota, idempotency

- Allowlist: the existing `OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS` set gates
  travel inserts AND patches (test-asserted, both: tests 21–22). Unset/empty ⇒
  travel requests refused (`not_allowlisted`, fail closed); guard path
  unaffected.
- Quota: caps on LIVE (non-dry-run) successful inserts, keyed by the block's
  *event local date* (reuse `HoldEventLocalDate` / `storedEventLocalDate`):
  max **8 per requester per event-date**, max **20 globally per event-date**.
  This is a **separate limiter instance** from the hold limiter — travel
  inserts never increment hold counts and hold inserts never increment travel
  counts (explicit test required, exact interleaving in test 14). In-memory;
  restart resets — same accepted trade-off as holds (the conflict check
  independently prevents double-booking).
- Idempotency: reuse the existing 24 h response cache **instance**
  (`holdCache`), but travel keys are **class/action-qualified**:
  `sender + "/event-insert/travel-block/" + request_id` and
  `sender + "/event-patch/travel-block/" + request_id` (same
  request_id → message-id → conversation-scoped fallback chain as
  `holdIdempotencyKey`). Hold keys are UNCHANGED (frozen path). Because the
  travel key format is disjoint from the hold key format, a request_id reused
  across classes can never silently replay the other class's cached response —
  the failure mode review flagged: the scheduler already derives write ids as
  `"sched-" + keyHash + "-" + step` with steps `insert`/`move`/`cancel`
  (`internal/scheduler/execution.go`), so a phase-3 atomic booking (1 hold
  insert + 2 travel inserts in one session) would otherwise produce three
  writes named `sched-<hash>-insert` and get the first's cached response for
  the other two, leaving travel legs unwritten while the caller believes they
  exist.
  - **Requester contract (normative, also goes in
    `docs/OUTLOOK_CALENDAR_WRITE_AGENT.md`):** `meta.request_id` MUST be
    unique per mutation attempt, across all event classes and actions. Reuse
    within the same class+action returns the original cached response without
    re-validating that the new request resembles it (intended retry
    semantics); the writer cannot distinguish a retry from a new mutation.
    The phase-3 scheduler extension MUST give each travel leg a distinct step
    suffix (e.g. `-travel-1`, `-travel-2`); key qualification alone does not
    disambiguate two travel inserts sharing one id.
  - Cache hygiene: `holdResponseCache.Put` gains an expired-entry sweep that
    runs whenever the map exceeds 1024 entries (entries are otherwise only
    lazily evicted on `Get` of the same key, and dry-run — the default —
    caches every unique request id with no quota gate, so a buggy allowlisted
    agent emitting unique ids grows writer memory without bound). Invisible to
    all existing tests.
- Handler ordering mirrors the hold handlers:
  - insert: allowlist → tz gate → idempotency cache → quota `Allow` (event
    local date) → `BuildTravelInsert` → dry-run or `InsertEvent` → quota
    `Record` → cache → respond.
  - patch: allowlist → idempotency cache → tz gate → `BuildTravelPatch` →
    cancelled-idempotent short-circuit → dry-run or
    `PatchEventExpecting`/`PatchEvent` → cache → respond.
  - **Normative-source rule:** wherever these simplified bullets and the
    actual `handleHoldInsert`/`handleHoldPatch` bodies differ, the hold
    handler bodies are normative — e.g. quota `Allow` runs only when
    `!cfg.DryRun && dateOK`, the event date falls back to
    `storedEventLocalDate` after Build when the pre-parse fails, and `Record`
    is skipped on dry-run. The travel handlers mirror the hold bodies exactly,
    with `invalid_travel`, `travelRates`, the qualified idempotency key, and
    travel metric names substituted.

### Error codes

Reuse the existing `MutationResponse` shape (`dry_run`, `event`, `would_write`,
`error`, `error_code`). Codes on the travel path:

| Code | When |
|---|---|
| `not_allowlisted` | sender not in `OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS` (insert or patch) |
| `tz_mismatch` | host-timezone probe failed at startup (insert or patch) |
| `rate_limited` | travel quota exceeded for the event local date |
| `invalid_travel` | any Build/validate failure (shape, window, size cap, marker spoof, cross-agent patch, cancel-shape, cancelled-block patch) |
| `conflict` | service error contains "conflict" (busy overlap OR stale snapshot) |
| `not_owned` | allowlisted requester patching an event with neither guard, hold, nor travel marker — or a guard-marked event (precedence ruling) — existing code path, existing message |

### Metrics

New counters, named by pattern: `event_travel_insert`,
`event_travel_insert_dry_run`, `event_travel_insert_idempotent`,
`event_travel_patch`, `event_travel_patch_dry_run`,
`event_travel_patch_idempotent`, `event_travel_cancel_idempotent`. Existing
counters untouched.

## Request contract (JSON)

Travel insert (bus message body to `ucla-tdg-outlook-calendar-write-agent`,
type `request`, capability `event-insert`, with `meta.request_id` set —
**unique per mutation attempt across all classes**, see idempotency section):

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

Success response (`would_write` instead of `event` in dry-run); stored
description gains the marker block:

```json
{
  "dry_run": false,
  "event": {
    "id": "<Outlook EntryID>",
    "summary": "Travel: Alto Cedro → 200 Medical Plaza",
    "description": "travel_for=00000000ABCD...\nDestination: 200 Medical Plaza, Los Angeles, CA 90024\nroute: drive ~25 min + 10 min parking (200 Medical Plaza structure)\n\nmanaged_by=ucla-tdg-scheduler-agent\nowner_agent=ucla-tdg-outlook-calendar-write-agent\nhold_class=travel-block",
    "location": "200 Medical Plaza, Los Angeles, CA 90024",
    "start": {"date_time": "2026-06-15T08:20:00-07:00", "time_zone": "America/Los_Angeles"},
    "end":   {"date_time": "2026-06-15T08:55:00-07:00", "time_zone": "America/Los_Angeles"},
    "show_as": "busy"
  }
}
```

Move (re-validated, conflict-checked excluding self, stale-snapshot-checked).
**Note:** if the patch includes `description`, it must be agenda text only —
never a body read back from Outlook with the marker block still in it (the
reserved-key refusal rejects round-tripped markers; strip everything from the
`managed_by=` marker line onward first — the writer re-appends the marker):

```json
{
  "action": "event-patch",
  "calendar_id": "default",
  "event_id": "<Outlook EntryID>",
  "event": {
    "start": {"date_time": "2026-06-15T08:00:00-07:00", "time_zone": "America/Los_Angeles"},
    "end":   {"date_time": "2026-06-15T08:35:00-07:00", "time_zone": "America/Los_Angeles"}
  }
}
```

Cancel (only summary + show_as; everything else, including location, refused):

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

Error response shape (example):

```json
{"dry_run": false, "error": "conflict: 2026-06-15T08:30:00-07:00/2026-06-15T09:00:00-07:00", "error_code": "conflict"}
```

## Files to touch (exact)

Historical implementation list from the original branch. Current source lives
under `/home/joelkehle/Projects/shared/calendar-agents`; path names below are
relative to that repo unless they explicitly describe the original review
context.

Post-migration note: shared event-class constants now live in
`~/Projects/shared/calendar-agents/pkg/calendarcontract`, and the write-agent
request/response schema now lives in
`~/Projects/shared/calendar-agents/pkg/outlookwritecontract`. The local
`internal/outlookcalendarwrite/contracts.go` file should alias them rather than
redefine them.

1. `internal/outlookcalendarwrite/contracts.go`
   - Add consts: `TravelSummaryPrefix = "Travel: "`,
     `TravelClassMarker = "hold_class=travel-block"`.
   - Add the `SnapshotCheckedService` optional interface.
   - `ReservedHoldMarkerKeys` already covers `hold_class=`; no change.
2. `internal/outlookcalendarwrite/travel_markers.go` (new)
   - `ensureTravelMarker(description, requester)`, `travelMarkerBlock(requester)`,
     `travelMarkerRequester(description) (string, bool)`,
     `stripTravelMarker(description)`, exported `HasTravelMarker(description)` —
     exact mirrors of `hold_markers.go` with `TravelClassMarker` as the third
     anchored line; reuse `descriptionLines` and
     `containsReservedHoldMarkerKey` unchanged.
3. `internal/outlookcalendarwrite/validation.go`
   - Consts `minTravelDuration = 10 * time.Minute`, `maxTravelDuration = 2 * time.Hour`,
     `maxTravelDescriptionBytes = 4096`.
   - `IsTravelInsert(event EventInput) bool` (trimmed summary has
     `TravelSummaryPrefix`), `IsTravelPatch(existing StoredEvent) bool`
     (`HasTravelMarker`).
   - Guard `BuildInsert`: add the single `hold_class=` refusal (§Forgery
     resistance ruling 2) — refuse before `ensureOwnershipMarker` with
     `"event.description must not contain reserved ownership marker keys"`.
     No other guard change; guard `BuildPatch` untouched.
   - `BuildTravelInsert(event EventInput, requester string) (StoredEvent, error)`
     and `BuildTravelPatch(existing StoredEvent, patch EventInput, requester string)`
     — mirrors of the hold builders: requester required; size cap;
     reserved-key refusal; same-requester patch ownership
     (`"travel blocks can only be patched by their creating agent"`);
     cancel-patch short-circuit; cancelled-block patch refusal;
     `validateTravelFinalEvent(event, time.Now())`.
   - `validateTravelFinalEvent`: prefix, marker, non-empty agenda, timed-only,
     tz/offset rules, same local date, 10 min–2 h, future, ≤30 days,
     05:00–23:00 local start, `show_as` busy — using the error strings in the
     normative table.
   - Extract `validateClassTimes(start, end EventDateTime, timedMessage string)`
     from `validateHoldTimes` (hold caller passes
     `"working holds must be timed events"`, byte-identical; travel passes
     `"travel blocks must be timed events"`). Do NOT reuse `validateHoldTimes`
     directly on the travel path. `validateHoldDateTime` reused as-is
     (class-neutral messages).
   - Extract `buildClassCancelPatch(existing, patch, label string)` with
     `buildHoldCancelPatch` delegating with label `"working-hold"` so all hold
     messages stay byte-identical; travel delegates with `"travel-block"`.
4. `internal/outlookcalendarwrite/hold_state.go`
   - Generalize the limiter: give it per-requester and per-date limit fields
     set at construction (`newRateLimiter(perRequester, perDate int)`); hold
     instance keeps `(2, 5)` via the existing constants; add
     `maxTravelInsertsPerRequesterPerDate = 8`,
     `maxTravelInsertsPerDate = 20`. Behavior-preserving refactor only — all
     existing hold limiter tests pass unmodified.
   - Add `travelIdempotencyKey(evt, action)` producing the class/action-
     qualified key (hold key function untouched).
   - `holdResponseCache.Put`: expired-entry sweep when `len(entries) > 1024`.
5. `internal/outlookcalendarwrite/agent.go`
   - `Agent` gains `travelRates` (second limiter instance, travel limits);
     `holdCache` is shared. `AgentConfig` unchanged.
   - `handleInsert`: after `IsHoldInsert` branch, add
     `if IsTravelInsert(req.Event) { return a.handleTravelInsert(...) }`.
   - `handlePatch`: re-ordered per the precedence ruling — guard
     `HasOwnershipMarker(existing.Description)` checked FIRST (routing to the
     existing `not_owned`-for-requesters / guard-`BuildPatch` logic), then
     `IsHoldPatch` (unchanged hold path), then `IsTravelPatch` (new travel
     path), then the existing `isHoldRequester` → `not_owned` fallback with
     its message untouched.
   - `handleTravelInsert` / `handleTravelPatch`: exact mirrors of the hold
     handler BODIES (normative-source rule) with `invalid_travel`,
     `travelRates`, `travelIdempotencyKey`, the travel metric names, and the
     snapshot-checked patch call; reuse `holdServiceErrorCode`,
     `HoldEventLocalDate`, `storedEventLocalDate`, `isCancelledHold` (rename
     to `isCancelledManaged` ONLY if all hold call sites stay identical;
     otherwise add a travel twin).
   - Bus registration `Description` string: extend to mention travel blocks
     (e.g. "guard-block, working-hold, and travel-block agent ...");
     `Capabilities` stay `event-insert`/`event-patch`.
6. `internal/outlookcalendarwrite/service.go` (PowerShell, last line of defense)
   - Add `Test-TravelMarker($body)` — copy of `Test-HoldMarker` with the third
     line `"hold_class=travel-block"`.
   - Insert conflict gate becomes:
     `if (((Test-HoldMarker ([string]$payload.description)) -or (Test-TravelMarker ([string]$payload.description))) -and [string]$payload.show_as -ne "free") { Assert-NoBusyConflict $namespace "" $payload.start.date_time $payload.end.date_time }`.
   - Patch ownership: `$hasManagedOwnership = (Test-HoldMarker $body) -or (Test-TravelMarker $body)`;
     when false, the existing guard-marker refusal runs verbatim. Patch
     conflict gate extended the same way as insert (still excluding
     `$item.EntryID`).
   - Patch stale-snapshot check: when `JK_OUTLOOK_WRITE_EXPECT_START` and
     `JK_OUTLOOK_WRITE_EXPECT_END` are both non-empty, compare against
     `$item.Start`/`$item.End` and `throw ("conflict: stale snapshot " + ...)`
     on mismatch, before any field is written.
   - `PowerShellService` implements `PatchEventExpecting` (sets the two env
     vars; plain `PatchEvent` leaves them empty/unset).
   - **Regression constraint:** `service_test.go`'s
     `TestPowerShellScriptKeepsGuardDefenseAndAddsHoldConflictCheck` asserts
     exact substrings of `outlookWritePowerShell` (including the literal
     `Test-HoldMarker $body` and the guard refusal `if (-not
     $body.Contains("managed_by=jk-calendar-guard-agent") ...)` lines, and the
     absence of `$candidate.Subject`). Every required substring must survive
     the rewrite verbatim — the `$hasManagedOwnership = (Test-HoldMarker
     $body) -or (Test-TravelMarker $body)` form does.
   - `Assert-NoBusyConflict`, `Test-GuardOwnershipMarker`, olFree mapping,
     reminder suppression: unchanged.
7. `cmd/outlook-calendar-write-agent/main.go` — ONE-line change: the tz-probe
   failure log becomes
   `"%s working-hold and travel-block writes disabled: %v"` (the same
   `HoldTimeZoneOK=false` flag now disables both classes; the old message
   would mislead an operator during incident triage). Nothing else.
8. `scripts/install-outlook-calendar-write-agent.ps1` — **no change**.
9. `docs/OUTLOOK_CALENDAR_WRITE_AGENT.md` — document the third class: prefix,
   marker block, duration/window, size cap, quotas, cancel, `travel_for=`
   convention (untrusted on read-back), request/response examples, the
   request_id-uniqueness rule, the marker-strip-before-patch rule, and the
   single-writer serialization invariant (mirror the working-hold section).
10. `docs/OUTLOOK_CALENDAR_TRAVEL_CLASS_SPEC.md` — this file.
11. Tests (new files): `internal/outlookcalendarwrite/travel_validation_test.go`,
    `internal/outlookcalendarwrite/travel_agent_test.go`,
    `internal/outlookcalendarwrite/travel_service_test.go`. Existing test
    files are not modified. Add helpers `validTravelInput*` mirroring
    `validHoldInput*` (summary `TravelSummaryPrefix + "Office → 200 Medical Plaza"`,
    description `"travel_for=evt-parent"`).

## Test list (table-driven where shaped that way)

`travel_validation_test.go`:

| # | Test | Asserts |
|---|---|---|
| 1 | `TestBuildTravelInsertAppendsMarkerBlock` | marker is exactly `managed_by=<requester>\nowner_agent=...\nhold_class=travel-block`; no guard `managed_by` line; `HasTravelMarker` true |
| 1a | `TestBuildTravelInsertCarriesLocation` | optional `location` is trimmed and carried into `StoredEvent.Location` |
| 2 | `TestBuildTravelInsertRejectsSpoofedMarkerKeys` | description containing any of `managed_by=`/`owner_agent=`/`hold_class=` refused with "reserved" |
| 3 | `TestBuildTravelInsertRejectsUnsafeShapes` (table) | all-day form; 9-min duration; 121-min duration; cross-midnight end; start in past; start >30 days out; local start 04:59; local start 23:01; `time_zone` ≠ `America/Los_Angeles`; offset not matching LA at that instant; `show_as: tentative`; missing summary/description/start/end; empty agenda after marker strip; description > 4096 bytes; prohibited `attendees` field; summary without `Travel: ` prefix not classified as travel (`IsTravelInsert` false) — including the surprising bare cases `"Travel:"` and `"Travel: "` (trim makes them fall through to the guard path) |
| 4 | `TestBuildTravelInsertBoundaryDurations` | exactly 10 min and exactly 120 min accepted; local start 05:00 accepted; local start 23:00 accepted **with a 10-min duration** (any duration > 59 min from 23:00 crosses midnight and would fail `sameLocalDate` for the wrong reason) |
| 5 | `TestBuildTravelPatchRequiresSameRequester` | patch by a different allowlisted agent refused with "creating agent" |
| 6 | `TestBuildTravelPatchKeepsPrefix` | patching summary to `Joel + x` (or any non-`Travel: ` value) refused |
| 7 | `TestBuildTravelPatchCancelStateMachine` (table) | valid cancel (`[CANCELLED] ` + exact summary, `show_as: free`, nothing else) → cancelled event; cancel with extra description/location/start/end refused; cancel with `show_as` ≠ free refused; cancel summary not gaining prefix refused; non-cancel patch of a cancelled block refused; re-cancel of a cancelled block returns existing unchanged; all hold cancel error strings unchanged (delegation check) |
| 7a | `TestBuildTravelPatchUpdatesLocation` | non-cancel travel patches can update the visible Outlook Location field |
| 8 | `TestTravelAndHoldMarkersAreDisjoint` | `HasHoldMarker(travel description)` false; `HasTravelMarker(hold description)` false; `IsHoldPatch`/`IsTravelPatch` route accordingly; marker survives Outlook CRLF + trailing-whitespace round-trip (mirror existing hold round-trip test) |
| 8a | `TestBuildGuardInsertRejectsEmbeddedHoldClassKey` | guard insert (`No more meetings ...` summary) whose description embeds a full forged travel marker block — or any `hold_class=` substring, any case — refused with "reserved"; guard insert with the live guard agent's actual description shape (`<reason>\n\nmanaged_by=jk-calendar-guard-agent\nowner_agent=<writer>`) still ACCEPTED (managed_by/owner_agent alone not refused) |

`travel_agent_test.go` (bus-recorder pattern from `hold_agent_test.go`; every
insert in quota tests uses a **distinct `meta.request_id`**, or the shared
cache short-circuits before quota is exercised):

| # | Test | Asserts |
|---|---|---|
| 9 | `TestAgentTravelInsertRequiresAllowlistedRequester` | allowlist unset/non-member → `not_allowlisted`; no service call |
| 10 | `TestAgentTravelInsertRefusedOnTimeZoneMismatch` | `HoldTimeZoneOK: false` → `tz_mismatch` |
| 11 | `TestAgentTravelInsertDuplicateRequestReturnsCachedResponse` | same `meta.request_id` (or message id) twice → 1 insert call, identical response |
| 11a | `TestAgentTravelIdempotencyKeyClassQualified` | hold insert with request_id X, then travel insert with the same request_id X → travel insert is NOT replayed from the hold cache entry (2 insert calls, second response is the travel event) |
| 12 | `TestAgentTravelInsertRefusesNinthSameDatePerRequester` | 8 live inserts on one event-date succeed, 9th → `rate_limited` |
| 13 | `TestAgentTravelInsertRefusesTwentyFirstSameDateGlobally` | 20 across requesters succeed, 21st → `rate_limited`; **needs ≥3 allowlisted requesters** (per-requester cap is 8: e.g. 8+8+4=20) |
| 14 | `TestAgentTravelQuotaIndependentOfHoldQuota` | exact interleaving, one agent instance, one event-date D: requester R1 — 2 holds succeed, 3rd hold `rate_limited`, then travel 1–8 from R1 on D all succeed (hold exhaustion does not bleed into travel), 9th travel `rate_limited`; requester R2 — 8 travels on D succeed, 9th travel `rate_limited`, then 2 holds from R2 on D still succeed (travel exhaustion does not bleed into holds). Global caps respected by construction (holds 4 ≤ 5, travel 16 ≤ 20) |
| 15 | `TestAgentTravelInsertReturnsConflictCode` | service error `conflict: <ranges>` → `error_code: "conflict"` |
| 16 | `TestAgentTravelPatchByNonCreatorRefused` | existing travel block `managed_by=agent-a`; patch from allowlisted `agent-b` → `invalid_travel` |
| 17 | `TestAgentTravelCancelDuplicateRequestIDReturnsCachedResponse` | cancel idempotency via cache, 1 patch call |
| 18 | `TestAgentTravelDoubleCancelSucceedsWithoutMutation` | already-cancelled block + cancel patch → success response, 0 patch calls |
| 19 | `TestAgentTravelInsertDryRunDefault` | `DryRun: true` → `would_write` populated, no service call, no quota consumption |
| 20 | `TestAgentGuardInsertStillSucceedsWithTravelCodePresent` | guard insert with allowlist unset still succeeds (companion to existing test; existing one stays untouched) |
| 21 | `TestAgentTravelPatchRequiresAllowlistedRequester` | existing travel-marked event; sender not in allowlist → `not_allowlisted`, 0 PatchEvent calls |
| 22 | `TestAgentTravelPatchRefusedOnTimeZoneMismatch` | existing travel-marked event; `HoldTimeZoneOK: false` → `tz_mismatch`, 0 PatchEvent calls |
| 23 | `TestAgentPatchGuardOwnershipPrecedesForgedTravelMarker` | existing event whose body contains BOTH the guard ownership marker and a forged 3-line travel marker: patch from an allowlisted requester → `not_owned` with the existing message (guard class wins); patch never reaches the travel path (0 travel-path effects, 0 PatchEvent calls) |
| 24 | `TestAgentTravelPatchStaleSnapshotConflict` | fake service implementing `SnapshotCheckedService` whose `PatchEventExpecting` returns a `conflict: stale snapshot ...` error → `error_code: "conflict"`; fake NOT implementing it → travel patch falls back to plain `PatchEvent` and succeeds |

`travel_service_test.go` (pure Go, runs on Linux — the PS script IS
unit-testable via substring assertions, mirroring
`TestPowerShellScriptKeepsGuardDefenseAndAddsHoldConflictCheck`):

| # | Test | Asserts |
|---|---|---|
| 25 | `TestPowerShellScriptAddsTravelDefense` | `outlookWritePowerShell` contains `function Test-TravelMarker($body) {`, `hold_class=travel-block`, the travel-extended insert and patch conflict-gate lines, the `$hasManagedOwnership = (Test-HoldMarker $body) -or (Test-TravelMarker $body)` ownership line, and the `JK_OUTLOOK_WRITE_EXPECT_START`/`JK_OUTLOOK_WRITE_EXPECT_END` stale-snapshot check; still does NOT contain `$candidate.Subject` |

Regression gates (existing tests, must pass UNMODIFIED): the full
`hold_*_test.go`, `validation_test.go`, `agent_test.go`, `service_test.go`
suites — including `TestAgentGuardInsertSucceedsWithHoldRequestersUnset`,
`TestAgentHoldRequesterPatchingGuardBlockReturnsNotOwned`,
`TestAgentGuardPatchPathUnchangedForGuardAgent`,
`TestPowerShellScriptKeepsGuardDefenseAndAddsHoldConflictCheck` (all its
required substrings must survive the script rewrite), and every hold
quota/cancel/idempotency test. `go build ./... && go test ./...` green before
commit on `location-awareness`.

Live smoke is explicitly out of scope (no deploy, no laptop access); the COM
behavior of the script changes is additionally covered by exact-diff review
against the snippets in §Files-to-touch item 6, on top of test 25.

## Unchanged behavior (explicit)

- Guard blocks: summary rules (`No more meetings` prefix / `Meeting Quota
  Reached`), all-day/≤4 h validation, `ensureOwnershipMarker`, `BuildPatch`,
  the guard PowerShell ownership refusal text, and all guard tests —
  unchanged, with exactly the two review-mandated hardenings in §Forgery
  resistance (guard `BuildInsert` refuses `hold_class=`; `handlePatch` checks
  guard ownership first). Both are invisible to every existing test and to the
  live guard agent's payloads (verified against
  `internal/calendarguard/agent.go`).
- Working holds: prefix, marker block, 15 min–2 h, 07:00–21:00, 30-day window,
  2/5 quota values and counting, cancel state machine and its exact error
  strings, `not_owned` fallback message, `invalid_hold` code, idempotency key
  format and semantics, the read-modify-write patch race (documented above,
  not fixed on the frozen hold path) — byte-for-byte. The limiter refactor
  (item 4), the `validateClassTimes`/`buildClassCancelPatch` extractions, and
  the cache sweep must be invisible to every existing hold test.
- `Assert-NoBusyConflict` (including the guard-marker exemption and
  ranges-only privacy), `Test-GuardOwnershipMarker`, `Test-HoldMarker`,
  busy-status mapping, reminder suppression, JSON I/O of the COM script.
- Bus capabilities (`event-insert`, `event-patch`), agent id, HTTP health
  endpoints, dry-run default (true), `calendar_id` normalization,
  `OUTLOOK_CALENDAR_WRITE_*` env surface (no new vars), host-timezone probe.
- Single-writer sequential event handling (`busagent.Loop`) — now an explicit
  contract invariant, see §Design.
- No delete capability, no attendees/recurrence/conference fields, no email,
  no new state store (quota + idempotency remain ephemeral process memory;
  Outlook stays source of truth — STATE_ARCHITECTURE.md unchanged).

## Out of scope (explicitly)

- Scheduler-side behavior: atomic meeting+travel booking, the ~15 min
  reconciliation watcher, leave-by computation, venue/travel-time data
  (`locations.json`, venues file) — separate specs (location-prep phases 2–3,
  scheduler.v1 extension). This spec only gives the scheduler a sanctioned
  write primitive. Two writer-contract obligations on those specs are fixed
  here: per-mutation-unique `meta.request_id`s (the current
  `sched-<hash>-<step>` scheme collides on multi-leg bookings), and re-read
  before destructive patches (stale-snapshot `conflict` means re-plan).
- Real drive times / maps API (phase 5), daily-briefing rendering (repo B).
- All-day or multi-day travel (flights); v1 is local ground travel, 10–120 min.
- Live deployment, installer/scheduled-task changes, JK-profile rollout.

## Review Log (2026-06-11 peer review of v1)

All findings were verified against the code on branch `location-awareness`
before incorporation. One finding required a factual correction; everything
else was accepted.

| # | Severity | Finding | Disposition |
|---|---|---|---|
| 1 | blocker | Guard insert path lets any bus agent forge travel markers (guard `BuildInsert` has no reserved-key check and no allowlist; forged `HasTravelMarker` bodies route patches to the travel path; no dual-marker precedence). | **Accepted with one correction.** Verified: guard `BuildInsert` (validation.go) never calls `containsReservedHoldMarkerKey`, and the guard insert path is allowlist-free. Fixed via §Forgery resistance: guard-ownership-first patch precedence + guard-insert `hold_class=` refusal + untrusted-body rule for the watcher. **Correction:** the finding's suggested "reject reserved marker keys in guard BuildInsert" (full set) is NOT safe — the live `jk-calendar-guard-agent` legitimately embeds `managed_by=jk-calendar-guard-agent` and `owner_agent=<writer>` lines in its insert descriptions (`internal/calendarguard/agent.go`, `writeRequestForBlock`), which the finding itself flagged as needing verification. Only `hold_class=` is rejected; that alone closes the forge vector since both marker matchers require the `hold_class=` third line. Test 8a pins both halves. |
| 2 | should-fix | Class-agnostic shared idempotency cache silently replays wrong-class responses; the scheduler's `sched-<hash>-<step>` id scheme guarantees a phase-3 collision. | **Accepted.** Verified `scheduler/execution.go` (`writeRequest`, steps `insert`/`move`/`cancel`) and the cache-before-Build ordering in `handleHoldInsert`. Travel keys are now class/action-qualified (hold keys frozen); request_id uniqueness per mutation is a normative contract rule; the same-class-same-id residual is named and pushed onto the phase-3 spec. |
| 3 | should-fix | Stale-snapshot full-overwrite patch race; routine for a 15-min watcher. | **Accepted.** Verified the PS patch unconditionally rewrites Subject/Body/Start/End/BusyStatus with no live-time comparison, and self-exclusion blinds the conflict check to it. Fixed travel-only via `SnapshotCheckedService` + `JK_OUTLOOK_WRITE_EXPECT_START/END` → `conflict` (optional-interface design keeps frozen test fakes compiling). Hold race documented, not fixed (frozen path). Watcher re-read rule added. Test 24. |
| 4 | should-fix | No travel-patch allowlist / tz-gate tests. | **Accepted.** Tests 21–22 added. |
| 5 | nit | Single-inflight serialization is load-bearing but unstated. | **Accepted.** Verified `busagent.Loop` dispatches sequentially. Invariant added to §Design and Unchanged behavior. |
| 6 | nit | Round-tripping a read body into a patch is always refused (reserved-key substring match). | **Accepted.** Verified `containsReservedHoldMarkerKey` substring-matches anywhere. Marker-strip rule added to the marker section and request contract. |
| 7 | nit | Unbounded description (Windows env-block limit) and unbounded dry-run cache growth. | **Accepted.** Verified env-var transport and lazy same-key-only eviction. 4096-byte travel-only description cap + sweep-on-Put above 1024 entries added; both invisible to frozen hold tests. |
| 8 | should-fix | "Reuse validateHoldTimes directly" contradicts the mandated travel timed-only message. | **Accepted.** Verified the hard-coded `"working holds must be timed events"`. Reuse-directly struck; label-parameterized `validateClassTimes` extraction mandated; `validateHoldDateTime` confirmed class-neutral and reusable. |
| 9 | should-fix | Travel error-string set incomplete. | **Accepted.** Full normative table enumerated + substitution rule (`working-hold`→`travel-block`, `working holds`→`travel blocks`, neutral messages identical). |
| 10 | should-fix | Test 14 impossible as worded (both directions can't be shown for one requester/date). | **Accepted.** Verified the limiter arithmetic. Test 14 rewritten with an exact two-requester interleaving and the distinct-request_id requirement. |
| 11 | should-fix | "PS not unit-testable on Linux" is wrong; `service_test.go` substring test constrains the rewrite. | **Accepted.** Verified `TestPowerShellScriptKeepsGuardDefenseAndAddsHoldConflictCheck` and its required substrings. Test 25 added in a new file; substring-preservation constraint recorded in item 6. |
| 12 | nit | Insert ordering bullet elides dry-run/dateOK conditionals. | **Accepted.** Normative-source rule added: hold handler bodies win over the simplified bullets. |
| 13 | nit | Cross-class request_id reuse contract unstated for consumers. | **Accepted.** Folded into the finding-2 fix: uniqueness rule in the request contract and `docs/OUTLOOK_CALENDAR_WRITE_AGENT.md`. |
| 14 | nit | Test 13 needs ≥3 requesters; test 4's 23:00 boundary needs a short duration. | **Accepted.** Verified both (8-cap arithmetic; `sameLocalDate` refusal past midnight). Noted in tests 13 and 4. |
| 15 | nit | Bare `"Travel:"` summary falls through to the guard path with a confusing error. | **Accepted.** Verified the trim-then-HasPrefix classification. Bare-prefix cases added to test 3 and documented in §Classification first. |
| 16 | nit | main.go tz-probe log becomes inaccurate but was frozen. | **Accepted.** One-line log update permitted in item 7 (`"working-hold and travel-block writes disabled"`). |
