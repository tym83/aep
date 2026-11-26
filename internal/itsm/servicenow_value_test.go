package itsm

import (
	"encoding/json"
	"testing"
)

// Shapes observed on a live instance: Table API strings and Change Management
// API numbers, plain or wrapped in value/display_value objects.
func TestServiceNowValueShapes(t *testing.T) {
	for raw, want := range map[string]string{
		`"-5"`: "-5",
		`-5`:   "-5",
		`{"value":"-5","display_value":"New"}`: "-5",
		`{"value":-5,"display_value":"New"}`:   "-5",
		`{"value":null,"display_value":""}`:    "",
		`{"display_value":"New"}`:              "",
	} {
		var v snValue
		if err := json.Unmarshal([]byte(raw), &v); err != nil || string(v) != want {
			t.Fatalf("%s: %q, %v", raw, v, err)
		}
	}
	var r snRecord
	if err := json.Unmarshal([]byte(`{"result":{"sys_id":{"value":"abc"},"state":{"value":-5}}}`), &struct {
		Result *snRecord `json:"result"`
	}{&r}); err != nil || r.State != "-5" || r.ID != "abc" {
		t.Fatalf("%+v %v", r, err)
	}
	var bad snValue
	if json.Unmarshal([]byte(`{"value":{"nested":1}}`), &bad) == nil {
		t.Fatal("object value accepted")
	}
}
