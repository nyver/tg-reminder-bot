package scheduler

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

type conditionState struct {
	Matched         bool       `json:"matched"`
	LastTriggeredAt *time.Time `json:"last_triggered_at,omitempty"`
}

type observationMetadata struct {
	Condition *conditionState `json:"condition,omitempty"`
}

type conditionDecision struct {
	Matched bool
	Notify  bool
	State   conditionState
}

// reminderCondition converts legacy target/direction fields into the generic
// model. Legacy reminders remain edge-triggered and an empty direction keeps
// the historical "lower than the previous sample" behavior.
func reminderCondition(spec domain.Spec) (domain.Condition, error) {
	if spec.Condition != nil {
		if err := spec.Condition.Validate(); err != nil {
			return domain.Condition{}, err
		}
		return *spec.Condition, nil
	}

	operator := domain.ConditionOperatorLT
	switch strings.ToLower(strings.TrimSpace(spec.Direction)) {
	case "", "below", "down":
	case "above", "up":
		operator = domain.ConditionOperatorGT
	default:
		return domain.Condition{}, fmt.Errorf("unsupported legacy direction %q", spec.Direction)
	}
	return domain.Condition{
		Operator:      operator,
		Target:        spec.Target,
		EdgeTriggered: true,
	}, nil
}

func evaluateMetricCondition(condition domain.Condition, value int64, prev *domain.Observation, now time.Time) conditionDecision {
	previousState, hasState := decodeConditionState(prev)
	eventLike := condition.Target == nil || condition.Operator == domain.ConditionOperatorChanged || condition.Operator == domain.ConditionOperatorChangedPct
	matched := conditionMatches(condition, value, prev)
	previousMatched := false
	if prev != nil && !eventLike {
		if hasState {
			previousMatched = previousState.Matched
		} else {
			previousMatched = conditionMatchesValue(condition, prev.Value)
		}
	}

	notify := false
	if matched {
		switch {
		case eventLike:
			notify = true
		case condition.EdgeTriggered:
			notify = prev != nil && !previousMatched
		default:
			notify = prev == nil || !previousMatched || cooldownElapsed(previousState.LastTriggeredAt, condition.Cooldown.Duration, now)
		}
		if notify && !cooldownElapsed(previousState.LastTriggeredAt, condition.Cooldown.Duration, now) {
			notify = false
		}
	}

	state := conditionState{
		Matched:         matched,
		LastTriggeredAt: previousState.LastTriggeredAt,
	}
	if notify {
		triggeredAt := now.UTC()
		state.LastTriggeredAt = &triggeredAt
	}
	return conditionDecision{Matched: matched, Notify: notify, State: state}
}

func conditionMatches(condition domain.Condition, value int64, prev *domain.Observation) bool {
	switch condition.Operator {
	case domain.ConditionOperatorChanged:
		return prev != nil && value != prev.Value
	case domain.ConditionOperatorChangedPct:
		return prev != nil && percentChanged(prev.Value, value) >= *condition.ChangePercent
	}
	if condition.Target != nil {
		return compareMetric(condition.Operator, value, *condition.Target)
	}
	return prev != nil && compareMetric(condition.Operator, value, prev.Value)
}

func conditionMatchesValue(condition domain.Condition, value int64) bool {
	return condition.Target != nil && compareMetric(condition.Operator, value, *condition.Target)
}

func compareMetric(operator string, value, reference int64) bool {
	switch operator {
	case domain.ConditionOperatorLT:
		return value < reference
	case domain.ConditionOperatorLTE:
		return value <= reference
	case domain.ConditionOperatorGT:
		return value > reference
	case domain.ConditionOperatorGTE:
		return value >= reference
	default:
		return false
	}
}

func percentChanged(previous, current int64) float64 {
	if previous == 0 {
		if current == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return math.Abs(float64(current)-float64(previous)) / math.Abs(float64(previous)) * 100
}

func cooldownElapsed(last *time.Time, cooldown time.Duration, now time.Time) bool {
	return last == nil || cooldown <= 0 || !now.Before(last.Add(cooldown))
}

func decodeConditionState(prev *domain.Observation) (conditionState, bool) {
	if prev == nil || len(prev.Raw) == 0 || string(prev.Raw) == "null" {
		return conditionState{}, false
	}
	var metadata observationMetadata
	if err := json.Unmarshal(prev.Raw, &metadata); err != nil {
		return conditionState{}, false
	}
	if metadata.Condition == nil {
		return conditionState{}, false
	}
	return *metadata.Condition, true
}

func encodeConditionState(state conditionState) json.RawMessage {
	raw, err := json.Marshal(observationMetadata{Condition: &state})
	if err != nil {
		return nil
	}
	return raw
}
