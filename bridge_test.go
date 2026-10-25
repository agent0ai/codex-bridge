package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"go.yaml.in/yaml/v3"
)

func decode(t *testing.T, s string) object {
	t.Helper()
	var o object
	if err := json.Unmarshal([]byte(s), &o); err != nil {
		t.Fatal(err)
	}
	return o
}

func TestMissingEditorShowsConfigPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "my config.toml")
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", filepath.Join(t.TempDir(), "missing-editor"))
	err := run([]string{"--config", path, "edit"})
	if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "VISUAL/EDITOR") {
		t.Fatalf("missing config path or editor guidance: %v", err)
	}
}

func TestEditorSelection(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, visual, editor, want string
		installed                  []string
	}{
		{name: "prefer micro", installed: []string{"vi", "nano", "micro", "editor"}, want: "micro"},
		{name: "fall back to nano", installed: []string{"vim", "nano"}, want: "nano"},
		{name: "vim before vi", installed: []string{"vi", "vim"}, want: "vim"},
		{name: "system alternative", installed: []string{"vi", "editor"}, want: "editor"},
		{name: "vi last", installed: []string{"vi"}, want: "vi"},
		{name: "VISUAL overrides EDITOR", installed: []string{"micro", "nano", "vim"}, visual: "nano --view", editor: "vim", want: "nano\n--view"},
		{name: "EDITOR overrides detection", installed: []string{"micro", "vim"}, editor: "vim -f", want: "vim\n-f"},
		{name: "none installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := t.TempDir()
			if err := os.Symlink(sh, filepath.Join(bin, "sh")); err != nil {
				t.Fatal(err)
			}
			for _, name := range tc.installed {
				if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nprintf '%s\\n' \"${0##*/}\" \"$@\"\n"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("PATH", bin)
			t.Setenv("VISUAL", tc.visual)
			t.Setenv("EDITOR", tc.editor)
			path := filepath.Join(t.TempDir(), "my config.toml")
			cmd, err := editorCommand(path)
			if tc.want == "" {
				if err == nil {
					t.Fatal("expected no editor error")
				}
				if err := run([]string{"--config", path, "edit"}); err == nil || !strings.Contains(err.Error(), path) {
					t.Fatalf("missing config path: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			output, err := cmd.Output()
			if err != nil || string(output) != tc.want+"\n"+path+"\n" {
				t.Fatalf("selected editor/arguments: %q, error: %v", output, err)
			}
		})
	}
}

func TestConversationTranslation(t *testing.T) {
	req := decode(t, `{"model":"alias","instructions":"system guidance","tools":[{"type":"namespace","name":"repo","tools":[{"type":"function","name":"read","parameters":{"type":"object"}}]},{"type":"custom","name":"apply_patch","description":"patch"}],"input":[{"role":"developer","content":[{"type":"input_text","text":"developer guidance"}]},{"role":"user","content":[{"type":"input_text","text":"look"},{"type":"input_image","image_url":"data:image/png;base64,AA","detail":"original"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"consider"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Checking"}]},{"type":"function_call","namespace":"repo","name":"read","call_id":"c1","arguments":"{}"},{"type":"custom_tool_call","name":"apply_patch","call_id":"c2","input":"*** Begin Patch\n*** End Patch"},{"type":"function_call_output","call_id":"c1","output":"file"},{"type":"custom_tool_call_output","call_id":"c2","output":"OK"},{"role":"user","content":"next"}],"text":{"format":{"type":"json_schema","name":"result","schema":{"type":"object"},"strict":true}}}`)
	chat, names, err := toChat(req)
	if err != nil {
		t.Fatal(err)
	}
	messages := arr(chat["messages"])
	if len(messages) != 7 {
		t.Fatalf("messages: %#v", messages)
	}
	a := obj(messages[3])
	if len(arr(a["tool_calls"])) != 2 || a["reasoning_content"] != "consider" || a["content"] != "Checking" {
		t.Fatalf("assistant: %#v", a)
	}
	alias := str(obj(obj(arr(chat["tools"])[0])["function"])["name"])
	if names[alias].Namespace != "repo" {
		t.Fatal(names)
	}
	if obj(obj(chat["response_format"])["json_schema"])["name"] != "result" {
		t.Fatal(chat)
	}
	image := obj(arr(obj(messages[2])["content"])[1])
	if obj(image["image_url"])["detail"] != "high" {
		t.Fatal(image)
	}
}

func TestRejectLossyInputs(t *testing.T) {
	for _, s := range []string{
		`{"previous_response_id":"r","input":[]}`,
		`{"input":[{"type":"function_call_output","call_id":"missing","output":"hi"}]}`,
		`{"input":[{"type":"function_call","name":"x","call_id":"c","arguments":"{}"}]}`,
		`{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file"}]}]}`,
		`{"tools":[{"type":"computer_use"}]}`,
	} {
		if _, _, err := toChat(decode(t, s)); err == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

func streamFixture(chunks ...string) string {
	var b strings.Builder
	for _, s := range chunks {
		fmt.Fprintf(&b, ": heartbeat\r\ndata: %s\r\n\r\n", s)
	}
	return b.String()
}

func TestStreamToolsReasoningUsageAndReplay(t *testing.T) {
	fixture := streamFixture(
		`{"choices":[{"index":0,"delta":{"reasoning_content":"Think","content":"Hi"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","function":{"name":"apply_","arguments":"{\"input\":"}},{"index":0,"id":"c1","function":{"name":"read","arguments":"{"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"name":"patch","arguments":"\"patch\"}"}},{"index":0,"function":{"arguments":"}"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":12,"completion_tokens":8,"total_tokens":20,"prompt_tokens_details":{"cached_tokens":4},"completion_tokens_details":{"reasoning_tokens":3}}}`, `[DONE]`)
	w := httptest.NewRecorder()
	up := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(fixture))}
	err := translateResponse(w, up, true, "alias", toolMap{"read": {"read", "ns", false}, "apply_patch": {"apply_patch", "", true}})
	if err != nil {
		t.Fatal(err)
	}
	var events []object
	if err = readSSE(w.Body, func(b []byte) error {
		var e object
		if err := json.Unmarshal(b, &e); err != nil {
			return err
		}
		events = append(events, e)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, e := range events {
		if int(number(e["sequence_number"])) != i {
			t.Fatal(e)
		}
	}
	last := events[len(events)-1]
	if last["type"] != "response.completed" {
		t.Fatal(last)
	}
	response := obj(last["response"])
	output := arr(response["output"])
	if len(output) != 4 || obj(output[2])["namespace"] != "ns" || obj(output[3])["input"] != "patch" {
		t.Fatal(output)
	}
	if obj(response["usage"])["total_tokens"] != float64(20) {
		t.Fatal(response)
	}
	replay := append(output, object{"type": "function_call_output", "call_id": "c1", "output": "contents"}, object{"type": "custom_tool_call_output", "call_id": "c2", "output": "patched"})
	chat, _, err := toChat(object{"input": replay})
	if err != nil {
		t.Fatal(err)
	}
	a := obj(arr(chat["messages"])[0])
	if a["reasoning_content"] != "Think" || len(arr(a["tool_calls"])) != 2 {
		t.Fatal(chat)
	}
}

func TestIncompleteStreamCannotExecuteTool(t *testing.T) {
	for _, finish := range []string{"", `,"finish_reason":"length"`} {
		fixture := streamFixture(`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c","function":{"name":"run","arguments":"{}"}}]}`+finish+`}]}`, `[DONE]`)
		w := httptest.NewRecorder()
		up := &http.Response{Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(fixture))}
		if err := translateResponse(w, up, true, "model", toolMap{"run": {"run", "", false}}); err == nil {
			t.Fatal("accepted truncated stream")
		}
		if strings.Contains(w.Body.String(), `"type":"function_call"`) || !strings.Contains(w.Body.String(), "response.failed") {
			t.Fatal(w.Body.String())
		}
	}
}

func TestNonStreamingProvider(t *testing.T) {
	w := httptest.NewRecorder()
	up := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))}
	if err := translateResponse(w, up, false, "alias", nil); err != nil {
		t.Fatal(err)
	}
	if decode(t, w.Body.String())["status"] != "completed" {
		t.Fatal(w.Body.String())
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRoutesNeverLeakSubscriptionCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	config := `listen: 127.0.0.1:8787
models:
  - name: custom
    model: remote-id
    base_url: https://provider.example/v1
    api: chat
    api_key: provider-secret
    params:
      temperature: 0.25
      vendor:
        thinking: false
`
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	b := newBridge(path, "local-secret", object{"models": []any{object{"slug": "builtin"}}})
	calls := 0
	b.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Host == "provider.example" {
			if r.Header.Get("Authorization") != "Bearer provider-secret" || r.Header.Get("ChatGPT-Account-ID") != "" || r.Header.Get("X-Codex-Bridge-Token") != "" {
				t.Fatalf("leaked headers: %v", r.Header)
			}
			body := decode(t, string(raw))
			if body["model"] != "remote-id" || body["temperature"] != 0.25 || obj(body["vendor"])["thinking"] != false {
				t.Fatal(body)
			}
		} else {
			if r.URL.String() != "https://chatgpt.com/backend-api/codex/responses" || r.Header.Get("Authorization") != "Bearer subscription-secret" || r.Header.Get("ChatGPT-Account-ID") != "account" || r.Header.Get("X-Codex-Bridge-Token") != "" {
				t.Fatalf("wrong subscription route: %v %v", r.URL, r.Header)
			}
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]}`))}, nil
	})
	for _, model := range []string{"custom", "builtin"} {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"`+model+`","input":"hi"}`))
		req.Header.Set("X-Codex-Bridge-Token", "local-secret")
		req.Header.Set("Authorization", "Bearer subscription-secret")
		req.Header.Set("ChatGPT-Account-ID", "account")
		w := httptest.NewRecorder()
		b.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	for _, origin := range []string{"", "https://evil.example"} {
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"custom"}`))
		if origin != "" {
			req.Header.Set("Origin", origin)
			req.Header.Set("X-Codex-Bridge-Token", "local-secret")
		}
		w := httptest.NewRecorder()
		b.ServeHTTP(w, req)
		if w.Code != 401 && w.Code != 403 {
			t.Fatal(w.Code)
		}
	}
	if calls != 2 {
		t.Fatal("unauthorized upstream request")
	}
}

func TestConfigEditPreservesUnrelatedSettings(t *testing.T) {
	original := []byte("# user comment\nmodel_provider = '''original''' # trailing\nmodel_catalog_json = \"/tmp/models\"\nmodel = \"gpt-5.5\"\n[projects.\"/tmp/x\"]\ntrust_level = \"trusted\"\n")
	got, values, err := removeRootStrings(original, map[string]bool{"model_provider": true, "model_catalog_json": true})
	if err != nil {
		t.Fatal(err)
	}
	if values["model_provider"] != "original" || values["model_catalog_json"] != "/tmp/models" {
		t.Fatal(values)
	}
	var parsed object
	if err = toml.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err, string(got))
	}
	if parsed["model"] != "gpt-5.5" || !bytes.Contains(got, []byte("# trailing")) {
		t.Fatal(string(got))
	}
}

func TestConfigValidation(t *testing.T) {
	for _, contents := range []string{
		`listen: 0.0.0.0:8787`, `listen: localhost:8787`, `unknown: true`,
		`models: [{name: x, base_url: "https://user:pass@example.com", api: chat}]`,
		`models: [{name: x, base_url: "https://example.com/v1", api: chat, params: {stream: false}}]`,
		`models: [{name: x, base_url: "https://example.com/v1", api: chat, typo: true}]`,
		"listen: 127.0.0.1:8787\nlisten: 127.0.0.1:8788\n",
		"listen: 127.0.0.1:8787\n---\nmodels: []\n",
		`models: [{name: x, base_url: "https://example.com/v1", api: chat, params: {vendor: {1: invalid}}}]`,
		`models: [{name: x, base_url: "https://example.com/v1", api: chat, params: {temperature: .nan}}]`,
	} {
		p := filepath.Join(t.TempDir(), "config")
		_ = os.WriteFile(p, []byte(contents), 0600)
		if _, err := loadConfig(p); err == nil {
			t.Fatalf("accepted %s", contents)
		}
	}
}

func TestYAMLAndLegacyConfigPreserveValues(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, "config.toml")
	data := "listen='127.0.0.1:8787'\n[[models]]\nname='custom'\napi='chat'\nbase_url='https://example.com/v1'\napi_key='private-key'\n[models.params]\ntemperature=0.2\nlabels=['on','off']\n[models.params.vendor]\nthinking=false\n"
	if err := os.WriteFile(legacy, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	want, err := loadConfig(legacy)
	if err != nil {
		t.Fatal(err)
	}
	b, err := yaml.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	got, err := loadConfig(path)
	if err != nil || !sameJSON(got, want) {
		t.Fatalf("migration changed config values: %v", err)
	}
	if c, err := loadConfig("config.example.yaml"); err != nil || len(c.Models) != 3 {
		t.Fatalf("example config invalid: %v", err)
	}
}

func TestSyncIdempotentAndUnsyncPreservesUserEdits(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CODEX_HOME", dir)
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' '{\"models\":[{\"slug\":\"builtin\",\"model_messages\":{\"instructions_template\":\"instructions\"}}]}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(bin)+":"+os.Getenv("PATH"))
	target := filepath.Join(dir, "config.toml")
	original := "# keep this comment\nmodel_provider = 'prior'\nmodel = 'builtin'\n[model_providers.prior]\nname = 'Prior'\nbase_url = 'https://example.com/v1'\n"
	if err := os.WriteFile(target, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "bridge.yaml")
	c := Config{Listen: "127.0.0.1:8787", Models: []Model{{Name: "custom", Model: "remote", API: "chat", ContextWindow: 32768}}}
	if codexSyncStatus(configPath, c) == "" {
		t.Fatal("reported synced before sync")
	}
	if err := syncCodex(configPath, c); err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(target)
	if err := syncCodex(configPath, c); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(target)
	if !bytes.Equal(first, second) {
		t.Fatal("sync changes config on every invocation")
	}
	if reason := codexSyncStatus(configPath, c); reason != "" {
		t.Fatal("reported unsynced after sync:", reason)
	}
	changed := c
	changed.Listen = "127.0.0.1:9999"
	if codexSyncStatus(configPath, changed) == "" {
		t.Fatal("missed changed listen address")
	}
	changed = c
	changed.Models = append([]Model(nil), c.Models...)
	changed.Models[0].Vision = true
	if codexSyncStatus(configPath, changed) == "" {
		t.Fatal("missed changed model metadata")
	}
	changed.Models = nil
	if codexSyncStatus(configPath, changed) == "" {
		t.Fatal("missed removed model")
	}
	changed = c
	changed.Models = append([]Model(nil), c.Models...)
	changed.Models[0].APIKey = "new-key"
	changed.Models[0].Params = object{"temperature": 0.5}
	if reason := codexSyncStatus(configPath, changed); reason != "" {
		t.Fatal("hot-loaded settings should not require sync:", reason)
	}
	catalogPath := filepath.Join(dir, "codex-bridge-models.json")
	catalog, _ := os.ReadFile(catalogPath)
	if err := os.WriteFile(catalogPath, []byte(`{"models":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if codexSyncStatus(configPath, c) == "" {
		t.Fatal("missed edited catalog")
	}
	if err := os.WriteFile(catalogPath, catalog, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, append(second, []byte("\n[projects.\"/new\"]\ntrust_level = 'trusted'\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if reason := codexSyncStatus(configPath, c); reason != "" {
		t.Fatal("unrelated Codex settings should not affect sync:", reason)
	}
	if err := unsyncCodex(); err != nil {
		t.Fatal(err)
	}
	if codexSyncStatus(configPath, c) == "" {
		t.Fatal("reported synced after unsync")
	}
	result, _ := os.ReadFile(target)
	var parsed object
	if err := toml.Unmarshal(result, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed["model_provider"] != "prior" || obj(obj(parsed["projects"])["/new"])["trust_level"] != "trusted" || !bytes.Contains(result, []byte("# keep this comment")) {
		t.Fatal(string(result))
	}
}

func TestExternalModelsSortBeforeBuiltins(t *testing.T) {
	base := object{"models": []any{object{"slug": "builtin", "priority": -10}}}
	c := Config{Models: []Model{{Name: "first"}, {Name: "second"}}}
	catalog, err := makeCatalog(base, c)
	if err != nil {
		t.Fatal(err)
	}
	models := arr(catalog["models"])
	if number(obj(models[1])["priority"]) >= number(obj(models[2])["priority"]) || number(obj(models[2])["priority"]) >= number(obj(models[0])["priority"]) {
		t.Fatal("external models are not first in config order")
	}
}

func TestResponsesPassthroughAndAuthErrors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.toml")
	config := []byte("[[models]]\nname='native'\nmodel='remote'\napi='responses'\nbase_url='https://provider.example/v1'\napi_key_env='BRIDGE_TEST_KEY'\n[models.params]\ntemperature=0.7\n")
	if err := os.WriteFile(p, config, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_TEST_KEY", "test-secret")
	b := newBridge(p, "local", object{})
	for _, status := range []int{200, 401, 302} {
		b.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(r.Body)
			body := decode(t, string(raw))
			if r.URL.Path != "/v1/responses" || body["model"] != "remote" || body["input"] != "question" || body["temperature"] != 0.7 || r.Header.Get("Authorization") != "Bearer test-secret" {
				t.Fatal(r.URL, body)
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: test-secret\n\n"))}, nil
		})
		r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"native","input":"question","stream":true}`))
		r.Header.Set("X-Codex-Bridge-Token", "local")
		w := httptest.NewRecorder()
		b.ServeHTTP(w, r)
		if status == 200 {
			if w.Body.String() != "data: test-secret\n\n" {
				t.Fatal(w.Body.String())
			}
		} else if w.Code != 502 || strings.Contains(w.Body.String(), "test-secret") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestSwitchToResponsesKeepsReasoningWithoutDanglingIDs(t *testing.T) {
	req := decode(t, `{"input":[{"type":"reasoning","id":"rs_bridge_123","summary":[{"type":"summary_text","text":"Useful thought"}]},{"type":"message","id":"msg_bridge_456","role":"assistant","content":[{"type":"output_text","text":"Answer"}]},{"type":"reasoning","id":"rs_openai","encrypted_content":"opaque","summary":[]},{"type":"function_call","id":"fc_bridge_789","call_id":"call_123","name":"read","arguments":"{}"}]}`)
	normalizeResponsesInput(req)
	input := arr(req["input"])
	if obj(input[0])["type"] != "message" || !strings.Contains(str(obj(arr(obj(input[0])["content"])[0])["text"]), "Useful thought") {
		t.Fatal(input)
	}
	if obj(input[1])["id"] != nil || obj(input[2])["encrypted_content"] != "opaque" || obj(input[3])["id"] != nil || obj(input[3])["call_id"] != "call_123" {
		t.Fatal(input)
	}
}

func TestProviderURLs(t *testing.T) {
	for _, tc := range []struct{ base, suffix, want string }{
		{"https://example.com/api/v1?api-version=1", "/chat/completions", "https://example.com/api/v1/chat/completions?api-version=1"},
		{"https://example.com/v1/chat/completions", "/chat/completions", "https://example.com/v1/chat/completions"},
		{"https://example.com/v1/responses?version=2", "/responses/compact", "https://example.com/v1/responses/compact?version=2"},
	} {
		if got := endpointURL(tc.base, tc.suffix); got != tc.want {
			t.Fatalf("got %s, want %s", got, tc.want)
		}
	}
}

func TestAttachmentsInToolResultsAndInlineFiles(t *testing.T) {
	req := decode(t, `{"input":[{"role":"user","content":[{"type":"input_file","filename":"report.pdf","file_data":"data:application/pdf;base64,JVBERi0="}]},{"type":"function_call","call_id":"c1","name":"view_image","arguments":"{}"},{"type":"function_call","call_id":"c2","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[{"type":"input_text","text":"Image"},{"type":"input_image","image_url":"data:image/png;base64,AA"}]},{"type":"function_call_output","call_id":"c2","output":"text"}]}`)
	chat, _, err := toChat(req)
	if err != nil {
		t.Fatal(err)
	}
	messages := arr(chat["messages"])
	file := obj(obj(arr(obj(messages[0])["content"])[0])["file"])
	if file["filename"] != "report.pdf" || file["file_data"] != "data:application/pdf;base64,JVBERi0=" {
		t.Fatal(file)
	}
	if len(messages) != 5 || obj(messages[2])["role"] != "tool" || obj(messages[3])["role"] != "tool" || obj(messages[4])["role"] != "user" {
		t.Fatal(messages)
	}
	for _, p := range arr(obj(messages[2])["content"]) {
		if obj(p)["type"] != "text" {
			t.Fatal("media leaked into Chat tool message")
		}
	}
	media := arr(obj(messages[4])["content"])
	if obj(obj(media[1])["image_url"])["url"] != "data:image/png;base64,AA" {
		t.Fatal(media)
	}
}

func TestCacheControlsAndPromptPrefixStayStable(t *testing.T) {
	req := decode(t, `{"model":"alias","instructions":"unchanging instructions","prompt_cache_key":"same-thread","prompt_cache_retention":"24h","prompt_cache_options":{"mode":"explicit","ttl":"30m"},"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"input":[{"role":"user","content":[{"type":"input_text","text":"stable context","prompt_cache_breakpoint":{"mode":"explicit"},"cache_control":{"type":"ephemeral"}}]}]}`)
	first, _, err := toChat(req)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := json.Marshal(first)
	for _, key := range []string{"prompt_cache_key", "prompt_cache_retention", "prompt_cache_options"} {
		want, _ := json.Marshal(req[key])
		got, _ := json.Marshal(first[key])
		if !bytes.Equal(want, got) {
			t.Fatalf("lost %s", key)
		}
	}
	for i := 0; i < 20; i++ {
		repeated, _, err := toChat(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(repeated)
		if !bytes.Equal(firstJSON, b) {
			t.Fatal("identical input changed its upstream representation")
		}
	}
	req["input"] = append(arr(req["input"]), object{"type": "message", "id": "msg_bridge_random", "role": "assistant", "content": []any{object{"type": "output_text", "text": "ACK"}}}, object{"role": "user", "content": "next turn"})
	next, _, err := toChat(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"tools", "prompt_cache_key", "prompt_cache_retention", "prompt_cache_options"} {
		a, _ := json.Marshal(first[key])
		b, _ := json.Marshal(next[key])
		if !bytes.Equal(a, b) {
			t.Fatalf("%s changed", key)
		}
	}
	prefix, _ := json.Marshal(arr(next["messages"])[:len(arr(first["messages"]))])
	old, _ := json.Marshal(first["messages"])
	if !bytes.Equal(prefix, old) {
		t.Fatal("new turn mutated the cached prefix")
	}
	part := obj(arr(obj(arr(first["messages"])[1])["content"])[0])
	if part["prompt_cache_breakpoint"] == nil || part["cache_control"] == nil {
		t.Fatal("lost content cache controls")
	}
}

func TestCacheUsageAccounting(t *testing.T) {
	for _, tc := range []struct {
		usage  string
		cached float64
	}{
		{`{"prompt_tokens":1000,"completion_tokens":20,"total_tokens":1020,"prompt_tokens_details":{"cached_tokens":800}}`, 800},
		{`{"prompt_tokens":1000,"completion_tokens":20,"total_tokens":1020,"prompt_cache_hit_tokens":700,"prompt_cache_miss_tokens":300}`, 700},
		{`{"prompt_tokens":1000,"completion_tokens":20,"total_tokens":1020,"prompt_tokens_details":{"cached_tokens":0},"prompt_cache_hit_tokens":700}`, 0},
	} {
		got := responsesUsage(decode(t, tc.usage))
		if number(got["input_tokens"]) != 1000 || number(obj(got["input_tokens_details"])["cached_tokens"]) != tc.cached {
			t.Fatal(got)
		}
	}
}
