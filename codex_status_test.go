package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func modelSocket(t *testing.T, mode string, models []object) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bridge-ws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "socket")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		initialized := false
		for {
			var req object
			if wsjson.Read(ctx, conn, &req) != nil {
				return
			}
			if req["method"] == "initialized" {
				initialized = true
				continue
			}
			response := object{"id": req["id"]}
			if req["method"] == "initialize" {
				response["result"] = object{"userAgent": "codex/0.164.0"}
			} else if req["method"] == "model/list" && initialized {
				if mode == "stall" {
					continue
				}
				if mode == "rpc error" {
					response["error"] = object{"code": -32603, "message": "private detail must not be printed"}
				} else if mode == "missing data" {
					response["result"] = object{}
				} else if mode == "malformed" {
					_ = conn.Write(ctx, websocket.MessageText, []byte("not json"))
					return
				} else {
					if obj(req["params"])["cursor"] == "second" {
						response["result"] = object{"data": models[1:], "nextCursor": nil}
					} else {
						response["result"] = object{"data": models[:1], "nextCursor": "second"}
					}
					if mode == "repeated cursor" {
						obj(response["result"])["nextCursor"] = "second"
					}
				}
			} else {
				response["error"] = object{"code": -32600}
			}
			// Notifications and unrelated replies may arrive before our response.
			_ = wsjson.Write(ctx, conn, object{"method": "notice"})
			_ = wsjson.Write(ctx, conn, object{"id": 999, "result": object{}})
			if wsjson.Write(ctx, conn, response) != nil {
				return
			}
		}
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return socket
}

func TestLiveCodexSyncStatus(t *testing.T) {
	for _, mode := range []string{"synced", "stale", "extra model", "capabilities changed", "rpc error", "missing data", "malformed", "repeated cursor", "stopped", "unreachable", "invalid daemon", "invalid metadata"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("CODEX_HOME", dir)
			script := `#!/bin/sh
case "$*" in
  'debug models --bundled') printf '%s' '{"models":[{"slug":"builtin"}]}' ;;
  'app-server daemon version') printf '%s' "$TEST_CODEX_DAEMON" ;;
  *) exit 1 ;;
esac
`
			if err := os.WriteFile(filepath.Join(dir, "codex"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			c := Config{Listen: "127.0.0.1:8787", Models: []Model{{Name: "custom", Model: "remote", API: "chat", ContextWindow: 32768}}}
			path := filepath.Join(dir, "bridge.yaml")
			if err := syncCodex(path, c, false); err != nil {
				t.Fatal(err)
			}
			entry := object{"model": "custom", "displayName": "custom", "description": "remote via chat (codex-bridge)", "hidden": false, "defaultReasoningEffort": "none", "supportedReasoningEfforts": []any{}, "inputModalities": []any{"text"}}
			models := []object{entry}
			switch mode {
			case "stale":
				entry["model"] = "old-custom"
			case "extra model":
				models = append(models, entry)
			case "capabilities changed":
				entry["inputModalities"] = []any{"text", "image"}
			case "invalid metadata":
				delete(entry, "inputModalities")
			}
			daemon := object{"status": "running", "socketPath": modelSocket(t, mode, models)}
			if mode == "stopped" {
				daemon["status"] = "notRunning"
			}
			if mode == "unreachable" {
				daemon["socketPath"] = filepath.Join(dir, "missing")
			}
			if mode == "invalid daemon" {
				delete(daemon, "socketPath")
			}
			raw, _ := json.Marshal(daemon)
			t.Setenv("TEST_CODEX_DAEMON", string(raw))
			reason, err := codexSyncStatus(path, c)
			switch mode {
			case "synced":
				if reason != "" || err != nil {
					t.Fatalf("matching live models did not verify: %s %v", reason, err)
				}
			case "stale", "extra model", "capabilities changed":
				if reason == "" || err != nil {
					t.Fatalf("drift not reported as unsynced: %s %v", reason, err)
				}
			default:
				if reason != "" || err == nil || strings.Contains(err.Error(), "private detail") {
					t.Fatalf("failed verification misreported or leaked RPC details: %s %v", reason, err)
				}
			}
		})
	}
}

func TestCodexModelQueryTimeout(t *testing.T) {
	socket := modelSocket(t, "stall", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := codexModels(ctx, socket); err == nil {
		t.Fatal("unresponsive model query did not fail")
	}
}
