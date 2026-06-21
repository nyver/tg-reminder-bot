package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/nlu"
	tele "gopkg.in/telebot.v3"
)

// ReminderService manages reminder lifecycle.
type ReminderService interface {
	Create(ctx context.Context, rem *domain.Reminder) error
	ListByUser(ctx context.Context, userID int64) ([]domain.Reminder, error)
	Cancel(ctx context.Context, userID int64, id uuid.UUID) error
	Remove(ctx context.Context, userID int64, id uuid.UUID) error
	Pause(ctx context.Context, userID int64, id uuid.UUID, pause bool) error
}

// UserService manages users.
type UserService interface {
	GetOrCreate(ctx context.Context, userID int64) (*domain.User, error)
	SetTZ(ctx context.Context, userID int64, tz string) error
}

type Handler struct {
	reminders ReminderService
	users     UserService
	dialogs   DialogStore
	parser    nlu.Parser
	log       *slog.Logger
}

func NewHandler(
	reminders ReminderService,
	users UserService,
	dialogs DialogStore,
	parser nlu.Parser,
	log *slog.Logger,
) *Handler {
	return &Handler{
		reminders: reminders,
		users:     users,
		dialogs:   dialogs,
		parser:    parser,
		log:       log,
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
	bot.Handle("/tz", h.handleTZ)
	bot.Handle(tele.OnText, h.handleText)
	bot.Handle("\fconfirm_yes", h.handleConfirmYes)
	bot.Handle("\fconfirm_no", h.handleConfirmNo)
}

func (h *Handler) handleStart(c tele.Context) error {
	ctx := context.Background()
	if _, err := h.users.GetOrCreate(ctx, c.Sender().ID); err != nil {
		h.log.Error("getorcreate user", "err", err)
	}
	return c.Send(msgWelcome, tele.ModeMarkdown)
}

func (h *Handler) handleHelp(c tele.Context) error {
	return c.Send(msgHelp, tele.ModeMarkdown)
}

func (h *Handler) handleList(c tele.Context) error {
	ctx := context.Background()
	rems, err := h.reminders.ListByUser(ctx, c.Sender().ID)
	if err != nil {
		return c.Send("Ошибка при получении списка напоминаний.")
	}
	if len(rems) == 0 {
		return c.Send("У вас нет активных напоминаний.")
	}
	var sb strings.Builder
	sb.WriteString("*Ваши напоминания:*\n\n")
	for i, r := range rems {
		sb.WriteString(fmt.Sprintf("%d. %s \\[%s\\]\n`/cancel %s`\n`/remove %s`\n\n",
			i+1, escapeMarkdown(r.RawText), string(r.Status), r.ID.String(), r.ID.String()))
	}
	return c.Send(sb.String(), tele.ModeMarkdown)
}

func (h *Handler) handleCancel(c tele.Context) error {
	ctx := context.Background()
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
	ctx := context.Background()
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

func (h *Handler) handlePause(c tele.Context) error {
	return h.setPause(c, true)
}

func (h *Handler) handleResume(c tele.Context) error {
	return h.setPause(c, false)
}

func (h *Handler) setPause(c tele.Context, pause bool) error {
	ctx := context.Background()
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
	ctx := context.Background()
	tz := strings.TrimSpace(c.Message().Payload)
	if tz == "" {
		u, _ := h.users.GetOrCreate(ctx, c.Sender().ID)
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

func (h *Handler) handleText(c tele.Context) error {
	ctx := context.Background()
	userID := c.Sender().ID
	text := c.Text()

	dialog, err := h.dialogs.Get(ctx, userID)
	if err != nil {
		h.log.Error("dialog get", "err", err)
		return c.Send("Внутренняя ошибка. Попробуйте снова.")
	}

	switch dialog.State {
	case domain.DialogAwaitConfirm:
		// Пользователь набрал текст вместо нажатия кнопки — переспросим.
		return c.Send("Пожалуйста, используйте кнопки ниже.")

	case domain.DialogAwaitField:
		return h.handleFieldInput(ctx, c, dialog, text)

	default:
		return h.startParsing(ctx, c, userID, text)
	}
}

func (h *Handler) startParsing(ctx context.Context, c tele.Context, userID int64, text string) error {
	if _, err := h.users.GetOrCreate(ctx, userID); err != nil {
		h.log.Warn("getorcreate user", "err", err)
	}

	result, err := h.parser.Parse(ctx, text)
	if err != nil {
		h.log.Error("parse failed", "err", err)
		return c.Send("Не удалось распознать напоминание. Попробуйте переформулировать.")
	}

	// Ask clarification if fields are missing.
	if len(result.Missing) > 0 && result.Confidence < 0.7 {
		ctxData := &DialogContext{
			RawText:    text,
			ParsedSpec: mustMarshal(result.Spec),
			Confidence: result.Confidence,
			Missing:    result.Missing,
			FieldName:  result.Missing[0],
		}
		ctxJSON, _ := encodeContext(ctxData)
		_ = h.dialogs.Set(ctx, &domain.Dialog{
			UserID:  userID,
			State:   domain.DialogAwaitField,
			Context: ctxJSON,
		})
		return c.Send(fmt.Sprintf("Уточните: %s", fieldPrompt(result.Missing[0])))
	}

	// Show confirmation.
	return h.askConfirmation(ctx, c, userID, text, result)
}

func (h *Handler) askConfirmation(ctx context.Context, c tele.Context, userID int64, rawText string, result *nlu.ParseResult) error {
	ctxData := &DialogContext{
		RawText:    rawText,
		ParsedSpec: mustMarshal(result.Spec),
		Confidence: result.Confidence,
	}
	ctxJSON, _ := encodeContext(ctxData)
	_ = h.dialogs.Set(ctx, &domain.Dialog{
		UserID:  userID,
		State:   domain.DialogAwaitConfirm,
		Context: ctxJSON,
	})

	confirmMsg := fmt.Sprintf("*Создать напоминание?*\n\n%s", formatSpec(result.Spec))
	menu := &tele.ReplyMarkup{}
	menu.Inline(
		menu.Row(
			menu.Data("✅ Да, создать", "confirm_yes"),
			menu.Data("✏️ Исправить", "confirm_no"),
		),
	)
	return c.Send(confirmMsg, menu, tele.ModeMarkdown)
}

func (h *Handler) handleConfirmYes(c tele.Context) error {
	ctx := context.Background()
	userID := c.Sender().ID

	dialog, err := h.dialogs.Get(ctx, userID)
	if err != nil || dialog.State != domain.DialogAwaitConfirm {
		return c.Respond(&tele.CallbackResponse{Text: "Сессия истекла. Начните заново."})
	}
	dc, err := decodeContext(dialog.Context)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Ошибка данных."})
	}

	var spec domain.Spec
	if err := json.Unmarshal(dc.ParsedSpec, &spec); err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Ошибка данных."})
	}

	rem := buildReminder(userID, dc.RawText, spec)
	if err := h.reminders.Create(ctx, rem); err != nil {
		h.log.Error("create reminder", "err", err)
		return c.Respond(&tele.CallbackResponse{Text: "Ошибка сохранения."})
	}

	_ = h.dialogs.Reset(ctx, userID)
	_ = c.Respond(&tele.CallbackResponse{})
	return c.Edit("✅ Напоминание создано!")
}

func (h *Handler) handleConfirmNo(c tele.Context) error {
	ctx := context.Background()
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
	result, err := h.parser.Parse(ctx, combined)
	if err != nil {
		return c.Send("Не удалось распознать. Попробуйте ещё раз.")
	}
	return h.askConfirmation(ctx, c, c.Sender().ID, combined, result)
}

func buildReminder(userID int64, rawText string, spec domain.Spec) *domain.Reminder {
	rem := &domain.Reminder{
		UserID:  userID,
		RawText: rawText,
		Spec:    spec,
		Status:  domain.StatusActive,
	}
	switch spec.Trigger {
	case domain.TriggerAnchor, domain.TriggerThreshold, domain.TriggerDigest:
		rem.Kind = domain.KindConditional
	default:
		if spec.Trigger == "" {
			rem.Kind = domain.KindAbsolute
		}
	}
	return rem
}

func formatSpec(spec *domain.Spec) string {
	if spec == nil {
		return "(пусто)"
	}
	var sb strings.Builder
	if spec.Event.Title != "" {
		sb.WriteString("📌 *" + escapeMarkdown(spec.Event.Title) + "*\n")
	}
	if spec.Message != "" {
		sb.WriteString(escapeMarkdown(spec.Message) + "\n")
	}
	if spec.Trigger != "" {
		sb.WriteString(fmt.Sprintf("Тип: `%s`\n", spec.Trigger))
	}
	return sb.String()
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

func escapeMarkdown(s string) string {
	replacer := strings.NewReplacer(
		"_", "\\_", "*", "\\*", "[", "\\[", "]", "\\]",
		"(", "\\(", ")", "\\)", "~", "\\~", "`", "\\`",
		">", "\\>", "#", "\\#", "+", "\\+", "-", "\\-",
		"=", "\\=", "|", "\\|", "{", "\\{", "}", "\\}",
		".", "\\.", "!", "\\!",
	)
	return replacer.Replace(s)
}

func mustMarshal(v interface{}) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

const msgWelcome = `*Привет! Я бот напоминаний.*

Просто напишите что-нибудь вроде:
• «напомни 25 декабря в 10:00 поздравить маму»
• «каждый будний день в 9:00 напоминай выпить таблетку»
• «уведоми за 3 часа до КВН на Первом канале»

/help — справка
/list — список напоминаний`

const msgHelp = `*Команды:*

/list — список активных напоминаний
/cancel <id> — отменить напоминание
/remove <id> — удалить напоминание без возможности восстановления
/pause <id> — приостановить
/resume <id> — возобновить
/tz <зона> — установить часовой пояс (например, Europe/Moscow)
/help — эта справка

*Примеры напоминаний:*
• «напомни завтра в 9:00 позвонить маме»
• «каждый понедельник в 8:30 напоминай про совещание»
• «уведоми за 3 часа до КВН на Первом»
• «вот ссылка на товар — уведоми при снижении цены»
• «каждый день в 9:00 — 5 дешёвых билетов СПб→Калининград на месяц вперёд»`
