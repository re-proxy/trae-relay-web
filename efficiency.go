package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

const (
	intelligenceURL = "https://artificialanalysis.ai/api/v2/language/models/free"
	benchmarkTTL    = 6 * time.Hour
)

type benchmarkModel struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Evaluations struct {
		Intelligence *float64 `json:"artificial_analysis_intelligence_index"`
	} `json:"evaluations"`
}

type benchmarkModelOption struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Intelligence float64 `json:"intelligence"`
}

type benchmarkScores struct {
	version   float64
	updatedAt time.Time
	byID      map[string]*benchmarkModel
	bySlug    map[string]*benchmarkModel
	byName    map[string]*benchmarkModel
}

type benchmarkSnapshot struct {
	benchmarkScores
	scoresFailed bool
	usageFailed  bool
	usage        taskLeaderboard
}

func (a *App) benchmarkJSON(req *http.Request, target any) error {
	ctx, cancel := context.WithTimeout(req.Context(), 12*time.Second)
	defer cancel()
	res, err := a.client.Do(req.WithContext(ctx))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(target)
}

func (a *App) fetchBenchmarks(ctx context.Context) (benchmarkScores, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	snapshot := benchmarkScores{
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
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s?page=%d", intelligenceURL, page), nil)
		if err != nil {
			return benchmarkScores{}, err
		}
		req.Header.Set("x-api-key", a.cfg.ArtificialAnalysisKey)
		if err := a.benchmarkJSON(req, &result); err != nil {
			return benchmarkScores{}, err
		}
		if page == 1 {
			snapshot.version = result.Version
			total = result.Pagination.Total
		}
		if result.Version <= 0 || result.Version != snapshot.version {
			return benchmarkScores{}, errors.New("inconsistent Intelligence Index version")
		}
		if result.Pagination.Page != page || result.Pagination.Total != total || total < page || result.Pagination.HasMore != (page < total) || len(result.Data) == 0 {
			return benchmarkScores{}, fmt.Errorf("incomplete model list at page %d", page)
		}
		for i := range result.Data {
			row := &result.Data[i]
			if row.ID == "" {
				continue
			}
			snapshot.byID[row.ID] = row
			indexBenchmark(snapshot.bySlug, row.Slug, row)
			indexBenchmarkModelName(snapshot.byName, row.Name, row)
		}
		if !result.Pagination.HasMore {
			snapshot.updatedAt = time.Now()
			return snapshot, nil
		}
	}
	return benchmarkScores{}, errors.New("too many model pages")
}

func indexBenchmarkModelName(index map[string]*benchmarkModel, name string, row *benchmarkModel) {
	indexBenchmark(index, benchmarkName(name), row)
	start := strings.LastIndex(name, " (")
	if start < 0 || !strings.HasSuffix(name, ")") {
		return
	}
	base := strings.TrimSpace(name[:start])
	qualifier := strings.SplitN(name[start+2:len(name)-1], ",", 2)[0]
	if effort := benchmarkEffort(qualifier); effort != "" {
		indexBenchmark(index, benchmarkName(base+" "+effort), row)
	}
}

func benchmarkEffort(value string) string {
	effort := benchmarkName(value)
	switch effort {
	case "none", "non-reasoning", "low", "medium", "high", "xhigh", "x-high", "max":
		return strings.ReplaceAll(effort, "x-high", "xhigh")
	default:
		return ""
	}
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

func (a *App) benchmarkWorker(ctx context.Context) {
	if a.cfg.ArtificialAnalysisKey == "" {
		return
	}
	timer := time.NewTimer(0)
	defer timer.Stop()
	var snapshot benchmarkSnapshot
	var nextScores, nextUsage time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		now := time.Now()
		if !now.Before(nextScores) {
			next, err := a.fetchBenchmarks(ctx)
			if ctx.Err() != nil {
				return
			}
			snapshot.scoresFailed = err != nil
			if err == nil {
				snapshot.benchmarkScores = next
				nextScores = time.Now().Add(benchmarkTTL)
			} else {
				log.Printf("Artificial Analysis model list: %v", err)
				nextScores = time.Now().Add(time.Hour)
			}
		}
		if !now.Before(nextUsage) {
			next, err := a.fetchTaskLeaderboard(ctx)
			if ctx.Err() != nil {
				return
			}
			snapshot.usageFailed = err != nil
			if err == nil {
				snapshot.usage = next
				nextUsage = time.Now().Add(benchmarkTTL)
			} else {
				log.Printf("Terminal-Bench task tokens: %v", err)
				nextUsage = time.Now().Add(time.Hour)
			}
		}
		a.mu.Lock()
		a.benchmarks = snapshot
		a.mu.Unlock()
		timer.Reset(min(time.Until(nextScores), time.Until(nextUsage)))
	}
}

func benchmarkName(name string) string {
	return strings.Join(strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}), "-")
}

func upstreamBenchmarkName(name string) string {
	return benchmarkName(strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(name), "__dev"), "__max"))
}

func (snapshot benchmarkScores) match(model Model, max bool, effort string, aliases map[string]string) *benchmarkModel {
	return snapshot.matchMapped(model, max, effort, aliases, "")
}

func (snapshot benchmarkScores) modelOptions() []benchmarkModelOption {
	options := []benchmarkModelOption{}
	for _, row := range snapshot.byID {
		if row == nil || row.Evaluations.Intelligence == nil {
			continue
		}
		options = append(options, benchmarkModelOption{ID: row.ID, Name: row.Name, Intelligence: *row.Evaluations.Intelligence})
	}
	sort.Slice(options, func(i, j int) bool { return strings.ToLower(options[i].Name) < strings.ToLower(options[j].Name) })
	return options
}

func (snapshot benchmarkScores) matchMapped(model Model, max bool, effort string, aliases map[string]string, mappedID string) *benchmarkModel {
	if mappedID != "" {
		if row := snapshot.byID[mappedID]; row != nil && row.Evaluations.Intelligence != nil {
			return row
		}
		return nil
	}
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
			effortCandidate := candidate
			if !strings.HasSuffix(effortCandidate, suffix) {
				effortCandidate += suffix
			}
			if row, found := snapshot.byName[effortCandidate]; found && row != nil {
				return row
			}
		}
		if row, found := snapshot.byName[candidate]; found {
			if row != nil {
				return row
			}
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
	TaskAgent     string  `json:"task_agent,omitempty"`
}

type benchmarkMatch struct {
	candidate efficiencyCandidate
	benchmark *benchmarkModel
	pricing   *ModelPricing
	taskAgent string
	taskModel string
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
		row := snapshot.matchMapped(model, max, effort, a.cfg.ArtificialAnalysisAliases, setting.BenchmarkID)
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
			taskAgent: setting.TaskAgentName,
			taskModel: setting.TaskModelName,
		})
	}
	return matches, enabled
}

func (a *App) efficiency(models []Model, settings map[string]ModelSetting, snapshot benchmarkSnapshot, now time.Time) M {
	if a.cfg.ArtificialAnalysisKey == "" {
		return M{"status": "unconfigured"}
	}
	failed := snapshot.scoresFailed || snapshot.usageFailed
	if snapshot.updatedAt.IsZero() || snapshot.usage == nil {
		if failed {
			return M{"status": "unavailable"}
		}
		return M{"status": "loading"}
	}
	status := "ready"
	if failed {
		status = "stale"
	}
	matches, enabled := a.benchmarkMatches(models, settings, snapshot)
	byAgent := map[string][]efficiencyCandidate{}
	manualCandidates := []efficiencyCandidate{}
	for _, match := range matches {
		rate, offPeak := match.pricing.rateAt(now)
		if rate == nil {
			continue
		}
		candidate := match.candidate
		candidate.Rate = *rate
		candidate.OffPeak = offPeak
		if match.taskAgent != "" {
			agent, ok := snapshot.usage[match.taskAgent]
			if !ok {
				continue
			}
			average := agent.tokensForMapped(match.benchmark, match.taskModel, candidate.Effort)
			if average <= 0 {
				continue
			}
			candidate.TaskAgent = agent.name
			candidate.TokensPerTask = average
			candidate.CostPerTask = candidate.Rate * average
			if !math.IsInf(candidate.CostPerTask, 0) {
				manualCandidates = append(manualCandidates, candidate)
			}
			continue
		}
		for name, agent := range snapshot.usage {
			average := agent.tokensForMapped(match.benchmark, match.taskModel, candidate.Effort)
			if average <= 0 {
				continue
			}
			candidate.TokensPerTask = average
			candidate.CostPerTask = candidate.Rate * average
			candidate.TaskAgent = agent.name
			if !math.IsInf(candidate.CostPerTask, 0) {
				byAgent[name] = append(byAgent[name], candidate)
			}
		}
	}
	var agent string
	autoCandidates := []efficiencyCandidate{}
	for name, group := range byAgent {
		if len(group) > len(autoCandidates) || (len(group) == len(autoCandidates) && name < agent) {
			agent, autoCandidates = name, group
		}
	}
	candidates := append(manualCandidates, autoCandidates...)
	agentLabel := snapshot.usage[agent].name
	if len(manualCandidates) > 0 {
		if len(autoCandidates) > 0 {
			agentLabel = "自动匹配与手动指定 Agent"
		} else {
			agentLabel = "手动指定 Agent"
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
		"agent": agentLabel, "candidates": candidates,
		"benchmark_options": snapshot.benchmarkScores.modelOptions(), "task_model_options": snapshot.usage.modelOptions()}
}
