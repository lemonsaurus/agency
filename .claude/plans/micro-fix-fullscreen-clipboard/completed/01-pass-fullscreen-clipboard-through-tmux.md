# Pass Fullscreen Clipboard Through tmux

**Sequence:** 1 of 1
**Status:** complete
**Acceptance criteria:** Fullscreen Pi drag selection reaches the Windows clipboard.
**Depends on:** none
**Parallel-safe with:** none

## Goal

Applications inside Agency can write the outer terminal clipboard with OSC 52, and right-click still opens tmux's pane menu.

## Approach

Enable tmux application clipboard writes, open the full pane menu at the mouse position, and source generated settings when adopting an existing session. Cover the generated directives and reload command in focused tmux tests.

## Verification

- Install the test build and start a fresh Agency session.
- Drag-select text in fullscreen Pi and paste it into another Windows application.
- Right-click the fullscreen Pi pane and confirm tmux's pane menu opens.
