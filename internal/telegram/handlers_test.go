package telegram

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/nlu"
	"github.com/nyver2k/remindertgbot/internal/provider"
	"github.com/nyver2k/remindertgbot/internal/scheduler"
	tele "gopkg.in/telebot.v3"
)

func TestWriteTVShowsGroupsByChannelAndDay(t *testing.T) {
	loc := time.FixedZone("MSK", 3*60*60)
	shows := []provider.TVShowtime{
		{Title: "Следствие вели: Лабиринт (2017)", Channel: "TEAM Crime24 HD", StartsAt: time.Date(2026, 6, 22, 17, 30, 0, 0, time.UTC), EndsAt: time.Date(2026, 6, 22, 18, 11, 0, 0, time.UTC)},
		{Title: "Другая серия", Channel: "TEAM Crime24 HD", StartsAt: time.Date(2026, 6, 23, 17, 30, 0, 0, time.UTC)},
		{Title: "Фильм!", Channel: "Первый канал", StartsAt: time.Date(2026, 6, 22, 19, 0, 0, 0, time.UTC)},
	}

	var sb strings.Builder
	writeTVShows(&sb, shows, "", loc)

	want := "*TEAM Crime24 HD*\n" +
		"_пн, 22 июн_\n" +
		"  `20:30–21:11` — Следствие вели: Лабиринт \\(2017\\)\n" +
		"_вт, 23 июн_\n" +
		"  `20:30` — Другая серия\n\n" +
		"*Первый канал*\n" +
		"_пн, 22 июн_\n" +
		"  `22:00` — Фильм\\!\n"
	if got := sb.String(); got != want {
		t.Fatalf("writeTVShows() =\n%q\nwant:\n%q", got, want)
	}
}

func TestBuildConditionalReminderSchedulesImmediateEvaluation(t *testing.T) {
	now := time.Date(2026, 6, 21, 17, 30, 0, 0, time.UTC)
	result := &nlu.ParseResult{
		Kind: domain.KindConditional,
		Spec: &domain.Spec{
			Trigger: domain.TriggerAnchor,
			Event: domain.EventSpec{
				Type: "tv_program", Title: "Этот день победы",
				Params: map[string]string{"channel": "Первый канал"},
			},
		},
		Confidence: 0.95,
	}

	rem, err := buildReminder(1, "raw", result, now, "")
	if err != nil {
		t.Fatal(err)
	}
	if rem.Kind != domain.KindConditional || rem.EvalCron != defaultConditionalCron {
		t.Fatalf("unexpected reminder: %+v", rem)
	}
	if rem.NextEvalAt == nil || !rem.NextEvalAt.Equal(now) {
		t.Fatalf("next_eval_at = %v", rem.NextEvalAt)
	}
}

func TestBuildTravelDigestReminder(t *testing.T) {
	now := time.Date(2026, 6, 21, 17, 30, 0, 0, time.UTC)
	result := &nlu.ParseResult{
		Kind: domain.KindConditional,
		Spec: &domain.Spec{
			Trigger:     domain.TriggerDigest,
			TopN:        5,
			HorizonDays: 30,
			Event: domain.EventSpec{
				Type: "travel",
				Params: map[string]string{
					"origin":      "SPB",
					"destination": "KGD",
					"modes":       "air,rail",
				},
			},
		},
		Confidence: 0.95,
		EvalCron:   "0 9 * * *",
	}

	rem, err := buildReminder(1, "raw", result, now, "")
	if err != nil {
		t.Fatal(err)
	}
	if rem.Kind != domain.KindConditional || rem.EvalCron != "0 9 * * *" {
		t.Fatalf("unexpected reminder: %+v", rem)
	}
	if rem.NextEvalAt == nil {
		t.Fatal("expected NextEvalAt to be set")
	}
	if !rem.NextEvalAt.After(now) {
		t.Fatalf("NextEvalAt = %v, want after creation time %v", rem.NextEvalAt, now)
	}
	if got := rem.NextEvalAt.In(time.FixedZone("MSK", 3*60*60)).Format("15:04"); got != "09:00" {
		t.Fatalf("NextEvalAt local time = %s, want 09:00", got)
	}
}

func TestWriteListReminderRSSIsCompact(t *testing.T) {
	id := uuid.MustParse("04e914b7-adb0-48ad-ae87-690f6550751a")
	rem := domain.Reminder{
		ID:      id,
		RawText: "каждый день в 18:00 создай дайджест новостей на основе https://openai.com/news/rss.xml, https://blog.google/technology/ai/rss/",
		Status:  domain.StatusActive,
		Spec: domain.Spec{
			Trigger: domain.TriggerDigest,
			TopN:    7,
			Event: domain.EventSpec{
				Type: "rss",
				Params: map[string]string{
					"url": "https://openai.com/news/rss.xml,https://blog.google/technology/ai/rss/,https://www.theverge.com/rss/ai-artificial-intelligence/index.xml",
				},
			},
		},
		EvalCron: "0 18 * * *",
	}

	var sb strings.Builder
	(&Handler{}).writeListReminder(context.Background(), &sb, 1, rem, time.UTC)
	got := sb.String()

	if strings.Contains(got, "https://openai.com/news/rss.xml") || strings.Contains(got, rem.RawText) {
		t.Fatalf("list item should not repeat long RSS URLs/raw text, got:\n%s", got)
	}
	for _, want := range []string{
		"*1\\. RSS дайджест: openai\\.com, blog\\.google, theverge\\.com*",
		"Статус: `активно`",
		"• Рассылка: `18:00` · топ\\-7",
		"• Ленты \\(3\\): openai\\.com, blog\\.google, theverge\\.com",
		"• Действия:",
		"`/run 04e914b7-adb0-48ad-ae87-690f6550751a`",
		"`/pause 04e914b7-adb0-48ad-ae87-690f6550751a`",
		"`/remove 04e914b7-adb0-48ad-ae87-690f6550751a`",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("list item missing %q in:\n%s", want, got)
		}
	}
}

func TestBuildListMessagesSplitsLongOutput(t *testing.T) {
	rems := make([]domain.Reminder, 30)
	for i := range rems {
		rems[i] = domain.Reminder{
			ID:      uuid.New(),
			RawText: strings.Repeat("длинное напоминание ", 30),
			Status:  domain.StatusActive,
		}
	}

	messages := (&Handler{}).buildListMessages(context.Background(), rems, time.UTC)
	if len(messages) < 2 {
		t.Fatalf("expected multiple list messages, got %d", len(messages))
	}
	joined := strings.Join(messages, "")
	for i, message := range messages {
		if got := runeLen(message); got > telegramListMessageLimit {
			t.Fatalf("message %d has %d runes, limit is %d", i, got, telegramListMessageLimit)
		}
	}
	for _, rem := range rems {
		if !strings.Contains(joined, rem.ID.String()) {
			t.Fatalf("list output is missing reminder %s", rem.ID)
		}
	}
}

type staticPriceHistory struct {
	observation *domain.Observation
}

func (s staticPriceHistory) Last(context.Context, uuid.UUID) (*domain.Observation, error) {
	return s.observation, nil
}

func TestWriteListPriceDoesNotRepeatTitle(t *testing.T) {
	rem := domain.Reminder{
		ID:     uuid.New(),
		Status: domain.StatusActive,
		Spec: domain.Spec{
			Trigger: domain.TriggerThreshold,
			Event: domain.EventSpec{
				Type:  "price",
				Title: "Laptop Pro",
			},
		},
	}
	h := &Handler{history: staticPriceHistory{observation: &domain.Observation{
		Title:      "Laptop Pro",
		Value:      100_000,
		Currency:   "RUB",
		ObservedAt: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC),
	}}}

	var sb strings.Builder
	h.writeListReminder(context.Background(), &sb, 1, rem, time.UTC)
	if got := strings.Count(sb.String(), "Laptop Pro"); got != 1 {
		t.Fatalf("title appears %d times in:\n%s", got, sb.String())
	}
}

func TestBuildAbsoluteReminderPreservesFireAt(t *testing.T) {
	fireAt := "2026-06-22T09:00:00+03:00"
	result := &nlu.ParseResult{
		Kind:       domain.KindAbsolute,
		Spec:       &domain.Spec{Message: "Позвонить маме"},
		Confidence: 0.95,
		FireAt:     &fireAt,
	}

	rem, err := buildReminder(1, "raw", result, time.Now(), "")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := time.Parse(time.RFC3339, fireAt)
	if rem.Kind != domain.KindAbsolute || rem.NextEvalAt == nil || !rem.NextEvalAt.Equal(want) {
		t.Fatalf("unexpected reminder: %+v", rem)
	}
}

func TestParseChannelAndDate(t *testing.T) {
	loc := time.FixedZone("MSK", 3*60*60)
	today := time.Date(2026, 6, 22, 0, 0, 0, 0, loc)
	now := today.Add(12 * time.Hour)

	cases := []struct {
		input   string
		channel string
		day     time.Time
	}{
		{"Первый канал", "Первый канал", today},
		{"Первый канал сегодня", "Первый канал", today},
		{"Первый канал завтра", "Первый канал", today.AddDate(0, 0, 1)},
		{"Первый канал послезавтра", "Первый канал", today.AddDate(0, 0, 2)},
		{"Первый канал 25.06", "Первый канал", time.Date(2026, 6, 25, 0, 0, 0, 0, loc)},
		{"Первый канал 01.01.2027", "Первый канал", time.Date(2027, 1, 1, 0, 0, 0, 0, loc)},
		{"СТС", "СТС", today},
	}
	for _, tc := range cases {
		ch, d := parseChannelAndDate(tc.input, now, loc)
		if ch != tc.channel {
			t.Errorf("parseChannelAndDate(%q) channel = %q, want %q", tc.input, ch, tc.channel)
		}
		if !d.Equal(tc.day) {
			t.Errorf("parseChannelAndDate(%q) day = %v, want %v", tc.input, d, tc.day)
		}
	}
}

func TestFormatFireLine(t *testing.T) {
	fireAt := "2026-06-23T10:00:00+03:00"
	result := &nlu.ParseResult{FireAt: &fireAt}
	got := formatFireLine(result)
	if got != "⏰ 23 июн в 10:00\n" {
		t.Fatalf("formatFireLine = %q", got)
	}
}

func TestFormatFireLineNil(t *testing.T) {
	if got := formatFireLine(nil); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
	if got := formatFireLine(&nlu.ParseResult{}); got != "" {
		t.Fatalf("expected empty for no FireAt, got %q", got)
	}
}

func TestFormatCronLineRu(t *testing.T) {
	cases := []struct{ expr, want string }{
		{"0 10 * * *", "каждый день в 10:00"},
		{"30 8 * * 1", "каждый пн в 08:30"},
		{"0 9 * * 1-5", "пн–пт в 09:00"},
		{"0 9 * * 6", "каждую сб в 09:00"},
		{"0 9 1 * *", ""}, // specific dom — unsupported
		{"*/5 * * * *", "каждые 5 минут"},
		{"*/30 * * * *", "каждые 30 минут"},
		{"0 * * * *", "каждый час"},
		{"0 */2 * * *", "каждые 2 часа"},
		{"0 */12 * * *", "каждые 12 часов"},
	}
	for _, tc := range cases {
		if got := formatCronLineRu(tc.expr); got != tc.want {
			t.Errorf("formatCronLineRu(%q) = %q, want %q", tc.expr, got, tc.want)
		}
	}
}

func TestFilterEndedShows(t *testing.T) {
	now := time.Date(2026, 6, 22, 15, 0, 0, 0, time.UTC)
	shows := []provider.TVShowtime{
		{Title: "Уже закончилась", StartsAt: now.Add(-2 * time.Hour), EndsAt: now.Add(-1 * time.Hour)},
		{Title: "Идёт сейчас", StartsAt: now.Add(-30 * time.Minute), EndsAt: now.Add(30 * time.Minute)},
		{Title: "Будущая", StartsAt: now.Add(time.Hour), EndsAt: now.Add(2 * time.Hour)},
		{Title: "Без времени окончания", StartsAt: now.Add(-time.Hour)},
	}

	got := filterEndedShows(shows, now)
	if len(got) != 3 {
		t.Fatalf("got %d shows, want 3: %+v", len(got), got)
	}
	if got[0].Title != "Идёт сейчас" || got[1].Title != "Будущая" || got[2].Title != "Без времени окончания" {
		t.Fatalf("unexpected titles: %v", got)
	}
}

func TestHandleTVArgParsing(t *testing.T) {
	cases := []struct {
		payload string
		title   string
		channel string
	}{
		{"КВН", "КВН", ""},
		{"КВН | Первый канал", "КВН", "Первый канал"},
		{"| Первый канал", "", "Первый канал"},
		{"|Первый канал", "", "Первый канал"},
		{"КВН | первый", "КВН", "первый"},
	}
	for _, tc := range cases {
		args := tc.payload
		title, channel := args, ""
		if parts := strings.SplitN(args, "|", 2); len(parts) == 2 {
			title = strings.TrimSpace(parts[0])
			channel = strings.TrimSpace(parts[1])
		}
		if title != tc.title || channel != tc.channel {
			t.Errorf("payload=%q: got title=%q channel=%q, want title=%q channel=%q",
				tc.payload, title, channel, tc.title, tc.channel)
		}
	}
}

func TestStripMarkdownV2DetectsMarker(t *testing.T) {
	text, opts := stripMarkdownV2(scheduler.MarkdownV2Prefix + "*bold*")
	if text != "*bold*" {
		t.Fatalf("text = %q, want marker stripped", text)
	}
	if len(opts) != 1 || opts[0] != tele.ModeMarkdownV2 {
		t.Fatalf("opts = %+v, want [tele.ModeMarkdownV2]", opts)
	}
}

func TestStripMarkdownV2LeavesPlainTextUnchanged(t *testing.T) {
	const plain = "just a plain reminder text"
	text, opts := stripMarkdownV2(plain)
	if text != plain {
		t.Fatalf("text = %q, want unchanged", text)
	}
	if opts != nil {
		t.Fatalf("opts = %+v, want nil for plain text", opts)
	}
}

func TestValidateParseResultRejectsEmptySpec(t *testing.T) {
	if err := validateParseResult(&nlu.ParseResult{Spec: &domain.Spec{}}); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateParseResultRejectsRSSWithoutURL(t *testing.T) {
	result := &nlu.ParseResult{
		Confidence: 0.97,
		Spec: &domain.Spec{
			Trigger: domain.TriggerDigest,
			Event:   domain.EventSpec{Type: "rss"},
		},
	}
	if err := validateParseResult(result); err == nil {
		t.Fatal("expected validation error for rss reminder without url")
	}
}

// TestBuildReminderRSSDigestFromFreeText mirrors the NLU flow for a message
// like "каждый день в 18:00 создай дайджест новостей на основе <ссылка>":
// the fast-path parser recognizes it as event.type=rss, and buildReminder
// must turn it into a conditional reminder on the daily cron it extracted.
func TestBuildReminderRSSDigestFromFreeText(t *testing.T) {
	now := time.Date(2026, 6, 21, 17, 30, 0, 0, time.UTC)
	result := &nlu.ParseResult{
		Kind:       domain.KindConditional,
		EvalCron:   "0 18 * * *",
		Confidence: 0.97,
		Spec: &domain.Spec{
			Trigger: domain.TriggerDigest,
			Message: "RSS-дайджест новостей",
			Event: domain.EventSpec{
				Type:   "rss",
				Params: map[string]string{"url": "https://lenta.ru/rss"},
			},
		},
	}

	rem, err := buildReminder(1, "каждый день в 18:00 создай дайджест новостей на основе https://lenta.ru/rss", result, now, "")
	if err != nil {
		t.Fatal(err)
	}
	if rem.Kind != domain.KindConditional {
		t.Fatalf("kind = %q, want conditional", rem.Kind)
	}
	if rem.EvalCron != "0 18 * * *" {
		t.Fatalf("eval_cron = %q, want %q", rem.EvalCron, "0 18 * * *")
	}
	if rem.Spec.Event.Type != "rss" || rem.Spec.Event.Params["url"] != "https://lenta.ru/rss" {
		t.Fatalf("unexpected event spec: %+v", rem.Spec.Event)
	}
	if rem.NextEvalAt == nil {
		t.Fatal("expected NextEvalAt to be set")
	}
	if !rem.NextEvalAt.After(now) {
		t.Fatalf("NextEvalAt = %v, want after creation time %v", rem.NextEvalAt, now)
	}
	if got := rem.NextEvalAt.In(time.FixedZone("MSK", 3*60*60)).Format("15:04"); got != "18:00" {
		t.Fatalf("NextEvalAt local time = %s, want 18:00", got)
	}
}

// TestBuildAbsoluteNonAnchorReminderPreservesFireAt ensures that absolute
// reminders WITHOUT an anchor trigger (e.g. "напомни завтра в 9:00 позвонить")
// still use the user-specified fire time for NextEvalAt.
func TestBuildAbsoluteNonAnchorReminderPreservesFireAt(t *testing.T) {
	now := time.Date(2026, 6, 21, 17, 30, 0, 0, time.UTC)
	fireAt := "2026-06-22T09:00:00+03:00"
	result := &nlu.ParseResult{
		Kind:       domain.KindAbsolute,
		Spec:       &domain.Spec{Message: "Позвонить маме"}, // no trigger
		Confidence: 0.95,
		FireAt:     &fireAt,
	}

	rem, err := buildReminder(1, "raw", result, now, "")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := time.Parse(time.RFC3339, fireAt)
	if rem.Kind != domain.KindAbsolute {
		t.Fatalf("kind = %q", rem.Kind)
	}
	if rem.NextEvalAt == nil {
		t.Fatal("expected NextEvalAt to be set")
	}
	if !rem.NextEvalAt.Equal(want) {
		t.Fatalf("NextEvalAt = %v, want %v", rem.NextEvalAt, want)
	}
}

func TestParseRSSArgsDefaults(t *testing.T) {
	feedURLs, hour, minute, topN, err := parseRSSArgs("https://lenta.ru/rss")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(feedURLs) != 1 || feedURLs[0] != "https://lenta.ru/rss" {
		t.Errorf("feedURLs = %v", feedURLs)
	}
	if hour != rssDefaultCronHour || minute != rssDefaultCronMinute {
		t.Errorf("time = %02d:%02d, want default %02d:%02d", hour, minute, rssDefaultCronHour, rssDefaultCronMinute)
	}
	if topN != rssDefaultTopN {
		t.Errorf("topN = %d, want default %d", topN, rssDefaultTopN)
	}
}

func TestParseRSSArgsCustomTimeAndTopN(t *testing.T) {
	feedURLs, hour, minute, topN, err := parseRSSArgs("https://lenta.ru/rss | 08:30 | 10")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(feedURLs) != 1 || feedURLs[0] != "https://lenta.ru/rss" || hour != 8 || minute != 30 || topN != 10 {
		t.Errorf("got urls=%v hour=%d minute=%d topN=%d", feedURLs, hour, minute, topN)
	}
}

// TestParseRSSArgsMultipleURLs verifies that a comma-separated list of feed
// URLs in the first pipe-segment produces a combined digest reminder.
func TestParseRSSArgsMultipleURLs(t *testing.T) {
	feedURLs, _, _, _, err := parseRSSArgs("https://lenta.ru/rss, https://habr.com/rss |09:30")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"https://lenta.ru/rss", "https://habr.com/rss"}
	if len(feedURLs) != len(want) || feedURLs[0] != want[0] || feedURLs[1] != want[1] {
		t.Errorf("feedURLs = %v, want %v", feedURLs, want)
	}
}

func TestParseRSSArgsRejectsTooManyURLs(t *testing.T) {
	var sb strings.Builder
	for i := 0; i < rssMaxURLs+1; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString("https://example.com/feed")
	}
	if _, _, _, _, err := parseRSSArgs(sb.String()); err == nil {
		t.Error("expected error for more than rssMaxURLs feeds")
	}
}

func TestFeedHostsDisplay(t *testing.T) {
	cases := []struct {
		urls []string
		want string
	}{
		{[]string{"https://www.lenta.ru/rss"}, "lenta.ru"},
		{[]string{"https://lenta.ru/rss", "https://habr.com/rss"}, "lenta.ru, habr.com"},
		{
			[]string{"https://a.com/rss", "https://b.com/rss", "https://c.com/rss", "https://d.com/rss"},
			"a.com, b.com, c.com +1 ещё",
		},
	}
	for _, tc := range cases {
		if got := feedHostsDisplay(tc.urls); got != tc.want {
			t.Errorf("feedHostsDisplay(%v) = %q, want %q", tc.urls, got, tc.want)
		}
	}
}

func TestParseRSSArgsRejectsInvalidURL(t *testing.T) {
	cases := []string{"", "not-a-url", "ftp://lenta.ru/rss", "   "}
	for _, payload := range cases {
		if _, _, _, _, err := parseRSSArgs(payload); err == nil {
			t.Errorf("payload %q: expected error, got nil", payload)
		}
	}
}

func TestParseRSSArgsRejectsTopNOutOfRange(t *testing.T) {
	if _, _, _, _, err := parseRSSArgs("https://lenta.ru/rss | 09:00 | 0"); err == nil {
		t.Error("expected error for topN=0")
	}
	if _, _, _, _, err := parseRSSArgs("https://lenta.ru/rss | 09:00 | 21"); err == nil {
		t.Error("expected error for topN=21")
	}
}

func TestParseRSSArgsRejectsUnknownExtraParam(t *testing.T) {
	if _, _, _, _, err := parseRSSArgs("https://lenta.ru/rss | garbage"); err == nil {
		t.Error("expected error for unrecognized extra parameter")
	}
}

func TestFeedHostStripsWWW(t *testing.T) {
	cases := map[string]string{
		"https://www.lenta.ru/rss":        "lenta.ru",
		"https://lenta.ru/rss":            "lenta.ru",
		"http://sub.example.com/feed.xml": "sub.example.com",
	}
	for url, want := range cases {
		if got := feedHost(url); got != want {
			t.Errorf("feedHost(%q) = %q, want %q", url, got, want)
		}
	}
}

func TestCronToHHMM(t *testing.T) {
	if got := cronToHHMM("30 8 * * *"); got != "08:30" {
		t.Errorf("cronToHHMM = %q, want 08:30", got)
	}
	if got := cronToHHMM("not a cron"); got != "not a cron" {
		t.Errorf("cronToHHMM fallback = %q", got)
	}
}

func TestConfirmationDecision(t *testing.T) {
	cases := []struct {
		text string
		want confirmDecision
	}{
		{"да", confirmDecisionYes},
		{" Да! ", confirmDecisionYes},
		{"создать", confirmDecisionYes},
		{"нет", confirmDecisionNo},
		{"исправить", confirmDecisionNo},
		{"не понял", confirmDecisionUnknown},
	}
	for _, tc := range cases {
		if got := confirmationDecision(tc.text); got != tc.want {
			t.Errorf("confirmationDecision(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestMainMenuContainsQuickCommands(t *testing.T) {
	menu := mainMenu()
	if menu == nil {
		t.Fatal("expected menu")
	}
	if !menu.ResizeKeyboard || !menu.IsPersistent {
		t.Fatalf("unexpected menu flags: resize=%v persistent=%v", menu.ResizeKeyboard, menu.IsPersistent)
	}
	if len(menu.ReplyKeyboard) != 3 {
		t.Fatalf("rows = %d, want 3", len(menu.ReplyKeyboard))
	}
	got := []string{
		menu.ReplyKeyboard[0][0].Text,
		menu.ReplyKeyboard[0][1].Text,
		menu.ReplyKeyboard[1][0].Text,
		menu.ReplyKeyboard[1][1].Text,
		menu.ReplyKeyboard[2][0].Text,
	}
	want := []string{"/list", "/help", "/tv", "/rss", "/tz"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("button %d = %q, want %q", i, got[i], want[i])
		}
	}
}
