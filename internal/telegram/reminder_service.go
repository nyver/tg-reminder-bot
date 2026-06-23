package telegram

import (
	"context"

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

type userServiceAdapter struct {
	repo userRepository
}

type userRepository interface {
	GetOrCreate(ctx context.Context, userID int64) (*domain.User, error)
	SetTZ(ctx context.Context, userID int64, tz string) error
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
