package telegram

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/nlu"
	"github.com/nyver2k/remindertgbot/internal/provider"
	"github.com/nyver2k/remindertgbot/internal/scheduler"
	"github.com/robfig/cron/v3"
	tele "gopkg.in/telebot.v3"
)

// ReminderService manages reminder lifecycle.
type ReminderService interface {
	Create(ctx context.Context, rem *domain.Reminder) error
	Get(ctx context.Context, userID int64, id uuid.UUID) (*domain.Reminder, error)
	ListByUser(ctx context.Context, userID int64) ([]domain.Reminder, error)
	Cancel(ctx context.Context, userID int64, id uuid.UUID) error
	Remove(ctx context.Context, userID int64, id uuid.UUID) error
	Pause(ctx context.Context, userID int64, id uuid.UUID, pause bool) error
}

// PriceHistory returns the last price observation for a reminder.
type PriceHistory interface {
	Last(ctx context.Context, reminderID uuid.UUID) (*domain.Observation, error)
}

// UserService manages users.
type UserService interface {
	GetOrCreate(ctx context.Context, userID int64) (*domain.User, error)
	SetTZ(ctx context.Context, userID int64, tz string) error
}

// PriceProber fetches the current price of a product URL.
// Used only in the confirmation dialog; nil disables price preview.
type PriceProber interface {
	Sample(ctx context.Context, q provider.Query) (provider.Measurement, error)
}

type Handler struct {
	reminders            ReminderService
	users                UserService
	dialogs              DialogStore
	parser               nlu.Parser
	prices               PriceProber          // optional
	history              PriceHistory         // optional, for /refresh delta
	schedule             provider.TVScheduler // optional
	evaluator            *scheduler.Evaluator // optional, for /run
	priceDefaultPollCron string
	log                  *slog.Logger
}

func NewHandler(
	reminders ReminderService,
	users UserService,
	dialogs DialogStore,
	parser nlu.Parser,
	prices PriceProber,
	history PriceHistory,
	schedule provider.TVScheduler,
	evaluator *scheduler.Evaluator,
	priceDefaultPollCron string,
	log *slog.Logger,
) *Handler {
	return &Handler{
		reminders:            reminders,
		users:                users,
		dialogs:              dialogs,
		parser:               parser,
		prices:               prices,
		history:              history,
		schedule:             schedule,
		evaluator:            evaluator,
		priceDefaultPollCron: priceDefaultPollCron,
		log:                  log,
	}
}

func (h *Handler) RegisterRoutes(bot *tele.Bot) {
	bot.Handle("/start", h.handleStart)
	bot.Handle("/help", h.handleHelp)
	bot.Handle("/list", h.handleList)
	bot.Handle("/cancel", h.handleCancel)
	bot.Handle("/remove", h.handleRemove)
	bot.Handle("/pause", h.handlePause)
	bot.Handle("/resume", h.handleResume)
	bot.Handle("/refresh", h.handleRefresh)
	bot.Handle("/run", h.handleRun)
	bot.Handle("/tz", h.handleTZ)
	bot.Handle("/tv", h.handleTV)
	bot.Handle("/rss", h.handleRSS)
	bot.Handle(tele.OnText, h.handleText)
	bot.Handle("\fconfirm_yes", h.handleConfirmYes)
	bot.Handle("\fconfirm_no", h.handleConfirmNo)
}

func (h *Handler) handleStart(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	if _, err := h.users.GetOrCreate(ctx, c.Sender().ID); err != nil {
		h.log.Error("getorcreate user", "err", err)
	}
	return c.Send(msgWelcome, mainMenu(), tele.ModeMarkdown)
}

func (h *Handler) handleHelp(c tele.Context) error {
	return c.Send(msgHelp, mainMenu(), tele.ModeMarkdown)
}

func (h *Handler) handleList(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	rems, err := h.reminders.ListByUser(ctx, c.Sender().ID)
	if err != nil {
		return c.Send("Ошибка при получении списка напоминаний.")
	}
	if len(rems) == 0 {
		return c.Send(msgEmptyList, mainMenu(), tele.ModeMarkdown)
	}

	loc, _ := time.LoadLocation(defaultUserTimezone)
	if u, err := h.users.GetOrCreate(ctx, c.Sender().ID); err == nil && u.TZ != "" {
		if l, err := time.LoadLocation(u.TZ); err == nil {
			loc = l
		}
	}

	for _, message := range h.buildListMessages(ctx, rems, loc) {
		if err := c.Send(message, tele.ModeMarkdownV2); err != nil {
			return err
		}
	}
	return nil
}

func (h *Handler) buildListMessages(ctx context.Context, rems []domain.Reminder, loc *time.Location) []string {
	const continuationHeader = "*Ваши напоминания \\(продолжение\\):*\n\n"

	var messages []string
	var current strings.Builder
	current.WriteString(fmt.Sprintf("*Ваши напоминания:* %d\n\n", len(rems)))
	for i, r := range rems {
		var item strings.Builder
		h.writeListReminder(ctx, &item, i+1, r, loc)
		if runeLen(current.String())+runeLen(item.String()) > telegramListMessageLimit {
			messages = append(messages, current.String())
			current.Reset()
			current.WriteString(continuationHeader)
		}
		current.WriteString(item.String())
	}
	if current.Len() > 0 {
		messages = append(messages, current.String())
	}
	return messages
}

func (h *Handler) writeListReminder(ctx context.Context, sb *strings.Builder, index int, r domain.Reminder, loc *time.Location) {
	title := truncateRunes(listReminderTitle(r), listReminderTitleLimit)
	sb.WriteString(fmt.Sprintf("*%d\\. %s*\n", index, escapeMarkdown(title)))
	sb.WriteString(fmt.Sprintf("   • Статус: `%s`\n", escapeMarkdown(statusLabel(r.Status))))

	switch {
	case r.Spec.Trigger == domain.TriggerDigest && r.Spec.Event.Type == "rss":
		writeListRSSDetails(sb, r)
	case r.Spec.Trigger == domain.TriggerThreshold && r.Spec.Event.Type == "price":
		h.writeListPriceDetails(ctx, sb, r, loc)
	case r.Spec.Trigger == domain.TriggerAnchor && r.Spec.Event.Type == "tv_program":
		writeListTVDetails(sb, r)
	case r.EvalCron != "":
		if line := formatCronLineRu(r.EvalCron); line != "" {
			sb.WriteString("   • Расписание: " + escapeMarkdown(line) + "\n")
		}
	}

	writeListActions(sb, r)
	sb.WriteByte('\n')
}

func listReminderTitle(r domain.Reminder) string {
	switch {
	case r.Spec.Trigger == domain.TriggerDigest && r.Spec.Event.Type == "rss":
		if title := strings.TrimSpace(r.Spec.Event.Title); title != "" {
			return "RSS дайджест: " + title
		}
		if urls := splitFeedURLs(r.Spec.Event.Params["url"]); len(urls) > 0 {
			return "RSS дайджест: " + feedHostsDisplay(urls)
		}
		return "RSS дайджест"
	case r.Spec.Trigger == domain.TriggerAnchor && r.Spec.Event.Type == "tv_program":
		if r.Spec.Event.Title != "" {
			return "ТВ: " + r.Spec.Event.Title
		}
	case r.Spec.Trigger == domain.TriggerThreshold && r.Spec.Event.Type == "price":
		if r.Spec.Event.Title != "" {
			return "Цена: " + r.Spec.Event.Title
		}
	case r.Spec.Message != "":
		return r.Spec.Message
	}
	return r.RawText
}

func writeListRSSDetails(sb *strings.Builder, r domain.Reminder) {
	feeds := splitFeedURLs(r.Spec.Event.Params["url"])
	if r.EvalCron != "" {
		sb.WriteString(fmt.Sprintf("   • Рассылка: `%s`", escapeMarkdown(cronToHHMM(r.EvalCron))))
		if topN := orDefaultInt(r.Spec.TopN, rssDefaultTopN); topN > 0 {
			sb.WriteString(fmt.Sprintf(" · топ\\-%d", topN))
		}
		sb.WriteByte('\n')
	}
	if len(feeds) > 0 {
		sb.WriteString(fmt.Sprintf("   • Ленты \\(%d\\): %s\n", len(feeds), escapeMarkdown(feedHostsDisplay(feeds))))
	}
}

func (h *Handler) writeListPriceDetails(ctx context.Context, sb *strings.Builder, r domain.Reminder, loc *time.Location) {
	if h.history != nil {
		if obs, err := h.history.Last(ctx, r.ID); err == nil && obs != nil && obs.Value > 0 {
			title := obs.Title
			if title == "" {
				title = r.Spec.Event.Title
			}
			if title != "" && strings.TrimSpace(title) != strings.TrimSpace(r.Spec.Event.Title) {
				sb.WriteString("   • Товар: " + escapeMarkdown(truncateRunes(title, listReminderTitleLimit)) + "\n")
			}
			at := obs.ObservedAt.In(loc).Format("02.01 15:04")
			sb.WriteString(fmt.Sprintf("   • Последняя цена: *%s* \\(%s\\)\n",
				escapeMarkdown(formatPriceRub(obs.Value, obs.Currency)),
				escapeMarkdown(at),
			))
		}
	}
	if u := r.Spec.Event.Params["url"]; u != "" {
		sb.WriteString("   • Сайт: " + escapeMarkdown(feedHost(u)) + "\n")
	}
	sb.WriteString(fmt.Sprintf("   • Обновить цену: `/refresh %s`\n", r.ID.String()))
}

func writeListTVDetails(sb *strings.Builder, r domain.Reminder) {
	if ch := r.Spec.Event.Params["channel"]; ch != "" {
		sb.WriteString("   • Канал: " + escapeMarkdown(truncateRunes(ch, listReminderTitleLimit)) + "\n")
	}
	if r.Spec.LeadTime.Duration > 0 {
		sb.WriteString("   • Уведомить за: " + escapeMarkdown(formatDurationRu(r.Spec.LeadTime.Duration)) + "\n")
	}
}

func writeListActions(sb *strings.Builder, r domain.Reminder) {
	id := r.ID.String()
	sb.WriteString("   • Действия:\n")
	sb.WriteString(fmt.Sprintf("     `/run %s`\n", id))
	command := "pause"
	if r.Status != domain.StatusActive {
		command = "resume"
	}
	sb.WriteString(fmt.Sprintf("     `/%s %s`\n", command, id))
	sb.WriteString(fmt.Sprintf("     `/remove %s`\n", id))
}

func splitFeedURLs(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if u := strings.TrimSpace(part); u != "" {
			out = append(out, u)
		}
	}
	return out
}

func statusLabel(status domain.Status) string {
	switch status {
	case domain.StatusActive:
		return "активно"
	case domain.StatusPaused:
		return "пауза"
	case domain.StatusDone:
		return "завершено"
	case domain.StatusFailed:
		return "ошибка"
	default:
		return string(status)
	}
}

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func runeLen(s string) int {
	return len([]rune(s))
}

func truncateRunes(s string, limit int) string {
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= limit {
		return string(runes)
	}
	return strings.TrimSpace(string(runes[:limit-1])) + "…"
}

func (h *Handler) handleCancel(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	args := strings.TrimSpace(c.Message().Payload)
	if args == "" {
		return c.Send("Укажите ID напоминания: `/cancel <id>`", tele.ModeMarkdown)
	}
	id, err := uuid.Parse(args)
	if err != nil {
		return c.Send("Неверный ID напоминания.")
	}
	if err := h.reminders.Cancel(ctx, c.Sender().ID, id); err != nil {
		return c.Send("Напоминание не найдено или уже отменено.")
	}
	return c.Send("✅ Напоминание отменено.")
}

func (h *Handler) handleRemove(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	args := strings.TrimSpace(c.Message().Payload)
	if args == "" {
		return c.Send("Укажите ID напоминания: `/remove <id>`", tele.ModeMarkdown)
	}
	id, err := uuid.Parse(args)
	if err != nil {
		return c.Send("Неверный ID напоминания.")
	}
	if err := h.reminders.Remove(ctx, c.Sender().ID, id); err != nil {
		return c.Send("Напоминание не найдено.")
	}
	return c.Send("✅ Напоминание удалено без возможности восстановления.")
}

func (h *Handler) handleRefresh(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	args := strings.TrimSpace(c.Message().Payload)
	if args == "" {
		return c.Send("Укажите ID напоминания: `/refresh <id>`", tele.ModeMarkdown)
	}
	id, err := uuid.Parse(args)
	if err != nil {
		return c.Send("Неверный ID напоминания.")
	}

	rem, err := h.reminders.Get(ctx, c.Sender().ID, id)
	if err != nil {
		return c.Send("Напоминание не найдено.")
	}
	if rem.Spec.Trigger != domain.TriggerThreshold || rem.Spec.Event.Type != "price" {
		return c.Send("Команда `/refresh` доступна только для напоминаний о снижении цены.", tele.ModeMarkdown)
	}
	if h.prices == nil {
		return c.Send("Провайдер цен недоступен.")
	}

	_ = c.Bot().Notify(c.Sender(), tele.Typing)

	sampleCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	m, err := h.prices.Sample(sampleCtx, provider.Query{
		Title:  rem.Spec.Event.Title,
		Params: rem.Spec.Event.Params,
	})
	if err != nil || !m.Available || m.Value <= 0 {
		var sb strings.Builder
		sb.WriteString("⚠️ Не удалось получить текущую цену\n")
		if u := rem.Spec.Event.Params["url"]; u != "" {
			sb.WriteString(escapeMarkdown(u) + "\n")
		}
		if m.HTTPStatus > 0 {
			sb.WriteString(fmt.Sprintf("\nHTTP статус: *%d*", m.HTTPStatus))
		}
		return c.Send(sb.String(), tele.ModeMarkdownV2)
	}

	title := rem.Spec.Event.Title
	if title == "" {
		title = m.Title
	}

	var sb strings.Builder
	if title != "" {
		sb.WriteString("📌 *" + escapeMarkdown(title) + "*\n")
	}
	sb.WriteString(fmt.Sprintf("💰 Текущая цена: *%s*\n", formatPriceRub(m.Value, m.Currency)))

	if h.history != nil {
		if prev, hErr := h.history.Last(ctx, rem.ID); hErr == nil && prev != nil && prev.Value > 0 {
			diff := prev.Value - m.Value
			switch {
			case diff > 0:
				sb.WriteString(fmt.Sprintf("📉 Снизилась на *%s* с прошлой проверки\n", formatPriceRub(diff, m.Currency)))
			case diff < 0:
				sb.WriteString(fmt.Sprintf("📈 Выросла на *%s* с прошлой проверки\n", formatPriceRub(-diff, m.Currency)))
			default:
				sb.WriteString("➡️ Не изменилась с прошлой проверки\n")
			}
		}
	}

	if u := rem.Spec.Event.Params["url"]; u != "" {
		sb.WriteString("🔗 " + escapeMarkdown(u))
	}
	return c.Send(sb.String(), tele.ModeMarkdownV2)
}

// handleRun forces immediate evaluation of a configured reminder, regardless
// of its trigger type or cron schedule (e.g. running an RSS/travel digest
// reminder immediately generates and sends that digest). It reuses the same
// scheduler.Evaluator the worker runs on each tick, but sends the resulting
// text straight to the chat instead of going through the persisted
// notification queue — so a manual run is never silently deduplicated by the
// per-day idempotency key a scheduled digest uses, and it never disturbs the
// reminder's own NextEvalAt/cron progression.
func (h *Handler) handleRun(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), manualRunTimeout)
	defer cancel()
	args := strings.TrimSpace(c.Message().Payload)
	if args == "" {
		return c.Send("Укажите ID напоминания: `/run <id>`", tele.ModeMarkdown)
	}
	id, err := uuid.Parse(args)
	if err != nil {
		return c.Send("Неверный ID напоминания.")
	}
	if h.evaluator == nil {
		return c.Send("Принудительный запуск сейчас недоступен.")
	}

	rem, err := h.reminders.Get(ctx, c.Sender().ID, id)
	if err != nil {
		return c.Send("Напоминание не найдено.")
	}
	if u, err := h.users.GetOrCreate(ctx, c.Sender().ID); err == nil {
		rem.UserTZ = u.TZ
	}

	_ = c.Bot().Notify(c.Sender(), tele.Typing)

	runCtx, cancel := context.WithTimeout(ctx, manualRunTimeout)
	defer cancel()
	planned, err := h.evaluator.Evaluate(runCtx, *rem)
	if err != nil {
		h.log.Error("manual run evaluate", "id", rem.ID, "err", err)
		return c.Send("Ошибка при выполнении напоминания. Попробуйте позже.")
	}
	if len(planned) == 0 {
		return c.Send("Сейчас отправлять нечего: условие не выполнено или данные недоступны.")
	}
	for _, p := range planned {
		text, opts := stripMarkdownV2(p.Text)
		if err := c.Send(text, opts...); err != nil {
			h.log.Error("manual run send", "id", rem.ID, "err", err)
		}
	}
	return nil
}

func (h *Handler) handlePause(c tele.Context) error {
	return h.setPause(c, true)
}

func (h *Handler) handleResume(c tele.Context) error {
	return h.setPause(c, false)
}

func (h *Handler) setPause(c tele.Context, pause bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	args := strings.TrimSpace(c.Message().Payload)
	if args == "" {
		cmd := "pause"
		if !pause {
			cmd = "resume"
		}
		return c.Send(fmt.Sprintf("Укажите ID: `/%s <id>`", cmd), tele.ModeMarkdown)
	}
	id, err := uuid.Parse(args)
	if err != nil {
		return c.Send("Неверный ID напоминания.")
	}
	if err := h.reminders.Pause(ctx, c.Sender().ID, id, pause); err != nil {
		return c.Send("Напоминание не найдено.")
	}
	verb := "приостановлено"
	if !pause {
		verb = "возобновлено"
	}
	return c.Send("✅ Напоминание " + verb + ".")
}

func (h *Handler) handleTZ(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	tz := strings.TrimSpace(c.Message().Payload)
	if tz == "" {
		u, err := h.users.GetOrCreate(ctx, c.Sender().ID)
		if err != nil {
			return c.Send("Не удалось получить профиль. Попробуйте позже.")
		}
		return c.Send(fmt.Sprintf("Текущий часовой пояс: `%s`\nДля изменения: `/tz Europe/Moscow`", u.TZ), tele.ModeMarkdown)
	}
	if _, err := time.LoadLocation(tz); err != nil {
		return c.Send("Неверный часовой пояс. Пример: `Europe/Moscow`, `Asia/Yekaterinburg`.", tele.ModeMarkdown)
	}
	if err := h.users.SetTZ(ctx, c.Sender().ID, tz); err != nil {
		return c.Send("Ошибка при сохранении часового пояса.")
	}
	return c.Send(fmt.Sprintf("✅ Часовой пояс установлен: `%s`", tz), tele.ModeMarkdown)
}

var ruMonths = [13]string{"", "янв", "фев", "мар", "апр", "май", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"}
var ruWeekdays = [7]string{"вс", "пн", "вт", "ср", "чт", "пт", "сб"}

func (h *Handler) handleTV(c tele.Context) error {
	if h.schedule == nil {
		return c.Send("Расписание телепрограмм недоступно.")
	}

	args := strings.TrimSpace(c.Message().Payload)
	if args == "" {
		return c.Send(
			"Использование:\n"+
				"`/tv КВН` — расписание на всех каналах\n"+
				"`/tv КВН | Первый канал` — только на заданном канале\n"+
				"`/tv | Первый канал` — программа канала на сегодня\n"+
				"`/tv | Первый канал 25.06` — программа канала на дату",
			tele.ModeMarkdown,
		)
	}

	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	userID := c.Sender().ID

	loc, _ := time.LoadLocation(defaultUserTimezone)
	if u, err := h.users.GetOrCreate(ctx, userID); err == nil && u.TZ != "" {
		if l, err := time.LoadLocation(u.TZ); err == nil {
			loc = l
		}
	}

	title, channel := args, ""
	if parts := strings.SplitN(args, "|", 2); len(parts) == 2 {
		title = strings.TrimSpace(parts[0])
		channel = strings.TrimSpace(parts[1])
	}

	// Empty title + channel → full channel day schedule.
	if title == "" {
		if channel == "" {
			return c.Send("Укажите канал: `/tv | Первый канал`", tele.ModeMarkdown)
		}
		return h.handleTVChannelDay(ctx, c, channel, time.Now(), loc)
	}

	now := time.Now()
	shows, err := h.schedule.QuerySchedule(ctx, title, channel, now, now.Add(7*24*time.Hour))
	if err != nil {
		h.log.Error("tv schedule query", "err", err)
		return c.Send("Ошибка при запросе расписания. Попробуйте позже.")
	}

	if len(shows) == 0 {
		if channel != "" {
			return c.Send(fmt.Sprintf(
				"Программа *%s* на *%s* не найдена в расписании на ближайшую неделю.",
				escapeMarkdown(title), escapeMarkdown(channel),
			), tele.ModeMarkdown)
		}
		return c.Send(fmt.Sprintf(
			"Программа *%s* не найдена в расписании на ближайшую неделю.",
			escapeMarkdown(title),
		), tele.ModeMarkdown)
	}

	const maxShows = 50
	truncated := len(shows) > maxShows
	if truncated {
		shows = shows[:maxShows]
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("📺 *%s*:\n\n", escapeMarkdown(title)))
	writeTVShows(&sb, shows, channel, loc)

	if truncated {
		sb.WriteString(fmt.Sprintf("\n_…показаны первые %d результатов_", maxShows))
	}

	return c.Send(sb.String(), tele.ModeMarkdownV2)
}

// handleTVChannelDay shows the full programme schedule for a channel on a given day.
// channelArg may include an optional date suffix: "Первый канал 25.06" or "Первый канал завтра".
func (h *Handler) handleTVChannelDay(ctx context.Context, c tele.Context, channelArg string, now time.Time, loc *time.Location) error {
	channel, day := parseChannelAndDate(channelArg, now, loc)
	if channel == "" {
		return c.Send("Укажите название канала.")
	}

	from := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, loc)
	to := from.Add(24 * time.Hour)

	chName, shows, err := h.schedule.ChannelDaySchedule(ctx, channel, from, to)
	if err != nil {
		h.log.Error("tv channel day schedule", "err", err)
		return c.Send("Ошибка при запросе расписания. Попробуйте позже.")
	}
	if len(shows) == 0 {
		return c.Send(fmt.Sprintf(
			"Программа для *%s* на %s не найдена\\.",
			escapeMarkdown(channel), escapeMarkdown(from.Format("02.01")),
		), tele.ModeMarkdownV2)
	}

	if chName == "" {
		chName = channel
	}

	// For today's schedule, hide programmes that have already ended.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	if from.Equal(today) {
		shows = filterEndedShows(shows, now)
	}
	if len(shows) == 0 {
		return c.Send(fmt.Sprintf(
			"Все программы *%s* на сегодня уже завершились\\.",
			escapeMarkdown(chName),
		), tele.ModeMarkdownV2)
	}

	dateLabel := from.Format("02.01.2006")
	var sb strings.Builder
	fmt.Fprintf(&sb, "📺 *%s* — %s:\n\n", escapeMarkdown(chName), escapeMarkdown(dateLabel))
	writeTVDaySchedule(&sb, shows, loc)
	return c.Send(sb.String(), tele.ModeMarkdownV2)
}

// filterEndedShows removes shows whose end time is known and already passed.
// Shows with no EndsAt are kept — we can't confirm they're over.
func filterEndedShows(shows []provider.TVShowtime, now time.Time) []provider.TVShowtime {
	out := shows[:0]
	for _, s := range shows {
		if !s.EndsAt.IsZero() && !s.EndsAt.After(now) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// writeTVDaySchedule renders a flat (single-channel) day schedule without channel headers.
func writeTVDaySchedule(sb *strings.Builder, shows []provider.TVShowtime, loc *time.Location) {
	for _, show := range shows {
		local := show.StartsAt.In(loc)
		timeStr := local.Format("15:04")
		if !show.EndsAt.IsZero() {
			timeStr += "–" + show.EndsAt.In(loc).Format("15:04")
		}
		sb.WriteString("  `" + timeStr + "` — " + escapeMarkdown(show.Title) + "\n")
	}
}

// parseChannelAndDate splits "Первый канал 25.06" → ("Первый канал", <June 25>).
// The optional date token can be: DD.MM, DD.MM.YY, DD.MM.YYYY, "сегодня", "завтра", "послезавтра".
// If no date is found, returns today.
func parseChannelAndDate(s string, now time.Time, loc *time.Location) (channel string, day time.Time) {
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	s = strings.TrimSpace(s)
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", today
	}
	last := fields[len(fields)-1]
	if d, ok := parseDayToken(last, today); ok {
		channel = strings.TrimSpace(strings.Join(fields[:len(fields)-1], " "))
		return channel, d
	}
	return s, today
}

// parseDayToken parses a single date token relative to today.
func parseDayToken(token string, today time.Time) (time.Time, bool) {
	switch strings.ToLower(token) {
	case "сегодня":
		return today, true
	case "завтра":
		return today.AddDate(0, 0, 1), true
	case "послезавтра":
		return today.AddDate(0, 0, 2), true
	}
	// DD.MM
	if t, err := time.ParseInLocation("02.01", token, today.Location()); err == nil {
		return time.Date(today.Year(), t.Month(), t.Day(), 0, 0, 0, 0, today.Location()), true
	}
	// DD.MM.YY
	if t, err := time.ParseInLocation("02.01.06", token, today.Location()); err == nil {
		return t, true
	}
	// DD.MM.YYYY
	if t, err := time.ParseInLocation("02.01.2006", token, today.Location()); err == nil {
		return t, true
	}
	return time.Time{}, false
}

func writeTVShows(sb *strings.Builder, shows []provider.TVShowtime, fallbackChannel string, loc *time.Location) {
	type channelGroup struct {
		name  string
		shows []provider.TVShowtime
	}

	groups := make([]channelGroup, 0)
	groupIndex := make(map[string]int)
	for _, show := range shows {
		name := show.Channel
		if name == "" {
			name = fallbackChannel
		}
		if name == "" {
			name = "Канал не указан"
		}

		i, ok := groupIndex[name]
		if !ok {
			i = len(groups)
			groupIndex[name] = i
			groups = append(groups, channelGroup{name: name})
		}
		groups[i].shows = append(groups[i].shows, show)
	}

	for groupNo, group := range groups {
		if groupNo > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString("*" + escapeMarkdown(group.name) + "*\n")

		var curDay string
		for _, show := range group.shows {
			local := show.StartsAt.In(loc)
			day := fmt.Sprintf("%s, %d %s", ruWeekdays[local.Weekday()], local.Day(), ruMonths[local.Month()])
			if day != curDay {
				curDay = day
				sb.WriteString("_" + escapeMarkdown(day) + "_\n")
			}

			timeStr := local.Format("15:04")
			if !show.EndsAt.IsZero() {
				timeStr += "–" + show.EndsAt.In(loc).Format("15:04")
			}
			sb.WriteString("  `" + timeStr + "` — " + escapeMarkdown(show.Title) + "\n")
		}
	}
}

// rssDefaultCronHour/Minute — daily digest time used when the user does not
// specify one explicitly.
const (
	rssDefaultCronHour   = 9
	rssDefaultCronMinute = 0
	rssDefaultTopN       = 10
	rssMinTopN           = 1
	rssMaxTopN           = 20
	// rssMaxURLs bounds how many feeds one /rss reminder can combine. Matches
	// provider/rss's own maxFeedURLs — enforced here too so a bad list fails
	// fast with a clear message instead of only at evaluation time.
	rssMaxURLs = 10
)

func (h *Handler) handleRSS(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)
	if args == "" {
		return c.Send(
			"Использование:\n"+
				"`/rss https://example.com/feed.xml` — дайджест важных новостей ежедневно в 09:00 (топ-10)\n"+
				"`/rss https://a.com/feed.xml,https://b.com/feed.xml` — общий дайджест по нескольким лентам\n"+
				"`/rss https://example.com/feed.xml | 08:30` — своё время рассылки\n"+
				"`/rss https://example.com/feed.xml | 08:30 | 10` — своё время и число новостей (1–20)",
			tele.ModeMarkdown,
		)
	}

	feedURLs, hour, minute, topN, err := parseRSSArgs(args)
	if err != nil {
		return c.Send("⚠️ " + err.Error())
	}
	cronExpr := fmt.Sprintf("%d %d * * *", minute, hour)
	title := feedHostsDisplay(feedURLs)

	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	userID := c.Sender().ID

	userTZ := ""
	if u, err := h.users.GetOrCreate(ctx, userID); err == nil && u.TZ != "" {
		userTZ = u.TZ
	}

	next, err := nextCronAt(cronExpr, time.Now(), userTZ)
	if err != nil {
		h.log.Error("rss nextCronAt", "err", err)
		return c.Send("Ошибка при расчёте расписания рассылки.")
	}

	rem := &domain.Reminder{
		UserID:   userID,
		RawText:  "RSS-дайджест: " + title,
		Kind:     domain.KindConditional,
		Status:   domain.StatusActive,
		EvalCron: cronExpr,
		Spec: domain.Spec{
			Trigger: domain.TriggerDigest,
			TopN:    topN,
			Event: domain.EventSpec{
				Type:   "rss",
				Title:  title,
				Params: map[string]string{"url": strings.Join(feedURLs, ",")},
			},
		},
		NextEvalAt: &next,
	}
	rem.IdempotencyKey = reminderIdemKey(rem)

	if err := h.reminders.Create(ctx, rem); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			return c.Send("ℹ️ У вас уже есть подписка на эту ленту (или набор лент) с такими же параметрами.")
		}
		h.log.Error("create rss reminder", "err", err)
		return c.Send("Ошибка при сохранении подписки.")
	}

	return c.Send(fmt.Sprintf(
		"✅ Дайджест настроен: *%s*\nВремя рассылки: `%02d:%02d`\nЧисло новостей: *%d*\n\n`/cancel %s` — отменить\n`/pause %s` — приостановить",
		escapeMarkdown(title), hour, minute, topN, rem.ID.String(), rem.ID.String(),
	), tele.ModeMarkdown)
}

// parseRSSArgs parses the /rss payload "<url>[,<url>...][ | HH:MM][ | N]"
// into its feed URLs (one or more, combined into a single ranked digest),
// daily digest time and top-N item count. It performs a cheap, local
// validation of each URL (scheme + host) — the full SSRF-safe check happens
// in provider/rss at evaluation time on the worker, since that's where the
// request is actually made.
func parseRSSArgs(payload string) (feedURLs []string, hour, minute, topN int, err error) {
	parts := strings.Split(payload, "|")
	for _, raw := range strings.Split(parts[0], ",") {
		u := strings.TrimSpace(raw)
		if u == "" {
			continue
		}
		parsed, uerr := url.ParseRequestURI(u)
		if uerr != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" {
			return nil, 0, 0, 0, fmt.Errorf("некорректная ссылка: %s", u)
		}
		feedURLs = append(feedURLs, u)
	}
	if len(feedURLs) == 0 {
		return nil, 0, 0, 0, fmt.Errorf("укажите ссылку на RSS/Atom-ленту")
	}
	if len(feedURLs) > rssMaxURLs {
		return nil, 0, 0, 0, fmt.Errorf("слишком много лент: максимум %d", rssMaxURLs)
	}

	hour, minute = rssDefaultCronHour, rssDefaultCronMinute
	topN = rssDefaultTopN
	for _, part := range parts[1:] {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if t, terr := time.Parse("15:04", p); terr == nil {
			hour, minute = t.Hour(), t.Minute()
			continue
		}
		if n, nerr := strconv.Atoi(p); nerr == nil {
			if n < rssMinTopN || n > rssMaxTopN {
				return nil, 0, 0, 0, fmt.Errorf("число новостей должно быть от %d до %d", rssMinTopN, rssMaxTopN)
			}
			topN = n
			continue
		}
		return nil, 0, 0, 0, fmt.Errorf("не удалось разобрать параметр %q (ожидается время ЧЧ:ММ или число)", p)
	}
	return feedURLs, hour, minute, topN, nil
}

// feedHost extracts a short display name for a feed URL, e.g.
// "https://www.lenta.ru/rss" -> "lenta.ru".
func feedHost(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	return strings.TrimPrefix(u.Hostname(), "www.")
}

// feedHostsDisplay builds a short display label for one or more feed URLs,
// e.g. "lenta.ru" for one, "lenta.ru, habr.com" for a few, and
// "lenta.ru, habr.com +2 ещё" beyond a handful — used as the digest title
// and in the /rss confirmation message.
func feedHostsDisplay(urls []string) string {
	const maxShown = 3
	hosts := make([]string, 0, len(urls))
	for _, u := range urls {
		hosts = append(hosts, feedHost(u))
	}
	if len(hosts) <= maxShown {
		return strings.Join(hosts, ", ")
	}
	return strings.Join(hosts[:maxShown], ", ") + fmt.Sprintf(" +%d ещё", len(hosts)-maxShown)
}

// cronToHHMM converts a "<minute> <hour> * * *" cron expression (as produced
// by handleRSS) back into "HH:MM" for display in /list. Falls back to the
// raw expression if it doesn't match that shape.
func cronToHHMM(cronExpr string) string {
	fields := strings.Fields(cronExpr)
	if len(fields) < 2 {
		return cronExpr
	}
	minute, err1 := strconv.Atoi(fields[0])
	hour, err2 := strconv.Atoi(fields[1])
	if err1 != nil || err2 != nil {
		return cronExpr
	}
	return fmt.Sprintf("%02d:%02d", hour, minute)
}

func (h *Handler) handleText(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	userID := c.Sender().ID
	text := c.Text()

	dialog, err := h.dialogs.Get(ctx, userID)
	if err != nil {
		h.log.Error("dialog get", "err", err)
		return c.Send("Внутренняя ошибка. Попробуйте снова.")
	}

	// Expire stale dialogs so users cannot accidentally confirm a days-old reminder.
	if dialog.State != domain.DialogIdle {
		if dc, decErr := decodeContext(dialog.Context); decErr == nil &&
			!dc.CreatedAt.IsZero() && time.Since(dc.CreatedAt) > dialogTTL {
			_ = h.dialogs.Reset(ctx, userID)
			dialog.State = domain.DialogIdle
		}
	}

	switch dialog.State {
	case domain.DialogAwaitConfirm:
		switch confirmationDecision(text) {
		case confirmDecisionYes:
			return h.confirmReminderByText(ctx, c, userID)
		case confirmDecisionNo:
			_ = h.dialogs.Reset(ctx, userID)
			return c.Send("Ок, не создаю. Пришлите новый текст напоминания, когда будете готовы.", mainMenu())
		default:
			return c.Send("Ответьте «да», чтобы создать, или «нет», чтобы исправить текст. Кнопки под сообщением тоже работают.")
		}

	case domain.DialogAwaitField:
		return h.handleFieldInput(ctx, c, dialog, text)

	default:
		return h.startParsing(ctx, c, userID, text)
	}
}

func (h *Handler) startParsing(ctx context.Context, c tele.Context, userID int64, text string) error {
	_ = c.Bot().Notify(c.Sender(), tele.Typing)

	loc, userTZ, err := h.loadUserLocation(ctx, userID)
	if err != nil {
		h.log.Error("load user timezone", "user_id", userID, "err", err)
		return c.Send("Не удалось получить часовой пояс. Попробуйте позже.", mainMenu())
	}

	result, err := h.parser.Parse(ctx, text, loc)
	if err != nil {
		h.log.Error("parse failed", "err", err)
		return c.Send(msgParseFailed, mainMenu())
	}
	if result == nil || result.Spec == nil || result.Confidence <= 0 {
		return c.Send(msgParseFailed, mainMenu())
	}

	return h.continueParsing(ctx, c, userID, text, result, userTZ)
}

func (h *Handler) loadUserLocation(ctx context.Context, userID int64) (*time.Location, string, error) {
	u, err := h.users.GetOrCreate(ctx, userID)
	if err != nil {
		return nil, "", fmt.Errorf("get user: %w", err)
	}
	if u == nil {
		return nil, "", fmt.Errorf("get user: empty profile")
	}
	tz := strings.TrimSpace(u.TZ)
	if tz == "" {
		tz = defaultUserTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, "", fmt.Errorf("load user timezone %q: %w", tz, err)
	}
	return loc, tz, nil
}

// continueParsing asks for any still-missing fields, otherwise validates and
// shows the confirmation dialog. Shared by the initial free-text parse and
// the field-clarification follow-up (handleFieldInput) so answering one of
// several missing fields re-prompts for whatever is still missing instead of
// jumping straight to a confirmation that validateParseResult will reject.
func (h *Handler) continueParsing(ctx context.Context, c tele.Context, userID int64, text string, result *nlu.ParseResult, userTZ string) error {
	if len(result.Missing) > 0 {
		ctxData := &DialogContext{
			RawText:    text,
			Kind:       result.Kind,
			ParsedSpec: mustMarshal(result.Spec),
			Confidence: result.Confidence,
			Missing:    result.Missing,
			FieldName:  result.Missing[0],
			EvalCron:   result.EvalCron,
			FireAt:     result.FireAt,
			UserTZ:     userTZ,
			CreatedAt:  time.Now(),
		}
		ctxJSON, err := encodeContext(ctxData)
		if err != nil {
			h.log.Error("encode dialog context", "err", err)
			return c.Send("Внутренняя ошибка. Попробуйте снова.")
		}
		_ = h.dialogs.Set(ctx, &domain.Dialog{
			UserID:  userID,
			State:   domain.DialogAwaitField,
			Context: ctxJSON,
		})
		return c.Send(fmt.Sprintf("Уточните: %s", fieldPrompt(result.Missing[0])))
	}
	if err := validateParseResult(result); err != nil {
		h.log.Warn("invalid parse result", "err", err, "confidence", result.Confidence)
		if errors.Is(err, errTravelUnsupported) {
			return c.Send("Поиск билетов пока недоступен. Можно создать обычное напоминание о поездке или выбрать другой тип напоминания.")
		}
		return c.Send("Не удалось определить все параметры напоминания. Попробуйте переформулировать.")
	}

	return h.askConfirmation(ctx, c, userID, text, result, userTZ)
}

func (h *Handler) askConfirmation(ctx context.Context, c tele.Context, userID int64, rawText string, result *nlu.ParseResult, userTZ string) error {
	evalCron := result.EvalCron
	if evalCron == "" && result.Spec != nil && result.Spec.Trigger == domain.TriggerThreshold {
		evalCron = h.priceDefaultPollCron
	}
	ctxData := &DialogContext{
		RawText:    rawText,
		Kind:       result.Kind,
		ParsedSpec: mustMarshal(result.Spec),
		Confidence: result.Confidence,
		EvalCron:   evalCron,
		FireAt:     result.FireAt,
		UserTZ:     userTZ,
		CreatedAt:  time.Now(),
	}
	ctxJSON, err := encodeContext(ctxData)
	if err != nil {
		h.log.Error("encode dialog context", "err", err)
		return c.Send("Внутренняя ошибка. Попробуйте снова.")
	}
	_ = h.dialogs.Set(ctx, &domain.Dialog{
		UserID:  userID,
		State:   domain.DialogAwaitConfirm,
		Context: ctxJSON,
	})

	confirmMsg := fmt.Sprintf("*Создать напоминание?*\n\n%s", h.formatConfirmSpec(ctx, result))
	menu := &tele.ReplyMarkup{}
	menu.Inline(
		menu.Row(
			menu.Data("✅ Да, создать", "confirm_yes"),
			menu.Data("✏️ Исправить", "confirm_no"),
		),
	)
	return c.Send(confirmMsg, menu, tele.ModeMarkdownV2)
}

// formatConfirmSpec builds the human-readable spec block for the confirmation
// dialog. For price-drop reminders it probes the current price (best-effort).
func (h *Handler) formatConfirmSpec(ctx context.Context, result *nlu.ParseResult) string {
	spec := result.Spec
	base := formatSpec(spec) + formatFireLine(result)
	if spec == nil || spec.Trigger != domain.TriggerThreshold || spec.Event.Type != "price" {
		return base
	}
	if h.prices == nil {
		return base
	}

	priceCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	m, err := h.prices.Sample(priceCtx, provider.Query{
		Title:  spec.Event.Title,
		Params: spec.Event.Params,
	})
	if err != nil {
		h.log.Warn("price probe for confirmation failed", "err", err)
		return base + "⚠️ _Текущую цену не удалось получить_\n"
	}
	if !m.Available || m.Value <= 0 {
		return base + "⚠️ _Текущую цену не удалось получить_\n"
	}

	// Update spec title from page if NLU left it empty.
	title := spec.Event.Title
	if title == "" && m.Title != "" {
		title = m.Title
	}

	var sb strings.Builder
	if title != "" {
		sb.WriteString("📌 *" + escapeMarkdown(title) + "*\n")
	}
	sb.WriteString(fmt.Sprintf("💰 Текущая цена: *%s*\n", formatPriceRub(m.Value, m.Currency)))
	sb.WriteString("📉 Уведомить при снижении цены\n")
	if u := spec.Event.Params["url"]; u != "" {
		sb.WriteString("🔗 " + escapeMarkdown(u) + "\n")
	}
	if pollLine := formatFireLine(result); pollLine != "" {
		sb.WriteString(pollLine)
	}
	return sb.String()
}

// formatFireLine returns a human-readable fire time line for the confirmation dialog.
// Returns "" for conditional/anchor reminders — they have no user-visible absolute time.
func formatFireLine(result *nlu.ParseResult) string {
	if result == nil {
		return ""
	}
	if result.FireAt != nil {
		t, err := time.Parse(time.RFC3339, *result.FireAt)
		if err == nil {
			date := fmt.Sprintf("%d %s", t.Day(), ruMonths[t.Month()])
			if t.Year() != time.Now().Year() {
				date += fmt.Sprintf(" %d", t.Year())
			}
			return fmt.Sprintf("⏰ %s в %s\n", date, t.Format("15:04"))
		}
	}
	if result.EvalCron != "" {
		if line := formatCronLineRu(result.EvalCron); line != "" {
			return "🔁 " + line + "\n"
		}
	}
	return ""
}

// formatCronLineRu formats simple 5-field cron expressions into Russian.
// Returns "" for patterns it cannot handle.
func formatCronLineRu(expr string) string {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return ""
	}
	m, h, dom, mon, dow := fields[0], fields[1], fields[2], fields[3], fields[4]
	if dom != "*" || mon != "*" {
		return ""
	}

	// */N * * * * — every N minutes
	if strings.HasPrefix(m, "*/") && h == "*" && dow == "*" {
		n, err := strconv.Atoi(m[2:])
		if err == nil && n > 0 {
			return fmt.Sprintf("каждые %d %s", n, pluralRu(n, "минуту", "минуты", "минут"))
		}
	}
	// 0 */N * * * — every N hours
	if m == "0" && strings.HasPrefix(h, "*/") && dow == "*" {
		n, err := strconv.Atoi(h[2:])
		if err == nil && n > 0 {
			return fmt.Sprintf("каждые %d %s", n, pluralRu(n, "час", "часа", "часов"))
		}
	}
	// 0 * * * * — every hour
	if m == "0" && h == "*" && dow == "*" {
		return "каждый час"
	}

	mi, errM := strconv.Atoi(m)
	hi, errH := strconv.Atoi(h)
	if errM != nil || errH != nil {
		return ""
	}
	timeStr := fmt.Sprintf("%02d:%02d", hi, mi)
	var dayStr string
	switch dow {
	case "*":
		dayStr = "каждый день"
	case "1-5":
		dayStr = "пн–пт"
	case "1":
		dayStr = "каждый пн"
	case "2":
		dayStr = "каждый вт"
	case "3":
		dayStr = "каждую ср"
	case "4":
		dayStr = "каждый чт"
	case "5":
		dayStr = "каждую пт"
	case "6":
		dayStr = "каждую сб"
	case "0", "7":
		dayStr = "каждое вс"
	default:
		return ""
	}
	return dayStr + " в " + timeStr
}

func formatPriceRub(kopecks int64, currency string) string {
	rubles := kopecks / 100
	sym := "₽"
	switch currency {
	case "USD":
		sym = "$"
	case "EUR":
		sym = "€"
	}
	// Format with thousands separator.
	s := strconv.FormatInt(rubles, 10)
	var result []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ' ')
		}
		result = append(result, c)
	}
	return string(result) + " " + sym
}

var (
	errPendingSessionExpired = errors.New("pending reminder session expired")
	errPendingDialogData     = errors.New("pending reminder dialog data is invalid")
	errPendingReminderSpec   = errors.New("pending reminder spec is invalid")
)

func (h *Handler) handleConfirmYes(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	userID := c.Sender().ID

	err := h.createReminderFromDialog(ctx, userID)
	if err != nil {
		switch {
		case errors.Is(err, errPendingSessionExpired):
			return c.Respond(&tele.CallbackResponse{Text: "Сессия истекла. Начните заново."})
		case errors.Is(err, errPendingDialogData):
			return c.Respond(&tele.CallbackResponse{Text: "Ошибка данных."})
		case errors.Is(err, errPendingReminderSpec):
			return c.Respond(&tele.CallbackResponse{Text: "Некорректные параметры напоминания."})
		case errors.Is(err, domain.ErrAlreadyExists):
			_ = c.Respond(&tele.CallbackResponse{})
			return c.Edit("ℹ️ У вас уже есть такое напоминание.")
		default:
			h.log.Error("create reminder", "err", err)
			return c.Respond(&tele.CallbackResponse{Text: "Ошибка сохранения."})
		}
	}

	_ = c.Respond(&tele.CallbackResponse{})
	return c.Edit("✅ Напоминание создано!\n\n/list — посмотреть все напоминания")
}

func (h *Handler) confirmReminderByText(ctx context.Context, c tele.Context, userID int64) error {
	err := h.createReminderFromDialog(ctx, userID)
	if err != nil {
		switch {
		case errors.Is(err, errPendingSessionExpired):
			return c.Send("Сессия истекла. Пришлите текст напоминания заново.", mainMenu())
		case errors.Is(err, errPendingDialogData):
			return c.Send("Не удалось прочитать черновик напоминания. Начните заново.", mainMenu())
		case errors.Is(err, errPendingReminderSpec):
			return c.Send("Параметры напоминания выглядят некорректно. Попробуйте переформулировать.", mainMenu())
		case errors.Is(err, domain.ErrAlreadyExists):
			return c.Send("ℹ️ У вас уже есть такое напоминание.", mainMenu())
		default:
			h.log.Error("create reminder", "err", err)
			return c.Send("Не удалось сохранить напоминание. Попробуйте позже.", mainMenu())
		}
	}

	return c.Send("✅ Напоминание создано!\n\n/list — посмотреть все напоминания", mainMenu())
}

func (h *Handler) createReminderFromDialog(ctx context.Context, userID int64) error {
	dialog, err := h.dialogs.Get(ctx, userID)
	if err != nil || dialog.State != domain.DialogAwaitConfirm {
		return errPendingSessionExpired
	}
	dc, err := decodeContext(dialog.Context)
	if err != nil {
		return errPendingDialogData
	}
	if !dc.CreatedAt.IsZero() && time.Since(dc.CreatedAt) > dialogTTL {
		_ = h.dialogs.Reset(ctx, userID)
		return errPendingSessionExpired
	}

	var spec domain.Spec
	if err := json.Unmarshal(dc.ParsedSpec, &spec); err != nil {
		return errPendingDialogData
	}

	result := &nlu.ParseResult{
		Kind: dc.Kind, Spec: &spec, Confidence: dc.Confidence,
		EvalCron: dc.EvalCron, FireAt: dc.FireAt,
	}
	rem, err := buildReminder(userID, dc.RawText, result, time.Now(), dc.UserTZ)
	if err != nil {
		h.log.Error("build reminder", "err", err)
		_ = h.dialogs.Reset(ctx, userID)
		return errPendingReminderSpec
	}
	if err := h.reminders.Create(ctx, rem); err != nil {
		if errors.Is(err, domain.ErrAlreadyExists) {
			_ = h.dialogs.Reset(ctx, userID)
			return domain.ErrAlreadyExists
		}
		return err
	}

	_ = h.dialogs.Reset(ctx, userID)
	return nil
}

func (h *Handler) handleConfirmNo(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	_ = h.dialogs.Reset(ctx, c.Sender().ID)
	_ = c.Respond(&tele.CallbackResponse{})
	return c.Edit("Отменено. Отправьте новый текст напоминания.")
}

func (h *Handler) handleFieldInput(ctx context.Context, c tele.Context, dialog *domain.Dialog, text string) error {
	dc, err := decodeContext(dialog.Context)
	if err != nil {
		_ = h.dialogs.Reset(ctx, c.Sender().ID)
		return c.Send("Что-то пошло не так. Начните заново.")
	}

	// Merge field answer into existing parsed spec via re-parse with context.
	combined := dc.RawText + " " + text
	loc, userTZ, err := h.loadUserLocation(ctx, c.Sender().ID)
	if err != nil {
		h.log.Error("load user timezone", "user_id", c.Sender().ID, "err", err)
		return c.Send("Не удалось получить часовой пояс. Попробуйте позже.")
	}
	result, err := h.parser.Parse(ctx, combined, loc)
	if err != nil {
		// Don't leave user stuck — reset dialog so next message starts fresh.
		_ = h.dialogs.Reset(ctx, c.Sender().ID)
		return c.Send("Не удалось распознать. Попробуйте описать напоминание заново.")
	}
	return h.continueParsing(ctx, c, c.Sender().ID, combined, result, userTZ)
}

const (
	defaultUserTimezone    = "Europe/Moscow"
	defaultConditionalCron = "*/5 * * * *"
	// handlerTimeout caps the total wall time of a single Telegram handler.
	// Prevents goroutine leaks when DB or external calls hang indefinitely.
	handlerTimeout           = 15 * time.Second
	manualRunTimeout         = 3 * time.Minute
	telegramListMessageLimit = 3500
	listReminderTitleLimit   = 240
)

var errTravelUnsupported = errors.New("travel reminders are not supported")

func buildReminder(userID int64, rawText string, result *nlu.ParseResult, now time.Time, userTZ string) (*domain.Reminder, error) {
	if err := validateParseResult(result); err != nil {
		return nil, err
	}
	spec := *result.Spec
	kind := parseResultKind(result)
	rem := &domain.Reminder{
		UserID:  userID,
		RawText: rawText,
		Spec:    spec,
		Status:  domain.StatusActive,
		Kind:    kind,
	}
	switch kind {
	case domain.KindAbsolute:
		fireAt, err := time.Parse(time.RFC3339, *result.FireAt)
		if err != nil {
			return nil, fmt.Errorf("parse fire_at: %w", err)
		}
		// For anchor-triggered reminders (e.g. TV show, price drop), we need
		// the watcher to evaluate immediately so the EPG provider can look up
		// the real event time and compute the correct notification fire time
		// (event_time - lead_time). If we waited until event_time, the
		// notification window would already have passed.
		if result.Spec.Trigger == domain.TriggerAnchor {
			rem.NextEvalAt = PtrTime(now.UTC())
		} else {
			rem.NextEvalAt = &fireAt
		}
	case domain.KindRecurring:
		rem.EvalCron = result.EvalCron
		next, err := nextCronAt(result.EvalCron, now, userTZ)
		if err != nil {
			return nil, err
		}
		rem.NextEvalAt = &next
	case domain.KindConditional:
		rem.EvalCron = result.EvalCron
		if rem.EvalCron == "" {
			rem.EvalCron = defaultConditionalCron
		}
		next := now.UTC()
		if result.Spec.Trigger == domain.TriggerDigest {
			var err error
			next, err = nextCronAt(rem.EvalCron, now, userTZ)
			if err != nil {
				return nil, err
			}
		}
		rem.NextEvalAt = &next
	default:
		return nil, fmt.Errorf("unsupported reminder kind %q", kind)
	}
	rem.IdempotencyKey = reminderIdemKey(rem)
	return rem, nil
}

// reminderIdemKey produces a stable key that identifies "the same reminder"
// for a given user. Two reminders are considered identical if they track the
// same event/cron with the same lead-time, regardless of the human-readable
// message text.
func reminderIdemKey(rem *domain.Reminder) string {
	var b strings.Builder
	b.WriteString(strconv.FormatInt(rem.UserID, 10))
	b.WriteByte('|')
	b.WriteString(string(rem.Kind))
	b.WriteByte('|')
	switch rem.Kind {
	case domain.KindConditional:
		b.WriteString(rem.Spec.Event.Type)
		b.WriteByte('|')
		b.WriteString(normalizeField(rem.Spec.Event.Title))
		b.WriteByte('|')
		b.WriteString(canonicalParams(rem.Spec.Event.Params))
		b.WriteByte('|')
		b.WriteString(rem.Spec.LeadTime.Duration.String())
	case domain.KindAbsolute:
		if rem.NextEvalAt != nil {
			b.WriteString(rem.NextEvalAt.UTC().Format(time.RFC3339))
		}
		b.WriteByte('|')
		b.WriteString(normalizeField(rem.Spec.Message))
	case domain.KindRecurring:
		b.WriteString(rem.EvalCron)
		b.WriteByte('|')
		b.WriteString(normalizeField(rem.Spec.Message))
	}
	h := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", h[:16])
}

func normalizeField(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

func canonicalParams(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+normalizeField(params[k]))
	}
	return strings.Join(parts, ",")
}

func validateParseResult(result *nlu.ParseResult) error {
	if result == nil || result.Spec == nil || result.Confidence <= 0 {
		return fmt.Errorf("empty parse result")
	}
	kind := parseResultKind(result)
	switch kind {
	case domain.KindAbsolute:
		if result.FireAt == nil {
			return fmt.Errorf("absolute reminder has no fire_at")
		}
		if _, err := time.Parse(time.RFC3339, *result.FireAt); err != nil {
			return fmt.Errorf("invalid fire_at: %w", err)
		}
	case domain.KindRecurring:
		if _, err := parseCron(result.EvalCron); err != nil {
			return err
		}
	case domain.KindConditional:
		if result.Spec.Trigger == "" || result.Spec.Event.Type == "" {
			return fmt.Errorf("conditional reminder is incomplete")
		}
		switch result.Spec.Event.Type {
		case "price":
			if result.Spec.Event.Params["url"] == "" {
				return fmt.Errorf("price reminder requires event.params.url")
			}
		case "tv_program":
			if result.Spec.Event.Title == "" {
				return fmt.Errorf("TV reminder has no title")
			}
			if result.Spec.Event.Params["channel"] == "" && result.Spec.Event.Params["channel_id"] == "" {
				return fmt.Errorf("TV reminder has no channel")
			}
		case "travel":
			return errTravelUnsupported
		case "rss":
			if result.Spec.Trigger != domain.TriggerDigest {
				return fmt.Errorf("rss reminder requires digest trigger")
			}
			u := result.Spec.Event.Params["url"]
			if u == "" {
				return fmt.Errorf("rss reminder requires event.params.url")
			}
			if n := strings.Count(u, ",") + 1; n > rssMaxURLs {
				return fmt.Errorf("rss reminder has too many feed URLs (%d, max %d)", n, rssMaxURLs)
			}
		default:
			if result.Spec.Event.Title == "" {
				return fmt.Errorf("conditional reminder is incomplete")
			}
		}
	default:
		return fmt.Errorf("unknown reminder kind %q", kind)
	}
	return nil
}

func parseResultKind(result *nlu.ParseResult) domain.Kind {
	if result == nil || result.Spec == nil {
		return ""
	}
	if result.Spec.Trigger == domain.TriggerAnchor || result.Spec.Trigger == domain.TriggerThreshold || result.Spec.Trigger == domain.TriggerDigest {
		return domain.KindConditional
	}
	if result.Kind != "" {
		return result.Kind
	}
	if result.FireAt != nil {
		return domain.KindAbsolute
	}
	if result.EvalCron != "" {
		return domain.KindRecurring
	}
	return ""
}

func parseCron(expr string) (cron.Schedule, error) {
	if strings.TrimSpace(expr) == "" {
		return nil, fmt.Errorf("cron expression is empty")
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	schedule, err := parser.Parse(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid cron expression: %w", err)
	}
	return schedule, nil
}

func nextCronAt(expr string, now time.Time, userTZ string) (time.Time, error) {
	schedule, err := parseCron(expr)
	if err != nil {
		return time.Time{}, err
	}
	tz := strings.TrimSpace(userTZ)
	if tz == "" {
		tz = defaultUserTimezone
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return time.Time{}, fmt.Errorf("load timezone %q: %w", tz, err)
	}
	return schedule.Next(now.In(loc)), nil
}

// PtrTime returns a pointer to the given time value.
func PtrTime(t time.Time) *time.Time { return &t }

func formatSpec(spec *domain.Spec) string {
	if spec == nil {
		return "(пусто)"
	}
	var sb strings.Builder
	if spec.Event.Title != "" {
		sb.WriteString("📌 *" + escapeMarkdown(spec.Event.Title) + "*\n")
	} else if spec.Message != "" && spec.Trigger != domain.TriggerThreshold {
		// For price reminders the meaningful title is the product name from the page,
		// shown in the enriched confirmation path. In the fallback path the generic
		// message just duplicates the "📉 Уведомить при снижении цены" line below.
		sb.WriteString("📌 *" + escapeMarkdown(spec.Message) + "*\n")
	}
	switch spec.Trigger {
	case domain.TriggerAnchor:
		if ch := spec.Event.Params["channel"]; ch != "" {
			sb.WriteString("📺 " + escapeMarkdown(ch) + "\n")
		}
		if spec.LeadTime.Duration > 0 {
			sb.WriteString("⏰ Уведомить за " + escapeMarkdown(formatDurationRu(spec.LeadTime.Duration)) + " до начала\n")
		}
	case domain.TriggerThreshold:
		if u := spec.Event.Params["url"]; u != "" {
			sb.WriteString("🔗 " + escapeMarkdown(u) + "\n")
		}
		sb.WriteString("📉 Уведомить при снижении цены\n")
	case domain.TriggerDigest:
		if spec.Event.Type == "rss" {
			for _, u := range strings.Split(spec.Event.Params["url"], ",") {
				if u = strings.TrimSpace(u); u != "" {
					sb.WriteString("📰 " + escapeMarkdown(u) + "\n")
				}
			}
		}
		if spec.TopN > 0 {
			sb.WriteString(fmt.Sprintf("📋 Топ\\-%d предложений\n", spec.TopN))
		}
		if spec.HorizonDays > 0 {
			sb.WriteString(fmt.Sprintf("📅 Горизонт: %d дн\\.\n", spec.HorizonDays))
		}
	default:
		// Only write message as body if the event title was already shown as header
		// and the message adds something new. When Event.Title is empty, spec.Message
		// was already rendered as the header above — don't repeat it.
		if spec.Message != "" && spec.Event.Title != "" && spec.Event.Title != spec.Message {
			sb.WriteString(escapeMarkdown(spec.Message) + "\n")
		}
	}
	return sb.String()
}

// formatDurationRu converts a duration to a human-readable Russian string.
func formatDurationRu(d time.Duration) string {
	switch {
	case d%(7*24*time.Hour) == 0:
		n := int(d / (7 * 24 * time.Hour))
		return fmt.Sprintf("%d %s", n, pluralRu(n, "неделю", "недели", "недель"))
	case d%(24*time.Hour) == 0:
		n := int(d / (24 * time.Hour))
		return fmt.Sprintf("%d %s", n, pluralRu(n, "день", "дня", "дней"))
	case d%time.Hour == 0:
		n := int(d / time.Hour)
		return fmt.Sprintf("%d %s", n, pluralRu(n, "час", "часа", "часов"))
	case d >= time.Minute:
		n := int(d / time.Minute)
		return fmt.Sprintf("%d %s", n, pluralRu(n, "минуту", "минуты", "минут"))
	default:
		n := int(d / time.Second)
		if n <= 0 {
			n = 1
		}
		return fmt.Sprintf("%d %s", n, pluralRu(n, "секунду", "секунды", "секунд"))
	}
}

func pluralRu(n int, one, few, many string) string {
	n = n % 100
	if n >= 11 && n <= 19 {
		return many
	}
	switch n % 10 {
	case 1:
		return one
	case 2, 3, 4:
		return few
	default:
		return many
	}
}

func fieldPrompt(field string) string {
	switch field {
	case "time":
		return "в какое время?"
	case "date":
		return "на какую дату?"
	case "origin":
		return "откуда?"
	case "destination":
		return "куда?"
	default:
		return field + "?"
	}
}

// mdReplacer escapes all MarkdownV2 special characters.
// Initialised once at package load — strings.NewReplacer is not cheap.
var mdReplacer = strings.NewReplacer(
	"\\", "\\\\",
	"_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]",
	"(", "\\(", ")", "\\)", "~", "\\~", "`", "\\`",
	">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-",
	"=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}",
	".", "\\.", "!", "\\!",
)

func escapeMarkdown(s string) string { return mdReplacer.Replace(s) }

func mustMarshal(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

type confirmDecision int

const (
	confirmDecisionUnknown confirmDecision = iota
	confirmDecisionYes
	confirmDecisionNo
)

func confirmationDecision(text string) confirmDecision {
	normalized := strings.ToLower(strings.TrimSpace(text))
	normalized = strings.Trim(normalized, ".!,? ")
	switch normalized {
	case "да", "д", "yes", "y", "ok", "ок", "создать", "сохрани", "сохранить", "подтверждаю":
		return confirmDecisionYes
	case "нет", "не", "n", "no", "отмена", "отменить", "исправить", "переделать", "назад":
		return confirmDecisionNo
	default:
		return confirmDecisionUnknown
	}
}

func mainMenu() *tele.ReplyMarkup {
	menu := &tele.ReplyMarkup{
		ResizeKeyboard: true,
		IsPersistent:   true,
		Placeholder:    "Напишите напоминание или выберите команду",
	}
	menu.Reply(
		menu.Row(menu.Text("/list"), menu.Text("/help")),
		menu.Row(menu.Text("/tv"), menu.Text("/rss")),
		menu.Row(menu.Text("/tz")),
	)
	return menu
}

const msgWelcome = `*Привет! Я бот напоминаний.*

Просто напишите обычным текстом, что и когда нужно напомнить. Я уточню детали и попрошу подтвердить перед сохранением.

Например:
• «напомни завтра в 9:00 позвонить маме»
• «каждый будний день в 9:00 напоминай выпить таблетку»
• «уведоми при снижении цены: https://example.com/product»

/tv — расписание TV программ
/rss — ежедневный RSS-дайджест
/help — справка
/list — список напоминаний`

const msgEmptyList = `У вас пока нет активных напоминаний.

Напишите мне обычной фразой, например:
• «напомни завтра в 9:00 позвонить маме»
• «каждый понедельник в 8:30 напоминай про совещание»
• «уведоми при снижении цены: https://example.com/product»`

const msgParseFailed = `Не удалось распознать напоминание.

Попробуйте написать проще: что сделать и когда.
Например: «напомни завтра в 9:00 позвонить маме».`

const msgHelp = `*Что можно сделать*

Напоминание можно создать без команды: просто отправьте текст, а я покажу черновик и попрошу подтверждение.

*Команды:*

/list — список активных напоминаний
/cancel <id> — отменить напоминание
/remove <id> — удалить напоминание без возможности восстановления
/pause <id> — приостановить
/resume <id> — возобновить
/refresh <id> — запросить текущую цену прямо сейчас
/run <id> — принудительно запустить напоминание сейчас (например, сгенерировать дайджест)
/tz <зона> — установить часовой пояс (например, Europe/Moscow)
/tv <программа> — расписание на ближайшую неделю
/tv <программа> | <канал> — расписание на конкретном канале
/tv | <канал> — программа канала на сегодня
/tv | <канал> 25\.06 — программа канала на дату
/rss <ссылка> — дайджест важных новостей ежедневно в 09:00 (топ\-10)
/rss <ссылка1>,<ссылка2> — общий дайджест по нескольким лентам
/rss <ссылка> | ЧЧ:ММ — своё время рассылки
/rss <ссылка> | ЧЧ:ММ | N — своё время и число новостей \(1\-20\)
/help — эта справка

*Примеры напоминаний:*
• «напомни завтра в 9:00 позвонить маме»
• «каждый понедельник в 8:30 напоминай про совещание»
• «уведоми за 3 часа до КВН на Первом»
• «уведоми за 1 неделю до КВН на Первом»
• «вот ссылка на товар — уведоми при снижении цены»
• «вот ссылка — уведоми при снижении цены каждые 4 часа»
• «каждый день в 18:00 создай дайджест новостей на основе https://lenta.ru/rss»`
