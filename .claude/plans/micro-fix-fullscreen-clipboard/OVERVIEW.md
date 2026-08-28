# Pass fullscreen clipboard writes through Agency

**Linear:** none
**Branch:** `lemon/micro-fix/fullscreen-clipboard`
**Created:** 2026-08-28

## Summary

Allow trusted applications inside Agency's tmux session to copy through OSC 52. Keep Agency's pane menu available while Pi fullscreen captures mouse input.

## Acceptance Criteria

- [x] Dragging a selection in fullscreen Pi copies it to the Windows clipboard through Agency.
- [x] Right-clicking a fullscreen Pi pane opens tmux's pane menu.

## Architectural Decisions

- **Clipboard path**: Use tmux's `set-clipboard on` mode for application-originated OSC 52 writes.
- **Config lifecycle**: Source the generated config when Agency adopts an existing tmux session.
- **Pattern to follow**: Keep clipboard behavior in Agency's generated tmux config and focused tmux tests.

## Commit Plan

1. Pass application clipboard writes through tmux.

## Notes

This allows trusted pane applications to set the system clipboard.
