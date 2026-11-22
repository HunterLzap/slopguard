---
name: slopguard
description: Run SlopGuard code checks before handing off repository changes and review recurring-error proposals when requested.
---

# SlopGuard

Before handing off code changes, call `sg_verify` with the absolute project path
and `scope: changed`. Use `scope: all` for a requested full scan or a project
without a Git worktree.

- On `FAIL`, resolve diagnostics within the task scope, then run verification again.
- On `MISSING_TOOL`, report the missing checker and its installation guidance.
- On `UNSUPPORTED` or `NO_CHANGES`, report the scan outcome as returned.
- On `PASS`, use the executed checkers and scanned file count as evidence.

Use `sg_status` for the last scan and pending proposal count. Use
`sg_list_proposals` when the user asks to review proposals. Explain the proposed
rule or hint and share the sandbox evidence where available.

Call `sg_apply_proposal` with `confirm: true` only after the user approves that
proposal. Use `sg_dismiss_proposal` with the mode the user requests.
