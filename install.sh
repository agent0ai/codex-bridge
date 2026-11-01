#!/bin/sh
set -eu

main() {
    case "$(uname -s)" in
        Linux) install_os=linux ;;
        Darwin) install_os=darwin ;;
        *) echo "codex-bridge: only Linux and macOS are supported" >&2; exit 1 ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) install_arch=amd64 ;;
        aarch64|arm64) install_arch=arm64 ;;
        *) echo "codex-bridge: only amd64 and arm64 are supported" >&2; exit 1 ;;
    esac
    if [ "$#" -gt 1 ]; then
        echo "Usage: install.sh [binary-path]" >&2
        exit 1
    fi
    install_path=${1:-"$HOME/.local/bin/codex-bridge"}
    case "$install_path" in
        /*) ;;
        *) install_path="$(pwd)/$install_path" ;;
    esac
    if [ -d "$install_path" ]; then
        echo "codex-bridge: destination must be a file: $install_path" >&2
        exit 1
    fi
    install_dir=$(dirname "$install_path")
    mkdir -p "$install_dir"
    install_tmp=$(mktemp -d "$install_dir/.codex-bridge-install.XXXXXX")
    trap 'rm -rf "$install_tmp"' 0
    trap 'exit 1' HUP INT TERM
    curl -fsSL --connect-timeout 15 --max-time 300 \
        "https://github.com/agent0ai/codex-bridge/releases/latest/download/codex-bridge-$install_os-$install_arch" \
        -o "$install_tmp/codex-bridge"
    chmod 755 "$install_tmp/codex-bridge"
    install_version=$("$install_tmp/codex-bridge" --version)
    case "$install_version" in
        "codex-bridge "*) ;;
        *) echo "codex-bridge: downloaded file did not report a valid version" >&2; exit 1 ;;
    esac
    # Rename on the same filesystem so a failed download cannot damage a running binary.
    mv -f "$install_tmp/codex-bridge" "$install_path"
    printf 'Installed %s at %s\n' "$install_version" "$install_path"
    case ":$PATH:" in
        *:"$install_dir":*) ;;
        *) printf 'Add %s to your PATH to run codex-bridge.\n' "$install_dir" ;;
    esac
}

main "$@"
