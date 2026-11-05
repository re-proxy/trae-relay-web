package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"time"
)

func (a *App) manageKey(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Action string `json:"action"`
	}
	if decode(w, r, &b) != nil || len(b.Name) > 120 {
		fail(w, 400, "invalid_key_request")
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	before := a.store.state.APIKey
	old := append([]APIKey{}, a.store.state.Keys...)
	keys := a.store.keysLocked()
	var issued string
	if b.Action == "create" {
		if len(keys) >= 32 {
			fail(w, 400, "key_limit_reached")
			return
		}
		if b.Name == "" {
			b.Name = "新密钥"
		}
		issued = secret()
		keys = append(keys, APIKey{ID: randomID(), Name: b.Name, Secret: issued, Enabled: true, Created: time.Now()})
	} else {
		found := false
		for i := range keys {
			if keys[i].ID != b.ID {
				continue
			}
			found = true
			switch b.Action {
			case "rename":
				if b.Name == "" {
					fail(w, 400, "name_required")
					return
				}
				keys[i].Name = b.Name
			case "enable":
				keys[i].Enabled = true
			case "disable":
				keys[i].Enabled = false
			case "delete":
				keys = append(keys[:i], keys[i+1:]...)
			default:
				fail(w, 400, "invalid_action")
				return
			}
			break
		}
		if !found {
			fail(w, 404, "key_not_found")
			return
		}
	}
	// Fold the legacy entry into the list on the first management mutation.
	a.store.state.APIKey = ""
	a.store.state.Keys = keys
	if e := a.store.saveLocked(); e != nil {
		a.store.state.APIKey = before
		a.store.state.Keys = old
		fail(w, 500, "storage_write_failed")
		return
	}
	result := M{"ok": true}
	if issued != "" {
		result["api_key"] = issued
	}
	writeJSON(w, 200, result)
}

func (a *App) accountInspect(w http.ResponseWriter, r *http.Request, balance bool) {
	var b struct {
		ID string `json:"id"`
	}
	if decode(w, r, &b) != nil {
		fail(w, 400, "invalid_json")
		return
	}
	a.store.mu.Lock()
	var acc Account
	found := false
	for _, v := range a.store.state.Accounts {
		if v.ID == b.ID {
			acc = v
			found = true
			break
		}
	}
	a.store.mu.Unlock()
	if !found {
		fail(w, 404, "account_not_found")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	start := time.Now()
	acc, e := a.fresh(ctx, acc, false)
	var result M
	if e == nil {
		if balance {
			result, e = a.queryBalance(ctx, acc)
		} else {
			result, e = a.checkAccount(ctx, acc)
		}
	}
	if result == nil {
		result = M{}
	}
	result["checked_at"] = time.Now().UTC()
	result["latency_ms"] = time.Since(start).Milliseconds()
	result["ok"] = e == nil
	if e != nil {
		result["error"] = e.Error()
	}
	a.store.mu.Lock()
	saved := false
	for i := range a.store.state.Accounts {
		if a.store.state.Accounts[i].ID == b.ID {
			if balance {
				a.store.state.Accounts[i].Balance = result
			} else {
				a.store.state.Accounts[i].Check = result
			}
			saved = true
			break
		}
	}
	var saveErr error
	if saved {
		saveErr = a.store.saveLocked()
	}
	a.store.mu.Unlock()
	if saveErr != nil {
		fail(w, 500, "storage_write_failed")
		return
	}
	if !saved {
		fail(w, 404, "account_not_found")
		return
	}
	// Diagnostic failures are data, so the UI can display a timestamped result.
	writeJSON(w, 200, result)
}
func (a *App) checkAccount(ctx context.Context, acc Account) (M, error) {
	if acc.Scope == "saas" {
		if e := a.verifyEnterprise(ctx, &acc); e != nil {
			return nil, e
		}
	} else {
		m, e := a.post(ctx, oauthHost, "/cloudide/api/v3/trae/GetUserInfo", M{"ReqSource": "IDE", "IDEVersion": a.cfg.Version}, acc)
		if e != nil {
			return nil, e
		}
		if id := str(obj(m["Result"]), "UserID"); id == "" || id != acc.UserID {
			return nil, errors.New("identity_verification_failed")
		}
	}
	models, e := a.getModels(ctx, acc, true)
	if e != nil {
		return nil, e
	}
	return M{"identity_verified": true, "model_count": len(models), "mode": "metadata_only"}, nil
}
func (a *App) queryBalance(ctx context.Context, acc Account) (M, error) {
	if acc.Scope == "saas" {
		if e := a.verifyEnterprise(ctx, &acc); e != nil {
			return nil, e
		}
		m, e := a.post(ctx, enterpriseHost, "/cloudide/api/v3/trae/GetUserInfo", M{"Attributes": []string{"currency_pricing"}}, acc)
		if e != nil {
			return nil, e
		}
		if number(m, "code", -1) != 0 {
			return nil, errors.New("enterprise_usage_rejected")
		}
		result := M{"kind": "enterprise", "details_url": enterpriseHost + "/personal/usage", "remaining": nil, "note": "实际消费和剩余额度请在企业控制台查看"}
		if !boolean(obj(obj(m["Attributes"])["currency_pricing"]), "Enable", false) {
			return result, nil
		}
		m, e = a.post(ctx, enterpriseHost, "/trae/gtm/tob/api/v1/config/plan_attribute", M{"attributes": []any{}}, acc)
		if e != nil {
			return nil, e
		}
		if number(m, "code", -1) != 0 {
			return nil, errors.New("enterprise_plan_rejected")
		}
		plan := obj(m["Data"])
		for _, v := range list(plan["Attributes"]) {
			attr := obj(v)
			if str(attr, "Attribute") == "currency_pricing" && boolean(attr, "Enable", false) {
				if n := number(attr, "Value", math.NaN()); !math.IsNaN(n) && !math.IsInf(n, 0) {
					result["plan_quota"] = n
					result["unit"] = "CNY"
				}
			}
		}
		return result, nil
	}
	m, e := a.post(ctx, oauthHost, "/trae/api/v2/pay/ide_user_ent_usage", M{"require_usage": true, "req_source": 0}, acc)
	if e != nil {
		return nil, e
	}
	if number(m, "code", 0) != 0 {
		return nil, errors.New("balance_query_rejected")
	}
	if n, ok := findBalance(m); ok {
		return M{"kind": "personal", "remaining": n, "unit": "积分"}, nil
	}
	return nil, errors.New("balance_not_available")
}

// Follow the original implementation's precedence: summary, events, then packs.
func findBalance(root any) (float64, bool) {
	for mode := 0; mode < 3; mode++ {
		if n, ok := walkBalance(root, mode, 0); ok {
			return n, true
		}
	}
	return 0, false
}
func finite(m M, k string) (float64, bool) {
	v := number(m, k, math.NaN())
	return v, !math.IsNaN(v) && !math.IsInf(v, 0)
}
func walkBalance(v any, mode, depth int) (float64, bool) {
	if depth > 10 {
		return 0, false
	}
	if m, ok := v.(map[string]any); ok {
		if mode == 0 {
			s := obj(m["usage_summary"])
			for _, k := range []string{"remaining_amount", "available_amount"} {
				if n, ok := finite(s, k); ok {
					return math.Max(0, n), true
				}
			}
			total, t := finite(s, "total_amount")
			used, u := finite(s, "consumed_amount")
			if t && u {
				return math.Max(0, total-used), true
			}
		}
		if mode == 1 {
			for _, k := range []string{"cn_credits_remain_info", "credits_remain_info", "creditsRemainInfo"} {
				info := obj(m[k])
				for _, f := range []string{"ide_credits", "ideCredits", "remaining_amount", "available_amount"} {
					if n, ok := finite(info, f); ok {
						return math.Max(0, n), true
					}
				}
			}
			for _, f := range []string{"ide_credits", "ideCredits"} {
				if n, ok := finite(m, f); ok {
					return math.Max(0, n), true
				}
			}
		}
		if mode == 2 {
			sum := 0.0
			found := false
			for _, p := range list(m["user_entitlement_pack_list"]) {
				item := obj(p)
				base := obj(item["entitlement_base_info"])
				end := number(item, "expire_time", number(base, "end_time", 0))
				if end > 0 && end <= float64(time.Now().Unix()) {
					continue
				}
				limit, ok := finite(obj(base["quota"]), "credits_limit")
				if !ok || limit < 0 {
					continue
				}
				usage := obj(item["usage"])
				if len(usage) == 0 {
					usage = obj(base["usage"])
				}
				used := number(usage, "credits_amount", 0)
				if math.IsNaN(used) || math.IsInf(used, 0) {
					continue
				}
				sum += math.Max(0, limit-used)
				found = true
			}
			if found {
				return sum, true
			}
		}
		for _, child := range m {
			if n, ok := walkBalance(child, mode, depth+1); ok {
				return n, true
			}
		}
	} else if arr, ok := v.([]any); ok {
		for _, child := range arr {
			if n, ok := walkBalance(child, mode, depth+1); ok {
				return n, true
			}
		}
	}
	return 0, false
}
