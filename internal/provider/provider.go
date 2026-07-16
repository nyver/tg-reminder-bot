package provider

import (
	"context"
	"time"
)

// EventProvider looks up time-anchored events (e.g. TV schedule).
type EventProvider interface {
	Type() string
	Lookup(ctx context.Context, q Query, from, to time.Time) ([]Event, error)
}

// MetricProvider samples a scalar metric (e.g. product price).
type MetricProvider interface {
	Type() string
	Sample(ctx context.Context, q Query) (Measurement, error)
}

// SearchProvider searches for offers within a date range (e.g. travel tickets).
type SearchProvider interface {
	Type() string
	Search(ctx context.Context, q SearchQuery) ([]Offer, error)
}

// NewsProvider fetches and importance-ranks items from a news feed (e.g. RSS/Atom).
type NewsProvider interface {
	Type() string
	Fetch(ctx context.Context, q Query) ([]NewsItem, error)
}

type Query struct {
	Title  string
	Params map[string]string
}

// SearchQuery carries the sliding-window date range computed from HorizonDays.
type SearchQuery struct {
	Origin, Destination string
	DateFrom, DateTo    time.Time
	Modes               []string
	Limit               int
}

type Event struct {
	Identity string
	Title    string
	AnchorAt time.Time
	Meta     map[string]string
}

type Measurement struct {
	Value      int64
	Currency   string
	Available  bool
	Title      string
	HTTPStatus int // non-zero when Available=false due to HTTP error
	Meta       map[string]string
}

// TVShowtime is a single broadcast slot returned by TVScheduler.
type TVShowtime struct {
	Title    string
	Channel  string
	StartsAt time.Time
	EndsAt   time.Time // zero if unknown
}

// TVScheduler looks up TV programme schedules on demand.
type TVScheduler interface {
	QuerySchedule(ctx context.Context, title, channel string, from, to time.Time) ([]TVShowtime, error)
	// ChannelDaySchedule returns all programmes for the named channel in [from, to).
	// channelName is the canonical display name resolved from the index.
	ChannelDaySchedule(ctx context.Context, channel string, from, to time.Time) (channelName string, shows []TVShowtime, err error)
}

// NewsItem is a single entry from a news feed, importance-ranked by the provider.
type NewsItem struct {
	Title       string
	Link        string
	Summary     string
	PublishedAt time.Time
	Score       float64
}

type Offer struct {
	Signature string // dedup key: mode|carrier|number|depart_date
	Mode      string // air | rail
	Title     string
	Carrier   string
	Price     int64 // kopecks
	Currency  string
	DepartAt  time.Time
	Duration  time.Duration
	Transfers int
	BookURL   string
	Meta      map[string]string
}
