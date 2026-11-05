package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Daily check-in for personal accounts. The endpoints and the multi-key flag
// search follow the reference relay, because upstream answers in several shapes
// depending on account state.

const (
	checkinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	checkinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"
	entUsagePath      = "/trae/api/v2/pay/ide_user_ent_usage"
)

var checkinRunning atomic.Bool

// checkinDeviceID mirrors the reference relay: a stable identity derived from
// the user id, deliberately independent from the web-login device id so signing
// in again never changes it.
func checkinDeviceID(userID string) string {
	sum := sha256.Sum256([]byte("TraeRelay-checkin-probe:" + userID))
	v := binary.BigEndian.Uint64(sum[:8])
	return strconv.FormatUint(100000000000000+v%900000000000000, 10)
}

func intValue(v any) (int, bool) {
	switch x := v.(type) {
	case float64:
		return int(x), true
	case int:
		return x, true
	case json.Number:
		n, e := x.Int64()
		return int(n), e == nil
	case string:
		n, e := strconv.Atoi(x)
		return n, e == nil
	}
	return 0, false
}

// checkinDeepCode finds a business code anywhere in the response; a non-zero
// code wins over the outer code=0 wrapper.
func checkinDeepCode(v any, depth int) (int, bool) {
	if depth > 8 {
		return 0, false
	}
	switch t := v.(type) {
	case map[string]any:
		found := false
		for _, k := range []string{"code", "status_code", "error_code"} {
			raw, ok := t[k]
			if !ok {
				continue
			}
			n, parsed := intValue(raw)
			if !parsed {
				continue
			}
			found = true
			if n != 0 {
				return n, true
			}
		}
		for _, child := range t {
			n, ok := checkinDeepCode(child, depth+1)
			if !ok {
				continue
			}
			found = true
			if n != 0 {
				return n, true
			}
		}
		return 0, found
	case []any:
		found := false
		for _, child := range t {
			n, ok := checkinDeepCode(child, depth+1)
			if !ok {
				continue
			}
			found = true
			if n != 0 {
				return n, true
			}
		}
		return 0, found
	}
	return 0, false
}

func flagValue(v any) (bool, bool) {
	switch x := v.(type) {
	case bool:
		return x, true
	case float64:
		return x != 0, true
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes", "already", "checked_in":
			return true, true
		case "false", "0", "no":
			return false, true
		}
	}
	return false, false
}

// checkinFlag looks for the "already checked in" flag. checked_in and friends
// take precedence so a sibling did_checked_in=false cannot hide a claimed day.
func checkinFlag(v any, depth int) (bool, bool) {
	if depth > 8 {
		return false, false
	}
	switch t := v.(type) {
	case map[string]any:
		found := false
		for _, k := range []string{"checked_in", "did_checked_in", "didCheckedIn"} {
			raw, ok := t[k]
			if !ok {
				continue
			}
			b, parsed := flagValue(raw)
			if !parsed {
				continue
			}
			found = true
			if b {
				return true, true
			}
		}
		if found {
			return false, true
		}
		for _, k := range []string{"already_checked_in", "is_checked_in", "has_checked_in", "isCheckIn", "checkedIn", "claimed", "alreadyClaimed", "checkin_status", "checkinStatus"} {
			if b, ok := flagValue(t[k]); ok {
				return b, true
			}
		}
		for _, child := range t {
			if b, ok := checkinFlag(child, depth+1); ok {
				return b, true
			}
		}
		return false, false
	case []any:
		for _, child := range t {
			if b, ok := checkinFlag(child, depth+1); ok {
				return b, true
			}
		}
	}
	return false, false
}

// checkinState reduces a status response to 1 already checked, 2 not checked
// yet, -2 account-level rejection and -1 unknown.
func checkinState(m M) int {
	if code, ok := checkinDeepCode(m, 0); ok && code != 0 {
		if code == 9095 {
			return -2
		}
		return -1
	}
	checked, ok := checkinFlag(m, 0)
	if !ok {
		return -1
	}
	if !checked {
		return 2
	}
	if raw, present := m["checked_in"]; present {
		if b, ok := flagValue(raw); ok && !b {
			return -2
		}
	}
	return 1
}

func (a *App) checkinPost(ctx context.Context, acc Account, path string, body M) (M, bool) {
	raw, _ := json.Marshal(body)
	req, e := http.NewRequestWithContext(ctx, "POST", oauthHost+path, bytes.NewReader(raw))
	if e != nil {
		return nil, false
	}
	a.headers(req, acc, false)
	// Only device-key accounts may override the identity header; the reference
	// relay keeps the web-login device id for the other flows.
	if len(acc.PrivateKey) > 0 {
		req.Header.Set("x-device-id", checkinDeviceID(acc.UserID))
	}
	resp, e := a.client.Do(req)
	if e != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, false
	}
	var m M
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&m) != nil {
		return nil, false
	}
	return m, true
}

// entTotal reads the account-wide credit total; the remaining balance is not
// used because concurrent chats can lower it while a claim settles.
func (a *App) entTotal(ctx context.Context, acc Account) (float64, bool) {
	m, e := a.post(ctx, oauthHost, entUsagePath, M{"require_usage": true, "req_source": 0}, acc)
	if e != nil {
		return 0, false
	}
	if code, ok := checkinDeepCode(m, 0); ok && code != 0 {
		return 0, false
	}
	n, ok := finite(obj(m["usage_summary"]), "total_amount")
	return n, ok
}

func (a *App) refreshCredits(ctx context.Context, acc Account) {
	start := time.Now()
	balance, e := a.queryBalance(ctx, acc)
	if e != nil {
		return
	}
	// Same shape the console uses, so a refreshed balance renders instead of
	// looking like a failed query.
	balance["checked_at"] = time.Now().UTC()
	balance["latency_ms"] = time.Since(start).Milliseconds()
	balance["ok"] = true
	a.store.mu.Lock()
	for i := range a.store.state.Accounts {
		if a.store.state.Accounts[i].ID == acc.ID {
			a.store.state.Accounts[i].Balance = balance
			break
		}
	}
	e = a.store.saveLocked()
	a.store.mu.Unlock()
	if e != nil {
		log.Printf("storage error: %v", e)
	}
}

// doCheckin returns 0 credited, 1 already claimed today, -1 failed, -2 account
// rejected the request and -3 accepted but credit not confirmed yet.
func (a *App) doCheckin(ctx context.Context, acc Account) int {
	if acc.Scope == "saas" || acc.UserID == "" {
		return -1
	}
	if next, e := a.fresh(ctx, acc, false); e == nil {
		acc = next
	} else {
		return -1
	}
	body := M{"req_source": 1}
	status, ok := a.checkinPost(ctx, acc, checkinStatusPath, body)
	if !ok {
		return -1
	}
	switch checkinState(status) {
	case 1:
		a.refreshCredits(ctx, acc)
		return 1
	case 2:
	default:
		return -1
	}
	before, beforeOK := a.entTotal(ctx, acc)
	claim, ok := a.checkinPost(ctx, acc, checkinClaimPath, body)
	if !ok {
		// A timeout can follow a successful claim, so never replay it blindly.
		if status, ok := a.checkinPost(ctx, acc, checkinStatusPath, body); ok && checkinState(status) == 1 {
			a.refreshCredits(ctx, acc)
			return -3
		}
		return -1
	}
	code, found := checkinDeepCode(claim, 0)
	if found && code != 0 {
		log.Printf("checkin rejected account=%s code=%d", acc.ID, code)
		if code == 9095 {
			return -2
		}
		return -1
	}
	if !found {
		return -3
	}
	confirmed := false
	if status, ok := a.checkinPost(ctx, acc, checkinStatusPath, body); ok && checkinState(status) == 1 {
		confirmed = true
	}
	after, afterOK := a.entTotal(ctx, acc)
	a.refreshCredits(ctx, acc)
	if confirmed && beforeOK && afterOK && after > before {
		log.Printf("checkin credited account=%s amount=%.1f", acc.ID, after-before)
		return 0
	}
	log.Printf("checkin accepted account=%s; credit confirmation pending", acc.ID)
	return -3
}

func checkinDay(now time.Time) string { return now.Format("20060102") }

func checkinDue(acc Account, day string, now time.Time) bool {
	if acc.Scope == "saas" || acc.UserID == "" || acc.CheckinOff {
		return false
	}
	if acc.CheckinDay == day {
		switch acc.CheckinResult {
		case 0, 1, -2, -3:
			return false
		}
	}
	return acc.CheckinDay != day || acc.CheckinAt <= now.Unix()
}

// runCheckins performs every due check-in sequentially; a single failing
// account never blocks the others.
func (a *App) runCheckins(ctx context.Context, now time.Time) {
	if !checkinRunning.CompareAndSwap(false, true) {
		return
	}
	defer checkinRunning.Store(false)
	day := checkinDay(now)
	for _, acc := range a.store.all() {
		if !checkinDue(acc, day, now) {
			continue
		}
		run, cancel := context.WithTimeout(ctx, 90*time.Second)
		result := a.doCheckin(run, acc)
		cancel()
		if e := a.store.recordCheckin(acc.ID, day, result); e != nil {
			log.Printf("storage error: %v", e)
		}
	}
}

func (a *App) checkinWorker(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if !a.cfg.CheckinEnabled {
			continue
		}
		now := time.Now()
		if now.Hour()*60+now.Minute() < a.cfg.CheckinHour*60+a.cfg.CheckinMinute {
			continue
		}
		a.runCheckins(ctx, now)
	}
}

func checkinMessage(result int) string {
	switch result {
	case 0:
		return "签到成功，积分已入账"
	case 1:
		return "今日已签到"
	case -2:
		return "账号暂时不支持签到"
	case -3:
		return "已提交，积分确认中"
	}
	return "签到失败，稍后自动重试"
}

// manageCheckin runs a check-in now, or toggles the automatic schedule.
func (a *App) manageCheckin(w http.ResponseWriter, r *http.Request) {
	var b struct {
		ID      string `json:"id"`
		Action  string `json:"action"`
		Enabled bool   `json:"enabled"`
	}
	if decode(w, r, &b) != nil {
		fail(w, 400, "invalid_json")
		return
	}
	acc, found := a.store.find(b.ID)
	if !found {
		fail(w, 404, "account_not_found")
		return
	}
	if b.Action == "auto" {
		if acc.Scope == "saas" {
			fail(w, 400, "checkin_personal_only")
			return
		}
		ok, e := a.store.setCheckinOff(b.ID, !b.Enabled)
		if e != nil || !ok {
			fail(w, 500, "storage_write_failed")
			return
		}
		writeJSON(w, 200, M{"ok": true, "enabled": b.Enabled})
		return
	}
	if acc.Scope == "saas" {
		fail(w, 400, "checkin_personal_only")
		return
	}
	if !checkinRunning.CompareAndSwap(false, true) {
		fail(w, 409, "checkin_in_progress")
		return
	}
	defer checkinRunning.Store(false)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	result := a.doCheckin(ctx, acc)
	if e := a.store.recordCheckin(acc.ID, checkinDay(time.Now()), result); e != nil {
		fail(w, 500, "storage_write_failed")
		return
	}
	writeJSON(w, 200, M{"ok": result == 0 || result == 1 || result == -3, "result": result, "message": checkinMessage(result)})
}
