package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type toolIdentity struct {
	Name, Namespace string
	Custom          bool
}
type toolMap map[string]toolIdentity

// Summary-only reasoning from Chat Completions has no server-side Responses item to retrieve.
func normalizeResponsesInput(req object) {
	input, ok := req["input"].([]any)
	if !ok {
		return
	}
	out := make([]any, 0, len(input))
	for _, raw := range input {
		item := obj(raw)
		if str(item["type"]) == "reasoning" && str(item["encrypted_content"]) == "" {
			var summary []string
			for _, part := range arr(item["summary"]) {
				if text := str(obj(part)["text"]); text != "" {
					summary = append(summary, text)
				}
			}
			if len(summary) > 0 {
				out = append(out, object{"type": "message", "role": "assistant", "content": []any{object{"type": "output_text", "text": "Previous reasoning summary: " + strings.Join(summary, "\n")}}})
			}
			continue
		}
		id := str(item["id"])
		for _, prefix := range []string{"msg_bridge_", "fc_bridge_", "rs_bridge_"} {
			if strings.HasPrefix(id, prefix) {
				delete(item, "id")
				break
			}
		}
		out = append(out, raw)
	}
	req["input"] = out
}

func toolName(namespace, name string) string {
	if namespace == "" && len(name) <= 64 && !strings.ContainsAny(name, " .:/") {
		return name
	}
	sum := sha256.Sum256([]byte(namespace + "\x00" + name))
	return "bridge_" + hex.EncodeToString(sum[:16])
}

func translateTools(input []any) ([]any, toolMap, error) {
	out := []any{}
	names := toolMap{}
	var add func(object, string) error
	add = func(t object, ns string) error {
		kind := str(t["type"])
		// shortcut: Chat Completions has no hosted web search; use provider params or an MCP search tool when needed.
		if kind == "web_search" || kind == "web_search_preview" {
			return nil
		}
		if kind == "namespace" {
			for _, child := range arr(t["tools"]) {
				if err := add(obj(child), str(t["name"])); err != nil {
					return err
				}
			}
			return nil
		}
		if kind != "function" && kind != "custom" {
			return fmt.Errorf("Chat Completions cannot execute Responses tool type %q; disable that hosted tool", kind)
		}
		name := str(t["name"])
		if name == "" {
			return fmt.Errorf("tool has no name")
		}
		alias := toolName(ns, name)
		if _, exists := names[alias]; exists {
			return fmt.Errorf("duplicate tool identity %s", name)
		}
		names[alias] = toolIdentity{name, ns, kind == "custom"}
		f := object{"name": alias}
		for _, k := range []string{"description", "parameters", "strict"} {
			if v, ok := t[k]; ok {
				f[k] = v
			}
		}
		if kind == "custom" {
			f["parameters"] = object{"type": "object", "properties": object{"input": object{"type": "string", "description": "The complete raw input to the tool."}}, "required": []string{"input"}, "additionalProperties": false}
			description := str(t["description"])
			description = strings.ReplaceAll(description, "This is a FREEFORM tool, so do not wrap the patch in JSON.", "")
			f["description"] = description + "\nPass the entire raw tool input as the JSON string property input."
			if name == "apply_patch" {
				f["description"] = str(f["description"]) + "\nPatch syntax: first line exactly *** Begin Patch; last line exactly *** End Patch. Create files with *** Add File: path followed by lines prefixed with +. Delete with *** Delete File: path. Edit with *** Update File: path, then @@ context markers and lines prefixed with space, - or +. Do not use unified diff headers or line numbers. Example input:\n*** Begin Patch\n*** Add File: hello.txt\n+Hello\n*** End Patch"
			} else if format := obj(t["format"]); format != nil {
				encoded, _ := json.Marshal(format)
				f["description"] = str(f["description"]) + "\nThe raw input must obey this format: " + string(encoded)
			}
		}
		out = append(out, object{"type": "function", "function": f})
		return nil
	}
	for _, t := range input {
		if err := add(obj(t), ""); err != nil {
			return nil, nil, err
		}
	}
	return out, names, nil
}

func contentParts(v any) (any, error) {
	if s, ok := v.(string); ok {
		return s, nil
	}
	out := []any{}
	for _, raw := range arr(v) {
		p := obj(raw)
		switch str(p["type"]) {
		case "input_text", "output_text", "text":
			out = append(out, object{"type": "text", "text": str(p["text"])})
		case "input_image":
			u := str(p["image_url"])
			if u == "" {
				return nil, fmt.Errorf("input_image must use image_url; file IDs cannot be translated")
			}
			image := object{"url": u}
			if d := str(p["detail"]); d != "" {
				if d == "original" {
					d = "high"
				}
				image["detail"] = d
			}
			out = append(out, object{"type": "image_url", "image_url": image})
		case "refusal":
			out = append(out, object{"type": "text", "text": str(p["refusal"])})
		default:
			return nil, fmt.Errorf("cannot translate content type %q to Chat Completions", str(p["type"]))
		}
	}
	if len(out) == 1 && obj(out[0])["type"] == "text" {
		return obj(out[0])["text"], nil
	}
	return out, nil
}

func toChat(req object) (object, toolMap, error) {
	if str(req["previous_response_id"]) != "" {
		return nil, nil, fmt.Errorf("previous_response_id is unsupported: send full input history (Codex HTTP does this)")
	}
	tools, names, err := translateTools(arr(req["tools"]))
	if err != nil {
		return nil, nil, err
	}
	messages := []any{}
	if s := str(req["instructions"]); s != "" {
		messages = append(messages, object{"role": "system", "content": s})
	}
	for _, raw := range arr(req["tools"]) {
		if kind := str(obj(raw)["type"]); kind == "web_search" || kind == "web_search_preview" {
			messages = append(messages, object{"role": "system", "content": "Hosted web search is unavailable on this Chat Completions connection. Use only the supplied function tools; do not claim to have searched the web without a search tool result."})
			break
		}
	}
	var assistant object
	flush := func() {
		if assistant != nil {
			messages = append(messages, assistant)
			assistant = nil
		}
	}
	ensure := func() object {
		if assistant == nil {
			assistant = object{"role": "assistant", "content": nil}
		}
		return assistant
	}
	inputs := arr(req["input"])
	if s, ok := req["input"].(string); ok {
		inputs = []any{object{"type": "message", "role": "user", "content": s}}
	}
	pending := map[string]bool{}
	for _, raw := range inputs {
		item := obj(raw)
		kind := str(item["type"])
		switch kind {
		case "message", "":
			role := str(item["role"])
			if role == "developer" {
				role = "system"
			}
			if role != "system" && role != "user" && role != "assistant" {
				return nil, nil, fmt.Errorf("unsupported message role %q", role)
			}
			content, e := contentParts(item["content"])
			if e != nil {
				return nil, nil, e
			}
			if role == "assistant" {
				a := ensure()
				if a["content"] != nil {
					flush()
					a = ensure()
				}
				a["content"] = content
			} else {
				flush()
				messages = append(messages, object{"role": role, "content": content})
			}
		case "reasoning":
			var parts []string
			for _, p := range arr(item["summary"]) {
				if s := str(obj(p)["text"]); s != "" {
					parts = append(parts, s)
				}
			}
			if len(parts) > 0 {
				a := ensure()
				a["reasoning_content"] = str(a["reasoning_content"]) + strings.Join(parts, "\n")
			}
		case "function_call", "custom_tool_call":
			id := str(item["call_id"])
			if id == "" || pending[id] {
				return nil, nil, fmt.Errorf("missing or duplicate tool call ID")
			}
			pending[id] = true
			name := toolName(str(item["namespace"]), str(item["name"]))
			args := str(item["arguments"])
			if kind == "custom_tool_call" {
				b, _ := json.Marshal(object{"input": str(item["input"])})
				args = string(b)
			}
			a := ensure()
			a["tool_calls"] = append(arr(a["tool_calls"]), object{"id": id, "type": "function", "function": object{"name": name, "arguments": args}})
		case "function_call_output", "custom_tool_call_output":
			flush()
			id := str(item["call_id"])
			if !pending[id] {
				return nil, nil, fmt.Errorf("tool output %q has no preceding call", id)
			}
			delete(pending, id)
			output, e := contentParts(item["output"])
			if e != nil {
				return nil, nil, e
			}
			messages = append(messages, object{"role": "tool", "tool_call_id": id, "content": output})
		default:
			return nil, nil, fmt.Errorf("cannot translate Responses input type %q", kind)
		}
	}
	flush()
	if len(pending) > 0 {
		return nil, nil, fmt.Errorf("tool calls are missing their results")
	}
	out := object{"model": req["model"], "messages": messages, "stream": true, "stream_options": object{"include_usage": true}}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	for _, key := range []string{"temperature", "top_p", "parallel_tool_calls", "seed", "user"} {
		if v, ok := req[key]; ok {
			out[key] = v
		}
	}
	if v, ok := req["max_output_tokens"]; ok {
		out["max_tokens"] = v
	}
	if effort := str(obj(req["reasoning"])["effort"]); effort != "" {
		out["reasoning_effort"] = effort
	}
	if choice, ok := req["tool_choice"]; ok {
		if s, ok := choice.(string); ok {
			out["tool_choice"] = s
		} else {
			c := obj(choice)
			kind := str(c["type"])
			if kind != "function" && kind != "custom" {
				return nil, nil, fmt.Errorf("unsupported tool_choice type %q", kind)
			}
			out["tool_choice"] = object{"type": "function", "function": object{"name": toolName(str(c["namespace"]), str(c["name"]))}}
		}
	}
	if format := obj(obj(req["text"])["format"]); format != nil {
		switch str(format["type"]) {
		case "json_schema":
			schema := object{}
			for _, k := range []string{"name", "description", "schema", "strict"} {
				if v, ok := format[k]; ok {
					schema[k] = v
				}
			}
			out["response_format"] = object{"type": "json_schema", "json_schema": schema}
		case "text", "json_object":
			out["response_format"] = format
		default:
			return nil, nil, fmt.Errorf("unsupported text.format")
		}
	}
	return out, names, nil
}
