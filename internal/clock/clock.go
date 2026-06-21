package clock

import "time"

// Clock abstracts time.Now() for testability.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func Real() Clock { return realClock{} }

func (realClock) Now() time.Time { return time.Now() }

// Fake is a deterministic clock for tests.
type Fake struct {
	T time.Time
}

func NewFake(t time.Time) *Fake { return &Fake{T: t} }

func (f *Fake) Now() time.Time { return f.T }

func (f *Fake) Set(t time.Time) { f.T = t }

func (f *Fake) Advance(d time.Duration) { f.T = f.T.Add(d) }
