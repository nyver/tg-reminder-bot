package telegram

import (
	"context"
	"strings"

	"github.com/nyver2k/remindertgbot/internal/delivery"
	"github.com/nyver2k/remindertgbot/internal/scheduler"
	tele "gopkg.in/telebot.v3"
)

// TelebotSender implements delivery.Sender using telebot Bot.
type TelebotSender struct {
	bot *tele.Bot
}

func NewTelebotSender(bot *tele.Bot) *TelebotSender {
	return &TelebotSender{bot: bot}
}

func (s *TelebotSender) Send(_ context.Context, message delivery.OutboundMessage) error {
	chat := &tele.Chat{ID: message.UserID}
	text, opts := stripMarkdownV2(message.Text)
	markup := outboundMarkup(message.Actions)
	if markup != nil {
		opts = append(opts, markup)
	}
	if message.Quiet {
		opts = append(opts, tele.Silent)
	}
	_, err := s.bot.Send(chat, text, opts...)
	return err
}

func outboundMarkup(rows [][]delivery.OutboundAction) *tele.ReplyMarkup {
	if len(rows) == 0 {
		return nil
	}
	markup := &tele.ReplyMarkup{}
	buttons := make([]tele.Row, 0, len(rows))
	for _, row := range rows {
		buttonsRow := make(tele.Row, 0, len(row))
		for _, action := range row {
			data, err := encodeCallback(action.Entity, action.Action, action.ID)
			if err != nil {
				continue
			}
			buttonsRow = append(buttonsRow, tele.Btn{Text: action.Text, Data: data})
		}
		if len(buttonsRow) > 0 {
			buttons = append(buttons, buttonsRow)
		}
	}
	if len(buttons) == 0 {
		return nil
	}
	markup.Inline(buttons...)
	return markup
}

// stripMarkdownV2 detects the scheduler.MarkdownV2Prefix sentinel some
// PlannedNotification.Text values carry (set by digest rendering) and
// returns the cleaned text plus the send option needed to render it with
// Telegram's MarkdownV2 parser. Text without the marker is returned
// unchanged with no options, to be sent as plain text like every other
// notification kind — those include provider-controlled titles,
// descriptions and URLs that are not MarkdownV2-escaped, so malformed
// content must not be allowed to break delivery.
func stripMarkdownV2(text string) (string, []interface{}) {
	if stripped, ok := strings.CutPrefix(text, scheduler.MarkdownV2Prefix); ok {
		return stripped, []interface{}{tele.ModeMarkdownV2}
	}
	return text, nil
}
