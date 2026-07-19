package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestConditionValidate(t *testing.T) {
	percent := 10.0
	tests := []struct {
		name      string
		condition Condition
		wantErr   string
	}{
		{name: "edge comparison", condition: Condition{Operator: ConditionOperatorLT, EdgeTriggered: true}},
		{name: "level with cooldown", condition: Condition{Operator: ConditionOperatorGTE, Cooldown: Duration{Duration: time.Hour}}},
		{name: "percentage change", condition: Condition{Operator: ConditionOperatorChangedPct, ChangePercent: &percent, EdgeTriggered: true}},
		{name: "unknown operator", condition: Condition{Operator: "eq", EdgeTriggered: true}, wantErr: "unsupported"},
		{name: "percentage missing", condition: Condition{Operator: ConditionOperatorChangedPct, EdgeTriggered: true}, wantErr: "change_percent"},
		{name: "level without cooldown", condition: Condition{Operator: ConditionOperatorLT}, wantErr: "cooldown"},
		{name: "negative cooldown", condition: Condition{Operator: ConditionOperatorChanged, EdgeTriggered: true, Cooldown: Duration{Duration: -time.Second}}, wantErr: "negative"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.condition.Validate()
			if test.wantErr == "" && err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("Validate() error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestConditionJSONUsesDurationString(t *testing.T) {
	target := int64(5_000_000)
	spec := Spec{Condition: &Condition{
		Operator: ConditionOperatorLT, Target: &target,
		Cooldown: Duration{Duration: 24 * time.Hour},
	}}
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"cooldown":"24h0m0s"`) {
		t.Fatalf("condition JSON = %s", raw)
	}

	var decoded Spec
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Condition == nil || decoded.Condition.Cooldown.Duration != 24*time.Hour {
		t.Fatalf("decoded condition = %+v", decoded.Condition)
	}
}
