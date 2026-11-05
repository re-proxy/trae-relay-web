package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type ToolCall struct {
	ID, Name, Args string
	Index          int
}
type Event struct {
	Kind, Text string
	Tool       ToolCall
}
type Result struct {
	Text, Reason, Finish string
	Tools                []ToolCall
	Usage                M
}

var sanitize = strings.NewReplacer("</opencode_tool_call>", "</tool_invoke>", "<opencode_tool_call>", "<tool_invoke>", "</opencode_tool_result>", "</tool_result>", "<opencode_tool_result", "<tool_result")

func contentParts(v any) ([]M, error) {
	out := []M{}
	if s, ok := v.(string); ok {
		if s != "" {
			out = append(out, M{"type": "text", "text": sanitize.Replace(s)})
		}
		return out, nil
	}
	if v == nil {
		return out, nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, errors.New("invalid_message_content")
	}
	for _, part := range arr {
		p := obj(part)
		switch str(p, "type") {
		case "text", "input_text", "output_text":
			out = append(out, M{"type": "text", "text": sanitize.Replace(str(p, "text"))})
		case "image_url":
			url := str(obj(p["image_url"]), "url")
			if url == "" {
				return nil, errors.New("invalid_image_url")
			}
			out = append(out, M{"type": "image_url", "image_url": M{"url": url}})
		case "input_image":
			url := str(p, "image_url")
			if url == "" {
				return nil, errors.New("invalid_image_url")
			}
			out = append(out, M{"type": "image_url", "image_url": M{"url": url}})
		default:
			return nil, errors.New("unsupported_content_type")
		}
	}
	return out, nil
}
func soloMessages(messages []M) ([]M, error) {
	out := []M{}
	pending := []M{}
	names := map[string]string{}
	for _, m := range messages {
		role := str(m, "role")
		if role == "developer" {
			role = "system"
		}
		if role != "tool" {
			out = append(out, pending...)
			pending = nil
		}
		parts, e := contentParts(m["content"])
		if e != nil {
			return nil, e
		}
		msg := M{"role": role, "content": parts}
		switch role {
		case "system", "user", "assistant", "tool":
		default:
			return nil, errors.New("unsupported_message_role")
		}
		if role == "tool" {
			id := str(m, "tool_call_id")
			if id == "" {
				return nil, errors.New("missing_tool_call_id")
			}
			text, images := []M{}, []M{}
			for _, p := range parts {
				if p["type"] == "image_url" {
					images = append(images, p)
				} else {
					text = append(text, p)
				}
			}
			if len(images) > 0 {
				source, _ := json.Marshal(M{"tool_call_id": id, "tool_name": names[id]})
				ip := []M{{"type": "text", "text": "Tool result image: " + string(source)}}
				ip = append(ip, images...)
				pending = append(pending, M{"role": "user", "content": ip})
				if len(text) == 0 {
					text = append(text, M{"type": "text", "text": "Images follow."})
				}
			}
			msg["content"] = text
			msg["tool_call_id"] = id
			if n := str(m, "name"); n != "" {
				msg["name"] = n
			}
		}
		if role == "assistant" {
			tc := []M{}
			for _, raw := range list(m["tool_calls"]) {
				t := obj(raw)
				f := obj(t["function"])
				name := str(f, "name")
				if name == "" {
					continue
				}
				id := str(t, "id")
				if id == "" {
					id = "call_" + randomID()
				}
				names[id] = name
				args := str(f, "arguments")
				if args == "" {
					args = "{}"
				}
				tc = append(tc, M{"id": id, "type": "function", "function_call": M{"name": name, "arguments": args}})
			}
			if len(tc) > 0 {
				msg["tool_calls"] = tc
			}
		}
		if len(parts) > 0 || msg["tool_calls"] != nil || role == "tool" {
			out = append(out, msg)
		}
	}
	return append(out, pending...), nil
}
func upstreamBody(messages []M, tools []M, model Model, setting ModelSetting, effort string) (M, error) {
	msgs, e := soloMessages(messages)
	if e != nil {
		return nil, e
	}
	session := randomID()
	name := model.ModelName
	body := M{"session_id": session, "conversation_id": session, "messages": msgs, "function": model.Function, "stream": true, "config_name": model.ID, "model": name, "model_name": name, "config_source": model.Source, "is_custom_model": false, "provider": model.Provider}
	if setting.Max && model.Max {
		if model.MaxModel != "" {
			body["model"] = model.MaxModel
			body["model_name"] = model.MaxModel
		}
		cw := model.MaxContext
		if cw <= 0 {
			cw = 1000000
		}
		body["mode_type"] = 1
		body["context_window_size"] = cw
	}
	if effort == "" {
		effort = setting.Effort
	}
	if effort == "" {
		effort = model.DefaultEffort
	}
	for _, v := range model.Efforts {
		if v == effort {
			body["reasoning_effort"] = effort
			break
		}
	}
	if len(tools) > 0 {
		arr := []M{}
		for _, t := range tools {
			f := obj(t["function"])
			params, _ := json.Marshal(f["parameters"])
			if f["parameters"] == nil {
				params = []byte("{}")
			}
			arr = append(arr, M{"type": "function", "function": M{"name": str(f, "name"), "description": str(f, "description"), "parameters": string(params)}})
		}
		body["tools"] = arr
		body["tool_choice"] = "auto"
	}
	return body, nil
}

// parseSSE accepts CRLF, multi-line data, comments and unnamed events. Every line and
// event is bounded; an EOF without done is an upstream failure, never a success.
func parseSSE(r io.Reader, handle func(string, string) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 2<<20)
	event := ""
	data := []string{}
	size := 0
	dispatch := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		e := handle(event, strings.Join(data, "\n"))
		event = ""
		data = nil
		size = 0
		return e
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if e := dispatch(); e != nil {
				return e
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch key {
		case "event":
			event = value
		case "data":
			size += len(value)
			if size > 2<<20 {
				return errors.New("upstream_event_too_large")
			}
			data = append(data, value)
		}
	}
	if e := scanner.Err(); e != nil {
		return errors.New("upstream_stream_read_failed")
	}
	return dispatch()
}

var errStreamDone = errors.New("stream done")

// UpstreamError keeps the upstream status and error payload so a throttled,
// expired or rejected call stays distinguishable instead of collapsing into one
// opaque relay failure.
type UpstreamError struct {
	Status  int    // upstream HTTP status; 0 when the failure arrived inside the stream
	Code    int    // upstream error code from an SSE error event; 0 otherwise
	Message string // upstream text when it supplied one
	Cause   string // relay-side category such as upstream_first_event_timeout
}

func (e *UpstreamError) Error() string {
	switch {
	case e.Code != 0:
		return "upstream_event_error_" + strconv.Itoa(e.Code)
	case e.Status != 0:
		return "upstream_http_" + strconv.Itoa(e.Status)
	default:
		return e.Cause
	}
}

// upstreamText returns the start of a failure body, preferring its message field.
func upstreamText(r io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(r, 512))
	var m M
	if json.Unmarshal(data, &m) == nil {
		if s := str(m, "message"); s != "" {
			return s
		}
		if s := str(obj(m["error"]), "message"); s != "" {
			return s
		}
	}
	s := strings.TrimSpace(string(data))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

func (a *App) runUpstream(ctx context.Context, acc Account, body M, sink func(Event) error) (Result, error) {
	if acc.Scope == "saas" {
		body["conversation_id"] = randomID()
	}
	host := soloHost
	if acc.Scope == "saas" {
		host = enterpriseHost
	}
	encoded, _ := json.Marshal(body)
	for attempt := 0; attempt < 2; attempt++ {
		result, emitted, e := a.streamOnce(ctx, acc, host, string(encoded), str(body, "session_id"), sink)
		if e == nil {
			return result, nil
		}
		var u *UpstreamError
		// An expired token surfaces either as HTTP 401 or as code 1001 inside an
		// accepted stream. Retrying is only safe before anything reached the
		// client, so a partially streamed reply keeps its original error.
		if attempt == 0 && !emitted && errors.As(e, &u) && (u.Status == 401 || u.Code == 1001) {
			if next, err := a.fresh(ctx, acc, true); err == nil {
				acc = next
				continue
			}
		}
		return result, e
	}
	return Result{Finish: "stop", Usage: M{}}, errors.New("upstream_authentication_failed")
}

// streamOnce performs a single upstream attempt and reports whether any event
// already reached the client; that decides whether the caller may still retry.
func (a *App) streamOnce(ctx context.Context, acc Account, host, payload, session string, sink func(Event) error) (Result, bool, error) {
	result := Result{Finish: "stop", Usage: M{}}
	stream, cancel := context.WithCancel(ctx)
	defer cancel()
	req, e := http.NewRequestWithContext(stream, "POST", host+"/api/agent/v3/llm_utils_chat", strings.NewReader(payload))
	if e != nil {
		return result, false, e
	}
	a.headers(req, acc, true)
	resp, e := a.client.Do(req)
	if e != nil {
		return result, false, errors.New("upstream_connection_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return result, false, &UpstreamError{Status: resp.StatusCode, Message: upstreamText(resp.Body)}
	}
	// The reference relay bounds the wait for the first stream event separately
	// from the whole request, because a throttled account can hold the connection
	// open without producing anything.
	var timedOut atomic.Bool
	timer := time.AfterFunc(a.cfg.FirstEvent, func() {
		timedOut.Store(true)
		cancel()
	})
	defer timer.Stop()
	tools := map[int]ToolCall{}
	total := 0
	wireBytes := 0
	done := false
	emitted := false
	e = parseSSE(resp.Body, func(event, data string) error {
		timer.Stop()
		wireBytes += len(data)
		if wireBytes > 48<<20 {
			return errors.New("upstream_output_limit")
		}
		event = strings.ToLower(event)
		if data == "[DONE]" {
			done = true
			return errStreamDone
		}
		var m M
		if json.Unmarshal([]byte(data), &m) != nil {
			return nil
		}
		switch event {
		case "error":
			return &UpstreamError{Code: int(number(m, "code", 0)), Message: str(m, "message")}
		case "done":
			done = true
			if f := str(m, "finish_reason"); f != "" {
				result.Finish = f
			}
			return errStreamDone
		case "token_usage", "notify_usage", "usage":
			u := obj(m["usage"])
			if len(u) > 0 {
				result.Usage = u
			} else {
				pt, ct := number(m, "prompt_tokens", 0), number(m, "completion_tokens", 0)
				result.Usage = M{"prompt_tokens": pt, "completion_tokens": ct, "total_tokens": number(m, "total_tokens", pt+ct), "prompt_tokens_details": M{"cached_tokens": number(m, "cache_read_input_tokens", 0), "cache_creation_input_tokens": number(m, "cache_creation_input_tokens", 0)}, "completion_tokens_details": M{"reasoning_tokens": number(m, "reasoning_tokens", 0)}}
			}
			return nil
		case "output", "message", "text", "":
			for _, raw := range list(m["tool_calls"]) {
				t := obj(raw)
				idx := int(number(t, "index", 0))
				tc := tools[idx]
				tc.Index = idx
				if id := str(t, "id"); id != "" {
					tc.ID = id
				}
				f := obj(t["function_call"])
				if len(f) == 0 {
					f = obj(t["function"])
				}
				if n := str(f, "name"); n != "" {
					tc.Name = n
				}
				args, partial := str(f, "arguments"), str(f, "partial_arguments")
				if partial != "" {
					tc.Args += partial
				} else if args != "" {
					if strings.HasPrefix(args, tc.Args) {
						tc.Args = args
					} else {
						tc.Args += args
					}
				}
				if len(tc.Args) > 4<<20 || len(tools) > 128 {
					return errors.New("upstream_tool_limit")
				}
				tools[idx] = tc
			}
			for _, key := range []string{"content", "response", "reasoning", "reasoning_content"} {
				text := str(m, key)
				if text == "" {
					continue
				}
				total += len(text)
				if total > 32<<20 {
					return errors.New("upstream_output_limit")
				}
				kind := "text"
				if strings.HasPrefix(key, "reasoning") {
					kind = "reason"
					result.Reason += text
				} else {
					result.Text += text
				}
				emitted = true
				if e := sink(Event{Kind: kind, Text: text}); e != nil {
					return e
				}
			}
		}
		return nil
	})
	if e != nil && !errors.Is(e, errStreamDone) {
		if timedOut.Load() {
			return result, emitted, &UpstreamError{Cause: "upstream_first_event_timeout"}
		}
		return result, emitted, e
	}
	if !done {
		return result, emitted, errors.New("upstream_closed_without_done")
	}
	indexes := []int{}
	for i := range tools {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		tc := tools[i]
		if tc.Name == "" {
			continue
		}
		if tc.ID == "" {
			tc.ID = "call_" + randomID()
		}
		if tc.Args == "" {
			tc.Args = "{}"
		}
		result.Tools = append(result.Tools, tc)
		if e = sink(Event{Kind: "tool", Tool: tc}); e != nil {
			return result, true, e
		}
	}
	if len(result.Tools) > 0 {
		result.Finish = "tool_calls"
	}
	if acc.Scope != "saas" {
		a.enrichUsage(ctx, acc, session, result.Usage)
	}
	return result, true, nil
}

// Billing may lag behind done. Only accept matching session and token totals.
// A failed accounting lookup never turns a completed generation into an error.
func (a *App) enrichUsage(ctx context.Context, acc Account, session string, usage M) {
	pt, ct := number(usage, "prompt_tokens", 0), number(usage, "completion_tokens", 0)
	if pt <= 0 || number(obj(usage["prompt_tokens_details"]), "cached_tokens", 0) > 0 {
		return
	}
	window, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	for i := 0; i < 3; i++ {
		if i > 0 {
			timer := time.NewTimer(time.Duration(i) * 200 * time.Millisecond)
			select {
			case <-window.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
		call, stop := context.WithTimeout(window, 800*time.Millisecond)
		data, err := a.post(call, soloHost, "/api/v1/commercial/get_session_usage", M{"session_id": session}, acc)
		stop()
		if err != nil {
			if window.Err() != nil {
				return
			}
			continue
		}
		s := obj(data["user_usage_group_by_session"])
		extra := obj(s["extra_info"])
		cached := number(extra, "cache_read_token", -1)
		if str(s, "session_id") != session || number(extra, "input_token", -1) != pt || number(extra, "output_token", -1) != ct || cached < 0 || cached > pt {
			continue
		}
		details := obj(usage["prompt_tokens_details"])
		details["cached_tokens"] = cached
		usage["prompt_tokens_details"] = details
		return
	}
}
func usageResponses(u M) M {
	return M{"input_tokens": u["prompt_tokens"], "output_tokens": u["completion_tokens"], "total_tokens": u["total_tokens"], "input_tokens_details": u["prompt_tokens_details"], "output_tokens_details": u["completion_tokens_details"]}
}
func intString(n int) string { return strconv.Itoa(n) }
