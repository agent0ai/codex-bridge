package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func codexSyncStatus(path string, c Config) (string, error) {
	if reason, err := codexConfigStatus(path, c); reason != "" || err != nil {
		return reason, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	socket, err := codexDaemonSocket(ctx)
	if err != nil {
		return "", err
	}
	models, err := codexModels(ctx, socket)
	if err != nil {
		return "", fmt.Errorf("could not read the running Codex model list: %w", err)
	}
	return compareCodexModels(models, c), nil
}

func codexDaemonSocket(ctx context.Context) (string, error) {
	raw, err := exec.CommandContext(ctx, "codex", "app-server", "daemon", "version").Output()
	var daemon struct {
		Status     string `json:"status"`
		SocketPath string `json:"socketPath"`
	}
	if err != nil || json.Unmarshal(raw, &daemon) != nil {
		return "", fmt.Errorf("could not inspect the Codex server")
	}
	if daemon.Status == "notRunning" {
		return "", fmt.Errorf("the Codex server is not running")
	}
	if daemon.Status != "running" || daemon.SocketPath == "" {
		return "", fmt.Errorf("Codex did not report a running server socket")
	}
	return daemon.SocketPath, nil
}

func codexModels(ctx context.Context, socket string) ([]object, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	conn, _, err := websocket.Dial(ctx, "ws://localhost/", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		return nil, err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)
	id := 0
	request := func(method string, params object, result any) error {
		id++
		if err := wsjson.Write(ctx, conn, object{"id": id, "method": method, "params": params}); err != nil {
			return err
		}
		for {
			var response struct {
				ID     int                 `json:"id"`
				Result json.RawMessage     `json:"result"`
				Error  *struct{ Code int } `json:"error"`
			}
			if err := wsjson.Read(ctx, conn, &response); err != nil {
				return err
			}
			if response.ID != id {
				continue
			}
			if response.Error != nil {
				return fmt.Errorf("Codex rejected %s (code %d)", method, response.Error.Code)
			}
			if len(response.Result) == 0 || string(response.Result) == "null" {
				return fmt.Errorf("Codex returned no result for %s", method)
			}
			return json.Unmarshal(response.Result, result)
		}
	}
	var initialized object
	if err := request("initialize", object{"clientInfo": object{"name": "codex_bridge", "version": version}}, &initialized); err != nil {
		return nil, err
	}
	if err := wsjson.Write(ctx, conn, object{"method": "initialized"}); err != nil {
		return nil, err
	}
	var models []object
	cursor := ""
	seen := map[string]bool{}
	for {
		params := object{"limit": 100, "includeHidden": true}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Data       []object `json:"data"`
			NextCursor *string  `json:"nextCursor"`
		}
		if err := request("model/list", params, &page); err != nil {
			return nil, err
		}
		if page.Data == nil {
			return nil, fmt.Errorf("Codex returned an invalid model list")
		}
		for _, model := range page.Data {
			if str(model["model"]) == "" {
				return nil, fmt.Errorf("Codex returned a model without a name")
			}
			for _, field := range []string{"displayName", "description", "defaultReasoningEffort"} {
				if _, ok := model[field].(string); !ok {
					return nil, fmt.Errorf("Codex returned invalid model metadata")
				}
			}
			if _, ok := model["hidden"].(bool); !ok {
				return nil, fmt.Errorf("Codex returned invalid model visibility")
			}
			for _, field := range []string{"inputModalities", "supportedReasoningEfforts"} {
				if _, ok := model[field].([]any); !ok {
					return nil, fmt.Errorf("Codex returned invalid model capabilities")
				}
			}
		}
		models = append(models, page.Data...)
		if page.NextCursor == nil {
			return models, nil
		}
		cursor = *page.NextCursor
		if cursor == "" || seen[cursor] || len(models) > 10000 {
			return nil, fmt.Errorf("Codex returned invalid model pagination")
		}
		seen[cursor] = true
	}
}

func compareCodexModels(models []object, c Config) string {
	wanted := map[string]bool{}
	for i, m := range c.Models {
		wanted[m.Name] = true
		if i >= len(models) || models[i]["model"] != m.Name {
			return "the running Codex model list differs; run sync --force"
		}
		modalities := []string{"text"}
		if m.Vision {
			modalities = append(modalities, "image")
		}
		efforts := []any{}
		defaultEffort := "none"
		for _, effort := range m.ReasoningEfforts {
			efforts = append(efforts, object{"reasoningEffort": effort, "description": effort})
		}
		if len(m.ReasoningEfforts) > 0 {
			defaultEffort = m.ReasoningEfforts[0]
		}
		expected := object{"displayName": m.Name, "description": m.Model + " via " + m.API + " (codex-bridge)", "hidden": false, "inputModalities": modalities, "supportedReasoningEfforts": efforts, "defaultReasoningEffort": defaultEffort}
		for field, value := range expected {
			if !sameJSON(models[i][field], value) {
				return "the running Codex settings for " + m.Name + " differ; run sync --force"
			}
		}
	}
	for _, model := range models[len(c.Models):] {
		if strings.HasSuffix(str(model["description"]), "(codex-bridge)") || wanted[str(model["model"])] {
			return "the running Codex list contains extra bridge models; run sync --force"
		}
	}
	return ""
}
