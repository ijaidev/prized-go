---
name: os-setup
description: "Manual. Install or refresh OpenShip in this repo: AGENTS.md pointer, .openship references, config, labels. Run when the user asks to set up OpenShip, for example /os-setup."
---

# Setup

One install per repo. Idempotent. No runner, scheduler, queue, or issues.

## Preflight

- Confirm the repo root: `git rev-parse --show-toplevel`.
- Resolve `assets/` relative to this `SKILL.md`, not the current directory.
- Read an existing `.openship/config.json` and `AGENTS.md`. Never assume a fresh repo.
- Invalid `config.json` → stop and show the parse error; never overwrite it.
- Malformed `<!-- os:begin -->` / `<!-- os:end -->` markers → stop and show them.

## Plan

Build the full write plan before changing anything.

1. **References.** Target `.openship/{CONTRACT,ARTIFACTS,REVIEW,WATCH,DEBUG}.md`, `VERSION`.
   - Missing → copy from `assets/`.
   - Present and identical → skip.
   - Present and different → keep the local copy unless the user approves the diff.
2. **Pointer.** Render `assets/AGENTS.block.md`.
   - Markers `<!-- os:begin -->` and `<!-- os:end -->` exist → replace the block.
   - No markers → append one blank line, then the block.
   - Preserve every byte outside the markers.
   - Replace `<!-- os:version … -->` with the version from `assets/VERSION`.
3. **Config.** Merge `assets/config.default.json` into `.openship/config.json`: existing keys win, missing keys are added.
4. **Commands.** Detect from the repo and propose one line:
   - `package.json` scripts: `test`, `lint`, `build`
   - `Makefile` targets of the same names
   - `Cargo.toml`, `pyproject.toml`, `go.mod`, `justfile`
   - Leave `null` when unknown. Never invent a command.
5. **Labels.** Read `assets/labels.tsv` (tab-separated: name, color, description). When authenticated, compare with `gh label list`; create or update the set, never delete.

## Confirm

- No file changes and every label exists with the expected color and description → print `OpenShip is up to date (<version>).` and stop.
- Only missing files with no conflicts → apply; os-setup was explicitly invoked.
- A changed reference, a changed `AGENTS.md` pointer, or a config value that would change behavior → show one combined diff and ask once.
- Never ask per file.

## Apply

```bash
SKILL_DIR="<absolute dir of this SKILL.md>"
mkdir -p .openship
for f in CONTRACT.md ARTIFACTS.md REVIEW.md WATCH.md DEBUG.md; do
  [ -e ".openship/$f" ] || cp "$SKILL_DIR/assets/$f" ".openship/$f"
done
cp "$SKILL_DIR/assets/VERSION" .openship/VERSION
```

- Copy an existing reference only after the user approves its diff; otherwise leave it untouched.
- Write the merged `AGENTS.md` and `.openship/config.json`, with the detected commands in `config.json`. Unknown stays `null` and `discover`.
- Create labels:

```bash
while IFS=$'\t' read -r name color desc; do
  [ -z "$name" ] && continue
  gh label create "$name" --color "$color" --description "$desc" --force
done < "$SKILL_DIR/assets/labels.tsv"
```

- If `gh` is missing or unauthenticated: write all files, skip labels, print the loop above.
- Never delete labels that are not in the file. A repo may have its own labels.
- Never commit, push, open issues, install CI, or create a GitHub Action.

## Report

Print, in this order:

1. Files written / skipped.
2. Labels created / updated / skipped.
3. Detected commands, or `discover` where unknown.
4. Version.
5. One next command:
   - fuzzy idea → `/os-grill`
   - clear change → `/os-spec`
   - existing issue → `/os-triage`

Recommend committing `AGENTS.md`, `.openship/`, and `skills-lock.json`.

## Refresh

After `npx skills update`, run `/os-setup` again. The version stamp exposes a stale pointer. A locally edited reference is never clobbered without a diff.
