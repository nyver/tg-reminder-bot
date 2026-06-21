package telegram

import (
	"time"

	tele "gopkg.in/telebot.v3"
)

// NewBot creates and configures a telebot Bot.
func NewBot(token string, h *Handler) (*tele.Bot, error) {
	pref := tele.Settings{
		Token:  token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}
	bot, err := tele.NewBot(pref)
	if err != nil {
		return nil, err
	}
	h.RegisterRoutes(bot)
	return bot, nil
}
