# CLAUDE.md — Agency

## What is this?

A terminal-native TUI app (Go) that manages multiple AI coding agent sessions inside tmux. It gives you a Bloomberg-terminal-style tiled view of all your agents working simultaneously on a big screen.

## Core philosophy

- **Everything visible at once.** No tabs, no switching. Every agent pane is on screen in a tiled grid.
- **Terminal-native.** No web UI, no Electron. Lives in tmux, operated from the keyboard.
- **Agent-agnostic.** Supports Claude Code, Codex CLI, Gemini CLI, and any arbitrary command.
- **Agents can self-spawn.** An agent running inside a pane can request new panes via a unix socket.
- **Keep it simple.** No animations, no over-engineering. Just fast, reliable pane management.

## Architecture

```
┌───────────────────────────────────────────────────────┐
│ tmux session managed by agency                        │
│ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐ │
│ │ Claude 1 │ │ Codex 1  │ │ Gemini 1 │ │ Claude 2 │ │
│ │          │ │          │ │          │ │          │ │
│ └──────────┘ └──────────┘ └──────────┘ └──────────┘ │
│ ┌──────────┐ ┌──────────┐                            │
│ │ Claude 3 │ │ Custom   │    (grid auto-tiles)       │
│ │          │ │          │                            │
│ └──────────┘ └──────────┘                            │
│ [status bar: session | 6 agents | 14:32]             │
└───────────────────────────────────────────────────────┘

agency binary
  ├── manages tmux session lifecycle
  ├── spawns/kills panes with agent processes
  ├── applies tiled layout on every change
  ├── sets pane borders with agent type + status
  ├── listens on unix socket for spawn requests
  └── provides a command palette (tmux popup)
```

## Tech stack

- **Language:** Go
- **TUI:** Bubble Tea (charmbracelet/bubbletea) — only for the command palette popup
- **tmux interaction:** Shell out to `tmux` CLI (not libtmux)
- **Config:** TOML at `~/.config/agency/config.toml`
- **IPC:** Unix socket at `/tmp/agency-{session}.sock`

## Project structure

```
agency/
├── cmd/
│   └── agency/
│       └── main.go              # Entry point, CLI parsing
├── internal/
│   ├── config/
│   │   └── config.go            # TOML config loading + defaults
│   ├── tmux/
│   │   └── tmux.go              # All tmux CLI interactions
│   ├── session/
│   │   └── session.go           # Session + pane state management
│   ├── agents/
│   │   └── agents.go            # Agent type registry + detection
│   ├── palette/
│   │   └── palette.go           # Bubble Tea command palette
│   ├── ipc/
│   │   └── socket.go            # Unix socket server for agent spawns
│   └── status/
│       └── detect.go            # Agent status detection (running/waiting/done)
├── configs/
│   └── default.toml             # Default config with agent definitions
├── scripts/
│   └── agency-spawn             # Small script agents can call to request a pane
├── go.mod
├── go.sum
├── Makefile
├── README.md
└── CLAUDE.md                    # This file
```

## Config format

```toml
[session]
name = "agency"
default_layout = "tiled"         # tiled | columns | rows | main-vertical

[theme]
active_border = "#89b4fa"
inactive_border = "#45475a"
status_bg = "#181825"
status_fg = "#cdd6f4"

[agents.claude]
command = "claude"
icon = "🤖"
border_color = "#cba6f7"

[agents.codex]
command = "codex"
icon = "🧠"
border_color = "#89b4fa"

[agents.gemini]
command = "gemini"
icon = "✦"
border_color = "#f9e2af"

# Users can add arbitrary agent types:
# [agents.aider]
# command = "aider --model ollama_chat/gemma3"
# icon = "🔧"
# border_color = "#a6e3a1"
```

## CLI commands

```
agency                        Launch new session (or reattach if exists)
agency spawn claude           Spawn a Claude pane
agency spawn codex            Spawn a Codex pane
agency spawn gemini           Spawn a Gemini pane
agency spawn --cmd "aider"    Spawn arbitrary command
agency kill <pane-id>         Kill a specific pane
agency kill-all               Kill all agent panes
agency list                   List all panes with status
agency layout tiled           Switch layout
agency layout columns         Switch layout
agency attach                 Reattach to existing session
agency config                 Print resolved config
```

## Keyboard shortcuts (inside tmux)

These are the most important part. The prefix is Ctrl+Space.

### Spawning agents (the critical feature)

```
Prefix + c         → Open command palette (fuzzy list of agent types to spawn)
Prefix + 1         → Quick-spawn Claude Code
Prefix + 2         → Quick-spawn Codex
Prefix + 3         → Quick-spawn Gemini
Prefix + 4         → Quick-spawn from config slot 4 (user-defined)
```

When a new pane spawns, the layout automatically re-tiles.

### Navigation

```
Prefix + Arrow     → Move focus between panes
Prefix + S-Arrow   → Resize panes
Click              → Focus pane (mouse enabled)
Scroll             → Scroll pane history
```

### Layout

```
Prefix + =         → Tiled grid (even distribution)
Prefix + |         → All columns side by side (ultrawide mode)
Prefix + -         → All rows stacked
Prefix + m         → Main pane left + stacked right
Prefix + Space     → Cycle through layouts
```

### Management

```
Prefix + x         → Kill focused pane (with confirmation)
Prefix + f         → Zoom/unzoom focused pane (fullscreen toggle)
Prefix + b         → Broadcast mode (type in ALL panes at once)
Prefix + r         → Respawn dead pane with same agent
Prefix + d         → Detach (session keeps running)
```

## How the command palette works

`Prefix + c` triggers `tmux display-popup` which runs `agency palette`. This opens a Bubble Tea TUI inside a floating tmux popup. It shows a fuzzy-searchable list:

```
┌─ Spawn Agent ──────────────────┐
│ > _                            │
│                                │
│   🤖  Claude Code              │
│   🧠  Codex                    │
│   ✦   Gemini CLI               │
│   🔧  aider                    │
│   ⚡  Custom command...        │
│                                │
│   ↑↓ navigate  Enter spawn     │
│   Esc cancel                   │
└────────────────────────────────┘
```

Selecting "Custom command..." prompts for a command string. The palette calls `agency spawn <agent>` which handles the tmux pane creation.

## How agent self-spawning works

When agency launches, it starts a unix socket server at `/tmp/agency-{session}.sock`. It also sets the env var `AGENCY_SOCKET` in every spawned pane so agents know where to reach it.

The `scripts/agency-spawn` script is a tiny bash wrapper:

```bash
#!/usr/bin/env bash
# Usage: agency-spawn claude
# Usage: agency-spawn --cmd "my-custom-thing"
# Called BY an agent running inside an agency pane.
echo "spawn:${1}" | socat - UNIX-CONNECT:"$AGENCY_SOCKET"
```

An agent (like Claude Code) can be instructed to run `agency-spawn claude` to request a new sibling pane. The socket server receives the message, spawns the pane, and re-tiles.

The protocol is dead simple — newline-delimited messages:
- `spawn:claude` → spawn a claude pane
- `spawn:codex` → spawn a codex pane
- `spawn:cmd:aider --yes` → spawn arbitrary command
- `kill:3` → kill pane 3
- `layout:tiled` → switch layout

## Pane border labels

Each pane gets a top border label via tmux's `pane-border-format`. Format:

```
 🤖 Claude #1 — ~/projects/myapp [running]
```

Components:
- Agent icon (from config)
- Agent type + instance number
- Working directory (from `pane_current_path`)
- Status: `running` / `waiting` / `idle` / `done`

Status detection approach:
- Poll `tmux capture-pane` output periodically (every 2s)
- Match against known patterns per agent type:
  - Claude: look for `❯` prompt (waiting), tool use output (running)
  - Codex: similar heuristics
  - Gemini: similar heuristics
- Keep it simple and pattern-based. Don't over-engineer this.

## tmux config

Agency should generate/manage its own tmux config and apply it when creating sessions. It should NOT touch the user's `~/.tmux.conf`. Instead:

1. Write an agency-specific config to `~/.config/agency/tmux.conf`
2. Launch tmux with: `tmux -f ~/.config/agency/tmux.conf new-session -s agency`

This keeps agency's tmux settings isolated from the user's normal tmux usage.

The generated tmux.conf should include:
- Catppuccin Mocha colors (inline, no plugin dependency)
- All the keybindings listed above
- `pane-border-status top` with the label format
- Mouse support on
- Large scrollback (50000)
- `mode-keys emacs` (NO vim bindings anywhere)

The number key spawn bindings (Prefix+1/2/3) should be implemented as tmux `bind-key` that calls `agency spawn <type>`.

## Status bar

Bottom status bar showing:
- Left: session name in a colored pill
- Center: (empty, or window list if multiple windows)
- Right: total pane count, agent breakdown (e.g. "3🤖 2🧠 1✦"), current time

## Lifecycle & cleanup

### Signal handling

- Catch `SIGTERM` and `SIGINT`. Ignore `SIGHUP`.
- On signal: stop socket listener → stop status poller → remove socket file → remove lock file → exit 0.
- Do **NOT** kill agent panes — tmux owns them, they keep running.
- Double Ctrl+C pattern: second signal forces immediate `exit(1)`.
- Use `signal.NotifyContext` for clean context-based cancellation.

### Single instance prevention

- Lock file at `/tmp/agency-{session}.lock`.
- Use `syscall.Flock` with `LOCK_EX | LOCK_NB` — atomic, crash-safe.
- On lock failure: print message suggesting `agency attach`, exit 1.
- OS releases flock automatically on crash (no stale lock problem).
- Stale lock files left behind on crash are harmless — next flock succeeds.

### Orphan pane handling (crash recovery)

- On startup, check if tmux session already exists.
- If yes: scan panes with `tmux list-panes`, rebuild internal state from tmux.
- Detect agent type by matching running command against agent registry.
- Dead panes with exited processes labeled "unknown".
- tmux is the source of truth — agency is stateless and crash-recoverable.
- No state persisted to disk, everything reconstructed from tmux.

### Graceful shutdown sequence

Ordered steps, total < 100ms:

1. Close socket listener.
2. Cancel status poller.
3. Remove socket file.
4. Remove lock file.
5. Log + exit 0.

### Startup sequence

1. Load config (or defaults).
2. Determine session name.
3. Acquire lock file — exit if locked.
4. Check tmux session: adopt orphans if exists, create new if not.
5. Start unix socket server.
6. Set `AGENCY_SOCKET` in all panes.
7. Start status poller.
8. Attach to tmux session (if interactive).
9. Wait for shutdown signal.

## Build & install

```makefile
build:
	go build -o bin/agency ./cmd/agency

install: build
	cp bin/agency ~/.local/bin/
	cp scripts/agency-spawn ~/.local/bin/

run: build
	./bin/agency
```

## What NOT to build

- No web UI
- No animations
- No plugin system
- No multi-session support (one session at a time is fine)
- No agent-to-agent communication (out of scope)
- No token/cost tracking (out of scope)
- No git worktree management (out of scope — just panes and processes)

## Development workflow

This project uses Go modules. Run `go mod tidy` after adding dependencies.

Key dependencies:
- `github.com/charmbracelet/bubbletea` — command palette TUI
- `github.com/charmbracelet/lipgloss` — styled terminal output
- `github.com/pelletier/go-toml/v2` — config parsing
- No other heavy dependencies. Keep it lean.

Testing: focus on the `internal/tmux` package having good tests for command construction. The rest is mostly integration.
