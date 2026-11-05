package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

func TestCheckinDeviceID(t *testing.T) {
	cases := map[string]string{
		"3753108912225017": "859912485447222",
		"1":                "708130746575025",
		"user-abc":         "222530924087923",
	}
	for uid, want := range cases {
		if got := checkinDeviceID(uid); got != want {
			t.Fatalf("%s → %s, want %s", uid, got, want)
		}
	}
	if len(checkinDeviceID("x")) != 15 {
		t.Fatal("device id must be a 15 digit number")
	}
}

func TestCheckinStateParsing(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{`{"code":0,"Data":{"checked_in":true}}`, 1},
		{`{"code":0,"Data":{"checked_in":false}}`, 2},
		// checked_in wins over a contradicting did_checked_in.
		{`{"Data":{"checked_in":true,"did_checked_in":false}}`, 1},
		{`{"Data":{"already_checked_in":"yes"}}`, 1},
		{`{"Data":{"checkin_status":0}}`, 2},
		{`{"code":9095}`, -2},
		{`{"code":5001}`, -1},
		{`{"Data":{}}`, -1},
	}
	for _, c := range cases {
		var m M
		if e := json.Unmarshal([]byte(c.body), &m); e != nil {
			t.Fatal(e)
		}
		if got := checkinState(m); got != c.want {
			t.Fatalf("%s → %d, want %d", c.body, got, c.want)
		}
	}
}

func TestCheckinClaimsAndConfirms(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	acc, _ := a.store.find("acc")
	claims, status, totals := 0, 0, 0
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case checkinStatusPath:
			status++
			if claims > 0 {
				fmt.Fprint(w, `{"code":0,"Data":{"checked_in":true}}`)
			} else {
				fmt.Fprint(w, `{"code":0,"Data":{"checked_in":false}}`)
			}
		case checkinClaimPath:
			claims++
			fmt.Fprint(w, `{"code":0}`)
		case entUsagePath:
			totals++
			amount := 2500
			if claims > 0 {
				amount = 2510
			}
			fmt.Fprintf(w, `{"code":0,"usage_summary":{"total_amount":%d,"consumed_amount":0}}`, amount)
		default:
			w.WriteHeader(404)
		}
	})
	if got := a.doCheckin(context.Background(), acc); got != 0 {
		t.Fatalf("doCheckin = %d, want 0", got)
	}
	if claims != 1 || status < 2 || totals < 2 {
		t.Fatalf("claims=%d status=%d totals=%d", claims, status, totals)
	}
	if e := a.store.recordCheckin("acc", checkinDay(time.Now()), 0); e != nil {
		t.Fatal(e)
	}
	acc, _ = a.store.find("acc")
	if checkinDue(acc, checkinDay(time.Now()), time.Now()) {
		t.Fatal("a credited day must not be due again")
	}
	// The refreshed balance is stored so the console shows the added credits.
	bal := obj(acc.Balance)
	if number(bal, "remaining", -1) != 2510 {
		t.Fatalf("balance not refreshed: %v", acc.Balance)
	}
	if bal["ok"] != true || bal["checked_at"] == nil || number(bal, "latency_ms", -1) < 0 {
		t.Fatalf("refreshed balance is not console-shaped: %v", acc.Balance)
	}
}

func TestCheckinClaimFailureRechecksStatus(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	acc, _ := a.store.find("acc")
	claims := 0
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case checkinStatusPath:
			if claims > 0 {
				fmt.Fprint(w, `{"code":0,"Data":{"checked_in":true}}`)
			} else {
				fmt.Fprint(w, `{"code":0,"Data":{"checked_in":false}}`)
			}
		case checkinClaimPath:
			claims++
			w.WriteHeader(500)
		case entUsagePath:
			fmt.Fprint(w, `{"code":0,"usage_summary":{"total_amount":2500,"consumed_amount":0}}`)
		default:
			w.WriteHeader(404)
		}
	})
	// A failed claim may still have been credited upstream, so the relay must
	// re-read the status instead of retrying the claim.
	if got := a.doCheckin(context.Background(), acc); got != -3 {
		t.Fatalf("doCheckin = %d, want -3", got)
	}
	if claims != 1 {
		t.Fatalf("claim replayed %d times", claims)
	}
}

func TestCheckinUsesStableDeviceID(t *testing.T) {
	a := testApp(t)
	seed(t, a)
	acc, _ := a.store.find("acc")
	acc.PrivateKey = []byte("device-key")
	if e := a.store.putAccount(acc); e != nil {
		t.Fatal(e)
	}
	seen := map[string]string{}
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Get("x-device-id")
		fmt.Fprint(w, `{"code":0,"Data":{"checked_in":true}}`)
	})
	if got := a.doCheckin(context.Background(), acc); got != 1 {
		t.Fatalf("doCheckin = %d, want 1", got)
	}
	if seen[checkinStatusPath] != checkinDeviceID(acc.UserID) {
		t.Fatalf("x-device-id = %q", seen[checkinStatusPath])
	}
}

func TestCheckinDueRules(t *testing.T) {
	now := time.Now()
	day := checkinDay(now)
	base := Account{ID: "a", Scope: "trae", UserID: "uid"}
	if !checkinDue(base, day, now) {
		t.Fatal("a fresh personal account must be due")
	}
	saas := base
	saas.Scope = "saas"
	if checkinDue(saas, day, now) {
		t.Fatal("enterprise accounts never check in")
	}
	off := base
	off.CheckinOff = true
	if checkinDue(off, day, now) {
		t.Fatal("opt-out must be honoured")
	}
	for _, result := range []int{0, 1, -2, -3} {
		done := base
		done.CheckinDay = day
		done.CheckinResult = result
		if checkinDue(done, day, now) {
			t.Fatalf("result %d must not repeat today", result)
		}
	}
	failed := base
	failed.CheckinDay = day
	failed.CheckinResult = -1
	failed.CheckinAt = now.Add(10 * time.Minute).Unix()
	if checkinDue(failed, day, now) {
		t.Fatal("a failure waits for its retry time")
	}
	failed.CheckinAt = now.Add(-time.Minute).Unix()
	if !checkinDue(failed, day, now) {
		t.Fatal("a due retry must run")
	}
	yesterday := base
	yesterday.CheckinDay = now.AddDate(0, 0, -1).Format("20060102")
	yesterday.CheckinResult = 0
	if !checkinDue(yesterday, day, now) {
		t.Fatal("a new day must check in again")
	}
}
