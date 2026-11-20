# Artifacts

Provenance: Intent → Spec → Ticket → Brief → PR → Review. Every artifact names its parent.

## Spec (feature)

```markdown
# <Title>
Status: draft | ready | shipped
Updated: <ISO-8601>
Links: <intent, prior art, related>

## Problem
<what breaks, for whom, why now; evidence>

## Outcome
<observable state when shipped>

## In scope
- ...

## Out of scope
- ...

## Requirements
R1. WHEN <condition> THE SYSTEM SHALL <behavior>.
R2. ...

## Acceptance scenarios
S1. GIVEN ... WHEN ... THEN ...
S2. ...

## Constraints
<compat, data, security, perf, rollout>

## No-change list
<files, interfaces, or behaviors that must not change>

## Open questions
- [ ] ...
```

Rules: behavioral and testable; no file paths or line numbers; ≤120 lines; split if longer. `Status` moves draft → ready on confirm; `shipped` only through the post-merge check in `WATCH.md`.

## Spec (bugfix)

Use when the fix touches 3+ files or changes behavior. Otherwise write a ticket with a repro.

```markdown
Status: draft | ready | shipped
Updated: <ISO-8601>

## Current behavior (defect)
WHEN <condition> THEN the system <incorrect behavior>.

## Expected behavior (correct)
WHEN <condition> THE SYSTEM SHALL <correct behavior>.

## Unchanged behavior (regression guard)
WHEN <condition> THE SYSTEM SHALL CONTINUE TO <existing behavior>.

## Repro
1. ...

## Root cause hypothesis
<one paragraph; confidence: high | medium | low>

## Fix + regression test
<approach; the test that fails before and passes after>
```

## Brief

A comment on the issue. Handoff memo, not a mini-spec.

```markdown
## Agent Brief
<!-- os:brief spec=#N|none at=<ISO-8601> -->

**Read first:** Spec #N — sole source of behavior.
**Ticket:** #N
**Category:** bug | feature | refactor | chore
**Autonomy:** advise | build | deliver | ship
**Merger:** author | reviewer

### Decisions made
- <decision> — spec §R2 / comment <link>

### Hazards
- <what breaks if done naively>
- <gate that must not be weakened>

### Verification
- `<command>` → <expected>
- Surface: `<command>` → <observed>; or `best-effort` + why

### Open questions
- none — ready

### Out of scope
- ...
```

Rules: ≤40 lines; no behavior restatement; every decision has a source; `os:ready` means no open questions. Durability over precision: a stable decision beats a precise guess. No spec: `spec=none`; the ticket is the only artifact. Only the user authorizes `ship` or a reviewer merger; never self-grant.

## Ticket

```markdown
## What to build
<vertical slice; user-observable>

## Acceptance criteria
- [ ] <traces to spec R#>

## Blocked by
- #...

## Verification
- `<command>` → <expected>

## Notes
<interfaces, decisions, constraints>
```

Rules: one context window; demoable; prefactor first; >7 tickets means split the spec. The ticket narrows the spec; when a spec exists, the author reads it for behavior.

## PR

```markdown
Issue: #N
Spec: #N (read at <ISO>) or none
Brief: <comment link>

## Why
<problem; link the issue>

## Decisions
- <choice> — spec §R2 / comment <link>

## What changed
<smallest reviewable view>

## Requirements
- R1: <how it is satisfied or tested>

## Evidence
<commands + output; surface run; verification: verified | best-effort | reverted>

## Scope
<deliberately excluded>

## Merge danger
Door: one-way | two-way
Blast radius: low | medium | high

## Verification
- [ ] test
- [ ] lint / typecheck / build
- [ ] surface (if behavior)
```

Rules: ≤40 lines; ready PR, not draft; evidence is observed, not predicted. No completion claims without fresh verification evidence.

## Review

```markdown
OS-REVIEW 1
Head: <sha>
Issue: #N
Spec: #N (or none)
Verdict: APPROVE | CHANGES | BLOCKED

## Spec compliance
- R1: pass | fail | not verifiable
- S1: pass | fail | not verifiable

## Findings
| ID | Severity | Location | Finding | Required fix |
|---|---|---|---|---|
| F1 | blocker | file:line | ... | ... |

## Evidence checked
- `<command>` → <result>
- Runtime: <observation>

## Out of scope
- ...
```

`APPROVE` only if no blocker/should and evidence covers the diff.

## Fix

```markdown
OS-FIX 1
Head: <sha>
Review: <review comment link>

| ID | Status | Evidence |
|---|---|---|
| F1 | fixed | `<command>` → <result> |
| F2 | declined | <reason> |
```

Rules: `declined` needs evidence, not preference. No push without a fresh verification run.
