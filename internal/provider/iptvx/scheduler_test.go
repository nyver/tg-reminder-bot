package iptvx

import (
	"context"
	"testing"
	"time"
)

func TestSchedulerQueryScheduleWithoutChannel(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	store := &memStore{
		channels: []EPGChannel{
			{ID: "one", DisplayName: "Первый канал"},
			{ID: "sts", DisplayName: "СТС"},
		},
		progs: []EPGProgramme{
			{ChannelID: "one", Title: "КВН. Высшая лига", StartsAt: from.Add(time.Hour)},
			{ChannelID: "sts", Title: "КВН", StartsAt: from.Add(2 * time.Hour)},
			{ChannelID: "sts", Title: "Новости", StartsAt: from.Add(3 * time.Hour)},
		},
	}

	shows, err := NewScheduler(store).QuerySchedule(context.Background(), "КВН", "", from, from.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) != 2 {
		t.Fatalf("got %d shows, want 2: %+v", len(shows), shows)
	}
	if shows[0].Channel != "Первый канал" || shows[1].Channel != "СТС" {
		t.Fatalf("unexpected channels: %+v", shows)
	}
}

func TestSchedulerChannelDaySchedule(t *testing.T) {
	t.Parallel()

	loc, _ := time.LoadLocation("Europe/Moscow")
	day := time.Date(2026, 6, 22, 0, 0, 0, 0, loc)
	store := &memStore{
		channels: []EPGChannel{
			{ID: "one", DisplayName: "Первый канал"},
			{ID: "sts", DisplayName: "СТС"},
		},
		progs: []EPGProgramme{
			{ChannelID: "one", Title: "Доброе утро", StartsAt: day.Add(6 * time.Hour)},
			{ChannelID: "one", Title: "Новости", StartsAt: day.Add(9 * time.Hour)},
			{ChannelID: "sts", Title: "Другой канал", StartsAt: day.Add(8 * time.Hour)},
			// Outside the day window — should not appear.
			{ChannelID: "one", Title: "Вчера", StartsAt: day.Add(-time.Hour)},
		},
	}

	from := day
	to := day.Add(24 * time.Hour)
	chName, shows, err := NewScheduler(store).ChannelDaySchedule(context.Background(), "Первый", from, to)
	if err != nil {
		t.Fatal(err)
	}
	if chName != "Первый канал" {
		t.Fatalf("chName = %q, want Первый канал", chName)
	}
	if len(shows) != 2 {
		t.Fatalf("got %d shows, want 2: %+v", len(shows), shows)
	}
	if shows[0].Title != "Доброе утро" || shows[1].Title != "Новости" {
		t.Fatalf("unexpected titles: %+v", shows)
	}
	for _, s := range shows {
		if s.Channel != "Первый канал" {
			t.Fatalf("show channel = %q, want Первый канал", s.Channel)
		}
	}
}

func TestSchedulerChannelDayScheduleUnknownChannel(t *testing.T) {
	t.Parallel()
	store := &memStore{
		channels: []EPGChannel{{ID: "one", DisplayName: "Первый канал"}},
	}
	chName, shows, err := NewScheduler(store).ChannelDaySchedule(
		context.Background(), "Несуществующий канал",
		time.Now(), time.Now().Add(24*time.Hour),
	)
	if err != nil || chName != "" || shows != nil {
		t.Fatalf("expected empty result, got chName=%q shows=%v err=%v", chName, shows, err)
	}
}

func TestSchedulerQueryScheduleWithFuzzyChannel(t *testing.T) {
	t.Parallel()

	from := time.Date(2026, 6, 22, 0, 0, 0, 0, time.UTC)
	store := &memStore{
		channels: []EPGChannel{
			{ID: "one", DisplayName: "Первый канал"},
			{ID: "five", DisplayName: "Пятый канал"},
		},
		progs: []EPGProgramme{
			{ChannelID: "one", Title: "КВН", StartsAt: from.Add(time.Hour)},
			{ChannelID: "five", Title: "КВН", StartsAt: from.Add(2 * time.Hour)},
		},
	}

	shows, err := NewScheduler(store).QuerySchedule(context.Background(), "КВН", "Первый", from, from.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(shows) != 1 || shows[0].Channel != "Первый канал" {
		t.Fatalf("unexpected shows: %+v", shows)
	}
}
