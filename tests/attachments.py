#!/usr/bin/env python3
"""Live image and local-file checks. Requires a0-vision, a0-qwen and a ChatGPT login."""
import json
import os
from pathlib import Path
import struct
import subprocess
import tempfile
import zlib


def png(path, left, right):
    width, height = 160, 80
    raw = b"".join(b"\0" + b"".join(bytes(left if x < width // 2 else right)
                                  for x in range(width)) for _ in range(height))

    def chunk(kind, data):
        return (struct.pack(">I", len(data)) + kind + data
                + struct.pack(">I", zlib.crc32(kind + data) & 0xffffffff))

    path.write_bytes(b"\x89PNG\r\n\x1a\n"
                     + chunk(b"IHDR", struct.pack(">IIBBBBB", width, height, 8, 2, 0, 0, 0))
                     + chunk(b"IDAT", zlib.compress(raw)) + chunk(b"IEND", b""))


def run(cwd, model, prompt, images=None, resume=None):
    command = ["codex", "exec"]
    if resume:
        command += ["resume", resume]
    command += ["--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox",
                "-m", model, "--json"]
    if images:
        command += ["--image", ",".join(images)]
    result = subprocess.run(command + ["--", prompt], cwd=cwd, stdin=subprocess.DEVNULL,
                            capture_output=True, text=True, timeout=180)
    assert result.returncode == 0, result.stdout + result.stderr
    events = [json.loads(line) for line in result.stdout.splitlines()]
    assert any(e["type"] == "turn.completed" for e in events), events
    messages = [e["item"]["text"] for e in events if e["type"] == "item.completed"
                and e["item"]["type"] == "agent_message"]
    thread_id = next(e["thread_id"] for e in events if e["type"] == "thread.started")
    return "\n".join(messages).lower(), thread_id, events


with tempfile.TemporaryDirectory(prefix="bridge-attachments-") as directory:
    root = Path(directory)
    png(root / "a.png", (255, 0, 0), (0, 0, 255))
    png(root / "b.png", (0, 180, 0), (255, 255, 0))
    (root / "note.txt").write_text("Marker: COBALT-482\nCount: 37\n")

    text, thread, _ = run(root, "a0-vision", "Name the image's two colors from left to right. Do not use tools.", ["a.png"])
    assert "red" in text and "blue" in text and text.index("red") < text.index("blue"), text
    print("PASS single PNG attachment", flush=True)
    for model in ("gpt-6-luna", "a0-vision"):
        text, _, _ = run(root, model, "What were the two colors in the image I attached? Give left-to-right order. Do not use tools.", resume=thread)
        assert "red" in text and "blue" in text, (model, text)
        print(f"PASS image history retained when switching to {model}", flush=True)

    text, _, _ = run(root, "a0-vision", "For image 1 and image 2, report the two colors in left-to-right order. Do not use tools.", ["a.png", "b.png"])
    positions = [text.index(color) for color in ("red", "blue", "green", "yellow")]
    assert positions == sorted(positions), text
    print("PASS multiple PNG attachments in order", flush=True)

    text, thread, _ = run(root, "a0-vision", "Use view_image to open b.png and report its colors left to right. Do not inspect image bytes with shell or Python.")
    assert "green" in text and "yellow" in text, text
    sessions = Path(os.environ.get("CODEX_HOME", str(Path.home() / ".codex"))) / "sessions"
    paths = list(sessions.rglob(f"*{thread}.jsonl"))
    assert paths, thread
    payloads = [json.loads(line).get("payload", {}) for line in paths[0].read_text().splitlines()]
    assert any(p.get("type") == "function_call" and p.get("name") == "view_image" for p in payloads)
    print("PASS view_image tool output reaches the vision model", flush=True)

    text, _, events = run(root, "a0-qwen", "Read @note.txt with a file-reading tool and report its marker and count.")
    assert "cobalt-482" in text and "37" in text, text
    assert any(e.get("item", {}).get("type") == "command_execution" for e in events), events
    print("PASS local text-file reference read through Codex tools", flush=True)
