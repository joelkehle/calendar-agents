---
summary: Spec — scheduler v2 travel awareness: origins/venues knowledge, travel-estimate capability, atomic travel blocks around offsite bookings, and a reconciliation watcher that materializes travel time as real busy calendar events.
read_when:
  - implementing or reviewing scheduler v2 (travel blocks, travel-estimate, watcher)
  - adding or editing venue knowledge (data/venues.json) or Joel's origins (data/locations.json)
  - widening ucla-tdg-outlook-calendar-write-agent with the travel event class
  - deciding how any agent should reason about Joel's travel time
---

# Spec: Scheduler v2 — Origins, Travel Estimates, Travel Blocks, Reconciliation Watcher

Status: implemented from the original 2026-06-11 draft. Runtime source now
lives in `~/Projects/shared/calendar-agents`; older in-file references to
`jk-email-agents` and the `location-awareness` branch describe the historical
implementation origin.

Joel's rulings (2026-06-11), binding on this design:

1. **Travel time MUST be real busy calendar events** ("if I'm in the car 30 min,
   that's on the calendar") — never internal scheduler math. Humans, Outlook
   free/busy, and every agent must see it.
2. **Yellow Outlook category = offsite** (not at Joel's office).
3. Private events are fully visible to the fleet; no redaction concerns here.

## Problem

The fleet now sees `location` and `categories` on events, but nothing protects
the time it takes Joel to get anywhere. The briefing has no single travel brain
to ask "how long to 200 Medical Plaza at 9am"; the scheduler books holds with
zero travel slack; and offsite meetings Joel books himself get no travel
protection at all.

## Non-goals (v1)

- Live drive times / maps API (phase 5 — needs Joel's API key). v1 is a STATIC
  matrix plus a default estimate.
- Travel blocks for personal-profile calendars.
- Moving travel blocks when `schedule-move` moves a hold (the watcher's
  reattach logic covers offsite meetings; scheduler-hold moves leave orphan
  handling to the watcher's adjacency rules, see §7.6).
- Natural-language venue inference from event titles (the briefing's "where is
  this?" nag covers missing locations — design doc phase 2).

## Architecture overview

```
data/locations.json ─┐
data/venues.json ────┴─► internal/travelknowledge (load + match + estimate)
                                  │
                                  ▼
                     internal/scheduler (ucla-tdg-scheduler-agent)
                     ├── travel-estimate capability (read-only, sync reply)
                     ├── schedule-request + location → hold + travel blocks
                     └── reconciliation watcher (15-min tick, today+3 days)
                                  │  events-list (read agent; phase-1.5
                                  │  additive change surfaces entry_id, §0)
                                  │  event-insert / event-patch (write agent)
                                  ▼
                     internal/outlookcalendarwrite
                     └── NEW sanctioned event class: travel block
                         ("Travel: " prefix, own marker, own caps)
```

No new authoritative state store (STATE_ARCHITECTURE.md reviewed): Outlook owns
events; venue/origin knowledge is two flat repo JSON files; all watcher state is
in-memory and restart-safe via writer-side idempotency.

---

## 0. Read-agent prerequisite (phase 1.5): writable event ids

**Why (review blocker B1):** the watcher discovers events via `events-list`,
but the read agent's event ids are SYNTHETIC: `outlookEventID()` in
`internal/outlookcalendar/extractor.go` returns
`"outlook_" + shortHash(GlobalAppointmentID|EntryID|...)` (sha1, first 16 hex
chars), and `extended_properties.private.source_entry` is also a one-way hash
of EntryID. The write agent's patch path resolves targets via
`$namespace.GetItemFromID($env:JK_OUTLOOK_WRITE_EVENT_ID)`
(`internal/outlookcalendarwrite/service.go`), which requires the RAW Outlook
EntryID. Today the only usable write ids come out of the write agent's own
insert responses (`Event-FromItem` sets `id = [string]$item.EntryID`). Without
a change, every §7.5 reattach/cancel would fail "item not found" forever.

**Change (additive, this branch, before any watcher work):**

- `internal/outlookcalendar/extractor.go`: add
  `"entry_id": strings.TrimSpace(row.EntryID)` to
  `ExtendedProperties.Private` (omit the key when EntryID is empty). Raw
  EntryID is an opaque MAPI identifier, not content; per Joel's ruling private
  events are fully fleet-visible, so exposing it is sanctioned. It is exposed
  for masked private events too (the mask hides subject/location, not
  identity).
- `calendarreadcontract.Event.ID` stays the synthetic hash (unchanged — downstream consumers
  key on it). `source_entry` stays as-is (unchanged). This is the ONLY
  read-agent change; everything else in `internal/outlookcalendar` is
  untouched. §11.6 is amended accordingly (no longer "byte-for-byte").
- Test: extend `extractor_test.go` (additively) — row with EntryID ⇒
  `entry_id` present and raw; row without ⇒ key absent.
- The watcher uses `extended_properties.private.entry_id` for every
  `event-patch` it issues. Events lacking it are mutation-skipped with metric
  `watch_travel_no_entry_id` (they still count for adjacency/busy logic).
- Live-smoke item (documented for a later deploy session, NOT run here):
  prove a watcher-discovered block can actually be cancelled via its
  `entry_id`.

---

## 1. Origins and venues — `internal/travel` (new package)

### 1.1 Files

| File | Contents |
|---|---|
| `internal/travelknowledge/locations.go` | `Origins` loader for `data/locations.json` + date-based residence resolution |
| `internal/travelknowledge/venues.go` | `Venues` loader for `data/venues.json` + location-string matching (venue / office / virtual) |
| `internal/travelknowledge/estimate.go` | `Knowledge` (origins+venues), origin-selection rule, `Estimate()` |
| `internal/travelknowledge/locations_test.go`, `venues_test.go`, `estimate_test.go` | table-driven tests (§9) |
| `data/locations.json` | EXISTING — extended with `id` fields (additive, §1.2) |
| `data/venues.json` | NEW — seed in §1.3 |

### 1.2 `data/locations.json` (extend, additive only)

Each residence and the work entry gains a required stable `"id"` (lowercase
kebab). Existing keys unchanged. Resulting shape:

```json
{
  "_doc": "Joel's origin locations ...",
  "residences": [
    {"id": "alto-cedro", "label": "Monte & Jacqueline's (housesitting)",
     "address": "9121 Alto Cedro Drive, Beverly Hills, CA 90210",
     "from": "2026-05-18", "until": "2026-06-30"},
    {"id": "orange-drive", "label": "Home (David Spitzer's house)",
     "address": "322 S. Orange Drive, Los Angeles, CA 90036",
     "from": "2026-07-01", "until": null}
  ],
  "work": {"id": "ucla-tdg-office", "label": "UCLA TDG office",
           "address": "10889 Wilshire Blvd, Los Angeles, CA 90095",
           "note": "UNVERIFIED address — confirm with Joel before relying on it for travel math"}
}
```

Loader validation (all violations ⇒ load error, agent refuses travel features
but otherwise runs — see §1.6 degradation):

- every residence: non-empty `id`, `label`, `address`, valid `from`
  (`YYYY-MM-DD`); `until` null or valid date ≥ `from`.
- ids unique across residences + work.
- residence validity windows must not overlap (inclusive day granularity).
- `work.id` non-empty.

### 1.3 `data/venues.json` (new) — schema `venues.v1`

```json
{
  "schema": "venues.v1",
  "_doc": "Venue knowledge for travel math (static matrix v1). Minutes are hand-entered estimates; promote stable facts to the wiki when proven.",
  "default_travel_minutes": 30,
  "office_aliases": ["10889 wilshire", "wilshire center", "ucla tdg"],
  "virtual_aliases": ["http://", "https://", "microsoft teams", "teams meeting",
                      "zoom", "webex", "meet.google", "dial-in", "conference call"],
  "venues": [
    {
      "id": "200-medical-plaza",
      "name": "200 Medical Plaza",
      "address": "200 Medical Plaza, Los Angeles, CA 90024",
      "match": ["200 medical plaza", "200 med plaza"],
      "walk_minutes": 10,
      "parking": "Patient drop-off/pick-up loop off Westwood Plaza for pickups; 200 Medical Plaza structure adjacent if parking — allow 10-15 min for parking + walk.",
      "travel_minutes": {"ucla-tdg-office": 5, "alto-cedro": 20, "orange-drive": 25}
    },
    {
      "id": "ucla-tdg-office",
      "name": "UCLA TDG office",
      "address": "10889 Wilshire Blvd, Los Angeles, CA 90095",
      "match": ["10889 wilshire", "wilshire center", "ucla tdg"],
      "walk_minutes": 5,
      "parking": "UNVERIFIED — confirm building parking with Joel.",
      "travel_minutes": {"alto-cedro": 20, "orange-drive": 20}
    }
  ]
}
```

Notes:

- **The office is both an origin and a venue.** Office-as-venue supplies
  residence→office minutes (morning meetings AT the office still get a
  leave-by line in the briefing).
- **`virtual_aliases` (review blocker B3):** Outlook routinely auto-populates
  `Location` with "Microsoft Teams Meeting", Zoom/Webex join URLs, or dial-in
  strings. A location matching any virtual alias is NEVER offsite and never
  yields travel math from the location signal (§1.4, §7.2). It is data, not
  code, so Joel can extend the list without a release.
- All seeded minutes are estimates pending Joel's confirmation (§12).
  Implementer: copy the numbers above verbatim; do not invent more venues.
- Loader validation: `schema == "venues.v1"`; `default_travel_minutes` in
  [5, 180]; venue ids unique and non-empty; `name` non-empty; every `match`
  alias non-empty after normalization and ≥ 4 runes; every `office_aliases`
  entry ≥ 6 runes (guards against substring false positives like a bare
  "office" matching "Marc's office"); every `virtual_aliases` entry ≥ 4 runes;
  `walk_minutes` in [0, 60]; every `travel_minutes` value in [1, 180];
  `travel_minutes` keys need not cover every origin (missing key ⇒ default
  fallback, §1.5).

### 1.4 Matching rules (exact)

`normalize(s)`: lowercase, replace all whitespace runs (incl. newlines) with a
single space, trim.

- **Virtual match** (checked FIRST wherever "is this offsite?" is the
  question): a location string is virtual iff any `virtual_aliases` entry
  (normalized) is a substring of the normalized location. Virtual locations
  match no venue, are never offsite, and produce `Source = "virtual"`
  estimates (§1.5).
- **Office match**: a location string "is the office" iff any `office_aliases`
  entry (normalized) is a substring of the normalized location. Checked
  before venue matching wherever "is this offsite?" is the question; note the
  office is still venue-matchable for estimates TO the office.
- **Venue match**: a venue matches a location string iff any of its `match`
  aliases (normalized) is a substring of the normalized location. First match
  in file order wins. No fuzzy matching.
- Empty/whitespace location matches nothing.

### 1.5 Origin selection and estimate (exact)

`Knowledge.Estimate(eventStart time.Time, location string) (Estimate, error)`

Origin rule (deterministic, v1). The clock used is the approximate DEPARTURE
time, not the meeting start — a 09:15 offsite meeting is departed-for from
home, not the office (review nit N2):

1. `departure` = `eventStart − default_travel_minutes` converted to
   `America/Los_Angeles`.
2. `residence` = the residence whose `[from, until]` window (inclusive,
   `until == null` ⇒ open) contains `departure`'s local date. None ⇒ residence
   absent.
3. Destination is the office (per §1.4 office match) ⇒ origin = residence.
   If residence absent ⇒ error `no_origin`.
4. Otherwise: origin = office if `departure` is Mon–Fri AND its local time is
   in [09:00, 18:00); else origin = residence. If the chosen origin is absent
   (no valid residence, or office id missing) fall back to the other; both
   absent ⇒ error `no_origin`.

`no_origin` is a PER-CALL error, distinct from load failure: it fires for any
date outside all residence windows (e.g. any date before 2026-05-18 in the
current data) when the office fallback does not apply. Callers' behavior on
it is normative: travel-estimate ⇒ `estimate_unavailable` (§2.3); offsite
booking ⇒ `estimate_unavailable`, zero writes (§5); watcher ⇒ skip that
meeting this tick with `watch_travel_skipped` + logged reason (§7.3).

Estimate:

```go
type Estimate struct {
    Minutes      int    // drive + walk, total door-to-door
    DriveMinutes int
    WalkMinutes  int
    OriginID     string
    OriginLabel  string
    OriginAddress string
    VenueID      string // "" when unmatched
    VenueName    string // "" when unmatched
    VenueAddress string // "" when unmatched
    Parking      string // "" when unmatched
    Source       string // "matrix" | "default" | "virtual"
    IsOffice     bool   // destination matched office_aliases
    IsVirtual    bool   // destination matched virtual_aliases
}
```

- Location virtual: `IsVirtual = true`, all minutes 0, no origin/venue
  resolution, `Source = "virtual"`. (Callers must not derive travel blocks
  from virtual estimates.)
- Venue matched AND `travel_minutes[originID]` present:
  `DriveMinutes = travel_minutes[originID]`, `WalkMinutes = walk_minutes`,
  `Source = "matrix"`.
- Venue matched but origin key missing, OR no venue matched:
  `DriveMinutes = default_travel_minutes`, `WalkMinutes = 0` (unmatched) or
  `walk_minutes` (matched venue, missing origin), `Source = "default"`.
- `Minutes = DriveMinutes + WalkMinutes`. Invariant: non-virtual estimates
  always have `Minutes ≥ 1` (matrix floor is 1, default floor is 5).
- Travel-block duration derived from an estimate (used in §6/§7):
  `blockMinutes = clamp(roundUpToMultipleOf5(Minutes), 10, 120)`.
- Scheduler-created travel blocks set Outlook `Location` to the endpoint of
  that travel segment, not always to the meeting venue. Before-event blocks
  use the verified venue name + address when venue knowledge has one; return
  blocks use the verified return/next target label + address when origin
  knowledge has one. Summaries stay short (`Travel: Manhattan Country Club...`)
  while minimized Outlook cards show the correct address for that leg.

### 1.6 Loading and degradation

- Scheduler `Config` gains `LocationsPath` (env `SCHEDULER_LOCATIONS_PATH`,
  default `data/locations.json`) and `VenuesPath` (env `SCHEDULER_VENUES_PATH`,
  default `data/venues.json`). Paths resolve relative to the process working
  directory; the systemd unit must set absolute paths at deploy time (deploy is
  out of scope here; document in the env file comments).
- Files are read ONCE at startup (`travel.Load(locationsPath, venuesPath)`).
  No hot reload in v1 (restart to pick up edits) — documented limitation.
- Load failure ⇒ log + metric `travel_knowledge_load_failed`; the agent still
  serves scheduler.v1 exactly as today, but: `travel-estimate` replies
  `error_code=estimate_unavailable`; `schedule-request` with a `location`
  field replies `error_code=estimate_unavailable` (it must not silently book
  without travel protection); the watcher logs once per tick and does nothing.

---

## 2. Bus capability: `travel-estimate` (scheduler.v1 addition)

One travel brain for the fleet. The daily-briefing agent (repo B,
`internal/dailybriefing`) calls this over the UCLA bus to render "leave by
HH:MM — park at X, allow N min" lines; it must NOT grow its own matrix.

### 2.1 Registration

Add `"travel-estimate"` to the scheduler's `Capabilities` slice in
`NewAgent` (`internal/scheduler/agent.go`). Same loop, same passport.

### 2.2 Request

```json
{
  "action": "travel-estimate",
  "request_id": "<caller-unique>",
  "event_start": "2026-06-12T09:00:00-07:00",
  "location": "200 Medical Plaza"
}
```

Validation (synchronous; violations ⇒ `status=error`,
`error_code=invalid_request`, message naming the field):

- `request_id` required (consistency with scheduler.v1; the reply is
  deterministic so no idempotency cache entry is stored).
- `event_start` required, RFC3339 with offset.
- `location` required, non-empty after trim, ≤ 200 runes. (Callers with NO
  location string should not call this — the briefing's missing-location nag
  covers that case.)
- The existing prohibited-field scan (`firstProhibitedField`, populated by
  `DecodeRequest` into `req.ProhibitedField()`) is checked by the estimate
  branch itself; attendee-ish fields ⇒ standard `refused` reply.

### 2.3 Reply

```json
{
  "status": "estimated",
  "request_id": "...",
  "estimate": {
    "minutes": 15,
    "drive_minutes": 5,
    "walk_minutes": 10,
    "origin": {"id": "ucla-tdg-office", "label": "UCLA TDG office"},
    "venue": {"id": "200-medical-plaza", "name": "200 Medical Plaza",
              "parking": "Patient drop-off/pick-up loop ..."},
    "source": "matrix",
    "is_office": false,
    "is_virtual": false
  }
}
```

- ONE new `Reply` field: `Estimate *EstimateResult \`json:"estimate,omitempty"\``.
  A single nil pointer guarantees every existing reply serialization is
  byte-identical; INNER fields are plain types (`bool`, `int`) so
  `"is_office": false` and `"is_virtual": false` serialize normally (review
  nit N1 — do NOT scatter omitempty scalars on `Reply`).
- `venue` omitted when unmatched; `origin`/`venue` omitted on virtual
  estimates; `source` ∈ `matrix` | `default` | `virtual`.
- `is_office: true` when the location matches `office_aliases` (briefing may
  then suppress offsite framing but still render leave-by from a residence).
- `is_virtual: true` when the location matches `virtual_aliases` (briefing
  suppresses leave-by lines entirely; minutes are 0).
- Errors: `invalid_request` (shape), `estimate_unavailable` (knowledge not
  loaded, or origin rule returned `no_origin`).

### 2.4 Handling (exact — placement is normative, review should-fix S3)

In `handleEvent`, the estimate branch sits IMMEDIATELY after the ack and
`DecodeRequest`, and BEFORE `validateRequest`, `canonicalKey`, the
reply-cache `Get`, and the inflight/enqueue logic:

1. Ack (unchanged), `DecodeRequest` (unchanged).
2. If `req.ProhibitedField() != ""` ⇒ `refusedReply` (same shape as today's
   refusals), return.
3. If `action == "travel-estimate"` ⇒ run §2.2 validation, compute the
   estimate, `sendReply` synchronously, return. Never enters `validateRequest`
   (whose action switch would reject the action as "unsupported scheduler
   action"), never touches `a.cache`, never enqueues a job, never opens an
   upstream session.
4. Otherwise: the existing flow, unchanged. (Note: the existing
   prohibited-field check inside `validateRequest` still runs for scheduler.v1
   actions; step 2 makes the check action-independent without changing any
   existing reply.)

This ordering also closes the request-id-reuse hazard: a caller reusing a
`request_id` from an earlier `schedule-request` gets a fresh `estimated`
reply, never a replayed `booked` one (the cache is keyed
`canonicalKey(evt.From, req.RequestID)` with no action component — verified
in `cache.go`). Test required (§9).

New constants in `contracts.go`:

```go
CapabilityEstimate       = "travel-estimate"
StatusEstimated          = "estimated"
ErrorEstimateUnavailable = "estimate_unavailable"
```

`Reply.Terminal()` is NOT modified (review should-fix S3): `Terminal()` is
consulted only by `runJob` and `replyCache.Put`, neither of which the estimate
path touches; adding `StatusEstimated` there would create an
accidental-caching hazard for zero benefit.

---

## 3. Write-agent extension: the travel event class

New sanctioned class in `internal/outlookcalendarwrite`, third alongside guard
blocks and working holds. **Guard-block and working-hold behavior, markers,
caps, and tests are unchanged** (two narrow, additive exceptions: the
`replayed` response field in §3.7 and the optional cap decrement in §3.6, both
of which must leave every existing test passing) — every other rule below
applies only to the travel path.

### 3.1 Constants (`contracts.go`)

```go
TravelSummaryPrefix = "Travel: "
TravelClassMarker   = "hold_class=travel-block"
```

`ReservedHoldMarkerKeys` is unchanged (`managed_by=`, `owner_agent=`,
`hold_class=`).

**Threat note (review blocker B2 — the draft-v1 claim that reserved keys
"already block spoofing" was FALSE and is retracted):**
`containsReservedHoldMarkerKey` is called only in `BuildHoldInsert` and
`BuildHoldPatch`. The GUARD paths (`BuildInsert`/`BuildPatch`) perform no
reserved-key scan and have no sender allowlist, so ANY bus agent can today
plant a syntactically valid hold — or, once it exists, travel — marker block
(with an attacker-chosen `managed_by`) inside a guard event's description.
Marker presence alone therefore must NOT be trusted for class routing. The
travel class defends with two-factor classification (§3.2); the equivalent
latent hole for the HOLD class (forged hold marker in a guard event +
attacker-chosen `managed_by` naming a hold requester) predates this spec, is
out of scope to fix here (existing tests must not change), and is recorded in
§12 as a hardening item. A `travel_for=` line is NOT reserved and is permitted
in inbound descriptions (verified against `containsReservedHoldMarkerKey`).

### 3.2 Classification (extends §classification-first of the holds spec)

- An insert is a **travel block** iff its summary starts with
  `TravelSummaryPrefix` (post-trim). Checked BEFORE the hold check in
  `handleInsert` (the prefixes are disjoint; order is for clarity).
- A patch targets a travel block iff **BOTH** hold: the EXISTING event's
  description carries the exact travel marker block (§3.3) **AND** the
  existing event's summary starts with `TravelSummaryPrefix` (or
  `CancelledPrefix + TravelSummaryPrefix`). The conjunction is unforgeable:
  guard validation (`validateFinalEvent`) only ever accepts guard summaries,
  hold validation only accepts `"Joel + "` summaries, and `BuildTravelInsert`
  rejects inbound reserved-marker keys — so no path exists by which a
  non-travel event acquires both the marker AND the prefix (review blocker
  B2). A forged travel marker inside a guard-summary event does NOT route to
  the travel path.
- `handlePatch` order: travel (marker AND summary prefix) → travel path;
  hold marker → hold path; else guard path with the existing hold-requester
  `not_owned` gate extended to also cover travel requesters (a travel
  requester may never patch a guard block).

### 3.3 Marker block (mirrors hold markers; `hold_markers.go`)

The agent appends, exactly:

```text
managed_by=<authenticated bus sender id (evt.From)>
owner_agent=ucla-tdg-outlook-calendar-write-agent
hold_class=travel-block
```

`travelMarkerRequester(description)` matches the anchored three-line block
exactly like `holdMarkerRequester` (line-trimmed, Outlook round-trip safe).
A travel patch requires existing `managed_by` == requesting sender.

### 3.4 Validation — `BuildTravelInsert(event, requester)` / `BuildTravelPatch(existing, patch, requester)` (`validation.go`)

- Summary must start with `TravelSummaryPrefix` (post-trim).
- Timed only; all-day forms rejected; start/end on the same local date.
- Duration ≥ 10 min, ≤ 2 h.
- Start must be ≥ `now − 5 minutes` (grace: a travel block for an imminent
  meeting may be inserted while the clock ticks past its start) and within
  the next 30 days.
- Local start ≥ **06:00** AND local end ≤ **22:00** (review nit N4: the
  ceiling is enforced on the END at the writer, not just scheduler-side, so
  the writer remains the safety boundary; this implies local start ≤ 21:50
  given the 10-min minimum). Wider than the hold window on purpose: travel can
  precede 07:00 meetings and follow 21:00 ones.
- `time_zone` absent or exactly `America/Los_Angeles`, offset-checked at the
  instant — identical mechanics to `validateHoldDateTime`; the startup
  host-timezone gate (`HoldTimeZoneOK`) applies to travel writes identically.
- `show_as` must normalize to `busy`.
- Description: must NOT contain reserved marker keys (inbound); after marker
  strip it must be non-empty (the scheduler always writes the audit lines,
  §6.2). `travel_for=`/`parent_start=`/`Destination:`/`Parking:` lines are
  ordinary description text to the writer.
- Prohibited fields (attendees/recurrence/conference) rejected as today.
- Cancel state machine: identical to holds — a patch whose ONLY changes are
  summary gaining `[CANCELLED] ` and `show_as: free` cancels; cancelled travel
  blocks refuse all further patches; double-cancel is idempotent success.
- Non-cancel patches re-validate all insert rules + the PowerShell conflict
  check (§3.5) against the merged event. (Go-side validation has no busy-set
  access; the conflict check exists ONLY in PowerShell — §3.5 is therefore
  normative for both inserts and patches, review should-fix S4.)

### 3.5 Conflict check + PowerShell defense (`service.go`)

- Add `Test-TravelMarker($body)` to the embedded PowerShell (anchored
  three-line match, same shape as `Test-HoldMarker` with
  `hold_class=travel-block`).
- **Insert branch:** run `Assert-NoBusyConflict` when the payload description
  carries the travel marker and `show_as != "free"` (same trigger pattern as
  holds). Guard blocks remain the only conflict exemption.
- **Patch branch (review should-fix S4 — explicit, mirroring the existing
  hold patch trigger at the `Test-HoldMarker $payload.description` line):**
  when `Test-TravelMarker $payload.description` and
  `$payload.show_as -ne "free"`, run `Assert-NoBusyConflict` excluding the
  item's own EntryID. Without this, a watcher reattach computed from a stale
  events-list snapshot could move a block onto a meeting created between scan
  and patch.
- Patch defense (last line): accept the exact travel marker block **AND**
  a `Travel: `-prefixed (or `[CANCELLED] Travel: `-prefixed) Subject as a
  third acceptable ownership — the same two-factor rule as §3.2, mirrored in
  PowerShell — alongside guard and hold blocks. The guard check text is
  retained verbatim.

### 3.6 Authorization, caps, idempotency (`agent.go`, `hold_state.go`)

- New env `OUTLOOK_CALENDAR_WRITE_TRAVEL_REQUESTERS` (comma-separated bus agent
  ids; parse with the existing `ParseHoldRequesters`). Unset/empty ⇒ travel
  requests refused `not_allowlisted` (fail closed). Deploy-time value:
  `ucla-tdg-scheduler-agent`. Wire through
  `cmd/outlook-calendar-write-agent/main.go` and document in
  `docs/OUTLOOK_CALENDAR_WRITE_AGENT.md` (+ the install script env, same as
  hold requesters were done).
- **Separate caps — travel must NOT consume the 2/day hold quota** (design doc
  phase 3 requirement). New limiter instance (same `holdRateLimiter` type) with
  constants `maxTravelInsertsPerRequesterPerDate = 12`,
  `maxTravelInsertsPerDate = 16`, keyed by the block's event local date. Live
  (non-dry-run) successful inserts only.
- **Shared-budget math (review should-fix S2):** booking-path inserts (§6),
  watcher inserts (§7.3), and watcher move-churn all arrive from the same
  sender (`ucla-tdg-scheduler-agent`) and draw on ONE per-requester budget.
  12 = 4 booked offsite holds × 2 blocks (8) + headroom of 4 for the watcher
  protecting Joel's self-booked offsite meetings and for churn.
- **Budget release on cancel (review should-fix S2):** the limiter gains a
  `Release(requester, eventDate)` (decrement, floor 0) called on every LIVE
  successful travel cancel-patch, keyed by the cancelling sender + the block's
  event local date. Compensation cancels (§6.3) and watcher orphan cancels
  (§7.5) thus return budget, so failed bookings cannot permanently starve
  travel protection for a date. Apply the same release to the HOLD limiter on
  successful hold cancel-patches ONLY IF every existing hold test stays green
  unmodified; otherwise restrict release to the travel limiter and document
  the hold-quota burn under compensation as accepted v1 behavior. Add the §9
  test either way.
- Idempotency: reuse the existing `holdResponseCache` (it is keyed by
  sender + `meta.request_id`; key-space collision with holds is impossible
  because the scheduler uses distinct request-id prefixes, and even identical
  keys would only ever replay the caller's own response). 24 h TTL unchanged.
- Error codes reused verbatim: `not_allowlisted`, `rate_limited`, `conflict`,
  `tz_mismatch`; travel validation failures use a new code `invalid_travel`
  (parallel to `invalid_hold`).
- Metrics: `event_travel_insert`, `event_travel_insert_dry_run`,
  `event_travel_insert_idempotent`, `event_travel_patch`,
  `event_travel_patch_dry_run`, `event_travel_cancel_idempotent`,
  `event_travel_budget_released`.

### 3.7 Replay visibility (review should-fix S1)

`MutationResponse` gains one additive field:
`Replayed bool \`json:"replayed,omitempty"\``. It is set `true` on every
`holdResponseCache` HIT (hold and travel classes alike) before the cached
response is sent; cached entries themselves are stored without it. The cached
`StoredEvent` reflects the event AS INSERTED, not its current state — a
replayed "success" says nothing about whether the event has since been
cancelled. Callers that compose multi-step writes (§6) MUST verify replayed
prerequisites. The field is invisible (omitted) on all fresh responses, so
existing decode-based test assertions are unaffected; if any existing test
asserts exact replay JSON, extend that test additively rather than weakening
the field.

---

## 4. scheduler.v1 contract extension: `location` on `schedule-request`

### 4.1 Request (additive)

```json
{
  "action": "schedule-request",
  "request_id": "...",
  "purpose": "Pick up Marc after procedure",
  "requester_label": "Fable",
  "duration_minutes": 60,
  "window": "tomorrow morning",
  "agenda": "...",
  "location": "200 Medical Plaza"
}
```

- `location` optional, free text, ≤ 200 runes after trim (violation ⇒
  `invalid_request`, "location must be 200 characters or fewer").
- Absent/empty `location` ⇒ behavior identical to today, byte-for-byte
  (the `travel` reply field is omitted; see §11).
- `location` matching `office_aliases` OR `virtual_aliases` ⇒ treated as
  onsite/no-travel: NO travel blocks, reply carries no `travel` field.
  (Estimates TO the office are the briefing's business via `travel-estimate`,
  not the booking path's; virtual meetings need no travel by definition.)
- `location` present but travel knowledge failed to load, or the estimate
  returns `no_origin` ⇒ `error_code=estimate_unavailable`, ZERO writes
  (review should-fix S5; it must not silently book without travel
  protection).
- `location` on `schedule-move`/`schedule-cancel`: accepted and ignored in v1
  (documented; the watcher reconciles around moved offsite meetings, and
  scheduler holds are not offsite-detectable — see §7.6 and §12).

### 4.2 Reply (additive)

`booked` replies for offsite requests gain:

```json
"travel": {
  "minutes": 15,
  "origin_id": "ucla-tdg-office",
  "estimate_source": "matrix",
  "before": {"event_id": "...", "start": "...", "end": "..."},
  "after":  {"event_id": "...", "start": "...", "end": "..."},
  "notes": []
}
```

- **`before` and `after` are ALWAYS both present on a booked offsite reply**
  (review should-fix S5: draft v1's "independently omitted" language
  contradicted §5/§6, which require both intervals free and insert both
  blocks or fail the whole request — there is no booking-time shrink/skip
  path in v1). `notes` is reserved for future use and always `[]` in v1.
- New `Reply` field `Travel *TravelBooking \`json:"travel,omitempty"\`` — all
  existing replies serialize identically when nil.

---

## 5. Travel-aware slot selection (offsite `schedule-request` only)

When a `schedule-request` carries a non-office, non-virtual `location`:

1. Compute the estimate (§1.5) using the CANDIDATE slot start as `eventStart`
   (origin can differ across candidates spanning the §1.5 boundaries —
   recompute per candidate; determinism is what matters). Estimate error
   (`no_origin`) ⇒ reply `estimate_unavailable`, zero writes (§4.1).
   `blockMinutes` per §1.5.
2. A candidate slot `[s, e)` is additionally required to satisfy
   (new helper `candidateAllowedWithTravel` in `policy.go`; existing
   `candidateAllowed` is NOT modified):
   - `[s − blockMinutes, s)` and `[e, e + blockMinutes)` do not overlap any
     busy interval (same busy set; guard all-day blocks and transparent events
     already excluded read-side by `busyIntervals`).
   - The before block must start ≥ `now + 5 min` and ≥ 06:00 local; the after
     block must end ≤ 22:00 local; both on the same local date as the hold.
   - The 10-minute post-busy buffer rule applies to the BEFORE BLOCK's start
     (not the hold's start — the travel block is now the thing that follows a
     meeting).
   - Lunch rule: the HOLD still may not overlap [12:00, 13:00); travel blocks
     ARE exempt from the lunch rule (driving through lunch is allowed; a
     candidate whose travel covers lunch is fine).
3. Infeasibility and nearest-alternative logic unchanged, but run with the
   travel-aware predicate so the alternative is honest.

---

## 6. Atomic travel-block booking (offsite `schedule-request`)

"Atomic" here means: **either the hold and both travel blocks exist, or none
do** — implemented as ordered inserts with compensating cancels, since Outlook
offers no transactions. Be honest in code comments: a crash between steps can
strand events until the watcher tick reconciles or a cancel-patch compensation
runs on retry; §6.4 spells out the exact retry semantics.

### 6.1 Order and steps (within the existing `executeRequest` retry loop)

1. Insert the HOLD (existing path, conflict-retry ≤ 3 unchanged). The slot
   came from the travel-aware predicate (§5).
   **Replay guard (review should-fix S1):** if the writer response carries
   `replayed: true` (§3.7), the hold may have been inserted by an earlier
   attempt and subsequently compensation-cancelled. Verify it: one
   `events-list` call for the hold's local date; the hold exists iff an event
   with the expected summary, start, and end is present WITHOUT the
   `[CANCELLED] ` prefix. Missing/cancelled ⇒ re-insert once under the
   alternate key `"sched-" + keyHash + "-insert-r2"`; if THAT response is also
   replayed-and-missing, fail with `travel_booking_failed` (no further key
   generations). Fresh (non-replayed) responses skip the verification.
2. Insert the BEFORE travel block: `[holdStart − blockMinutes, holdStart)`.
3. Insert the AFTER travel block: `[holdEnd, holdEnd + blockMinutes)`.
4. Reply `booked` with the `travel` field.

Each step uses a distinct writer idempotency key:
`meta.request_id = "sched-" + keyHash + "-insert"` (hold, unchanged — passed
as the existing `writeRequest` step name),
`"sched-" + keyHash + "-travel-before"`, `"sched-" + keyHash + "-travel-after"`.

### 6.2 Travel block event content (insert payload)

- Summary (exact grammar, parsed back by the watcher — §7.4):
  - before: `Travel: <dest> (for <HH:MM>)`
  - after: `Travel: <dest> (return <HH:MM>)`
  - `<HH:MM>` = the PARENT boundary time, zero-padded 24 h: before uses the
    parent start; return uses the parent end.
  - `<dest>` = venue `name` if matched, else the raw location normalized
    (whitespace-collapsed) and truncated to 60 runes, else `offsite`.
    `<dest>` must not be re-derived at parse time; the regex tolerates any
    text (§7.4).
- Description (agenda):

```text
travel_for=<parent event id>
parent_start=<parent start RFC3339>
Destination: <visible travel endpoint>
Parking: <venue parking notes, or "unknown">
```

  `Destination:` matches the visible Outlook `Location` field for the travel
  block: the meeting venue on before-event legs, and the return/next target on
  return legs. These lines are AUDIT ONLY. `events-list` does not return descriptions
  (verified: `rowsToEvents` in `internal/outlookcalendar/extractor.go` never
  populates `Description`), so nothing may ever depend on reading them back.
  The writer permits them (§3.1/§3.4).
- `show_as: busy`; times in `America/Los_Angeles` (writer rules §3.4).

### 6.3 Failure handling (exact)

- Step 2 or 3 fails (`conflict`, `rate_limited`, any writer error, or upstream
  timeout after the standard retry): COMPENSATE — cancel-patch every event
  inserted so far in this request (after block if any, before block if any,
  then the hold), each with its own idempotency key
  (`...-travel-before-cancel` etc.), then reply
  `status=error, error_code=travel_booking_failed`, message naming the failed
  step and the writer's code. New constant
  `ErrorTravelBooking = "travel_booking_failed"`. Successful compensation
  cancels release writer budget (§3.6).
- Compensation failures: log + metric `schedule_travel_compensation_failed`;
  reply still `travel_booking_failed` with a note that orphaned events may
  remain (they are `[CANCELLED]`-prefixed where the cancel succeeded; the
  remainder are visible on the calendar and harmless-but-ugly — Joel-visible
  by design).
- A `conflict` on a travel insert does NOT loop back into hold re-selection in
  v1 (keeps the retry state machine simple); the whole request fails with
  `travel_booking_failed` and the caller may retry with a fresh `request_id`.
  Documented limitation.

### 6.4 Idempotent replay (honest semantics — review should-fix S1)

The scheduler's reply cache (sender + `request_id`, 24 h) covers the composite
result: duplicates of a COMPLETED request replay the cached terminal reply.
The hazardous sequence is: hold inserted → travel step fails → compensation
cancels the hold → scheduler crashes BEFORE caching its terminal reply (the
reply cache is in-memory) → caller retries the same `request_id`. Without the
step-1 replay guard, the writer would replay the stale hold-insert success
(the event is actually `[CANCELLED]`/free), steps 2–3 would then succeed
against the now-free slot, and the scheduler would reply `booked` pointing at
a cancelled hold — two travel blocks bracketing nothing. The §6.1 replay
guard (verify-on-replay + one `-insert-r2` re-insert) closes this; the residue
is bounded: at most one extra cancelled hold visible on the calendar.

---

## 7. Reconciliation watcher

A background loop inside `ucla-tdg-scheduler-agent` (new file
`internal/scheduler/watcher.go`) that makes Joel's OWN offsite meetings get
travel protection, and keeps travel blocks attached when parents move.

**Deployment dependency (normative — review should-fix S6):** watcher
correctness assumes the read agent (`ucla-tdg-outlook-calendar-agent`) runs
with `OUTLOOK_CALENDAR_INCLUDE_PRIVATE_DETAILS=true` (the code default,
matching Joel-equivalent calendar visibility for Joel's own trusted fleet).
When a surface explicitly opts into redaction, private (sensitivity 2) events
are masked to subject "Private appointment" with a blanked location: private
offsite meetings are then only detectable via the yellow category, and private
travel blocks cannot match the §7.4 grammar (they degrade to foreign busy
events — adjacency still prevents double-creation, but reattach/cancel never
fires for them). Defense-in-depth: events whose summary is exactly
`"Private appointment"` are treated as busy-but-untouchable — they count for
adjacency and shrinking but the watcher never creates travel for them and
never mutates them (skip metric `watch_travel_skipped`, logged reason
`masked_private`).

### 7.1 Schedule and scope

- Tick every `SCHEDULER_WATCH_INTERVAL_MIN` minutes (default 15; `0` OR any
  negative value ⇒ watcher disabled entirely — `withDefaults` clamps, review
  nit N3). First tick after a random 0–60 s jitter post-startup.
- Scan horizon: today + `SCHEDULER_WATCH_HORIZON_DAYS` days (default `3`, so
  4 local dates), via one `events-list` per date with `MaxResults: 200`. The
  read agent clamps to 200 and returns NO truncation flag, so truncation is detected as
  `len(events) == requested MaxResults` ⇒ logged warning + metric
  `watch_events_truncated` (review nit N3); the tick still processes what it
  got but performs NO cancels that tick (missing events must not look like
  orphaned parents).
- Each tick derives one upstream session through the existing
  `newUpstreamSession`/`requestUpstream` machinery with session key
  `"watch|" + tickStart.Format(time.RFC3339)`, yielding conversation id
  `"sched-" + idempotencyHash(key)` (review nit N5: the existing helper
  hardcodes the `"sched-"` prefix and response routing only requires that
  prefix; do NOT invent a `sched-w...` literal format).
- Watcher mutations never run concurrently with each other (single goroutine);
  they MAY interleave with schedule-request jobs — the writer's conflict check
  is the arbiter, as today.
- Started from `Agent.Run` alongside the worker pool; respects ctx cancel.
- Write plumbing (review nit N5): the existing `writeRequest` hardcodes
  `meta.request_id = "sched-" + keyHash + "-" + step`; the watcher gets its
  own thin helper, e.g.
  `writeTravelMutation(ctx, session, payload, metaRequestID string)`, that
  passes an EXPLICIT meta key (the `schedw-*` keys below) through
  `requestUpstream`.

### 7.2 Event classification per tick (exact skip list)

For each event in the scan, in order, SKIP (never create travel for) when:

1. all-day (`start.date` set) for travel creation/busy shrinking; all-day
   entries remain available as origin/return context,
2. `transparency == "transparent"` (case-insensitive),
3. guard block (summary `== "Meeting Quota Reached"` or prefix
   `"No more meetings"` — reuse `isGuardSummary`),
4. summary prefix `"[CANCELLED] "`,
5. summary prefix `"Travel: "` (never travel-for-travel),
6. summary exactly `"Meeting Buffer"` (placeholder; does not block creation
   of an explicit travel card with visible destination/location),
7. summary exactly `"Private appointment"` (masked-private sentinel, §7
   preamble),
8. event already ended (`end <= now`),
9. event starts beyond the writer's 30-day insert horizon (cannot happen
   within a 4-day scan; assert anyway).

Remaining events are **busy meetings**. A busy meeting is **offsite** iff:

- `categories` contains the yellow category — match rule: a category string
  `c` matches iff `EqualFold(c, cfg.OffsiteCategory)` OR
  `strings.Contains(lower(c), "yellow")`. `cfg.OffsiteCategory` from env
  `SCHEDULER_OFFSITE_CATEGORY`, default `"Yellow category"` (Outlook's default
  display name — confirm live, §12; the `yellow` substring fallback covers
  renames like "Yellow - Offsite"); **OR**
- `location` is non-empty AND does not match `office_aliases` AND does not
  match `virtual_aliases` (§1.4 — review blocker B3: without the virtual
  exclusion, every Teams/Zoom/Webex meeting would be bracketed with 30-minute
  busy travel blocks).

Event-specific durable location rules may infer a physical location before the
plain `location` test. Current rule: a visible Wednesday David K/Kronemyer
meeting with a blank location is treated as face-to-face at
`805 Tiverton Ave, Los Angeles, CA 90024`. If the event has a virtual location
only, it remains virtual unless the yellow category or text such as
`face to face`, `f2f`, or `in person` confirms that the virtual string is not
the actual endpoint. An explicit physical location still wins. Masked
`Private appointment` rows are never inferred.

**Precedence when signals disagree:** the yellow category WINS over the
virtual-location exclusion — a yellow-categorized Teams meeting IS offsite
(Joel's explicit categorization outranks Outlook's auto-populated location
string; he may be joining from somewhere he must drive to). Yellow with no
usable location falls through to the default estimate as before. Confirm with
Joel (§12); test both directions (§9).

Scheduler-created working holds (`"Joel + "` prefix) have no location and no
category, so they are never offsite here — their travel blocks come from §6.

### 7.3 Missing-block detection and creation

For each offsite meeting `M` (skip if `M.start <= now` for the before side):

- **Before block exists** iff any `"Travel: "`-prefixed, non-cancelled, busy,
  timed event `T` has `T.end ∈ [M.start − 20 min, M.start + 5 min]`.
- **After block exists** iff any such `T` has
  `T.start ∈ [M.end − 5 min, M.end + 20 min]`.
- Adjacency is OWNERSHIP-BLIND: a human-created or other-agent travel event in
  the slot counts as existing (we must not double-book; we also cannot read
  ownership from events-list).

When a side is missing, verify the leg context before computing the estimate.
For the before side, a known physical prior anchor can supply the departure
origin; if an all-day office-closure note says the office closed before the
computed departure time, the active residence supplies the origin. For the
after side, a known later physical anchor supplies the return target; otherwise
the watcher reuses the verified departure origin for the same meeting. This
prevents a plausible but wrong "go home" return block during the workday when
the outbound leg clearly left from the office. Then compute the estimate (§1.5;
yellow-with-no-location or yellow-with-virtual-location ⇒ no venue match ⇒
default 30 min, dest `offsite`; estimate error `no_origin` ⇒ skip this meeting
this tick, `watch_travel_skipped`, logged reason `no_origin` — review
should-fix S5), then the desired interval, then SHRINK against neighbors:

- before: `I = [M.start − blockMinutes, M.start)`. For every busy meeting or
  travel block `B` (per §7.2 list 1–6 skips; `B ≠ M`) overlapping `I`, raise
  `I.start` to `B.end` if `B.end < M.start`, else the side is blocked — skip
  with metric `watch_travel_skipped` (label-free counter; the reason —
  `adjacent_busy` — goes in the log line only, review nit N5: the telemetry
  registry has no label support).
- after: mirror image (`lower I.end to B.start`).
- Clamp `I.start ≥ now + 1 min` (before side), clamp to 06:00–22:00 local,
  same local date as `M`'s side boundary.
- Surviving length: round DOWN to a 5-min multiple; if < 10 min ⇒ skip
  (logged reason `too_short`).

If shrink leaves a surviving before-travel interval that is shorter than the
required estimate because it was compressed by a lower-priority meeting, the
watcher must not treat the shortened block as fully successful. It records a
read/propose communication recommendation with metric
`watch_travel_communication_proposed` and a log line naming the blocker, the
required leave time, and the current block start. This is intentionally NOT an
email write: draft creation/sending remains a separate explicit user-approved
action. Current deterministic priority hints are conservative and
human-readable: Josh/Jeanson or boss/manager obligations outrank ordinary
1:1s, and Carol one-on-ones are considered lower priority for this rule. The
proposal body should say the earlier meeting can continue only until the
required leave time (for example, Carol 11:00-11:45 compressed a 30-minute
noon West Medical trip, so the proposed note says Joel can do the first
30 minutes and must drop at 11:30 for Josh).

Insert via the travel class (§3) with summary/description per §6.2 and writer
idempotency key (review should-fix S7 — the key MUST embed the desired
interval, not just the parent start, or the writer's 24 h response cache
replays a stale insert when a parent's END moves while its start is
unchanged):

```
meta.request_id = "schedw-" + idempotencyHash(
    M.ID + "|" + M.start + "|" + M.end + "|" + side + "|" + I.start + "|" + I.end)
```

Deterministic across restarts for the same calendar state (so
`TestWatcherIdempotentAcrossRestart` holds), distinct whenever the desired
block differs. A moved parent (start OR end) naturally produces a new key; the
stale block is handled by §7.5.

**Human deletion policy (review should-fix S6):** the watcher keeps an
in-memory set of keys whose insert reported success (fresh or `replayed`,
§3.7) THIS process lifetime. If a key in that set is needed again on a later
tick (the side reads as missing although we "successfully" inserted it), the
block was deleted out from under us — almost always by Joel, the one actor
who CAN delete. Do not fight him: denylist that (parent id, parent start,
side) for the process lifetime, metric `watch_travel_human_removed`, log
loudly. Residual blind spots, documented honestly: (a) within the writer's
24 h response-cache TTL a re-send of the same key replays the cached success
without writing — the in-memory set turns that into detection on the NEXT
tick rather than silent failure; (b) after a scheduler restart the set is
empty, so one recreate attempt may fire before detection re-arms. Both are
accepted v1 behavior.

### 7.4 Parentage detection — summary grammar + adjacency (and its limits)

The watcher recognizes ITS OWN travel blocks by the summary grammar:

```regexp
^Travel: (.+) \((for|return) ([01]\d|2[0-3]):([0-5]\d)\)$
```

Group 1 = dest (opaque, never re-validated), group 2 = side, groups 3–4 =
parent local boundary `HH:MM` (before=start, return=end). Legacy return blocks
that used the parent start are still recognized.

**Honest detection limits (write these into the doc comment verbatim):**

- `events-list` does not return event descriptions, so `travel_for=<id>` audit
  lines are unreadable; the parent linkage is NOT recoverable from the body.
  Summary grammar + same-date time adjacency is the ONLY linkage.
- The grammar carries only a parent boundary `HH:MM` — two candidate meetings
  with the same relevant boundary minute on the same date are ambiguous; the
  watcher must then do nothing (no move, no cancel) and emit
  `watch_travel_ambiguous`.
- A parent moved across midnight, or whose travel block was manually retitled,
  is unlinkable; its old block will look orphaned (§7.5) and its new slot will
  get fresh blocks (§7.3). Net effect is correct but transits through a
  cancel+insert rather than a move.
- Human-created events that happen to match the grammar are
  indistinguishable from watcher blocks; mutation attempts on them fail
  writer-side (`not_owned`/no marker) and are logged + skipped — the writer's
  ownership markers, not the watcher's parsing, are the safety boundary.

### 7.5 Moved/cancelled parents (per grammar-matching travel block `T` in scan)

All mutations in this section address the target event by its
`extended_properties.private.entry_id` (§0); a grammar-matching block WITHOUT
an `entry_id` is logged + skipped with `watch_travel_no_entry_id` (it still
counts for adjacency).

Let `hh:mm` = the grammar time, `d` = `T`'s local date, side = for/return.

1. **Still attached?** If an offsite meeting on `d` with the relevant boundary
   at `hh:mm` (before=start, return=end; legacy return=start also accepted)
   exists with the side's adjacency satisfied, recompute the leg's summary,
   visible `Location`, and audit description. If the attached block is stale
   (for example blank visible location from an older scheduler build, or a
   legacy return summary using the parent start), issue an `event-patch` for
   those fields only, preserving the live block times. Otherwise nothing to do.
   Idempotency key
   `"schedw-repair-" + idempotencyHash(T.ID + "|" + M.ID + "|" + M.start + "|" + M.end + "|" + side + "|" + oldSummary + "|" + oldLocation + "|" + newSummary + "|" + newLocation)`.
   Successful repairs increment `watch_travel_repaired`.
2. **Anchored to anything?** If ANY busy meeting (per §7.2 skips 1–6, not just
   offsite — this protects travel blocks around scheduler-booked offsite
   HOLDS, which carry no location/category) satisfies the side's adjacency to
   `T` ⇒ leave `T` alone. (A meeting moved into the slot adopts the block;
   imperfect but safe.)
3. **Reattach (moved parent)?** If exactly ONE offsite meeting on `d` lacks
   the side's block (per §7.3 existence test) ⇒ treat as the moved parent:
   `event-patch` `T` (by entry_id) to the recomputed interval and new-grammar
   summary for that meeting (re-running §7.3's shrink rules; if the recomputed
   side must be skipped, fall through to cancel). The writer re-runs the
   conflict check on this patch (§3.5). Idempotency key
   `"schedw-move-" + idempotencyHash(T.ID + "|" + M.ID + "|" + M.start + "|" + M.end + "|" + side + "|" + I.start + "|" + I.end)`.
   Zero candidates ⇒ step 4. Multiple ⇒ do nothing + `watch_travel_ambiguous`.
4. **Orphan ⇒ cancel.** Cancel-patch `T` (`[CANCELLED] ` + `show_as: free`),
   key `"schedw-cancel-" + idempotencyHash(T.ID + "|" + T.start)`.
   `not_owned`/marker refusals from the writer are expected for non-watcher
   events: log + metric `watch_travel_not_owned`, skip permanently for this
   process lifetime (in-memory denylist by event id). Successful cancels
   release writer budget (§3.6).

### 7.6 Interaction with scheduler-booked holds

Travel blocks booked by §6 use the same grammar, so the watcher sees them as
its own. Because their parent HOLD is busy-but-not-offsite, step 7.5(2)
anchors them (adjacency to ANY busy meeting) — the watcher will NOT cancel
them. If `schedule-move` later moves the hold, the old blocks lose their
anchor and are cancelled (7.5(4)); the hold's new slot gets NO new blocks
(holds are not offsite-detectable). This asymmetry is accepted in v1 and
documented in the scheduler spec's deviations list (§8 Docs updates); fixing
it requires a location field on write-agent events (§12).

### 7.7 Rate limiting and idempotency (watcher-side)

- Hard cap: ≤ 6 mutations (inserts + patches + cancels) per tick; overflow
  waits for the next tick (`watch_mutations_deferred` metric).
- Per desired-key attempt memory: one attempt per tick; after 3 consecutive
  writer failures for the same key, stop attempting it (process-lifetime
  backoff, metric `watch_travel_backoff`). Writer caps (§3.6) are the
  global backstop.
- All state is in-memory; restart safety = writer idempotency cache + the
  deterministic keys above. The human-deletion policy and its restart blind
  spot are in §7.3.
- Dry-run alignment: when the write agent is in dry-run, watcher mutations
  return `would_write` responses; the watcher treats them as success for
  backoff purposes (it will re-attempt next tick by design — dry-run is a
  staging mode, noisy ticks are acceptable and observable).

### 7.8 Watcher NEVER list (restating, normative)

The watcher must never create, move, or cancel travel around: all-day events,
transparent events, guard blocks, `[CANCELLED] ` events, other `Travel: `
events, masked-private sentinel events, virtual-location meetings (absent the
yellow category), events that already ended, or anything when travel knowledge
failed to load. It must never delete anything (no delete capability exists),
never touch guard blocks (writer enforces independently), never mutate an
event without an `entry_id`, never cancel during a truncated tick, never
recreate a block Joel deleted (within the §7.3 detection limits), and never
exceed its per-tick mutation cap.

---

## 8. Files to touch (exhaustive)

Repo A — `/home/joelkehle/Projects/jk/jk-email-agents`, branch
`location-awareness`:

| File | Change |
|---|---|
| `internal/outlookcalendar/extractor.go` | §0 — additive `entry_id` in extended properties |
| `internal/outlookcalendar/extractor_test.go` | §0 test (additive) |
| `data/locations.json` | add `id` to each residence + work (additive) |
| `data/venues.json` | NEW — seed per §1.3 (incl. `virtual_aliases`) |
| `internal/travel/locations.go` | NEW — origins loader + residence-by-date |
| `internal/travel/venues.go` | NEW — venues loader + venue/office/virtual matching |
| `internal/travel/estimate.go` | NEW — `Knowledge`, origin rule (departure-time), `Estimate` |
| `internal/travel/{locations,venues,estimate}_test.go` | NEW — §9 tests |
| `internal/scheduler/contracts.go` | Runtime wrapper around `~/Projects/shared/calendar-agents/pkg/schedulercontract`; `Config` keeps runtime fields (`LocationsPath`, `VenuesPath`, `OffsiteCategory`, `WatchIntervalMin`, `WatchHorizonDays`) and `withDefaults` clamping ≤ 0 ⇒ disabled. `Reply.Terminal()` remains unchanged in the shared package |
| `internal/scheduler/agent.go` | register `travel-estimate`; estimate branch placed per §2.4 (before validateRequest/cache); validate `location` length; load `travel.Knowledge` in `NewAgent` (degradation per §1.6); start watcher in `Run` |
| `internal/scheduler/estimate.go` | NEW — travel-estimate request decode/validate/reply |
| `internal/scheduler/execution.go` | travel-aware `executeRequest` branch (§5–6): expanded slot predicate, ordered inserts, replay guard, compensation, `writeTravelInsert`/`writeTravelCancel` helpers, watcher write helper with explicit meta key |
| `internal/scheduler/policy.go` | ADD `candidateAllowedWithTravel` (existing functions untouched) |
| `internal/scheduler/watcher.go` | NEW — §7 |
| `internal/scheduler/{agent,execution,policy,watcher,estimate}_test.go` | §9 tests (existing test files extended, never edited destructively) |
| `internal/outlookcalendarwrite/contracts.go` | aliases shared event-class constants from `~/Projects/shared/calendar-agents/pkg/calendarcontract` and write-agent schema from `~/Projects/shared/calendar-agents/pkg/outlookwritecontract`; `MutationResponse.Replayed` (additive) |
| `internal/outlookcalendarwrite/hold_markers.go` | travel marker ensure/match/strip helpers |
| `internal/outlookcalendarwrite/validation.go` | `IsTravelInsert`, `IsTravelPatch` (marker AND summary prefix), `BuildTravelInsert`, `BuildTravelPatch`, `validateTravelFinalEvent` (incl. end ≤ 22:00) |
| `internal/outlookcalendarwrite/agent.go` | classification order (§3.2), travel allowlist (`OUTLOOK_CALENDAR_WRITE_TRAVEL_REQUESTERS`), travel caps + release-on-cancel, `invalid_travel` code, `Replayed` on cache hits, extend the hold-requester `not_owned` gate to travel requesters |
| `internal/outlookcalendarwrite/hold_state.go` | travel cap constants + second limiter instance + `Release` |
| `internal/outlookcalendarwrite/service.go` | PowerShell: `Test-TravelMarker`, conflict-check trigger for travel on BOTH insert and patch branches, patch defense accepts travel marker + Subject prefix |
| `internal/outlookcalendarwrite/travel_*_test.go` | NEW — §9 tests |
| `cmd/scheduler-agent/main.go` | env wiring: `SCHEDULER_LOCATIONS_PATH`, `SCHEDULER_VENUES_PATH`, `SCHEDULER_OFFSITE_CATEGORY`, `SCHEDULER_WATCH_INTERVAL_MIN` |
| `cmd/outlook-calendar-write-agent/main.go` | parse `OUTLOOK_CALENDAR_WRITE_TRAVEL_REQUESTERS` |
| `docs/SCHEDULER_TRAVEL_SPEC.md` | this spec |
| `docs/SCHEDULER_AGENT_SPEC.md` | add a one-line pointer to this spec under Out of scope/v2; append §7.6 asymmetry to Known deviations |
| `docs/OUTLOOK_CALENDAR_WRITE_AGENT.md` + holds spec | document the travel class, env, caps, `replayed` field |
| read-agent doc (events-list contract) | document `entry_id` + the `INCLUDE_PRIVATE_DETAILS=true` watcher dependency + live-smoke item (§0) |

Repo B (`ucla-tdg-project-agents`, branch `location-briefing`) consumes
`travel-estimate` over the bus — NO repo-B changes are in scope for this spec.

HARD RULES restated for the implementer: code + tests + docs only; never
deploy, ssh, send live bus traffic, restart services, or push; commit on
`location-awareness` only when `go build ./... && go test ./...` are green;
do not modify guard-block behavior or any existing passing test; the
write-agent live deploy steps (env values) are documentation only.

## 9. Test list (table-driven throughout)

### `internal/outlookcalendar` (§0)

| Test | Cases |
|---|---|
| `TestExtractorEntryID` | row with EntryID ⇒ raw `entry_id` in extended properties; row without ⇒ key absent; existing extractor tests untouched and green |

### `internal/travel`

| Test | Cases |
|---|---|
| `TestLoadLocations` | valid file; missing id; duplicate ids; overlapping residence windows; bad date; open-ended until; missing work id |
| `TestResidenceForDate` | inside first window; boundary days (from/until inclusive); gap between windows ⇒ none; after open-ended from |
| `TestLoadVenues` | valid seed; bad schema string; duplicate venue ids; alias < 4 runes; office alias < 6 runes; virtual alias < 4 runes; default minutes out of range; matrix value out of range; return matrix/buffer values out of range |
| `TestMatchVenue` | exact alias; alias substring inside longer location; case/whitespace/newline normalization; first-match-wins ordering; no match; empty location |
| `TestIsOffice` | each seeded alias; "Marc's office" does NOT match; empty |
| `TestIsVirtual` | "Microsoft Teams Meeting"; a Zoom join URL; "https://..." link; dial-in string; a real street address does NOT match; empty |
| `TestOriginRule` | departure-time evaluation: 09:00 meeting with 30-min default ⇒ departure 08:30 ⇒ residence; weekday 10:00 meeting ⇒ office; weekday 18:15 meeting (departure 17:45) ⇒ office; weekday 18:45 meeting (departure 18:15) ⇒ residence; Saturday ⇒ residence; destination=office ⇒ residence; no residence valid ⇒ fallback office; both absent ⇒ no_origin; date before all windows ⇒ no_origin |
| `TestEstimate` | matrix hit (drive+walk sum, source matrix); explicit origin and directed return matrix; venue hit/origin key missing ⇒ default drive + venue walk; unmatched ⇒ default 30 walk 0; virtual ⇒ zeros + is_virtual + source virtual; block-minutes rounding (min 10, round-up-to-5, cap 120) |

### `internal/scheduler` — travel-estimate

| Test | Cases |
|---|---|
| `TestTravelEstimateValidation` | missing request_id; missing/blank location; >200-rune location; non-RFC3339 event_start; prohibited field ⇒ refused |
| `TestTravelEstimateReply` | matrix venue ⇒ nested estimate incl. parking/origin and explicit `is_office: false`; unknown venue ⇒ default + no venue object; office location ⇒ is_office true; virtual location ⇒ is_virtual true, zero minutes, no origin/venue; knowledge unloaded ⇒ estimate_unavailable; eventStart outside all residence windows ⇒ estimate_unavailable; no idempotency cache entry stored |
| `TestEstimateBypassesReplyCache` | same sender + request_id: first schedule-request (booked, cached), then travel-estimate ⇒ fresh `estimated` reply, NOT the cached booking (§2.4 placement) |

### `internal/scheduler` — offsite booking

| Test | Cases |
|---|---|
| `TestRequestLocationValidation` | absent ⇒ unchanged path; office alias ⇒ no travel; virtual location ⇒ no travel; >200 runes ⇒ invalid_request |
| `TestCandidateAllowedWithTravel` | travel-before conflicts ⇒ candidate rejected; travel-after conflicts ⇒ rejected; travel over lunch allowed, hold over lunch still rejected; before-block 06:00 floor; after-block 22:00 ceiling; 10-min post-busy buffer applies to before-block start; now+5min lead on before block |
| `TestOffsiteBookingHappyPath` | fake writer: hold + before + after inserted with exact summaries/descriptions (grammar, travel_for/parent_start lines), distinct meta.request_ids, booked reply carries travel field with BOTH before and after |
| `TestOffsiteBookingCompensation` | before insert conflicts ⇒ hold cancel-patched, error travel_booking_failed; after insert fails ⇒ hold + before cancelled; compensation cancel fails ⇒ reply still terminal + metric |
| `TestOffsiteBookingReplayGuard` | step-1 response replayed + hold verified live ⇒ proceed; replayed + hold cancelled on calendar ⇒ re-insert under `-insert-r2`; second replay-and-missing ⇒ travel_booking_failed |
| `TestOffsiteBookingIdempotentReplay` | duplicate request_id replays composite reply, zero upstream calls |
| `TestKnowledgeUnloadedBooking` | location present + knowledge load failed ⇒ estimate_unavailable, no writes |
| `TestNoOriginBooking` | offsite request whose slot dates fall outside all residence windows ⇒ estimate_unavailable, no writes |

### `internal/scheduler` — watcher

| Test | Cases |
|---|---|
| `TestWatcherSkipList` | one case per skip: all-day, transparent, guard summary (both forms), [CANCELLED], Travel: prefix, "Private appointment" sentinel, already-ended; offsite-yellow and offsite-location positives; office-location negative; Teams-location meeting ⇒ NO blocks; Zoom-URL location ⇒ NO blocks; yellow-category Teams meeting ⇒ blocks (yellow wins); yellow match rules (exact configured name, contains-yellow fallback, case-insensitivity) |
| `TestWatcherCreatesMissingBlocks` | offsite meeting bare ⇒ both blocks inserted with grammar summaries + deterministic schedw- keys; existing adjacent Travel event (any owner) within tolerance ⇒ side not created; tolerance edges (−20/+5 before; −5/+20 after) |
| `TestWatcherRecalculatesSummerSolsticeTravel` | Thursday 2026-06-18 MCC event with incorrect invite address, all-day housesitting, early office closure, and a virtual 4pm anchor ⇒ before block 16:45–18:00 from active residence; return block 20:00–21:00 back to active residence; return summary uses parent end |
| `TestWatcherInfersDavidKTivertonLocation` | visible Wednesday David K/Kronemyer meeting with blank location ⇒ travel blocks use verified 805 Tiverton; plain virtual David K meeting stays virtual/no blocks; virtual plus yellow/face-to-face hint infers Tiverton; Thursday visible David K meeting does not infer |
| `TestWatcherShrinkAndSkip` | back-to-back meetings: after side trimmed to gap, rounded down to 5-min, skipped when <10; before side clamped to now+1min; 06:00/22:00 clamps; fully blocked side skipped with metric (reason in log only) |
| `TestWatcherYellowNoLocation` | default 30-min blocks, dest "offsite" |
| `TestWatcherNoOrigin` | offsite meeting on a date outside all residence windows ⇒ skipped, metric, no writes |
| `TestWatcherKeyEmbedsInterval` | parent END extended (start unchanged) ⇒ NEW meta.request_id, fresh insert actually issued; old block handled by reattach-or-cancel |
| `TestWatcherOrphanCancel` | grammar block with no adjacent busy meeting and no offsite candidate ⇒ cancel patch with schedw-cancel key, addressed by entry_id |
| `TestWatcherAnchorsToAnyBusy` | travel block adjacent to a scheduler HOLD (no location/category) ⇒ untouched (regression guard for §7.6) |
| `TestWatcherReattach` | parent moved same-date ⇒ exactly-one-candidate patch with recomputed interval + new grammar; multiple candidates ⇒ no action + ambiguous metric; recomputed side unfittable ⇒ cancel |
| `TestWatcherNoEntryID` | grammar block without entry_id ⇒ no mutation, watch_travel_no_entry_id, still counts for adjacency |
| `TestWatcherTruncatedTick` | len(events) == MaxResults ⇒ watch_events_truncated, inserts allowed, cancels suppressed |
| `TestWatcherHumanRemoved` | key inserted successfully, block missing next tick ⇒ (parent, side) denylisted, watch_travel_human_removed, no re-insert |
| `TestWatcherRateLimitAndBackoff` | 7 needed mutations ⇒ 6 performed, 1 deferred; 3 consecutive failures ⇒ key denylisted; not_owned ⇒ permanent skip for event id |
| `TestWatcherDisabled` | interval 0 ⇒ no goroutine effects; negative interval ⇒ same |
| `TestWatcherIdempotentAcrossRestart` | same calendar state, fresh watcher ⇒ identical meta.request_ids emitted |

### `internal/outlookcalendarwrite` — travel class

| Test | Cases |
|---|---|
| `TestTravelClassification` | "Travel: " insert routes to travel path; "Joel + " still routes to hold path; guard summaries untouched; patch routing requires marker AND summary prefix — forged travel marker inside a guard-summary event does NOT route to the travel path (regression guard for §3.2); patch routing by existing marker (travel vs hold vs guard) |
| `TestBuildTravelInsert` | valid; all-day rejected; <10 min, >2 h rejected; cross-midnight rejected; start older than now−5min rejected; >30 days rejected; 05:59 local start rejected, 06:00 accepted; local end 22:01 rejected, 22:00 accepted (21:55–23:55 block rejected); show_as free rejected; reserved marker key in inbound description rejected; travel_for=/parent_start= lines ACCEPTED; empty post-strip description rejected; marker block appended exactly with managed_by=sender |
| `TestBuildTravelPatch` | cross-agent patch refused; times-only move re-validated + summary keeps prefix; cancel state machine (only summary+show_as; cancelled refuses further patches; double-cancel idempotent success) |
| `TestTravelAllowlistAndCaps` | env unset ⇒ not_allowlisted (fail closed); 13th same-event-date insert per requester refused rate_limited; 17th global refused; HOLD caps untouched by travel inserts (regression: 2 holds + 12 travel same date all succeed); successful travel cancel releases budget ⇒ failed-booking compensation does not permanently consume the date's protection budget |
| `TestTravelIdempotency` | duplicate meta.request_id returns cached response WITH `replayed: true`, single service call; fresh responses omit the field |
| `TestTravelConflictAndDefense` | PowerShell trigger logic unit-tested at the Go boundary where feasible: insert conflict error mapped to code conflict; PATCH into a busy interval ⇒ conflict (§3.5 patch trigger); patch of guard block by travel requester ⇒ not_owned; guard regression suite untouched and green with both new envs unset |
| `TestTravelTZGate` | HoldTimeZoneOK=false ⇒ tz_mismatch for travel writes |

### Build gate

`go build ./... && go test ./...` green in repo A before commit on
`location-awareness`. No live smoke in this task (deploy is out of scope);
write the live-smoke script steps into the doc updates for a later session —
they MUST include §0's "cancel a watcher-discovered block by entry_id" proof
and the yellow-category-name confirmation.

## 10. Metrics (telemetry registry, counters — label-free; reasons in logs)

`travel_knowledge_load_failed`, `travel_estimate_requests`,
`travel_estimate_errors`, `schedule_travel_bookings`,
`schedule_travel_compensations`, `schedule_travel_compensation_failed`,
`schedule_travel_replay_reinserts`,
`watch_ticks`, `watch_events_truncated`, `watch_travel_created`,
`watch_travel_moved`, `watch_travel_cancelled`, `watch_travel_skipped`,
`watch_travel_ambiguous`, `watch_travel_not_owned`, `watch_travel_backoff`,
`watch_travel_no_entry_id`, `watch_travel_human_removed`,
`watch_mutations_deferred`; writer-side per §3.6.

## 11. Unchanged behavior (normative — reviewers verify each)

1. **Guard blocks**: validation, markers, summaries, PowerShell guard checks,
   and every existing guard test — byte-for-byte unchanged.
2. **Working holds**: prefix, marker block, 15min–2h/07:00–21:00/30-day rules,
   2-per-requester/5-global daily caps, cancel state machine, idempotency —
   unchanged except the two narrow additive items in §3.6 (optional cap
   release) and §3.7 (`replayed` flag on cache hits), both of which must leave
   every existing test passing unmodified. Travel inserts never count against
   hold caps.
3. **scheduler.v1 without `location`**: request validation, window grammar,
   slot policy (lunch, 10-min buffer, 30-min lead, 15-min boundaries),
   conflict-retry, infeasible/nearest-alternative, refusal rules, reply JSON
   bytes (no `travel` key, no `estimate` key) — identical to today.
4. **`schedule-move` / `schedule-cancel`**: behavior unchanged (new `location`
   key, if sent, is ignored).
5. **Prohibited-field refusals** (`involves_other_people`): unchanged and also
   applied to `travel-estimate` bodies (§2.4 step 2).
6. **Read agent** (`internal/outlookcalendar`): ONE additive change — the §0
   `entry_id` extended property. Event ids, masking, filtering, ordering, and
   every existing read test are otherwise unchanged. (Draft v1 claimed the
   read agent was untouched; that claim was unimplementable — see §13, B1.)
7. **Bus loop semantics**: ack-then-enqueue, derived `sched-*` conversations,
   response routing, 60 s + one retry upstream policy — unchanged; the watcher
   reuses them.
8. **`Reply.Terminal()`**: unchanged (§2.4).
9. **Existing tests**: none edited or deleted; only added (with the §3.7
   caveat about extending — never weakening — any exact-JSON replay
   assertion).
10. **`internal/meetingintake` (repo B)**: not touched; repo B not touched at
    all.

## 12. Open questions for Joel (none block implementation; defaults stated)

1. **Office address is UNVERIFIED** (`locations.json` note). Seed matrix
   minutes are estimates; briefing output should carry the uncertainty until
   confirmed. Default: ship as seeded.
2. **Actual yellow category string** on Joel's Outlook (assumed
   `"Yellow category"`; the contains-"yellow" fallback covers variants).
   Confirm during live smoke.
3. **Yellow + virtual location precedence** (§7.2): yellow wins (travel blocks
   created). Confirm this matches Joel's intent for yellow-categorized Teams
   meetings.
4. **Origin at departure time** (§1.5): origin is evaluated at
   `eventStart − default_travel_minutes`, so ~09:00–09:30 meetings get
   residence-origin estimates. Confirm the boundary feels right.
5. **After-block destination** is modeled as a return leg of equal duration.
   Real life may chain to the next venue. v2 with the maps API can chain legs.
6. **Holds asymmetry (§7.6)**: moving an offsite hold drops its travel blocks
   without recreating them. Fix requires a `location` field on write-agent
   events so holds become offsite-detectable — proposed v2 item.
7. **Other category colors**: schema supports any mapping; Joel to enumerate
   when convenient (design doc convention).
8. **Hold-marker forgery hardening** (§3.1 threat note): the guard
   insert/patch paths accept descriptions containing forged hold-class marker
   blocks (no reserved-key scan, no allowlist). The travel class is defended
   by two-factor routing; retrofitting the same summary+marker conjunction to
   HOLD routing — and/or a reserved-key scan on guard descriptions — is a
   v2 hardening item gated on not touching existing passing tests.

## 13. Review log

Codex peer review, 2026-06-11 (two parallel review passes; overlapping
findings merged below). Every finding was verified against the code in
`/home/joelkehle/Projects/jk/jk-email-agents` before acceptance — file
references confirmed in `internal/outlookcalendar/extractor.go`,
`internal/outlookcalendarwrite/{agent,service,validation,hold_markers,hold_state,contracts}.go`,
`internal/scheduler/{agent,cache,contracts,execution,policy}.go`,
`internal/telemetry/telemetry.go`, and `cmd/outlook-calendar-agent/main.go`.
**No finding was factually wrong; none were rejected.** All blockers and
should-fixes are incorporated; all nits were accepted as well.

### Blockers

| ID | Finding (merged) | Verification | Resolution |
|---|---|---|---|
| B1 | Watcher reattach/orphan-cancel unimplementable: events-list ids are `outlook_`+sha1-hash synthetics and `source_entry` is also hashed, but the write agent's patch path needs a raw Outlook EntryID (`GetItemFromID`); draft v1 simultaneously declared the read agent untouched (was §11.6). Two reviewers filed this independently. | CONFIRMED — `outlookEventID()`/`shortHash()` in extractor.go; `$namespace.GetItemFromID($env:JK_OUTLOOK_WRITE_EVENT_ID)` in service.go; `Event-FromItem` is the only raw-EntryID source. | Option (a) adopted: new §0 phase-1.5 additive read-agent change surfacing raw `entry_id` in extended properties (sanctioned by Joel's full-visibility ruling); §7.5 mutations address by entry_id and skip without it (`watch_travel_no_entry_id`); §11.6 amended; live-smoke proof item added (§9). |
| B2 | Travel-marker-first patch routing lets forged markers (plantable via the guard insert/patch paths, which run no reserved-key scan and have no allowlist) reroute guard events into the travel mutation path; draft v1's "spoofing already blocked" sentence was false. | CONFIRMED — `containsReservedHoldMarkerKey` called only from `BuildHoldInsert`/`BuildHoldPatch`; guard `BuildInsert`/`BuildPatch` scan nothing; `handleInsert`/`handlePatch` guard paths have no allowlist. | False sentence retracted; §3.2 travel patch classification now requires marker AND `Travel: ` summary prefix (unforgeable conjunction — no validation path lets a non-travel event hold both); §3.5 PowerShell patch defense mirrors the two-factor check; pre-existing hold-class hole documented in §3.1 threat note + §12.8 (out of scope: existing tests must not change). |
| B3 | §7.2's bare "non-empty, non-office location ⇒ offsite" classifies every Teams/Zoom/Webex/dial-in meeting as offsite — the watcher would bracket all virtual meetings with real busy 30-min travel blocks, polluting free/busy and burning the travel cap ahead of genuinely offsite meetings. Two reviewers filed this independently. | CONFIRMED — extractor passes `[string]$item.Location` through verbatim; nothing filters URLs/Teams strings. | `virtual_aliases` added to venues.json (§1.3, data not code); virtual match checked first (§1.4); `Estimate.IsVirtual`/`source:"virtual"` (§1.5); booking treats virtual as onsite (§4.1); §7.2 excludes virtual from the location test, with explicit precedence: yellow category WINS over virtual (Joel confirmation item §12.3); test cases added (§9). |

### Should-fixes

| ID | Finding (merged) | Verification | Resolution |
|---|---|---|---|
| S1 | Idempotency hole: hold inserted → travel step fails → compensation cancels hold → scheduler crash before reply-cache write → caller retry replays the writer's stale cached insert success ⇒ `booked` reply pointing at a cancelled hold. | CONFIRMED — `holdResponseCache` (24 h) stores the response as inserted; scheduler `replyCache` is in-memory. | §3.7 adds additive `MutationResponse.Replayed` set on every cache hit; §6.1 step-1 replay guard verifies a replayed hold via events-list and re-inserts once under `-insert-r2`; §6.4 rewritten with honest semantics; tests added. |
| S2 | Booking, watcher, and move-churn share one 8/day per-requester travel budget under the scheduler's identity; failed bookings burn never-decremented counters (`Record` only) ⇒ sticky starvation of travel protection (and hold-quota burn via compensation). Two reviewers filed variants (one as a nit). | CONFIRMED — `holdRateLimiter` has no decrement; both flows send as `ucla-tdg-scheduler-agent`. | §3.6: caps raised to 12/requester/day + 16/day global with explicit shared-budget math; limiter gains `Release` on successful live travel cancel-patch (compensation and orphan cancels return budget); hold-limiter release applied only if existing hold tests stay green, else documented burn; starvation regression test added. |
| S3 | travel-estimate inline dispatch under-specified: `validateRequest` would reject the action, the reply cache has no action component (request-id reuse could replay a `booked` reply to an estimate call), and adding `StatusEstimated` to `Terminal()` would create an accidental-cache hazard (`Terminal()` is consulted by `runJob`/`cache.Put`, never `sendReply`). Two reviewers filed this independently. | CONFIRMED — handleEvent flow and `canonicalKey` in agent.go/cache.go; `Terminal()` call sites in shared scheduler contract, cache.go, and agent.go. | §2.4 rewritten with normative placement (after ack+DecodeRequest+prohibited check, before validateRequest/canonicalKey/cache/inflight); `Terminal()` explicitly unchanged; `TestEstimateBypassesReplyCache` added. |
| S4 | Patch-side conflict-check trigger for travel was unspecified (§3.5 covered inserts only); watcher reattach patches could land on a busy interval; Go-side validation has no busy-set access so the PowerShell trigger is the only check. | CONFIRMED — existing hold patch branch runs `Test-HoldMarker $payload.description` + `Assert-NoBusyConflict` (service.go); the travel mirror was missing from the draft. | §3.5 patch branch now explicitly mirrors the hold trigger (`Test-TravelMarker` + show_as gate + self-EntryID exclusion); §3.4 clarified that the conflict check is PowerShell-only; test row added. |
| S5 | Three booking/estimate consistency gaps: (i) §4.2's "before/after independently omitted" contradicted §5/§6's all-or-nothing semantics; (ii) `Estimate()` `no_origin` behavior unspecified for the booking path and watcher; (iii) (filed together by reviewer 2) dates outside all residence windows are realistic in the shipped data. | CONFIRMED — locations.json windows start 2026-05-18; draft had no per-call error path outside §2.3. | §4.2: both blocks always present on booked offsite replies, `notes` reserved; §1.5/§4.1/§5: booking `no_origin` ⇒ `estimate_unavailable`, zero writes; §7.3: watcher skips with metric + logged reason; tests added for both paths. |
| S6 | Private-event masking silently breaks offsite detection and grammar parsing if a read surface opts into redaction; spec never stated the deployment prerequisite. Also (same reviewer): deterministic watcher keys + 24 h writer cache make human deletion of a travel block pathological (silent 24 h gap, then recreating a block Joel deleted). | CONFIRMED — masking lives in the extractor PowerShell; cache TTL in hold_state.go. The read-agent default is now full details, matching Joel-equivalent calendar visibility. | §7 preamble: normative `INCLUDE_PRIVATE_DETAILS=true` dependency + degraded-mode description + masked-sentinel defensive skip (§7.2 item 6); §7.3 human-deletion policy: per-process success-key memory ⇒ detect-and-denylist `(parent, side)` with `watch_travel_human_removed`, honest documentation of the 24 h and restart blind spots; tests added. |
| S7 | Watcher insert idempotency key omitted the desired interval: a parent whose END moves (start unchanged) re-sends the identical key and the writer's 24 h cache replays the original insert — no write, false `watch_travel_created`, stale block. | CONFIRMED — cache replay semantics in hold_state.go; draft key was `hash(M.ID|M.start|side)`. | §7.3 key now embeds `M.end` and the computed `I.start`/`I.end` (still deterministic across restarts); §7.5(3) move key likewise; `TestWatcherKeyEmbedsInterval` added. |

### Nits (all accepted)

| ID | Finding (merged) | Resolution |
|---|---|---|
| N1 | §2.3 example showed `"is_office": false`, impossible under the omitempty mandate; nested structs would need pointers. Two reviewers filed this independently. | §2.3 restructured to a single `Reply.Estimate *EstimateResult` pointer with plain-typed inner fields; legacy byte-identity guaranteed by one nil check; the §1.5 non-virtual `Minutes ≥ 1` invariant stated. |
| N2 | Origin rule used meeting START to pick office origin — 09:00–09:30 offsite meetings would get undersized office-origin estimates despite departing from home. | §1.5 origin rule now evaluates at `eventStart − default_travel_minutes` (departure time); Joel confirmation item §12.4; origin tests updated. |
| N3 | `watch_events_truncated` undetectable (read agent clamps silently, no flag); negative `SCHEDULER_WATCH_INTERVAL_MIN` unspecified. | §7.1: truncation defined as `len(events) == requested MaxResults`, with cancels suppressed on truncated ticks; `withDefaults` clamps ≤ 0 ⇒ disabled; tests added. The same reviewer's `Terminal()` point is merged into S3. |
| N4 | Writer enforced the travel window on START only; a 21:55–23:55 block would pass writer validation, leaving the 22:00 ceiling scheduler-side only. | §3.4 adds writer-enforced local END ≤ 22:00 (so the writer remains the safety boundary); `TestBuildTravelInsert` rows added. |
| N5 | Watcher plumbing guesses: the `sched-w<hash>` conversation literal cannot be produced by `newUpstreamSession` (hardcodes `"sched-"+hash`); `writeRequest` hardcodes its meta-key format so the watcher needs an explicit-meta-key helper; `watch_travel_skipped{reason=...}` labels don't exist in the telemetry registry (`IncCounter(name)` only). | §7.1 pins the session-key convention (`"watch|"+tickStart` ⇒ `sched-<hash>`) and the explicit-meta-key write helper; §7.3/§10 use a single label-free `watch_travel_skipped` counter with reasons in log lines only. |
| N6 | Shared travel-cap rationale counted only the booking flow. | Merged into S2 (caps raised + budget math + release-on-cancel). |

## 14. Implementation notes (2026-06-11, Fable — deviations recorded honestly)

Implemented on `location-awareness`; `go build ./... && go test ./...` green;
no existing test modified. Deviations from the letter of this spec:

1. **Package path.** `internal/travel` is already occupied by the live
   trip-planning travel agent (port 8203), so the knowledge package lives at
   `internal/travelknowledge` (same API as §1).
2. **§3 superseded by the committed travel class.** The write-agent travel
   class was implemented first from `docs/OUTLOOK_CALENDAR_TRAVEL_CLASS_SPEC.md`
   (commit `6bd8155`), whose rulings differ from §3 and win where they
   conflict: local window 05:00–23:00 (writer side; the scheduler and watcher
   still enforce 06:00–22:00 on every block THEY create, so the user-visible
   §5/§7.3 bounds hold), caps 8/requester + 20/global per event date (not
   12/16; same shared-budget rationale), SAME allowlist env
   `OUTLOOK_CALENDAR_WRITE_HOLD_REQUESTERS` (no
   `OUTLOOK_CALENDAR_WRITE_TRAVEL_REQUESTERS` env exists — travel-class spec
   ruling 3), class/action-qualified idempotency keys, 4096-byte description
   cap, and a stale-snapshot check on live travel patches. §3's two genuinely
   new items were added here additively: `MutationResponse.Replayed` (§3.7)
   and travel-limiter `Release` on live cancel (§3.6) with metric
   `event_travel_budget_released`. The HOLD limiter got NO release (hold
   behavior frozen; compensation hold-quota burn is accepted v1 behavior, as
   §3.6 allows). §9's write-agent test rows are covered by the travel-class
   suite committed with `6bd8155` plus the two new tests
   (`TestTravelIdempotencyReplayedFlag`, `TestAgentTravelCancelReleasesBudget`).
3. **§1.5 origin fallback is weekday-only.** As written ("fall back to the
   other"), `no_origin` was unreachable for non-office destinations: the
   loader requires a valid `work` entry, so an absent residence always fell
   back to the office — contradicting §1.5's own note and §9's
   `TestWatcherNoOrigin`/`TestNoOriginBooking`. Implemented: residence→office
   fallback applies only when the departure is Mon–Fri; weekends outside all
   residence windows yield `no_origin`. (`TestOriginRule`'s "no residence
   valid ⇒ fallback office" row runs on a weekday and passes.)
4. **§7.3/§7.5 intra-tick ordering.** Blocks are reconciled before creation;
   a planned reattach claims its (meeting, side) so the creation pass does not
   also insert for it. Step §7.5(3) reattach applies only when NO offsite
   meeting still starts at the grammar `HH:MM` on the block's date: a parent
   whose END moved while its start stayed is handled as cancel + fresh insert
   under a new interval-embedding key (the §13/S7 scenario;
   `TestWatcherKeyEmbedsInterval` pins this), while a truly moved parent gets
   the §7.5(3) patch (`TestWatcherReattach`).
5. **JSON key spelling.** The read agent serializes extended properties as
   `extendedProperties.private.entry_id` (the `calendarreadcontract.Event` tag), not
   `extended_properties` as written in §0/§7.5.
6. **Masked-private events anchor.** §7 preamble says masked-private events
   "count for adjacency"; §7.5(2)'s skip-list cross-reference excluded them.
   The preamble wins: they are anchors (safer — fewer cancels) and shrink
   neighbors, and are never mutated and never get travel.
7. **`travel-estimate` request field.** The meeting start rides in a new
   `event_start` field on the scheduler `Request` struct (§2.2's shape),
   ignored by all scheduler.v1 actions.
8. **Watcher dry-run.** `watch_travel_created` counts dry-run `would_write`
   responses too (§7.7 treats them as success for backoff), but dry-run
   successes do NOT arm the §7.3 human-deletion detector — nothing was
   written, so a missing block next tick is not a human deletion.
