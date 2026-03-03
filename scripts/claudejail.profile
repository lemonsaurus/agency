# claudejail — Firejail profile for sandboxed Claude Code sessions
# Restricts filesystem access to CWD (passed via --whitelist by wrapper script)

# ---------------------------------------------------------------------------
# Filesystem: deny everything, then whitelist selectively
# ---------------------------------------------------------------------------

# Start with full home blackout — wrapper adds --whitelist=$PWD
blacklist ${HOME}

# Claude runtime (read-only)
noblacklist ${HOME}/.local/share/claude
read-only ${HOME}/.local/share/claude

# Claude config/state/memory (read-write — Claude writes here)
noblacklist ${HOME}/.claude
noblacklist ${HOME}/.local/bin/claude

# Node.js via nvm (read-only)
noblacklist ${HOME}/.nvm
read-only ${HOME}/.nvm

# Git config (read-only)
noblacklist ${HOME}/.gitconfig
read-only ${HOME}/.gitconfig
noblacklist ${HOME}/.config/git
read-only ${HOME}/.config/git

# Restrict /etc to networking, SSL, and identity essentials
private-etc alternatives,ca-certificates,crypto-policies,hostname,hosts,ld.so.cache,ld.so.conf,ld.so.conf.d,ld.so.preload,localtime,login.defs,nsswitch.conf,passwd,pki,resolv.conf,ssl

# Empty /tmp and /dev
private-tmp
private-dev

# ---------------------------------------------------------------------------
# Networking — Claude needs Anthropic API access
# ---------------------------------------------------------------------------
protocol unix,inet,inet6

# ---------------------------------------------------------------------------
# Security hardening
# ---------------------------------------------------------------------------

# Drop all Linux capabilities
caps.drop all

# Seccomp syscall filtering
seccomp

# Block privilege escalation
nonewprivs
noroot

# IPC namespace isolation
ipc-namespace

# Block D-Bus, sound, video, input
nodbus
nosound
novideo
no3d

# ---------------------------------------------------------------------------
# Deliberately omitted (would break Claude):
#   disable-exec.inc   — Claude execs subprocesses (git, npm, bash, etc.)
#   private-bin         — Claude invokes many different tools
#   shell none          — Bash tool requires a shell
#   memory-deny-write-execute — breaks Node.js JIT compiler
# ---------------------------------------------------------------------------
