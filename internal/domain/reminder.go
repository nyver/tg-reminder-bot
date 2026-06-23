package domain

import (
	"encoding/json"
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
	StatusActive Status = "active"
	StatusPaused Status = "paused"
	StatusDone   Status = "done"
	StatusFailed Status = "failed"
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
	// UserTZ is populated by the scheduler at load time (not persisted).
	UserTZ string
}

// Spec — нормализованное намерение (хранится как jsonb).
type Spec struct {
	Trigger       Trigger   `json:"trigger,omitempty"`
	LeadTime      Duration  `json:"lead_time,omitempty"`    // anchor
	Target        *int64    `json:"target,omitempty"`       // threshold: целевая цена
	Direction     string    `json:"direction,omitempty"`    // threshold: "below"
	Currency      string    `json:"currency,omitempty"`
	TopN          int       `json:"top_n,omitempty"`        // digest
	HorizonDays   int       `json:"horizon_days,omitempty"` // digest: скользящее окно (дней)
	LookaheadDays int       `json:"lookahead_days,omitempty"` // anchor
	Event         EventSpec `json:"event"`
	Message       string    `json:"message"`
}

// EventSpec — параметры источника данных.
type EventSpec struct {
	Type   string            `json:"type"`   // tv_program | price | travel
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
	NotificationPending NotificationStatus = "pending"
	NotificationSent    NotificationStatus = "sent"
	NotificationFailed  NotificationStatus = "failed"
)

type ScheduledNotification struct {
	ID             uuid.UUID
	ReminderID     uuid.UUID
	FireAt         time.Time
	Text           string
	IdempotencyKey string
	Status         NotificationStatus
	Attempts       int
	CreatedAt      time.Time
	SentAt         *time.Time
}

type User struct {
	ID        int64
	TZ        string
	CreatedAt time.Time
}

// DialogState — FSM-состояние диалога пользователя (переживает рестарты).
type DialogState string

const (
	DialogIdle        DialogState = "idle"
	DialogAwaitSpec   DialogState = "await_spec"
	DialogAwaitConfirm DialogState = "await_confirm"
	DialogAwaitField  DialogState = "await_field"
)

type Dialog struct {
	UserID    int64
	State     DialogState
	Context   json.RawMessage
	UpdatedAt time.Time
}
