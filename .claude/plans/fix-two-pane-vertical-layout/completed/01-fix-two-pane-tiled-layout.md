# Fix two-pane tiled layout

**Sequence:** 1 of 1
**Status:** completed
**Acceptance criteria:** A tiled window with two panes uses two columns.
**Depends on:** none
**Parallel-safe with:** none

## Goal

A tiled window containing two panes places them side by side.

## Approach

Keep the existing column distribution and custom tmux layout builder. Treat two panes as two single-pane columns and update the focused layout tests.

## Verification

- Inspect the generated two-pane distribution and custom layout expectations.
