package batch

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

// Several drivers share one machine, each a chat bot woken by its own
// webhook or a session that waits itself, and each drives a batch of its
// own. Which batch each drives now is the store's lifecycle log,
// drivers.jsonl beside the batches: a batch becomes its driver's current
// when it starts or is used, and stops being live when it ends or another of
// its driver's takes over. A driver that starts a batch never touches
// another driver's.

// The kinds of change.
const (
	ChangeCurrent = "current" // the batch is its driver's current one
	ChangeEnded   = "ended"   // the batch ended, and its driver drives none
)

// Change is one change to which batch a driver drives.
type Change struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Driver string    `json:"driver"` // "" for a driver that names none
	Batch  string    `json:"batch"`
}

// Drivers is which batch each driver drives, as the lifecycle log stands.
type Drivers struct {
	Current map[string]string    // driver to its current batch
	Ended   map[string]time.Time // batch to when it ended, unless used since
}

// DriverOf is who drives a batch: the name it was started under, else the
// webhook that wakes its driver; "" for a driver that named none.
func DriverOf(m Meta) string { return cmp.Or(m.Driver, m.Webhook) }

func (s Store) changesPath() string { return filepath.Join(s.Dir, "drivers.jsonl") }

// Drivers reads which batch each driver drives now.
func (s Store) Drivers() (Drivers, error) {
	if _, err := os.Lstat(s.changesPath()); os.IsNotExist(err) {
		if _, err := os.Lstat(filepath.Join(s.Dir, "current")); err == nil {
			// A store an older drive kept: its one current batch becomes its driver's.
			if err := s.changing(func(Drivers) ([]Change, error) { return nil, nil }); err != nil {
				return Drivers{}, err
			}
		}
	}
	return s.readDrivers()
}

func (s Store) readDrivers() (Drivers, error) {
	d := Drivers{Current: map[string]string{}, Ended: map[string]time.Time{}}
	data, err := os.ReadFile(s.changesPath())
	if os.IsNotExist(err) {
		return d, nil
	}
	if err != nil {
		return d, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var c Change
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			return d, fmt.Errorf("%s: a line does not parse: %w", s.changesPath(), err)
		}
		switch c.Kind {
		case ChangeCurrent:
			d.Current[c.Driver] = c.Batch
			delete(d.Ended, c.Batch)
		case ChangeEnded:
			if d.Current[c.Driver] == c.Batch {
				delete(d.Current, c.Driver)
			}
			d.Ended[c.Batch] = c.At
		}
	}
	return d, sc.Err()
}

// changing runs fn under the store's lock with the lifecycle as it stands,
// and appends the changes it returns. A store an older drive kept, with one
// current batch for every driver, is carried over first.
func (s Store) changing(fn func(Drivers) ([]Change, error)) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.Dir, ".drivers.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if err := s.carryOver(); err != nil {
		return err
	}
	d, err := s.readDrivers()
	if err != nil {
		return err
	}
	changes, err := fn(d)
	if err != nil || len(changes) == 0 {
		return err
	}
	out, err := os.OpenFile(s.changesPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	for _, c := range changes {
		c.At = c.At.UTC()
		line, err := json.Marshal(c)
		if err != nil {
			return err
		}
		if _, err := out.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// carryOver starts the lifecycle log of a store that has none, from the
// one `current` link an older drive kept, then removes the link, which no
// driver reads any longer. Its caller holds the store's lock.
func (s Store) carryOver() error {
	if _, err := os.Lstat(s.changesPath()); err == nil {
		return nil
	}
	var start []byte
	link := filepath.Join(s.Dir, "current")
	if target, err := os.Readlink(link); err == nil {
		if b, err := s.Get(filepath.Base(target)); err == nil {
			c := Change{At: time.Now().UTC(), Kind: ChangeCurrent, Driver: DriverOf(b.Meta), Batch: b.Meta.Name}
			line, _ := json.Marshal(c)
			start = append(line, '\n')
		}
	}
	if err := os.WriteFile(s.changesPath(), start, 0o644); err != nil {
		return err
	}
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Current opens the batch driver drives now; ErrNoBatch when it drives none.
func (s Store) Current(driver string) (*Batch, error) {
	d, err := s.Drivers()
	if err != nil {
		return nil, err
	}
	name, ok := d.Current[driver]
	if !ok {
		return nil, ErrNoBatch
	}
	return s.Get(name)
}

// Live opens every driver's current batch, oldest first.
func (s Store) Live() ([]*Batch, error) {
	d, err := s.Drivers()
	if err != nil {
		return nil, err
	}
	var out []*Batch
	for _, name := range d.Current {
		b, err := s.Get(name)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Meta.Started.Before(out[j].Meta.Started) })
	return out, nil
}

// Use makes a batch its driver's current one at a time, and returns the
// batch it replaces, nil when there was none or it was this one.
func (s Store) Use(name string, at time.Time) (replaced *Batch, err error) {
	b, err := s.Get(name)
	if err != nil {
		return nil, err
	}
	driver := DriverOf(b.Meta)
	err = s.changing(func(d Drivers) ([]Change, error) {
		if prev := d.Current[driver]; prev != "" && prev != name {
			if replaced, err = s.Get(prev); err != nil {
				return nil, err
			}
		}
		return []Change{{At: at, Kind: ChangeCurrent, Driver: driver, Batch: name}}, nil
	})
	return replaced, err
}

// End ends a batch its driver drives now, at a time, so it is no longer live.
func (s Store) End(name string, at time.Time) error {
	b, err := s.Get(name)
	if err != nil {
		return err
	}
	driver := DriverOf(b.Meta)
	return s.changing(func(d Drivers) ([]Change, error) {
		if d.Current[driver] != name {
			return nil, fmt.Errorf("batch %s is not live: no driver drives it now", name)
		}
		return []Change{{At: at, Kind: ChangeEnded, Driver: driver, Batch: name}}, nil
	})
}
