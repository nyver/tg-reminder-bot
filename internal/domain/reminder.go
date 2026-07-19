package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/google/uuid"
)

type Kind string

const (
	KindAbsolute    Kind = "absolute"
	KindRecurring   Kind = "recurring"
	KindConditional Kind = "conditional"
)

type Trigger string

const (
	TriggerAnchor    Trigger = "anchor"
	TriggerThreshold Trigger = "threshold"
	TriggerDigest    Trigger = "digest"
)

type Status string

const (
	StatusActive    Status = "active"
	StatusPaused    Status = "paused"
	StatusDone      Status = "done"
	StatusCancelled Status = "cancelled"
	StatusFailed    Status = "failed"
)

type Reminder struct {
	ID             uuid.UUID
	UserID         int64
	Kind           Kind
	RawText        string
	Spec           Spec
	Status         Status
	NextEvalAt     *time.Time
	EvalCron       string
	IdempotencyKey string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	Version        int64
	// UserTZ is populated by the scheduler at load time (not persisted).
	UserTZ string
}

// Spec — нормализованное намерение (хранится как jsonb).
type Spec struct {
	Trigger       Trigger    `json:"trigger,omitempty"`
	LeadTime      Duration   `json:"lead_time,omitempty"` // anchor
	Condition     *Condition `json:"condition,omitempty"` // threshold
	Target        *int64     `json:"target,omitempty"`    // deprecated: use condition.target
	Direction     string     `json:"direction,omitempty"` // deprecated: use condition.operator
	Currency      string     `json:"currency,omitempty"`
	TopN          int        `json:"top_n,omitempty"`          // digest
	HorizonDays   int        `json:"horizon_days,omitempty"`   // digest: скользящее окно (дней)
	LookaheadDays int        `json:"lookahead_days,omitempty"` // anchor
	Event         EventSpec  `json:"event"`
	Message       string     `json:"message"`
}

const (
	ConditionOperatorLT         = "lt"
	ConditionOperatorLTE        = "lte"
	ConditionOperatorGT         = "gt"
	ConditionOperatorGTE        = "gte"
	ConditionOperatorChanged    = "changed"
	ConditionOperatorChangedPct = "changed_pct"
)

// Condition describes when a scalar metric produces a notification. Comparison
// operators use Target when set and otherwise compare with the previous sample.
type Condition struct {
	Operator      string   `json:"operator"`
	Target        *int64   `json:"target,omitempty"`
	ChangePercent *float64 `json:"change_percent,omitempty"`
	EdgeTriggered bool     `json:"edge_triggered"`
	Cooldown      Duration `json:"cooldown,omitempty"`
}

// Validate rejects conditions that would either never match or notify on every
// poll accidentally. A level-triggered condition therefore requires a cooldown.
func (c Condition) Validate() error {
	switch c.Operator {
	case ConditionOperatorLT, ConditionOperatorLTE, ConditionOperatorGT, ConditionOperatorGTE,
		ConditionOperatorChanged:
	case ConditionOperatorChangedPct:
		if c.ChangePercent == nil || *c.ChangePercent <= 0 || math.IsNaN(*c.ChangePercent) || math.IsInf(*c.ChangePercent, 0) {
			return fmt.Errorf("condition %q requires a positive change_percent", c.Operator)
		}
	default:
		return fmt.Errorf("unsupported condition operator %q", c.Operator)
	}
	if c.Cooldown.Duration < 0 {
		return fmt.Errorf("condition cooldown cannot be negative")
	}
	if !c.EdgeTriggered && c.Cooldown.Duration <= 0 {
		return fmt.Errorf("level-triggered condition requires a positive cooldown")
	}
	return nil
}

// EventSpec — параметры источника данных.
type EventSpec struct {
	Type   string            `json:"type"` // tv_program | price | travel | rss | weather | exchange_rate
	Title  string            `json:"title"`
	Params map[string]string `json:"params"`
}

// Duration — кастомный тип для хранения time.Duration в JSON как строки (e.g. "3h").
type Duration struct {
	time.Duration
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.Duration.String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// Observation — точка истории измерений.
// threshold: замер цены; digest: снимок дня (value = мин. цена, raw = топ-N).
type Observation struct {
	ID         uuid.UUID
	ReminderID uuid.UUID
	Value      int64
	Currency   string
	Available  bool
	Title      string
	Raw        json.RawMessage
	ObservedAt time.Time
}

type NotificationStatus string

const (
	NotificationPending   NotificationStatus = "pending"
	NotificationSent      NotificationStatus = "sent"
	NotificationFailed    NotificationStatus = "failed"
	NotificationCancelled NotificationStatus = "cancelled"
)

type ScheduledNotification struct {
	ID                   uuid.UUID
	ReminderID           uuid.UUID
	FireAt               time.Time
	Text                 string
	IdempotencyKey       string
	Status               NotificationStatus
	Attempts             int
	CreatedAt            time.Time
	SentAt               *time.Time
	ParentNotificationID *uuid.UUID
}

type User struct {
	ID        int64
	TZ        string
	CreatedAt time.Time
}

// UserPreferences contains Telegram UI defaults that are independent of the
// user's timezone, which remains stored on User for backward compatibility.
type UserPreferences struct {
	UserID               int64
	QuietStart           string
	QuietEnd             string
	MorningTime          string
	DefaultSnoozeMinutes int
	UpdatedAt            time.Time
}

// NotificationAction is an audit record for an idempotent inline action.
type NotificationAction struct {
	ID             uuid.UUID
	NotificationID uuid.UUID
	UserID         int64
	Action         string
	Payload        json.RawMessage
	CreatedAt      time.Time
}

// DialogState — FSM-состояние диалога пользователя (переживает рестарты).
type DialogState string

const (
	DialogIdle         DialogState = "idle"
	DialogAwaitSpec    DialogState = "await_spec"
	DialogAwaitConfirm DialogState = "await_confirm"
	DialogAwaitField   DialogState = "await_field"
	DialogAwaitEdit    DialogState = "await_edit"
)

type Dialog struct {
	UserID    int64
	State     DialogState
	Context   json.RawMessage
	UpdatedAt time.Time
}
