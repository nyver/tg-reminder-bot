package iptvx

import (
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/nyver2k/remindertgbot/internal/provider"
)

const (
	providerType    = "tv_program"
	maxDownloadSize = 512 << 20 // 512 MB
)

// EPGStore persists and queries imported EPG data.
type EPGStore interface {
	// ImportEPG atomically replaces all EPG data (delete-all + insert-new).
	ImportEPG(ctx context.Context, channels []EPGChannel, progs []EPGProgramme) error
	// Channels returns all channels (for fuzzy name matching).
	Channels(ctx context.Context) ([]EPGChannel, error)
	// Programmes returns programmes for channelID in [from, to).
	Programmes(ctx context.Context, channelID string, from, to time.Time) ([]EPGProgramme, error)
	// SearchProgrammes finds programmes whose title contains titleLike, optionally
	// filtered to channelID (empty string = all channels). Results include the
	// channel display name and are ordered by starts_at.
	SearchProgrammes(ctx context.Context, titleLike, channelID string, from, to time.Time) ([]EPGSearchResult, error)
	// HasFutureSchedule returns true when the DB contains at least one programme
	// starting after now, indicating the imported data is still fresh.
	HasFutureSchedule(ctx context.Context) (bool, error)
}

type EPGChannel struct {
	ID          string
	DisplayName string
}

type EPGProgramme struct {
	ChannelID string // populated during import, empty in query results
	Title     string
	StartsAt  time.Time
	EndsAt    time.Time // zero if unknown
	Desc      string
}

// EPGSearchResult is returned by EPGStore.SearchProgrammes and includes the
// channel display name alongside programme fields.
type EPGSearchResult struct {
	ChannelID   string
	ChannelName string
	Title       string
	StartsAt    time.Time
	EndsAt      time.Time
}

type Config struct {
	URL            string
	FilePath       string
	UpdateInterval time.Duration
	Timeout        time.Duration
}

// Provider downloads and caches an XMLTV EPG file, importing it into an
// EPGStore on startup and on every UpdateInterval tick.
// Call Run(ctx) as a background goroutine to enable periodic refresh.
type Provider struct {
	cfg    Config
	store  EPGStore
	client *http.Client
	log    *slog.Logger

	// in-memory channel list for fast fuzzy matching between imports
	chMu     sync.RWMutex
	channels []EPGChannel
}

func New(cfg Config, store EPGStore, log *slog.Logger) *Provider {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	if cfg.UpdateInterval <= 0 {
		cfg.UpdateInterval = 7 * 24 * time.Hour
	}
	if log == nil {
		log = slog.Default()
	}
	return &Provider{
		cfg:    cfg,
		store:  store,
		client: &http.Client{Timeout: cfg.Timeout},
		log:    log,
	}
}

func (p *Provider) Type() string { return providerType }

// Run loads the EPG on startup and refreshes it on every UpdateInterval.
// Blocks until ctx is cancelled.
func (p *Provider) Run(ctx context.Context) error {
	if err := p.ensureImported(ctx); err != nil {
		p.log.Warn("iptvx: initial EPG import failed, will retry on next tick", "err", err)
	}

	ticker := time.NewTicker(p.cfg.UpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := p.ensureImported(ctx); err != nil {
				p.log.Error("iptvx: EPG refresh failed", "err", err)
			}
		}
	}
}

func (p *Provider) Lookup(ctx context.Context, q provider.Query, from, to time.Time) ([]provider.Event, error) {
	if p.cfg.URL == "" {
		return nil, nil
	}
	if strings.TrimSpace(q.Title) == "" || !from.Before(to) {
		return nil, nil
	}

	ch := p.resolveChannel(q.Params)
	if ch.ID == "" {
		return nil, nil
	}

	progs, err := p.store.Programmes(ctx, ch.ID, from, to)
	if err != nil {
		return nil, fmt.Errorf("iptvx: query programmes: %w", err)
	}

	seen := make(map[string]struct{})
	var result []provider.Event
	for _, prog := range progs {
		if !matches(prog.Title, q.Title) {
			continue
		}
		identity := fmt.Sprintf("iptvx:%s:%d:%s", ch.ID, prog.StartsAt.Unix(), normalize(prog.Title))
		if _, dup := seen[identity]; dup {
			continue
		}
		seen[identity] = struct{}{}

		meta := map[string]string{
			"channel":    ch.DisplayName,
			"channel_id": ch.ID,
		}
		if !prog.EndsAt.IsZero() {
			meta["stop_at"] = prog.EndsAt.UTC().Format(time.RFC3339)
		}
		if prog.Desc != "" {
			meta["description"] = prog.Desc
		}
		result = append(result, provider.Event{
			Identity: identity,
			Title:    prog.Title,
			AnchorAt: prog.StartsAt,
			Meta:     meta,
		})
	}
	return result, nil
}

// ensureImported ensures the in-memory channel cache and the DB are populated
// with a fresh EPG schedule.
//
// Priority:
//  1. DB has channels AND at least one programme in the future → warm cache, done.
//  2. Otherwise: download the file if absent/stale, then parse+import into DB.
func (p *Provider) ensureImported(ctx context.Context) error {
	// Check the DB first, regardless of whether the local file exists.
	// This handles container restarts where the file is gone but the DB is intact.
	channels, dbErr := p.store.Channels(ctx)
	if dbErr == nil && len(channels) > 0 {
		hasFuture, _ := p.store.HasFutureSchedule(ctx)
		if hasFuture {
			p.chMu.Lock()
			p.channels = channels
			p.chMu.Unlock()
			p.log.Info("iptvx: EPG already in DB, skipping import", "channels", len(channels))
			return nil
		}
	}

	// DB is empty or all programmes are in the past: (re)download if the file is stale.
	info, err := os.Stat(p.cfg.FilePath)
	fileStale := err != nil || time.Since(info.ModTime()) >= p.cfg.UpdateInterval

	if fileStale {
		if dlErr := p.download(ctx); dlErr != nil {
			if _, statErr := os.Stat(p.cfg.FilePath); statErr == nil {
				p.log.Warn("iptvx: download failed, importing from stale cache", "err", dlErr)
			} else {
				return fmt.Errorf("iptvx download: %w", dlErr)
			}
		}
	}

	return p.importFile(ctx)
}

func (p *Provider) download(ctx context.Context) error {
	if p.cfg.URL == "" {
		return errors.New("iptvx: URL not configured")
	}

	p.log.Info("iptvx: downloading EPG file", "url", p.cfg.URL)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.cfg.URL, nil)
	if err != nil {
		return err
	}
	// Disable transparent decompression so raw bytes are saved as-is.
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("iptvx: HTTP %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(p.cfg.FilePath), 0o755); err != nil {
		return fmt.Errorf("iptvx: mkdir: %w", err)
	}

	tmp := p.cfg.FilePath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return fmt.Errorf("iptvx: create tmp: %w", err)
	}

	_, copyErr := io.Copy(f, io.LimitReader(resp.Body, maxDownloadSize))
	_ = f.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("iptvx: write: %w", copyErr)
	}

	if err := os.Rename(tmp, p.cfg.FilePath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("iptvx: rename: %w", err)
	}

	p.log.Info("iptvx: EPG file saved", "path", p.cfg.FilePath)
	return nil
}

// importFile parses the cached XMLTV file and calls store.ImportEPG.
func (p *Provider) importFile(ctx context.Context) error {
	f, err := os.Open(p.cfg.FilePath)
	if err != nil {
		return fmt.Errorf("iptvx: open: %w", err)
	}
	defer f.Close()

	r, closer, err := wrapReader(f)
	if err != nil {
		return fmt.Errorf("iptvx: open reader: %w", err)
	}
	defer closer()

	channels, progs, err := parseXMLTV(r)
	if err != nil {
		return fmt.Errorf("iptvx: parse: %w", err)
	}

	if err := p.store.ImportEPG(ctx, channels, progs); err != nil {
		return fmt.Errorf("iptvx: db import: %w", err)
	}

	p.chMu.Lock()
	p.channels = channels
	p.chMu.Unlock()

	p.log.Info("iptvx: EPG imported",
		"channels", len(channels),
		"programmes", len(progs))
	return nil
}

// wrapReader detects gzip by magic bytes and wraps accordingly.
func wrapReader(f *os.File) (io.Reader, func(), error) {
	header := make([]byte, 2)
	n, _ := f.Read(header)
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, func() {}, err
	}
	if n == 2 && header[0] == 0x1f && header[1] == 0x8b {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, func() {}, err
		}
		return gz, func() { _ = gz.Close() }, nil
	}
	return f, func() {}, nil
}

// resolveChannel returns the channel matching params, using the in-memory cache.
func (p *Provider) resolveChannel(params map[string]string) EPGChannel {
	if id := strings.TrimSpace(params["channel_id"]); id != "" {
		name := strings.TrimSpace(params["channel"])
		if name == "" {
			name = id
		}
		return EPGChannel{ID: id, DisplayName: name}
	}
	name := strings.TrimSpace(params["channel"])
	if name == "" {
		return EPGChannel{}
	}

	p.chMu.RLock()
	channels := p.channels
	p.chMu.RUnlock()

	if len(channels) == 0 {
		p.log.Warn("iptvx: channel cache empty, EPG not imported yet")
		return EPGChannel{}
	}

	bestScore := 0
	var best EPGChannel
	for _, c := range channels {
		if s := matchScore(c.DisplayName, name); s > bestScore {
			bestScore = s
			best = c
		}
	}
	if best.ID != "" {
		p.log.Info("iptvx: channel resolved", "query", name, "id", best.ID, "display_name", best.DisplayName, "score", bestScore)
	} else {
		p.log.Warn("iptvx: channel not matched", "query", name)
	}
	return best
}

// --- XMLTV streaming parser ---

type xmlChannel struct {
	ID           string         `xml:"id,attr"`
	DisplayNames []xmlLangValue `xml:"display-name"`
}

type xmlLangValue struct {
	Lang  string `xml:"lang,attr"`
	Value string `xml:",chardata"`
}

func (c xmlChannel) displayName() string {
	var first string
	for _, d := range c.DisplayNames {
		if first == "" {
			first = d.Value
		}
		if d.Lang == "ru" || d.Lang == "" {
			return d.Value
		}
	}
	return first
}

type xmlProg struct {
	Start   string         `xml:"start,attr"`
	Stop    string         `xml:"stop,attr"`
	Channel string         `xml:"channel,attr"`
	Titles  []xmlLangValue `xml:"title"`
	Descs   []xmlLangValue `xml:"desc"`
}

func (p xmlProg) title() string {
	var first string
	for _, t := range p.Titles {
		if first == "" {
			first = t.Value
		}
		if t.Lang == "ru" || t.Lang == "" {
			return t.Value
		}
	}
	return first
}

func (p xmlProg) desc() string {
	for _, d := range p.Descs {
		if d.Lang == "ru" || d.Lang == "" {
			return d.Value
		}
	}
	if len(p.Descs) > 0 {
		return p.Descs[0].Value
	}
	return ""
}

// parseXMLTV streams the XMLTV document without loading the full tree into memory.
func parseXMLTV(r io.Reader) ([]EPGChannel, []EPGProgramme, error) {
	var channels []EPGChannel
	var progs []EPGProgramme
	dec := xml.NewDecoder(r)

	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}

		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}

		switch se.Name.Local {
		case "channel":
			var xc xmlChannel
			if err := dec.DecodeElement(&xc, &se); err != nil {
				continue
			}
			if xc.ID != "" {
				channels = append(channels, EPGChannel{
					ID:          xc.ID,
					DisplayName: xc.displayName(),
				})
			}

		case "programme":
			var xp xmlProg
			if err := dec.DecodeElement(&xp, &se); err != nil {
				continue
			}
			title := xp.title()
			if title == "" || xp.Channel == "" {
				continue
			}
			start, ok := parseXMLTVTime(xp.Start)
			if !ok {
				continue
			}
			var stop time.Time
			if xp.Stop != "" {
				stop, _ = parseXMLTVTime(xp.Stop)
			}
			progs = append(progs, EPGProgramme{
				ChannelID: xp.Channel,
				Title:     title,
				StartsAt:  start,
				EndsAt:    stop,
				Desc:      xp.desc(),
			})
		}
	}

	sort.Slice(progs, func(i, j int) bool {
		return progs[i].StartsAt.Before(progs[j].StartsAt)
	})

	return channels, progs, nil
}

func parseXMLTVTime(s string) (time.Time, bool) {
	t, err := time.Parse("20060102150405 -0700", s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// --- matching helpers ---

func matches(value, wanted string) bool {
	return matchScore(value, wanted) > 0
}

func matchScore(value, wanted string) int {
	v := normalize(value)
	w := normalize(wanted)
	if v == "" || w == "" {
		return 0
	}
	if v == w {
		return 3
	}
	if strings.Contains(v, w) || strings.Contains(w, v) {
		return 2
	}
	return 0
}

func normalize(s string) string {
	s = strings.ToLower(strings.ReplaceAll(s, "ё", "е"))
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, s)
}
