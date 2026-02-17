# Session Context

## User Prompts

### Prompt 1

Implement the following plan:

# Plan: Fix annotated tag checkout in `checkoutRef`

## Context

OpenVox mode calls `checkoutRef()` for every tag. For **annotated tags** (like `v1.4.4`), the git reference points to a tag object, not a commit. `wt.Reset(&git.ResetOptions{Commit: ref.Hash()})` fails because it expects a commit hash:

```
reset v1.4.4: invalid reset option: object not found
```

Lightweight tags work fine because their ref hash IS the commit hash directly.

## Fix

**File: `pkg/sync...

### Prompt 2

commit this

