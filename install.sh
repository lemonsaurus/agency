#!/usr/bin/env bash
# install.sh — one-liner installer for agency
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/lemonsaurus/agency/main/install.sh | bash
#
# What it does:
#   1. Installs tmux build deps (libevent, ncurses, bison)
#   2. Installs tmux 3.5+ from source if missing or outdated
#   3. Installs Go 1.24+ if missing or outdated
#   4. Builds and installs agency from source
#
# Sudo is requested only for system-level installs (packages, /usr/local).
# Everything else runs as your user.
#
# Options (env vars):
#   INSTALL_DIR  — where to put binaries (default: ~/.local/bin)
#   SKIP_DEPS    — set to 1 to skip system dependency installation
#   REPO_DIR     — path to a local clone of the repo (skips git clone)

set -euo pipefail

# Wrap everything in main() so a partial download can't execute half a script.
main() {

REPO="lemonsaurus/agency"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"
SKIP_DEPS="${SKIP_DEPS:-0}"
REPO_DIR="${REPO_DIR:-}"

TMUX_VERSION="3.5a"
GO_VERSION="1.24.1"

# --- Colors & output ---

bold="\033[1m"
dim="\033[2m"
green="\033[32m"
red="\033[31m"
yellow="\033[33m"
cyan="\033[36m"
reset="\033[0m"

info()  { printf "${bold}${cyan}  ▸${reset} %s\n" "$*"; }
ok()    { printf "${bold}${green}  ✓${reset} %s\n" "$*"; }
warn()  { printf "${bold}${yellow}  !${reset} %s\n" "$*"; }
fail()  { printf "${bold}${red}  ✗${reset} %s\n" "$*" >&2; exit 1; }
step()  { printf "\n${bold}  [%s] %s${reset}\n\n" "$1" "$2"; }

# --- Platform detection ---

OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
ARCH="$(uname -m)"

case "$OS" in
    linux)  OS="linux" ;;
    darwin) OS="darwin" ;;
    *)      fail "Unsupported OS: $OS" ;;
esac

case "$ARCH" in
    x86_64|amd64)  ARCH="amd64" ;;
    aarch64|arm64) ARCH="arm64" ;;
    *)             fail "Unsupported architecture: $ARCH" ;;
esac

# --- Helpers ---

need_cmd() {
    command -v "$1" &>/dev/null
}

version_gte() {
    # Returns 0 if $1 >= $2 (semantic version comparison)
    printf '%s\n%s' "$2" "$1" | sort -V -C
}

get_tmux_version() {
    tmux -V 2>/dev/null | grep -oE '[0-9]+\.[0-9]+[a-z]?' || echo "0.0"
}

ensure_sudo() {
    # Prompt for sudo once upfront, so the user isn't surprised later.
    if ! sudo -v 2>/dev/null; then
        fail "sudo is required to install system packages. Run as a sudoer."
    fi
}

detect_pkg_manager() {
    if need_cmd apt-get; then
        echo "apt"
    elif need_cmd dnf; then
        echo "dnf"
    elif need_cmd yum; then
        echo "yum"
    elif need_cmd pacman; then
        echo "pacman"
    elif need_cmd brew; then
        echo "brew"
    else
        echo "unknown"
    fi
}

# --- Banner ---

printf "\n"
printf "${cyan}      _    ____ _____ _   _  ______   __${reset}\n"
printf "${cyan}     / \\  / ___| ____| \\ | |/ ___\\ \\ / /${reset}\n"
printf "${cyan}    / _ \\| |  _|  _| |  \\| | |    \\ V / ${reset}\n"
printf "${cyan}   / ___ \\ |_| | |___| |\\  | |___  | |  ${reset}\n"
printf "${cyan}  /_/   \\_\\____|_____|_| \\_|\\____| |_|  ${reset}\n"
printf "\n"
printf "${dim}  AI agent multiplexer for tmux${reset}\n"
printf "\n"

info "Platform: ${OS}/${ARCH}"

PKG_MANAGER="$(detect_pkg_manager)"
info "Package manager: ${PKG_MANAGER}"

# Validate sudo access once, before we start doing anything.
info "Some steps require sudo for system-level installs."
ensure_sudo

# ============================================================
# Step 1: System dependencies (for building tmux)
# ============================================================

install_tmux_deps() {
    step "1/4" "Installing tmux build dependencies"

    case "$PKG_MANAGER" in
        apt)
            sudo apt-get update -qq
            sudo apt-get install -y -qq \
                build-essential libevent-dev libncurses-dev bison pkg-config
            ;;
        dnf|yum)
            sudo "$PKG_MANAGER" install -y \
                gcc make libevent-devel ncurses-devel bison pkg-config
            ;;
        pacman)
            sudo pacman -S --noconfirm --needed \
                base-devel libevent ncurses bison pkg-config
            ;;
        brew)
            brew install libevent ncurses bison pkg-config
            ;;
        *)
            warn "Unknown package manager. Install these manually:"
            warn "  libevent, ncurses, bison, pkg-config, a C compiler"
            return 1
            ;;
    esac

    ok "Build dependencies installed"
}

# ============================================================
# Step 2: tmux 3.5+
# ============================================================

install_tmux() {
    step "2/4" "Checking tmux"

    local current
    current="$(get_tmux_version)"

    if need_cmd tmux && version_gte "$current" "3.5"; then
        ok "tmux ${current} already installed (>= 3.5)"
        return 0
    fi

    if [[ "$current" != "0.0" ]]; then
        info "tmux ${current} found, but >= 3.5 is required"
    else
        info "tmux not found"
    fi

    # On macOS, prefer brew
    if [[ "$OS" == "darwin" ]] && need_cmd brew; then
        info "Installing tmux via Homebrew..."
        brew install tmux
        ok "tmux installed via Homebrew"
        return 0
    fi

    # On Arch, pacman has recent tmux
    if [[ "$PKG_MANAGER" == "pacman" ]]; then
        info "Installing tmux via pacman..."
        sudo pacman -S --noconfirm tmux
        ok "tmux installed via pacman"
        return 0
    fi

    # Build from source
    info "Building tmux ${TMUX_VERSION} from source..."

    need_cmd make || fail "make is required to build tmux"
    need_cmd gcc || need_cmd cc || fail "A C compiler is required to build tmux"

    local tmpdir
    tmpdir="$(mktemp -d)"

    local tarball="tmux-${TMUX_VERSION}.tar.gz"
    local url="https://github.com/tmux/tmux/releases/download/${TMUX_VERSION}/${tarball}"

    info "Downloading ${tarball}..."
    curl -fsSL "$url" -o "${tmpdir}/${tarball}"

    info "Extracting..."
    tar -xzf "${tmpdir}/${tarball}" -C "$tmpdir"

    info "Configuring..."
    (cd "${tmpdir}/tmux-${TMUX_VERSION}" && ./configure --prefix=/usr/local >/dev/null 2>&1) \
        || fail "tmux configure failed. Are build dependencies installed?"

    info "Compiling (this may take a minute)..."
    (cd "${tmpdir}/tmux-${TMUX_VERSION}" && make -j"$(nproc 2>/dev/null || echo 2)" >/dev/null 2>&1) \
        || fail "tmux build failed"

    # Only sudo for the actual install into /usr/local
    info "Installing to /usr/local (sudo)..."
    (cd "${tmpdir}/tmux-${TMUX_VERSION}" && sudo make install >/dev/null 2>&1) \
        || fail "tmux install failed"

    rm -rf "$tmpdir"

    ok "tmux ${TMUX_VERSION} installed to /usr/local/bin/tmux"
}

# ============================================================
# Step 3: Go 1.24+
# ============================================================

install_go() {
    step "3/4" "Checking Go"

    # Check both PATH go and /usr/local/go
    local go_bin=""
    if need_cmd go; then
        go_bin="go"
    elif [[ -x /usr/local/go/bin/go ]]; then
        go_bin="/usr/local/go/bin/go"
        export PATH="/usr/local/go/bin:$PATH"
    fi

    if [[ -n "$go_bin" ]]; then
        local current
        current="$($go_bin version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+(\.[0-9]+)?' || echo "0.0")"
        if version_gte "$current" "1.24"; then
            ok "Go ${current} already installed (>= 1.24)"
            return 0
        fi
        info "Go ${current} found, but >= 1.24 is required"
    else
        info "Go not found"
    fi

    # On macOS, prefer brew
    if [[ "$OS" == "darwin" ]] && need_cmd brew; then
        info "Installing Go via Homebrew..."
        brew install go
        ok "Go installed via Homebrew"
        return 0
    fi

    # Download official binary
    local tarball="go${GO_VERSION}.${OS}-${ARCH}.tar.gz"
    local url="https://go.dev/dl/${tarball}"

    info "Downloading Go ${GO_VERSION}..."
    local tmpdir
    tmpdir="$(mktemp -d)"

    curl -fsSL "$url" -o "${tmpdir}/${tarball}"

    # Remove distro Go packages to avoid conflicts (e.g. old /usr/bin/go
    # wrapper that tries to download toolchains and hangs).
    case "$PKG_MANAGER" in
        apt)
            if dpkg -l golang-go &>/dev/null || dpkg -l golang &>/dev/null; then
                info "Removing old system Go packages to avoid conflicts (sudo)..."
                sudo apt-get remove -y -qq golang-go golang 2>/dev/null || true
                sudo apt-get autoremove -y -qq 2>/dev/null || true
            fi
            ;;
        dnf|yum)
            if rpm -q golang &>/dev/null; then
                info "Removing old system Go package to avoid conflicts (sudo)..."
                sudo "$PKG_MANAGER" remove -y golang 2>/dev/null || true
            fi
            ;;
        pacman)
            if pacman -Qi go &>/dev/null; then
                info "Removing old system Go package to avoid conflicts (sudo)..."
                sudo pacman -Rns --noconfirm go 2>/dev/null || true
            fi
            ;;
    esac

    # Only sudo for the extraction into /usr/local
    info "Installing to /usr/local/go (sudo)..."
    sudo rm -rf /usr/local/go
    sudo tar -C /usr/local -xzf "${tmpdir}/${tarball}"

    rm -rf "$tmpdir"

    export PATH="/usr/local/go/bin:$PATH"

    ok "Go ${GO_VERSION} installed to /usr/local/go"
}

# ============================================================
# Step 4: Build and install agency
# ============================================================

install_agency() {
    step "4/4" "Installing agency"

    local src tmpdir=""

    if [[ -n "$REPO_DIR" ]]; then
        src="$REPO_DIR"
        info "Using local repo at ${src}..."
    else
        tmpdir="$(mktemp -d)"
        need_cmd git || fail "git is required to clone the repository"
        info "Cloning ${REPO}..."
        git clone --depth 1 "https://github.com/${REPO}.git" "$tmpdir/agency" 2>/dev/null
        src="$tmpdir/agency"
    fi

    info "Building..."
    (cd "$src" && go build -o agency ./cmd/agency) \
        || fail "Build failed"

    mkdir -p "$INSTALL_DIR"
    cp "$src/agency" "$INSTALL_DIR/"
    chmod +x "$INSTALL_DIR/agency"
    cp "$src/scripts/agency-spawn" "$INSTALL_DIR/"
    chmod +x "$INSTALL_DIR/agency-spawn"

    ok "agency       → ${INSTALL_DIR}/agency"
    ok "agency-spawn → ${INSTALL_DIR}/agency-spawn"

    # Install claudejail-mac on macOS
    if [[ "$OS" == "darwin" ]]; then
        cp "$src/scripts/claudejail-mac" "$INSTALL_DIR/"
        chmod +x "$INSTALL_DIR/claudejail-mac"
        ok "claudejail-mac → ${INSTALL_DIR}/claudejail-mac"
        if ! need_cmd docker; then
            warn "Docker not found — claudejail-mac requires Docker Desktop"
            warn "  https://docs.docker.com/desktop/install/mac-install/"
        fi
    fi

    # Set up config if none exists
    local config_dir="$HOME/.config/agency"
    local config_file="$config_dir/config.toml"
    if [[ ! -f "$config_file" ]]; then
        mkdir -p "$config_dir"
        cp "$src/configs/default.toml" "$config_file"
        ok "config       → ${config_file}"
    else
        info "Config already exists at ${config_file}, skipping"
    fi

    [[ -n "$tmpdir" ]] && rm -rf "$tmpdir"
}

# ============================================================
# Run it
# ============================================================

if [[ "$SKIP_DEPS" != "1" ]]; then
    install_tmux_deps
fi
install_tmux
install_go
install_agency

# --- PATH fix ---

# Build a list of PATH entries the user might need.
paths_to_add=()

if [[ ":$PATH:" != *":$INSTALL_DIR:"* ]]; then
    paths_to_add+=("$INSTALL_DIR")
fi

if [[ -x /usr/local/go/bin/go ]] && [[ ":$PATH:" != *":/usr/local/go/bin:"* ]]; then
    paths_to_add+=("/usr/local/go/bin")
fi

if [[ ${#paths_to_add[@]} -gt 0 ]]; then
    printf "\n"

    # Build the export line
    path_addition=""
    for p in "${paths_to_add[@]}"; do
        path_addition+="${p}:"
    done
    export_line="export PATH=\"${path_addition}\$PATH\""

    # Detect which shell rc file to use
    user_shell="${SHELL:-/bin/bash}"
    case "$user_shell" in
        */zsh)  shell_rc="$HOME/.zshrc" ;;
        */bash) shell_rc="$HOME/.bashrc" ;;
        *)      shell_rc="$HOME/.profile" ;;
    esac

    warn "${INSTALL_DIR} is not in your PATH."
    printf "  ${dim}Add this to ${shell_rc}?${reset}\n"
    printf "  ${dim}${export_line}${reset}\n"
    printf "\n"
    printf "  Add to ${shell_rc}? [Y/n] "
    read -r reply </dev/tty
    reply="${reply:-Y}"

    if [[ "$reply" =~ ^[Yy]$ ]]; then
        printf "\n# Added by agency installer\n%s\n" "$export_line" >> "$shell_rc"
        ok "Added to ${shell_rc}"
        warn "Run ${dim}source ${shell_rc}${reset}${bold}${yellow} or open a new terminal to use agency."
    else
        info "Skipped. Add it manually when you're ready:"
        printf "  ${dim}${export_line}${reset}\n"
    fi
fi

printf "\n"
printf "  ${green}${bold}Done!${reset} Run ${cyan}${bold}agency${reset} to get started.\n"
printf "\n"

} # end main()

# This is the entry point. The script only executes once bash has
# parsed the entire file, so a truncated download does nothing.
main "$@"
