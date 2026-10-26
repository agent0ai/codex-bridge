#!/usr/bin/env python3
"""Native lifecycle and TUI regression: stale catalog, --no-daemon, sync --force.

Uses a temporary CODEX_HOME, dummy credentials, and no inference requests.
Requires Linux/macOS and a complete Codex 0.164.0 installation for daemon start.
"""
import argparse
import fcntl
import json
import os
from pathlib import Path
import pty
import re
import select
import shutil
import socket
import struct
import subprocess
import sys
import tempfile
import termios
import time


def picker(env, cwd, flags=()):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 45, 160, 0, 0))
    process = subprocess.Popen(["codex", "--no-alt-screen", *flags], env=env, cwd=cwd,
                               stdin=slave, stdout=slave, stderr=slave, start_new_session=True)
    os.close(slave)

    def read_until(needle, timeout=40):
        output = b""
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            if select.select([master], [], [], 0.1)[0]:
                chunk = os.read(master, 65536)
                output += chunk
                if b"\x1b[6n" in chunk:
                    os.write(master, b"\x1b[1;1R")
                clean = re.sub(rb"\x1b\[[0-?]*[ -/]*[@-~]", b"", output)
                if needle in clean:
                    return clean.decode(errors="replace")
            if process.poll() is not None:
                break
        raise AssertionError(f"TUI did not show {needle!r}: {output[-5000:]!r}")

    try:
        read_until(b"state=idle")
        os.write(master, b"/model")
        time.sleep(0.3)
        os.write(master, b"\r")
        return read_until(b"esc")
    finally:
        os.write(master, b"\x1b\x03\x03")
        try:
            process.wait(timeout=8)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)
        os.close(master)


parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--codex", default=shutil.which("codex"))
parser.add_argument("--bridge", default=shutil.which("codex-bridge"))
args = parser.parse_args()
assert args.codex and args.bridge, "codex and codex-bridge must be installed"
args.bridge = str(Path(args.bridge).resolve())

# Keep Codex's Unix socket path below the macOS length limit.
with tempfile.TemporaryDirectory(prefix="bridge-picker-", dir="/tmp") as tmp:
    root = Path(tmp)
    home, cwd, bin_dir = root / "User Home", root / "work", root / "bin"
    codex_home = root / "codex"
    for directory in (home, codex_home, cwd, bin_dir):
        directory.mkdir(mode=0o700)
    (bin_dir / "codex").symlink_to(Path(args.codex).resolve())
    env = dict(os.environ, HOME=str(home), CODEX_HOME=str(codex_home), TERM="xterm-256color",
               PATH=str(bin_dir) + os.pathsep + os.environ["PATH"])
    for key in ("OPENAI_API_KEY", "CODEX_API_KEY", "XDG_CONFIG_HOME"):
        env.pop(key, None)
    (codex_home / "auth.json").write_text(json.dumps({"OPENAI_API_KEY": "test-not-a-real-key"}))
    (codex_home / "config.toml").write_text(
        'model = "gpt-6-sol"\nmodel_provider = "offline"\ncli_auth_credentials_store = "file"\n'
        '[model_providers.offline]\nname = "Offline test"\n'
        'base_url = "http://127.0.0.1:1/v1"\nwire_api = "responses"\n'
        f'[projects."{cwd}"]\ntrust_level = "trusted"\n')
    state = codex_home / "app-server-daemon"
    state.mkdir()
    (state / "settings.json").write_text(json.dumps({"updater": {"autoUpdateEnabled": False}}))
    config_dir = home / ("Library/Application Support" if sys.platform == "darwin" else ".config")
    bridge_config = config_dir / "codex-bridge/config.yaml"
    editor = bin_dir / "test-editor"
    editor.write_text('#!/bin/sh\n[ -s "$1" ] || exit 42\nprintf "\\n# edited\\n" >> "$1"\n')
    editor.chmod(0o700)
    env.update(VISUAL="", EDITOR=str(editor))

    def run(*command):
        return subprocess.run(command, env=env, cwd=cwd, check=True,
                              capture_output=True, text=True, timeout=120).stdout

    def verify_status(expected):
        result = subprocess.run([args.bridge, "--config", str(bridge_config), "status"],
                                env=env, cwd=cwd, capture_output=True, text=True, timeout=20)
        lines = [line for line in result.stdout.splitlines() if line.startswith("Codex sync:")]
        assert lines == ["Codex sync: " + expected], (result.stdout, result.stderr)
        assert result.returncode == 0 and "Running at http://" in result.stdout, (result.stdout, result.stderr)
        print("PASS live status: " + expected, flush=True)

    print(run("codex", "--version").strip(), flush=True)
    run(args.bridge, "edit")
    assert bridge_config.read_text().endswith("# edited\n"), "edit missed the default config path"
    print("PASS default config and editor argument with spaces: " + str(bridge_config), flush=True)
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    bridge_config.write_text(f"listen: 127.0.0.1:{port}\nmodels:\n  - name: picker-test-model\n    model: offline\n"
                             "    api: chat\n    base_url: http://127.0.0.1:1/v1\n")
    try:
        assert "Started at" in run(args.bridge, "start")
        assert "Already running" in run(args.bridge, "start")
        print("PASS background start and repeated start", flush=True)
        run("codex", "app-server", "daemon", "start")
        run(args.bridge, "--config", str(bridge_config), "sync")
        verify_status("unsynced")
        stale = picker(env, cwd)
        assert "Select Model and Effort" in stale and "picker-test-model" not in stale, stale
        print("PASS real TUI reproduces the stale catalog after normal sync", flush=True)
        fresh = picker(env, cwd, ["--no-daemon"])
        assert "1. picker-test-model" in fresh, fresh
        print("PASS --no-daemon shows the external model first", flush=True)
        run(args.bridge, "--config", str(bridge_config), "sync", "--force")
        verify_status("synced")
        restarted = picker(env, cwd)
        assert "1. picker-test-model" in restarted, restarted
        print("PASS sync --force refreshes the normal shared-server TUI picker", flush=True)
        bridge_config.write_text(bridge_config.read_text() + "    vision: true\n")
        run(args.bridge, "--config", str(bridge_config), "sync")
        verify_status("unsynced")
        run(args.bridge, "--config", str(bridge_config), "sync", "--force")
        verify_status("synced")
        run("codex", "app-server", "daemon", "stop")
        verify_status("could not verify")
    finally:
        try:
            run("codex", "app-server", "daemon", "stop")
        finally:
            run(args.bridge, "stop")
    stopped = subprocess.run([args.bridge, "status"], env=env, cwd=cwd,
                             capture_output=True, text=True, timeout=20)
    assert stopped.returncode != 0 and "Stopped or unreachable" in stopped.stdout, (stopped.stdout, stopped.stderr)
    print("PASS background stop and stopped status", flush=True)
