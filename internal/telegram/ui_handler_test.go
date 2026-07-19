package telegram

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/nlu"
	tele "gopkg.in/telebot.v3"
)

type telegramContextStub struct {
	tele.Context
	sender    *tele.User
	callback  *tele.Callback
	message   *tele.Message
	text      string
	sent      []interface{}
	edited    []interface{}
	responses []*tele.CallbackResponse
}

func (c *telegramContextStub) Sender() *tele.User       { return c.sender }
func (c *telegramContextStub) Callback() *tele.Callback { return c.callback }
func (c *telegramContextStub) Message() *tele.Message   { return c.message }
func (c *telegramContextStub) Text() string             { return c.text }
func (c *telegramContextStub) Send(what interface{}, _ ...interface{}) error {
	c.sent = append(c.sent, what)
	return nil
}
func (c *telegramContextStub) Edit(what interface{}, _ ...interface{}) error {
	c.edited = append(c.edited, what)
	return nil
}
func (c *telegramContextStub) Respond(responses ...*tele.CallbackResponse) error {
	c.responses = append(c.responses, responses...)
	return nil
}

type dialogStoreStub struct{ dialog *domain.Dialog }

func (s *dialogStoreStub) Get(_ context.Context, userID int64) (*domain.Dialog, error) {
	if s.dialog == nil {
		return &domain.Dialog{UserID: userID, State: domain.DialogIdle, Context: json.RawMessage("{}")}, nil
	}
	return s.dialog, nil
}
func (s *dialogStoreStub) Set(_ context.Context, dialog *domain.Dialog) error {
	s.dialog = dialog
	return nil
}
func (s *dialogStoreStub) Reset(_ context.Context, userID int64) error {
	s.dialog = &domain.Dialog{UserID: userID, State: domain.DialogIdle, Context: json.RawMessage("{}")}
	return nil
}

type uiReminderService struct {
	reminders []domain.Reminder
	paused    bool
	finished  bool
	removed   bool
}

func (s *uiReminderService) find(userID int64, id uuid.UUID) (*domain.Reminder, error) {
	for i := range s.reminders {
		if s.reminders[i].ID == id && s.reminders[i].UserID == userID {
			return &s.reminders[i], nil
		}
	}
	return nil, domain.ErrNotFound
}
func (s *uiReminderService) Create(context.Context, *domain.Reminder) error { return nil }
func (s *uiReminderService) Get(_ context.Context, userID int64, id uuid.UUID) (*domain.Reminder, error) {
	return s.find(userID, id)
}
func (s *uiReminderService) ListByUser(_ context.Context, userID int64) ([]domain.Reminder, error) {
	result := make([]domain.Reminder, 0, len(s.reminders))
	for _, reminder := range s.reminders {
		if reminder.UserID == userID {
			result = append(result, reminder)
		}
	}
	return result, nil
}
func (s *uiReminderService) Cancel(context.Context, int64, uuid.UUID) error { return nil }
func (s *uiReminderService) Remove(_ context.Context, userID int64, id uuid.UUID) error {
	if _, err := s.find(userID, id); err != nil {
		return err
	}
	s.removed = true
	return nil
}
func (s *uiReminderService) Pause(_ context.Context, userID int64, id uuid.UUID, pause bool) error {
	if _, err := s.find(userID, id); err != nil {
		return err
	}
	s.paused = pause
	return nil
}
func (s *uiReminderService) Finish(_ context.Context, userID int64, id uuid.UUID) error {
	if _, err := s.find(userID, id); err != nil {
		return err
	}
	s.finished = true
	return nil
}
func (s *uiReminderService) Update(context.Context, *domain.Reminder, int64) error { return nil }
func (s *uiReminderService) Duplicate(_ context.Context, userID int64, id uuid.UUID, _ time.Time, _ string) (*domain.Reminder, error) {
	reminder, err := s.find(userID, id)
	if err != nil {
		return nil, err
	}
	copy := *reminder
	copy.ID = uuid.New()
	return &copy, nil
}

func newUIHandlerTestFixture() (*Handler, *uiReminderService, *dialogStoreStub, domain.Reminder) {
	next := time.Now().UTC().Add(time.Hour)
	reminder := domain.Reminder{
		ID: uuid.New(), UserID: 42, Kind: domain.KindAbsolute, RawText: "task",
		Spec: domain.Spec{Message: "task"}, Status: domain.StatusActive, NextEvalAt: &next, Version: 2,
	}
	reminders := &uiReminderService{reminders: []domain.Reminder{reminder}}
	dialogs := &dialogStoreStub{}
	handler := &Handler{
		reminders: reminders,
		users:     userServiceStub{user: &domain.User{ID: 42, TZ: "UTC"}},
		dialogs:   dialogs,
	}
	return handler, reminders, dialogs, reminder
}

func callbackContext(t *testing.T, entity, action string, id uuid.UUID) *telegramContextStub {
	t.Helper()
	data, err := encodeCallback(entity, action, id)
	if err != nil {
		t.Fatal(err)
	}
	return &telegramContextStub{sender: &tele.User{ID: 42}, callback: &tele.Callback{Data: data}}
}

func TestReminderCallbacksMutateOwnedReminder(t *testing.T) {
	for _, action := range []string{"view", "noop", "pause", "resume", "finish", "duplicate", "delete", "delete_confirm", "edit", "edit_text"} {
		t.Run(action, func(t *testing.T) {
			handler, reminders, dialogs, reminder := newUIHandlerTestFixture()
			ctx := callbackContext(t, "reminder", action, reminder.ID)
			if err := handler.handleCallback(ctx); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "pause":
				if !reminders.paused {
					t.Fatal("pause was not applied")
				}
			case "finish":
				if !reminders.finished {
					t.Fatal("finish was not applied")
				}
			case "delete_confirm":
				if !reminders.removed {
					t.Fatal("delete was not applied")
				}
			case "edit_text":
				if dialogs.dialog == nil || dialogs.dialog.State != domain.DialogAwaitEdit {
					t.Fatalf("dialog = %+v", dialogs.dialog)
				}
			}
		})
	}
}

func TestHandleCallbackRejectsMalformedAndForeignData(t *testing.T) {
	handler, _, _, reminder := newUIHandlerTestFixture()
	malformed := &telegramContextStub{sender: &tele.User{ID: 42}, callback: &tele.Callback{Data: "bad"}}
	if err := handler.handleCallback(malformed); err != nil || len(malformed.responses) != 1 {
		t.Fatalf("malformed response err=%v responses=%d", err, len(malformed.responses))
	}
	foreign := callbackContext(t, "reminder", "finish", reminder.ID)
	foreign.sender.ID = 7
	if err := handler.handleCallback(foreign); err != nil || len(foreign.responses) != 1 {
		t.Fatalf("foreign response err=%v responses=%d", err, len(foreign.responses))
	}
}

func TestMenuAndTodayHandlers(t *testing.T) {
	handler, _, dialogs, _ := newUIHandlerTestFixture()
	for _, item := range []string{menuNew, menuList, menuToday, menuHelp} {
		ctx := &telegramContextStub{sender: &tele.User{ID: 42}, text: item}
		handled, err := handler.handleMenuText(ctx, item)
		if err != nil || !handled || len(ctx.sent) == 0 {
			t.Fatalf("menu %q handled=%v sent=%d err=%v", item, handled, len(ctx.sent), err)
		}
		if dialogs.dialog == nil || dialogs.dialog.State != domain.DialogIdle {
			t.Fatalf("menu %q did not reset dialog", item)
		}
	}
	if handled, err := handler.handleMenuText(&telegramContextStub{}, "ordinary text"); err != nil || handled {
		t.Fatalf("ordinary text handled=%v err=%v", handled, err)
	}
}

type notificationActionsStub struct{ result NotificationActionResult }

func (s notificationActionsStub) Apply(context.Context, int64, uuid.UUID, string, time.Time) (NotificationActionResult, error) {
	return s.result, nil
}

func TestNotificationCallbackAcknowledgesAction(t *testing.T) {
	handler, _, _, _ := newUIHandlerTestFixture()
	handler.notificationActions = notificationActionsStub{result: NotificationActionResult{Message: "done"}}
	ctx := callbackContext(t, "notification", "done", uuid.New())
	if err := handler.handleCallback(ctx); err != nil || len(ctx.responses) != 1 || ctx.responses[0].Text != "done" {
		t.Fatalf("responses=%+v err=%v", ctx.responses, err)
	}
}

func TestDraftEditCallbackPersistsFSM(t *testing.T) {
	handler, _, dialogs, _ := newUIHandlerTestFixture()
	dc := &DialogContext{Mode: "create", ParsedSpec: mustMarshal(&domain.Spec{Message: "task"}), CreatedAt: time.Now()}
	raw, _ := encodeContext(dc)
	dialogs.dialog = &domain.Dialog{UserID: 42, State: domain.DialogAwaitConfirm, Context: raw}
	ctx := callbackContext(t, "draft", "edit_time", uuid.Nil)
	if err := handler.handleCallback(ctx); err != nil {
		t.Fatal(err)
	}
	if dialogs.dialog.State != domain.DialogAwaitEdit {
		t.Fatalf("dialog = %+v", dialogs.dialog)
	}
	stored, err := decodeContext(dialogs.dialog.Context)
	if err != nil || stored.FieldName != "time" {
		t.Fatalf("context=%+v err=%v", stored, err)
	}
}

func TestSettingsUIAndInput(t *testing.T) {
	handler, _, dialogs, _ := newUIHandlerTestFixture()
	repo := &preferencesRepoStub{value: domain.UserPreferences{UserID: 42, MorningTime: "09:00", DefaultSnoozeMinutes: 10}}
	handler.preferences = NewUserPreferencesService(repo)
	ctx := &telegramContextStub{sender: &tele.User{ID: 42}}
	if err := handler.showSettings(ctx, false); err != nil || len(ctx.sent) != 1 {
		t.Fatalf("show settings sent=%d err=%v", len(ctx.sent), err)
	}
	callback := callbackContext(t, "settings", "morning", uuid.Nil)
	if err := handler.handleCallback(callback); err != nil || dialogs.dialog.State != domain.DialogAwaitEdit {
		t.Fatalf("settings callback dialog=%+v err=%v", dialogs.dialog, err)
	}
	dc := &DialogContext{Mode: "settings", FieldName: "morning"}
	input := &telegramContextStub{sender: &tele.User{ID: 42}}
	if err := handler.applySettingsInput(context.Background(), input, dc, "08:15"); err != nil {
		t.Fatal(err)
	}
	if repo.value.MorningTime != "08:15" || len(input.sent) != 1 {
		t.Fatalf("prefs=%+v sent=%d", repo.value, len(input.sent))
	}
}

type parserStub struct{ result *nlu.ParseResult }

func (s parserStub) Parse(context.Context, string, *time.Location) (*nlu.ParseResult, error) {
	return s.result, nil
}

func TestHandleEditFieldInputForDraftAndReminder(t *testing.T) {
	handler, reminders, dialogs, reminder := newUIHandlerTestFixture()
	create := &DialogContext{
		Mode: "create", RawText: "task", Kind: domain.KindAbsolute,
		ParsedSpec: mustMarshal(&domain.Spec{Message: "task"}), FieldName: "time",
		UserTZ: "UTC", CreatedAt: time.Now(),
	}
	raw, _ := encodeContext(create)
	ctx := &telegramContextStub{sender: &tele.User{ID: 42}}
	if err := handler.handleEditFieldInput(context.Background(), ctx, &domain.Dialog{UserID: 42, State: domain.DialogAwaitEdit, Context: raw}, "23:30"); err != nil {
		t.Fatal(err)
	}
	if dialogs.dialog == nil || dialogs.dialog.State != domain.DialogAwaitConfirm || len(ctx.sent) != 1 {
		t.Fatalf("dialog=%+v sent=%d", dialogs.dialog, len(ctx.sent))
	}

	fireAt := time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
	edit := &DialogContext{
		Mode: "reminder", ReminderID: reminder.ID.String(), Version: reminder.Version,
		RawText: reminder.RawText, Kind: reminder.Kind, ParsedSpec: mustMarshal(&reminder.Spec),
		FieldName: "text", FireAt: &fireAt, UserTZ: "UTC", CreatedAt: time.Now(),
	}
	raw, _ = encodeContext(edit)
	ctx = &telegramContextStub{sender: &tele.User{ID: 42}}
	if err := handler.handleEditFieldInput(context.Background(), ctx, &domain.Dialog{UserID: 42, State: domain.DialogAwaitEdit, Context: raw}, "new task"); err != nil {
		t.Fatal(err)
	}
	if reminders.reminders[0].Spec.Message != "new task" || len(ctx.sent) != 1 {
		t.Fatalf("reminder=%+v sent=%d", reminders.reminders[0], len(ctx.sent))
	}
}

func TestApplyConditionInputUsesParserCondition(t *testing.T) {
	target := int64(100)
	handler := &Handler{parser: parserStub{result: &nlu.ParseResult{Spec: &domain.Spec{
		Condition: &domain.Condition{Operator: domain.ConditionOperatorLTE, Target: &target}, Currency: "RUB",
	}}}}
	dc := &DialogContext{RawText: "price", ParsedSpec: mustMarshal(&domain.Spec{Event: domain.EventSpec{Type: "price"}})}
	if err := handler.applyConditionInput(context.Background(), dc, "ниже 100", time.UTC); err != nil {
		t.Fatal(err)
	}
	var spec domain.Spec
	if err := json.Unmarshal(dc.ParsedSpec, &spec); err != nil || spec.Condition == nil || *spec.Condition.Target != target {
		t.Fatalf("spec=%+v err=%v", spec, err)
	}
}

func TestApplySettingsInputVariants(t *testing.T) {
	for _, tc := range []struct {
		field string
		input string
	}{
		{"timezone", "Europe/Moscow"},
		{"quiet", "22:00-08:00"},
		{"quiet", "выкл"},
		{"morning", "07:45"},
		{"snooze", "30"},
	} {
		t.Run(tc.field+tc.input, func(t *testing.T) {
			handler, _, _, _ := newUIHandlerTestFixture()
			repo := &preferencesRepoStub{value: domain.UserPreferences{UserID: 42, MorningTime: "09:00", DefaultSnoozeMinutes: 10}}
			handler.preferences = NewUserPreferencesService(repo)
			ctx := &telegramContextStub{sender: &tele.User{ID: 42}}
			if err := handler.applySettingsInput(context.Background(), ctx, &DialogContext{Mode: "settings", FieldName: tc.field}, tc.input); err != nil {
				t.Fatal(err)
			}
			if len(ctx.sent) != 1 {
				t.Fatalf("sent = %d", len(ctx.sent))
			}
		})
	}
}
