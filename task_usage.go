package main

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

const requestyBenchmarkURL = "https://www.requesty.ai/coding-benchmark"

var (
	requestyEscapedRows = regexp.MustCompile(`\\"rows\\":(\[.*?\])`)
	requestyPlainRows   = regexp.MustCompile(`"rows":(\[.*?\])`)
)

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
	label     string
}

type taskModelOption struct {
	AgentID string   `json:"agent_id"`
	Agent   string   `json:"agent"`
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	Efforts []string `json:"efforts"`
}

func (a *App) fetchTaskLeaderboard(ctx context.Context) (taskLeaderboard, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestyBenchmarkURL, nil)
	if err != nil {
		return nil, err
	}
	var page struct {
		Rows []struct {
			Model        string  `json:"model"`
			Tasks        int     `json:"tasks"`
			TotalTokensM float64 `json:"totalTokensM"`
		} `json:"rows"`
	}
	body, err := a.benchmarkBody(req)
	if err != nil {
		return nil, err
	}
	if err := decodeRequestyRows(body, &page); err != nil {
		return nil, err
	}
	if len(page.Rows) == 0 {
		return nil, errors.New("no Requesty coding benchmark token rows")
	}
	const agentName = "Requesty Coding Agent"
	const agentKey = "requesty-coding-agent"
	agent := taskAgent{name: agentName, usage: map[taskModel]taskMeasurement{}}
	for _, row := range page.Rows {
		if row.Model == "" || row.Tasks < 129 || row.TotalTokensM <= 0 {
			continue
		}
		modelKey := taskModel{taskModelName(row.Model), "default"}
		if modelKey.name == "" {
			continue
		}
		agent.usage[modelKey] = taskMeasurement{average: row.TotalTokensM * 1_000_000, label: row.Model}
	}
	if len(agent.usage) == 0 {
		return nil, errors.New("no complete Requesty coding benchmark token rows")
	}
	return taskLeaderboard{agentKey: agent}, nil
}

func decodeRequestyRows(body []byte, target any) error {
	page := html.UnescapeString(string(body))
	matches := requestyEscapedRows.FindSubmatch([]byte(page))
	if len(matches) == 0 {
		matches = requestyPlainRows.FindSubmatch([]byte(page))
		if len(matches) == 0 {
			return errors.New("Requesty coding benchmark rows not found")
		}
		return json.Unmarshal(matches[1], target)
	}
	decoded := string(matches[1])
	for attempts := 0; attempts < 3 && !json.Valid([]byte(decoded)); attempts++ {
		decoded = strings.ReplaceAll(decoded, `\"`, `"`)
	}
	if !json.Valid([]byte(decoded)) {
		return errors.New("invalid escaped Requesty coding benchmark rows")
	}
	return json.Unmarshal([]byte(decoded), target)
}

func (data taskLeaderboard) modelOptions() []taskModelOption {
	type optionState struct {
		agent   string
		label   string
		efforts map[string]bool
	}
	options := map[string]*optionState{}
	for agentName, agent := range data {
		for key, measurement := range agent.usage {
			optionKey := agentName + "\x00" + key.name
			option := options[optionKey]
			if option == nil {
				option = &optionState{agent: agent.name, label: measurement.label, efforts: map[string]bool{}}
				options[optionKey] = option
			}
			if measurement.label != "" {
				option.label = measurement.label
			}
			option.efforts[key.effort] = true
		}
	}
	result := make([]taskModelOption, 0, len(options))
	for key, option := range options {
		efforts := make([]string, 0, len(option.efforts))
		for effort := range option.efforts {
			efforts = append(efforts, effort)
		}
		parts := strings.SplitN(key, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		result = append(result, taskModelOption{AgentID: parts[0], Agent: option.agent, Name: parts[1], Label: option.label, Efforts: efforts})
	}
	sort.Slice(result, func(i, j int) bool {
		left, right := strings.ToLower(result[i].Agent+" "+result[i].Label), strings.ToLower(result[j].Agent+" "+result[j].Label)
		return left < right
	})
	for i := range result {
		sort.Strings(result[i].Efforts)
	}
	return result
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
	return agent.tokensForMapped(model, "", effort)
}

func (agent taskAgent) tokensForMapped(model *benchmarkModel, mappedName, effort string) float64 {
	effort = benchmarkName(effort)
	if effort == "" {
		effort = "none"
	}
	if mappedName != "" {
		if measurement, ok := agent.usage[taskModel{taskModelName(mappedName), effort}]; ok {
			return measurement.average
		}
		if measurement, ok := agent.usage[taskModel{taskModelName(mappedName), "default"}]; ok {
			return measurement.average
		}
		return 0
	}
	name := strings.TrimSpace(model.Name)
	if start := strings.LastIndex(name, " ("); start >= 0 && strings.HasSuffix(name, ")") {
		suffix := strings.SplitN(name[start+2:len(name)-1], ",", 2)[0]
		if benchmarkEffort(suffix) == effort || (effort == "none" && benchmarkName(suffix) == "non-reasoning") {
			name = name[:start]
		}
	}
	for _, value := range []string{name, model.Slug} {
		if measurement, ok := agent.usage[taskModel{taskModelName(value), effort}]; ok {
			return measurement.average
		}
		if measurement, ok := agent.usage[taskModel{taskModelName(value), "default"}]; ok {
			return measurement.average
		}
	}
	return 0
}
