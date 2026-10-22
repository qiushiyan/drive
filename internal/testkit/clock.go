package testkit

import (
	"sync"
	"time"
)

// Clock is time for a test: Sleep moves it forward at once, so a wait that
// would take minutes takes microseconds, and the world and the driver
// read the same time.
type Clock struct {
	mu sync.Mutex
	t  time.Time
}

// NewClock starts a clock now.
func NewClock() *Clock { return &Clock{t: time.Now().UTC().Truncate(time.Second)} }

// Now reads the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Sleep moves the clock forward and yields, so other goroutines run.
func (c *Clock) Sleep(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	time.Sleep(50 * time.Microsecond)
}
