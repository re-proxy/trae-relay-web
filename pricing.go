package main

import (
	"encoding/json"
	"math"
)

// Values are upstream catalogue multipliers, not currency/token prices or an
// assertion that this account qualifies for a membership/activity discount.
type ModelPricing struct {
	Catalog        *float64     `json:"catalog_rate,omitempty"`
	Standard       *float64     `json:"standard_rate,omitempty"`
	Member         *float64     `json:"member_rate,omitempty"`
	Activity       *float64     `json:"activity_rate,omitempty"`
	ActivityMember *float64     `json:"activity_member_rate,omitempty"`
	ActivityType   string       `json:"activity_type,omitempty"`
	Windows        []RateWindow `json:"off_peak_windows,omitempty"`
}
type RateWindow struct {
	Weekdays []int `json:"weekdays"`
	Start    int   `json:"start_minute"`
	End      int   `json:"end_minute"`
}

func pricingObject(v any) M {
	if s, ok := v.(string); ok {
		var m M
		if json.Unmarshal([]byte(s), &m) == nil {
			return obj(m)
		}
		return M{}
	}
	return obj(v)
}
func priceNumber(m M, k string) *float64 {
	n := number(m, k, math.NaN())
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return nil
	}
	return &n
}
func parseModelPricing(layers ...M) *ModelPricing {
	p := &ModelPricing{}
	var before *float64
	for _, layer := range layers {
		config := pricingObject(layer["display_contact_config"])
		consumption := obj(config["consumption_rate"])
		data := obj(consumption["data"])
		if len(data) == 0 {
			data = consumption
		}
		if n := priceNumber(data, "rate"); n != nil {
			p.Catalog = n
		}
		discount := obj(obj(config["discount"])["data"])
		if n := priceNumber(discount, "consumption_rate"); n != nil {
			p.Member = n
		}
		activity := obj(obj(config["activity_discount"])["data"])
		current := obj(activity["current"])
		if n := priceNumber(current, "consumption_rate"); n != nil {
			p.Activity = n
		}
		if n := priceNumber(current, "before_consumption_rate"); n != nil {
			before = n
		}
		if typ := str(current, "discount_type"); typ != "" {
			p.ActivityType = typ
		}
		if n := priceNumber(obj(activity["member"]), "after_consumption_rate"); n != nil {
			p.ActivityMember = n
		}
		if raw, ok := obj(activity["off_peak"])["time_windows"]; ok {
			p.Windows = nil
			for _, v := range list(raw) {
				w := obj(v)
				start, end := number(w, "start_minute", -1), number(w, "end_minute", -1)
				if start < 0 || end > 1440 || end <= start || start != math.Trunc(start) || end != math.Trunc(end) {
					continue
				}
				days := []int{}
				valid := true
				for _, day := range list(w["weekdays"]) {
					n, ok := day.(float64)
					if !ok || n < 1 || n > 7 || n != math.Trunc(n) {
						valid = false
						break
					}
					days = append(days, int(n))
				}
				if valid {
					p.Windows = append(p.Windows, RateWindow{days, int(start), int(end)})
				}
			}
		}
	}
	p.Standard = p.Catalog
	if before != nil {
		p.Standard = before
	}
	if p.Standard == nil && p.Member == nil && p.Activity == nil && p.ActivityMember == nil {
		return nil
	}
	return p
}
