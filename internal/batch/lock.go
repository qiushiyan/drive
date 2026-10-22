package batch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Owner is the driver acting on a session, as its lock file says.
type Owner struct {
	PID   int       `json:"pid"`
	What  string    `json:"what"`
	Since time.Time `json:"since"`
}

// Busy is an action refused because another driver is acting on the same
// session.
type Busy struct{ Owner }

func (b *Busy) Error() string {
	return fmt.Sprintf("another driver (pid %d) is %s", b.PID, b.What)
}

func (s Store) lockPath(key string) string {
	return filepath.Join(s.Dir, "locks", safe(key))
}

// Own takes the lock on one session for one action, held until release is
// called or the process ends. It does not queue behind another driver:
// whatever the second driver decided, it decided on what the first is
// changing, so it gets a *Busy naming the first.
func (s Store) Own(key, what string) (release func(), err error) {
	p := s.lockPath(key)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	// A Busy probe takes the lock for an instant; a real owner holds it for
	// seconds. A few short tries tell them apart.
	for try := 0; ; try++ {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || try == 4 {
			defer f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, &Busy{readOwner(p)}
			}
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := json.Marshal(Owner{PID: os.Getpid(), What: what, Since: time.Now()})
	_ = f.Truncate(0)
	_, _ = f.WriteAt(b, 0)
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// Busy says who is acting on a session, or nil when no driver is.
func (s Store) Busy(key string) *Owner {
	p := s.lockPath(key)
	f, err := os.OpenFile(p, os.O_RDWR, 0)
	if err != nil {
		return nil
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		o := readOwner(p)
		return &o
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return nil
}

func readOwner(p string) Owner {
	var o Owner
	if b, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(b, &o)
	}
	if o.What == "" {
		o.What = "acting on it"
	}
	return o
}
