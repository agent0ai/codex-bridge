package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func apiError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(object{"error": object{"message": message, "type": "bridge_error", "code": status}})
}

type bridge struct {
	path, token string
	client      *http.Client
	builtins    map[string]bool
	shutdown    func()
}

func newBridge(path, token string, base object) *bridge {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 90 * time.Second
	b := &bridge{path: path, token: token, builtins: map[string]bool{}, client: &http.Client{Transport: transport, Timeout: 15 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for _, raw := range arr(base["models"]) {
		b.builtins[str(obj(raw)["slug"])] = true
	}
	return b
}

func (b *bridge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Origin") != "" {
		apiError(w, 403, "browser requests are not allowed")
		return
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Codex-Bridge-Token")), []byte(b.token)) != 1 {
		apiError(w, 401, "missing or invalid local bridge token; run sync")
		return
	}
	if r.URL.Path == "/health" && r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(object{"service": "codex-bridge", "config": b.path})
		return
	}
	if r.URL.Path == "/stop" && r.Method == "POST" {
		w.WriteHeader(202)
		if b.shutdown != nil {
			go b.shutdown()
		}
		return
	}
	if r.URL.Path != "/v1/responses" && r.URL.Path != "/v1/responses/compact" {
		apiError(w, 404, "unknown bridge endpoint")
		return
	}
	if r.Method != "POST" {
		apiError(w, 405, "use POST")
		return
	}
	if e := r.Header.Get("Content-Encoding"); e != "" && e != "identity" {
		apiError(w, 415, "compressed requests are unsupported")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		apiError(w, 413, "request exceeds 32 MiB or could not be read")
		return
	}
	var req object
	if err = json.Unmarshal(data, &req); err != nil || req == nil {
		apiError(w, 400, "expected a JSON object")
		return
	}
	model := str(req["model"])
	if model == "" {
		apiError(w, 400, "model is required")
		return
	}
	c, err := loadConfig(b.path)
	if err != nil {
		apiError(w, 503, "bridge config is invalid; run sync to see the validation error")
		return
	}
	var route *Model
	for i := range c.Models {
		if c.Models[i].Name == model {
			route = &c.Models[i]
			break
		}
	}
	if route == nil {
		if !b.builtins[model] {
			apiError(w, 400, "unknown model; add it to the bridge config and sync")
			return
		}
		normalizeResponsesInput(req)
		data, err = json.Marshal(req)
		if err != nil {
			apiError(w, 500, "could not encode subscription request")
			return
		}
		b.subscription(w, r, data)
		return
	}
	if r.URL.Path != "/v1/responses" && route.API == "chat" {
		apiError(w, 400, "Chat Completions models use Codex local compaction")
		return
	}
	key, err := route.key()
	if err != nil {
		apiError(w, 503, err.Error())
		return
	}
	upstream := req
	names := toolMap{}
	suffix := strings.TrimPrefix(r.URL.Path, "/v1")
	if route.API == "chat" {
		upstream, names, err = toChat(req)
		if err != nil {
			apiError(w, 400, err.Error())
			return
		}
		suffix = "/chat/completions"
	} else {
		normalizeResponsesInput(upstream)
	}
	upstream["model"] = route.Model
	for _, k := range route.DropParams {
		delete(upstream, k)
	}
	merge(upstream, route.Params)
	data, err = json.Marshal(upstream)
	if err != nil {
		apiError(w, 500, "could not encode upstream request")
		return
	}
	upReq, err := http.NewRequestWithContext(r.Context(), "POST", endpointURL(route.BaseURL, suffix), bytes.NewReader(data))
	if err != nil {
		apiError(w, 500, "invalid upstream request")
		return
	}
	upReq.Header.Set("Content-Type", "application/json")
	upReq.Header.Set("Accept", "text/event-stream")
	if key != "" {
		upReq.Header.Set("Authorization", "Bearer "+key)
	}
	start := time.Now()
	up, err := b.client.Do(upReq)
	if err != nil {
		log.Printf("model=%s upstream connection failed: %T", model, err)
		apiError(w, 502, "upstream connection failed")
		return
	}
	defer up.Body.Close()
	if up.StatusCode < 200 || up.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(up.Body, 64<<10))
		message := string(raw)
		for _, secret := range []string{key, r.Header.Get("Authorization"), b.token} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		if retry := up.Header.Get("Retry-After"); retry != "" {
			w.Header().Set("Retry-After", retry)
		}
		status := up.StatusCode
		// A third-party key failure must not trigger Codex to refresh the subscription login.
		if status == 401 || status == 403 || (status >= 300 && status < 400) {
			status = http.StatusBadGateway
		}
		apiError(w, status, fmt.Sprintf("%s upstream HTTP %d: %s", model, up.StatusCode, message))
		return
	}
	if route.API == "responses" {
		w.Header().Set("Content-Type", up.Header.Get("Content-Type"))
		w.WriteHeader(up.StatusCode)
		err = copyStream(w, up.Body)
	} else {
		streaming, _ := req["stream"].(bool)
		err = translateResponse(w, up, streaming, model, names)
	}
	log.Printf("model=%s api=%s status=%d elapsed=%s complete=%t", model, route.API, up.StatusCode, time.Since(start).Round(time.Millisecond), err == nil)
}

func copyStream(w http.ResponseWriter, r io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, e := w.Write(buf[:n]); e != nil {
				return e
			}
			if e := http.NewResponseController(w).Flush(); e != nil {
				return e
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (b *bridge) subscription(w http.ResponseWriter, r *http.Request, data []byte) {
	if r.Header.Get("Authorization") == "" {
		apiError(w, 401, "run codex login to use subscription models")
		return
	}
	base := "https://api.openai.com/v1"
	if r.Header.Get("ChatGPT-Account-ID") != "" {
		base = "https://chatgpt.com/backend-api/codex"
	}
	target, _ := url.Parse(base)
	// Only this fixed OpenAI destination receives the incoming Codex credentials.
	proxy := httputil.ReverseProxy{Rewrite: func(pr *httputil.ProxyRequest) {
		pr.SetURL(target)
		pr.Out.URL.Path = target.Path + strings.TrimPrefix(r.URL.Path, "/v1")
		pr.Out.Header.Del("X-Codex-Bridge-Token")
		pr.Out.Header.Del("Cookie")
	}, Transport: b.client.Transport, FlushInterval: -1, ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) { apiError(w, 502, "OpenAI connection failed") }}
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	proxy.ServeHTTP(w, r)
}

func serve(path string, c Config, token string) error {
	base, err := bundledCatalog()
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	if err = atomicWrite(path+".running", []byte(c.Listen)); err != nil {
		listener.Close()
		return err
	}
	defer os.Remove(path + ".running")
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	b := newBridge(path, token, base)
	b.shutdown = cancel
	server := &http.Server{Handler: b, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second, MaxHeaderBytes: 1 << 20}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		<-ctx.Done()
		shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = server.Shutdown(shutdownCtx)
		_ = server.Close()
	}()
	log.Printf("codex-bridge listening on %s", c.Listen)
	err = server.Serve(listener)
	if err == http.ErrServerClosed {
		<-stopped
		return nil
	}
	return err
}
