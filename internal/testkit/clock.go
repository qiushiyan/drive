package testkit

import (
	"slices"
	"sync"
	"time"
)

// Clock is time for a test: Sleep moves it forward at once, so a wait that
// would take minutes takes microseconds, and the world and the driver
// read the same time.
type Clock struct {
	mu     sync.Mutex
	t      time.Time
	events []clockEvent
}

type clockEvent struct {
	at time.Time
	fn func()
}

// At runs fn once the clock reaches t, on the goroutine whose Sleep got it
// there, before that Sleep returns: something happening to a session at a set
// time while a watch polls, with no goroutine of its own to outlive the test.
func (c *Clock) At(t time.Time, fn func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, clockEvent{t, fn})
	slices.SortStableFunc(c.events, func(a, b clockEvent) int { return a.at.Compare(b.at) })
}

// NewClock starts a clock now.
func NewClock() *Clock { return NewClockAt(time.Now().UTC().Truncate(time.Second)) }

// NewClockAt starts a clock at t, for output pinned byte for byte.
func NewClockAt(t time.Time) *Clock { return &Clock{t: t} }

// Now reads the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Sleep moves the clock forward, runs what falls due, and yields, so other
// goroutines run.
func (c *Clock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	var due []func()
	for len(c.events) > 0 && !c.events[0].at.After(c.t) {
		due = append(due, c.events[0].fn)
		c.events = c.events[1:]
	}
	c.mu.Unlock()
	for _, fn := range due {
		fn()
	}
	time.Sleep(50 * time.Microsecond)
}
