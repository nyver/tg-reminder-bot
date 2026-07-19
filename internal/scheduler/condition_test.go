package scheduler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

func TestReminderConditionSupportsLegacyDirection(t *testing.T) {
	target := int64(100)
	condition, err := reminderCondition(domain.Spec{Target: &target, Direction: "above"})
	if err != nil {
		t.Fatal(err)
	}
	if condition.Operator != domain.ConditionOperatorGT || condition.Target == nil || *condition.Target != target || !condition.EdgeTriggered {
		t.Fatalf("condition = %+v", condition)
	}
}

func TestReminderConditionRejectsUnknownLegacyDirection(t *testing.T) {
	if _, err := reminderCondition(domain.Spec{Direction: "sideways"}); err == nil {
		t.Fatal("expected unknown legacy direction to fail")
	}
}

func TestEvaluateMetricConditionEdgeTriggered(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	target := int64(50)
	condition := domain.Condition{Operator: domain.ConditionOperatorLT, Target: &target, EdgeTriggered: true}

	decision := evaluateMetricCondition(condition, 49, &domain.Observation{Value: 55}, now)
	if !decision.Matched || !decision.Notify {
		t.Fatalf("crossing decision = %+v", decision)
	}

	prev := &domain.Observation{Value: 49, Raw: encodeConditionState(decision.State)}
	decision = evaluateMetricCondition(condition, 45, prev, now.Add(time.Minute))
	if !decision.Matched || decision.Notify {
		t.Fatalf("sustained edge decision = %+v", decision)
	}

	decision = evaluateMetricCondition(condition, 49, nil, now)
	if decision.Notify {
		t.Fatalf("first edge sample must only establish a baseline: %+v", decision)
	}
}

func TestEvaluateMetricConditionLevelTriggeredCooldown(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	target := int64(50)
	condition := domain.Condition{
		Operator: domain.ConditionOperatorLTE, Target: &target,
		Cooldown: domain.Duration{Duration: time.Hour},
	}

	first := evaluateMetricCondition(condition, 50, nil, now)
	if !first.Notify {
		t.Fatalf("first matching level sample = %+v", first)
	}
	prev := &domain.Observation{Value: 50, Raw: encodeConditionState(first.State)}
	beforeCooldown := evaluateMetricCondition(condition, 49, prev, now.Add(59*time.Minute))
	if beforeCooldown.Notify {
		t.Fatalf("notification repeated before cooldown: %+v", beforeCooldown)
	}
	prev = &domain.Observation{Value: 49, Raw: encodeConditionState(beforeCooldown.State)}
	afterCooldown := evaluateMetricCondition(condition, 48, prev, now.Add(time.Hour))
	if !afterCooldown.Notify {
		t.Fatalf("notification not repeated after cooldown: %+v", afterCooldown)
	}
}

func TestEvaluateMetricConditionChangedAndPercent(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	prev := &domain.Observation{Value: 100}
	changed := domain.Condition{Operator: domain.ConditionOperatorChanged, EdgeTriggered: true}
	if decision := evaluateMetricCondition(changed, 101, prev, now); !decision.Notify {
		t.Fatalf("changed decision = %+v", decision)
	}

	percent := 10.0
	changedPct := domain.Condition{Operator: domain.ConditionOperatorChangedPct, ChangePercent: &percent, EdgeTriggered: true}
	if decision := evaluateMetricCondition(changedPct, 110, prev, now); !decision.Notify {
		t.Fatalf("changed_pct decision = %+v", decision)
	}
	if decision := evaluateMetricCondition(changedPct, 109, prev, now); decision.Notify {
		t.Fatalf("sub-threshold changed_pct decision = %+v", decision)
	}
	if decision := evaluateMetricCondition(changedPct, 1, &domain.Observation{Value: 0}, now); !decision.Notify {
		t.Fatalf("change from zero decision = %+v", decision)
	}
}

func TestCompareMetricOperators(t *testing.T) {
	tests := []struct {
		operator string
		value    int64
		target   int64
		want     bool
	}{
		{domain.ConditionOperatorLT, 1, 2, true},
		{domain.ConditionOperatorLTE, 2, 2, true},
		{domain.ConditionOperatorGT, 3, 2, true},
		{domain.ConditionOperatorGTE, 2, 2, true},
		{"unknown", 2, 2, false},
	}
	for _, test := range tests {
		if got := compareMetric(test.operator, test.value, test.target); got != test.want {
			t.Errorf("compareMetric(%q, %d, %d) = %t, want %t", test.operator, test.value, test.target, got, test.want)
		}
	}
}

func TestDecodeConditionStateRejectsUnrelatedAndMalformedRaw(t *testing.T) {
	for _, raw := range []json.RawMessage{json.RawMessage(`{"other":true}`), json.RawMessage(`{`)} {
		if state, ok := decodeConditionState(&domain.Observation{Raw: raw}); ok {
			t.Fatalf("decodeConditionState(%s) = %+v, true", raw, state)
		}
	}
}
