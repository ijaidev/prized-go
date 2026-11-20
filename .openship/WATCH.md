# Watch

## Roles

- **author** — only writer; fixes or declines findings, pushes, merges when the grant names it; watches until merged or closed.
- **reviewer** — no code writes except a granted merge; reads the spec and diff, re-runs one claim, posts a verdict, stops. The named reviewer-merger waits for pending checks.

One watcher per role per PR. The caller sets the role: `as author` or `as reviewer`. Never guess.

## Labels

Issue states: `os:triage` → `os:needs-info` | `os:ready` → `os:in-progress` → `os:done`. `os:blocked` and `os:wontfix` are terminal until a human changes them.

PR review states: `os:needs-review` → `os:in-review` → `os:changes` → `os:needs-review` → `os:in-review` → `os:approved`. `os:blocked` when a human must decide.

The issue stays `os:in-progress` from claim to merge. Review state lives on the PR.

## Transitions

- Author claim: issue `os:ready` → `os:in-progress`.
- Author push: PR `os:needs-review`; issue stays `os:in-progress`.
- Reviewer claim: PR `os:needs-review` → `os:in-review`.
- Verdict: `os:in-review` → `os:changes` | `os:approved` | `os:blocked`.
- Fix push: PR `os:changes` → `os:needs-review`; issue stays `os:in-progress`.
- Head moved mid-review: `os:in-review` → `os:needs-review`.
- Merge: issue `os:done`; close.

## Label writes

Use REST. `gh pr edit` and `gh issue edit` fetch Projects-classic data and fail.

```bash
gh api --method POST repos/{owner}/{repo}/issues/<n>/labels -f 'labels[]=os:ready'
gh api --method DELETE repos/{owner}/{repo}/issues/<n>/labels/os:ready --silent
```

PRs use the same issues endpoint.

## Claims

- Author: one branch per issue, `os/<issue>-*`. Push it before work. A rejected push means another writer owns it; stop. Set `os:in-progress` only after the push. Keep it until merge. A second slug is not a second claim.
- Reviewer: take a head with `os:needs-review` (or an explicit user request) and no live `os:in-review`. Post the claim marker, re-read; an earlier claim for the same head wins. Then replace `os:needs-review` with `os:in-review`, review that head, post `OS-REVIEW`. Clear the claim only if the head is still latest; if it moved, restore `os:needs-review`.
- Stale: `os:in-review` with no claim activity for 30 minutes and no verdict on that head may be taken over. Delete a claim branch only when it has no PR and no commit newer than the claim.
- Labels are indexes; the branch ref and the `head=<sha>` markers are truth.

## Markers

```
<!-- os:review head=<sha> verdict=approve|changes|blocked -->
<!-- os:review-claim head=<sha> reviewer=<id> at=<ISO-8601> -->
<!-- os:fix head=<sha> review=<id> -->
<!-- os:watch role=author|reviewer last=<event-id|sha|iso> -->
<!-- os:blocked reason=<short> -->
```

## Dedupe

- review: `pr:<n>:review:<head>`
- fix: `pr:<n>:fix:<head>:<review-id>`
- watch: `pr:<n>:watch:<role>`
- merge: `pr:<n>:merge:<head>`

## Loop

1. Read your cursor. Fetch events newer than it.
2. Ignore your own actions and processed keys.
3. Act once per event by role: the author fixes or declines and pushes; the reviewer reviews the new head, posts a verdict, and stops. Never re-act on the same key in a session.
4. Update the cursor comment.
5. Quiet PR → do nothing, keep watching. Not a stop.

## Event order

1. **Conflicts** — the author reports and rebases only if safe; the reviewer reports only; never force-push a shared branch.
2. **Review threads** — the author checks each finding against the spec, then fixes or declines with evidence; on a fix push: clear `os:changes`, set `os:needs-review`, post `OS-FIX`. The reviewer claims the head and posts a verdict. Never silently complies.
3. **CI** — the author fixes only the diff's failures; flake earns one rerun; unrelated → report. The reviewer reports.

## Merge gate

The `ship` grant is per task and names the merger: `author` (default) or `reviewer`. A config default of `ship` is not a grant. The named merger checks the gate every cycle, not only on an event.

- Required checks green on the latest head.
- `APPROVE` on that head by a reviewer who is not the author.
- No open blocker/should.
- No human change request.
- Mergeable; branch current.
- No gate weakening.

Green with a grant: use the repo's merge method; never `--admin`; never merge `BEHIND` or `DIRTY`.
Green without a grant: keep `os:approved`, comment `Human merge required.`, keep watching.
Not green: report what is missing.

## Post-merge

The merger, in order:

1. Replace the ticket's state label with `os:done`; close it as completed.
2. Promote the frontier: open `os:ticket` children of the spec with a brief, all blockers closed, and no state label get `os:ready`.
3. Close the spec only when every `R#` (or bugfix requirement) maps to a merged PR's `Requirements` section and every child ticket is closed. Set `Status: shipped`, bump `Updated:`, close it. If every child is closed but an `R#` is uncovered, leave it open and comment the gap. Do not comment while any child is open.
4. Delete the branch if the repo does.

Never close a spec with an open child or an uncovered `R#`.

## Stop

Merged, closed, user says stop, 3 review rounds, 3 fix attempts on one failure, a security-sensitive or irreversible change, a gate weakening, or `os:blocked`. The reviewer stops after a verdict unless it is the named merger waiting on checks; the author watches through `os:approved`.

## Limits

- Native watcher if available; else one bounded poll per turn. At most one event per cycle; honor a ≥60s cooldown when the harness supports it. No `sleep` loops.
- GitHub GraphQL is separate from REST; fall back on exhaustion.
- Never claim to watch after the process ends. Leave the cursor.
