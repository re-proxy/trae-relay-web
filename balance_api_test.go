package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestBalanceAPIKeyRevocationAndCancelledWait(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	a.store.mu.Lock()
	a.store.state.Keys = append(a.store.state.Keys, APIKey{ID: "router", Secret: "router-key", Enabled: true})
	a.store.mu.Unlock()
	acc, _ := a.store.find("acc")
	acc.Balance = M{"ok": true, "remaining": float64(3), "checked_at": time.Now()}
	_ = a.store.putAccount(acc)
	r := httptest.NewRequest("GET", "/v1/balance", nil)
	r.Header.Set("X-API-Key", "router-key")
	w := httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	a.store.mu.Lock()
	a.store.state.Keys[0].Enabled = false
	a.store.mu.Unlock()
	w = httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("disabled key accepted")
	}
	a.balanceGate <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r = httptest.NewRequest("GET", "/v1/balance", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+a.store.apiKey())
	w = httptest.NewRecorder()
	a.handler().ServeHTTP(w, r)
	<-a.balanceGate
	if w.Code != 504 {
		t.Fatal("cancelled waiter blocked", w.Code)
	}
}

func TestBalanceAPIAuthAggregateAndCache(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	other, _ := a.store.find("acc")
	other.ID = "second"
	other.Token = "second-token"
	if e := a.store.putAccount(other); e != nil {
		t.Fatal(e)
	}
	disabled := other
	disabled.ID = "disabled"
	disabled.Enabled = false
	_ = a.store.putAccount(disabled)
	var calls atomic.Int32
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/trae/api/v2/pay/ide_user_ent_usage" {
			t.Error(r.URL.Path)
		}
		n := 10.5
		if r.Header.Get("x-cloudide-token") == "second-token" {
			n = 0
		}
		writeJSON(w, 200, M{"usage_summary": M{"remaining_amount": n}})
	})
	if w := request(a, "GET", "/v1/balance", nil, "", nil); w.Code != 401 {
		t.Fatal("missing authentication accepted")
	}
	cookie := adminCookie(t, a)
	if w := request(a, "GET", "/v1/balance", nil, "", cookie); w.Code != 401 {
		t.Fatal("admin cookie grants API access")
	}
	for i := 0; i < 2; i++ {
		w := request(a, "GET", "/v1/balance", nil, a.store.apiKey(), nil)
		var m M
		_ = json.Unmarshal(w.Body.Bytes(), &m)
		if w.Code != 200 || m["complete"] != true || number(m, "balance", -1) != 10.5 || number(m, "known_accounts", 0) != 2 || m["unit"] != "credits" {
			t.Fatal(w.Body.String())
		}
		for _, v := range list(m["accounts"]) {
			if obj(v)["cached"] != (i == 1) {
				t.Fatal("unexpected cache state", m)
			}
		}
		if strings.Contains(w.Body.String(), "TOP_SECRET") || strings.Contains(w.Body.String(), "second-token") || strings.Contains(w.Body.String(), "Personal") {
			t.Fatal("credentials/identity leaked")
		}
	}
	if calls.Load() != 2 {
		t.Fatal("polling queried upstream again", calls.Load())
	}
	for _, id := range []string{"acc", "second"} {
		acc, _ := a.store.find(id)
		acc.Balance["checked_at"] = time.Now().Add(-2 * time.Minute)
		_ = a.store.putAccount(acc)
	}
	w := request(a, "GET", "/v1/balance", nil, a.store.apiKey(), nil)
	if w.Code != 200 || calls.Load() != 4 {
		t.Fatal("expired cache not refreshed", w.Body.String(), calls.Load())
	}
}
func TestBalanceAPIPartialFailuresAndUnknown(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	other, _ := a.store.find("acc")
	other.ID = "failure"
	other.Token = "bad"
	_ = a.store.putAccount(other)
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-cloudide-token") == "bad" {
			w.WriteHeader(403)
			return
		}
		writeJSON(w, 200, M{"usage_summary": M{"remaining_amount": 0}})
	})
	w := request(a, "GET", "/v1/balance", nil, a.store.apiKey(), nil)
	var m M
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if w.Code != 200 || m["complete"] != false || m["ok"] != true || number(m, "balance", -1) != 0 || number(m, "unknown_accounts", 0) != 1 {
		t.Fatal(w.Body.String())
	}
	// An expired previously successful cache must not survive a failed refresh as
	// a successful/zero balance, and must not mutate a shared old map in place.
	first, _ := a.store.find("acc")
	first.Token = "bad"
	old := M{"ok": true, "remaining": float64(999), "checked_at": time.Now().Add(-2 * time.Minute)}
	first.Balance = old
	_ = a.store.putAccount(first)
	w = request(a, "GET", "/v1/balance", nil, a.store.apiKey(), nil)
	m = M{}
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	if w.Code != 502 || m["balance"] != nil || m["ok"] != false || old["ok"] != true || old["remaining"] != float64(999) {
		t.Fatal(w.Body.String())
	}
}
func TestBalanceAPIEnterpriseQuotaIsSeparate(t *testing.T) {
	a := testApp(t)
	_ = a.store.putAccount(Account{ID: "ent", Enabled: true, Scope: "saas", Balance: M{"ok": true, "kind": "enterprise", "plan_quota": float64(1000), "checked_at": time.Now()}})
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) { t.Error("valid cache called upstream") })
	w := request(a, "GET", "/v1/balance", nil, a.store.apiKey(), nil)
	var m M
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	account := obj(list(m["accounts"])[0])
	if w.Code != 200 || m["balance"] != nil || m["complete"] != false || account["remaining"] != nil || number(account, "plan_quota", 0) != 1000 || account["unit"] != "CNY" {
		t.Fatal(w.Body.String())
	}
	empty := testApp(t)
	if w = request(empty, "GET", "/v1/balance", nil, empty.store.apiKey(), nil); w.Code != 503 {
		t.Fatal(w.Code)
	}
}
func TestBalanceAPIConcurrentPollsCoalesce(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	var calls atomic.Int32
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		writeJSON(w, 200, M{"usage_summary": M{"remaining_amount": 7}})
	})
	key := a.store.apiKey()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(a, "GET", "/v1/balance", nil, key, nil)
			if w.Code != 200 {
				t.Error(w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatal("concurrent polls not coalesced", calls.Load())
	}
}

func TestAdminBalanceRefreshAndCancellation(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	cookie := adminCookie(t, a)
	if w := request(a, "POST", "/admin/balance/refresh", M{}, a.store.apiKey(), nil); w.Code != 401 {
		t.Fatal("API key grants admin refresh")
	}
	acc, _ := a.store.find("acc")
	old := M{"ok": true, "remaining": float64(17), "checked_at": time.Now()}
	acc.Balance = old
	_ = a.store.putAccount(acc)
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) { t.Error("fresh cache queried upstream") })
	if w := request(a, "POST", "/admin/balance/refresh", M{}, "", cookie); w.Code != 200 || !strings.Contains(w.Body.String(), `"cached":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// Hiding a browser cancels its poll; this must not poison the stored balance.
	acc.Balance = M{"ok": true, "remaining": float64(17), "checked_at": time.Now().Add(-2 * time.Minute)}
	_ = a.store.putAccount(acc)
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest("GET", "/v1/balance", nil).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+a.store.apiKey())
	done := make(chan struct{})
	go func() { a.handler().ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("query did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelled query did not stop")
	}
	saved, _ := a.store.find("acc")
	if saved.Balance["ok"] != true || number(saved.Balance, "remaining", 0) != 17 {
		t.Fatal("cancelled poll replaced prior balance", saved.Balance)
	}
}
