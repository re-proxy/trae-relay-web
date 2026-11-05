package main

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (a *App) admin(w http.ResponseWriter, r *http.Request) {
	// JSON-only mutations plus exact Origin validation protect cookie sessions from CSRF.
	if r.Method != "GET" {
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			fail(w, 415, "json_required")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, e := url.Parse(origin)
			if e != nil || u.Host != r.Host {
				fail(w, 403, "origin_rejected")
				return
			}
		}
	}
	if r.URL.Path == "/admin/login" && r.Method == "POST" {
		select {
		case a.loginGate <- struct{}{}:
			defer func() { time.AfterFunc(time.Second, func() { <-a.loginGate }) }()
		default:
			fail(w, 429, "login_rate_limited")
			return
		}
		var body struct {
			Password string `json:"password"`
		}
		if decode(w, r, &body) != nil {
			fail(w, 400, "invalid_json")
			return
		}
		if !equal(body.Password, a.cfg.AdminPassword) {
			fail(w, 401, "invalid_admin_password")
			return
		}
		token := secret()
		a.mu.Lock()
		for k, t := range a.adminSessions {
			if time.Now().After(t) {
				delete(a.adminSessions, k)
			}
		}
		if len(a.adminSessions) > 64 {
			a.mu.Unlock()
			fail(w, 429, "too_many_sessions")
			return
		}
		a.adminSessions[token] = time.Now().Add(12 * time.Hour)
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "relay_admin", Value: token, Path: "/admin", HttpOnly: true, Secure: r.TLS != nil || a.cfg.SecureCookie, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
		writeJSON(w, 200, M{"ok": true})
		return
	}
	cookie, e := r.Cookie("relay_admin")
	if e != nil {
		fail(w, 401, "admin_login_required")
		return
	}
	a.mu.Lock()
	expires, ok := a.adminSessions[cookie.Value]
	a.mu.Unlock()
	if !ok || time.Now().After(expires) {
		fail(w, 401, "admin_login_required")
		return
	}
	switch {
	case r.URL.Path == "/admin/logout" && r.Method == "POST":
		a.mu.Lock()
		delete(a.adminSessions, cookie.Value)
		a.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "relay_admin", Path: "/admin", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeJSON(w, 200, M{"ok": true})
	case r.URL.Path == "/admin/state" && r.Method == "GET":
		a.store.mu.Lock()
		accounts := []M{}
		for _, acc := range a.store.state.Accounts {
			accounts = append(accounts, M{"id": acc.ID, "name": acc.Name, "enabled": acc.Enabled, "scope": acc.Scope, "tenant_name": acc.TenantName, "expiry": acc.Expiry, "check": acc.Check, "balance": acc.Balance, "checkin_day": acc.CheckinDay, "checkin_result": acc.CheckinResult, "checkin_off": acc.CheckinOff})
		}
		settings := M{}
		for k, v := range a.store.state.Models {
			settings[k] = v
		}
		usage := append([]Usage{}, a.store.state.Usage...)
		keys := []M{}
		for _, k := range a.store.keysLocked() {
			keys = append(keys, M{"id": k.ID, "name": k.Name, "enabled": k.Enabled, "suffix": suffix(k.Secret), "created": k.Created})
		}
		a.store.mu.Unlock()
		a.mu.Lock()
		inflight := 0
		models := []Model{}
		for _, n := range a.busy {
			inflight += n
		}
		if acc, ok := a.store.active(); ok {
			models = append(models, a.models[acc.ID].Models...)
		}
		a.mu.Unlock()
		writeJSON(w, 200, M{"accounts": accounts, "models": models, "settings": settings, "usage": usage, "keys": keys, "in_flight": inflight, "api_key_suffix": suffix(a.store.apiKey()), "version": "0.1.0", "checkin_enabled": a.cfg.CheckinEnabled, "checkin_at": fmt.Sprintf("%02d:%02d", a.cfg.CheckinHour, a.cfg.CheckinMinute)})
	case r.URL.Path == "/admin/account/check" && r.Method == "POST":
		a.accountInspect(w, r, false)
	case r.URL.Path == "/admin/account/balance" && r.Method == "POST":
		a.accountInspect(w, r, true)
	case r.URL.Path == "/admin/account/checkin" && r.Method == "POST":
		a.manageCheckin(w, r)
	case r.URL.Path == "/admin/key" && r.Method == "POST":
		a.manageKey(w, r)
	case r.URL.Path == "/admin/login/start" && r.Method == "POST":
		v, e := a.beginLogin()
		if e != nil {
			fail(w, 400, e.Error())
			return
		}
		writeJSON(w, 200, v)
	case r.URL.Path == "/admin/login/complete" && r.Method == "POST":
		var b struct {
			ID       string `json:"id"`
			Callback string `json:"callback"`
			Name     string `json:"name"`
		}
		if decode(w, r, &b) != nil {
			fail(w, 400, "invalid_json")
			return
		}
		if e := a.completeLogin(r.Context(), b.ID, b.Callback, b.Name); e != nil {
			fail(w, 400, e.Error())
			return
		}
		writeJSON(w, 200, M{"ok": true})
	case r.URL.Path == "/admin/account" && r.Method == "POST":
		var b struct {
			ID     string `json:"id"`
			Action string `json:"action"`
			Name   string `json:"name"`
		}
		if decode(w, r, &b) != nil {
			fail(w, 400, "invalid_json")
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		for _, v := range a.busy {
			if v > 0 {
				fail(w, 409, "requests_in_flight")
				return
			}
		}
		a.refreshMu.Lock()
		defer a.refreshMu.Unlock()
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
		index := -1
		for i, v := range a.store.state.Accounts {
			if v.ID == b.ID {
				index = i
			}
		}
		if index < 0 {
			fail(w, 404, "account_not_found")
			return
		}
		switch b.Action {
		case "rename":
			if len(b.Name) == 0 || len(b.Name) > 120 {
				fail(w, 400, "invalid_name")
				return
			}
			a.store.state.Accounts[index].Name = b.Name
		case "enable":
			for i := range a.store.state.Accounts {
				a.store.state.Accounts[i].Enabled = i == index
			}
		case "disable":
			a.store.state.Accounts[index].Enabled = false
		case "delete":
			a.store.state.Accounts = append(a.store.state.Accounts[:index], a.store.state.Accounts[index+1:]...)
			delete(a.models, b.ID)
		default:
			fail(w, 400, "invalid_action")
			return
		}
		if e := a.store.saveLocked(); e != nil {
			fail(w, 500, "storage_write_failed")
			return
		}
		writeJSON(w, 200, M{"ok": true})
	case r.URL.Path == "/admin/models/refresh" && r.Method == "POST":
		acc, ok := a.store.active()
		if !ok {
			fail(w, 503, "no_enabled_account")
			return
		}
		_, e := a.getModels(r.Context(), acc, true)
		if e != nil {
			fail(w, 502, "model_catalog_unavailable")
			return
		}
		writeJSON(w, 200, M{"ok": true})
	case r.URL.Path == "/admin/model" && r.Method == "POST":
		var b struct {
			ID      string       `json:"id"`
			Setting ModelSetting `json:"setting"`
		}
		if decode(w, r, &b) != nil || b.ID == "" || len(b.Setting.Alias) > 120 {
			fail(w, 400, "invalid_model_setting")
			return
		}
		a.store.mu.Lock()
		a.store.state.Models[b.ID] = b.Setting
		e := a.store.saveLocked()
		a.store.mu.Unlock()
		if e != nil {
			fail(w, 500, "storage_write_failed")
			return
		}
		writeJSON(w, 200, M{"ok": true})
	case r.URL.Path == "/admin/key/rotate" && r.Method == "POST":
		a.store.mu.Lock()
		retained := []APIKey{}
		for _, k := range a.store.state.Keys {
			if k.ID != "legacy" {
				retained = append(retained, k)
			}
		}
		if len(retained) >= 32 {
			a.store.mu.Unlock()
			fail(w, 400, "key_limit_reached")
			return
		}
		a.store.state.APIKey = secret()
		a.store.state.Keys = retained
		key := a.store.state.APIKey
		e := a.store.saveLocked()
		a.store.mu.Unlock()
		if e != nil {
			fail(w, 500, "storage_write_failed")
			return
		}
		writeJSON(w, 200, M{"api_key": key})
	default:
		http.NotFound(w, r)
	}
}
func suffix(s string) string {
	if len(s) < 6 {
		return ""
	}
	return s[len(s)-6:]
}
