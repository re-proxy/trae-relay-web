package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type rewriteTransport struct {
	target *url.URL
	inner  http.RoundTripper
}

func (t rewriteTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = t.target.Scheme
	u.Host = t.target.Host
	c.URL = &u
	c.Host = u.Host
	return t.inner.RoundTrip(c)
}
func testApp(t *testing.T) *App {
	t.Helper()
	return testAppConfig(t, Config{})
}
func testAppConfig(t *testing.T, cfg Config) *App {
	t.Helper()
	if cfg.AdminPassword == "" {
		cfg.AdminPassword = "test-password-123456"
	}
	if cfg.MasterKey == "" {
		cfg.MasterKey = base64Key()
	}
	if cfg.DataDir == "" {
		cfg.DataDir = t.TempDir()
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = 2
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Version == "" {
		cfg.Version = "3.4.0"
	}
	if cfg.VersionCode == "" {
		cfg.VersionCode = "20260919"
	}
	a, e := newApp(cfg)
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func fixture(t *testing.T, a *App, h http.HandlerFunc) {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	u, _ := url.Parse(s.URL)
	a.client = &http.Client{Transport: rewriteTransport{u, http.DefaultTransport}, Timeout: 5 * time.Second}
}
func seed(t *testing.T, a *App) {
	t.Helper()
	if e := a.store.putAccount(Account{ID: "acc", Name: "Personal", Enabled: true, Scope: "trae", Token: "TOP_SECRET_TOKEN", RefreshToken: "TOP_SECRET_REFRESH", UserID: "uid", MachineID: "machine", DeviceID: "device"}); e != nil {
		t.Fatal(e)
	}
	a.models["acc"] = cachedModels{[]Model{{ID: "test-model", ModelName: "test__dev", Function: "chat_v3", Source: 1, Vision: true, Efforts: []string{"low", "high"}, DefaultEffort: "low"}}, time.Now()}
}
func request(a *App, method, path string, body any, key string, cookie *http.Cookie) *httptest.ResponseRecorder {
	var b io.Reader
	if body != nil {
		x, _ := json.Marshal(body)
		b = bytes.NewReader(x)
	}
	r := httptest.NewRequest(method, path, b)
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("Authorization", "Bearer "+key)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	return w
}
func TestEncryptedPersistence(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	raw, e := os.ReadFile(filepath.Join(a.cfg.DataDir, "state.enc"))
	if e != nil {
		t.Fatal(e)
	}
	for _, s := range []string{"TOP_SECRET_TOKEN", "TOP_SECRET_REFRESH", "Personal", a.store.apiKey()} {
		if bytes.Contains(raw, []byte(s)) {
			t.Fatal("secret persisted in plaintext")
		}
	}
	s, e := openStore(a.cfg.DataDir, a.cfg.MasterKey)
	if e != nil {
		t.Fatal(e)
	}
	acc, ok := s.active()
	if !ok || acc.Token != "TOP_SECRET_TOKEN" {
		t.Fatal("credential round trip failed")
	}
	if _, e = openStore(a.cfg.DataDir, base64Key()); e == nil {
		t.Fatal("wrong key accepted")
	}
	raw[len(raw)-1] ^= 1
	if e = os.WriteFile(filepath.Join(a.cfg.DataDir, "state.enc"), raw, 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = openStore(a.cfg.DataDir, a.cfg.MasterKey); e == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
func TestAuthSeparationAndCSRF(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	key := a.store.apiKey()
	if w := request(a, "GET", "/health", nil, "", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request(a, "GET", "/v1/status", nil, "", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request(a, "GET", "/admin/state", nil, key, nil); w.Code != 401 {
		t.Fatal("API key grants admin")
	}
	w := request(a, "POST", "/admin/login", M{"password": a.cfg.AdminPassword}, "", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	cookie := w.Result().Cookies()[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookie")
	}
	w = request(a, "GET", "/admin/state", nil, "", cookie)
	if strings.Contains(w.Body.String(), "TOP_SECRET") || strings.Contains(w.Body.String(), key) {
		t.Fatal("admin state leaked credentials")
	}
	if w = request(a, "GET", "/v1/status", nil, "", cookie); w.Code != 401 {
		t.Fatal("admin session grants API")
	}
	r := httptest.NewRequest("POST", "/admin/key/rotate", strings.NewReader("{}"))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Origin", "https://evil.example")
	r.AddCookie(cookie)
	w = httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("cross origin mutation allowed")
	}
	w = request(a, "POST", "/admin/key/rotate", M{}, "", cookie)
	if w.Code != 200 || a.store.apiKey() == key {
		t.Fatal("rotation failed")
	}
	if w = request(a, "GET", "/v1/status", nil, key, nil); w.Code != 401 {
		t.Fatal("old key still valid")
	}
	r = httptest.NewRequest("GET", "/v1/status", nil)
	r.Header.Set("X-API-Key", a.store.apiKey())
	w = httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal("X-API-Key failed")
	}
}
func TestSSEParsing(t *testing.T) {
	input := ": ping\r\nevent: output\r\ndata: {\"content\":\r\ndata: \"你好\"}\r\n\r\nevent: done\r\ndata: {}\r\n\r\n"
	n := 0
	e := parseSSE(strings.NewReader(input), func(event, data string) error {
		n++
		var m M
		if json.Unmarshal([]byte(data), &m) != nil {
			t.Fatal(data)
		}
		if n == 1 && (event != "output" || m["content"] != "你好") {
			t.Fatal(m)
		}
		return nil
	})
	if e != nil || n != 2 {
		t.Fatal(e, n)
	}
}
func TestNativeProtocolAndSSE(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	var calls atomic.Int32
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/api/agent/v3/llm_utils_chat" || r.Header.Get("Authorization") != "Cloud-IDE-JWT TOP_SECRET_TOKEN" {
			t.Error("bad upstream route/header")
		}
		var b M
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b["model"] != "test__dev" || b["function"] != "chat_v3" || b["reasoning_effort"] != "high" || b["session_id"] != b["conversation_id"] {
			t.Error(b)
		}
		p := obj(obj(list(b["tools"])[0])["function"])["parameters"]
		if _, ok := p.(string); !ok {
			t.Error("parameters must be JSON string")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: output\ndata: {\"reasoning_content\":\"想\"}\n\nevent: output\ndata: {\"content\":\"/my\",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function_call\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]}\n\nevent: output\ndata: {\"content\":\"/my\",\"tool_calls\":[{\"index\":0,\"function_call\":{\"partial_arguments\":\"\\\"ok\\\"}\"}}]}\n\nevent: token_usage\ndata: {\"prompt_tokens\":12,\"completion_tokens\":7,\"cache_read_input_tokens\":3}\n\nevent: done\ndata: {}\n\n")
	})
	body := M{"model": "test-model", "messages": []M{{"role": "user", "content": "hi"}}, "reasoning_effort": "high", "tools": []M{{"type": "function", "function": M{"name": "lookup", "parameters": M{"type": "object"}}}}}
	w := request(a, "POST", "/v1/chat/completions", body, a.store.apiKey(), nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response M
	_ = json.Unmarshal(w.Body.Bytes(), &response)
	choice := obj(list(response["choices"])[0])
	msg := obj(choice["message"])
	if msg["content"] != "/my/my" || msg["reasoning_content"] != "想" || choice["finish_reason"] != "tool_calls" {
		t.Fatal(response)
	}
	tool := obj(list(msg["tool_calls"])[0])
	if str(obj(tool["function"]), "arguments") != "{\"q\":\"ok\"}" {
		t.Fatal(tool)
	}
	if number(obj(response["usage"]), "total_tokens", 0) != 19 {
		t.Fatal(response)
	}
	body["stream"] = true
	w = request(a, "POST", "/v1/chat/completions", body, a.store.apiKey(), nil)
	if !strings.HasSuffix(w.Body.String(), "data: [DONE]\n\n") || !strings.Contains(w.Body.String(), "reasoning_content") {
		t.Fatal(w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
}
func TestPrematureEOFIsFailure(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "event: output\ndata: {\"content\":\"partial\"}\n\n")
	})
	body := M{"model": "test-model", "messages": []M{{"role": "user", "content": "hi"}}}
	w := request(a, "POST", "/v1/chat/completions", body, a.store.apiKey(), nil)
	if w.Code != 502 {
		t.Fatal(w.Code, w.Body.String())
	}
	body["stream"] = true
	w = request(a, "POST", "/v1/chat/completions", body, a.store.apiKey(), nil)
	if strings.Contains(w.Body.String(), "[DONE]") || !strings.Contains(w.Body.String(), "upstream_request_failed") {
		t.Fatal(w.Body.String())
	}
}
func TestResponsesPreviousAndLifecycle(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	var count atomic.Int32
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		var b M
		_ = json.NewDecoder(r.Body).Decode(&b)
		n := count.Add(1)
		if n == 2 && len(list(b["messages"])) != 3 {
			t.Error("previous response history missing", b)
		}
		fmt.Fprint(w, "event: output\ndata: {\"reasoning\":\"plan\",\"content\":\"hello\"}\n\nevent: done\ndata: {}\n\n")
	})
	body := M{"model": "test-model", "input": "hi"}
	w := request(a, "POST", "/v1/responses", body, a.store.apiKey(), nil)
	var result M
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result["status"] != "completed" {
		t.Fatal(w.Body.String())
	}
	body["previous_response_id"] = result["id"]
	body["stream"] = true
	w = request(a, "POST", "/v1/responses", body, a.store.apiKey(), nil)
	for _, event := range []string{"response.created", "response.in_progress", "response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.done", "response.output_item.done", "response.completed", "response.reasoning_summary_text.delta"} {
		if !strings.Contains(w.Body.String(), "event: "+event+"\n") {
			t.Fatal("missing", event, w.Body.String())
		}
	}
	body["previous_response_id"] = "missing"
	w = request(a, "POST", "/v1/responses", body, a.store.apiKey(), nil)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "previous_response_not_found") {
		t.Fatal(w.Body.String())
	}
}
func TestCancellationAndConcurrency(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	a.cfg.Concurrency = 1
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": waiting\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(cancelled)
	})
	ctx, cancel := context.WithCancel(context.Background())
	body := `{"model":"test-model","messages":[{"role":"user","content":"hi"}]}`
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+a.store.apiKey())
	done := make(chan struct{})
	go func() { a.handler().ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request not entered")
	}
	w := request(a, "POST", "/v1/chat/completions", M{"model": "test-model", "messages": []M{{"role": "user", "content": "second"}}}, a.store.apiKey(), nil)
	if w.Code != 503 {
		t.Fatal("concurrency limit bypassed")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream not cancelled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler stuck")
	}
	acc, e := a.pick(context.Background(), nil)
	if e != nil {
		t.Fatal("slot leaked")
	}
	a.release(acc.ID)
}
func TestPKCEDeviceSignatureAndCallbackBinding(t *testing.T) {
	a := testApp(t)
	v, e := a.beginLogin()
	if e != nil {
		t.Fatal(e)
	}
	id := str(v, "id")
	p := a.logins[id]
	u, _ := url.Parse(str(v, "url"))
	hash := sha256.Sum256([]byte(p.Verifier))
	if u.Query().Get("code_challenge") != base64.RawURLEncoding.EncodeToString(hash[:]) || u.Query().Get("auth_callback_url") != p.Callback {
		t.Fatal("invalid PKCE link")
	}
	block, _ := pem.Decode([]byte(str(p.Account.DeviceInfo, "DevicePublicKey")))
	pub, e := x509.ParsePKIXPublicKey(block.Bytes)
	if e != nil {
		t.Fatal(e)
	}
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		var b M
		_ = json.NewDecoder(r.Body).Decode(&b)
		if r.URL.Path != exchangePath {
			t.Error(r.URL.Path)
		}
		proof := obj(b["DeviceProof"])
		content := "POST\n" + exchangePath + "\n" + clientID + "\nrefresh\n" + str(proof, "Timestamp") + "\n" + str(proof, "Nonce")
		h := sha256.Sum256([]byte(content))
		sig, _ := base64.StdEncoding.DecodeString(str(proof, "Signature"))
		if !ecdsa.VerifyASN1(pub.(*ecdsa.PublicKey), h[:], sig) {
			t.Error("invalid device proof")
		}
		writeJSON(w, 200, M{"Result": M{"Token": "new", "RefreshToken": "rotated", "TokenExpireAt": float64(time.Now().Add(time.Hour).Unix())}})
	})
	acc := p.Account
	acc.RefreshToken = "refresh"
	next, e := a.refresh(context.Background(), acc)
	if e != nil || next.Token != "new" || next.RefreshToken != "rotated" {
		t.Fatal(next, e)
	}
	if e = a.completeLogin(context.Background(), id, "http://127.0.0.1:1/authorize?authCodeInfo=x", ""); e == nil {
		t.Fatal("mismatched callback accepted")
	}
	if _, ok := a.logins[id]; ok {
		t.Fatal("attempt was not consumed")
	}
}
func TestModelCatalogAndMultimodalTools(t *testing.T) {
	raw := `{"data":{"config_info_list":[{"config_name":"model-a","config_source":1,"model_detail_list":[{"model_name":"a__dev","support_vision":false,"reasoning_effort_config":{"support_thinking":true,"options":["low","high"],"default_level":"low"}},{"model_name":"a__max","max_mode":true}]},{"config_name":"search_agent_x","model_detail_list":[{}]},{"config_name":"hidden","config_switch":false,"model_detail_list":[{}]}]}}`
	var m M
	_ = json.Unmarshal([]byte(raw), &m)
	models := mergeModels(parseCatalog(m, "chat_v3"))
	if len(models) != 1 || models[0].MaxModel != "a__max" || !models[0].Max || len(models[0].Efforts) != 2 {
		t.Fatal(models)
	}
	input := []M{{"role": "developer", "content": "<opencode_tool_call>"}, {"role": "assistant", "tool_calls": []any{M{"id": "one", "function": M{"name": "tool", "arguments": "{}"}}}}, {"role": "tool", "tool_call_id": "one", "content": []any{M{"type": "image_url", "image_url": M{"url": "data:image/png;base64,AA=="}}}}, {"role": "tool", "tool_call_id": "two", "content": "result"}}
	msgs, e := soloMessages(input)
	if e != nil {
		t.Fatal(e)
	}
	if len(msgs) != 5 || msgs[0]["role"] != "system" || msgs[2]["role"] != "tool" || msgs[3]["role"] != "tool" || msgs[4]["role"] != "user" {
		t.Fatal(msgs)
	}
	b, _ := json.Marshal(msgs)
	if strings.Contains(string(b), "opencode_tool_call") || !strings.Contains(string(b), "image_url") {
		t.Fatal(string(b))
	}
}
func TestStopAcrossUnicodeChunks(t *testing.T) {
	f := stopFilter{stops: []string{"结束"}}
	out := f.feed("你好结", false) + f.feed("束之后", false) + f.feed("", true)
	if out != "你好" || !f.stopped {
		t.Fatal(out)
	}
	f = stopFilter{stops: []string{"XYZ"}}
	out = f.feed("abX", false) + f.feed("a", false) + f.feed("", true)
	if out != "abXa" {
		t.Fatal(out)
	}
}
func TestResponseCustomNamespaceTools(t *testing.T) {
	tools, spec, e := responseTools([]any{M{"type": "namespace", "name": "editor", "tools": []any{M{"type": "custom", "name": "apply", "description": "edit"}}}})
	if e != nil || len(tools) != 1 {
		t.Fatal(e)
	}
	name := str(obj(tools[0]["function"]), "name")
	if !spec[name].Custom || spec[name].Namespace != "editor" {
		t.Fatal(spec)
	}
	w := httptest.NewRecorder()
	d := downstream{w: w, ctx: context.Background(), responses: true, output: []M{}, indices: map[string]int{}, spec: spec}
	if e = d.emit(Event{Kind: "tool", Tool: ToolCall{ID: "call", Name: name, Args: `{"input":"patch"}`}}); e != nil {
		t.Fatal(e)
	}
	if d.output[0]["type"] != "custom_tool_call" || d.output[0]["namespace"] != "editor" || d.output[0]["input"] != "patch" {
		t.Fatal(d.output)
	}
}

func TestPersonalCallbackExchangeAndLegacyRefresh(t *testing.T) {
	a := testApp(t)
	start, e := a.beginLogin()
	if e != nil {
		t.Fatal(e)
	}
	p := a.logins[str(start, "id")]
	var exchanged, legacy bool
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		var b M
		_ = json.NewDecoder(r.Body).Decode(&b)
		switch r.URL.Path {
		case exchangePath:
			exchanged = true
			if b["CodeVerifier"] != p.Verifier || b["AuthCode"] != "one-use-code" {
				t.Error("wrong authorization exchange")
			}
			writeJSON(w, 200, M{"Result": M{"Token": "access", "RefreshToken": "refresh", "TokenExpireAt": float64(time.Now().Add(time.Hour).Unix())}})
		case "/cloudide/api/v3/trae/GetUserInfo":
			if r.Header.Get("x-cloudide-token") != "access" {
				t.Error("identity token missing")
			}
			writeJSON(w, 200, M{"Result": M{"UserID": "verified-user", "Username": "Alice"}})
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			legacy = true
			if b["ClientSecret"] != "-" || b["UserID"] != "verified-user" {
				t.Error(b)
			}
			writeJSON(w, 200, M{"Result": M{"Token": "legacy-new", "RefreshToken": "legacy-rotated", "TokenExpireAt": "2030-01-01T00:00:00Z"}})
		default:
			t.Error(r.URL.Path)
		}
	})
	code, _ := json.Marshal(M{"AuthCode": "one-use-code"})
	callback := p.Callback + "?" + url.Values{"scope": {"trae"}, "authCodeInfo": {url.QueryEscape(string(code))}}.Encode()
	if e = a.completeLogin(context.Background(), str(start, "id"), callback, ""); e != nil {
		t.Fatal(e)
	}
	a.store.mu.Lock()
	acc := a.store.state.Accounts[0]
	a.store.mu.Unlock()
	if !exchanged || acc.UserID != "verified-user" || acc.Enabled || acc.Name != "Alice" {
		t.Fatal("unverified identity persisted")
	}
	acc.PrivateKey = nil
	next, e := a.refresh(context.Background(), acc)
	if e != nil || !legacy || next.Token != "legacy-new" {
		t.Fatal(e)
	}
	if e = a.completeLogin(context.Background(), str(start, "id"), callback, ""); e == nil {
		t.Fatal("authorization replay accepted")
	}
}

func TestEnterpriseCallbackTrustedIdentity(t *testing.T) {
	a := testApp(t)
	v, e := a.beginLogin()
	if e != nil {
		t.Fatal(e)
	}
	p := a.logins[str(v, "id")]
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		var b M
		_ = json.NewDecoder(r.Body).Decode(&b)
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/oauth/ExchangeToken":
			if b["RefreshToken"] != "enterprise-refresh" || b["UserID"] != "" {
				t.Error(b)
			}
			writeJSON(w, 200, M{"code": 0, "Data": M{"Token": "enterprise-token", "RefreshToken": "next-refresh", "TokenExpireAt": float64(time.Now().Add(time.Hour).Unix())}})
		case "/cloudide/api/v3/trae/GetUserInfo":
			if r.Header.Get("x-cloudide-token") != "enterprise-token" {
				t.Error("missing enterprise auth")
			}
			writeJSON(w, 200, M{"code": 0, "HostInfo": M{"Api": enterpriseHost}, "Data": M{"UserInfo": M{"Scope": "saas", "UserID": "uid", "TenantID": "tid", "Name": "Employee", "Password": "SHOULD_NEVER_SAVE"}, "TenantInfoBase": M{"TenantName": "Team"}}})
		default:
			t.Error(r.URL.Path)
		}
	})
	q := url.Values{"scope": {"saas"}, "host": {enterpriseHost}, "loginTraceID": {p.Trace}, "refreshToken": {"enterprise-refresh"}, "userInfo": {"untrusted"}}
	if e = a.completeLogin(context.Background(), str(v, "id"), p.Callback+"?"+q.Encode(), ""); e != nil {
		t.Fatal(e)
	}
	a.store.mu.Lock()
	acc := a.store.state.Accounts[0]
	a.store.mu.Unlock()
	if acc.Scope != "saas" || acc.TenantID != "tid" || acc.TenantName != "Team" || len(acc.PrivateKey) != 0 {
		t.Fatal("enterprise verification failed")
	}
	b, _ := json.Marshal(acc)
	if strings.Contains(string(b), "SHOULD_NEVER_SAVE") {
		t.Fatal("server password persisted")
	}
	v, e = a.beginLogin()
	if e != nil {
		t.Fatal(e)
	}
	p = a.logins[str(v, "id")]
	q.Set("host", "https://evil.example")
	q.Set("loginTraceID", p.Trace)
	if e = a.completeLogin(context.Background(), str(v, "id"), p.Callback+"?"+q.Encode(), ""); e == nil {
		t.Fatal("untrusted enterprise host accepted")
	}
}

func TestCacheUsageAttribution(t *testing.T) {
	a := testApp(t)
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		var b M
		_ = json.NewDecoder(r.Body).Decode(&b)
		if r.URL.Path != "/api/v1/commercial/get_session_usage" {
			t.Error(r.URL.Path)
		}
		writeJSON(w, 200, M{"user_usage_group_by_session": M{"session_id": b["session_id"], "extra_info": M{"input_token": 10, "output_token": 4, "cache_read_token": 6}}})
	})
	usage := M{"prompt_tokens": float64(10), "completion_tokens": float64(4), "prompt_tokens_details": M{"cached_tokens": float64(0)}}
	a.enrichUsage(context.Background(), Account{}, "session", usage)
	if number(obj(usage["prompt_tokens_details"]), "cached_tokens", 0) != 6 {
		t.Fatal(usage)
	}
}

func chatBody(stream bool) M {
	return M{"model": "test-model", "messages": []M{{"role": "user", "content": "hi"}}, "stream": stream}
}
func TestUpstreamErrorsKeepStatusAndMessage(t *testing.T) {
	cases := []struct {
		name, sse     string
		status        int
		kind, message string
	}{
		{"rate limited", `event: error
data: {"code":4011,"message":"We're sorry, your requests have exceeded the rate limit."}

`, 429, "rate_limit_error", "We're sorry, your requests have exceeded the rate limit."},
		{"insufficient quota", "event: error\ndata: {\"code\":1005,\"message\":\"no quota left\"}\n\n", 403, "insufficient_quota", "no quota left"},
		{"invalid request", "event: error\ndata: {\"code\":4001,\"message\":\"bad payload\"}\n\n", 400, "invalid_request_error", "bad payload"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := testApp(t)
			seed(t, a)
			fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, c.sse)
			})
			w := request(a, "POST", "/v1/chat/completions", chatBody(false), a.store.apiKey(), nil)
			if w.Code != c.status {
				t.Fatalf("status %d body %s", w.Code, w.Body.String())
			}
			var out M
			_ = json.Unmarshal(w.Body.Bytes(), &out)
			e := obj(out["error"])
			if e["type"] != c.kind || e["message"] != c.message || e["code"] == nil {
				t.Fatal(w.Body.String())
			}
			s := request(a, "POST", "/v1/chat/completions", chatBody(true), a.store.apiKey(), nil)
			if s.Code != c.status || !strings.Contains(s.Body.String(), c.kind) || strings.Contains(s.Body.String(), "[DONE]") {
				t.Fatalf("stream body %s", s.Body.String())
			}
		})
	}
}
func TestExpiredTokenMapsTo401(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "ExchangeToken") {
			w.WriteHeader(500)
			return
		}
		w.WriteHeader(401)
		fmt.Fprint(w, `{"message":"token expired"}`)
	})
	w := request(a, "POST", "/v1/chat/completions", chatBody(false), a.store.apiKey(), nil)
	if w.Code != 401 || !strings.Contains(w.Body.String(), "authentication_error") || !strings.Contains(w.Body.String(), "token expired") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}
func TestStreamAuthErrorRefreshesOnce(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	var chat int
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "ExchangeToken") {
			fmt.Fprint(w, `{"Result":{"Token":"FRESH_TOKEN","TokenExpireAt":"2999-01-01T00:00:00Z"}}`)
			return
		}
		chat++
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT FRESH_TOKEN" {
			fmt.Fprint(w, "event: error\ndata: {\"code\":1001,\"message\":\"auth expired\"}\n\n")
			return
		}
		fmt.Fprint(w, "event: output\ndata: {\"content\":\"fresh reply\"}\n\nevent: done\ndata: {\"finish_reason\":\"stop\"}\n\n")
	})
	w := request(a, "POST", "/v1/chat/completions", chatBody(false), a.store.apiKey(), nil)
	if w.Code != 200 || chat != 2 || !strings.Contains(w.Body.String(), "fresh reply") {
		t.Fatalf("%d calls %d body %s", w.Code, chat, w.Body.String())
	}
	if acc, ok := a.store.active(); !ok || acc.Token != "FRESH_TOKEN" {
		t.Fatal("refreshed token not persisted")
	}
}
func TestFirstEventTimeoutIsReported(t *testing.T) {
	a := testAppConfig(t, Config{FirstEvent: 100 * time.Millisecond})
	seed(t, a)
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(600 * time.Millisecond)
		fmt.Fprint(w, "event: done\ndata: {}\n\n")
	})
	w := request(a, "POST", "/v1/chat/completions", chatBody(false), a.store.apiKey(), nil)
	if w.Code != 502 || !strings.Contains(w.Body.String(), "upstream_first_event_timeout") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func addAccount(t *testing.T, a *App, id string, balance M, models []Model) {
	t.Helper()
	if e := a.store.putAccount(Account{ID: id, Name: id, Enabled: true, Scope: "trae", Token: "TOKEN_" + id, RefreshToken: "REFRESH_" + id, UserID: "uid", MachineID: "machine", DeviceID: "device", Balance: balance}); e != nil {
		t.Fatal(e)
	}
	a.models[id] = cachedModels{models, time.Now()}
}
func poolModels(ids ...string) []Model {
	out := []Model{}
	for _, id := range ids {
		out = append(out, Model{ID: id, ModelName: id + "__dev", Function: "chat_v3", Source: 1})
	}
	return out
}
func TestPoolRoundRobinAndQueue(t *testing.T) {
	a := testAppConfig(t, Config{Concurrency: 1, QueueTimeout: 50 * time.Millisecond})
	addAccount(t, a, "acc1", nil, poolModels("m1"))
	addAccount(t, a, "acc2", nil, poolModels("m1"))
	first, e := a.pick(context.Background(), nil)
	if e != nil || first.ID != "acc1" {
		t.Fatalf("first %v %v", first.ID, e)
	}
	second, e := a.pick(context.Background(), nil)
	if e != nil || second.ID != "acc2" {
		t.Fatalf("second %v %v", second.ID, e)
	}
	start := time.Now()
	if _, e = a.pick(context.Background(), nil); e != errPoolBusy {
		t.Fatalf("expected busy, got %v", e)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("no queueing wait: %v", elapsed)
	}
	a.release("acc1")
	if third, e := a.pick(context.Background(), nil); e != nil || third.ID != "acc1" {
		t.Fatalf("third %v %v", third.ID, e)
	}
	a.release("acc2")
	a.release("acc1")
	if _, e := a.pick(context.Background(), map[string]bool{"acc1": true}); e != nil {
		t.Fatalf("exclude failed: %v", e)
	}
}
func TestPoolCreditOrderAndMinInterval(t *testing.T) {
	a := testAppConfig(t, Config{Concurrency: 1, SelectByCredits: true})
	addAccount(t, a, "low", M{"remaining": 10.0}, poolModels("m1"))
	addAccount(t, a, "high", M{"remaining": 900.0}, poolModels("m1"))
	addAccount(t, a, "unknown", nil, poolModels("m1"))
	for _, want := range []string{"high", "low", "unknown"} {
		acc, e := a.pick(context.Background(), nil)
		if e != nil || acc.ID != want {
			t.Fatalf("expected %s got %v %v", want, acc.ID, e)
		}
	}
	a = testAppConfig(t, Config{Concurrency: 2, MinInterval: 200 * time.Millisecond})
	addAccount(t, a, "acc1", nil, poolModels("m1"))
	addAccount(t, a, "acc2", nil, poolModels("m1"))
	first, _ := a.pick(context.Background(), nil)
	a.release(first.ID)
	second, e := a.pick(context.Background(), nil)
	if e != nil || second.ID == first.ID {
		t.Fatalf("min interval ignored: %v %v", second.ID, e)
	}
}
func TestModelsMergeAcrossAccounts(t *testing.T) {
	a := testApp(t)
	addAccount(t, a, "acc1", nil, poolModels("only-a"))
	addAccount(t, a, "acc2", nil, poolModels("only-b"))
	w := request(a, "GET", "/v1/models", nil, a.store.apiKey(), nil)
	var out struct{ Data []M }
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	ids := map[string]bool{}
	for _, m := range out.Data {
		ids[str(m, "id")] = true
	}
	if w.Code != 200 || !ids["only-a"] || !ids["only-b"] {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestUpstreamRequestIdentifiesDesktopClient(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	seen := ""
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: output\ndata: {\"content\":\"ok\"}\n\nevent: done\ndata: {}\n\n")
	})
	w := request(a, "POST", "/v1/chat/completions", M{"model": "test-model", "messages": []M{{"role": "user", "content": "hi"}}}, a.store.apiKey(), nil)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// A library default such as "Go-http-client/1.1" makes upstream answer a
	// misleading 4011 rate-limit error, so the request must identify as the
	// desktop client build that the credentials belong to.
	if seen != "Trae/"+a.cfg.Version {
		t.Fatalf("unexpected user agent %q", seen)
	}
}
