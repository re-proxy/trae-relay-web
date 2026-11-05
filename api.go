package main

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

type ToolSpec struct {
	Name, Namespace string
	Custom          bool
}

func toolName(name, ns string) string {
	clean := func(s string) string {
		out := ""
		for _, r := range s {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
				out += string(r)
			} else {
				out += "_"
			}
		}
		if out == "" {
			out = "tool"
		}
		if len(out) > 64 {
			h := sha512.Sum512([]byte(s))
			out = out[:54] + hex.EncodeToString(h[:])[:10]
		}
		return out
	}
	if ns == "" {
		return clean(name)
	}
	n, s := clean(name), clean(ns)
	if len(n) > 20 {
		n = n[:20]
	}
	if len(s) > 20 {
		s = s[:20]
	}
	h := sha512.Sum512([]byte(ns + "\n" + name))
	return s + "__" + n + "_" + hex.EncodeToString(h[:])[:12]
}
func responseTools(raw []any) ([]M, map[string]ToolSpec, error) {
	out := []M{}
	spec := map[string]ToolSpec{}
	var add func(M, string) error
	add = func(t M, ns string) error {
		typ := str(t, "type")
		if typ == "namespace" {
			for _, v := range list(t["tools"]) {
				if e := add(obj(v), str(t, "name")); e != nil {
					return e
				}
			}
			return nil
		}
		if typ != "function" && typ != "custom" {
			return errors.New("unsupported_tool_type")
		}
		name := str(t, "name")
		if name == "" {
			return errors.New("missing_tool_name")
		}
		internal := toolName(name, ns)
		if _, ok := spec[internal]; ok {
			return errors.New("duplicate_tool_name")
		}
		spec[internal] = ToolSpec{name, ns, typ == "custom"}
		params := t["parameters"]
		if typ == "custom" {
			params = M{"type": "object", "properties": M{"input": M{"type": "string"}}, "required": []string{"input"}}
		}
		out = append(out, M{"type": "function", "function": M{"name": internal, "description": str(t, "description"), "parameters": params}})
		return nil
	}
	for _, v := range raw {
		if e := add(obj(v), ""); e != nil {
			return nil, nil, e
		}
	}
	return out, spec, nil
}
func responseInput(body M) ([]M, error) {
	out := []M{}
	switch input := body["input"].(type) {
	case string:
		out = append(out, M{"role": "user", "content": input})
	case []any:
		for _, raw := range input {
			v := obj(raw)
			switch str(v, "type") {
			case "function_call":
				name := toolName(str(v, "name"), str(v, "namespace"))
				out = append(out, M{"role": "assistant", "content": "", "tool_calls": []any{M{"id": str(v, "call_id"), "type": "function", "function": M{"name": name, "arguments": str(v, "arguments")}}}})
			case "custom_tool_call":
				b, _ := json.Marshal(M{"input": str(v, "input")})
				out = append(out, M{"role": "assistant", "content": "", "tool_calls": []any{M{"id": str(v, "call_id"), "type": "function", "function": M{"name": toolName(str(v, "name"), str(v, "namespace")), "arguments": string(b)}}}})
			case "function_call_output", "custom_tool_call_output":
				out = append(out, M{"role": "tool", "tool_call_id": str(v, "call_id"), "content": v["output"]})
			case "reasoning":
				continue
			case "message", "":
				role := str(v, "role")
				if role == "" {
					role = "user"
				}
				out = append(out, M{"role": role, "content": v["content"]})
			default:
				return nil, errors.New("unsupported_input_type")
			}
		}
	default:
		return nil, errors.New("input_required")
	}
	return out, nil
}

type stopFilter struct {
	pending string
	stops   []string
	stopped bool
}

func (f *stopFilter) feed(s string, final bool) string {
	if f.stopped {
		return ""
	}
	f.pending += s
	cut := len(f.pending)
	found := false
	for _, stop := range f.stops {
		if i := strings.Index(f.pending, stop); i >= 0 && i < cut {
			cut = i
			found = true
		}
	}
	if found {
		out := f.pending[:cut]
		f.pending = ""
		f.stopped = true
		return out
	}
	keep := 0
	if !final {
		for _, stop := range f.stops {
			for n := 1; n < len(stop) && n <= len(f.pending); n++ {
				if strings.HasSuffix(f.pending, stop[:n]) && n > keep {
					keep = n
				}
			}
		}
	}
	n := len(f.pending) - keep
	for n > 0 && !utf8.ValidString(f.pending[:n]) {
		n--
	}
	out := f.pending[:n]
	f.pending = f.pending[n:]
	return out
}

type downstream struct {
	w                          http.ResponseWriter
	ctx                        context.Context
	stream, responses, started bool
	id, model                  string
	created                    int64
	seq                        int
	output                     []M
	indices                    map[string]int
	spec                       map[string]ToolSpec
	text, reason               string
}

func (d *downstream) send(name string, v any) error {
	if !d.stream {
		return nil
	}
	if e := d.ctx.Err(); e != nil {
		return e
	}
	if !d.started {
		d.w.Header().Set("Content-Type", "text/event-stream")
		d.w.Header().Set("X-Accel-Buffering", "no")
		d.w.WriteHeader(200)
		d.started = true
	}
	_ = http.NewResponseController(d.w).SetWriteDeadline(time.Now().Add(30 * time.Second))
	b, _ := json.Marshal(v)
	var e error
	if name != "" {
		_, e = fmt.Fprintf(d.w, "event: %s\ndata: %s\n\n", name, b)
	} else {
		_, e = fmt.Fprintf(d.w, "data: %s\n\n", b)
	}
	if e == nil {
		e = http.NewResponseController(d.w).Flush()
	}
	return e
}
func (d *downstream) event(name string, m M) error {
	m["type"] = name
	m["sequence_number"] = d.seq
	d.seq++
	return d.send(name, m)
}
func (d *downstream) response(status string, usage M) M {
	return M{"id": d.id, "object": "response", "created_at": d.created, "status": status, "model": d.model, "output": d.output, "usage": usage, "error": nil}
}
func (d *downstream) start() error {
	if !d.stream {
		return nil
	}
	if d.responses {
		if e := d.event("response.created", M{"response": d.response("in_progress", nil)}); e != nil {
			return e
		}
		return d.event("response.in_progress", M{"response": d.response("in_progress", nil)})
	}
	return d.send("", M{"id": d.id, "object": "chat.completion.chunk", "created": d.created, "model": d.model, "choices": []M{{"index": 0, "delta": M{"role": "assistant", "content": ""}, "finish_reason": nil}}})
}
func (d *downstream) ensure(kind string) (int, error) {
	if i, ok := d.indices[kind]; ok {
		return i, nil
	}
	i := len(d.output)
	d.indices[kind] = i
	item := M{"id": "msg_" + randomID(), "type": "message", "role": "assistant", "status": "in_progress", "content": []M{}}
	if kind == "reason" {
		item = M{"id": "rs_" + randomID(), "type": "reasoning", "summary": []M{}}
	}
	d.output = append(d.output, item)
	if e := d.event("response.output_item.added", M{"output_index": i, "item": item}); e != nil {
		return i, e
	}
	if kind == "reason" {
		return i, d.event("response.reasoning_summary_part.added", M{"output_index": i, "item_id": item["id"], "summary_index": 0, "part": M{"type": "summary_text", "text": ""}})
	}
	return i, d.event("response.content_part.added", M{"output_index": i, "item_id": item["id"], "content_index": 0, "part": M{"type": "output_text", "text": "", "annotations": []any{}}})
}
func (d *downstream) emit(e Event) error {
	if !d.responses {
		delta := M{}
		switch e.Kind {
		case "text":
			d.text += e.Text
			delta["content"] = e.Text
		case "reason":
			d.reason += e.Text
			delta["reasoning_content"] = e.Text
		case "tool":
			delta["tool_calls"] = []M{{"index": e.Tool.Index, "id": e.Tool.ID, "type": "function", "function": M{"name": e.Tool.Name, "arguments": e.Tool.Args}}}
		}
		return d.send("", M{"id": d.id, "object": "chat.completion.chunk", "created": d.created, "model": d.model, "choices": []M{{"index": 0, "delta": delta, "finish_reason": nil}}})
	}
	if e.Kind == "tool" {
		tc := e.Tool
		spec := d.spec[tc.Name]
		name := spec.Name
		if name == "" {
			name = tc.Name
		}
		item := M{"id": "fc_" + randomID(), "type": "function_call", "call_id": tc.ID, "name": name, "arguments": tc.Args, "status": "completed"}
		field := "arguments"
		event := "response.function_call_arguments"
		value := tc.Args
		if spec.Custom {
			var v M
			_ = json.Unmarshal([]byte(tc.Args), &v)
			value = str(v, "input")
			item["type"] = "custom_tool_call"
			delete(item, "arguments")
			item["input"] = value
			field = "input"
			event = "response.custom_tool_call_input"
		}
		if spec.Namespace != "" {
			item["namespace"] = spec.Namespace
		}
		index := len(d.output)
		d.output = append(d.output, item)
		start := M{}
		for k, v := range item {
			start[k] = v
		}
		start[field] = ""
		start["status"] = "in_progress"
		if e := d.event("response.output_item.added", M{"output_index": index, "item": start}); e != nil {
			return e
		}
		if e := d.event(event+".delta", M{"output_index": index, "item_id": item["id"], "delta": value}); e != nil {
			return e
		}
		if e := d.event(event+".done", M{"output_index": index, "item_id": item["id"], field: value}); e != nil {
			return e
		}
		return d.event("response.output_item.done", M{"output_index": index, "item": item})
	}
	i, err := d.ensure(e.Kind)
	if err != nil {
		return err
	}
	name := "response.output_text.delta"
	m := M{"output_index": i, "item_id": d.output[i]["id"], "delta": e.Text}
	if e.Kind == "reason" {
		d.reason += e.Text
		name = "response.reasoning_summary_text.delta"
		m["summary_index"] = 0
	} else {
		d.text += e.Text
		m["content_index"] = 0
	}
	return d.event(name, m)
}
func (d *downstream) finish(result Result) error {
	if d.responses {
		for kind, i := range d.indices {
			item := d.output[i]
			if kind == "text" {
				part := M{"type": "output_text", "text": d.text, "annotations": []any{}}
				item["content"] = []M{part}
				item["status"] = "completed"
				if e := d.event("response.output_text.done", M{"output_index": i, "item_id": item["id"], "content_index": 0, "text": d.text}); e != nil {
					return e
				}
				if e := d.event("response.content_part.done", M{"output_index": i, "item_id": item["id"], "content_index": 0, "part": part}); e != nil {
					return e
				}
			} else {
				part := M{"type": "summary_text", "text": d.reason}
				item["summary"] = []M{part}
				if e := d.event("response.reasoning_summary_text.done", M{"output_index": i, "item_id": item["id"], "summary_index": 0, "text": d.reason}); e != nil {
					return e
				}
				if e := d.event("response.reasoning_summary_part.done", M{"output_index": i, "item_id": item["id"], "summary_index": 0, "part": part}); e != nil {
					return e
				}
			}
			if e := d.event("response.output_item.done", M{"output_index": i, "item": item}); e != nil {
				return e
			}
		}
		v := d.response("completed", usageResponses(result.Usage))
		if !d.stream {
			writeJSON(d.w, 200, v)
			return nil
		}
		return d.event("response.completed", M{"response": v})
	}
	if !d.stream {
		msg := M{"role": "assistant", "content": d.text}
		if d.reason != "" {
			msg["reasoning_content"] = d.reason
		}
		if len(result.Tools) > 0 {
			calls := []M{}
			for _, tc := range result.Tools {
				calls = append(calls, M{"id": tc.ID, "type": "function", "function": M{"name": tc.Name, "arguments": tc.Args}})
			}
			msg["tool_calls"] = calls
		}
		writeJSON(d.w, 200, M{"id": d.id, "object": "chat.completion", "created": d.created, "model": d.model, "choices": []M{{"index": 0, "message": msg, "finish_reason": result.Finish}}, "usage": result.Usage})
		return nil
	}
	if e := d.send("", M{"id": d.id, "object": "chat.completion.chunk", "created": d.created, "model": d.model, "choices": []M{{"index": 0, "delta": M{}, "finish_reason": result.Finish}}, "usage": result.Usage}); e != nil {
		return e
	}
	_, e := fmt.Fprint(d.w, "data: [DONE]\n\n")
	if e == nil {
		e = http.NewResponseController(d.w).Flush()
	}
	return e
}

// upstreamFailure maps an upstream failure to the downstream status and error
// type so a throttled, expired or rejected call stays visible to the client.
func upstreamFailure(e error) (int, string, string, string) {
	status, kind, code, message := 502, "api_error", "upstream_request_failed", "Upstream request failed"
	var u *UpstreamError
	if !errors.As(e, &u) {
		return status, kind, code, message
	}
	switch {
	case u.Code == 1001 || u.Status == 401:
		status, kind, code = 401, "authentication_error", "upstream_authentication_failed"
	case u.Code == 1005:
		status, kind, code = 403, "insufficient_quota", "upstream_insufficient_quota"
	case u.Code == 429 || u.Code == 4008 || u.Code == 4011 || u.Status == 429:
		status, kind, code = 429, "rate_limit_error", "upstream_rate_limited"
	case u.Code == 4001:
		status, kind, code = 400, "invalid_request_error", "upstream_invalid_request"
	case u.Cause != "":
		code = u.Cause
	}
	if u.Message != "" {
		message = u.Message
	}
	return status, kind, code, message
}

func (a *App) chat(w http.ResponseWriter, r *http.Request, responses bool) {
	var body M
	if decode(w, r, &body) != nil {
		fail(w, 400, "invalid_json_or_body_too_large")
		return
	}
	if boolean(body, "background", false) {
		fail(w, 400, "background_not_supported")
		return
	}
	if str(body, "model") == "" {
		fail(w, 400, "model_required")
		return
	}
	var messages []M
	tools := []M{}
	spec := map[string]ToolSpec{}
	var e error
	if responses {
		messages, e = responseInput(body)
		if e == nil {
			tools, spec, e = responseTools(list(body["tools"]))
		}
	} else {
		for _, v := range list(body["messages"]) {
			messages = append(messages, obj(v))
		}
		for _, v := range list(body["tools"]) {
			t := obj(v)
			if str(t, "type") != "function" || str(obj(t["function"]), "name") == "" {
				e = errors.New("unsupported_tool_type")
				break
			}
			tools = append(tools, t)
		}
	}
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	if choice, ok := body["tool_choice"].(string); ok {
		switch choice {
		case "none":
			tools = nil
		case "auto", "required":
		default:
			fail(w, 400, "invalid_tool_choice")
			return
		}
	} else if choice := obj(body["tool_choice"]); len(choice) > 0 {
		name := str(obj(choice["function"]), "name")
		if name == "" {
			name = str(choice, "name")
			if responses {
				name = toolName(name, str(choice, "namespace"))
			}
		}
		selected := []M{}
		for _, tool := range tools {
			if str(obj(tool["function"]), "name") == name {
				selected = append(selected, tool)
			}
		}
		if len(selected) == 0 {
			fail(w, 400, "unknown_tool_choice")
			return
		}
		tools = selected
	}
	if len(messages) == 0 {
		fail(w, 400, "messages_required")
		return
	}
	acc, e := a.pick(r.Context(), nil)
	if e != nil {
		if e == errNoAccount {
			fail(w, 503, "no_enabled_account")
		} else {
			fail(w, 503, "account_concurrency_limit")
		}
		return
	}
	defer a.release(acc.ID)
	if responses {
		if prev := str(body, "previous_response_id"); prev != "" {
			a.mu.Lock()
			session, ok := a.sessions[acc.ID+":"+prev]
			a.mu.Unlock()
			if !ok {
				fail(w, 400, "previous_response_not_found")
				return
			}
			messages = append(append([]M{}, session.Messages...), messages...)
		}
		if instruction := str(body, "instructions"); instruction != "" {
			messages = append([]M{{"role": "system", "content": instruction}}, messages...)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.Timeout)
	defer cancel()
	models, e := a.getModels(ctx, acc, false)
	if e != nil {
		fail(w, 502, "model_catalog_unavailable")
		return
	}
	var model Model
	var setting ModelSetting
	for _, m := range models {
		st := a.store.setting(m.ID)
		if !st.Disabled && (strings.EqualFold(m.ID, str(body, "model")) || st.Alias != "" && strings.EqualFold(st.Alias, str(body, "model"))) {
			model = m
			setting = st
			break
		}
	}
	if model.ID == "" {
		fail(w, 400, "model_not_available")
		return
	}
	if value, ok := body["is_max_mode"].(float64); ok {
		setting.Max = value != 0
	} else if value, ok := body["is_max_mode"].(bool); ok {
		setting.Max = value
	}
	effort := str(body, "reasoning_effort")
	if responses {
		effort = str(obj(body["reasoning"]), "effort")
	}
	up, e := upstreamBody(messages, tools, model, setting, effort)
	if e != nil {
		fail(w, 400, e.Error())
		return
	}
	if !model.Vision {
		for _, raw := range up["messages"].([]M) {
			for _, p := range raw["content"].([]M) {
				if p["type"] == "image_url" {
					fail(w, 400, "model_does_not_support_images")
					return
				}
			}
		}
	}
	acc, e = a.fresh(ctx, acc, false)
	if e != nil {
		fail(w, 502, "account_refresh_failed")
		return
	}
	filter := stopFilter{}
	switch stop := body["stop"].(type) {
	case string:
		if len(stop) > 1024 {
			fail(w, 400, "invalid_stop")
			return
		}
		if stop != "" {
			filter.stops = []string{stop}
		}
	case []any:
		for _, v := range stop {
			if s, ok := v.(string); ok && s != "" && len(s) <= 1024 {
				filter.stops = append(filter.stops, s)
			} else {
				fail(w, 400, "invalid_stop")
				return
			}
		}
	}
	if len(filter.stops) > 4 {
		fail(w, 400, "too_many_stop_sequences")
		return
	}
	stream := boolean(body, "stream", false)
	d := &downstream{w: w, ctx: ctx, stream: stream, responses: responses, id: "chatcmpl_" + randomID(), model: str(body, "model"), created: time.Now().Unix(), indices: map[string]int{}, spec: spec, output: []M{}}
	if responses {
		d.id = "resp_" + randomID()
	}
	started := time.Now()
	began := false
	result, e := a.runUpstream(ctx, acc, up, func(ev Event) error {
		if !began {
			began = true
			if err := d.start(); err != nil {
				return err
			}
		}
		if ev.Kind == "text" {
			ev.Text = filter.feed(ev.Text, false)
			if ev.Text == "" {
				return nil
			}
		}
		return d.emit(ev)
	})
	if e == nil {
		if !began {
			e = d.start()
		}
		if e == nil {
			if tail := filter.feed("", true); tail != "" {
				e = d.emit(Event{Kind: "text", Text: tail})
			}
		}
		if filter.stopped {
			result.Finish = "stop"
		}
		if e == nil && responses && boolean(body, "store", true) {
			assistant := M{"role": "assistant", "content": d.text}
			if len(result.Tools) > 0 {
				calls := []any{}
				for _, tc := range result.Tools {
					calls = append(calls, M{"id": tc.ID, "type": "function", "function": M{"name": tc.Name, "arguments": tc.Args}})
				}
				assistant["tool_calls"] = calls
			}
			history := append(append([]M{}, messages...), assistant)
			encoded, _ := json.Marshal(history)
			if len(encoded) <= 4<<20 {
				a.mu.Lock()
				if len(a.sessions) >= 64 {
					var oldestKey string
					var oldest time.Time
					for k, s := range a.sessions {
						if oldestKey == "" || s.At.Before(oldest) {
							oldestKey = k
							oldest = s.At
						}
					}
					delete(a.sessions, oldestKey)
				}
				a.sessions[acc.ID+":"+d.id] = Session{history, time.Now()}
				a.mu.Unlock()
			}
		}
		if e == nil {
			e = d.finish(result)
		}
	}
	_ = a.store.record(Usage{Time: time.Now(), Model: model.ID, Account: acc.ID, Tokens: result.Usage, Success: e == nil, DurationMS: time.Since(started).Milliseconds()})
	if e != nil {
		status, kind, code, message := upstreamFailure(e)
		log.Printf("upstream failure model=%s account=%s status=%d code=%s message=%s", model.ID, acc.ID, status, code, message)
		if !d.started {
			writeJSON(w, status, M{"error": M{"type": kind, "code": code, "message": message}})
		} else if responses {
			_ = d.event("response.failed", M{"response": M{"id": d.id, "object": "response", "status": "failed", "error": M{"type": kind, "code": code, "message": message}}})
		} else {
			_ = d.send("", M{"error": M{"type": kind, "code": code, "message": message}})
		}
	}
}
