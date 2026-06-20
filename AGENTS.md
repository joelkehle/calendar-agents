READ ~/Projects/shared/agent-scripts/AGENTS.MD BEFORE ANYTHING (skip if missing).

# calendar-agents

## Purpose

Shared calendar agents and contracts for Joel Kehle. This repo owns
cross-context Outlook read/write adapter runtime, scheduler/travel runtime,
calendar identity, event-class contracts, and read/write wire schemas used by
both personal and UCLA TDG workflows.

## Start Here

- Read `README.md` for current module layout and migration status.
- Read `docs/CALENDAR_OWNERSHIP.md` before changing agent IDs, event ownership
  markers, scheduler/write-agent contracts, or deciding whether a feature belongs
  here versus a personal/professional consumer repo.
- Read `~/Projects/shared/manager/docs/services/port-allocations.md` before
  changing host ports, bus URLs, or health endpoints.
- Run `bus-discover` before adding or changing calendar-facing agentic behavior.

## Boundaries

- Keep live bus IDs stable unless Joel approves a deploy window and migration
  plan.
- Shared calendar core belongs here. Personal email/inbox policy stays in
  `~/Projects/jk`; UCLA intake/IP/deal policy stays in `~/Projects/ucla-tdg`.
- Do not put Gmail, inbox, IP, deal, or person-specific policy in this repo
  unless it is strictly a calendar contract consumed by both contexts.
- No secrets, tokens, raw email bodies, private keys, or private calendar event
  bodies in fixtures or docs.

## Testing

- Default gate: `go test ./...`
- Contract and runtime changes must include focused unit tests.

## Deploy

Runtime services must expose `GET /health` and `GET /metrics`. Before changing
ports, bus URLs, agent IDs, scheduled-task names, or systemd units, update the
manager service inventory/docs and rerun live bus discovery.
