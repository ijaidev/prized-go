---
name: os-spec
description: "Manual. Turn a clear intent into a durable behavior spec as a GitHub issue. Run when the user asks to write a spec or turn an idea into an issue, for example /os-spec."
---

# Spec

Produce one spec: the sole source of behavior. No code, no tickets.

## Preflight

- Read `.openship/CONTRACT.md`; absent → stop: `/os-setup` first.
- Read `.openship/ARTIFACTS.md` and `.openship/config.json`.
- If `.openship/` is missing, stop: `Run /os-setup first.`
- Need a clear problem and outcome. If fuzzy and this is not already the second hop, load `/os-grill` (one more hop), run to its stop, then resume. If this is the second hop, stop and ask for `/os-grill`. Announce: `No clear outcome — grilling first.`
- Right-size. A typo, dependency bump, docs fix, or one-file mechanical change does not get a spec. Say so and route to `/os-triage`.
- Read the code, tests, and docs the behavior touches. Ground every claim.

## Write

- Use the feature or bugfix template from `ARTIFACTS.md`.
- Set `Status: ready` and `Updated:` to now at creation. `shipped` is set only by the post-merge check.
- Behavior only. No file paths, line numbers, or implementation steps.
- Requirements: `R1. WHEN <condition> THE SYSTEM SHALL <behavior>.` One behavior each.
- Acceptance: `S1. GIVEN … WHEN … THEN …`. At least one per requirement.
- Bugfix: current behavior, expected behavior, `SHALL CONTINUE TO`, repro.
- No-change list: interfaces and behavior that must survive.
- Open questions that can change behavior → ask before creating. Never create a spec with unresolved behavior.
- ≤120 lines. Longer means split by outcome, not by layer.

## Confirm and create

Show the full spec once. Ask one question: `Create as spec issue?`

On yes:

```bash
gh issue create --title "<title>" --body-file <tmp> \
  --label os:spec --label os:<bug|feature|refactor>
```

Print the URL and: `Next: /os-tickets #N.`

## Spec home

- `config.specHome = "issue"` (default): the issue is the only copy.
- `"repo"`: write `specs/<slug>.md`, then open an issue whose body is a pointer. One copy, never both.
- No GitHub auth: use `"repo"` and say the GitHub steps were skipped.

## Rules

- One spec per outcome. Two outcomes are two specs.
- A spec states behavior, not implementation.
- Do not create tickets, branches, or PRs here.
- Comments that change behavior edit the spec first.
