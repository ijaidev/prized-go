---
name: os-review
description: "Manual. Review a PR against its spec with evidence and a machine-readable verdict. Run when the user asks to review a PR, for example /os-review."
---

# Review

Reviewer mode. Read-only except a granted merge. Review the head SHA, not the author's report.

## Preflight

- Read `.openship/CONTRACT.md`; absent → stop: `/os-setup` first.
- Read `.openship/REVIEW.md`, `.openship/ARTIFACTS.md`, `.openship/WATCH.md`, the PR, and its `Issue` / `Spec` / `Brief` links.
- Form findings from the spec and diff, not the PR narrative. Re-run one claim.
- Never review your own PR. If you authored it, stop.
- Claim the head: post `<!-- os:review-claim head=<sha> reviewer=<id> at=<ISO> -->`, replace the PR's `os:needs-review` with `os:in-review`, re-read. An earlier live claim on that head → stop.
- The issue stays `os:in-progress`; never change it.
- Fetch the spec, ticket, brief, and comments newer than `brief.at`.
- Missing links → search linked issues and commits. Still missing → quality-only review; state `Spec: none`.
- Diff > 400 changed lines or > 10 files, not mechanical → ask to split before line-by-line review. Security or data migration may proceed.

## Method

1. Pin the head SHA. Read the diff against the base.
2. **Spec axis**: every `R#` and `S#` → pass, fail, not verifiable. Cite evidence. Bugfix spec: current, expected, and unchanged behavior.
3. **Quality axis**: correctness, security, tests, scope, operability, gate integrity.
4. Re-run at least one claimed command. For behavior, drive the real surface once. Do not trust the PR's evidence.
5. Findings: `blocker`, `should`, `nit`. Each needs a concrete failure, spec violation, or exploit path. No invented findings.
6. Any weakened test, skip, timeout, snapshot, CI step, or suppression is a blocker.

## Verdict

Post one `OS-REVIEW` comment from `ARTIFACTS.md`, including:

```text
<!-- os:review head=<sha> verdict=approve|changes|blocked -->
```

- `APPROVE` — no blocker/should; evidence covers the diff.
- `CHANGES` — at least one blocker/should with a required fix.
- `BLOCKED` — cannot verify, missing artifact, or conflicting spec.

Only if the head you reviewed is still the latest: replace the PR's `os:in-review` by verdict — `APPROVE` → `os:approved`, `CHANGES` → `os:changes`, `BLOCKED` → `os:blocked`. If the head moved, restore `os:needs-review` and post the review for the SHA you read. If the repo requires a formal GitHub approval, submit `gh pr review --approve` only for `APPROVE`.

## After

- Post `OS-REVIEW` on every verdict, including `APPROVE` with zero findings. Never approve silently.
- If the brief names you merger and the verdict is `APPROVE`, run the merge gate now.
- Post the verdict and stop; a new head is a new review. To keep watching, load `/os-pr-watch as reviewer` — user request, or named merger waiting on checks.
- Never edit, push, or resolve threads. Merge only the head you approved.
- On merge, run the post-merge order in `WATCH.md`.
- Re-review only new heads and open IDs.
- A declined finding with evidence is a valid response; escalate only if the spec requires it.
- Nits never block. No scope expansion; follow-ups go to the issue.
