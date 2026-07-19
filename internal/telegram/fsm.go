package telegram

import (
	"context"
	"encoding/json"
	"time"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

// DialogStore persists FSM state across bot restarts.
type DialogStore interface {
	Get(ctx context.Context, userID int64) (*domain.Dialog, error)
	Set(ctx context.Context, d *domain.Dialog) error
	Reset(ctx context.Context, userID int64) error
}

// dialogTTL is the maximum age of a dialog before it is automatically reset.
// Prevents accidental confirmation of stale reminders started days/weeks ago.
const dialogTTL = 30 * time.Minute

// DialogContext carries the pending NLU parse result while awaiting confirmation.
type DialogContext struct {
	Mode       string          `json:"mode,omitempty"` // create | reminder | settings
	RawText    string          `json:"raw_text"`
	Kind       domain.Kind     `json:"kind,omitempty"`
	ParsedSpec json.RawMessage `json:"parsed_spec,omitempty"`
	Confidence float64         `json:"confidence,omitempty"`
	Missing    []string        `json:"missing,omitempty"`
	FieldName  string          `json:"field_name,omitempty"` // for await_field state
	EvalCron   string          `json:"eval_cron,omitempty"`
	FireAt     *string         `json:"fire_at,omitempty"`
	UserTZ     string          `json:"user_tz,omitempty"`    // user's IANA timezone for cron scheduling
	CreatedAt  time.Time       `json:"created_at,omitempty"` // for TTL enforcement
	ReminderID string          `json:"reminder_id,omitempty"`
	Version    int64           `json:"version,omitempty"`
}

func encodeContext(dc *DialogContext) (json.RawMessage, error) {
	return json.Marshal(dc)
}

func decodeContext(raw json.RawMessage) (*DialogContext, error) {
	dc := &DialogContext{}
	if err := json.Unmarshal(raw, dc); err != nil {
		return nil, err
	}
	return dc, nil
}
