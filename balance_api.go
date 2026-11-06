package main

import (
	"context"
	"math"
	"net/http"
	"sync"
	"time"
)

const balanceCacheTTL = 60 * time.Second

type AccountBalance struct {
	AccountID  string    `json:"account_id"`
	Kind       string    `json:"kind"`
	OK         bool      `json:"ok"`
	Remaining  *float64  `json:"remaining"`
	Unit       string    `json:"unit,omitempty"`
	PlanQuota  *float64  `json:"plan_quota,omitempty"`
	CheckedAt  time.Time `json:"checked_at"`
	Cached     bool      `json:"cached"`
	Error      string    `json:"error,omitempty"`
	DetailsURL string    `json:"details_url,omitempty"`
}

func balanceTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		parsed, _ := time.Parse(time.RFC3339Nano, t)
		return parsed
	}
	return time.Time{}
}
func cachedBalance(v M) bool {
	t := balanceTime(v["checked_at"])
	age := time.Since(t)
	_, ok := v["ok"].(bool)
	return ok && !t.IsZero() && age >= 0 && age < balanceCacheTTL
}

func (a *App) readAccountBalance(ctx context.Context, acc Account) AccountBalance {
	// Do not trust an old snapshot after waiting for the shared refresh gate.
	current, found := a.store.find(acc.ID)
	if !found || !current.Enabled {
		return AccountBalance{AccountID: acc.ID, Kind: "unknown", Error: "account_unavailable"}
	}
	acc = current
	value := acc.Balance
	cached := cachedBalance(value)
	if !cached {
		value = nil
		call, cancel := context.WithTimeout(ctx, 12*time.Second)
		defer cancel()
		fresh, e := a.fresh(call, acc, false)
		if e == nil {
			value, e = a.queryBalance(call, fresh)
		}
		if ctx.Err() != nil {
			return AccountBalance{AccountID: acc.ID, Kind: "unknown", Error: "balance_query_cancelled"}
		}
		if value == nil {
			value = M{}
		}
		value["checked_at"] = time.Now().UTC()
		value["ok"] = e == nil
		if e != nil {
			value["error"] = publicBalanceError(e.Error())
		}
		a.store.mu.Lock()
		for i := range a.store.state.Accounts {
			if a.store.state.Accounts[i].ID == acc.ID {
				a.store.state.Accounts[i].Balance = value
				break
			}
		}
		err := a.store.saveLocked()
		a.store.mu.Unlock()
		if err != nil {
			return AccountBalance{AccountID: acc.ID, Kind: "unknown", CheckedAt: balanceTime(value["checked_at"]), Error: "storage_write_failed"}
		}
	}
	result := AccountBalance{AccountID: acc.ID, Kind: "personal", Unit: "credits", OK: boolean(value, "ok", false), CheckedAt: balanceTime(value["checked_at"]), Cached: cached}
	if acc.Scope == "saas" {
		result.Kind = "enterprise"
		result.Unit = "CNY"
		result.DetailsURL = enterpriseHost + "/personal/usage"
	}
	if !result.OK {
		result.Error = publicBalanceError(str(value, "error"))
		return result
	}
	if result.Kind == "personal" {
		n, ok := finite(value, "remaining")
		if !ok || n < 0 {
			result.OK = false
			result.Error = "balance_not_available"
		} else {
			result.Remaining = &n
		}
	} else {
		if n, ok := finite(value, "plan_quota"); ok && n >= 0 {
			result.PlanQuota = &n
		}
	}
	return result
}

func publicBalanceError(s string) string {
	if len(s) == 0 || len(s) > 80 {
		return "balance_query_failed"
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return "balance_query_failed"
		}
	}
	return s
}

// balanceAPI returns upstream pool credits, not this API key's own quota. It
// never enters the inference queue or returns account names/identity/tokens.
func (a *App) balanceAPI(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	select {
	case a.balanceGate <- struct{}{}:
		defer func() { <-a.balanceGate }()
	case <-ctx.Done():
		fail(w, 504, "balance_query_timeout")
		return
	}
	accounts, ok := a.store.enabled()
	if !ok {
		fail(w, 503, "no_enabled_account")
		return
	}
	results := make([]AccountBalance, len(accounts))
	slots := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, acc := range accounts {
		wg.Add(1)
		go func(i int, acc Account) {
			defer wg.Done()
			select {
			case slots <- struct{}{}:
				defer func() { <-slots }()
				results[i] = a.readAccountBalance(ctx, acc)
			case <-ctx.Done():
				results[i] = AccountBalance{AccountID: acc.ID, Kind: "unknown", Error: "balance_query_timeout"}
			}
		}(i, acc)
	}
	wg.Wait()
	if ctx.Err() != nil {
		fail(w, 504, "balance_query_timeout")
		return
	}
	total := 0.0
	known, succeeded := 0, 0
	for _, v := range results {
		if v.OK {
			succeeded++
		}
		if v.OK && v.Kind == "personal" && v.Remaining != nil {
			total += *v.Remaining
			known++
		}
	}
	var balance *float64
	if known > 0 && !math.IsInf(total, 0) && !math.IsNaN(total) {
		balance = &total
	}
	status := 200
	if succeeded == 0 {
		status = 502
	}
	writeJSON(w, status, M{"object": "balance", "balance": balance, "unit": "credits", "complete": known == len(accounts) && balance != nil, "ok": succeeded > 0, "known_accounts": known, "unknown_accounts": len(accounts) - known, "accounts": results, "cache_ttl_seconds": int(balanceCacheTTL / time.Second)})
}
