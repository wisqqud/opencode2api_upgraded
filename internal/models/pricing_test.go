package models

import "testing"

func TestModelNameFromOfficialMetadata(t *testing.T) {
	prices, err := decodeMetadata([]byte(`{
		"opencode": {"models": {
			"deepseek-v4.1-flash-free": {"name": "DeepSeek V4.1 Flash Free"},
			"mimo-v2.6-flash-free": {"name": "MiMo-V2.6-Flash Free"},
			"legacy-free": {"id": "legacy-free"}
		}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	catalog := Catalog("", nil)
	if got := catalog.ModelName("deepseek-v4.1-flash-free"); got != "" {
		t.Fatalf("name before metadata = %q", got)
	}
	catalog.SetPricingStore(&PricingRegistry{models: prices})
	for model, want := range map[string]string{
		"deepseek-v4.1-flash-free": "DeepSeek V4.1 Flash Free",
		"mimo-v2.6-flash-free":     "MiMo-V2.6-Flash Free",
		"legacy-free":              "",
		"unknown-free":             "",
	} {
		if got := catalog.ModelName(model); got != want {
			t.Errorf("%s name = %q, want %q", model, got, want)
		}
	}
}
