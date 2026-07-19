package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/nlu"
	tele "gopkg.in/telebot.v3"
)

const (
	menuNew      = "➕ Новое напоминание"
	menuList     = "📋 Мои напоминания"
	menuToday    = "📅 Сегодня"
	menuSettings = "⚙️ Настройки"
	menuHelp     = "❓ Помощь"
)

func (h *Handler) handleMenuText(c tele.Context, text string) (bool, error) {
	text = strings.TrimSpace(text)
	if text != menuNew && text != menuList && text != menuToday && text != menuSettings && text != menuHelp {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	_ = h.dialogs.Reset(ctx, c.Sender().ID)
	switch text {
	case menuNew:
		return true, c.Send("Опишите, о чём и когда вам напомнить.", mainMenu())
	case menuList:
		return true, h.handleList(c)
	case menuToday:
		return true, h.handleToday(c)
	case menuSettings:
		return true, h.showSettings(c, false)
	case menuHelp:
		return true, h.handleHelp(c)
	}
	return false, nil
}

func (h *Handler) handleToday(c tele.Context) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	loc, _, err := h.loadUserLocation(ctx, c.Sender().ID)
	if err != nil {
		return c.Send("Не удалось получить настройки времени.")
	}
	reminders, err := h.reminders.ListByUser(ctx, c.Sender().ID)
	if err != nil {
		return c.Send("Ошибка при получении списка напоминаний.")
	}
	now := time.Now().In(loc)
	today := make([]domain.Reminder, 0, len(reminders))
	for _, reminder := range reminders {
		if reminder.NextEvalAt != nil && sameLocalDate(reminder.NextEvalAt.In(loc), now) {
			today = append(today, reminder)
		}
	}
	if len(today) == 0 {
		return c.Send("На сегодня напоминаний нет.", mainMenu())
	}
	return c.Send(renderReminderCard(today[0], loc), reminderCardMarkup(today, 0), tele.ModeMarkdownV2)
}

func (h *Handler) handleCallback(c tele.Context) error {
	if c.Callback() == nil {
		return nil
	}
	command, err := decodeCallback(c.Callback().Data)
	if err != nil {
		return c.Respond(&tele.CallbackResponse{Text: "Кнопка устарела. Откройте список снова."})
	}
	switch command.Entity {
	case "reminder":
		return h.handleReminderCallback(c, command)
	case "draft":
		return h.handleDraftCallback(c, command)
	case "notification":
		return h.handleNotificationCallback(c, command)
	case "settings":
		return h.handleSettingsCallback(c, command)
	default:
		return c.Respond(&tele.CallbackResponse{Text: "Кнопка больше недоступна."})
	}
}

func (h *Handler) handleReminderCallback(c tele.Context, command callbackCommand) error {
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	userID := c.Sender().ID
	switch command.Action {
	case "noop":
		return c.Respond(&tele.CallbackResponse{})
	case "view":
		_ = c.Respond(&tele.CallbackResponse{})
		return h.editReminderCard(ctx, c, command.ID)
	case "run":
		_ = c.Respond(&tele.CallbackResponse{Text: "Запускаю…"})
		return h.runNow(c, command.ID)
	case "pause", "resume":
		pause := command.Action == "pause"
		if err := h.reminders.Pause(ctx, userID, command.ID, pause); err != nil {
			return callbackUnavailable(c)
		}
		_ = c.Respond(&tele.CallbackResponse{Text: map[bool]string{true: "Приостановлено", false: "Возобновлено"}[pause]})
		return h.editReminderCard(ctx, c, command.ID)
	case "finish":
		if err := h.reminders.Finish(ctx, userID, command.ID); err != nil {
			return callbackUnavailable(c)
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "Завершено"})
		return c.Edit("✅ Напоминание завершено.")
	case "duplicate":
		user, err := h.users.GetOrCreate(ctx, userID)
		if err != nil {
			return callbackUnavailable(c)
		}
		duplicate, err := h.reminders.Duplicate(ctx, userID, command.ID, time.Now(), user.TZ)
		if err != nil {
			return callbackUnavailable(c)
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "Копия создана"})
		loc, _ := time.LoadLocation(user.TZ)
		return c.Send(renderReminderCard(*duplicate, loc), reminderCardMarkup([]domain.Reminder{*duplicate}, 0), tele.ModeMarkdownV2)
	case "delete":
		if _, err := h.reminders.Get(ctx, userID, command.ID); err != nil {
			return callbackUnavailable(c)
		}
		markup := &tele.ReplyMarkup{}
		markup.Inline(tele.Row{
			uiButton("Да, удалить", "reminder", "delete_confirm", command.ID),
			uiButton("Отмена", "reminder", "view", command.ID),
		})
		_ = c.Respond(&tele.CallbackResponse{})
		return c.Edit("Удалить напоминание без возможности восстановления?", markup)
	case "delete_confirm":
		if err := h.reminders.Remove(ctx, userID, command.ID); err != nil {
			return callbackUnavailable(c)
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "Удалено"})
		return c.Edit("🗑 Напоминание удалено.")
	case "edit":
		if _, err := h.reminders.Get(ctx, userID, command.ID); err != nil {
			return callbackUnavailable(c)
		}
		_ = c.Respond(&tele.CallbackResponse{})
		return c.Edit("Что изменить?", reminderEditMarkup(command.ID))
	case "edit_text", "edit_date", "edit_time", "edit_repeat", "edit_condition":
		return h.beginReminderEdit(ctx, c, command.ID, strings.TrimPrefix(command.Action, "edit_"))
	default:
		return callbackUnavailable(c)
	}
}

func (h *Handler) editReminderCard(ctx context.Context, c tele.Context, id uuid.UUID) error {
	reminders, err := h.reminders.ListByUser(ctx, c.Sender().ID)
	if err != nil {
		return callbackUnavailable(c)
	}
	index := -1
	for i := range reminders {
		if reminders[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return callbackUnavailable(c)
	}
	loc, _, err := h.loadUserLocation(ctx, c.Sender().ID)
	if err != nil {
		return callbackUnavailable(c)
	}
	return c.Edit(renderReminderCard(reminders[index], loc), reminderCardMarkup(reminders, index), tele.ModeMarkdownV2)
}

func reminderEditMarkup(id uuid.UUID) *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	markup.Inline(
		tele.Row{uiButton("📝 Текст", "reminder", "edit_text", id), uiButton("📅 Дата", "reminder", "edit_date", id)},
		tele.Row{uiButton("🕐 Время", "reminder", "edit_time", id), uiButton("🔁 Повторение", "reminder", "edit_repeat", id)},
		tele.Row{uiButton("🎯 Условие", "reminder", "edit_condition", id)},
		tele.Row{uiButton("← Назад", "reminder", "view", id)},
	)
	return markup
}

func (h *Handler) beginReminderEdit(ctx context.Context, c tele.Context, id uuid.UUID, field string) error {
	reminder, err := h.reminders.Get(ctx, c.Sender().ID, id)
	if err != nil {
		return callbackUnavailable(c)
	}
	user, err := h.users.GetOrCreate(ctx, c.Sender().ID)
	if err != nil {
		return callbackUnavailable(c)
	}
	var fireAt *string
	if reminder.NextEvalAt != nil {
		value := reminder.NextEvalAt.Format(time.RFC3339)
		fireAt = &value
	}
	dc := &DialogContext{
		Mode: "reminder", ReminderID: id.String(), Version: reminder.Version,
		RawText: reminder.RawText, Kind: reminder.Kind, ParsedSpec: mustMarshal(&reminder.Spec),
		EvalCron: reminder.EvalCron, FireAt: fireAt, UserTZ: user.TZ,
		FieldName: field, CreatedAt: time.Now(),
	}
	encoded, err := encodeContext(dc)
	if err != nil {
		return callbackUnavailable(c)
	}
	if err := h.dialogs.Set(ctx, &domain.Dialog{UserID: c.Sender().ID, State: domain.DialogAwaitEdit, Context: encoded}); err != nil {
		return callbackUnavailable(c)
	}
	_ = c.Respond(&tele.CallbackResponse{})
	return c.Send(editFieldPrompt(field))
}

func (h *Handler) handleDraftCallback(c tele.Context, command callbackCommand) error {
	switch command.Action {
	case "create":
		return h.handleConfirmYes(c)
	case "cancel":
		return h.handleConfirmNo(c)
	case "edit_text", "edit_date", "edit_time", "edit_repeat", "edit_condition":
		ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
		defer cancel()
		dialog, err := h.dialogs.Get(ctx, c.Sender().ID)
		if err != nil || dialog.State != domain.DialogAwaitConfirm {
			return callbackUnavailable(c)
		}
		dc, err := decodeContext(dialog.Context)
		if err != nil {
			return callbackUnavailable(c)
		}
		dc.Mode = "create"
		dc.FieldName = strings.TrimPrefix(command.Action, "edit_")
		dc.CreatedAt = time.Now()
		encoded, err := encodeContext(dc)
		if err != nil || h.dialogs.Set(ctx, &domain.Dialog{UserID: c.Sender().ID, State: domain.DialogAwaitEdit, Context: encoded}) != nil {
			return callbackUnavailable(c)
		}
		_ = c.Respond(&tele.CallbackResponse{})
		return c.Send(editFieldPrompt(dc.FieldName))
	default:
		return callbackUnavailable(c)
	}
}

func editFieldPrompt(field string) string {
	switch field {
	case "text":
		return "Отправьте новый текст напоминания."
	case "date":
		return "Отправьте новую дату: сегодня, завтра, ДД.ММ.ГГГГ или ГГГГ-ММ-ДД."
	case "time":
		return "Отправьте новое время в формате ЧЧ:ММ."
	case "repeat":
		return "Отправьте: нет, ежедневно, по будням, еженедельно — или cron из 5 полей."
	case "condition":
		return "Опишите только новое условие, например: «цена ниже 5000 рублей»."
	default:
		return "Отправьте новое значение."
	}
}

func (h *Handler) handleEditFieldInput(ctx context.Context, c tele.Context, dialog *domain.Dialog, input string) error {
	dc, err := decodeContext(dialog.Context)
	if err != nil || (!dc.CreatedAt.IsZero() && time.Since(dc.CreatedAt) > dialogTTL) {
		_ = h.dialogs.Reset(ctx, c.Sender().ID)
		return c.Send("Сессия редактирования истекла. Откройте карточку снова.", mainMenu())
	}
	if dc.Mode == "settings" {
		return h.applySettingsInput(ctx, c, dc, input)
	}
	loc, _, err := h.loadUserLocation(ctx, c.Sender().ID)
	if err != nil {
		return c.Send("Не удалось получить часовой пояс.")
	}
	if dc.FieldName == "condition" {
		if err := h.applyConditionInput(ctx, dc, input, loc); err != nil {
			return c.Send("Не удалось распознать условие. Например: «цена ниже 5000 рублей».")
		}
	} else if err := applyDraftField(dc, input, loc, time.Now()); err != nil {
		return c.Send("Некорректное значение: " + err.Error())
	}
	if dc.Mode == "reminder" {
		return h.saveReminderEdit(ctx, c, dc)
	}
	encoded, err := encodeContext(dc)
	if err != nil || h.dialogs.Set(ctx, &domain.Dialog{UserID: c.Sender().ID, State: domain.DialogAwaitConfirm, Context: encoded}) != nil {
		return c.Send("Не удалось сохранить черновик. Попробуйте снова.")
	}
	return h.sendDraftPreview(ctx, c, dc)
}

func (h *Handler) applyConditionInput(ctx context.Context, dc *DialogContext, input string, loc *time.Location) error {
	result, err := h.parser.Parse(ctx, dc.RawText+"; "+input, loc)
	if err != nil || result == nil || result.Spec == nil || result.Spec.Condition == nil {
		return domain.ErrInvalidSpec
	}
	var spec domain.Spec
	if err := json.Unmarshal(dc.ParsedSpec, &spec); err != nil {
		return err
	}
	spec.Condition = result.Spec.Condition
	spec.Target = result.Spec.Target
	spec.Direction = result.Spec.Direction
	if result.Spec.Currency != "" {
		spec.Currency = result.Spec.Currency
	}
	dc.ParsedSpec = mustMarshal(&spec)
	dc.RawText += "; " + input
	return nil
}

func applyDraftField(dc *DialogContext, input string, loc *time.Location, now time.Time) error {
	input = strings.TrimSpace(input)
	if input == "" {
		return errors.New("значение не может быть пустым")
	}
	var spec domain.Spec
	if err := json.Unmarshal(dc.ParsedSpec, &spec); err != nil {
		return errors.New("черновик повреждён")
	}
	switch dc.FieldName {
	case "text":
		oldMessage := spec.Message
		spec.Message = input
		if spec.Event.Title == "" || spec.Event.Title == oldMessage {
			spec.Event.Title = input
		}
		dc.RawText = input
	case "date", "time":
		base := now.In(loc).Add(time.Hour)
		if dc.FireAt != nil {
			if parsed, err := time.Parse(time.RFC3339, *dc.FireAt); err == nil {
				base = parsed.In(loc)
			}
		}
		if dc.FieldName == "date" {
			date, err := parseEditDate(input, now.In(loc))
			if err != nil {
				return err
			}
			base = time.Date(date.Year(), date.Month(), date.Day(), base.Hour(), base.Minute(), 0, 0, loc)
		} else {
			clock, err := time.Parse("15:04", input)
			if err != nil {
				return errors.New("ожидается время ЧЧ:ММ")
			}
			base = time.Date(base.Year(), base.Month(), base.Day(), clock.Hour(), clock.Minute(), 0, 0, loc)
			if dc.FireAt == nil && !base.After(now.In(loc)) {
				base = base.AddDate(0, 0, 1)
			}
		}
		value := base.Format(time.RFC3339)
		dc.FireAt = &value
		if dc.EvalCron != "" {
			fields := strings.Fields(dc.EvalCron)
			if len(fields) == 5 {
				fields[0], fields[1] = strconv.Itoa(base.Minute()), strconv.Itoa(base.Hour())
				dc.EvalCron = strings.Join(fields, " ")
			}
		}
	case "repeat":
		clock := now.In(loc)
		if dc.FireAt != nil {
			if parsed, err := time.Parse(time.RFC3339, *dc.FireAt); err == nil {
				clock = parsed.In(loc)
			}
		}
		switch strings.ToLower(input) {
		case "нет", "none", "без повторения":
			dc.EvalCron = ""
			dc.Kind = domain.KindAbsolute
			if dc.FireAt == nil || !clock.After(now.In(loc)) {
				next := now.In(loc).Add(time.Hour)
				value := next.Format(time.RFC3339)
				dc.FireAt = &value
			}
		case "ежедневно", "каждый день", "daily":
			dc.EvalCron = fmt.Sprintf("%d %d * * *", clock.Minute(), clock.Hour())
			dc.Kind = domain.KindRecurring
		case "по будням", "будни", "weekdays":
			dc.EvalCron = fmt.Sprintf("%d %d * * 1-5", clock.Minute(), clock.Hour())
			dc.Kind = domain.KindRecurring
		case "еженедельно", "каждую неделю", "weekly":
			dc.EvalCron = fmt.Sprintf("%d %d * * %d", clock.Minute(), clock.Hour(), int(clock.Weekday()))
			dc.Kind = domain.KindRecurring
		default:
			if _, err := parseCron(input); err != nil {
				return errors.New("неизвестное повторение")
			}
			dc.EvalCron = input
			dc.Kind = domain.KindRecurring
		}
	default:
		return errors.New("неизвестное поле")
	}
	dc.ParsedSpec = mustMarshal(&spec)
	return nil
}

func parseEditDate(input string, now time.Time) (time.Time, error) {
	switch strings.ToLower(strings.TrimSpace(input)) {
	case "сегодня", "today":
		return now, nil
	case "завтра", "tomorrow":
		return now.AddDate(0, 0, 1), nil
	}
	for _, layout := range []string{"02.01.2006", "2006-01-02"} {
		if value, err := time.ParseInLocation(layout, input, now.Location()); err == nil {
			return value, nil
		}
	}
	return time.Time{}, errors.New("ожидается дата ДД.ММ.ГГГГ или ГГГГ-ММ-ДД")
}

func (h *Handler) saveReminderEdit(ctx context.Context, c tele.Context, dc *DialogContext) error {
	id, err := uuid.Parse(dc.ReminderID)
	if err != nil {
		return c.Send("Черновик редактирования повреждён.")
	}
	current, err := h.reminders.Get(ctx, c.Sender().ID, id)
	if err != nil {
		return c.Send("Напоминание больше недоступно.")
	}
	var spec domain.Spec
	if err := json.Unmarshal(dc.ParsedSpec, &spec); err != nil {
		return c.Send("Черновик редактирования повреждён.")
	}
	current.RawText = dc.RawText
	current.Spec = spec
	current.Kind = dc.Kind
	current.EvalCron = dc.EvalCron
	if dc.EvalCron != "" {
		next, err := nextCronAt(dc.EvalCron, time.Now(), dc.UserTZ)
		if err != nil {
			return c.Send("Не удалось рассчитать следующее повторение.")
		}
		current.NextEvalAt = &next
	} else if current.Kind == domain.KindConditional {
		next := time.Now().UTC()
		current.NextEvalAt = &next
	} else if dc.FireAt != nil {
		next, err := time.Parse(time.RFC3339, *dc.FireAt)
		if err != nil || !next.After(time.Now()) {
			return c.Send("Дата и время должны быть в будущем.")
		}
		current.NextEvalAt = &next
	}
	if err := h.reminders.Update(ctx, current, dc.Version); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			_ = h.dialogs.Reset(ctx, c.Sender().ID)
			return c.Send("Напоминание уже изменилось в другом окне. Откройте карточку заново.")
		}
		return c.Send("Не удалось сохранить изменения.")
	}
	_ = h.dialogs.Reset(ctx, c.Sender().ID)
	loc, _, _ := h.loadUserLocation(ctx, c.Sender().ID)
	return c.Send("✅ Изменения сохранены.\n\n"+renderReminderCard(*current, loc), reminderCardMarkup([]domain.Reminder{*current}, 0), tele.ModeMarkdownV2)
}

func (h *Handler) sendDraftPreview(ctx context.Context, c tele.Context, dc *DialogContext) error {
	var spec domain.Spec
	if err := json.Unmarshal(dc.ParsedSpec, &spec); err != nil {
		return c.Send("Черновик повреждён. Начните заново.")
	}
	result := &nlu.ParseResult{Kind: dc.Kind, Spec: &spec, Confidence: dc.Confidence, EvalCron: dc.EvalCron, FireAt: dc.FireAt}
	return c.Send("*Создать напоминание?*\n\n"+h.formatConfirmSpec(ctx, result), draftPreviewMarkup(), tele.ModeMarkdownV2)
}

func (h *Handler) handleNotificationCallback(c tele.Context, command callbackCommand) error {
	if h.notificationActions == nil {
		return callbackUnavailable(c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	result, err := h.notificationActions.Apply(ctx, c.Sender().ID, command.ID, command.Action, time.Now())
	if err != nil {
		return callbackUnavailable(c)
	}
	_ = c.Respond(&tele.CallbackResponse{Text: result.Message})
	if result.RunNow {
		return h.runNow(c, result.ReminderID)
	}
	return nil
}

func (h *Handler) showSettings(c tele.Context, edit bool) error {
	if h.preferences == nil {
		return c.Send("Настройки пока недоступны.")
	}
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	user, err := h.users.GetOrCreate(ctx, c.Sender().ID)
	if err != nil {
		return c.Send("Не удалось загрузить настройки.")
	}
	prefs, err := h.preferences.Get(ctx, c.Sender().ID)
	if err != nil {
		return c.Send("Не удалось загрузить настройки.")
	}
	quiet := "выключены"
	if prefs.QuietStart != "" {
		quiet = prefs.QuietStart + "–" + prefs.QuietEnd
	}
	text := fmt.Sprintf("⚙️ *Настройки*\n\n🌍 Часовой пояс: `%s`\n🔕 Тихие часы: `%s`\n🌅 Утром: `%s`\n⏰ Отложить по умолчанию: `%d мин`",
		escapeMarkdown(user.TZ), escapeMarkdown(quiet), escapeMarkdown(prefs.MorningTime), prefs.DefaultSnoozeMinutes)
	markup := settingsMarkup()
	if edit {
		return c.Edit(text, markup, tele.ModeMarkdownV2)
	}
	return c.Send(text, markup, tele.ModeMarkdownV2)
}

func settingsMarkup() *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	id := uuid.Nil
	markup.Inline(
		tele.Row{uiButton("🌍 Часовой пояс", "settings", "timezone", id)},
		tele.Row{uiButton("🔕 Тихие часы", "settings", "quiet", id)},
		tele.Row{uiButton("🌅 Время «утром»", "settings", "morning", id)},
		tele.Row{uiButton("⏰ Стандартный snooze", "settings", "snooze", id)},
	)
	return markup
}

func (h *Handler) handleSettingsCallback(c tele.Context, command callbackCommand) error {
	if h.preferences == nil {
		return callbackUnavailable(c)
	}
	ctx, cancel := context.WithTimeout(context.Background(), handlerTimeout)
	defer cancel()
	if command.Action == "view" {
		_ = c.Respond(&tele.CallbackResponse{})
		return h.showSettings(c, true)
	}
	if command.Action != "timezone" && command.Action != "quiet" && command.Action != "morning" && command.Action != "snooze" {
		return callbackUnavailable(c)
	}
	dc := &DialogContext{Mode: "settings", FieldName: command.Action, CreatedAt: time.Now()}
	encoded, _ := encodeContext(dc)
	if err := h.dialogs.Set(ctx, &domain.Dialog{UserID: c.Sender().ID, State: domain.DialogAwaitEdit, Context: encoded}); err != nil {
		return callbackUnavailable(c)
	}
	prompt := map[string]string{
		"timezone": "Отправьте IANA-часовой пояс, например Europe/Moscow.",
		"quiet":    "Отправьте тихие часы как ЧЧ:ММ-ЧЧ:ММ или «выкл».",
		"morning":  "Отправьте время «утром» в формате ЧЧ:ММ.",
		"snooze":   "Отправьте стандартное время отсрочки в минутах (1–10080).",
	}[command.Action]
	_ = c.Respond(&tele.CallbackResponse{})
	return c.Send(prompt)
}

func (h *Handler) applySettingsInput(ctx context.Context, c tele.Context, dc *DialogContext, input string) error {
	input = strings.TrimSpace(input)
	if dc.FieldName == "timezone" {
		if _, err := time.LoadLocation(input); err != nil {
			return c.Send("Неизвестный часовой пояс. Пример: Europe/Moscow.")
		}
		if err := h.users.SetTZ(ctx, c.Sender().ID, input); err != nil {
			return c.Send("Не удалось сохранить часовой пояс.")
		}
	} else {
		prefs, err := h.preferences.Get(ctx, c.Sender().ID)
		if err != nil {
			return c.Send("Не удалось загрузить настройки.")
		}
		switch dc.FieldName {
		case "quiet":
			if strings.EqualFold(input, "выкл") || strings.EqualFold(input, "off") {
				prefs.QuietStart, prefs.QuietEnd = "", ""
			} else {
				parts := strings.Split(input, "-")
				if len(parts) != 2 {
					return c.Send("Используйте формат ЧЧ:ММ-ЧЧ:ММ или «выкл».")
				}
				prefs.QuietStart, prefs.QuietEnd = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
			}
		case "morning":
			prefs.MorningTime = input
		case "snooze":
			minutes, err := strconv.Atoi(input)
			if err != nil {
				return c.Send("Введите целое число минут.")
			}
			prefs.DefaultSnoozeMinutes = minutes
		}
		if err := h.preferences.Update(ctx, *prefs); err != nil {
			return c.Send("Некорректное значение: " + err.Error())
		}
	}
	_ = h.dialogs.Reset(ctx, c.Sender().ID)
	return c.Send("✅ Настройки сохранены.", mainMenu())
}

func callbackUnavailable(c tele.Context) error {
	return c.Respond(&tele.CallbackResponse{Text: "Объект недоступен или кнопка устарела."})
}

// QuietModeService resolves whether a Telegram notification should be silent.
type QuietModeService struct {
	users       UserService
	preferences UserPreferencesService
}

func NewQuietModeService(users UserService, preferences UserPreferencesService) *QuietModeService {
	return &QuietModeService{users: users, preferences: preferences}
}

func (s *QuietModeService) IsQuiet(ctx context.Context, userID int64, at time.Time) (bool, error) {
	user, err := s.users.GetOrCreate(ctx, userID)
	if err != nil {
		return false, err
	}
	prefs, err := s.preferences.Get(ctx, userID)
	if err != nil {
		return false, err
	}
	if prefs.QuietStart == "" || prefs.QuietEnd == "" {
		return false, nil
	}
	loc, err := time.LoadLocation(user.TZ)
	if err != nil {
		return false, err
	}
	start, err := time.Parse("15:04", prefs.QuietStart)
	if err != nil {
		return false, err
	}
	end, err := time.Parse("15:04", prefs.QuietEnd)
	if err != nil {
		return false, err
	}
	local := at.In(loc)
	minute := local.Hour()*60 + local.Minute()
	startMinute := start.Hour()*60 + start.Minute()
	endMinute := end.Hour()*60 + end.Minute()
	if startMinute == endMinute {
		return true, nil
	}
	if startMinute < endMinute {
		return minute >= startMinute && minute < endMinute, nil
	}
	return minute >= startMinute || minute < endMinute, nil
}
