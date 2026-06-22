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
