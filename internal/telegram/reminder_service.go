package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
)

type reminderRepository interface {
	Create(ctx context.Context, rem *domain.Reminder) error
	Get(ctx context.Context, id uuid.UUID) (*domain.Reminder, error)
	ListByUser(ctx context.Context, userID int64) ([]domain.Reminder, error)
	Cancel(ctx context.Context, userID int64, id uuid.UUID) error
	Remove(ctx context.Context, userID int64, id uuid.UUID) error
	Pause(ctx context.Context, userID int64, id uuid.UUID, pause bool) error
	Finish(ctx context.Context, userID int64, id uuid.UUID) error
	Update(ctx context.Context, rem *domain.Reminder, expectedVersion int64) error
}

func NewReminderService(repo reminderRepository) ReminderService {
	return &simpleReminderService{repo: repo}
}

type simpleReminderService struct {
	repo reminderRepository
}

func (s *simpleReminderService) Create(ctx context.Context, rem *domain.Reminder) error {
	return s.repo.Create(ctx, rem)
}

func (s *simpleReminderService) Get(ctx context.Context, userID int64, id uuid.UUID) (*domain.Reminder, error) {
	rem, err := s.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	// Scope to owner — hide existence from other users.
	if rem.UserID != userID {
		return nil, domain.ErrNotFound
	}
	return rem, nil
}

func (s *simpleReminderService) ListByUser(ctx context.Context, userID int64) ([]domain.Reminder, error) {
	return s.repo.ListByUser(ctx, userID)
}

func (s *simpleReminderService) Cancel(ctx context.Context, userID int64, id uuid.UUID) error {
	return s.repo.Cancel(ctx, userID, id)
}

func (s *simpleReminderService) Remove(ctx context.Context, userID int64, id uuid.UUID) error {
	return s.repo.Remove(ctx, userID, id)
}

func (s *simpleReminderService) Pause(ctx context.Context, userID int64, id uuid.UUID, pause bool) error {
	return s.repo.Pause(ctx, userID, id, pause)
}

func (s *simpleReminderService) Finish(ctx context.Context, userID int64, id uuid.UUID) error {
	return s.repo.Finish(ctx, userID, id)
}

func (s *simpleReminderService) Update(ctx context.Context, rem *domain.Reminder, expectedVersion int64) error {
	if rem.UserID == 0 {
		return domain.ErrInvalidSpec
	}
	return s.repo.Update(ctx, rem, expectedVersion)
}

func (s *simpleReminderService) Duplicate(ctx context.Context, userID int64, id uuid.UUID, now time.Time, timezone string) (*domain.Reminder, error) {
	original, err := s.Get(ctx, userID, id)
	if err != nil {
		return nil, err
	}
	// Round-trip the spec so maps are not shared between the original and copy.
	specJSON, err := json.Marshal(original.Spec)
	if err != nil {
		return nil, err
	}
	var spec domain.Spec
	if err := json.Unmarshal(specJSON, &spec); err != nil {
		return nil, err
	}
	copy := &domain.Reminder{
		UserID:     userID,
		Kind:       original.Kind,
		RawText:    original.RawText,
		Spec:       spec,
		Status:     domain.StatusActive,
		NextEvalAt: original.NextEvalAt,
		EvalCron:   original.EvalCron,
	}
	if copy.EvalCron != "" {
		copy.NextEvalAt, err = duplicateNextCron(copy.EvalCron, now, timezone)
		if err != nil {
			return nil, err
		}
	} else if copy.Kind == domain.KindConditional || copy.NextEvalAt == nil || !copy.NextEvalAt.After(now) {
		next := now.UTC()
		copy.NextEvalAt = &next
	}
	if err := s.Create(ctx, copy); err != nil {
		return nil, err
	}
	return copy, nil
}

func duplicateNextCron(expr string, now time.Time, timezone string) (*time.Time, error) {
	if timezone == "" {
		timezone = defaultUserTimezone
	}
	next, err := nextCronAt(expr, now, timezone)
	if err != nil {
		return nil, err
	}
	return &next, nil
}

type userServiceAdapter struct {
	repo userRepository
}

type userRepository interface {
	GetOrCreate(ctx context.Context, userID int64) (*domain.User, error)
	SetTZ(ctx context.Context, userID int64, tz string) error
}

// UserPreferencesService validates and persists Telegram UI defaults.
type UserPreferencesService interface {
	Get(ctx context.Context, userID int64) (*domain.UserPreferences, error)
	Update(ctx context.Context, preferences domain.UserPreferences) error
}

type userPreferencesRepository interface {
	GetOrCreate(ctx context.Context, userID int64) (*domain.UserPreferences, error)
	Update(ctx context.Context, preferences domain.UserPreferences) error
}

type userPreferencesService struct{ repo userPreferencesRepository }

func NewUserPreferencesService(repo userPreferencesRepository) UserPreferencesService {
	return &userPreferencesService{repo: repo}
}

func (s *userPreferencesService) Get(ctx context.Context, userID int64) (*domain.UserPreferences, error) {
	return s.repo.GetOrCreate(ctx, userID)
}

func (s *userPreferencesService) Update(ctx context.Context, p domain.UserPreferences) error {
	if err := validateClock(p.MorningTime); err != nil {
		return fmt.Errorf("morning time: %w", err)
	}
	if (p.QuietStart == "") != (p.QuietEnd == "") {
		return fmt.Errorf("quiet hours require both start and end")
	}
	if p.QuietStart != "" {
		if err := validateClock(p.QuietStart); err != nil {
			return fmt.Errorf("quiet start: %w", err)
		}
		if err := validateClock(p.QuietEnd); err != nil {
			return fmt.Errorf("quiet end: %w", err)
		}
	}
	if p.DefaultSnoozeMinutes < 1 || p.DefaultSnoozeMinutes > 7*24*60 {
		return fmt.Errorf("default snooze must be between 1 and 10080 minutes")
	}
	return s.repo.Update(ctx, p)
}

func validateClock(value string) error {
	_, err := time.Parse("15:04", strings.TrimSpace(value))
	return err
}

// NotificationActionResult tells the Telegram handler whether an action also
// requires an immediate provider evaluation.
type NotificationActionResult struct {
	ReminderID uuid.UUID
	RunNow     bool
	Message    string
}

type NotificationActionService interface {
	Apply(ctx context.Context, userID int64, notificationID uuid.UUID, action string, now time.Time) (NotificationActionResult, error)
}

type notificationStore interface {
	Get(ctx context.Context, id uuid.UUID) (*domain.ScheduledNotification, error)
	Enqueue(ctx context.Context, notification *domain.ScheduledNotification) error
}

type notificationActionStore interface {
	Record(ctx context.Context, action *domain.NotificationAction) error
}

type notificationActionService struct {
	notifications notificationStore
	actions       notificationActionStore
	reminders     ReminderService
	users         UserService
	preferences   UserPreferencesService
}

func NewNotificationActionService(notifications notificationStore, actions notificationActionStore, reminders ReminderService, users UserService, preferences UserPreferencesService) NotificationActionService {
	return &notificationActionService{notifications: notifications, actions: actions, reminders: reminders, users: users, preferences: preferences}
}

func (s *notificationActionService) Apply(ctx context.Context, userID int64, notificationID uuid.UUID, action string, now time.Time) (NotificationActionResult, error) {
	notification, err := s.notifications.Get(ctx, notificationID)
	if err != nil {
		return NotificationActionResult{}, domain.ErrNotFound
	}
	reminder, err := s.reminders.Get(ctx, userID, notification.ReminderID)
	if err != nil {
		return NotificationActionResult{}, domain.ErrNotFound
	}
	result := NotificationActionResult{ReminderID: reminder.ID}
	switch action {
	case "done":
		err = s.reminders.Finish(ctx, userID, reminder.ID)
		result.Message = "✅ Напоминание завершено."
	case "pause":
		err = s.reminders.Pause(ctx, userID, reminder.ID, true)
		result.Message = "⏸ Напоминание приостановлено."
	case "check", "repeat":
		result.RunNow = true
		result.Message = "▶ Запускаю снова."
	case "snooze_10", "snooze_60", "snooze_morning", "snooze_default":
		var fireAt time.Time
		fireAt, err = s.snoozeAt(ctx, userID, action, now)
		if err == nil {
			parent := notification.ID
			err = s.notifications.Enqueue(ctx, &domain.ScheduledNotification{
				ReminderID:           reminder.ID,
				FireAt:               fireAt,
				Text:                 notification.Text,
				IdempotencyKey:       "snooze:" + notification.ID.String() + ":" + action,
				Status:               domain.NotificationPending,
				ParentNotificationID: &parent,
			})
			result.Message = "⏰ Напоминание отложено."
		}
	default:
		return NotificationActionResult{}, domain.ErrNotFound
	}
	if err != nil && !errors.Is(err, domain.ErrAlreadyExists) {
		return NotificationActionResult{}, err
	}
	payload, _ := json.Marshal(map[string]string{"result": result.Message})
	err = s.actions.Record(ctx, &domain.NotificationAction{
		NotificationID: notification.ID,
		UserID:         userID,
		Action:         action,
		Payload:        payload,
	})
	if errors.Is(err, domain.ErrAlreadyExists) {
		result.RunNow = false
		result.Message = "Это действие уже выполнено."
		return result, nil
	}
	return result, err
}

func (s *notificationActionService) snoozeAt(ctx context.Context, userID int64, action string, now time.Time) (time.Time, error) {
	switch action {
	case "snooze_10":
		return now.Add(10 * time.Minute), nil
	case "snooze_60":
		return now.Add(time.Hour), nil
	}
	prefs, err := s.preferences.Get(ctx, userID)
	if err != nil {
		return time.Time{}, err
	}
	if action == "snooze_default" {
		return now.Add(time.Duration(prefs.DefaultSnoozeMinutes) * time.Minute), nil
	}
	user, err := s.users.GetOrCreate(ctx, userID)
	if err != nil {
		return time.Time{}, err
	}
	loc, err := time.LoadLocation(user.TZ)
	if err != nil {
		return time.Time{}, err
	}
	clock, err := time.Parse("15:04", prefs.MorningTime)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse morning time: %w", err)
	}
	localNow := now.In(loc)
	morning := time.Date(localNow.Year(), localNow.Month(), localNow.Day()+1, clock.Hour(), clock.Minute(), 0, 0, loc)
	return morning.UTC(), nil
}

func NewUserService(repo userRepository) UserService {
	return &userServiceAdapter{repo: repo}
}

func (s *userServiceAdapter) GetOrCreate(ctx context.Context, userID int64) (*domain.User, error) {
	return s.repo.GetOrCreate(ctx, userID)
}

func (s *userServiceAdapter) SetTZ(ctx context.Context, userID int64, tz string) error {
	return s.repo.SetTZ(ctx, userID, tz)
}
