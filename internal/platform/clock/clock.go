package clock

import (
	"sync"
	"time"
)

type System struct{}

func (System) Now() time.Time {
	return time.Now().UTC().Truncate(time.Microsecond)
}

type Fake struct {
	mu  sync.Mutex
	now time.Time
}

func NewFake(start time.Time) *Fake {
	return &Fake{now: start.UTC().Truncate(time.Microsecond)}
}

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}
