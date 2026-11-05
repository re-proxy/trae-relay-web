package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func adminCookie(t *testing.T, a *App) *http.Cookie {
	t.Helper()
	w := request(a, "POST", "/admin/login", M{"password": a.cfg.AdminPassword}, "", nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	return w.Result().Cookies()[0]
}
func TestMultipleKeysMigrationAndRevocation(t *testing.T) {
	a := testApp(t)
	legacy := a.store.apiKey()
	cookie := adminCookie(t, a)
	w := request(a, "POST", "/admin/key", M{"action": "create", "name": "workstation"}, "", cookie)
	var issued M
	_ = json.Unmarshal(w.Body.Bytes(), &issued)
	key := str(issued, "api_key")
	if w.Code != 200 || key == "" || !a.store.validKey(key) || !a.store.validKey(legacy) {
		t.Fatal("create broke legacy compatibility")
	}
	w = request(a, "GET", "/admin/state", nil, "", cookie)
	if strings.Contains(w.Body.String(), key) || strings.Contains(w.Body.String(), legacy) {
		t.Fatal("state leaked a key")
	}
	var state M
	_ = json.Unmarshal(w.Body.Bytes(), &state)
	keys := list(state["keys"])
	if len(keys) != 2 {
		t.Fatal(state)
	}
	id := str(obj(keys[1]), "id")
	for _, action := range []string{"disable", "enable", "rename"} {
		w = request(a, "POST", "/admin/key", M{"action": action, "id": id, "name": "renamed"}, "", cookie)
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
		if a.store.validKey(key) != (action != "disable") {
			t.Fatal(action)
		}
	}
	reopened, e := openStore(a.cfg.DataDir, a.cfg.MasterKey)
	if e != nil {
		t.Fatal(e)
	}
	if !reopened.validKey(key) || !reopened.validKey(legacy) {
		t.Fatal("migration not persisted")
	}
	w = request(a, "POST", "/admin/key", M{"action": "delete", "id": "legacy"}, "", cookie)
	if w.Code != 200 || a.store.validKey(legacy) || !a.store.validKey(key) {
		t.Fatal("independent revocation failed")
	}
	w = request(a, "GET", "/v1/status", nil, key, nil)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if w = request(a, "GET", "/admin/state", nil, key, nil); w.Code != 401 {
		t.Fatal("API key grants management")
	}
	w = request(a, "POST", "/admin/key", M{"action": "delete", "id": id}, "", cookie)
	if w.Code != 200 || a.store.validKey(key) {
		t.Fatal("revoked key still accepted")
	}
}
func TestPersonalBalanceAndMetadataCheck(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	cookie := adminCookie(t, a)
	calls := 0
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body M
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/GetUserInfo":
			writeJSON(w, 200, M{"Result": M{"UserID": "uid"}})
		case "/api/ide/v1/get_detail_param":
			writeJSON(w, 200, M{"config_info_list": []M{{"config_name": "m", "model_detail_list": []M{{"model_name": "m__dev"}}}}})
		case "/trae/api/v2/pay/ide_user_ent_usage":
			if body["require_usage"] != true {
				t.Error(body)
			}
			writeJSON(w, 200, M{"usage_summary": M{"total_amount": "100.5", "consumed_amount": "12.25"}, "ide_credits": 999})
		default:
			t.Error("unexpected or billable endpoint", r.URL.Path)
			w.WriteHeader(400)
		}
	})
	w := request(a, "POST", "/admin/account/check", M{"id": "acc"}, "", cookie)
	var result M
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if w.Code != 200 || result["ok"] != true || result["mode"] != "metadata_only" || number(result, "model_count", 0) != 1 {
		t.Fatal(w.Body.String())
	}
	w = request(a, "POST", "/admin/account/balance", M{"id": "acc"}, "", cookie)
	_ = json.Unmarshal(w.Body.Bytes(), &result)
	if result["ok"] != true || number(result, "remaining", 0) != 88.25 {
		t.Fatal(w.Body.String())
	}
	if calls != 4 {
		t.Fatal(calls)
	}
	reopened, e := openStore(a.cfg.DataDir, a.cfg.MasterKey)
	if e != nil {
		t.Fatal(e)
	}
	acc, _ := reopened.active()
	if acc.Check["ok"] != true || number(acc.Balance, "remaining", 0) != 88.25 {
		t.Fatal("diagnostics not persisted")
	}
}
func TestBalanceParsingAndFailures(t *testing.T) {
	for _, tc := range []struct {
		json string
		want float64
	}{
		{`{"data":{"credits_remain_info":{"available_amount":"2.5"}}}`, 2.5},
		{`{"usage_summary":{"remaining_amount":0},"ide_credits":100}`, 0},
		{`{"user_entitlement_pack_list":[{"expire_time":1,"entitlement_base_info":{"quota":{"credits_limit":999}}},{"entitlement_base_info":{"quota":{"credits_limit":40}},"usage":{"credits_amount":12}}]}`, 28},
	} {
		var m M
		_ = json.Unmarshal([]byte(tc.json), &m)
		got, ok := findBalance(m)
		if !ok || got != tc.want {
			t.Fatal(tc.json, got, ok)
		}
	}
	var invalid M
	_ = json.Unmarshal([]byte(`{"usage_summary":{"remaining_amount":"NaN"}}`), &invalid)
	if _, ok := findBalance(invalid); ok {
		t.Fatal("accepted nonfinite balance")
	}
	a := testApp(t)
	seed(t, a)
	cookie := adminCookie(t, a)
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	w := request(a, "POST", "/admin/account/check", M{"id": "acc"}, "", cookie)
	if !strings.Contains(w.Body.String(), `"ok":false`) || !strings.Contains(w.Body.String(), "upstream_http_401") {
		t.Fatal(w.Body.String())
	}
	w = request(a, "POST", "/admin/account/balance", M{"id": "missing"}, "", cookie)
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func TestEnterpriseQuotaIsNotRemainingBalance(t *testing.T) {
	a := testApp(t)
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloudide/api/v3/trae/GetUserInfo":
			writeJSON(w, 200, M{"code": 0, "HostInfo": M{"Api": enterpriseHost}, "Data": M{"UserInfo": M{"Scope": "saas", "UserID": "u", "TenantID": "t"}}, "Attributes": M{"currency_pricing": M{"Enable": true}}})
		case "/trae/gtm/tob/api/v1/config/plan_attribute":
			writeJSON(w, 200, M{"code": 0, "Data": M{"Attributes": []M{{"Attribute": "currency_pricing", "Enable": true, "Value": 1000}}}})
		default:
			t.Error(r.URL.Path)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, e := a.queryBalance(ctx, Account{Scope: "saas", UserID: "u", TenantID: "t"})
	if e != nil || result["remaining"] != nil || number(result, "plan_quota", 0) != 1000 {
		t.Fatal(result, e)
	}
}

func TestAdminPasswordOnlyRequiresNonEmpty(t *testing.T) {
	cfg := Config{AdminPassword: "a", MasterKey: base64Key(), DataDir: t.TempDir(), Concurrency: 2, Timeout: 5 * time.Second, Version: "3.4.0", VersionCode: "20260919"}
	a, e := newApp(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if w := request(a, "POST", "/admin/login", M{"password": "a"}, "", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(a, "POST", "/admin/login", M{"password": "b"}, "", nil); w.Code != 401 {
		t.Fatal(w.Code)
	}
	cfg.AdminPassword = ""
	if _, e := newApp(cfg); e == nil {
		t.Fatal("empty admin password must be rejected")
	}
}
