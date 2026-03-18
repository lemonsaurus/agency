<div align="center">

<img src="assets/logo.png" alt="Agency logo" width="200" />

*a beautiful terminal multiplexer grid for color coded agent sessions*

![Go version](https://img.shields.io/badge/go-1.24+-00ADD8) ![tmux](https://img.shields.io/badge/tmux-3.5%2B-1BB91F) ![License](https://img.shields.io/badge/license-MIT-blue) 
</div>

---

https://github.com/user-attachments/assets/0ddf6f56-4c13-4603-a85e-6e95914c4184

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/lemonsaurus/agency/main/install.sh | bash
```

This handles everything — installs tmux 3.5+ (building from source if your distro's version is too old), Go 1.24+, and agency itself. Sudo is only used for system-level installs (`apt`, `/usr/local`); everything else runs as your user.

You'll also need at least one agent CLI: `claude`, `codex`, `gemini`, or any command you like.

<details>
<summary>Manual install</summary>

#### Requirements

- **Go 1.24+**
- **tmux 3.5+** (most distros ship an older version — Ubuntu 24.04 has 3.2a)
- `socat` or `nc` for the agent self-spawn script

#### Building tmux 3.5+ from source

```bash
sudo apt install -y build-essential libevent-dev libncurses-dev \
  autoconf automake pkg-config bison

TMUX_VERSION=3.5a
curl -sL "https://github.com/tmux/tmux/releases/download/${TMUX_VERSION}/tmux-${TMUX_VERSION}.tar.gz" \
  | tar xz
cd tmux-${TMUX_VERSION}
./configure && make -j$(nproc) && sudo make install
tmux -V   # should show 3.5a or newer
```

> If `tmux -V` still reports the old version, make sure `/usr/local/bin` comes before `/usr/bin` in your `$PATH`.

After upgrading, kill any running tmux server so it picks up the new binary: `tmux kill-server`

#### Building agency

```bash
git clone https://github.com/lemonsaurus/agency
cd agency
make install          # builds and copies to ~/.local/bin/
```

</details>

---

## What is this?

Agency turns your terminal into a Bloomberg-style multi-terminal workstation. Every pane stays on screen in a tiled grid — no tabs, no alt-tabbing, no context switching. Designed for giant ultrawide monitor nerds who are trying to juggle and keep track of 10+ terminal sessions.

Each pane gets a unique color and a bold `agent@folder` label in its border, so you always know what's running where. Borders dynamically switch color to match the label. Each label has an icon. Hotkeys to start claude, codex and gemini sessions.

<sub>PS: This is not really appropriate for a small monitor - for that use case, maybe check out [agent deck](https://github.com/asheshgoplani/agent-deck).</sub>

---

## Features

- **Everything on screen at once** — tiled grid layout, auto-rebalances on every spawn
- **Per-pane color labels** — every pane gets a unique color and a `claudejail@myapp` style border label
- **Directory-aware spawning** — `agency spawn claude ~/projects/api ~/projects/frontend` opens one pane per directory
- **Glob support** — `agency spawn claude ~/projects/client-*` expands via your shell
- **Agent self-spawning** — agents call `agency-spawn claude` from inside a pane to request new sibling panes
- **Spawn dialog** — `Prefix+2/3/4/5` opens a directory picker pre-filled with the current pane's path
- **Command palette** — `Prefix+c` fuzzy-searches all configured agent types
- **Crash recovery** — if agency restarts, it re-adopts existing tmux panes automatically
- **Isolated tmux config** — agency manages its own `tmux.conf`, never touches your `~/.tmux.conf`
- **Catppuccin Mocha theme** — inline, no plugin dependencies

<img width="2280" height="1045" alt="image" src="https://github.com/user-attachments/assets/23e60f00-ea89-4b4f-920a-54a326983a20" />


---

## Quick start

```bash
agency                            # launch (creates a new tmux session, or reattaches)
```

Inside the session, use `Prefix+c` (`Ctrl+Space, c`) to open the command palette and pick an agent type. Or use the number keys:

| Shortcut | Action |
|---|---|
| `Prefix+1` | New plain terminal (in current pane's directory) |
| `Prefix+2` | Spawn claudejail (opens directory picker) |
| `Prefix+3` | Spawn claude |
| `Prefix+4` | Spawn codex |
| `Prefix+5` | Spawn gemini |
| `Prefix+c` | Command palette (all agent types) |

---

## CLI reference

```
agency                              Launch session (or reattach if one exists)
agency spawn <agent> [dir...]       Spawn one pane per directory
agency spawn --cmd "htop" [dir]     Spawn an arbitrary command
agency kill <pane-id>               Kill a specific pane
agency kill-all                     Kill all managed panes
agency list                         List all panes
agency layout <name>                Switch layout (tiled, columns, rows, main-vertical)
agency attach                       Reattach to a running session
agency config                       Print resolved config
agency logs                         Print path to the log file (tail -f it)
agency help                         Show help
```

You can also spawn multiple agents across many directories in one shot:

```bash
# One claude pane per client directory
agency spawn claude ~/projects/client-*/

# Three codex panes, explicit paths
agency spawn codex ~/api ~/frontend ~/infra
```

---

## Keyboard shortcuts

The tmux prefix is **`Ctrl+Space`**.

### Spawning

| Shortcut | Action |
|---|---|
| `Prefix+c` | Command palette |
| `Prefix+1` | New terminal |
| `Prefix+2–5` | Spawn agent (opens directory picker) |

### Navigation

| Shortcut | Action |
|---|---|
| `Prefix+Arrow` | Move focus between panes |
| `Prefix+Shift+Arrow` | Resize pane |
| Click | Focus pane (mouse enabled) |
| Scroll | Scroll pane history |

### Layout

| Shortcut | Action |
|---|---|
| `Prefix+=` | Tiled grid (even distribution) |
| `Prefix+\|` | All columns (ultrawide mode) |
| `Prefix+-` | All rows stacked |
| `Prefix+m` | Main pane left, stacked right |
| `Prefix+Space` | Cycle through layouts |

### Management

| Shortcut | Action |
|---|---|
| `Prefix+x` | Kill focused pane (with confirmation) |
| `Prefix+q` | Kill session (Enter or y to confirm) |
| `Prefix+f` | Zoom/unzoom focused pane |
| `Prefix+b` | Broadcast — type in all panes at once |
| `Prefix+r` | Respawn dead pane |
| `Prefix+d` | Detach (session keeps running) |

---

## How agent self-spawning works

When agency launches it starts a unix socket server at `/tmp/agency-{session}.sock` and exports `AGENCY_SOCKET` into every pane's environment.

The `agency-spawn` script (installed to `~/.local/bin/`) is a tiny wrapper agents can call:

```bash
agency-spawn claude                      # spawn a claude pane in the current directory
agency-spawn claude --dir ~/projects/api # spawn in a specific directory
agency-spawn --cmd "aider --yes"         # spawn an arbitrary command
```

A Claude Code agent given instructions like *"when you need to work on the backend, run `agency-spawn claude --dir ~/api`"* will request a new sibling pane over the socket. Agency receives the message, spawns the pane, and re-tiles the grid — all without leaving the terminal.

The protocol is plain text over the unix socket:

```
spawn:claude@/home/user/projects/api    → spawn agent pane in that directory
spawn:cmd:htop                          → spawn arbitrary command
kill:%3                                 → kill pane %3
layout:tiled                            → switch layout
```

---

## Config

Agency looks for `~/.config/agency/config.toml`. If it doesn't exist, built-in defaults are used. Copy `configs/default.toml` as a starting point:

```bash
mkdir -p ~/.config/agency
cp configs/default.toml ~/.config/agency/config.toml
```

```toml
[session]
name = "agency"
default_layout = "tiled"         # tiled | columns | rows | main-vertical

[theme]
active_border = "#89b4fa"
inactive_border = "#45475a"
status_bg = "#181825"
status_fg = "#cdd6f4"

[agents.claudejail]
command = "claudejail"
icon = "🔒"
border_color = "#f38ba8"

[agents.claude]
command = "claude"
icon = "🤖"
border_color = "#cba6f7"

# Add your own agent types:
# [agents.aider]
# command = "aider --model ollama_chat/gemma3"
# icon = "🔧"
# border_color = "#a6e3a1"
```

Agents are assigned to the number keys (`Prefix+2` through `Prefix+5`) in the order they appear in the config file.

---

## claudejail — sandboxed Claude Code

Agency ships two sandbox wrappers — one for Linux, one for macOS.

### claudejail (Linux)

Runs Claude Code inside a [Firejail](https://firejail.wordpress.com/) sandbox. Restricts Claude's filesystem access to **only the current working directory**.

What the sandbox does:

- **Blacklists your entire `$HOME`**, then whitelists only `$PWD`, `~/.claude`, and the `claude` binary
- **Drops all Linux capabilities**, enables seccomp filtering
- **Blocks privilege escalation** (`nonewprivs`, `noroot`)
- **Isolates IPC, /tmp, and /dev**
- **Blocks D-Bus, sound, video, 3D**
- **Restricts `/etc`** to only networking and SSL essentials
- **Allows network access** (Claude needs the Anthropic API)
- **Allows subprocess execution** (Claude needs git, npm, bash, etc.)

```bash
# Install firejail if you don't have it
sudo apt install firejail

# Install the claudejail script and firejail profile
make install-claudejail
```

This copies two files:

- `~/.local/bin/claudejail` — the wrapper script
- `~/.config/firejail/claudejail.profile` — the sandbox profile

### claudejail-mac (macOS)

Runs Claude Code inside a Docker container. Firejail is Linux-only, so this is the macOS equivalent. Requires [Docker Desktop](https://docs.docker.com/desktop/install/mac-install/).

What the sandbox does:

- **Mounts only `$PWD`** into the container — Claude cannot see the rest of your home directory
- **Mounts `~/.claude`** so Claude retains its config, memory, and auth across sessions
- **Allows network access** (Claude needs the Anthropic API)
- **Allows subprocess execution** (Claude needs git, npm, bash, etc.)
- **Uses permissive Claude settings** so long unattended sessions don't stall waiting for permission prompts

The Docker image is built automatically on first run (~1 min).

```bash
make install-claudejail-mac
```

This copies one file:

- `~/.local/bin/claudejail-mac` — the wrapper script

The one-liner installer (`curl ... | bash`) handles this automatically on macOS.

---

Then just use either wrapper anywhere you'd use `claude`:

```bash
cd ~/projects/myapp
claudejail                    # Linux
claudejail-mac                # macOS
```

Inside agency, `claudejail` is the default `Prefix+2` agent (Linux) and `claudejail-mac` is available on macOS. The source files live in `scripts/claudejail`, `scripts/claudejail.profile`, and `scripts/claudejail-mac`.

---

## Pane labels

Each pane gets a top-border label in the format `agent@folder`:

```
 🔒 claudejail@api   🤖 claude@frontend   🧠 codex@infra
```

- The label background color is unique per pane, cycling through a 12-color palette
- Plain terminal panes (spawned with `Prefix+1`) show `zsh@currentfolder` and update live as you `cd`
- Labels are stored as tmux pane options (`@agency_label`) so they survive application title changes

---

## Troubleshooting

**Pane borders are all the same color**
You need tmux 3.4+ for per-pane `pane-border-style`. On older versions the colored badge in the top border status bar still shows per-pane colors; only the border lines themselves won't differ. See [Installing tmux 3.5+](#installing-tmux-35) above.

**`agency spawn` types into the current pane instead of opening a new one**
This means agency isn't running (`AGENCY_SOCKET` isn't set or the socket server isn't listening). Start a session first with `agency`, then spawn from a pane inside it.

**Command palette / spawn dialog doesn't appear**
`display-popup` requires tmux 3.2+. Also verify `agency` is in your `$PATH` — the keybindings call it by name.

**Check the logs**
```bash
tail -f $(agency logs)
```

---

## Development

```bash
make build      # compile
make install    # build + install to ~/.local/bin/
make uninstall  # remove all installed binaries
go test ./...   # run tests
go vet ./...    # static analysis
```

To test the installer against your local checkout instead of cloning from GitHub:

```bash
REPO_DIR=$(pwd) bash install.sh
```

See `CLAUDE.md` for the full architecture spec and development guidelines.
