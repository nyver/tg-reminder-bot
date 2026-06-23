package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nyver2k/remindertgbot/internal/clock"
	"github.com/nyver2k/remindertgbot/internal/domain"
	"github.com/nyver2k/remindertgbot/internal/provider"
)

// PlannedNotification is the evaluator output before persistence.
type PlannedNotification struct {
	FireAt         time.Time
	Text           string
	IdempotencyKey string
}

// HistoryRepo abstracts observation storage.
type HistoryRepo interface {
	Last(ctx context.Context, reminderID uuid.UUID) (*domain.Observation, error)
	Save(ctx context.Context, obs *domain.Observation) error
}

// Evaluator converts a Reminder into zero or more PlannedNotifications.
type Evaluator struct {
	registry       providerRegistry
	history        HistoryRepo
	clock          clock.Clock
	maxHorizonDays int
	log            *slog.Logger
}

type providerRegistry interface {
	Event(typ string) (provider.EventProvider, bool)
	Metric(typ string) (provider.MetricProvider, bool)
	Search(typ string) (provider.SearchProvider, bool)
}

func NewEvaluator(registry providerRegistry, history HistoryRepo, clk clock.Clock, maxHorizonDays int, log *slog.Logger) *Evaluator {
	if log == nil {
		log = slog.Default()
	}
	return &Evaluator{
		registry:       registry,
		history:        history,
		clock:          clk,
		maxHorizonDays: maxHorizonDays,
		log:            log,
	}
}

func (e *Evaluator) Evaluate(ctx context.Context, r domain.Reminder) ([]PlannedNotification, error) {
	switch r.Spec.Trigger {
	case domain.TriggerAnchor:
		return e.evaluateAnchor(ctx, r)
	case domain.TriggerThreshold:
		return e.evaluateThreshold(ctx, r)
	case domain.TriggerDigest:
		return e.evaluateDigest(ctx, r)
	default:
		if r.Kind == domain.KindAbsolute || r.Kind == domain.KindRecurring {
			return e.evaluateScheduled(r), nil
		}
		return nil, fmt.Errorf("unknown trigger %q", r.Spec.Trigger)
	}
}

func (e *Evaluator) evaluateScheduled(r domain.Reminder) []PlannedNotification {
	due := e.clock.Now()
	if r.NextEvalAt != nil {
		due = *r.NextEvalAt
	}
	text := r.Spec.Message
	if text == "" {
		text = r.RawText
	}
	return []PlannedNotification{{
		FireAt:         due,
		Text:           text,
		IdempotencyKey: idemKey(r.ID, "scheduled:"+due.UTC().Format(time.RFC3339Nano)),
	}}
}

// Window computes the sliding date window [startOfDay(now), +horizon].
// Exported for tests (spec §19.2).
func (e *Evaluator) Window(r domain.Reminder) (from, to time.Time) {
	return e.window(r, e.clock.Now())
}

func (e *Evaluator) window(r domain.Reminder, now time.Time) (from, to time.Time) {
	loc := userTZ(r)
	from = startOfDay(now.In(loc))
	horizon := orDefault(r.Spec.HorizonDays, 30)
	if horizon < 1 {
		horizon = 30
	}
	if horizon > e.maxHorizonDays {
		horizon = e.maxHorizonDays
	}
	to = from.AddDate(0, 0, horizon)
	return from, to
}

// --- anchor ---

func (e *Evaluator) evaluateAnchor(ctx context.Context, r domain.Reminder) ([]PlannedNotification, error) {
	ep, ok := e.registry.Event(r.Spec.Event.Type)
	if !ok {
		return nil, fmt.Errorf("no event provider for %q", r.Spec.Event.Type)
	}
	now := e.clock.Now()
	lookahead := orDefault(r.Spec.LookaheadDays, 7)
	from, to := now, now.AddDate(0, 0, lookahead)

	events, err := ep.Lookup(ctx, provider.Query{
		Title:  r.Spec.Event.Title,
		Params: r.Spec.Event.Params,
	}, from, to)
	if err != nil {
		// Transient provider errors (DNS, network, timeouts) are treated as
		// "no events found this tick" rather than a hard failure.  The watcher
		// will retry on the next cron tick, which is the desired behaviour for
		// short-lived outages — see watcher.processReminder.
		e.log.Warn("anchor lookup transient error, will retry next tick",
			"reminder_id", r.ID,
			"provider", r.Spec.Event.Type,
			"err", err,
		)
		return nil, nil
	}
	e.log.Info("anchor lookup ok",
		"reminder_id", r.ID,
		"title", r.Spec.Event.Title,
		"events_found", len(events),
		"window_from", from.Format(time.RFC3339),
		"window_to", to.Format(time.RFC3339),
		"now", now.Format(time.RFC3339),
	)
	for _, ev := range events {
		fireAt := ev.AnchorAt.Add(-r.Spec.LeadTime.Duration)
		e.log.Info("anchor event",
			"reminder_id", r.ID,
			"event_title", ev.Title,
			"anchor_at", ev.AnchorAt.Format(time.RFC3339),
			"fire_at", fireAt.Format(time.RFC3339),
			"fire_in_past", fireAt.Before(now),
			"anchor_started", !ev.AnchorAt.After(now),
		)
	}

	// Events are sorted ascending by AnchorAt. We want exactly one notification —
	// for the nearest upcoming occurrence. Notifying about every occurrence in the
	// lookahead window would flood the user; on the next evaluation tick the
	// watcher will naturally pick up the next occurrence after this one fires.
	for _, ev := range events {
		fireAt := ev.AnchorAt.Add(-r.Spec.LeadTime.Duration)
		if fireAt.Before(now) {
			if !ev.AnchorAt.After(now) {
				continue // The event itself has already started.
			}
			fireAt = now // Lead time was missed, but the event is still upcoming.
		}
		key := userIdemKey(r.UserID, "anchor:"+ev.Identity)
		return []PlannedNotification{{
			FireAt:         fireAt,
			Text:           renderAnchorText(r.Spec, ev, userTZ(r)),
			IdempotencyKey: key,
		}}, nil
	}
	return nil, nil
}

// --- threshold ---

func (e *Evaluator) evaluateThreshold(ctx context.Context, r domain.Reminder) ([]PlannedNotification, error) {
	mp, ok := e.registry.Metric(r.Spec.Event.Type)
	if !ok {
		return nil, fmt.Errorf("no metric provider for %q", r.Spec.Event.Type)
	}
	now := e.clock.Now()

	m, err := mp.Sample(ctx, provider.Query{
		Title:  r.Spec.Event.Title,
		Params: r.Spec.Event.Params,
	})
	if err != nil {
		e.log.Warn("metric sample failed, will retry next tick",
			"reminder_id", r.ID,
			"provider", r.Spec.Event.Type,
			"err", err,
		)
		return priceUnavailableNotification(r, now), nil
	}

	// Value == 0 means the page loaded but price extraction failed (e.g. WAF block).
	// Treat it the same as unavailable to avoid a false "price dropped to 0" alert.
	if !m.Available || m.Value <= 0 {
		return priceUnavailableNotification(r, now), nil
	}

	// Read previous observation BEFORE saving so prev is truly the last point,
	// not the record we are about to insert.
	prev, err := e.history.Last(ctx, r.ID)
	if err != nil {
		return nil, fmt.Errorf("history last: %w", err)
	}

	obs := &domain.Observation{
		ReminderID: r.ID,
		Value:      m.Value,
		Currency:   m.Currency,
		Available:  m.Available,
		ObservedAt: now,
	}
	_ = e.history.Save(ctx, obs)

	if prev == nil {
		// First observation — establish baseline, no notification yet.
		return nil, nil
	}
	if m.Value >= prev.Value {
		return nil, nil // price didn't drop
	}

	// Check target threshold.
	if r.Spec.Target != nil && m.Value > *r.Spec.Target {
		return nil, nil
	}

	key := userIdemKey(r.UserID, "threshold:"+now.In(userTZ(r)).Format("2006-01-02"))
	return []PlannedNotification{{
		FireAt:         now,
		Text:           renderThresholdText(r.Spec, m, prev),
		IdempotencyKey: key,
	}}, nil
}

// --- digest ---

func (e *Evaluator) evaluateDigest(ctx context.Context, r domain.Reminder) ([]PlannedNotification, error) {
	sp, ok := e.registry.Search(r.Spec.Event.Type)
	if !ok {
		return nil, fmt.Errorf("no search provider for %q", r.Spec.Event.Type)
	}
	now := e.clock.Now()
	from, to := e.window(r, now)

	offers, err := sp.Search(ctx, e.buildSearchQuery(r, from, to))
	if err != nil {
		return nil, err
	}

	topN := orDefault(r.Spec.TopN, 5)
	top := PickTopN(offers, topN)
	if len(top) == 0 {
		return nil, nil
	}

	prev, _ := e.history.Last(ctx, r.ID)
	rawJSON, _ := json.Marshal(top)
	minPrice := top[0].Price
	_ = e.history.Save(ctx, &domain.Observation{
		ReminderID: r.ID,
		Value:      minPrice,
		Currency:   top[0].Currency,
		Available:  true,
		Raw:        rawJSON,
		ObservedAt: now,
	})

	text := renderDigest(r.Spec, top, prev, from, to)
	key := userIdemKey(r.UserID, "digest:"+now.In(userTZ(r)).Format("2006-01-02"))
	return []PlannedNotification{{
		FireAt:         now,
		Text:           text,
		IdempotencyKey: key,
	}}, nil
}

func (e *Evaluator) buildSearchQuery(r domain.Reminder, from, to time.Time) provider.SearchQuery {
	return provider.SearchQuery{
		Origin:      r.Spec.Event.Params["origin"],
		Destination: r.Spec.Event.Params["destination"],
		DateFrom:    from,
		DateTo:      to,
		Modes:       splitModes(r.Spec.Event.Params["modes"]),
		Limit:       50,
	}
}

// PickTopN is exported for use in tests and travel package.
func PickTopN(offers []provider.Offer, n int) []provider.Offer {
	sort.SliceStable(offers, func(i, j int) bool {
		if offers[i].Price != offers[j].Price {
			return offers[i].Price < offers[j].Price
		}
		if offers[i].Duration != offers[j].Duration {
			return offers[i].Duration < offers[j].Duration
		}
		return offers[i].Transfers < offers[j].Transfers
	})
	if n > 0 && len(offers) > n {
		return offers[:n]
	}
	return offers
}

// --- helpers ---

func idemKey(reminderID uuid.UUID, suffix string) string {
	h := sha256.Sum256([]byte(reminderID.String() + ":" + suffix))
	return fmt.Sprintf("%x", h[:16])
}

// userIdemKey produces a notification idempotency key scoped to a user rather
// than a specific reminder. This ensures that even if a user somehow created
// two identical reminders, only one notification row is ever inserted for the
// same event (anchor identity, threshold date, etc.).
func userIdemKey(userID int64, suffix string) string {
	h := sha256.Sum256([]byte(strconv.FormatInt(userID, 10) + ":" + suffix))
	return fmt.Sprintf("%x", h[:16])
}

func userTZ(r domain.Reminder) *time.Location {
	if r.UserTZ != "" {
		if loc, err := time.LoadLocation(r.UserTZ); err == nil {
			return loc
		}
	}
	loc, _ := time.LoadLocation("Europe/Moscow")
	return loc
}

func startOfDay(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func orDefault(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func priceUnavailableNotification(r domain.Reminder, now time.Time) []PlannedNotification {
	key := userIdemKey(r.UserID, "price_unavailable:"+r.ID.String()+":"+now.In(userTZ(r)).Format("2006-01-02"))
	return []PlannedNotification{{
		FireAt:         now,
		Text:           renderPriceUnavailableText(r.Spec),
		IdempotencyKey: key,
	}}
}

func splitModes(s string) []string {
	if s == "" {
		return []string{"air", "rail"}
	}
	parts := strings.Split(s, ",")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return parts
}

// --- text rendering ---

func renderAnchorText(spec domain.Spec, ev provider.Event, loc *time.Location) string {
	if spec.Event.Type == "tv_program" {
		return renderTVProgramText(ev, loc)
	}
	return fmt.Sprintf("🔔 *%s* начинается через %s!\n%s",
		ev.Title, spec.LeadTime.String(), spec.Message)
}

var ruMonths = [12]string{
	"января", "февраля", "марта", "апреля", "мая", "июня",
	"июля", "августа", "сентября", "октября", "ноября", "декабря",
}

func renderTVProgramText(ev provider.Event, loc *time.Location) string {
	at := ev.AnchorAt.In(loc)
	date := fmt.Sprintf("%d %s", at.Day(), ruMonths[at.Month()-1])
	timeStr := at.Format("15:04")
	channel := ev.Meta["channel"]
	desc := ev.Meta["description"]

	text := fmt.Sprintf("Телепрограмма *%s* на канале *%s* начнётся в %s, %s.", ev.Title, channel, timeStr, date)
	if desc != "" {
		text += " " + desc
	}
	return text
}

func renderPriceUnavailableText(spec domain.Spec) string {
	var sb strings.Builder
	sb.WriteString("⚠️ Не удалось получить текущую цену\n")
	if spec.Event.Title != "" {
		sb.WriteString("*" + spec.Event.Title + "*\n")
	}
	if u := spec.Event.Params["url"]; u != "" {
		sb.WriteString(u + "\n")
	}
	sb.WriteString("\nБот продолжит проверять и уведомит при снижении цены.")
	return sb.String()
}

func renderThresholdText(spec domain.Spec, m provider.Measurement, prev *domain.Observation) string {
	price := formatPrice(m.Value, m.Currency)
	delta := ""
	if prev != nil && prev.Value > 0 {
		diff := prev.Value - m.Value
		delta = fmt.Sprintf(" (−%s к предыдущей)", formatPrice(diff, m.Currency))
	}
	return fmt.Sprintf("📉 Цена снизилась!\n%s\nЦена: *%s*%s\n%s",
		spec.Event.Title, price, delta, spec.Message)
}

func renderDigest(spec domain.Spec, offers []provider.Offer, prev *domain.Observation, from, to time.Time) string {
	var sb strings.Builder
	origin := spec.Event.Params["origin"]
	dest := spec.Event.Params["destination"]
	window := fmt.Sprintf("%s–%s", from.Format("02.01"), to.Format("02.01.06"))

	sb.WriteString(fmt.Sprintf("🎫 *%s → %s* — %d самых дешёвых (окно: %s)\n",
		origin, dest, len(offers), window))

	if len(offers) > 0 && prev != nil && prev.Value > 0 {
		minToday := offers[0].Price
		delta := prev.Value - minToday
		sign := "−"
		if delta < 0 {
			delta = -delta
			sign = "+"
		}
		sb.WriteString(fmt.Sprintf("Минимум сегодня: *%s* (%s%s к вчера)\n\n",
			formatPrice(minToday, offers[0].Currency),
			sign, formatPrice(delta, offers[0].Currency)))
	} else if len(offers) > 0 {
		sb.WriteString(fmt.Sprintf("Минимум сегодня: *%s*\n\n",
			formatPrice(offers[0].Price, offers[0].Currency)))
	}

	for i, o := range offers {
		icon := "✈"
		if o.Mode == "rail" {
			icon = "🚆"
		}
		sb.WriteString(fmt.Sprintf("%d. %s %s · %s · %s · *%s* · [ссылка](%s)\n",
			i+1, icon, o.Title,
			o.DepartAt.Format("02 Jan 15:04"),
			formatDuration(o.Duration),
			formatPrice(o.Price, o.Currency),
			o.BookURL,
		))
	}
	return sb.String()
}

func formatPrice(kopecks int64, currency string) string {
	rubles := kopecks / 100
	sym := "₽"
	switch currency {
	case "USD":
		sym = "$"
	case "EUR":
		sym = "€"
	}
	return fmt.Sprintf("%s %s", formatThousands(rubles), sym)
}

func formatThousands(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var result []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ' ')
		}
		result = append(result, c)
	}
	return string(result)
}

func formatDuration(d time.Duration) string {
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	if h == 0 {
		return fmt.Sprintf("%d м", m)
	}
	return fmt.Sprintf("%d ч %d м", h, m)
}
