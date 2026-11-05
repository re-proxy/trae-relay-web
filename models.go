package main

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

type Model struct {
	ID            string   `json:"id"`
	Name          string   `json:"display_name"`
	ModelName     string   `json:"model_name"`
	MaxModel      string   `json:"max_model_name,omitempty"`
	Function      string   `json:"function"`
	Provider      string   `json:"provider"`
	Source        int      `json:"config_source"`
	Max           bool     `json:"max"`
	Vision        bool     `json:"vision"`
	Efforts       []string `json:"efforts"`
	DefaultEffort string   `json:"default_effort"`
	Context       int64    `json:"context"`
	MaxContext    int64    `json:"max_context"`
}

func internalModel(id string) bool {
	s := strings.ToLower(id)
	for _, p := range []string{"custom_", "search_agent", "browser_use", "computer_use", "file_search", "explore_"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	if strings.HasSuffix(s, "-auto") {
		return true
	}
	for _, v := range []string{"doubao-for-auto", "glm-4.7", "glm-4.6", "minimax-m2", "minimax-m2.1", "kimi-k2-0905", "summary", "fast_apply", "fast_apply_new", "title_generation", "input_optimization", "context_selection"} {
		if s == v {
			return true
		}
	}
	return false
}
func parseCatalog(root M, function string) []Model {
	if d, ok := root["data"].(map[string]any); ok {
		root = d
	}
	out := []Model{}
	for _, entry := range list(root["config_info_list"]) {
		c := obj(entry)
		id := str(c, "config_name")
		if id == "" || internalModel(id) || boolean(c, "is_invisible_to_user", false) || !boolean(c, "config_switch", true) || boolean(obj(c["display_config"]), "is_custom_model", false) || number(c, "config_source", 1) != 1 {
			continue
		}
		for _, detail := range list(c["model_detail_list"]) {
			m := M{}
			for k, v := range c {
				m[k] = v
			}
			for k, v := range obj(detail) {
				m[k] = v
			}
			if number(m, "config_source", 1) != 1 {
				continue
			}
			model := Model{ID: id, Name: str(m, "display_model_name"), ModelName: str(m, "model_name"), Function: function, Provider: str(m, "provider"), Source: 1, Vision: true, Max: boolean(c, "max_mode", false) || boolean(obj(c["display_config"]), "max_mode", false) || boolean(m, "max_mode", false)}
			if f := str(m, "raw_chat_function"); f != "" {
				model.Function = f
			}
			if model.Name == "" {
				model.Name = str(m, "display_name")
			}
			if model.Name == "" {
				model.Name = id
			}
			if model.ModelName == "" {
				model.ModelName = id
			}
			if strings.HasSuffix(model.ModelName, "__max") || boolean(obj(detail), "max_mode", false) {
				model.MaxModel = model.ModelName
				model.Max = true
			}
			features := obj(m["features"])
			if raw, ok := m["features"].(string); ok {
				_ = json.Unmarshal([]byte(raw), &features)
			}
			seen, vision := false, false
			for _, v := range []M{m, features} {
				for _, k := range []string{"support_vision", "vision_support", "vision_enabled", "support_image", "support_image_input", "image_input_enabled", "multimodal", "is_multimodal", "support_multimodal", "vision"} {
					if b, ok := v[k]; ok {
						seen = true
						vision = vision || b == true || b == "true" || b == "1" || b == "yes"
					}
				}
				for _, key := range []string{"capabilities", "modalities", "tags", "features_list"} {
					for _, raw := range list(v[key]) {
						if s, ok := raw.(string); ok {
							s = strings.ToLower(s)
							if strings.Contains(s, "vision") || strings.Contains(s, "image") || strings.Contains(s, "multimodal") {
								seen = true
								vision = true
							}
						}
					}
				}
			}
			if seen {
				model.Vision = vision
			}
			rec := obj(m["reasoning_effort_config"])
			if boolean(rec, "support_thinking", false) {
				for _, eff := range list(rec["options"]) {
					if s, ok := eff.(string); ok {
						model.Efforts = appendUnique(model.Efforts, strings.ToLower(strings.TrimSpace(s)))
					}
				}
			}
			model.DefaultEffort = str(rec, "default_level")
			for _, eff := range list(m["reasoning_effort_options"]) {
				if s, ok := eff.(string); ok {
					model.Efforts = appendUnique(model.Efforts, s)
				}
			}
			if model.DefaultEffort == "" {
				model.DefaultEffort = str(m, "default_reasoning_effort")
			}
			if len(model.Efforts) > 0 {
				valid := false
				for _, v := range model.Efforts {
					if v == model.DefaultEffort {
						valid = true
					}
				}
				if !valid {
					model.DefaultEffort = model.Efforts[0]
				}
			}
			cw := obj(m["context_window_size"])
			model.Context = int64(number(cw, "default", 200000))
			mx := cw["max"]
			if n, ok := mx.(float64); ok {
				model.MaxContext = int64(n)
			}
			for _, n := range list(mx) {
				if f, ok := n.(float64); ok && int64(f) > model.MaxContext {
					model.MaxContext = int64(f)
				}
			}
			tokens := obj(m["context_window_tokens"])
			model.Context = int64(number(tokens, "dev", float64(model.Context)))
			model.MaxContext = int64(number(tokens, "max", float64(model.MaxContext)))
			cwData := obj(obj(features["context_windows"])["data"])
			model.Context = int64(number(cwData, "dev_context", float64(model.Context)))
			if model.MaxContext == 0 {
				model.MaxContext = int64(number(cwData, "max_context", 0))
			}
			for _, raw := range list(cwData["max_context_list"]) {
				if n, ok := raw.(float64); ok && int64(n) > model.MaxContext {
					model.MaxContext = int64(n)
				}
			}
			out = append(out, model)
		}
	}
	return out
}
func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
func mergeModels(all []Model) []Model {
	out := []Model{}
	for _, m := range all {
		found := false
		for i := range out {
			d := &out[i]
			if strings.EqualFold(d.ID, m.ID) {
				found = true
				d.Max = d.Max || m.Max
				d.Vision = d.Vision || m.Vision
				if d.MaxModel == "" {
					d.MaxModel = m.MaxModel
				}
				if strings.Contains(m.ModelName, "__dev") {
					d.ModelName = m.ModelName
					d.Function = m.Function
				}
				if m.MaxContext > d.MaxContext {
					d.MaxContext = m.MaxContext
				}
				if d.DefaultEffort == "" {
					d.DefaultEffort = m.DefaultEffort
				}
				for _, v := range m.Efforts {
					d.Efforts = appendUnique(d.Efforts, v)
				}
				break
			}
		}
		if !found {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (a *App) getModels(ctx context.Context, acc Account, force bool) ([]Model, error) {
	a.mu.Lock()
	cached := a.models[acc.ID]
	a.mu.Unlock()
	if !force && len(cached.Models) > 0 && time.Since(cached.At) < time.Hour {
		return cached.Models, nil
	}
	acc, e := a.fresh(ctx, acc, false)
	if e != nil {
		return nil, e
	}
	host := soloHost
	if acc.Scope == "saas" {
		host = enterpriseHost
	}
	all := []Model{}
	for _, function := range []string{"chat_v3", "solo_agent"} {
		root, e := a.post(ctx, host, "/api/ide/v1/get_detail_param", M{"function": function, "config_names": nil, "need_prompt": false, "current_config_info": nil, "poly_prompt": true, "mode_type": nil, "agent_type": nil}, acc)
		if e != nil {
			return nil, e
		}
		all = append(all, parseCatalog(root, function)...)
	}
	all = mergeModels(all)
	if len(all) == 0 {
		return nil, errors.New("empty_model_catalog")
	}
	a.mu.Lock()
	a.models[acc.ID] = cachedModels{all, time.Now()}
	a.mu.Unlock()
	return all, nil
}
func (a *App) catalogWorker(ctx context.Context) {
	delay := time.Duration(0)
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		delay = 5 * time.Minute
		if acc, ok := a.store.active(); ok {
			if _, e := a.getModels(ctx, acc, false); e == nil {
				delay = time.Hour
			}
		}
	}
}
