package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

const rootBegin = "# BEGIN codex-bridge settings\n"
const rootEnd = "# END codex-bridge settings\n"
const providerBegin = "# BEGIN codex-bridge provider\n"
const providerEnd = "# END codex-bridge provider\n"

func stripBlock(b []byte, begin, end string) ([]byte, error) {
	i := bytes.Index(b, []byte(begin))
	if i < 0 {
		return b, nil
	}
	j := bytes.Index(b[i:], []byte(end))
	if j < 0 {
		return nil, fmt.Errorf("incomplete managed block %s", strings.TrimSpace(begin))
	}
	out := append([]byte{}, b[:i]...)
	return append(out, b[i+j+len(end):]...), nil
}

// Replace only root string values, keeping comments and all unrelated tables intact.
func removeRootStrings(b []byte, keys map[string]bool) ([]byte, map[string]string, error) {
	var p unstable.Parser
	p.Reset(b)
	type span struct{ start, end int }
	var spans []span
	values := map[string]string{}
	for p.NextExpression() {
		n := p.Expression()
		if n.Kind == unstable.Table || n.Kind == unstable.ArrayTable {
			break
		}
		if n.Kind != unstable.KeyValue {
			continue
		}
		k := n.Key()
		k.Next()
		name := string(k.Node().Data)
		if !k.IsLast() || !keys[name] {
			continue
		}
		v := n.Value()
		if v.Kind != unstable.String {
			return nil, nil, fmt.Errorf("%s must be a TOML string", name)
		}
		start := int(k.Node().Raw.Offset)
		end := int(v.Raw.Offset + v.Raw.Length)
		if end <= start {
			return nil, nil, fmt.Errorf("cannot locate %s", name)
		}
		spans = append(spans, span{start, end})
		values[name] = string(v.Data)
	}
	if err := p.Error(); err != nil {
		return nil, nil, err
	}
	out := append([]byte{}, b...)
	for i := len(spans) - 1; i >= 0; i-- {
		s := spans[i]
		out = append(out[:s.start], out[s.end:]...)
	}
	return out, values, nil
}

func codexDir() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex")
}

func bundledCatalog() (object, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "codex", "debug", "models", "--bundled")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("codex debug models --bundled failed (requires Codex 0.164.0): %w: %s", err, stderr.String())
	}
	var c object
	if err = json.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if len(arr(c["models"])) == 0 {
		return nil, fmt.Errorf("Codex returned an empty catalog")
	}
	return c, nil
}

func makeCatalog(base object, c Config) (object, error) {
	models := arr(base["models"])
	names := map[string]bool{}
	template := obj(models[0])
	for _, raw := range models {
		m := obj(raw)
		names[str(m["slug"])] = true
		if m["slug"] == "gpt-5.5" {
			template = m
		}
	}
	for i, m := range c.Models {
		if names[m.Name] {
			return nil, fmt.Errorf("model name %s collides with a built-in model; choose an alias", m.Name)
		}
		b, _ := json.Marshal(template)
		var entry object
		_ = json.Unmarshal(b, &entry)
		merge(entry, object{"slug": m.Name, "display_name": m.Name, "description": m.Model + " via " + m.API + " (codex-bridge)", "visibility": "list", "supported_in_api": true, "priority": 100 + i, "context_window": m.ContextWindow, "max_context_window": m.ContextWindow, "auto_compact_token_limit": nil, "upgrade": nil, "availability_nux": nil, "default_reasoning_level": nil, "supported_reasoning_levels": []any{}, "additional_speed_tiers": []any{}, "service_tiers": []any{}, "default_service_tier": nil, "tool_mode": "direct", "multi_agent_version": nil, "multi_agent_reasoning_effort": nil, "supports_search_tool": false, "supports_experimental_context": false, "use_responses_lite": false, "supports_reasoning_effort_updates": false, "node_repl_auto_review_required": false, "node_repl_disabled": true, "experimental_supported_tools": []any{}, "support_verbosity": false, "default_verbosity": nil, "supports_reasoning_summary_parameter": false, "supports_image_detail_original": false, "comp_hash": nil, "input_modalities": []string{"text"}})
		if m.Vision {
			entry["input_modalities"] = []string{"text", "image"}
		}
		if len(m.ReasoningEfforts) > 0 {
			levels := []any{}
			for _, effort := range m.ReasoningEfforts {
				levels = append(levels, object{"effort": effort, "description": effort})
			}
			entry["supported_reasoning_levels"] = levels
			entry["default_reasoning_level"] = m.ReasoningEfforts[0]
		}
		models = append(models, entry)
	}
	return object{"models": models}, nil
}

func syncCodex(path string, c Config) error {
	dir := codexDir()
	target := filepath.Join(dir, "config.toml")
	catalogPath := filepath.Join(dir, "codex-bridge-models.json")
	original, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	var parsed object
	if err = toml.Unmarshal(original, &parsed); err != nil {
		return fmt.Errorf("Codex config: %w", err)
	}
	managed := bytes.Contains(original, []byte(rootBegin))
	if !managed {
		if _, ok := obj(parsed["model_providers"])["codex-bridge"]; ok {
			return fmt.Errorf("provider codex-bridge already exists and is not managed by this utility")
		}
		if str(parsed["model_catalog_json"]) != "" {
			return fmt.Errorf("existing custom model_catalog_json must be merged manually before sync")
		}
	}
	base, err := bundledCatalog()
	if err != nil {
		return err
	}
	catalog, err := makeCatalog(base, c)
	if err != nil {
		return err
	}
	token, err := localToken(path)
	if err != nil {
		return err
	}
	b, err := stripBlock(original, rootBegin, rootEnd)
	if err != nil {
		return err
	}
	b, err = stripBlock(b, providerBegin, providerEnd)
	if err != nil {
		return err
	}
	b, _, err = removeRootStrings(b, map[string]bool{"model_provider": true, "model_catalog_json": true})
	if err != nil {
		return err
	}
	root, err := toml.Marshal(object{"model_provider": "codex-bridge", "model_catalog_json": catalogPath})
	if err != nil {
		return err
	}
	provider, err := toml.Marshal(object{"model_providers": object{"codex-bridge": object{"name": "Codex Bridge", "base_url": "http://" + c.Listen + "/v1", "wire_api": "responses", "requires_openai_auth": true, "supports_websockets": false, "http_headers": object{"X-Codex-Bridge-Token": token}}}})
	if err != nil {
		return err
	}
	next := append([]byte(rootBegin), root...)
	next = append(next, []byte(rootEnd)...)
	next = append(next, bytes.TrimRight(b, "\r\n")...)
	next = append(next, []byte("\n\n"+providerBegin)...)
	next = append(next, provider...)
	next = append(next, []byte(providerEnd)...)
	if err = toml.Unmarshal(next, &parsed); err != nil {
		return fmt.Errorf("generated config: %w", err)
	}
	if !managed {
		if err = atomicWrite(target+".bridge-backup", original); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(catalog, "", "  ")
	if err != nil {
		return err
	}
	if err = atomicWrite(catalogPath, data); err != nil {
		return err
	}
	current, e := os.ReadFile(target)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	if !bytes.Equal(current, original) {
		return fmt.Errorf("Codex config changed during sync; retry")
	}
	if err = atomicWrite(target, next); err != nil {
		return err
	}
	fmt.Printf("Synced %d external models plus %d built-in models to %s. Restart Codex to refresh /model.\n", len(c.Models), len(arr(base["models"])), target)
	return nil
}

func unsyncCodex() error {
	target := filepath.Join(codexDir(), "config.toml")
	original, err := os.ReadFile(target)
	if err != nil {
		return err
	}
	if !bytes.Contains(original, []byte(rootBegin)) {
		return fmt.Errorf("Codex config has no managed bridge settings")
	}
	backup, err := os.ReadFile(target + ".bridge-backup")
	if err != nil {
		return err
	}
	_, prior, err := removeRootStrings(backup, map[string]bool{"model_provider": true, "model_catalog_json": true})
	if err != nil {
		return err
	}
	b, err := stripBlock(original, rootBegin, rootEnd)
	if err != nil {
		return err
	}
	b, err = stripBlock(b, providerBegin, providerEnd)
	if err != nil {
		return err
	}
	values := object{}
	for k, v := range prior {
		values[k] = v
	}
	prefix, err := toml.Marshal(values)
	if err != nil {
		return err
	}
	result := append(prefix, b...)
	var parsed object
	if err = toml.Unmarshal(result, &parsed); err != nil {
		return err
	}
	if err = atomicWrite(target, result); err != nil {
		return err
	}
	fmt.Println("Restored previous provider settings; unrelated Codex settings preserved.")
	return nil
}
