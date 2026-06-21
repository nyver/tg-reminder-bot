package provider

import "fmt"

// Registry holds all registered providers by type string.
type Registry struct {
	events  map[string]EventProvider
	metrics map[string]MetricProvider
	search  map[string]SearchProvider
}

func NewRegistry() *Registry {
	return &Registry{
		events:  make(map[string]EventProvider),
		metrics: make(map[string]MetricProvider),
		search:  make(map[string]SearchProvider),
	}
}

func (r *Registry) RegisterEvent(p EventProvider) {
	r.events[p.Type()] = p
}

func (r *Registry) RegisterMetric(p MetricProvider) {
	r.metrics[p.Type()] = p
}

func (r *Registry) RegisterSearch(p SearchProvider) {
	r.search[p.Type()] = p
}

func (r *Registry) Event(typ string) (EventProvider, bool) {
	p, ok := r.events[typ]
	return p, ok
}

func (r *Registry) Metric(typ string) (MetricProvider, bool) {
	p, ok := r.metrics[typ]
	return p, ok
}

func (r *Registry) Search(typ string) (SearchProvider, bool) {
	p, ok := r.search[typ]
	return p, ok
}

// MustEvent panics if provider not found — for use during wiring only.
func (r *Registry) MustEvent(typ string) EventProvider {
	p, ok := r.events[typ]
	if !ok {
		panic(fmt.Sprintf("event provider %q not registered", typ))
	}
	return p
}
