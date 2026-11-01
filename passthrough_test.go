package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnknownEndpointModes(t *testing.T) {
	for _, tc := range []struct {
		name, mode, base string
		first, second    int
		want, calls      int
		wantSource       string
	}{
		{"default native success", "", "https://provider.example/api/v1", 200, 200, 200, 1, "native"},
		{"native error", "native", "https://provider.example/api/v1", 404, 200, 404, 1, "native"},
		{"openai only", "openai", "https://provider.example/api/v1", 200, 200, 200, 1, "openai"},
		{"auto fallback", "auto", "https://provider.example/api/v1/chat/completions", 404, 200, 200, 2, "openai"},
		{"auto both fail", "auto", "https://provider.example/api/v1/responses", 418, 503, 418, 2, "native"},
		{"auto unauthorized", "auto", "https://provider.example/api/v1", 401, 403, 401, 2, "native"},
		{"exactly 200", "auto", "https://provider.example/api/v1", 201, 200, 200, 2, "openai"},
		{"fallback must be 200", "auto", "https://provider.example/api/v1", 429, 201, 429, 2, "native"},
		{"redirect is failure", "auto", "https://provider.example/api/v1", 302, 200, 200, 2, "openai"},
		{"native connection error", "auto", "https://provider.example/api/v1", 0, 200, 200, 2, "openai"},
		{"fallback connection error", "auto", "https://provider.example/api/v1", 429, 0, 429, 2, "native"},
		{"both connection errors", "auto", "https://provider.example/api/v1", 0, 0, 502, 2, ""},
		{"same endpoint", "auto", "https://api.openai.com/v1", 404, 200, 404, 1, "native"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			config := fmt.Sprintf("models: [{name: custom, model: remote, api: chat, base_url: %q, api_key: provider-secret, unknown_endpoints: %q}]\n", tc.base, tc.mode)
			if err := os.WriteFile(path, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			c, err := loadConfig(path)
			if err != nil || (tc.mode == "" && c.Models[0].UnknownEndpoints != "auto") {
				t.Fatal("incorrect default:", c, err)
			}
			b := newBridge(path, "local")
			calls := 0
			const body = "{\n  \"model\": \"custom\", \"prompt\": \"draw\"\n}"
			b.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				source, status := "native", tc.first
				if tc.mode == "openai" || calls == 2 {
					source, status = "openai", tc.second
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil || string(raw) != body || r.Method != "POST" || r.URL.RawQuery != "x=1&x=2" {
					t.Fatal("changed request:", string(raw), r.URL, err)
				}
				if source == "native" {
					base := strings.TrimSuffix(strings.TrimSuffix(tc.base, "/chat/completions"), "/responses")
					if r.URL.String() != base+"/images/generations?x=1&x=2" || r.Header.Get("Authorization") != "Bearer provider-secret" {
						t.Fatal("wrong native route:", r.URL, r.Header)
					}
					for _, key := range []string{"ChatGPT-Account-ID", "OpenAI-Organization", "OpenAI-Project"} {
						if r.Header.Get(key) != "" {
							t.Fatal("leaked subscription header:", key)
						}
					}
				} else if r.URL.String() != "https://api.openai.com/v1/images/generations?x=1&x=2" || r.Header.Get("Authorization") != "Bearer openai-secret" {
					t.Fatal("wrong fallback route:", r.URL, r.Header)
				}
				if r.Header.Get("Content-Type") != "application/json" || r.Header.Get("X-Codex-Bridge-Token") != "" || r.Header.Get("Cookie") != "" {
					t.Fatal("incorrect headers:", r.Header)
				}
				if status == 0 {
					return nil, fmt.Errorf("%s connection failed", source)
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Source": {source}, "Retry-After": {"7"}}, Body: io.NopCloser(strings.NewReader(source))}, nil
			})
			r := httptest.NewRequest("POST", "/v1/images/generations?x=1&x=2", strings.NewReader(body))
			r.Header.Set("X-Codex-Bridge-Token", "local")
			r.Header.Set("Authorization", "Bearer openai-secret")
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("OpenAI-Organization", "private-org")
			r.Header.Set("OpenAI-Project", "private-project")
			r.Header.Set("Cookie", "private=cookie")
			w := httptest.NewRecorder()
			b.ServeHTTP(w, r)
			if w.Code != tc.want || calls != tc.calls || (tc.wantSource != "" && (w.Body.String() != tc.wantSource || w.Header().Get("X-Source") != tc.wantSource || w.Header().Get("Retry-After") != "7")) {
				t.Fatal("wrong selected response:", w.Code, calls, w.Body.String(), w.Header())
			}
		})
	}
}

func TestUnknownEndpointModelSources(t *testing.T) {
	for _, tc := range []struct{ name, query, contentType, encoding, body string }{
		{"query", "?model=custom", "application/octet-stream", "gzip", "opaque bytes"},
		{"upstream model id", "?model=remote-id", "application/octet-stream", "", "opaque bytes"},
		{"multipart", "", "multipart/form-data; boundary=test", "", "--test\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\ncustom\r\n--test\r\nContent-Disposition: form-data; name=\"image\"; filename=\"input.png\"\r\n\r\n\x00\xff\r\n--test--\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte("models: [{name: custom, model: remote-id, api: chat, base_url: 'https://provider.example/v1?version=2', unknown_endpoints: native}]"), 0600); err != nil {
				t.Fatal(err)
			}
			b := newBridge(path, "local")
			calls := 0
			b.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				raw, _ := io.ReadAll(r.Body)
				if r.URL.Host != "provider.example" || r.URL.Query().Get("version") != "2" || string(raw) != tc.body || r.Header.Get("Authorization") != "" {
					t.Fatal("wrong native request:", r.URL, string(raw), r.Header)
				}
				return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("OK"))}, nil
			})
			r := httptest.NewRequest("POST", "/v1/images/edits"+tc.query, strings.NewReader(tc.body))
			r.Header.Set("X-Codex-Bridge-Token", "local")
			r.Header.Set("Content-Type", tc.contentType)
			r.Header.Set("Content-Encoding", tc.encoding)
			w := httptest.NewRecorder()
			b.ServeHTTP(w, r)
			if w.Code != 200 || calls != 1 {
				t.Fatal(w.Code, calls, w.Body.String())
			}
		})
	}
}

func TestUnknownEndpointRejectsAmbiguousModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("models: [{name: a, model: shared, api: chat, base_url: 'https://a.example/v1'}, {name: b, model: shared, api: chat, base_url: 'https://b.example/v1'}]"), 0600); err != nil {
		t.Fatal(err)
	}
	b := newBridge(path, "local")
	b.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("ambiguous request was sent upstream")
		return nil, nil
	})
	r := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"shared"}`))
	r.Header.Set("X-Codex-Bridge-Token", "local")
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "multiple providers") {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestUnknownEndpointSubscriptionDestinationIsNotRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("models: [{name: custom, api: responses, base_url: 'https://chatgpt.com/backend-api/codex/responses', api_key: provider-key}]"), 0600); err != nil {
		t.Fatal(err)
	}
	b := newBridge(path, "local")
	calls := 0
	b.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != "https://chatgpt.com/backend-api/codex/images/generations" || r.Header.Get("Authorization") != "Bearer provider-key" || r.Header.Get("ChatGPT-Account-ID") != "" {
			t.Fatal("wrong native request:", r.URL, r.Header)
		}
		return &http.Response{StatusCode: 404, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("first error"))}, nil
	})
	r := httptest.NewRequest("POST", "/v1/images/generations", strings.NewReader(`{"model":"custom"}`))
	r.Header.Set("X-Codex-Bridge-Token", "local")
	r.Header.Set("Authorization", "Bearer subscription-key")
	r.Header.Set("ChatGPT-Account-ID", "account")
	w := httptest.NewRecorder()
	b.ServeHTTP(w, r)
	if calls != 1 || w.Code != 404 || w.Body.String() != "first error" {
		t.Fatal("retried the same endpoint:", calls, w.Code, w.Body.String())
	}
}
