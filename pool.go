package main

import (
	"context"
	"errors"
	"math"
	"sort"
	"time"
)

var errNoAccount = errors.New("no_enabled_account")
var errPoolBusy = errors.New("account_concurrency_limit")

// pick reserves one concurrency slot on an enabled account. Saturated accounts
// and accounts still inside their minimum request interval are skipped; when
// none is free the call polls until cfg.QueueTimeout expires, so a brief burst
// waits instead of failing immediately.
func (a *App) pick(ctx context.Context, exclude map[string]bool) (Account, error) {
	deadline := time.Now().Add(a.cfg.QueueTimeout)
	for {
		a.mu.Lock()
		accounts, any := a.store.enabled()
		acc, ok := a.reserveSlot(accounts, exclude)
		a.mu.Unlock()
		if ok {
			return acc, nil
		}
		if !any {
			return Account{}, errNoAccount
		}
		if !time.Now().Before(deadline) {
			return Account{}, errPoolBusy
		}
		wait := min(50*time.Millisecond, time.Until(deadline))
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Account{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// reserveSlot must be called with a.mu held. It walks the enabled accounts in
// round-robin or credit order and books the first one with a free slot.
func (a *App) reserveSlot(accounts []Account, exclude map[string]bool) (Account, bool) {
	if len(accounts) == 0 {
		return Account{}, false
	}
	order := make([]int, 0, len(accounts))
	if a.cfg.SelectByCredits {
		for i := range accounts {
			order = append(order, i)
		}
		sort.SliceStable(order, func(x, y int) bool {
			left, lok := credits(accounts[order[x]])
			right, rok := credits(accounts[order[y]])
			if lok != rok {
				return lok
			}
			return lok && left > right
		})
	} else {
		for i := range accounts {
			order = append(order, (a.cursor+i)%len(accounts))
		}
	}
	now := time.Now()
	for _, i := range order {
		acc := accounts[i]
		if exclude[acc.ID] || a.busy[acc.ID] >= a.cfg.Concurrency {
			continue
		}
		if a.cfg.MinInterval > 0 {
			if last, ok := a.lastUsed[acc.ID]; ok && now.Sub(last) < a.cfg.MinInterval {
				continue
			}
		}
		a.busy[acc.ID]++
		a.lastUsed[acc.ID] = now
		a.cursor = (i + 1) % len(accounts)
		return acc, true
	}
	return Account{}, false
}

// credits reads the last known remaining balance; accounts without one sort last.
func credits(acc Account) (float64, bool) {
	if len(acc.Balance) == 0 {
		return 0, false
	}
	v := number(acc.Balance, "remaining", math.Inf(-1))
	if math.IsInf(v, 0) || math.IsNaN(v) {
		return 0, false
	}
	return v, true
}
