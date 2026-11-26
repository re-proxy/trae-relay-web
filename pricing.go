package main

import (
	"encoding/json"
	"math"
	"slices"
	"time"
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

var pricingTimezone = time.FixedZone("CST", 8*60*60)

func (p *ModelPricing) isOffPeak() bool {
	return p.ActivityType == "off_peak" || (p.ActivityType == "" && len(p.Windows) > 0)
}

func (p *ModelPricing) rateAt(now time.Time) (*float64, bool) {
	if p.isOffPeak() && p.Activity != nil && (p.Standard == nil || *p.Activity <= *p.Standard) {
		beijing := now.In(pricingTimezone)
		weekday := (int(beijing.Weekday())+6)%7 + 1
		minute := beijing.Hour()*60 + beijing.Minute()
		for _, window := range p.Windows {
			if window.includes(weekday, minute) {
				return p.Activity, true
			}
		}
	}
	return p.Standard, false
}

func (w RateWindow) includes(weekday, minute int) bool {
	if w.Start <= w.End {
		if minute < w.Start || minute >= w.End {
			return false
		}
	} else if minute < w.End {
		// The after-midnight segment belongs to the previous day's window.
		weekday = (weekday+5)%7 + 1
	} else if minute < w.Start {
		return false
	}
	return len(w.Weekdays) == 0 || slices.Contains(w.Weekdays, weekday)
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
		catalog := priceNumber(data, "rate")
		discount := obj(obj(config["discount"])["data"])
		if n := priceNumber(discount, "consumption_rate"); n != nil {
			p.Member = n
		}
		activity := obj(obj(config["activity_discount"])["data"])
		current := obj(activity["current"])
		activityRate := priceNumber(current, "consumption_rate")
		regularRate := priceNumber(current, "before_consumption_rate")
		typ := str(current, "discount_type")
		changedProfile := p.isOffPeak() && ((catalog != nil && p.Catalog != nil && *catalog != *p.Catalog) ||
			(regularRate != nil && before != nil && *regularRate != *before))
		if changedProfile || (p.ActivityType != "" && typ != "" && p.ActivityType != typ) {
			// A differently priced detail may share the schedule without sharing its rates.
			p.Activity = nil
			p.ActivityMember = nil
			before = nil
		}
		if catalog != nil {
			p.Catalog = catalog
		}
		if activityRate != nil {
			p.Activity = activityRate
		}
		if regularRate != nil {
			before = regularRate
		}
		if typ != "" {
			if p.ActivityType != typ {
				p.Windows = nil
			}
			p.ActivityType = typ
		}
		if n := priceNumber(obj(activity["member"]), "after_consumption_rate"); n != nil {
			p.ActivityMember = n
		}
		if raw, ok := obj(activity["off_peak"])["time_windows"]; ok {
			if p.ActivityType == "" {
				p.ActivityType = "off_peak"
			}
			p.Windows = nil
			for _, v := range list(raw) {
				w := obj(v)
				start, end := number(w, "start_minute", -1), number(w, "end_minute", -1)
				if start < 0 || start >= 1440 || end < 0 || end > 1440 || start == end || start != math.Trunc(start) || end != math.Trunc(end) {
					continue
				}
				days := []int{}
				valid := true
				if rawDays, exists := w["weekdays"]; exists {
					if _, ok := rawDays.([]any); !ok {
						continue
					}
				}
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
	p.Standard = before
	if p.Standard == nil && !p.isOffPeak() {
		p.Standard = p.Catalog
	}
	if p.Standard == nil && p.Catalog == nil && p.Member == nil && p.Activity == nil && p.ActivityMember == nil {
		return nil
	}
	return p
}
