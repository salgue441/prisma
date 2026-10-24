package masking

import (
	"encoding/json"
	"strings"
	"testing"

	"prisma/internal/config"
	"prisma/internal/telemetry"
)

func mask(t *testing.T, rules []config.MaskRule, in string) map[string]any {
	t.Helper()
	f, err := New("r", &config.Masking{Rules: rules, HashKey: "k"}, telemetry.NewMetrics())
	if err != nil {
		t.Fatal(err)
	}
	out, err := f.Mask([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSelectors(t *testing.T) {
	in := `{"a":{"ssn":"1","b":{"ssn":"2"}},"list":[{"ssn":"3","keep":1},{"ssn":"4"}],"big":12345678901234567890,"html":"<b>"}`
	v := mask(t, []config.MaskRule{{Selector: "$..ssn", Strategy: "redact"}}, in)
	s, _ := json.Marshal(v)
	if strings.Count(string(s), `"***"`) != 4 {
		t.Errorf("recursive redact missed values: %s", s)
	}

	v = mask(t, []config.MaskRule{{Selector: "$.list[0].ssn", Strategy: "drop"}}, in)
	first := v["list"].([]any)[0].(map[string]any)
	if _, ok := first["ssn"]; ok || first["keep"] == nil {
		t.Errorf("indexed drop: %v", first)
	}

	v = mask(t, []config.MaskRule{{Selector: "$.a.*", Strategy: "redact"}}, in)
	if v["a"].(map[string]any)["b"] != "***" {
		t.Errorf("wildcard on object: %v", v["a"])
	}
}

func TestMaskPreservesNumbersAndHTML(t *testing.T) {
	f, _ := New("r", &config.Masking{Rules: []config.MaskRule{{Selector: "$.x", Strategy: "redact"}}}, telemetry.NewMetrics())
	out, err := f.Mask([]byte(`{"big":12345678901234567890,"html":"<b>&"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "12345678901234567890") || !strings.Contains(string(out), "<b>&") {
		t.Errorf("number precision or HTML escaping changed: %s", out)
	}
}

func TestStrategiesAndDetectors(t *testing.T) {
	v := mask(t, []config.MaskRule{
		{Selector: "$.card", Strategy: "partial", KeepLast: 4},
		{Selector: "$.id", Strategy: "hash"},
		{Detector: "credit_card", Strategy: "partial", KeepLast: 4},
		{Detector: "ssn", Strategy: "redact"},
	}, `{"card":"4111111111111111","id":"user-1","text":"cc 4111 1111 1111 1111, ssn 123-45-6789, order 1234567890123"}`)
	if v["card"] != "************1111" {
		t.Errorf("partial: %v", v["card"])
	}
	if id := v["id"].(string); !strings.HasPrefix(id, "hmac:") || len(id) != 5+32 {
		t.Errorf("hash: %v", id)
	}
	text := v["text"].(string)
	if strings.Contains(text, "4111 1111 1111 1111") || strings.Contains(text, "123-45-6789") {
		t.Errorf("detectors missed PII: %q", text)
	}
	if !strings.Contains(text, "1234567890123") {
		t.Errorf("non-Luhn number was masked: %q", text)
	}
}

func TestInvalidSelectors(t *testing.T) {
	for _, s := range []string{"a.b", "$", "$.", "$.a[", "$.a[x]", "$..", "$a"} {
		if _, err := parseSelector(s); err == nil {
			t.Errorf("parseSelector(%q) should fail", s)
		}
	}
}
