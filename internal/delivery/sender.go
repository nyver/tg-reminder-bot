package delivery

import (
	"context"

	"github.com/google/uuid"
)

// OutboundAction describes an interaction without coupling delivery to Telegram.
type OutboundAction struct {
	Text   string
	Entity string
	Action string
	ID     uuid.UUID
}

// OutboundMessage is a transport-independent notification with action rows.
type OutboundMessage struct {
	UserID  int64
	Text    string
	Quiet   bool
	Actions [][]OutboundAction
}

// Sender delivers a structured notification to the end user.
type Sender interface {
	Send(ctx context.Context, message OutboundMessage) error
}

// FakeSender records sent messages for tests.
type FakeSender struct {
	Sent []SentMessage
}

type SentMessage struct {
	UserID  int64
	Text    string
	Quiet   bool
	Actions [][]OutboundAction
}

func (f *FakeSender) Send(_ context.Context, message OutboundMessage) error {
	f.Sent = append(f.Sent, SentMessage{
		UserID: message.UserID, Text: message.Text, Quiet: message.Quiet, Actions: message.Actions,
	})
	return nil
}
