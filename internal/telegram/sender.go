package telegram

import (
	"context"

	tele "gopkg.in/telebot.v3"
)

// TelebotSender implements delivery.Sender using telebot Bot.
type TelebotSender struct {
	bot *tele.Bot
}

func NewTelebotSender(bot *tele.Bot) *TelebotSender {
	return &TelebotSender{bot: bot}
}

func (s *TelebotSender) Send(_ context.Context, userID int64, text string) error {
	chat := &tele.Chat{ID: userID}
	// Delivery messages include provider-controlled titles, descriptions and URLs.
	// Send them as plain text so malformed Markdown cannot break notification delivery.
	_, err := s.bot.Send(chat, text)
	return err
}
