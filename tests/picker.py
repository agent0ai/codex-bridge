#!/usr/bin/env python3
"""Real TUI regression: stale shared catalog, --no-daemon, then sync --force.

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
import struct
import subprocess
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

with tempfile.TemporaryDirectory(prefix="bridge-picker-") as tmp:
    root = Path(tmp)
    home, cwd, bin_dir = root / "home", root / "work", root / "bin"
    for directory in (home, cwd, bin_dir):
        directory.mkdir(mode=0o700)
    (bin_dir / "codex").symlink_to(Path(args.codex).resolve())
    env = dict(os.environ, CODEX_HOME=str(home), TERM="xterm-256color",
               PATH=str(bin_dir) + os.pathsep + os.environ["PATH"])
    for key in ("OPENAI_API_KEY", "CODEX_API_KEY"):
        env.pop(key, None)
    (home / "auth.json").write_text(json.dumps({"OPENAI_API_KEY": "test-not-a-real-key"}))
    (home / "config.toml").write_text(
        'model = "gpt-6-sol"\nmodel_provider = "offline"\ncli_auth_credentials_store = "file"\n'
        '[model_providers.offline]\nname = "Offline test"\n'
        'base_url = "http://127.0.0.1:1/v1"\nwire_api = "responses"\n'
        f'[projects."{cwd}"]\ntrust_level = "trusted"\n')
    state = home / "app-server-daemon"
    state.mkdir()
    (state / "settings.json").write_text(json.dumps({"updater": {"autoUpdateEnabled": False}}))
    bridge_config = root / "bridge.yaml"
    bridge_config.write_text("models:\n  - name: picker-test-model\n    model: offline\n"
                             "    api: chat\n    base_url: http://127.0.0.1:1/v1\n")

    def run(*command):
        return subprocess.run(command, env=env, cwd=cwd, check=True,
                              capture_output=True, text=True, timeout=120).stdout

    try:
        run("codex", "app-server", "daemon", "start")
        run(args.bridge, "--config", str(bridge_config), "sync")
        stale = picker(env, cwd)
        assert "Select Model and Effort" in stale and "picker-test-model" not in stale, stale
        print("PASS real TUI reproduces the stale catalog after normal sync", flush=True)
        fresh = picker(env, cwd, ["--no-daemon"])
        assert "1. picker-test-model" in fresh, fresh
        print("PASS --no-daemon shows the external model first", flush=True)
        run(args.bridge, "--config", str(bridge_config), "sync", "--force")
        restarted = picker(env, cwd)
        assert "1. picker-test-model" in restarted, restarted
        print("PASS sync --force refreshes the normal shared-server TUI picker", flush=True)
    finally:
        run("codex", "app-server", "daemon", "stop")
