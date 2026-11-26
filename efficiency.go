package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	intelligenceURL = "https://artificialanalysis.ai/api/v2/language/models"
	taskBenchmark   = "terminalbench_v4_1"
	tasksPerRun     = 66 // Terminal-Bench 4.1, per the Artificial Analysis benchmarking methodology.
	benchmarkTTL    = 6 * time.Hour
)

var errBenchmarkPlan = errors.New("Artificial Analysis requires Pro API access")

type benchmarkModel struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Evaluations struct {
		Intelligence *float64 `json:"artificial_analysis_intelligence_index"`
	} `json:"evaluations"`
}

type benchmarkSnapshot struct {
	version   float64
	updatedAt time.Time
	status    string
	byID      map[string]*benchmarkModel
	bySlug    map[string]*benchmarkModel
	byName    map[string]*benchmarkModel
	usage     map[string]taskUsage
}

type taskUsage struct {
	average   float64
	refreshAt time.Time
	failed    bool
}

func (a *App) benchmarkJSON(ctx context.Context, endpoint string, target any) error {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", a.cfg.ArtificialAnalysisKey)
	res, err := a.client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	switch res.StatusCode {
	case http.StatusOK:
		return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(target)
	case http.StatusForbidden:
		return errBenchmarkPlan
	default:
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
}

func (a *App) fetchBenchmarks(ctx context.Context) (benchmarkSnapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	snapshot := benchmarkSnapshot{
		status: "ready",
		byID:   map[string]*benchmarkModel{},
		bySlug: map[string]*benchmarkModel{},
		byName: map[string]*benchmarkModel{},
	}
	total := 0
	for page := 1; page <= 50; page++ {
		var result struct {
			Version    float64 `json:"intelligence_index_version"`
			Pagination struct {
				Page    int  `json:"page"`
				Total   int  `json:"total_pages"`
				HasMore bool `json:"has_more"`
			} `json:"pagination"`
			Data []benchmarkModel `json:"data"`
		}
		if err := a.benchmarkJSON(ctx, fmt.Sprintf("%s?page=%d", intelligenceURL, page), &result); err != nil {
			return benchmarkSnapshot{}, err
		}
		if page == 1 {
			snapshot.version = result.Version
			total = result.Pagination.Total
		}
		if result.Version <= 0 || result.Version != snapshot.version {
			return benchmarkSnapshot{}, errors.New("inconsistent Intelligence Index version")
		}
		if result.Pagination.Page != page || result.Pagination.Total != total || total < page || result.Pagination.HasMore != (page < total) || len(result.Data) == 0 {
			return benchmarkSnapshot{}, fmt.Errorf("incomplete model list at page %d", page)
		}
		for i := range result.Data {
			row := &result.Data[i]
			if row.ID == "" {
				continue
			}
			snapshot.byID[row.ID] = row
			indexBenchmark(snapshot.bySlug, row.Slug, row)
			indexBenchmark(snapshot.byName, benchmarkName(row.Name), row)
		}
		if !result.Pagination.HasMore {
			snapshot.updatedAt = time.Now()
			return snapshot, nil
		}
	}
	return benchmarkSnapshot{}, errors.New("too many model pages")
}

func indexBenchmark(index map[string]*benchmarkModel, key string, row *benchmarkModel) {
	if key == "" {
		return
	}
	if existing, ok := index[key]; ok && (existing == nil || existing.ID != row.ID) {
		index[key] = nil // A shared name or slug needs an explicit model ID.
		return
	}
	index[key] = row
}

func (a *App) fetchTaskTokens(ctx context.Context, id string, version float64) (float64, error) {
	var result struct {
		Version float64 `json:"intelligence_index_version"`
		Data    struct {
			ID     string `json:"id"`
			Counts map[string]struct {
				Input  *float64 `json:"input_tokens"`
				Output *float64 `json:"output_tokens"`
			} `json:"evaluation_token_counts"`
		} `json:"data"`
	}
	if err := a.benchmarkJSON(ctx, intelligenceURL+"/"+url.PathEscape(id), &result); err != nil {
		return 0, err
	}
	if result.Data.ID != id || result.Version != version {
		return 0, errors.New("benchmark detail does not match the model or Intelligence Index version")
	}
	counts := result.Data.Counts[taskBenchmark]
	if counts.Input == nil || counts.Output == nil || *counts.Input < 0 || *counts.Output < 0 {
		return 0, nil
	}
	// Output already includes reasoning tokens. The per-evaluation totals exclude repeats.
	return *counts.Input/tasksPerRun + *counts.Output/tasksPerRun, nil
}

func (a *App) benchmarkWorker(ctx context.Context) {
	if a.cfg.ArtificialAnalysisKey == "" {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	var snapshot benchmarkSnapshot
	var nextCatalog time.Time
	usage := map[string]taskUsage{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		now := time.Now()
		if !now.Before(nextCatalog) {
			next, err := a.fetchBenchmarks(ctx)
			switch {
			case err == nil:
				if snapshot.version != next.version {
					clear(usage)
				}
				snapshot = next
				nextCatalog = time.Now().Add(benchmarkTTL)
			case errors.Is(err, errBenchmarkPlan):
				snapshot.status = "plan_required"
				nextCatalog = time.Now().Add(time.Hour)
			default:
				snapshot.status = "unavailable"
				nextCatalog = time.Now().Add(time.Hour)
				if ctx.Err() == nil {
					log.Printf("Artificial Analysis model list: %v", err)
				}
			}
		}
		if snapshot.status == "ready" {
			models, settings := a.benchmarkCatalog()
			matches, _ := a.benchmarkMatches(models, settings, snapshot)
			for _, match := range matches {
				id := match.benchmark.ID
				measurement := usage[id]
				if time.Now().Before(measurement.refreshAt) {
					continue
				}
				average, err := a.fetchTaskTokens(ctx, id, snapshot.version)
				if ctx.Err() != nil {
					return
				}
				if err != nil {
					measurement.failed = true
					measurement.refreshAt = time.Now().Add(time.Hour)
					usage[id] = measurement
					log.Printf("Artificial Analysis task tokens for %s: %v", id, err)
					continue
				}
				usage[id] = taskUsage{average: average, refreshAt: time.Now().Add(benchmarkTTL)}
			}
		}
		if ctx.Err() != nil {
			return
		}
		snapshot.usage = maps.Clone(usage)
		a.mu.Lock()
		a.benchmarks = snapshot
		a.mu.Unlock()
		timer.Reset(30 * time.Second)
	}
}

func (a *App) benchmarkCatalog() ([]Model, map[string]ModelSetting) {
	a.mu.Lock()
	acc, ok := a.store.active()
	models := a.models[acc.ID].Models
	a.mu.Unlock()
	if !ok {
		return nil, nil
	}
	a.store.mu.Lock()
	settings := maps.Clone(a.store.state.Models)
	a.store.mu.Unlock()
	return models, settings
}

func benchmarkName(name string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}), "-")
}

func upstreamBenchmarkName(name string) string {
	return benchmarkName(strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(name), "__dev"), "__max"))
}

func (snapshot benchmarkSnapshot) match(model Model, max bool, effort string, aliases map[string]string) *benchmarkModel {
	name := model.ModelName
	if max && model.MaxModel != "" {
		name = model.MaxModel
	}
	key := name
	if effort != "" {
		key += "@" + effort
	}
	if target := aliases[key]; target != "" {
		if row := snapshot.byID[target]; row != nil {
			return row
		}
		return snapshot.bySlug[target]
	}

	base := upstreamBenchmarkName(name)
	names := []string{base}
	if base == upstreamBenchmarkName(model.ModelName) {
		names = append(names, benchmarkName(model.ID), benchmarkName(model.Name))
	}
	for _, candidate := range names {
		if candidate == "" {
			continue
		}
		if effort != "" {
			suffix := "-" + benchmarkName(effort)
			if !strings.HasSuffix(candidate, suffix) {
				candidate += suffix
			}
		}
		if row, found := snapshot.byName[candidate]; found {
			return row
		}
	}
	return nil
}

type efficiencyCandidate struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Rate          float64 `json:"rate"`
	OffPeak       bool    `json:"off_peak,omitempty"`
	Intelligence  float64 `json:"intelligence"`
	Max           bool    `json:"max"`
	Effort        string  `json:"effort,omitempty"`
	TokensPerTask float64 `json:"tokens_per_task"`
	CostPerTask   float64 `json:"cost_per_task"`
}

type benchmarkMatch struct {
	candidate efficiencyCandidate
	benchmark *benchmarkModel
	pricing   *ModelPricing
}

func (a *App) benchmarkMatches(models []Model, settings map[string]ModelSetting, snapshot benchmarkSnapshot) ([]benchmarkMatch, int) {
	var matches []benchmarkMatch
	enabled := 0
	for _, model := range models {
		setting := settings[model.ID]
		if setting.Disabled {
			continue
		}
		enabled++
		max := setting.Max && model.Max
		pricing := model.Pricing
		if max {
			pricing = model.MaxPricing
		}
		if pricing == nil || (pricing.Standard == nil && (!pricing.isOffPeak() || pricing.Activity == nil || len(pricing.Windows) == 0)) {
			continue
		}
		effort, _ := selectedReasoningEffort(model, setting, "")
		row := snapshot.match(model, max, effort, a.cfg.ArtificialAnalysisAliases)
		if row == nil || row.Evaluations.Intelligence == nil {
			continue
		}
		matches = append(matches, benchmarkMatch{
			candidate: efficiencyCandidate{
				ID: model.ID, Name: model.Name,
				Intelligence: *row.Evaluations.Intelligence, Max: max, Effort: effort,
			},
			benchmark: row,
			pricing:   pricing,
		})
	}
	return matches, enabled
}

func (a *App) efficiency(models []Model, settings map[string]ModelSetting, snapshot benchmarkSnapshot, now time.Time) M {
	if a.cfg.ArtificialAnalysisKey == "" {
		return M{"status": "unconfigured"}
	}
	status := snapshot.status
	if snapshot.updatedAt.IsZero() {
		if status == "" {
			status = "loading"
		}
		return M{"status": status}
	}
	if status != "ready" {
		status = "stale"
	}
	matches, enabled := a.benchmarkMatches(models, settings, snapshot)
	candidates := []efficiencyCandidate{}
	pending := false
	usageFailed := false
	for _, match := range matches {
		rate, offPeak := match.pricing.rateAt(now)
		if rate == nil {
			continue
		}
		measurement, loaded := snapshot.usage[match.benchmark.ID]
		pending = pending || !loaded
		usageFailed = usageFailed || measurement.failed
		if measurement.average <= 0 {
			continue
		}
		candidate := match.candidate
		candidate.Rate = *rate
		candidate.OffPeak = offPeak
		candidate.TokensPerTask = measurement.average
		candidate.CostPerTask = candidate.Rate * measurement.average
		if !math.IsInf(candidate.CostPerTask, 0) {
			candidates = append(candidates, candidate)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.CostPerTask != right.CostPerTask {
			return left.CostPerTask < right.CostPerTask
		}
		if left.Intelligence != right.Intelligence {
			return left.Intelligence > right.Intelligence
		}
		return left.ID < right.ID
	})
	return M{"status": status, "updated_at": snapshot.updatedAt, "version": snapshot.version, "enabled": enabled,
		"pending": pending, "usage_failed": usageFailed, "candidates": candidates}
}
