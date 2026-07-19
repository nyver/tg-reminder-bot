package telegram

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
	tele "gopkg.in/telebot.v3"
)

func renderReminderCard(reminder domain.Reminder, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	var body strings.Builder
	body.WriteString(cardHeading(reminder) + " *" + escapeMarkdown(truncateRunes(listReminderTitle(reminder), listReminderTitleLimit)) + "*\n\n")
	body.WriteString(statusIcon(reminder.Status) + " " + escapeMarkdown(cardStatusLabel(reminder.Status)) + "\n")
	if reminder.NextEvalAt != nil {
		body.WriteString("Следующее: " + escapeMarkdown(formatCardTime(*reminder.NextEvalAt, loc)) + "\n")
	}
	body.WriteString("Повторение: " + escapeMarkdown(cardRepeat(reminder)) + "\n")
	body.WriteString("Часовой пояс: " + escapeMarkdown(loc.String()) + "\n")

	switch {
	case reminder.Spec.Trigger == domain.TriggerThreshold && reminder.Spec.Event.Type == "price":
		body.WriteString("Условие: " + escapeMarkdown(metricConditionDescription(&reminder.Spec)) + "\n")
		if host := feedHost(reminder.Spec.Event.Params["url"]); host != "" {
			body.WriteString("Источник: " + escapeMarkdown(host) + "\n")
		}
	case reminder.Spec.Trigger == domain.TriggerThreshold && reminder.Spec.Event.Type == "exchange_rate":
		body.WriteString("Условие: " + escapeMarkdown(metricConditionDescription(&reminder.Spec)) + "\n")
	case reminder.Spec.Event.Type == "weather":
		if location := reminder.Spec.Event.Params["location"]; location != "" {
			body.WriteString("Место: " + escapeMarkdown(location) + "\n")
		}
		if reminder.Spec.Condition != nil {
			body.WriteString("Условие: " + escapeMarkdown(metricConditionDescription(&reminder.Spec)) + "\n")
		}
	case reminder.Spec.Trigger == domain.TriggerDigest && reminder.Spec.Event.Type == "rss":
		feeds := splitFeedURLs(reminder.Spec.Event.Params["url"])
		body.WriteString(fmt.Sprintf("Ленты: %s\n", escapeMarkdown(feedHostsDisplay(feeds))))
		body.WriteString(fmt.Sprintf("Материалов: %d\n", orDefaultInt(reminder.Spec.TopN, rssDefaultTopN)))
	case reminder.Spec.Event.Type == "tv_program":
		if channel := reminder.Spec.Event.Params["channel"]; channel != "" {
			body.WriteString("Канал: " + escapeMarkdown(channel) + "\n")
		}
		if reminder.Spec.LeadTime.Duration > 0 {
			body.WriteString("Заранее: " + escapeMarkdown(formatDurationRu(reminder.Spec.LeadTime.Duration)) + "\n")
		}
	}
	return strings.TrimSpace(body.String())
}

func cardHeading(reminder domain.Reminder) string {
	switch {
	case reminder.Spec.Event.Type == "price":
		return "💰"
	case reminder.Spec.Event.Type == "exchange_rate":
		return "💱"
	case reminder.Spec.Event.Type == "weather":
		return "🌦"
	case reminder.Spec.Event.Type == "rss":
		return "📰"
	case reminder.Spec.Event.Type == "tv_program":
		return "📺"
	case reminder.EvalCron != "":
		return "🔁"
	case reminder.Spec.Message != "" || reminder.RawText != "":
		return "⏰"
	default:
		return "📌"
	}
}

func statusIcon(status domain.Status) string {
	switch status {
	case domain.StatusActive:
		return "🟢"
	case domain.StatusPaused:
		return "⏸"
	case domain.StatusDone:
		return "✅"
	case domain.StatusCancelled:
		return "🚫"
	default:
		return "⚠️"
	}
}

func cardStatusLabel(status domain.Status) string {
	switch status {
	case domain.StatusActive:
		return "Активно"
	case domain.StatusPaused:
		return "На паузе"
	case domain.StatusDone:
		return "Завершено"
	case domain.StatusCancelled:
		return "Отменено"
	default:
		return "Ошибка"
	}
}

func cardRepeat(reminder domain.Reminder) string {
	if reminder.EvalCron == "" {
		return "нет"
	}
	return formatCronLineRu(reminder.EvalCron)
}

func formatCardTime(value time.Time, loc *time.Location) string {
	local := value.In(loc)
	now := time.Now().In(loc)
	switch {
	case sameLocalDate(local, now):
		return "сегодня " + local.Format("15:04")
	case sameLocalDate(local, now.AddDate(0, 0, 1)):
		return "завтра " + local.Format("15:04")
	default:
		return local.Format("02.01.2006 15:04")
	}
}

func sameLocalDate(a, b time.Time) bool {
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func reminderCardMarkup(reminders []domain.Reminder, index int) *tele.ReplyMarkup {
	if index < 0 || index >= len(reminders) {
		return nil
	}
	reminder := reminders[index]
	markup := &tele.ReplyMarkup{}
	pauseAction, pauseText := "pause", "⏸ Пауза"
	if reminder.Status == domain.StatusPaused {
		pauseAction, pauseText = "resume", "▶ Возобновить"
	}
	rows := []tele.Row{
		{uiButton("▶ Запустить сейчас", "reminder", "run", reminder.ID), uiButton(pauseText, "reminder", pauseAction, reminder.ID)},
		{uiButton("✏️ Изменить", "reminder", "edit", reminder.ID), uiButton("📋 Дублировать", "reminder", "duplicate", reminder.ID)},
		{uiButton("✅ Завершить", "reminder", "finish", reminder.ID), uiButton("🗑 Удалить", "reminder", "delete", reminder.ID)},
	}
	if len(reminders) > 1 {
		previous := reminders[(index-1+len(reminders))%len(reminders)].ID
		next := reminders[(index+1)%len(reminders)].ID
		rows = append(rows, tele.Row{
			uiButton("←", "reminder", "view", previous),
			uiButton(fmt.Sprintf("%d/%d", index+1, len(reminders)), "reminder", "noop", reminder.ID),
			uiButton("→", "reminder", "view", next),
		})
	}
	markup.Inline(rows...)
	return markup
}

func uiButton(text, entity, action string, id uuid.UUID) tele.Btn {
	data, err := encodeCallback(entity, action, id)
	if err != nil {
		return tele.Btn{Text: text}
	}
	return tele.Btn{Text: text, Data: data}
}

func draftPreviewMarkup() *tele.ReplyMarkup {
	markup := &tele.ReplyMarkup{}
	zero := uuid.Nil
	markup.Inline(
		tele.Row{uiButton("✅ Создать", "draft", "create", zero), uiButton("✏️ Текст", "draft", "edit_text", zero)},
		tele.Row{uiButton("📅 Дата", "draft", "edit_date", zero), uiButton("🕐 Время", "draft", "edit_time", zero)},
		tele.Row{uiButton("🔁 Повторение", "draft", "edit_repeat", zero), uiButton("🎯 Условие", "draft", "edit_condition", zero)},
		tele.Row{uiButton("Отмена", "draft", "cancel", zero)},
	)
	return markup
}
