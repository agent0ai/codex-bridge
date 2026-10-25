#!/usr/bin/env python3
"""Live Codex model-switch test; uses paid API calls. Run inside the test container."""
import argparse
import json
import queue
import subprocess
import tempfile
import threading

parser = argparse.ArgumentParser()
parser.add_argument("--external-only", action="store_true")
parser.add_argument("--subscription-model", default="gpt-6-luna")
args = parser.parse_args()
command = ["codex", "app-server", "--listen", "stdio://"]
if args.external_only:
    command += ["-c", "model_providers.codex-bridge.requires_openai_auth=false"]

with tempfile.TemporaryDirectory(prefix="bridge-smoke-") as cwd, tempfile.TemporaryFile() as stderr:
    process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=stderr, text=True)
    inbox = queue.Queue()

    def read():
        for line in process.stdout:
            inbox.put(json.loads(line))
        inbox.put({"error": "app-server exited"})

    threading.Thread(target=read, daemon=True).start()
    sequence = 0
    notifications = []

    def send(message):
        process.stdin.write(json.dumps(message) + "\n")
        process.stdin.flush()

    def request(method, params):
        global sequence
        sequence += 1
        send({"id": sequence, "method": method, "params": params})
        while True:
            message = inbox.get(timeout=180)
            if "error" in message:
                raise RuntimeError(message["error"])
            if message.get("id") == sequence:
                return message["result"]
            notifications.append(message)

    try:
        request("initialize", {"clientInfo": {"name": "bridge_smoke", "version": "0.1.0"},
                               "capabilities": {"experimentalApi": True}})
        send({"method": "initialized"})
        models = request("model/list", {"includeHidden": False, "limit": 100})["data"]
        names = [model["model"] for model in models]
        assert "a0-qwen" in names and "a0-deepseek" in names, names
        assert args.subscription_model in names, names
        print("Model picker includes external and subscription models", flush=True)
        result = request("thread/start", {"cwd": cwd, "model": "a0-qwen",
                         "modelProvider": "codex-bridge", "approvalPolicy": "never",
                         "sandbox": "danger-full-access"})
        thread_id = result["thread"]["id"]
        plan = [("a0-qwen", "Use apply_patch to create marker.txt containing ORBIT-731. Remember this marker and reply with it."),
                ("a0-deepseek", "Read marker.txt using a shell tool, then reply with the marker I gave you.")]
        if not args.external_only:
            plan.append((args.subscription_model, "Read marker.txt using a tool, then reply with the marker I gave you."))
        plan.append(("a0-qwen", "Read marker.txt using a shell tool, then reply with the marker I gave you."))
        for model, prompt in plan:
            notifications.clear()
            request("turn/start", {"threadId": thread_id, "model": model,
                                  "input": [{"type": "text", "text": prompt}]})
            text = ""
            tool_count = 0
            while True:
                message = notifications.pop(0) if notifications else inbox.get(timeout=180)
                if "error" in message:
                    raise RuntimeError(message["error"])
                if message.get("method") == "item/agentMessage/delta":
                    text += message["params"]["delta"]
                if message.get("method") == "item/completed" and message["params"]["item"]["type"] in ("commandExecution", "fileChange"):
                    tool_count += 1
                if message.get("method") == "turn/completed":
                    turn = message["params"]["turn"]
                    assert turn["status"] == "completed", turn
                    break
            assert "ORBIT-731" in text, (model, text)
            assert tool_count > 0, (model, "no completed file or command tool")
            print(f"PASS {model}: same thread {thread_id}, marker preserved, {tool_count} completed tools", flush=True)
        print("PASS model switching without profiles", flush=True)
    finally:
        process.terminate()
        try:
            process.wait(timeout=10)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=10)
