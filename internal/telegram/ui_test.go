package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/delivery"
	"github.com/nyver2k/remindertgbot/internal/domain"
	tele "gopkg.in/telebot.v3"
)

func TestCallbackProtocolRoundTripAndLimits(t *testing.T) {
	id := uuid.New()
	data, err := encodeCallback("notification", "snooze_morning", id)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) > callbackMaxBytes || strings.Contains(data, id.String()) {
		t.Fatalf("unsafe callback data %q", data)
	}
	command, err := decodeCallback(data)
	if err != nil {
		t.Fatal(err)
	}
	if command.Entity != "notification" || command.Action != "snooze_morning" || command.ID != id {
		t.Fatalf("decoded command = %+v", command)
	}
	for _, invalid := range []string{"v2:reminder:view:x", "v1:bad-entity:view:x", "v1:reminder:view:not-base64", strings.Repeat("x", 65)} {
		if _, err := decodeCallback(invalid); !errors.Is(err, errInvalidCallback) {
			t.Errorf("decodeCallback(%q) error = %v", invalid, err)
		}
	}
}

func TestRenderReminderCardsByType(t *testing.T) {
	next := time.Date(2030, 7, 20, 9, 0, 0, 0, time.UTC)
	target := int64(500000)
	cases := []struct {
		name string
		rem  domain.Reminder
		want []string
	}{
		{"regular", domain.Reminder{RawText: "Позвонить маме", Spec: domain.Spec{Message: "Позвонить маме"}, Status: domain.StatusActive, NextEvalAt: &next}, []string{"⏰", "Активно", "Повторение: нет"}},
		{"recurring", domain.Reminder{RawText: "Вода", EvalCron: "0 9 * * *", Spec: domain.Spec{Message: "Вода"}, Status: domain.StatusPaused, NextEvalAt: &next}, []string{"🔁", "На паузе", "каждый день"}},
		{"price", domain.Reminder{Spec: domain.Spec{Trigger: domain.TriggerThreshold, Event: domain.EventSpec{Type: "price", Title: "Ноутбук", Params: map[string]string{"url": "https://shop.example/p"}}, Condition: &domain.Condition{Operator: domain.ConditionOperatorLTE, Target: &target}}, Status: domain.StatusActive}, []string{"💰", "Условие", "shop\\.example"}},
		{"rss", domain.Reminder{Spec: domain.Spec{Trigger: domain.TriggerDigest, Event: domain.EventSpec{Type: "rss", Params: map[string]string{"url": "https://news.example/rss"}}, TopN: 5}, Status: domain.StatusActive}, []string{"📰", "Ленты", "Материалов: 5"}},
		{"weather", domain.Reminder{Spec: domain.Spec{Trigger: domain.TriggerAnchor, Event: domain.EventSpec{Type: "weather", Params: map[string]string{"location": "Казань"}}}, Status: domain.StatusActive}, []string{"🌦", "Казань"}},
		{"tv", domain.Reminder{Spec: domain.Spec{Trigger: domain.TriggerAnchor, Event: domain.EventSpec{Type: "tv_program", Title: "КВН", Params: map[string]string{"channel": "Первый"}}}, Status: domain.StatusActive}, []string{"📺", "Канал"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := renderReminderCard(tc.rem, time.UTC)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("card %q does not contain %q", got, want)
				}
			}
		})
	}
}

func TestReminderCardMarkupUsesNavigableCallbacks(t *testing.T) {
	reminders := []domain.Reminder{
		{ID: uuid.New(), Status: domain.StatusActive},
		{ID: uuid.New(), Status: domain.StatusPaused},
	}
	markup := reminderCardMarkup(reminders, 0)
	if markup == nil || len(markup.InlineKeyboard) != 4 {
		t.Fatalf("markup = %+v", markup)
	}
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if button.Data == "" {
				t.Fatalf("button without callback data: %+v", button)
			}
			if _, err := decodeCallback(button.Data); err != nil {
				t.Fatalf("invalid button callback %q: %v", button.Data, err)
			}
		}
	}
}

func TestAuxiliaryInlineMarkupsUseVersionedCallbacks(t *testing.T) {
	markups := []*tele.ReplyMarkup{draftPreviewMarkup(), reminderEditMarkup(uuid.New()), settingsMarkup()}
	for _, markup := range markups {
		if markup == nil || len(markup.InlineKeyboard) == 0 {
			t.Fatal("expected inline keyboard")
		}
		for _, row := range markup.InlineKeyboard {
			for _, button := range row {
				if _, err := decodeCallback(button.Data); err != nil {
					t.Fatalf("callback %q: %v", button.Data, err)
				}
			}
		}
	}
	for _, field := range []string{"text", "date", "time", "repeat", "condition", "other"} {
		if prompt := editFieldPrompt(field); prompt == "" {
			t.Fatalf("empty prompt for %s", field)
		}
	}
}

func TestOutboundMarkupEncodesTransportActions(t *testing.T) {
	id := uuid.New()
	markup := outboundMarkup([][]delivery.OutboundAction{{
		{Text: "Done", Entity: "notification", Action: "done", ID: id},
	}})
	if markup == nil || len(markup.InlineKeyboard) != 1 || len(markup.InlineKeyboard[0]) != 1 {
		t.Fatalf("markup = %+v", markup)
	}
	command, err := decodeCallback(markup.InlineKeyboard[0][0].Data)
	if err != nil || command.ID != id || command.Action != "done" {
		t.Fatalf("command=%+v err=%v", command, err)
	}
	if outboundMarkup(nil) != nil {
		t.Fatal("empty actions must not create markup")
	}
}

func TestApplyDraftFieldUpdatesOnlyRequestedSchedulePart(t *testing.T) {
	fireAt := "2030-07-20T09:00:00Z"
	dc := &DialogContext{
		ParsedSpec: mustMarshal(&domain.Spec{Message: "old"}),
		FieldName:  "time", FireAt: &fireAt, EvalCron: "0 9 * * 1-5", Kind: domain.KindRecurring,
	}
	if err := applyDraftField(dc, "14:30", time.UTC, time.Date(2030, 7, 19, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if dc.EvalCron != "30 14 * * 1-5" || dc.FireAt == nil || !strings.Contains(*dc.FireAt, "14:30") {
		t.Fatalf("updated context = %+v", dc)
	}
	dc.FieldName = "repeat"
	if err := applyDraftField(dc, "нет", time.UTC, time.Date(2030, 7, 19, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if dc.EvalCron != "" || dc.Kind != domain.KindAbsolute {
		t.Fatalf("repeat removal = %+v", dc)
	}
}

func TestApplyDraftFieldSupportsTextDateAndRepeatPresets(t *testing.T) {
	now := time.Date(2030, 7, 19, 10, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		field string
		input string
	}{
		{"text", "new text"},
		{"date", "завтра"},
		{"repeat", "ежедневно"},
		{"repeat", "по будням"},
		{"repeat", "еженедельно"},
		{"repeat", "15 8 * * 1"},
	} {
		dc := &DialogContext{
			RawText: "old", Kind: domain.KindAbsolute, FieldName: tc.field,
			ParsedSpec: mustMarshal(&domain.Spec{Message: "old", Event: domain.EventSpec{Title: "old"}}),
		}
		if err := applyDraftField(dc, tc.input, time.UTC, now); err != nil {
			t.Errorf("%s %q: %v", tc.field, tc.input, err)
		}
	}
	invalid := &DialogContext{FieldName: "repeat", ParsedSpec: mustMarshal(&domain.Spec{})}
	if err := applyDraftField(invalid, "sometimes", time.UTC, now); err == nil {
		t.Fatal("expected invalid repeat error")
	}
}

func TestParseEditDate(t *testing.T) {
	now := time.Date(2030, 7, 19, 10, 0, 0, 0, time.UTC)
	date, err := parseEditDate("завтра", now)
	if err != nil || date.Day() != 20 {
		t.Fatalf("tomorrow = %v, %v", date, err)
	}
	date, err = parseEditDate("21.07.2030", now)
	if err != nil || date.Day() != 21 {
		t.Fatalf("explicit date = %v, %v", date, err)
	}
	if _, err := parseEditDate("soon", now); err == nil {
		t.Fatal("expected invalid date error")
	}
}

type preferencesServiceStub struct {
	prefs *domain.UserPreferences
	err   error
}

func (s preferencesServiceStub) Get(context.Context, int64) (*domain.UserPreferences, error) {
	return s.prefs, s.err
}
func (s preferencesServiceStub) Update(context.Context, domain.UserPreferences) error { return s.err }

func TestQuietModeServiceSupportsOvernightRanges(t *testing.T) {
	service := NewQuietModeService(
		userServiceStub{user: &domain.User{ID: 42, TZ: "UTC"}},
		preferencesServiceStub{prefs: &domain.UserPreferences{QuietStart: "22:00", QuietEnd: "08:00"}},
	)
	for _, tc := range []struct {
		hour int
		want bool
	}{{23, true}, {7, true}, {12, false}} {
		got, err := service.IsQuiet(context.Background(), 42, time.Date(2030, 1, 1, tc.hour, 0, 0, 0, time.UTC))
		if err != nil || got != tc.want {
			t.Errorf("hour %d: quiet=%v err=%v, want %v", tc.hour, got, err, tc.want)
		}
	}
}

type preferencesRepoStub struct{ value domain.UserPreferences }

func (s *preferencesRepoStub) GetOrCreate(context.Context, int64) (*domain.UserPreferences, error) {
	copy := s.value
	return &copy, nil
}
func (s *preferencesRepoStub) Update(_ context.Context, value domain.UserPreferences) error {
	s.value = value
	return nil
}

func TestUserPreferencesServiceValidation(t *testing.T) {
	repo := &preferencesRepoStub{}
	service := NewUserPreferencesService(repo)
	valid := domain.UserPreferences{UserID: 42, MorningTime: "09:00", QuietStart: "22:00", QuietEnd: "08:00", DefaultSnoozeMinutes: 15}
	if err := service.Update(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	invalid := []domain.UserPreferences{
		{MorningTime: "morning", DefaultSnoozeMinutes: 10},
		{MorningTime: "09:00", QuietStart: "22:00", DefaultSnoozeMinutes: 10},
		{MorningTime: "09:00", DefaultSnoozeMinutes: 0},
	}
	for _, value := range invalid {
		if err := service.Update(context.Background(), value); err == nil {
			t.Errorf("expected validation error for %+v", value)
		}
	}
}

type actionNotificationStore struct {
	notification *domain.ScheduledNotification
	enqueued     *domain.ScheduledNotification
}

func (s *actionNotificationStore) Get(context.Context, uuid.UUID) (*domain.ScheduledNotification, error) {
	return s.notification, nil
}
func (s *actionNotificationStore) Enqueue(_ context.Context, notification *domain.ScheduledNotification) error {
	s.enqueued = notification
	return nil
}

type actionStoreStub struct {
	actions []domain.NotificationAction
	err     error
}

func (s *actionStoreStub) Record(_ context.Context, action *domain.NotificationAction) error {
	if s.err != nil {
		return s.err
	}
	s.actions = append(s.actions, *action)
	return nil
}

type reminderServiceStub struct{ reminder *domain.Reminder }

func (s reminderServiceStub) Create(context.Context, *domain.Reminder) error { return nil }
func (s reminderServiceStub) Get(_ context.Context, userID int64, _ uuid.UUID) (*domain.Reminder, error) {
	if s.reminder == nil || s.reminder.UserID != userID {
		return nil, domain.ErrNotFound
	}
	return s.reminder, nil
}
func (s reminderServiceStub) ListByUser(context.Context, int64) ([]domain.Reminder, error) {
	return nil, nil
}
func (s reminderServiceStub) Cancel(context.Context, int64, uuid.UUID) error        { return nil }
func (s reminderServiceStub) Remove(context.Context, int64, uuid.UUID) error        { return nil }
func (s reminderServiceStub) Pause(context.Context, int64, uuid.UUID, bool) error   { return nil }
func (s reminderServiceStub) Finish(context.Context, int64, uuid.UUID) error        { return nil }
func (s reminderServiceStub) Update(context.Context, *domain.Reminder, int64) error { return nil }
func (s reminderServiceStub) Duplicate(context.Context, int64, uuid.UUID, time.Time, string) (*domain.Reminder, error) {
	return s.reminder, nil
}

func TestNotificationActionServiceSnoozesWithParentAndAudit(t *testing.T) {
	notificationID := uuid.New()
	reminderID := uuid.New()
	notifications := &actionNotificationStore{notification: &domain.ScheduledNotification{ID: notificationID, ReminderID: reminderID, Text: "hello"}}
	actions := &actionStoreStub{}
	service := NewNotificationActionService(
		notifications, actions,
		reminderServiceStub{reminder: &domain.Reminder{ID: reminderID, UserID: 42}},
		userServiceStub{user: &domain.User{ID: 42, TZ: "UTC"}},
		preferencesServiceStub{prefs: &domain.UserPreferences{MorningTime: "09:00", DefaultSnoozeMinutes: 25}},
	)
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	result, err := service.Apply(context.Background(), 42, notificationID, "snooze_default", now)
	if err != nil {
		t.Fatal(err)
	}
	if result.ReminderID != reminderID || notifications.enqueued == nil || !notifications.enqueued.FireAt.Equal(now.Add(25*time.Minute)) {
		t.Fatalf("result=%+v notification=%+v", result, notifications.enqueued)
	}
	if notifications.enqueued.ParentNotificationID == nil || *notifications.enqueued.ParentNotificationID != notificationID || len(actions.actions) != 1 {
		t.Fatalf("parent/audit missing: %+v %+v", notifications.enqueued, actions.actions)
	}
	var payload map[string]string
	if err := json.Unmarshal(actions.actions[0].Payload, &payload); err != nil || payload["result"] == "" {
		t.Fatalf("payload = %s, err=%v", actions.actions[0].Payload, err)
	}
}

func TestNotificationActionServiceHidesForeignReminder(t *testing.T) {
	service := NewNotificationActionService(
		&actionNotificationStore{notification: &domain.ScheduledNotification{ID: uuid.New(), ReminderID: uuid.New()}},
		&actionStoreStub{}, reminderServiceStub{reminder: &domain.Reminder{UserID: 7}},
		userServiceStub{}, preferencesServiceStub{},
	)
	_, err := service.Apply(context.Background(), 42, uuid.New(), "done", time.Now())
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want not found", err)
	}
}

func TestNotificationActionServiceActionsAndSnoozeChoices(t *testing.T) {
	notificationID := uuid.New()
	reminderID := uuid.New()
	notifications := &actionNotificationStore{notification: &domain.ScheduledNotification{ID: notificationID, ReminderID: reminderID, Text: "hello"}}
	reminders := reminderServiceStub{reminder: &domain.Reminder{ID: reminderID, UserID: 42}}
	users := userServiceStub{user: &domain.User{ID: 42, TZ: "UTC"}}
	prefs := preferencesServiceStub{prefs: &domain.UserPreferences{MorningTime: "08:30", DefaultSnoozeMinutes: 15}}
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		action string
		run    bool
	}{
		{"done", false}, {"pause", false}, {"check", true}, {"repeat", true},
		{"snooze_10", false}, {"snooze_60", false}, {"snooze_morning", false},
	} {
		service := NewNotificationActionService(notifications, &actionStoreStub{}, reminders, users, prefs)
		result, err := service.Apply(context.Background(), 42, notificationID, tc.action, now)
		if err != nil || result.RunNow != tc.run || result.Message == "" {
			t.Errorf("action %s result=%+v err=%v", tc.action, result, err)
		}
	}
	service := NewNotificationActionService(notifications, &actionStoreStub{}, reminders, users, prefs)
	if _, err := service.Apply(context.Background(), 42, notificationID, "unknown", now); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unknown action error = %v", err)
	}
	duplicateService := NewNotificationActionService(notifications, &actionStoreStub{err: domain.ErrAlreadyExists}, reminders, users, prefs)
	result, err := duplicateService.Apply(context.Background(), 42, notificationID, "check", now)
	if err != nil || result.RunNow || !strings.Contains(result.Message, "уже") {
		t.Fatalf("duplicate result=%+v err=%v", result, err)
	}
}

type reminderRepositoryStub struct {
	reminder *domain.Reminder
	created  *domain.Reminder
	updated  *domain.Reminder
	version  int64
}

func (s *reminderRepositoryStub) Create(_ context.Context, reminder *domain.Reminder) error {
	reminder.ID = uuid.New()
	s.created = reminder
	return nil
}
func (s *reminderRepositoryStub) Get(context.Context, uuid.UUID) (*domain.Reminder, error) {
	if s.reminder == nil {
		return nil, domain.ErrNotFound
	}
	return s.reminder, nil
}
func (s *reminderRepositoryStub) ListByUser(context.Context, int64) ([]domain.Reminder, error) {
	return []domain.Reminder{*s.reminder}, nil
}
func (s *reminderRepositoryStub) Cancel(context.Context, int64, uuid.UUID) error { return nil }
func (s *reminderRepositoryStub) Remove(context.Context, int64, uuid.UUID) error { return nil }
func (s *reminderRepositoryStub) Pause(context.Context, int64, uuid.UUID, bool) error {
	return nil
}
func (s *reminderRepositoryStub) Finish(context.Context, int64, uuid.UUID) error { return nil }
func (s *reminderRepositoryStub) Update(_ context.Context, reminder *domain.Reminder, version int64) error {
	s.updated, s.version = reminder, version
	return nil
}

func TestSimpleReminderServiceScopesAndDuplicates(t *testing.T) {
	next := time.Date(2030, 1, 2, 9, 0, 0, 0, time.UTC)
	repo := &reminderRepositoryStub{reminder: &domain.Reminder{
		ID: uuid.New(), UserID: 42, Kind: domain.KindRecurring, RawText: "daily",
		Spec:   domain.Spec{Message: "daily", Event: domain.EventSpec{Params: map[string]string{"key": "value"}}},
		Status: domain.StatusActive, EvalCron: "0 9 * * *", NextEvalAt: &next, Version: 3,
	}}
	service := NewReminderService(repo)
	if _, err := service.Get(context.Background(), 7, repo.reminder.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("foreign get error = %v", err)
	}
	loaded, err := service.Get(context.Background(), 42, repo.reminder.ID)
	if err != nil || loaded.ID != repo.reminder.ID {
		t.Fatalf("loaded=%+v err=%v", loaded, err)
	}
	copy, err := service.Duplicate(context.Background(), 42, repo.reminder.ID, time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC), "UTC")
	if err != nil {
		t.Fatal(err)
	}
	if copy.ID == repo.reminder.ID || copy.Status != domain.StatusActive || copy.NextEvalAt == nil || !copy.NextEvalAt.Equal(next) {
		t.Fatalf("copy = %+v", copy)
	}
	copy.Spec.Event.Params["key"] = "changed"
	if repo.reminder.Spec.Event.Params["key"] != "value" {
		t.Fatal("duplicate shares mutable spec maps")
	}
	if err := service.Update(context.Background(), &domain.Reminder{}, 1); !errors.Is(err, domain.ErrInvalidSpec) {
		t.Fatalf("empty owner update error = %v", err)
	}
	valid := &domain.Reminder{UserID: 42}
	if err := service.Update(context.Background(), valid, 3); err != nil || repo.updated != valid || repo.version != 3 {
		t.Fatalf("update err=%v repo=%+v", err, repo)
	}
	if _, err := service.ListByUser(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	if err := service.Cancel(context.Background(), 42, repo.reminder.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Remove(context.Background(), 42, repo.reminder.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Pause(context.Background(), 42, repo.reminder.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := service.Finish(context.Background(), 42, repo.reminder.ID); err != nil {
		t.Fatal(err)
	}
}
