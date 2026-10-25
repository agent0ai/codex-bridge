package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"go.yaml.in/yaml/v3"
)

type object = map[string]any

type Config struct {
	Listen string  `toml:"listen" yaml:"listen"`
	Models []Model `toml:"models" yaml:"models"`
}

type Model struct {
	Name             string   `toml:"name" yaml:"name"`
	Model            string   `toml:"model" yaml:"model,omitempty"`
	BaseURL          string   `toml:"base_url" yaml:"base_url"`
	API              string   `toml:"api" yaml:"api"`
	APIKey           string   `toml:"api_key" yaml:"api_key,omitempty"`
	APIKeyEnv        string   `toml:"api_key_env" yaml:"api_key_env,omitempty"`
	Params           object   `toml:"params" yaml:"params,omitempty"`
	DropParams       []string `toml:"drop_params" yaml:"drop_params,omitempty"`
	ContextWindow    int      `toml:"context_window" yaml:"context_window,omitempty"`
	Vision           bool     `toml:"vision" yaml:"vision,omitempty"`
	ReasoningEfforts []string `toml:"reasoning_efforts" yaml:"reasoning_efforts,omitempty"`
}

func loadConfig(path string) (Config, error) {
	c := Config{Listen: "127.0.0.1:8787"}
	b, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if strings.EqualFold(filepath.Ext(path), ".toml") {
		err = toml.NewDecoder(bytes.NewReader(b)).DisallowUnknownFields().Decode(&c)
	} else {
		decoder := yaml.NewDecoder(bytes.NewReader(b))
		decoder.KnownFields(true)
		err = decoder.Decode(&c)
		if err == nil {
			var extra any
			if next := decoder.Decode(&extra); next != io.EOF {
				err = fmt.Errorf("expected one YAML document")
			}
		}
	}
	if err != nil {
		return c, fmt.Errorf("invalid config: %w", err)
	}
	if !validListen(c.Listen) {
		return c, fmt.Errorf("listen must be a loopback IP and nonzero port")
	}
	seen := map[string]bool{}
	for i := range c.Models {
		m := &c.Models[i]
		if m.Name == "" || strings.ContainsAny(m.Name, " \t\r\n") || seen[m.Name] {
			return c, fmt.Errorf("model names must be nonempty, unique, and contain no whitespace")
		}
		seen[m.Name] = true
		if m.Model == "" {
			m.Model = m.Name
		}
		if m.API != "chat" && m.API != "responses" {
			return c, fmt.Errorf("model %s: api must be chat or responses", m.Name)
		}
		u, e := url.Parse(m.BaseURL)
		if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
			return c, fmt.Errorf("model %s: invalid base_url (use HTTP(S) without userinfo or fragment)", m.Name)
		}
		m.BaseURL = strings.TrimRight(m.BaseURL, "/")
		if m.APIKey != "" && m.APIKeyEnv != "" {
			return c, fmt.Errorf("model %s: choose api_key or api_key_env", m.Name)
		}
		if _, err := json.Marshal(m.Params); err != nil {
			return c, fmt.Errorf("model %s: params must contain JSON-compatible values and string keys", m.Name)
		}
		if m.ContextWindow == 0 {
			m.ContextWindow = 32768
		}
		if m.ContextWindow < 1024 {
			return c, fmt.Errorf("model %s: context_window must be at least 1024", m.Name)
		}
		for _, k := range []string{"model", "messages", "input", "stream", "tools"} {
			if _, ok := m.Params[k]; ok {
				return c, fmt.Errorf("model %s: params.%s is controlled by the bridge", m.Name, k)
			}
			for _, d := range m.DropParams {
				if d == k {
					return c, fmt.Errorf("model %s: cannot drop %s", m.Name, k)
				}
			}
		}
		for _, e := range m.ReasoningEfforts {
			if !strings.Contains("|none|minimal|low|medium|high|xhigh|max|", "|"+e+"|") {
				return c, fmt.Errorf("model %s: invalid reasoning effort %s", m.Name, e)
			}
		}
	}
	return c, nil
}

func endpointURL(base, suffix string) string {
	u, _ := url.Parse(base)
	if strings.HasSuffix(strings.TrimRight(u.Path, "/"), suffix) {
		return u.String()
	}
	if suffix == "/responses/compact" && strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/responses") {
		suffix = "/compact"
	}
	return u.JoinPath(suffix).String()
}

func validListen(address string) bool {
	host, port, err := net.SplitHostPort(address)
	p, portErr := strconv.Atoi(port)
	return err == nil && portErr == nil && p > 0 && p <= 65535 && net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
}

func (m Model) key() (string, error) {
	if m.APIKeyEnv != "" {
		v := os.Getenv(m.APIKeyEnv)
		if v == "" {
			return "", fmt.Errorf("model %s: environment variable %s is empty in the proxy process", m.Name, m.APIKeyEnv)
		}
		return v, nil
	}
	return m.APIKey, nil
}

func merge(dst, src object) {
	for k, v := range src {
		if a, ok := v.(map[string]any); ok {
			if b, ok := dst[k].(map[string]any); ok {
				merge(b, a)
				continue
			}
		}
		dst[k] = v
	}
}

func atomicWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".bridge-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func localToken(path string) (string, error) {
	path += ".token"
	b, err := os.ReadFile(path)
	if err == nil {
		if len(b) != 64 {
			return "", fmt.Errorf("invalid local token file")
		}
		return string(b), nil
	}
	if !os.IsNotExist(err) {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	b = make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		return localToken(strings.TrimSuffix(path, ".token"))
	}
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(token)
	cerr := f.Close()
	if err != nil {
		return "", err
	}
	return token, cerr
}

func str(v any) string { s, _ := v.(string); return s }
func obj(v any) object { m, _ := v.(map[string]any); return m }
func arr(v any) []any  { a, _ := v.([]any); return a }
