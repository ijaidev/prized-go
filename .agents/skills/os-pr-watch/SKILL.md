---
name: os-pr-watch
description: "Automatic. Watch a PR and act on new events until merged, closed, or stopped. Load after opening a PR or when the user asks to watch one."
---

# PR Watch

Watch one PR. The role comes from the caller, not from this skill.

## Load

1. Read `.openship/CONTRACT.md`; absent → stop: `/os-setup` first.
2. Read `.openship/WATCH.md` (roles, transitions, gate) and the PR's `Issue` / `Spec` / `Brief`.
3. Role: from the invocation (`as <role>`). Missing → infer from context; ambiguous → ask once.
4. Confirm the role's lock per `WATCH.md`.
5. Announce: `Watching PR #N as <role> until merged, closed, or stopped.`
6. Find the cursor by marker: `<!-- os:watch role=<role> ... -->`. Create it if absent.
7. If the user says another watcher has your role, stop and report. A cursor alone is not proof of an active watcher.

## Each cycle

1. Fetch events newer than the cursor. Ignore your own actions and processed keys.
2. Act once per event, per the role table in `WATCH.md`. Never act twice on the same key in a session.
3. Update the cursor comment. Only the event handler changes labels.
4. Quiet PR → do nothing. Quiet is not a stop.

## Merge

Follow the merge gate in `WATCH.md`. The named merger checks it every cycle, not only on an event. Never `--admin`; never merge `BEHIND` or `DIRTY`.

## Stop

Per `WATCH.md`. On merge, run the post-merge order there. On stop without merge, say what remains and who must act.

## Limits

- Use the harness's native watch if it has one. Otherwise poll once per turn and say: `Watching only while this session is active; re-invoke /os-pr-watch #N as <role> to continue.`
- At most one event per cycle; honor a ≥60s cooldown when the harness supports it. No `sleep` loops, no `gh --watch`, no subagents.
- GitHub GraphQL is separate from REST; fall back on rate-limit exhaustion.
- Never claim to watch after the process ends. Leave the cursor.
