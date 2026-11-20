# OpenShip Contract

Read this before acting on a spec, ticket, or PR. If this file is missing, run `/os-setup`.

## Artifacts
- Spec: issue labeled `os:spec`; sole source of behavior.
- Ticket: issue labeled `os:ticket`; scope + acceptance.
- Brief: issue comment `## Agent Brief`; points at the spec (or `spec=none`), never restates behavior.
- Comments: context; a behavioral change edits the spec first.
- PR body: `Issue: #N`, `Spec: #N (read at <ISO>)`, `Brief: <link>`.
- Review: `OS-REVIEW` comment with head SHA + verdict.

## Labels
State (issue): `os:triage`, `os:needs-info`, `os:ready`, `os:in-progress`, `os:blocked`, `os:wontfix`, `os:done`.
Review (PR): `os:needs-review`, `os:in-review`, `os:changes`, `os:approved`, `os:blocked`.
Type: `os:spec`, `os:ticket`, `os:bug`, `os:feature`, `os:refactor`, `os:chore`.

## Modes
- **Facilitator**: writes artifacts; never code.
- **Author**: writes branch, commits, PR; never reviews; merges only when the brief names it merger.
- **Reviewer**: no code writes; never edits, pushes, or resolves threads; reviews one head, comments the verdict, stops; merges only when the brief names it merger.
- **Watcher**: loads after a PR with a role from the caller; the author watches until merged or closed; the reviewer does not persist.
- Manual skills run only when the user asks; `os-pr-watch` is the only automatic skill.
- A manual skill may load as a prerequisite of a skill the user invoked; it never self-starts.

## Authority
Behavior: spec. Scope: ticket. Handoff: brief. Delta: comments newer than `brief.at` — context, not behavior. Code: evidence. Review: claim, not authority; answer `fixed` or `declined` + evidence.
Conflict: stop and quote both sources; never pick silently.

## Preflight
1. Read this contract. Check the skill's required artifact.
2. Missing → load its producer, run to the producer's stop, resume. Announce in one line.
3. ≤2 automatic hops from the invoked skill; a user-approved hop resets the count. No cycles.
4. Never skip the producer's checkpoint. Never re-ask confirmed input.
5. Declined prerequisite → record `Prerequisite waived: <what> — <why>`; stop unless the output stays correct.

## Autonomy
`advise` analyze · `build` commit · `deliver` PR + fix · `ship` merge on green, per-task grant naming the merger. Default: `deliver`.

## Rules
- One writer per branch: `os/<issue>-<slug>`. Push it before work; a rejected push means another writer owns the issue.
- Claims: the branch locks authorship, PR `os:in-review` locks review. Labels index; markers and SHAs are truth.
- Labels: `gh api` REST; `gh pr edit` and `gh issue edit` fail on Projects-classic data.
- Evidence before claims: command + observed output.
- Right-size: typo → ticket; behavior or 3+ files → spec.
- Never weaken a gate (test, skip, timeout, snapshot, CI, suppression) without human sign-off.
- No subagents unless the user asks.

## References
`.openship/ARTIFACTS.md`, `REVIEW.md`, `WATCH.md`, `DEBUG.md`, `config.json`.
