---
name: os-implement
description: "Manual. Implement a ready ticket on its own branch, verify, and open a PR. Run when the user asks to implement a ticket or open a PR, for example /os-implement."
---

# Implement

Author mode. One ticket, one branch, one PR. Never merge unless the brief names you merger.

## Preflight

1. Read `.openship/CONTRACT.md`; absent → stop: `/os-setup` first.
2. Read `.openship/ARTIFACTS.md` and `.openship/DEBUG.md`.
3. Resolve the ticket: `gh issue view N`.
   - Spec, not ticket → chain to `/os-tickets` (one hop; it may create the spec, two total). If the work is still fuzzy, stop and ask for `/os-grill`. Announce each hop.
   - Ticket without `os:ready` → check dependencies. Open blocker → stop and name it.
   - Ticket without a brief → small and self-contained: draft the brief and confirm once. Otherwise: spec exists → chain to `/os-tickets`; no spec → stop and ask.
4. Read spec, ticket, brief, and comments newer than `brief.at`. Quote the `R#` and `S#` you will satisfy; with no spec, quote the ticket's acceptance criteria.
5. Spec vs ticket/comment conflict → stop and ask. Never pick silently.
6. Existing `os/<issue>-*` branch or PR for this issue → resume it only if you are the writer; otherwise stop and report. Never open a second.

## Autonomy

Read the brief, then `.openship/config.json`, then `deliver`. Precedence: explicit user instruction > brief > config. A merge still needs a per-task `ship` grant.

| Level | May |
|---|---|
| `advise` | Analyze only |
| `build` | Branch + commits |
| `deliver` | Push, open PR, fix findings |
| `ship` | Merge when the gate in `WATCH.md` is green and the grant is per task |

`advise` stops before Work. `build` stops after commits: no push, no PR.

## Work

1. `git fetch`; resume any `os/<issue>-*` branch. Otherwise create and push `os/<issue>-<slug>` from the default branch. A rejected push means another writer owns the issue; stop.
2. Claim: add `os:in-progress`; remove `os:ready`. The branch is the lock; the label is the index. Keep `os:in-progress` until merge.
3. Reproduce the bug or write the failing test first. For a feature, test the smallest behavior.
4. Implement the smallest diff that satisfies every requirement. No drive-by edits. Every changed line should trace directly to the ticket and the spec.
5. Run test, lint, and build. For behavior, drive the real surface. Classify: `verified` (observed), `best-effort` (tests only; say why), or `reverted`.
6. Commit in logical units: `<type>(<scope>): <what> (#N)`.
7. Push and open a ready PR from the template. Include `Issue`, `Spec (read at)`, `Brief`, `Decisions`, `Requirements`, `Evidence`, `Scope`, `Merge danger`.
8. Label the PR `os:needs-review`; keep the issue `os:in-progress`.
9. Load `/os-pr-watch as author` unless `config.watch = manual` or the user said not to watch.

## Stops

- 3 failed attempts on one failure → `<!-- os:blocked reason=... -->`, label `os:blocked`, report.
- Stale claim (branch, no PR, no commit newer than the claim) → delete the branch and restart; report it.
- Missing credential, ambiguous behavior, or a gate you must weaken → stop and ask.
- A review finding is a claim. If it is wrong, reply with evidence and decline; do not change code to satisfy it.
- Proof is evidence you observed yourself in this session. No completion claims without fresh verification evidence.

## Rules

- One writer per issue; any `os/<issue>-*` branch is the same claim. Do not force-push a shared branch.
- Never weaken a test, skip, timeout, snapshot, CI step, or suppression.
- Do not fix unrelated findings; file them.
- Do not merge unless the brief names you merger; do not approve or resolve review threads.
- No subagents unless the user asks.
