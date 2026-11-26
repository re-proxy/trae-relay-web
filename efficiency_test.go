package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestModelEfficiencyUsesTaskTokensAndTraeRates(t *testing.T) {
	a := testAppConfig(t, Config{ArtificialAnalysisKey: "benchmark-test-key"})
	seed(t, a)
	lowRate, highRate, promoRate := 1.0, 2.0, 0.05
	a.models["acc"] = cachedModels{[]Model{
		{ID: "verbose", Name: "Verbose", ModelName: "verbose__dev", Pricing: &ModelPricing{Standard: &lowRate}},
		{ID: "compact", Name: "Compact", ModelName: "compact__dev", Max: true, MaxModel: "compact__max",
			MaxPricing: &ModelPricing{Standard: &highRate, Catalog: &promoRate}, Efforts: []string{"high", "low"}, DefaultEffort: "low"},
		{ID: "missing", Name: "Missing", ModelName: "missing__dev", Pricing: &ModelPricing{Standard: &lowRate}},
		{ID: "disabled", Name: "Disabled", ModelName: "verbose__dev", Pricing: &ModelPricing{Standard: &promoRate}},
	}, time.Now()}
	a.store.state.Models["compact"] = ModelSetting{Max: true, Effort: "high"}
	a.store.state.Models["disabled"] = ModelSetting{Disabled: true}
	fixture(t, a, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/coding-benchmark" {
			if r.Method != "GET" || r.Header.Get("x-api-key") != "" || r.Header.Get("Authorization") != "" {
				t.Errorf("unexpected public Requesty request: %s %s", r.Method, r.URL)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`<script>"rows":[{"model":"verbose","tasks":129,"totalTokensM":0.003},{"model":"compact","tasks":129,"totalTokensM":0.001},{"model":"partial","tasks":12,"totalTokensM":0.0002}]</script>`))
			return
		}
		if r.Method != "GET" || r.Header.Get("x-api-key") != "benchmark-test-key" {
			t.Errorf("unexpected benchmark request: %s %s", r.Method, r.URL)
		}
		switch r.URL.RequestURI() {
		case "/api/v2/language/models/free?page=1":
			_, _ = w.Write([]byte(`{"intelligence_index_version":5,"pagination":{"page":1,"total_pages":2,"has_more":true},"data":[
				{"id":"v","name":"Verbose","slug":"verbose","evaluations":{"artificial_analysis_intelligence_index":41.2}},
				{"id":"c-low","name":"Compact (Low)","slug":"compact-low","evaluations":{"artificial_analysis_intelligence_index":12}}
			]}`))
		case "/api/v2/language/models/free?page=2":
			_, _ = w.Write([]byte(`{"intelligence_index_version":5,"pagination":{"page":2,"total_pages":2,"has_more":false},"data":[
				{"id":"c-high","name":"Compact (High, Default Fallback)","slug":"compact-high","evaluations":{"artificial_analysis_intelligence_index":33.5}},
				{"id":"m","name":"Missing","slug":"missing","evaluations":{"artificial_analysis_intelligence_index":20}}
			]}`))
		default:
			t.Errorf("unexpected benchmark endpoint: %s", r.URL)
			w.WriteHeader(http.StatusNotFound)
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.benchmarkWorker(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	cookie := adminCookie(t, a)
	var state struct {
		Efficiency struct {
			Status     string
			Enabled    int
			Agent      string
			Candidates []efficiencyCandidate
		}
	}
	for deadline := time.Now().Add(3 * time.Second); ; {
		w := request(a, "GET", "/admin/state", nil, "", cookie)
		if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "benchmark-test-key") {
			t.Fatalf("unexpected admin state response: %d", w.Code)
		}
		if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		if state.Efficiency.Status == "ready" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("benchmark data never loaded: %+v", state.Efficiency)
		}
		time.Sleep(time.Millisecond)
	}
	efficiency := state.Efficiency
	if efficiency.Enabled != 3 || efficiency.Agent != "Requesty Coding Agent" || len(efficiency.Candidates) != 2 {
		t.Fatalf("unexpected benchmark coverage: %+v", efficiency)
	}
	winner, runnerUp := efficiency.Candidates[0], efficiency.Candidates[1]
	if winner.ID != "compact" || !winner.Max || winner.Effort != "high" || winner.Intelligence != 33.5 ||
		winner.Rate != 2 || winner.TokensPerTask != 1000 || winner.CostPerTask != 2000 ||
		runnerUp.ID != "verbose" || runnerUp.CostPerTask != 3000 {
		t.Fatalf("unexpected cost ordering: %+v", efficiency.Candidates)
	}
}

func TestModelEfficiencyReordersAtOffPeakBoundary(t *testing.T) {
	a := &App{cfg: Config{ArtificialAnalysisKey: "benchmark-test-key"}}
	score, rate, busyRate, quietRate := 35.0, 0.1, 0.12, 0.06
	models := []Model{
		{ID: "fixed", Name: "Fixed", ModelName: "fixed__dev", Pricing: &ModelPricing{Standard: &rate}},
		{ID: "deepseek", Name: "DeepSeek", ModelName: "deepseek__dev", Pricing: &ModelPricing{
			Standard: &busyRate, Activity: &quietRate, ActivityType: "off_peak",
			Windows: []RateWindow{{Start: 720, End: 840}},
		}},
	}
	snapshot := benchmarkSnapshot{
		benchmarkScores: benchmarkScores{version: 5, updatedAt: time.Now(), byName: map[string]*benchmarkModel{}},
		usage:           taskLeaderboard{},
	}
	usage := map[taskModel]taskMeasurement{}
	for _, model := range models {
		row := &benchmarkModel{ID: model.ID}
		row.Name = model.Name
		row.Evaluations.Intelligence = &score
		snapshot.byName[model.ID] = row
		usage[taskModel{model.ID, "none"}] = taskMeasurement{average: 1000}
	}
	snapshot.usage["fixture"] = taskAgent{name: "Fixture", usage: usage}
	offPeakStart := time.Date(2026, 11, 26, 4, 0, 0, 0, time.UTC)
	before := a.efficiency(models, nil, snapshot, offPeakStart.Add(-time.Second))["candidates"].([]efficiencyCandidate)
	after := a.efficiency(models, nil, snapshot, offPeakStart)["candidates"].([]efficiencyCandidate)
	if len(before) != 2 || before[0].ID != "fixed" || before[1].CostPerTask != 120 || before[1].OffPeak {
		t.Fatalf("wrong ranking before the off-peak window: %+v", before)
	}
	if len(after) != 2 || after[0].ID != "deepseek" || after[0].CostPerTask != 60 || !after[0].OffPeak {
		t.Fatalf("wrong ranking in the off-peak window: %+v", after)
	}
}

func TestBenchmarkMatchFallsBackToGenericScoreAfterEffortSpecific(t *testing.T) {
	generic := &benchmarkModel{ID: "generic"}
	exact := &benchmarkModel{ID: "high"}
	snapshot := benchmarkScores{byName: map[string]*benchmarkModel{
		"model":      generic,
		"model-high": exact,
	}}
	model := Model{ID: "model", ModelName: "model__dev"}

	if got := snapshot.match(model, false, "high", nil); got != exact {
		t.Fatalf("expected effort-specific score, got %+v", got)
	}
	delete(snapshot.byName, "model-high")
	if got := snapshot.match(model, false, "high", nil); got != generic {
		t.Fatalf("expected generic score fallback, got %+v", got)
	}
}

func TestBenchmarkNamesIgnoreDefaultFallbackQualifier(t *testing.T) {
	row := &benchmarkModel{ID: "fable-max", Name: "Claude Fable 5.2 (Max, Default Fallback)"}
	index := map[string]*benchmarkModel{}
	indexBenchmarkModelName(index, row.Name, row)
	snapshot := benchmarkScores{byName: index}
	model := Model{ID: "fable", ModelName: "claude-fable-5-2__dev"}
	if got := snapshot.match(model, false, "max", nil); got != row {
		t.Fatalf("expected AA Max row despite fallback qualifier, got %+v", got)
	}
	agent := taskAgent{usage: map[taskModel]taskMeasurement{
		{"fable-5-2", "max"}: {average: 1234},
	}}
	if got := agent.tokensFor(row, "max"); got != 1234 {
		t.Fatalf("expected task token row after stripping fallback qualifier, got %v", got)
	}
}

func TestEfficiencyUsesSavedManualMappings(t *testing.T) {
	a := &App{cfg: Config{ArtificialAnalysisKey: "benchmark-test-key"}}
	rate, intelligence := 0.2, 48.0
	model := Model{ID: "upstream-model", Name: "Upstream Display", ModelName: "opaque-model__dev", Pricing: &ModelPricing{Standard: &rate}, Efforts: []string{"high"}, DefaultEffort: "high"}
	benchmark := &benchmarkModel{ID: "aa-id", Name: "Unrelated AA Name"}
	benchmark.Evaluations.Intelligence = &intelligence
	snapshot := benchmarkSnapshot{
		benchmarkScores: benchmarkScores{version: 5, updatedAt: time.Now(), byID: map[string]*benchmarkModel{"aa-id": benchmark}},
		usage: taskLeaderboard{"agent": {name: "Agent", usage: map[taskModel]taskMeasurement{
			{"terminal-bench-name", "high"}: {average: 1234},
		}}},
	}
	settings := map[string]ModelSetting{"upstream-model": {Effort: "high", BenchmarkID: "aa-id", TaskAgentName: "agent", TaskModelName: "terminal-bench-name"}}
	result := a.efficiency([]Model{model}, settings, snapshot, time.Now())
	candidates := result["candidates"].([]efficiencyCandidate)
	if len(candidates) != 1 || candidates[0].Intelligence != intelligence || candidates[0].TokensPerTask != 1234 || candidates[0].CostPerTask != 246.8 || candidates[0].TaskAgent != "Agent" {
		t.Fatalf("saved manual mappings were not applied: %+v", result)
	}
	if len(result["benchmark_options"].([]benchmarkModelOption)) != 1 || len(result["task_model_options"].([]taskModelOption)) != 1 {
		t.Fatalf("mapping choices missing from state: %+v", result)
	}
}
