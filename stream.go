package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + "bridge_" + hex.EncodeToString(b[:])
}

func readSSE(r io.Reader, fn func([]byte) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 16<<20)
	var data []string
	size := 0
	dispatch := func() error {
		if len(data) == 0 {
			return nil
		}
		b := []byte(strings.Join(data, "\n"))
		data = nil
		size = 0
		return fn(b)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
		} else if strings.HasPrefix(line, "data:") {
			size += len(line)
			if size > 16<<20 {
				return fmt.Errorf("upstream SSE event exceeds 16 MiB")
			}
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return dispatch()
}

type callDelta struct {
	ID, Name  string
	Arguments strings.Builder
}
type responseStream struct {
	w                            http.ResponseWriter
	streaming                    bool
	seq                          int
	response                     object
	output                       []any
	message, reasoning           object
	messageIndex, reasoningIndex int
	text, thought                strings.Builder
	calls                        map[int]*callDelta
	names                        toolMap
	finish                       string
	size                         int
}

func newResponse(w http.ResponseWriter, streaming bool, model string, names toolMap) *responseStream {
	return &responseStream{w: w, streaming: streaming, names: names, calls: map[int]*callDelta{}, output: []any{}, response: object{"id": newID("resp_"), "object": "response", "created_at": time.Now().Unix(), "model": model, "status": "in_progress", "output": []any{}, "error": nil, "incomplete_details": nil}}
}
func (s *responseStream) event(kind string, fields object) error {
	if !s.streaming {
		return nil
	}
	fields["type"] = kind
	fields["sequence_number"] = s.seq
	s.seq++
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	if _, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", kind, b); err != nil {
		return err
	}
	return http.NewResponseController(s.w).Flush()
}
func (s *responseStream) start() error {
	if s.streaming {
		s.w.Header().Set("Content-Type", "text/event-stream")
		s.w.Header().Set("Cache-Control", "no-cache")
	}
	if err := s.event("response.created", object{"response": s.response}); err != nil {
		return err
	}
	return s.event("response.in_progress", object{"response": s.response})
}
func (s *responseStream) addItem(item object) (int, error) {
	idx := len(s.output)
	s.output = append(s.output, item)
	return idx, s.event("response.output_item.added", object{"output_index": idx, "item": item})
}
func (s *responseStream) textDelta(delta string, reasoning bool) error {
	if delta == "" {
		return nil
	}
	s.size += len(delta)
	if s.size > 32<<20 {
		return fmt.Errorf("upstream response exceeds 32 MiB")
	}
	if reasoning {
		if s.reasoning == nil {
			s.reasoning = object{"type": "reasoning", "id": newID("rs_"), "summary": []any{}}
			var err error
			s.reasoningIndex, err = s.addItem(s.reasoning)
			if err != nil {
				return err
			}
			if err = s.event("response.reasoning_summary_part.added", object{"item_id": s.reasoning["id"], "output_index": s.reasoningIndex, "summary_index": 0, "part": object{"type": "summary_text", "text": ""}}); err != nil {
				return err
			}
		}
		s.thought.WriteString(delta)
		return s.event("response.reasoning_summary_text.delta", object{"item_id": s.reasoning["id"], "output_index": s.reasoningIndex, "summary_index": 0, "delta": delta})
	}
	if s.message == nil {
		s.message = object{"type": "message", "id": newID("msg_"), "role": "assistant", "status": "in_progress", "content": []any{}}
		var err error
		s.messageIndex, err = s.addItem(s.message)
		if err != nil {
			return err
		}
		if err = s.event("response.content_part.added", object{"item_id": s.message["id"], "output_index": s.messageIndex, "content_index": 0, "part": object{"type": "output_text", "text": "", "annotations": []any{}}}); err != nil {
			return err
		}
	}
	s.text.WriteString(delta)
	return s.event("response.output_text.delta", object{"item_id": s.message["id"], "output_index": s.messageIndex, "content_index": 0, "delta": delta})
}

func (s *responseStream) chunk(chunk object) error {
	if e := chunk["error"]; e != nil {
		return fmt.Errorf("upstream stream returned an error")
	}
	if u := obj(chunk["usage"]); u != nil {
		s.response["usage"] = object{"input_tokens": u["prompt_tokens"], "output_tokens": u["completion_tokens"], "total_tokens": u["total_tokens"], "input_tokens_details": object{"cached_tokens": number(obj(u["prompt_tokens_details"])["cached_tokens"])}, "output_tokens_details": object{"reasoning_tokens": number(obj(u["completion_tokens_details"])["reasoning_tokens"])}}
	}
	for _, raw := range arr(chunk["choices"]) {
		choice := obj(raw)
		if number(choice["index"]) != 0 {
			continue
		}
		delta := obj(choice["delta"])
		if delta == nil {
			delta = obj(choice["message"])
		}
		thought := str(delta["reasoning_content"])
		if thought == "" {
			thought = str(delta["reasoning"])
		}
		if err := s.textDelta(thought, true); err != nil {
			return err
		}
		if err := s.textDelta(str(delta["content"]), false); err != nil {
			return err
		}
		if refusal := str(delta["refusal"]); refusal != "" {
			if err := s.textDelta(refusal, false); err != nil {
				return err
			}
		}
		for i, raw := range arr(delta["tool_calls"]) {
			tc := obj(raw)
			index := i
			if _, ok := tc["index"]; ok {
				index = int(number(tc["index"]))
			}
			if index < 0 || index > 1024 {
				return fmt.Errorf("invalid tool call index")
			}
			call := s.calls[index]
			if call == nil {
				call = &callDelta{}
				s.calls[index] = call
			}
			if id := str(tc["id"]); id != "" {
				if call.ID != "" && call.ID != id {
					return fmt.Errorf("upstream changed a tool call ID")
				}
				call.ID = id
			}
			f := obj(tc["function"])
			call.Name += str(f["name"])
			call.Arguments.WriteString(str(f["arguments"]))
			s.size += len(str(f["arguments"])) + len(str(f["name"])) + len(str(tc["id"]))
			if s.size > 32<<20 {
				return fmt.Errorf("upstream response exceeds 32 MiB")
			}
		}
		if f := str(choice["finish_reason"]); f != "" {
			s.finish = f
		}
	}
	return nil
}

func number(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case json.Number:
		f, _ := n.Float64()
		return f
	}
	return 0
}

func (s *responseStream) complete() error {
	if s.finish == "" {
		return fmt.Errorf("upstream stream ended without a finish reason")
	}
	status := "completed"
	switch s.finish {
	case "stop", "tool_calls", "function_call":
	case "length":
		status = "incomplete"
		s.response["incomplete_details"] = object{"reason": "max_output_tokens"}
	case "content_filter":
		status = "incomplete"
		s.response["incomplete_details"] = object{"reason": "content_filter"}
	default:
		return fmt.Errorf("unknown upstream finish reason %q", s.finish)
	}
	// Never execute a possibly truncated tool call.
	if status != "completed" && len(s.calls) > 0 {
		return fmt.Errorf("upstream truncated tool arguments (%s)", s.finish)
	}
	indices := make([]int, 0, len(s.calls))
	for i := range s.calls {
		indices = append(indices, i)
	}
	sort.Ints(indices)
	calls := []object{}
	for _, i := range indices {
		c := s.calls[i]
		identity, ok := s.names[c.Name]
		if !ok {
			return fmt.Errorf("upstream called unknown tool %q", c.Name)
		}
		if !json.Valid([]byte(c.Arguments.String())) {
			return fmt.Errorf("upstream returned invalid JSON arguments for %s", identity.Name)
		}
		if c.ID == "" {
			c.ID = newID("call_")
		}
		item := object{"type": "function_call", "id": newID("fc_"), "call_id": c.ID, "name": identity.Name, "arguments": c.Arguments.String(), "status": "completed"}
		if identity.Namespace != "" {
			item["namespace"] = identity.Namespace
		}
		if identity.Custom {
			var args object
			if err := json.Unmarshal([]byte(c.Arguments.String()), &args); err != nil {
				return err
			}
			input, ok := args["input"].(string)
			if !ok {
				return fmt.Errorf("custom tool %s requires a string input", identity.Name)
			}
			item["type"] = "custom_tool_call"
			item["input"] = input
			delete(item, "arguments")
		}
		calls = append(calls, item)
	}
	if s.reasoning != nil {
		part := object{"type": "summary_text", "text": s.thought.String()}
		s.reasoning["summary"] = []any{part}
		for _, kind := range []string{"response.reasoning_summary_text.done", "response.reasoning_summary_part.done"} {
			if err := s.event(kind, object{"item_id": s.reasoning["id"], "output_index": s.reasoningIndex, "summary_index": 0, "text": s.thought.String(), "part": part}); err != nil {
				return err
			}
		}
		if err := s.event("response.output_item.done", object{"output_index": s.reasoningIndex, "item": s.reasoning}); err != nil {
			return err
		}
	}
	if s.message != nil {
		part := object{"type": "output_text", "text": s.text.String(), "annotations": []any{}}
		s.message["content"] = []any{part}
		s.message["status"] = status
		if err := s.event("response.output_text.done", object{"item_id": s.message["id"], "output_index": s.messageIndex, "content_index": 0, "text": s.text.String()}); err != nil {
			return err
		}
		if err := s.event("response.content_part.done", object{"item_id": s.message["id"], "output_index": s.messageIndex, "content_index": 0, "part": part}); err != nil {
			return err
		}
		if err := s.event("response.output_item.done", object{"output_index": s.messageIndex, "item": s.message}); err != nil {
			return err
		}
	}
	for _, item := range calls {
		done := object{}
		merge(done, item)
		item["status"] = "in_progress"
		custom := item["type"] == "custom_tool_call"
		field, event := "arguments", "function_call_arguments"
		if custom {
			field, event = "input", "custom_tool_call_input"
		}
		item[field] = ""
		idx, err := s.addItem(item)
		if err != nil {
			return err
		}
		if err = s.event("response."+event+".delta", object{"item_id": item["id"], "output_index": idx, "delta": done[field]}); err != nil {
			return err
		}
		if err = s.event("response."+event+".done", object{"item_id": item["id"], "output_index": idx, field: done[field]}); err != nil {
			return err
		}
		s.output[idx] = done
		if err = s.event("response.output_item.done", object{"output_index": idx, "item": done}); err != nil {
			return err
		}
	}
	s.response["status"] = status
	s.response["output"] = s.output
	if s.streaming {
		return s.event("response."+status, object{"response": s.response})
	}
	s.w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(s.w).Encode(s.response)
}

func (s *responseStream) fail(err error) {
	s.response["status"] = "failed"
	s.response["error"] = object{"code": "upstream_error", "message": err.Error()}
	s.response["output"] = s.output
	if s.streaming {
		_ = s.event("response.failed", object{"response": s.response})
	} else {
		apiError(s.w, 502, err.Error())
	}
}

func translateResponse(w http.ResponseWriter, up *http.Response, streaming bool, model string, names toolMap) error {
	s := newResponse(w, streaming, model, names)
	if err := s.start(); err != nil {
		return err
	}
	var err error
	if strings.Contains(up.Header.Get("Content-Type"), "text/event-stream") {
		done := errors.New("done")
		err = readSSE(up.Body, func(b []byte) error {
			if string(b) == "[DONE]" {
				return done
			}
			var chunk object
			if e := json.Unmarshal(b, &chunk); e != nil {
				return fmt.Errorf("invalid upstream SSE JSON: %w", e)
			}
			return s.chunk(chunk)
		})
		if errors.Is(err, done) {
			err = nil
		}
	} else {
		var chunk object
		err = json.NewDecoder(io.LimitReader(up.Body, 32<<20)).Decode(&chunk)
		if err == nil {
			err = s.chunk(chunk)
		}
	}
	if err == nil {
		err = s.complete()
	}
	if err != nil {
		s.fail(err)
	}
	return err
}
