---
summary: Ownership boundary for Joel's shared calendar capability across personal, professional, and shared workspaces.
read_when:
  - changing calendar agent IDs, scheduler travel-time logic, Outlook read/write agents, or calendar guard wiring
  - deciding whether calendar code belongs in jk, ucla-tdg, or shared
  - explaining why ucla-tdg-prefixed calendar IDs still exist
---

# Calendar Ownership Boundary

`~/Projects/jk` means Joel personal. `~/Projects/ucla-tdg` means Joel
professional. `~/Projects/shared` means used by both personal and professional
contexts.

Joel's primary calendar is a shared capability. It may be consumed by personal
agents and professional agents, but its core contracts are not personal-only and
not UCLA-only.

## Current Transitional Shape

The live implementation still runs mostly from `jk-email-agents` because the
work started near personal email and calendar guard automation. Some live bus
IDs still carry `ucla-tdg` prefixes because the first write-side scheduler
consumer was professional:

- `jk-outlook-calendar-agent`: personal-bus Outlook read profile.
- `ucla-tdg-outlook-calendar-agent`: professional-bus Outlook read profile.
- `ucla-tdg-outlook-calendar-write-agent`: shared Outlook write capability with
  a historical ID.
- `ucla-tdg-scheduler-agent`: shared scheduling/travel-time orchestrator with a
  historical ID.
- `jk-calendar-guard-agent`: dual-bus policy consumer that reads personal inbox
  pressure and professional calendar context before requesting blocks.

The ID prefix, bus URL, or Outlook adapter does not decide ownership. A shared
calendar service can register on one or both buses while remaining shared.

## Placement Rule

- Personal-only email, inbox, Polsia, unsubscribe, or Google Calendar utilities
  belong under `jk`.
- Professional-only UCLA TDG intake, project, IP, or deal workflows belong under
  `ucla-tdg`.
- Calendar core shared by personal and professional workflows belongs here under
  `shared/calendar-agents`.
- Consumer repos may keep thin adapters, deployment wiring, and policy hooks,
  but new shared calendar contracts should start here.

## Incremental Migration Path

Small slices beat a risky wholesale move:

1. Centralize shared-calendar IDs, names, read/write wire schemas, event
   classes, and scheduler bus contracts in this repo.
2. Stop adding new UCLA- or JK-owned language to shared calendar contracts.
3. Keep runtime IDs and allowlists stable until a deploy window exists.
4. Extract scheduler and Outlook adapter implementation code here as
   interfaces stabilize.
5. Leave `jk` and `ucla-tdg` repos with consumers/policies, not calendar-core
   ownership.
