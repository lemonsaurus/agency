# Split two-pane layouts vertically

**Branch:** `fix/two-pane-vertical-layout`
**Created:** 2026-08-25

## Summary

Place two panes side by side in the tiled layout instead of stacking them.

## Acceptance Criteria

- [x] A tiled window with two panes uses two columns.

## Architectural Decisions

- **Layout model**: Keep using the existing column distribution and custom tmux layout builder.

## Commit Plan

1. Fix two-pane tiled layout — Return two single-pane columns and cover the behavior in layout tests.
