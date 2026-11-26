package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestPricingProfilesAndDiscounts(t *testing.T) {
	var root M
	raw := `{"config_info_list":[{"config_name":"priced","display_contact_config":"{\"consumption_rate\":{\"data\":{\"rate\":0.08}},\"discount\":{\"data\":{\"consumption_rate\":0.4}},\"activity_discount\":{\"data\":{\"current\":{\"consumption_rate\":0.08,\"before_consumption_rate\":0.8,\"discount_type\":\"off_peak\"},\"member\":{\"after_consumption_rate\":0.04},\"off_peak\":{\"time_windows\":[{\"weekdays\":[1,2,3,4,5],\"start_minute\":0,\"end_minute\":480}]}}}}","model_detail_list":[{"model_name":"priced__max","max_mode":true,"display_contact_config":{"consumption_rate":{"rate":2},"activity_discount":{"data":{"current":{"before_consumption_rate":4}}}}},{"model_name":"priced__dev"}]}]}`
	if e := json.Unmarshal([]byte(raw), &root); e != nil {
		t.Fatal(e)
	}
	models := mergeModels(parseCatalog(root, "chat_v3"))
	if len(models) != 1 {
		t.Fatal(models)
	}
	m := models[0]
	p := m.Pricing
	if m.ModelName != "priced__dev" || p == nil || p.Standard == nil || *p.Standard != 0.8 || *p.Catalog != 0.08 || *p.Member != 0.4 || *p.Activity != 0.08 || *p.ActivityMember != 0.04 || p.ActivityType != "off_peak" || len(p.Windows) != 1 {
		t.Fatalf("bad standard pricing: %+v", p)
	}
	if m.MaxPricing == nil || *m.MaxPricing.Catalog != 2 || *m.MaxPricing.Standard != 4 {
		t.Fatalf("bad Max pricing: %+v", m.MaxPricing)
	}
	thursdayOffPeak := time.Date(2026, 11, 26, 4, 0, 0, 0, pricingTimezone)
	if rate, offPeak := m.MaxPricing.rateAt(thursdayOffPeak); rate == nil || *rate != 4 || offPeak {
		t.Fatal("standard profile's off-peak rate leaked to Max")
	}
	b, _ := json.Marshal(m)
	var out M
	_ = json.Unmarshal(b, &out)
	if number(obj(out["pricing"]), "standard_rate", -1) != 0.8 || number(obj(out["max_pricing"]), "standard_rate", -1) != 4 {
		t.Fatal(string(b))
	}
}
func TestPricingUnknownZeroAndMalformed(t *testing.T) {
	for _, v := range []any{nil, "not JSON", M{}, M{"consumption_rate": M{"rate": -1}}, M{"consumption_rate": M{"rate": "NaN"}}, M{"consumption_rate": M{"rate": "Inf"}}} {
		if p := parseModelPricing(M{"display_contact_config": v}); p != nil {
			t.Fatalf("invalid rate became known: %+v", p)
		}
	}
	p := parseModelPricing(M{"display_contact_config": M{"consumption_rate": M{"data": M{"rate": 0}}}})
	if p == nil || p.Standard == nil || *p.Standard != 0 {
		t.Fatal("valid zero lost")
	}
	p = parseModelPricing(M{"display_contact_config": M{"discount": M{"data": M{"consumption_rate": 0.5}}}})
	if p == nil || p.Standard != nil || p.Member == nil {
		t.Fatal("member rate presented as standard")
	}
}
func TestPricingDetailOverrideAndMerge(t *testing.T) {
	var root M
	_ = json.Unmarshal([]byte(`{"config_info_list":[{"config_name":"m","display_contact_config":{"consumption_rate":{"rate":1}},"model_detail_list":[{"model_name":"m__dev","display_contact_config":{"consumption_rate":{"rate":0.25}}}]}]}`), &root)
	models := mergeModels(append(parseCatalog(root, "chat_v3"), Model{ID: "m", Max: true, MaxPricing: &ModelPricing{}}))
	if len(models) != 1 || *models[0].Pricing.Standard != 0.25 || models[0].MaxPricing == nil {
		t.Fatal(models)
	}
	_ = json.Unmarshal([]byte(`{"config_info_list":[{"config_name":"max-only","model_detail_list":[{"model_name":"only__max","max_mode":true,"display_contact_config":{"consumption_rate":{"rate":2}}}]}]}`), &root)
	models = parseCatalog(root, "chat_v3")
	if len(models) != 1 || models[0].Pricing != nil || models[0].MaxPricing == nil {
		t.Fatal("Max rate leaked to standard profile")
	}
}

func TestOffPeakPricingUsesBeijingWindows(t *testing.T) {
	var config M
	if err := json.Unmarshal([]byte(`{
		"consumption_rate":{"data":{"rate":0.06}},
		"activity_discount":{"data":{
			"current":{"consumption_rate":0.06,"before_consumption_rate":0.12,"discount_type":"off_peak"},
			"member":{"after_consumption_rate":0.03},
			"off_peak":{"time_windows":[
				{"weekdays":[1,2,3,4,5],"start_minute":0,"end_minute":540},
				{"weekdays":[1,2,3,4,5],"start_minute":720,"end_minute":840},
				{"weekdays":[1,2,3,4,5],"start_minute":1080,"end_minute":1440},
				{"weekdays":[6,7],"start_minute":0,"end_minute":1440},
				{"weekdays":"invalid","start_minute":0,"end_minute":1440}
			]}
		}}
	}`), &config); err != nil {
		t.Fatal(err)
	}
	price := parseModelPricing(M{"display_contact_config": config})
	for _, test := range []struct {
		utc     string
		offPeak bool
	}{
		{"2026-11-26T00:59:00Z", true},  // Thursday 08:59
		{"2026-11-26T01:00:00Z", false}, // Thursday 09:00
		{"2026-11-26T04:00:00Z", true},  // Thursday 12:00
		{"2026-11-26T06:00:00Z", false}, // Thursday 14:00
		{"2026-11-26T10:00:00Z", true},  // Thursday 18:00
		{"2026-11-28T02:00:00Z", true},  // Saturday 10:00
	} {
		now, err := time.Parse(time.RFC3339, test.utc)
		if err != nil {
			t.Fatal(err)
		}
		want := 0.12
		if test.offPeak {
			want = 0.06
		}
		if rate, offPeak := price.rateAt(now); rate == nil || *rate != want || offPeak != test.offPeak {
			t.Errorf("at %s expected rate %v (off-peak %t), got %v (off-peak %t)", test.utc, want, test.offPeak, rate, offPeak)
		}
	}
	w := RateWindow{Weekdays: []int{7}, Start: 1320, End: 480}
	if !w.includes(7, 1320) || !w.includes(1, 479) || w.includes(1, 480) || w.includes(2, 479) {
		t.Fatal("overnight window did not carry Sunday's eligibility into Monday morning")
	}
	delete(obj(obj(obj(config["activity_discount"])["data"])["current"]), "before_consumption_rate")
	price = parseModelPricing(M{"display_contact_config": config})
	noonPeak := time.Date(2026, 11, 26, 10, 0, 0, 0, pricingTimezone)
	if rate, offPeak := price.rateAt(noonPeak); rate != nil || offPeak {
		t.Fatal("discounted catalog rate was treated as a known busy rate")
	}
}
