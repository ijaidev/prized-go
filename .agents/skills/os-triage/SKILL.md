---
name: os-triage
description: "Manual. Classify an incoming issue and route it: needs info, duplicate, wontfix, ticket, or spec. Run when the user asks to triage or route an issue, for example /os-triage."
---

# Triage

One issue in, one route out. Read-only except labels, comments, and a reasoned close for duplicate or wontfix.

## Preflight

- Read `.openship/CONTRACT.md`; absent → stop: `/os-setup` first.
- Read the issue, comments, repo, and `.openship/config.json`.
- Already `os:ticket` → check the brief and `os:ready`; route to `/os-implement`.
- Already `os:spec` → route to `/os-tickets`.
- Search before asking: `gh issue list --search`, code search, history.
- Reproduce a bug when cheap. Run the stated command; evidence beats speculation.

## Classify

Ask at most 3 questions, only when the answer changes the route.

| Signal | Route |
|---|---|
| Duplicate or already fixed | Link it; close or `os:wontfix` |
| No repro / missing info | `os:needs-info` + exact command/output needed |
| Typo, docs, dependency, one-file mechanical | Ticket: brief + `os:ticket`, `os:ready` |
| Clear bug with repro | Ticket: brief + regression test, `os:ready` |
| New behavior, data model, or 3+ files | Spec |
| Security | No public details; use the repo security policy |

Right-size: one-line cause and one-file fix → ticket. New behavior or contract change → spec. Bug touching 3+ files or changing behavior → bugfix spec.

## Route

- **Small**: post the brief (`spec=none`; the ticket is the only artifact), add `os:ticket` + type + `os:ready`. Print `Start: /os-implement #N.`
- **Large**: print `Next: /os-spec — <one-line outcome>.` Chain only if the user asked to proceed.
- **Needs info**: one comment with numbered questions and the exact evidence required. Stop.
- **Duplicate / won't fix**: link the reason; close only with a reason.

## Rules

- Do not write code, open PRs, or create a spec-sized ticket.
- A bug without a repro is not ready; ask for one.
- Do not relabel a human decision without saying so.
- A brief is ≤40 lines, points at the spec, and lists decisions with sources.
