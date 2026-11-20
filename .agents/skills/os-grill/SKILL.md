---
name: os-grill
description: "Manual. Interrogate a fuzzy idea into decisions before any spec or code. Run when the user asks to be grilled or to think an idea through, for example /os-grill."
---

# Grill

Read `.openship/CONTRACT.md`; absent → stop: `/os-setup` first.

Goal: a decision-complete intent in ≤10 questions. No spec, no code, no issue.

## Rules

- One question per turn. Never a questionnaire.
- Every question carries your recommended answer and why. The user can reply "yes".
- Answer from the repo first: code, docs, ADRs, issues, history. Never ask what you can discover.
- Challenge a premise once, with evidence. Do not argue.
- Skip anything the user already answered.
- Stop when the remaining unknowns cannot change the first ticket.
- If the user says "just write it", stop and hand off.

## Sequence

Ask only what is unresolved:

1. **Problem** — what breaks, for whom, evidence.
2. **Workaround** — what happens today; why it is insufficient.
3. **Outcome** — observable state when shipped.
4. **Smallest proof** — the thinnest version that demonstrates the outcome.
5. **Non-goals** — what must not change.
6. **Constraints** — compat, data, security, rollout, reversibility.
7. **Hazards** — what breaks if done naively; what a reviewer must check.

## Output

When done, print:

```markdown
## Intent
Problem: ...
Outcome: ...
In scope: ...
Out of scope: ...
Constraints: ...
Hazards: ...
Open: none
```

Then: `Next: /os-spec.`

Write `docs/intent/<slug>.md` only if the user asks for a durable copy. Otherwise stop here. Do not create a spec or an issue from this skill.
