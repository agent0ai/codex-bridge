#!/usr/bin/env python3
"""Measure real upstream cache usage during consecutive Codex turns, without logging prompts/keys."""
import argparse
import hashlib
import http.server
import json
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import time
import tomllib
import urllib.error
import urllib.request
import yaml

parser = argparse.ArgumentParser()
parser.add_argument("--model", default="a0-qwen")
parser.add_argument("--config", type=Path, default=Path.home() / ".config/codex-bridge/config.yaml")
parser.add_argument("--report", type=Path)
args = parser.parse_args()
source = args.config.read_text()
config = tomllib.loads(source) if args.config.suffix == ".toml" else yaml.safe_load(source)
model = next(m for m in config["models"] if m["name"] == args.model)
assert model["api"] == "chat", "This test measures the Chat Completions adapter."
upstream = model["base_url"].rstrip("/")
records = []


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


class Tap(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        raw = self.rfile.read(int(self.headers["Content-Length"]))
        record = {"body": json.loads(raw), "usage": None}
        records.append(record)
        endpoint = upstream if upstream.endswith("/chat/completions") else upstream + "/chat/completions"
        request = urllib.request.Request(endpoint, data=raw, headers={
            "Authorization": self.headers.get("Authorization", ""),
            "Content-Type": "application/json", "Accept": "text/event-stream"})
        try:
            with urllib.request.build_opener(NoRedirect).open(request, timeout=150) as response:
                self.send_response(response.status)
                self.send_header("Content-Type", response.headers["Content-Type"])
                self.end_headers()
                pending = b""
                while chunk := response.read1(65536):
                    pending += chunk
                    while b"\n" in pending:
                        line, pending = pending.split(b"\n", 1)
                        if line.startswith(b"data:") and line[5:].strip() != b"[DONE]":
                            payload = json.loads(line[5:])
                            if payload.get("usage"):
                                record["usage"] = payload["usage"]
                    self.wfile.write(chunk)
                    self.wfile.flush()
        except urllib.error.HTTPError as error:
            self.send_response(error.code)
            self.end_headers()
            self.wfile.write(error.read())


def fingerprint(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(",", ":")).encode()).hexdigest()


with tempfile.TemporaryDirectory(prefix="bridge-cache-") as directory:
    root = Path(directory)
    work = root / "work"
    work.mkdir()
    tap = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Tap)
    threading.Thread(target=tap.serve_forever, daemon=True).start()
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]
    config["listen"] = f"127.0.0.1:{port}"
    for entry in config["models"]:
        entry["base_url"] = f"http://127.0.0.1:{tap.server_port}/v1"
    private_path = root / "config.yaml"
    private_path.write_text(yaml.safe_dump(config, sort_keys=False))
    private_path.chmod(0o600)
    with (root / "proxy.log").open("w") as log:
        process = subprocess.Popen(["codex-bridge", "--config", str(private_path), "serve"], stdout=log, stderr=log)
    report = {"model": args.model, "turns": []}
    try:
        for _ in range(100):
            try:
                token = (root / "config.yaml.token").read_text()
                health = urllib.request.Request(f"http://127.0.0.1:{port}/health", headers={"X-Codex-Bridge-Token": token})
                with urllib.request.urlopen(health, timeout=1):
                    break
            except (OSError, urllib.error.URLError):
                assert process.poll() is None, (root / "proxy.log").read_text()
                time.sleep(0.1)
        else:
            raise AssertionError("test bridge did not start")
        overrides = ["-c", 'model_provider="codex-bridge"',
                     "-c", f'model_providers.codex-bridge.base_url="http://127.0.0.1:{port}/v1"',
                     "-c", f'model_providers.codex-bridge.http_headers.X-Codex-Bridge-Token="{token}"',
                     "-c", "model_providers.codex-bridge.requires_openai_auth=false"]
        thread = None
        previous_usage = {}
        for turn in range(1, 5):
            before = len(records)
            command = ["codex", "exec"] + (["resume", thread] if thread else [])
            command += ["--skip-git-repo-check", "-m", args.model, "--json"] + overrides
            prompt = f"Cache verification turn {turn}. Reply exactly ACK-{turn}. Do not use any tools."
            if turn == 1:
                reference = "\n".join(f"Reference row {i:04d}: amber cobalt delta evergreen." for i in range(150))
                prompt = "Keep this reference unchanged for later turns:\n" + reference + "\n" + prompt
            result = subprocess.run(command + ["--", prompt], cwd=work, stdin=subprocess.DEVNULL, capture_output=True, text=True, timeout=180)
            assert result.returncode == 0, result.stdout + result.stderr
            events = [json.loads(line) for line in result.stdout.splitlines()]
            current_thread = next(e["thread_id"] for e in events if e["type"] == "thread.started")
            assert thread is None or current_thread == thread
            thread = current_thread
            codex_usage = next(e["usage"] for e in events if e["type"] == "turn.completed")
            new = records[before:]
            assert new and all(r["usage"] for r in new), "upstream omitted usage"
            requests = []
            for index, record in enumerate(new, start=before):
                usage, body = record["usage"], record["body"]
                assert body.get("prompt_cache_key"), "Codex cache key was lost"
                inputs = usage["prompt_tokens"]
                cached = usage.get("prompt_tokens_details", {}).get("cached_tokens", usage.get("prompt_cache_hit_tokens", 0))
                assert 0 <= cached <= inputs
                row = {"input_tokens": inputs, "cached_input_tokens": cached,
                       "uncached_input_tokens": inputs - cached, "hit_ratio": cached / inputs,
                       "upstream_usage": usage}
                if index:
                    prior = records[index - 1]["body"]
                    row["cache_key_stable"] = body.get("prompt_cache_key") == prior.get("prompt_cache_key")
                    row["tools_stable"] = fingerprint(body.get("tools")) == fingerprint(prior.get("tools"))
                    row["prior_messages_unchanged"] = prior["messages"] == body["messages"][:len(prior["messages"])]
                    assert all(row[k] for k in ("cache_key_stable", "tools_stable", "prior_messages_unchanged")), row
                requests.append(row)
            raw_input = sum(r["input_tokens"] for r in requests)
            raw_cached = sum(r["cached_input_tokens"] for r in requests)
            # Codex resume reports cumulative thread usage; compare each turn's delta.
            delta = {key: codex_usage[key] - previous_usage.get(key, 0)
                     for key in ("input_tokens", "cached_input_tokens", "output_tokens")}
            assert delta == {"input_tokens": raw_input, "cached_input_tokens": raw_cached,
                             "output_tokens": sum(r["upstream_usage"]["completion_tokens"] for r in requests)}, delta
            previous_usage = codex_usage
            report["turns"].append({"turn": turn, "requests": requests, "codex_usage": codex_usage,
                                    "codex_usage_delta": delta})
            print(f"{args.model} turn {turn}: input={raw_input}, cached={raw_cached}, uncached={raw_input-raw_cached}, hit={raw_cached/raw_input:.2%}; stable_prefix={turn == 1 or requests[0]['prior_messages_unchanged']}", flush=True)
            # The provider may publish cache entries asynchronously.
            time.sleep(2)
        warm = [r for turn in report["turns"][1:] for r in turn["requests"]]
        report["subsequent_turns_weighted_hit_ratio"] = sum(r["cached_input_tokens"] for r in warm) / sum(r["input_tokens"] for r in warm)
        assert any(r["cached_input_tokens"] > 0 for r in warm), "provider reported no subsequent cache hits; inspect report"
        report["thread_id"] = thread
        if args.report:
            args.report.write_text(json.dumps(report, indent=2) + "\n")
        print(f"PASS stable prompt prefix and exact usage accounting; subsequent weighted hit ratio {report['subsequent_turns_weighted_hit_ratio']:.2%}", flush=True)
    finally:
        process.terminate()
        process.wait(timeout=15)
        tap.shutdown()
        tap.server_close()
