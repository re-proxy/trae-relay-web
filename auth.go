package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const oauthHost = "https://api.trae.cn"
const enterpriseHost = "https://console.enterprise.trae.cn"
const soloHost = "https://trae-api-cn.mchost.guru"
const clientID = "ono9krqynydwx5"
const exchangePath = "/trae/api/v3/oauth/ExchangeToken"

func base64Key() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.StdEncoding.EncodeToString(b)
}
func str(m M, k string) string {
	v := m[k]
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return ""
}
func obj(v any) M {
	m, _ := v.(map[string]any)
	if m == nil {
		return M{}
	}
	return m
}
func list(v any) []any { m, _ := v.([]any); return m }
func boolean(m M, k string, d bool) bool {
	v, ok := m[k].(bool)
	if ok {
		return v
	}
	return d
}
func number(m M, k string, d float64) float64 {
	switch v := m[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		if n, e := strconv.ParseFloat(v, 64); e == nil {
			return n
		}
	}
	return d
}
func expiry(v any) int64 {
	s := ""
	switch x := v.(type) {
	case string:
		s = x
	case float64:
		s = strconv.FormatInt(int64(x), 10)
	}
	n, _ := strconv.ParseInt(s, 10, 64)
	if n > 1e12 {
		n /= 1000
	}
	if n == 0 {
		t, e := time.Parse(time.RFC3339, s)
		if e == nil {
			n = t.Unix()
		}
	}
	return n
}
func (a *App) post(ctx context.Context, host, path string, body any, acc Account) (M, error) {
	b, _ := json.Marshal(body)
	r, e := http.NewRequestWithContext(ctx, "POST", host+path, strings.NewReader(string(b)))
	if e != nil {
		return nil, e
	}
	a.headers(r, acc, false)
	resp, e := a.client.Do(r)
	if e != nil {
		return nil, errors.New("upstream_connection_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New("upstream_http_" + strconv.Itoa(resp.StatusCode))
	}
	var m M
	if e = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 8<<20)).Decode(&m); e != nil {
		return nil, errors.New("invalid_upstream_json")
	}
	if len(obj(obj(m["ResponseMetadata"])["Error"])) > 0 {
		return nil, errors.New("upstream_rejected_authorization")
	}
	return m, nil
}
func (a *App) headers(r *http.Request, acc Account, sse bool) {
	h := r.Header
	h.Set("Content-Type", "application/json")
	h.Set("Authorization", "Cloud-IDE-JWT "+acc.Token)
	h.Set("x-cloudide-token", acc.Token)
	h.Set("x-ide-token", acc.Token)
	h.Set("x-uid", acc.UserID)
	h.Set("x-app-id", "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8")
	h.Set("x-app-version", "default")
	h.Set("x-app-version-code", a.cfg.VersionCode)
	h.Set("x-ide-version", a.cfg.Version)
	h.Set("x-ide-version-code", a.cfg.VersionCode)
	h.Set("x-ide-version-type", "stable")
	h.Set("x-device-id", acc.DeviceID)
	h.Set("x-machine-id", acc.MachineID)
	h.Set("x-request-id", randomID())
	h.Set("x-device-cpu", "AMD")
	h.Set("x-device-brand", "83DG")
	h.Set("x-device-type", "windows")
	h.Set("x-os-version", "Windows 11 Pro")
	h.Set("x-system-type", "Windows")
	if sse {
		h.Set("Accept", "text/event-stream")
	} else {
		h.Set("Accept", "application/json")
	}
}
func tokenApply(acc *Account, m M) error {
	token := str(m, "Token")
	if token == "" {
		token = str(m, "AccessToken")
	}
	if token == "" {
		return errors.New("missing_token")
	}
	acc.Token = token
	if rt := str(m, "RefreshToken"); rt != "" {
		acc.RefreshToken = rt
	}
	acc.Expiry = expiry(m["TokenExpireAt"])
	return nil
}
func (a *App) verifyEnterprise(ctx context.Context, acc *Account) error {
	m, e := a.post(ctx, enterpriseHost, "/cloudide/api/v3/trae/GetUserInfo", M{"Attributes": []string{"currency_pricing"}}, *acc)
	if e != nil {
		return e
	}
	if number(m, "code", -1) != 0 {
		return errors.New("enterprise_identity_rejected")
	}
	data := obj(m["Data"])
	u := obj(data["UserInfo"])
	uid, tenant := str(u, "UserID"), str(u, "TenantID")
	if str(u, "Scope") != "saas" || uid == "" || tenant == "" || str(obj(m["HostInfo"]), "Api") != enterpriseHost {
		return errors.New("unsupported_enterprise_identity")
	}
	if acc.UserID != "" && acc.UserID != uid || acc.TenantID != "" && acc.TenantID != tenant {
		return errors.New("enterprise_identity_changed")
	}
	acc.Scope = "saas"
	acc.UserID = uid
	acc.TenantID = tenant
	acc.TenantName = str(obj(data["TenantInfoBase"]), "TenantName")
	if acc.Name == "" {
		acc.Name = str(u, "Name")
	}
	return nil
}
func (a *App) refresh(ctx context.Context, acc Account) (Account, error) {
	if acc.RefreshToken == "" {
		return acc, errors.New("missing_refresh_token")
	}
	var m M
	var e error
	if acc.Scope == "saas" {
		m, e = a.post(ctx, enterpriseHost, "/cloudide/api/v3/trae/oauth/ExchangeToken", M{"RefreshToken": acc.RefreshToken, "ClientSecret": "-", "UserID": ""}, acc)
		if e == nil && number(m, "code", -1) != 0 {
			e = errors.New("enterprise_refresh_rejected")
		}
		if e == nil {
			e = tokenApply(&acc, obj(m["Data"]))
		}
		if e == nil && acc.Expiry <= time.Now().Unix() {
			e = errors.New("enterprise_token_expired")
		}
		if e == nil {
			e = a.verifyEnterprise(ctx, &acc)
		}
	} else if len(acc.PrivateKey) > 0 {
		key, err := x509.ParseECPrivateKey(acc.PrivateKey)
		if err != nil {
			return acc, errors.New("invalid_device_key")
		}
		stamp := time.Now().Unix()
		nonce := strings.ReplaceAll(randomID(), "-", "")
		content := "POST\n" + exchangePath + "\n" + clientID + "\n" + acc.RefreshToken + "\n" + strconv.FormatInt(stamp, 10) + "\n" + nonce
		hash := sha256.Sum256([]byte(content))
		sig, err := ecdsa.SignASN1(rand.Reader, key, hash[:])
		if err != nil {
			return acc, err
		}
		m, e = a.post(ctx, oauthHost, exchangePath, M{"ClientID": clientID, "ClientSecret": "", "RefreshToken": acc.RefreshToken, "DeviceInfo": acc.DeviceInfo, "IDEVersion": a.cfg.Version, "DeviceProof": M{"Signature": base64.StdEncoding.EncodeToString(sig), "Timestamp": stamp, "Nonce": nonce}}, acc)
		if e == nil {
			e = tokenApply(&acc, obj(m["Result"]))
		}
	} else {
		m, e = a.post(ctx, oauthHost, "/cloudide/api/v3/trae/oauth/ExchangeToken", M{"ClientID": clientID, "ClientSecret": "-", "RefreshToken": acc.RefreshToken, "UserID": acc.UserID}, Account{})
		if e == nil {
			e = tokenApply(&acc, obj(m["Result"]))
		}
	}
	return acc, e
}
func (a *App) fresh(ctx context.Context, acc Account, force bool) (Account, error) {
	a.refreshMu.Lock()
	defer a.refreshMu.Unlock()
	a.store.mu.Lock()
	for _, v := range a.store.state.Accounts {
		if v.ID == acc.ID {
			acc = v
			break
		}
	}
	a.store.mu.Unlock()
	if force || (acc.RefreshToken != "" && acc.Expiry > 0 && time.Now().Unix() >= acc.Expiry-1800) {
		next, err := a.refresh(ctx, acc)
		if err != nil {
			return acc, err
		}
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
		for i, current := range a.store.state.Accounts {
			if current.ID == next.ID {
				a.store.state.Accounts[i] = next
				return next, a.store.saveLocked()
			}
		}
		return acc, errors.New("account_removed")
	}
	return acc, nil
}

type LoginAttempt struct {
	Account                   Account
	Verifier, Trace, Callback string
	Created                   time.Time
	Processing                bool
}

func (a *App) beginLogin() (M, error) {
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		return nil, e
	}
	private, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		return nil, e
	}
	pub, e := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if e != nil {
		return nil, e
	}
	machine := sha512.Sum512([]byte(randomID()))
	acc := Account{ID: randomID(), MachineID: hex.EncodeToString(machine[:]), DeviceID: randomID(), PrivateKey: private, Scope: "trae"}
	acc.DeviceInfo = M{"DeviceID": acc.DeviceID, "MachineID": acc.MachineID, "PlatformCode": "IDE_PC", "DeviceType": "PC", "DeviceName": "TraeRelayWeb", "DeviceModel": "", "DeviceBrand": "", "DeviceCPU": "", "OSInfo": "Windows", "OSVersion": "", "ClientVersion": a.cfg.Version, "DevicePublicKey": string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub}))}
	b := make([]byte, 48)
	if _, e = rand.Read(b); e != nil {
		return nil, e
	}
	verifier := base64.RawURLEncoding.EncodeToString(b)
	hash := sha256.Sum256([]byte(verifier))
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		return nil, e
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	callback := "http://127.0.0.1:" + strconv.Itoa(port) + "/authorize"
	id := secret()
	attempt := &LoginAttempt{Account: acc, Verifier: verifier, Trace: randomID(), Callback: callback, Created: time.Now()}
	params := url.Values{"login_version": {"1"}, "auth_from": {"trae"}, "login_channel": {"native_ide"}, "plugin_version": {"2.3.90286"}, "auth_type": {"local"}, "client_id": {clientID}, "redirect": {"0"}, "login_trace_id": {attempt.Trace}, "auth_callback_url": {callback}, "machine_id": {acc.MachineID}, "device_id": {acc.DeviceID}, "x_machine_id": {acc.MachineID}, "x_device_id": {acc.DeviceID}, "x_app_version": {a.cfg.Version}, "x_app_type": {"stable"}, "x_device_type": {"Windows"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(hash[:])}, "code_challenge_method": {"S256"}}
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, v := range a.logins {
		if time.Since(v.Created) > 30*time.Minute {
			delete(a.logins, k)
		}
	}
	if len(a.logins) >= 8 {
		return nil, errors.New("too_many_login_attempts")
	}
	a.logins[id] = attempt
	return M{"id": id, "url": "https://www.trae.cn/authorization?" + params.Encode(), "callback": callback, "expires_in": 1800}, nil
}
func (a *App) completeLogin(ctx context.Context, id, callback, name string) error {
	a.mu.Lock()
	p := a.logins[id]
	if p == nil || p.Processing || time.Since(p.Created) > 30*time.Minute {
		a.mu.Unlock()
		return errors.New("login_expired_or_busy")
	}
	p.Processing = true
	a.mu.Unlock()
	defer func() { a.mu.Lock(); delete(a.logins, id); a.mu.Unlock() }()
	u, e := url.Parse(callback)
	if e != nil || u.Scheme != "http" || u.Host+u.Path != strings.TrimPrefix(p.Callback, "http://") || u.Fragment != "" || u.User != nil || len(callback) > 65536 {
		return errors.New("invalid_callback_url")
	}
	q, e := url.ParseQuery(u.RawQuery)
	if e != nil {
		return errors.New("invalid_callback_query")
	}
	for _, v := range q {
		if len(v) != 1 {
			return errors.New("duplicate_callback_parameter")
		}
	}
	acc := p.Account
	acc.Name = name
	if q.Get("scope") == "saas" {
		if q.Get("loginTraceID") != p.Trace || q.Get("host") != enterpriseHost {
			return errors.New("enterprise_callback_mismatch")
		}
		acc.Scope = "saas"
		acc.PrivateKey = nil
		acc.RefreshToken = q.Get("refreshToken")
		acc, e = a.refresh(ctx, acc)
		if e != nil {
			return e
		}
	} else {
		scope := q.Get("scope")
		if scope != "" && scope != "trae" && scope != "marscode" {
			return errors.New("unsupported_scope")
		}
		var code M
		rawCode := q.Get("authCodeInfo")
		if json.Unmarshal([]byte(rawCode), &code) != nil {
			if decoded, err := url.QueryUnescape(rawCode); err == nil {
				_ = json.Unmarshal([]byte(decoded), &code)
			}
		}
		if str(code, "AuthCode") == "" {
			return errors.New("missing_authorization_code")
		}
		m, e := a.post(ctx, oauthHost, exchangePath, M{"ClientID": clientID, "AuthCode": str(code, "AuthCode"), "CodeVerifier": p.Verifier, "DeviceInfo": acc.DeviceInfo, "IDEVersion": a.cfg.Version}, Account{})
		if e != nil {
			return e
		}
		if e = tokenApply(&acc, obj(m["Result"])); e != nil {
			return e
		}
		m, e = a.post(ctx, oauthHost, "/cloudide/api/v3/trae/GetUserInfo", M{"ReqSource": "IDE", "IDEVersion": a.cfg.Version}, acc)
		if e != nil {
			return e
		}
		user := obj(m["Result"])
		acc.UserID = str(user, "UserID")
		if acc.UserID == "" {
			return errors.New("identity_verification_failed")
		}
		if acc.Name == "" {
			acc.Name = str(user, "Username")
		}
	}
	if acc.Name == "" {
		acc.Name = "Trae account"
	}
	return a.store.putAccount(acc)
}
