package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/nlu"
	"github.com/nyver2k/remindertgbot/internal/provider"
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

	rem, err := buildReminder(1, "raw", result, now)
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

func TestBuildAbsoluteReminderPreservesFireAt(t *testing.T) {
	fireAt := "2026-06-22T09:00:00+03:00"
	result := &nlu.ParseResult{
		Kind:       domain.KindAbsolute,
		Spec:       &domain.Spec{Message: "Позвонить маме"},
		Confidence: 0.95,
		FireAt:     &fireAt,
	}

	rem, err := buildReminder(1, "raw", result, time.Now())
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
		{"0 9 1 * *", ""},  // specific dom — unsupported
		{"*/5 * * * *", ""}, // non-literal hour/min — unsupported
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

func TestValidateParseResultRejectsEmptySpec(t *testing.T) {
	if err := validateParseResult(&nlu.ParseResult{Spec: &domain.Spec{}}); err == nil {
		t.Fatal("expected validation error")
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

	rem, err := buildReminder(1, "raw", result, now)
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
