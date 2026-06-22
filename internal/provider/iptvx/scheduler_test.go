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
