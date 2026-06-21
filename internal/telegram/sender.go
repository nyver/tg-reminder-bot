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
	_, err := s.bot.Send(chat, text, tele.ModeMarkdown)
	return err
}
