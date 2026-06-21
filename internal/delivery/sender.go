package delivery

import (
	"context"

	"github.com/nyver2k/remindertgbot/internal/domain"
)

// Sender delivers a notification to the end user.
type Sender interface {
	Send(ctx context.Context, userID int64, text string) error
}

// FakeSender records sent messages for tests.
type FakeSender struct {
	Sent []SentMessage
}

type SentMessage struct {
	UserID int64
	Text   string
}

func (f *FakeSender) Send(_ context.Context, userID int64, text string) error {
	f.Sent = append(f.Sent, SentMessage{UserID: userID, Text: text})
	return nil
}

// ReminderLookup resolves UserID from a notification.
type ReminderLookup interface {
	Get(ctx context.Context, id interface{}) (*domain.Reminder, error)
}
