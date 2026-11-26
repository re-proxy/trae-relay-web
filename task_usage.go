package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const terminalBenchURL = "https://api.harborframework.com/functions/v1/leaderboard-read"

type taskLeaderboard map[string]taskAgent

type taskAgent struct {
	name  string
	usage map[taskModel]taskMeasurement
}

type taskModel struct {
	name   string
	effort string
}

type taskMeasurement struct {
	average   float64
	updatedAt time.Time
}

func (a *App) fetchTaskLeaderboard(ctx context.Context) (taskLeaderboard, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	agents := taskLeaderboard{}
	total := 0
	for page := 1; page <= 50; page++ {
		body := fmt.Sprintf(`{"package":"terminal-bench/terminal-bench","name":"4-1-0","page":%d,"page_size":100}`, page)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, terminalBenchURL, strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		var result struct {
			Pagination struct {
				Page  int `json:"page"`
				Total int `json:"total_pages"`
			} `json:"pagination"`
			Rows []taskLeaderboardRow `json:"rows"`
		}
		if err := a.benchmarkJSON(req, &result); err != nil {
			return nil, err
		}
		if page == 1 {
			total = result.Pagination.Total
		}
		if len(result.Rows) == 0 || result.Pagination.Page != page || result.Pagination.Total != total || total < page {
			return nil, fmt.Errorf("incomplete Terminal-Bench leaderboard at page %d", page)
		}
		for _, row := range result.Rows {
			agents.add(row)
		}
		if page == total {
			if len(agents) == 0 {
				return nil, errors.New("no Terminal-Bench task token totals")
			}
			return agents, nil
		}
	}
	return nil, errors.New("too many Terminal-Bench leaderboard pages")
}

type taskLeaderboardRow struct {
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
	Metadata  struct {
		Agent struct {
			Label string `json:"label"`
		} `json:"agent_display"`
		Model struct {
			Label string `json:"label"`
		} `json:"model_display"`
		Effort string `json:"reasoning_effort"`
	} `json:"metadata"`
	Metrics struct {
		Trials int   `json:"n_trials"`
		Tokens int64 `json:"total_tokens"`
	} `json:"metrics"`
}

func (agents taskLeaderboard) add(row taskLeaderboardRow) {
	label := strings.TrimSpace(row.Metadata.Agent.Label)
	agentKey := benchmarkName(label)
	key := taskModel{taskModelName(row.Metadata.Model.Label), benchmarkName(row.Metadata.Effort)}
	if row.Status != "display" || agentKey == "" || key.name == "" || key.effort == "" || row.Metrics.Trials <= 0 || row.Metrics.Tokens <= 0 {
		return
	}
	agent, found := agents[agentKey]
	if !found {
		agent = taskAgent{name: label, usage: map[taskModel]taskMeasurement{}}
		agents[agentKey] = agent
	}
	if existing, found := agent.usage[key]; found && !row.UpdatedAt.After(existing.updatedAt) {
		return
	}
	agent.usage[key] = taskMeasurement{
		average:   float64(row.Metrics.Tokens) / float64(row.Metrics.Trials),
		updatedAt: row.UpdatedAt,
	}
}

func taskModelName(name string) string {
	name = benchmarkName(name)
	for _, family := range []string{"claude-opus-", "claude-sonnet-", "claude-haiku-", "claude-fable-"} {
		if strings.HasPrefix(name, family) {
			return strings.TrimPrefix(name, "claude-")
		}
	}
	return name
}

func (agent taskAgent) tokensFor(model *benchmarkModel, effort string) float64 {
	effort = benchmarkName(effort)
	if effort == "" {
		effort = "none"
	}
	name := strings.TrimSpace(model.Name)
	if start := strings.LastIndex(name, " ("); start >= 0 && strings.HasSuffix(name, ")") {
		suffix := benchmarkName(name[start+2 : len(name)-1])
		if suffix == effort || (effort == "none" && suffix == "non-reasoning") {
			name = name[:start]
		}
	}
	for _, value := range []string{name, model.Slug} {
		if measurement, ok := agent.usage[taskModel{taskModelName(value), effort}]; ok {
			return measurement.average
		}
	}
	return 0
}
