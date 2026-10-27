package iptvx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestImport_malformedXMLKeepsPreviousData verifies that a parse failure in the
// middle of the stream aborts the import without touching existing data or the
// in-memory channel cache.
func TestImport_malformedXMLKeepsPreviousData(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	filePath := filepath.Join(dir, "epg.xml")
	broken := `<tv><channel id="9"><display-name>Новый</display-name></channel><programme start="20260622210000 +0300" channel="9"><title>X</title>`
	if err := os.WriteFile(filePath, []byte(broken), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := []EPGChannel{{ID: "1", DisplayName: "Первый канал"}}
	store := &memStore{channels: prev}
	p := newTestProvider(t, "http://unused", filePath, store)
	p.channels = prev

	if err := p.importFile(context.Background()); err == nil {
		t.Fatal("expected parse error for truncated XML")
	}
	if store.imports != 0 {
		t.Fatalf("imports = %d, want 0", store.imports)
	}
	if len(store.channels) != 1 || store.channels[0].ID != "1" {
		t.Fatalf("store channels changed: %+v", store.channels)
	}
	if len(p.channels) != 1 || p.channels[0].ID != "1" {
		t.Fatalf("channel cache changed: %+v", p.channels)
	}
}

type failingWriter struct{ failOn string }

func (w failingWriter) AddChannel(EPGChannel) error {
	if w.failOn == "channel" {
		return fmt.Errorf("channel write failed")
	}
	return nil
}

func (w failingWriter) AddProgramme(EPGProgramme) error {
	if w.failOn == "programme" {
		return fmt.Errorf("programme write failed")
	}
	return nil
}

func TestParseXMLTV_propagatesWriterErrors(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 6, 22, 21, 0, 0, 0, time.UTC)
	doc := makeXMLTV(start, start.Add(time.Hour))
	for _, kind := range []string{"channel", "programme"} {
		_, err := parseXMLTV(context.Background(), strings.NewReader(doc), failingWriter{failOn: kind})
		if err == nil || !strings.Contains(err.Error(), kind) {
			t.Fatalf("failOn=%s: err = %v", kind, err)
		}
	}
}

func TestParseXMLTV_countsAndSkipsInvalid(t *testing.T) {
	t.Parallel()
	doc := `<tv>` +
		`<channel id="1"><display-name lang="en">One</display-name><display-name lang="ru">Один</display-name></channel>` +
		`<channel><display-name>no id</display-name></channel>` +
		`<programme start="20260622210000 +0300" channel="1"><title lang="ru">Ok</title><desc lang="en">d</desc></programme>` +
		`<programme start="bad" channel="1"><title>Bad time</title></programme>` +
		`<programme start="20260622210000 +0300" channel="1"></programme>` +
		`</tv>`
	w := &memWriter{}
	res, err := parseXMLTV(context.Background(), strings.NewReader(doc), w)
	if err != nil {
		t.Fatal(err)
	}
	if res.programmes != 1 || len(w.progs) != 1 {
		t.Fatalf("programmes = %d (written %d), want 1", res.programmes, len(w.progs))
	}
	if len(res.channels) != 1 || res.channels[0].DisplayName != "Один" {
		t.Fatalf("channels = %+v", res.channels)
	}
	if w.progs[0].Desc != "d" {
		t.Fatalf("desc fallback = %q, want d", w.progs[0].Desc)
	}
}

func TestParseXMLTV_stopsOnContextCancel(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	sb.WriteString(`<tv><channel id="1"><display-name>One</display-name></channel>`)
	for i := 0; i < 2500; i++ {
		sb.WriteString(`<programme start="20260622210000 +0300" channel="1"><title>T</title></programme>`)
	}
	sb.WriteString(`</tv>`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := parseXMLTV(ctx, strings.NewReader(sb.String()), &memWriter{})
	if err == nil {
		t.Fatal("expected context error")
	}
	if res.programmes != 1000 {
		t.Fatalf("programmes = %d, want stop at first check (1000)", res.programmes)
	}
}
