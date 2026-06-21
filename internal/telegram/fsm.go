package telegram

import (
	"context"
	"encoding/json"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

// DialogStore persists FSM state across bot restarts.
type DialogStore interface {
	Get(ctx context.Context, userID int64) (*domain.Dialog, error)
	Set(ctx context.Context, d *domain.Dialog) error
	Reset(ctx context.Context, userID int64) error
}

// DialogContext carries the pending NLU parse result while awaiting confirmation.
type DialogContext struct {
	RawText    string          `json:"raw_text"`
	ParsedSpec json.RawMessage `json:"parsed_spec,omitempty"`
	Confidence float64         `json:"confidence,omitempty"`
	Missing    []string        `json:"missing,omitempty"`
	FieldName  string          `json:"field_name,omitempty"` // for await_field state
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
