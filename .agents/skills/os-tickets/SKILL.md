---
name: os-tickets
description: "Manual. Slice a spec into tracer tickets with briefs and native dependencies. Run when the user asks to break a spec into tickets, for example /os-tickets."
---

# Tickets

Turn one spec into 3–7 vertical tickets. Each is one author, one context window, one demo.

## Preflight

- Read `.openship/CONTRACT.md`; absent → stop: `/os-setup` first.
- Read the spec issue, its comments, `.openship/ARTIFACTS.md`, and `config.json`.
- Missing spec → chain to `/os-spec` (one more hop). If the spec is fuzzy and the budget allows, `/os-spec` chains to `/os-grill` (the second hop). Announce each hop; stop at two.
- Spec has open behavior questions → stop; resolve them in the spec first.
- Spec touches 3+ independent outcomes → split the spec first.

## Slice

- **Vertical**: each ticket delivers observable behavior, not a layer.
- **Tracer first**: the thinnest end-to-end path. Then widen.
- **Prefactor** before the feature that needs it.
- **Wide refactor**: expand → migrate → contract, one ticket per step.
- Dependencies form a DAG. No cycles. Every `Blocked by` ticket is created in this batch or already open.
- Only the frontier (no open blockers) gets `os:ready`. Do not label dependency-waiting tickets `os:blocked`; that label is for human decisions. The post-merge check in `WATCH.md` promotes the next frontier.
- More than 7 tickets means split the spec. Fewer is fine.

## Brief

One brief per ticket, posted as a comment after the issue exists. Use the Brief template:

- `<!-- os:brief spec=#N|none at=<ISO-8601> -->`
- `**Autonomy:**` defaults to `deliver`; set `ship` only on the user's explicit words. `**Merger:**` defaults to `author`.
- Decisions with sources, hazards, verification, out of scope.
- ≤40 lines. Never restate behavior.
- Any open question means the ticket is not `os:ready`.

## Confirm and create

Show a table: title, type, blocked by, demo, and the first ticket's brief. Ask once: `Create these tickets?`

On yes, for each:

```bash
gh issue create --title "<title>" --body-file <tmp> \
  --label os:ticket --label os:<bug|feature|refactor|chore>
```

Then add dependencies:

```bash
id=$(gh api repos/{owner}/{repo}/issues/<blocker> --jq .id)
gh api --method POST repos/{owner}/{repo}/issues/<blocked>/dependencies/blocked_by \
  -F issue_id="$id"
```

If the dependency API is unavailable, write `Blocked by: #N` in the body and add a parent task list. Never invent a native link.

Post each brief. Set `os:ready` on the frontier only.

## Report

Print the DAG and the frontier. `Start: /os-implement #<frontier>.`

## Rules

- Do not write code or open PRs.
- A ticket narrows the spec: scope, acceptance, verification. It never restates behavior; the author reads the spec for behavior.
- Preserve the spec's requirement IDs in acceptance criteria.
- Only the user authorizes `ship` or a reviewer merger; never self-grant.
