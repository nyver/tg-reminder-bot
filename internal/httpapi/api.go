package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type reminderRepo interface {
	ListByUser(ctx context.Context, userID int64) ([]domain.Reminder, error)
	Get(ctx context.Context, id uuid.UUID) (*domain.Reminder, error)
	UpdateStatus(ctx context.Context, id uuid.UUID, status domain.Status) error
}

type notificationRepo interface {
	ListFailed(ctx context.Context, limit int) ([]domain.ScheduledNotification, error)
	Retry(ctx context.Context, id uuid.UUID) error
	Get(ctx context.Context, id uuid.UUID) (*domain.ScheduledNotification, error)
}

type observationRepo interface {
	List(ctx context.Context, reminderID uuid.UUID, limit int) ([]domain.Observation, error)
}

// Server is the Admin API HTTP server.
type Server struct {
	reminders     reminderRepo
	notifications notificationRepo
	observations  observationRepo
	log           *slog.Logger
	mux           *http.ServeMux
}

func NewServer(
	reminders reminderRepo,
	notifications notificationRepo,
	observations observationRepo,
	log *slog.Logger,
) *Server {
	s := &Server{
		reminders:     reminders,
		notifications: notifications,
		observations:  observations,
		log:           log,
		mux:           http.NewServeMux(),
	}
	s.registerRoutes()
	return s
}

func (s *Server) registerRoutes() {
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/readyz", s.handleReadyz)
	s.mux.Handle("/metrics", promhttp.Handler())
	s.mux.HandleFunc("GET /api/users/{id}/reminders", s.handleListReminders)
	s.mux.HandleFunc("GET /api/reminders/{id}", s.handleGetReminder)
	s.mux.HandleFunc("GET /api/reminders/{id}/observations", s.handleListObservations)
	s.mux.HandleFunc("POST /api/reminders/{id}/cancel", s.handleCancelReminder)
	s.mux.HandleFunc("GET /api/notifications", s.handleListNotifications)
	s.mux.HandleFunc("POST /api/notifications/{id}/retry", s.handleRetryNotification)
}

func (s *Server) Run(ctx context.Context, port int) error {
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", port),
		Handler: s.mux,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	s.log.Info("api listening", "port", port)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleListReminders(w http.ResponseWriter, r *http.Request) {
	// TODO M6: parse {id} from path using r.PathValue("id")
	writeJSON(w, http.StatusOK, map[string]string{"status": "not implemented"})
}

func (s *Server) handleGetReminder(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	rem, err := s.reminders.Get(r.Context(), id)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	writeJSON(w, http.StatusOK, rem)
}

func (s *Server) handleListObservations(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	obs, err := s.observations.List(r.Context(), id, 30)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, obs)
}

func (s *Server) handleCancelReminder(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if err := s.reminders.UpdateStatus(r.Context(), id, domain.StatusDone); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	if status != "failed" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "status=failed required"})
		return
	}
	notifs, err := s.notifications.ListFailed(r.Context(), 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, notifs)
}

func (s *Server) handleRetryNotification(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	if err := s.notifications.Retry(r.Context(), id); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found or not failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "retrying"})
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
