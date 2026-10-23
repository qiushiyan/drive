// Package batch keeps a drive's record: one directory per batch of sessions
// driven together, holding an append-only event log, the immutable files
// sent to sessions, and the batch's settings. Everything a driver did and
// everything the owner said that grants or decides something is an event;
// what is true now (which sessions belong to the batch, what is held, what is
// still being sent) is derived from the log, never written separately.
package batch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"syscall"
	"time"
)

// Meta is a batch's settings, fixed when it starts.
type Meta struct {
	Name       string    `json:"name"`
	Started    time.Time `json:"started"`
	Wake       string    `json:"wake"`        // "webhook": drive wakes a chat driver; "none": the driver waits itself
	RexSession string    `json:"rex_session"` // where its sessions open
	TZ         string    `json:"tz"`
	// Webhook names the file that wakes this batch's driver, so two
	// drivers on one machine never wake each other.
	Webhook string `json:"webhook,omitempty"`
	// AfterWake runs after each wake is posted, such as archiving the chat it went to.
	AfterWake []string `json:"after_wake,omitempty"`
}

// Batch is one batch's directory.
type Batch struct {
	Meta Meta
	Dir  string
}

// Store is the directory holding every batch and the pointer to the current one.
type Store struct {
	Dir string
}

var nameForm = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ErrNoBatch is a store with no current batch.
var ErrNoBatch = errors.New("no current batch: start one with `drive batch new NAME`")

// New starts a batch and makes it current. A name already used is refused.
func (s Store) New(m Meta) (*Batch, error) {
	if !nameForm.MatchString(m.Name) {
		return nil, fmt.Errorf("batch name %q: use letters, digits, '.', '_' or '-', such as 2026-10-23", m.Name)
	}
	dir := filepath.Join(s.Dir, "batches", m.Name)
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return nil, err
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("batch %q exists; `drive batch use %s` makes it current", m.Name, m.Name)
		}
		return nil, err
	}
	for _, sub := range []string{"sends"} {
		if err := os.Mkdir(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, err
		}
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "batch.json"), append(b, '\n'), 0o644); err != nil {
		return nil, err
	}
	if err := s.Use(m.Name); err != nil {
		return nil, err
	}
	bt := &Batch{Meta: m, Dir: dir}
	return bt, bt.Append(Event{Kind: KindBatch, At: m.Started, Note: "started"})
}

// Use makes a batch current.
func (s Store) Use(name string) error {
	if _, err := s.Get(name); err != nil {
		return err
	}
	link := filepath.Join(s.Dir, "current")
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(filepath.Join("batches", name), tmp); err != nil {
		return err
	}
	return os.Rename(tmp, link)
}

// Get opens a batch by name.
func (s Store) Get(name string) (*Batch, error) {
	dir := filepath.Join(s.Dir, "batches", name)
	b, err := os.ReadFile(filepath.Join(dir, "batch.json"))
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("no batch %q; `drive batch list` shows them", name)
	}
	if err != nil {
		return nil, err
	}
	var m Meta
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Join(dir, "batch.json"), err)
	}
	return &Batch{Meta: m, Dir: dir}, nil
}

// Current opens the current batch.
func (s Store) Current() (*Batch, error) {
	target, err := os.Readlink(filepath.Join(s.Dir, "current"))
	if os.IsNotExist(err) {
		return nil, ErrNoBatch
	}
	if err != nil {
		return nil, err
	}
	return s.Get(filepath.Base(target))
}

// List names every batch, oldest first.
func (s Store) List() ([]Meta, error) {
	dirs, _ := filepath.Glob(filepath.Join(s.Dir, "batches", "*", "batch.json"))
	var out []Meta
	for _, d := range dirs {
		if b, err := s.Get(filepath.Base(filepath.Dir(d))); err == nil {
			out = append(out, b.Meta)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out, nil
}

// Zone is the batch's display zone.
func (b *Batch) Zone() *time.Location {
	if b.Meta.TZ != "" {
		if loc, err := time.LoadLocation(b.Meta.TZ); err == nil {
			return loc
		}
	}
	return time.Local
}

func (b *Batch) logPath() string { return filepath.Join(b.Dir, "events.jsonl") }

// Events reads the whole log.
func (b *Batch) Events() ([]Event, error) {
	data, err := os.ReadFile(b.logPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var evs []Event
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, fmt.Errorf("%s: a line does not parse: %w", b.logPath(), err)
		}
		evs = append(evs, e)
	}
	return evs, sc.Err()
}

// Append adds events to the log, each a whole line written at once.
func (b *Batch) Append(evs ...Event) error {
	f, err := os.OpenFile(b.logPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, e := range evs {
		if e.At.IsZero() {
			e.At = time.Now()
		}
		e.At = e.At.UTC()
		line, err := json.Marshal(e)
		if err != nil {
			return err
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			return err
		}
	}
	return nil
}

// Locked runs fn while holding the batch's lock, with the log as it stands.
// Every decision that admits an action (a send, an open, a hold) is made in
// here, so two drivers acting at once see each other's intents.
func (b *Batch) Locked(fn func(evs []Event) error) error {
	f, err := os.OpenFile(filepath.Join(b.Dir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	evs, err := b.Events()
	if err != nil {
		return err
	}
	return fn(evs)
}

// SaveSend writes a step's text to a new file under sends/, never
// overwriting one: what a session was told stays as it was.
func (b *Batch) SaveSend(session string, body []byte) (string, error) {
	dir := filepath.Join(b.Dir, "sends")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for n := 1; ; n++ {
		p := filepath.Join(dir, fmt.Sprintf("%s-%d.md", safe(session), n))
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o444)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, werr := f.Write(body)
		cerr := f.Close()
		return p, errors.Join(werr, cerr)
	}
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safe(s string) string { return unsafeChars.ReplaceAllString(s, "_") }
