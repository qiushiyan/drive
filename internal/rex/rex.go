// Package rex is drive's view of Rex, the terminal multiplexer the sessions
// run in: the status Claude Code publishes for each block, the screen, typing
// and keys, and opening and closing blocks. CLI is the implementation that
// runs the `rex` command; tests substitute their own Terminal.
package rex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// ErrNoServer is a call that found no Rex server running. drive never
// starts one: a server takes the environment of the process that started
// it into every pane it opens, and one a watch's call started on 10-29 gave
// every pane a chat driver's NO_COLOR=1, FORCE_COLOR=0 and TERM=dumb.
var ErrNoServer = errors.New("Rex's server is not running")

// ErrNoSession is a Rex session, named by its label or id, that no longer
// runs, as after its server restarted.
var ErrNoSession = errors.New("no such Rex session")

// ErrNoBlock is a block that no longer runs: closed, or ended with its
// server.
var ErrNoBlock = errors.New("no such Rex block")

// IsSessionID says whether ref names a Rex session by its id, which dies
// with its server, rather than by its label, which a new session can take.
func IsSessionID(ref string) bool { return strings.HasPrefix(ref, "session:") }

// Status is what Claude Code last published for a block through its hooks:
// idle, working, done, blocked (Kind permission, question or auth) or error.
type Status struct {
	State     string
	Kind      string
	Msg       string
	UpdatedAt time.Time
	PID       int // the Claude Code process
}

// Block is one terminal block.
type Block struct {
	ID      string // "block:<uuid>"
	Label   string
	Window  string // the window number the user counts tabs by; "-" when detached
	Session string // the Rex session's label
}

// Short is the six characters people and the bot call a block by.
func Short(id string) string {
	if len(id) >= 6 {
		return id[len(id)-6:]
	}
	return id
}

// Open says how to start a block.
type Open struct {
	Session string
	Dir     string
	Label   string
	Command []string
}

// Terminal is everything drive asks of Rex.
type Terminal interface {
	// Status is Claude Code's record for the block; nil when no Claude runs there.
	Status(block string) (*Status, error)
	// Blocks lists the blocks of a session, or of every session when it is "".
	Blocks(session string) ([]Block, error)
	// Capture is the screen as text, or with its terminal sequences when vt.
	Capture(block string, vt bool) (string, error)
	// Type writes text into the block as if typed.
	Type(block, text string) error
	// Key presses keys, such as "enter", "escape", "ctrl+c".
	Key(block string, keys ...string) error
	// Run opens a block and returns its id; ErrNoSession when o's session
	// does not run.
	Run(o Open) (string, error)
	// New starts o's session, by its label, with the block as its first,
	// and returns the block's id. Rex gives two sessions one label, so a
	// caller that may race another runs New under a lock, after Run.
	New(o Open) (string, error)
	// Close closes a block.
	Close(block string) error
	// EnsureSession resolves ref, a Rex session's label or id, to its
	// label, starting a session under a label unless one runs, and says
	// whether it started it. An id names only a session that runs.
	EnsureSession(ref string) (label string, started bool, err error)
}

// CLI runs the rex command.
type CLI struct {
	Bin string // default "rex" from PATH
}

func (c CLI) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	if b := os.Getenv("DRIVE_REX"); b != "" {
		return b
	}
	return "rex"
}

// scoping are the variables a Rex pane sets for what runs in it, which
// scope the rex CLI's lookups to that pane's session, window, block and
// client: from a pane in another session, every block drive names reads as
// not found. drive names its sessions and blocks itself.
var scoping = []string{"REX_SESSION", "REX_WINDOW", "REX_BLOCK", "REX_CLIENT"}

// callError is a rex command that failed, as its stderr's first line says,
// and the kind of failure it was, if drive tells it apart.
type callError struct {
	msg  string
	kind error
}

func (e *callError) Error() string { return e.msg }
func (e *callError) Unwrap() error { return e.kind }

// run runs rex, never starting a server that is not running (ErrNoServer),
// and outside any pane's scope.
func (c CLI) run(args ...string) ([]byte, error) {
	cmd := exec.Command(c.bin(), append([]string{"--autostart=false"}, args...)...)
	cmd.Env = slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return slices.Contains(scoping, k)
	})
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		e := &callError{msg: fmt.Sprintf("rex %s: %s", args[0], firstLine(msg))}
		switch {
		case strings.Contains(msg, "server is not running"):
			e.kind = ErrNoServer
		case strings.Contains(msg, "session target") && strings.Contains(msg, "not found"):
			e.kind = ErrNoSession
		case strings.Contains(msg, "block target") && strings.Contains(msg, "not found"):
			e.kind = ErrNoBlock
		}
		return out, e
	}
	return out, nil
}

func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
	}
	return s
}

// Status implements Terminal. A block that no longer exists reads as an error.
func (c CLI) Status(block string) (*Status, error) {
	out, err := c.run("block", "call", "-b", block, "program_status")
	if err != nil {
		return nil, err
	}
	var st struct {
		Records []struct {
			ID        string `json:"id"`
			State     string `json:"state"`
			Kind      string `json:"kind"`
			Msg       string `json:"msg"`
			UpdatedAt string `json:"updated_at"`
			Owner     struct {
				PID int `json:"pid"`
			} `json:"owner"`
		} `json:"records"`
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return nil, fmt.Errorf("rex program_status for %s: %w", block, err)
	}
	for _, r := range st.Records {
		if r.ID == "claude" {
			at, _ := time.Parse(time.RFC3339Nano, r.UpdatedAt)
			return &Status{State: r.State, Kind: r.Kind, Msg: r.Msg, UpdatedAt: at, PID: r.Owner.PID}, nil
		}
	}
	return nil, nil
}

// Blocks implements Terminal.
func (c CLI) Blocks(session string) ([]Block, error) {
	sessions := []string{session}
	if session == "" {
		out, err := c.run("session", "ls")
		if err != nil {
			return nil, err
		}
		sessions = nil
		sc := bufio.NewScanner(bytes.NewReader(out))
		for first := true; sc.Scan(); first = false {
			if f := strings.Fields(sc.Text()); !first && len(f) > 0 {
				sessions = append(sessions, f[0])
			}
		}
	}
	var all []Block
	for _, s := range sessions {
		out, err := c.run("block", "ls", "-s", s, "--json")
		if err != nil {
			return nil, err
		}
		var ls struct {
			Blocks []struct {
				ID    string `json:"block_id"`
				Label string `json:"label"`
			} `json:"blocks"`
		}
		if err := json.Unmarshal(out, &ls); err != nil {
			return nil, fmt.Errorf("rex block ls -s %s: %w", s, err)
		}
		// The window number is only in the table form: WINDOW ID FLAVOR LABEL PLACEMENT.
		windows := map[string]string{}
		if table, err := c.run("block", "ls", "-s", s); err == nil {
			sc := bufio.NewScanner(bytes.NewReader(table))
			for sc.Scan() {
				if f := strings.Fields(sc.Text()); len(f) >= 2 {
					windows[f[1]] = f[0]
				}
			}
		}
		for _, b := range ls.Blocks {
			w := windows[Short(b.ID)]
			if w == "" {
				w = "-"
			}
			all = append(all, Block{ID: b.ID, Label: b.Label, Window: w, Session: s})
		}
	}
	return all, nil
}

// Capture implements Terminal.
func (c CLI) Capture(block string, vt bool) (string, error) {
	args := []string{"capture", "-b", block}
	if vt {
		args = append(args, "--format", "vt")
	} else {
		args = append(args, "--trim", "--unwrap")
	}
	out, err := c.run(args...)
	return string(out), err
}

// Type implements Terminal.
func (c CLI) Type(block, text string) error {
	_, err := c.run("send", "-b", block, text)
	return err
}

// Key implements Terminal.
func (c CLI) Key(block string, keys ...string) error {
	_, err := c.run(append([]string{"send-key", "-b", block}, keys...)...)
	return err
}

// Run implements Terminal.
func (c CLI) Run(o Open) (string, error) {
	args := []string{"run", "--focus=false", "--shell", "none", "--json", "-c", o.Dir, "--label", o.Label}
	if o.Session != "" {
		args = append(args, "-s", o.Session)
	}
	out, err := c.run(append(append(args, "--"), o.Command...)...)
	if err != nil {
		return "", err
	}
	var res struct {
		BlockIDs []string `json:"block_ids"`
	}
	if err := json.Unmarshal(out, &res); err != nil || len(res.BlockIDs) == 0 {
		return "", fmt.Errorf("rex run printed no block id: %s", firstLine(string(out)))
	}
	return res.BlockIDs[0], nil
}

// New implements Terminal.
func (c CLI) New(o Open) (string, error) {
	out, err := c.run(append([]string{"new", o.Session, "--json", "--shell", "none", "-c", o.Dir, "--"}, o.Command...)...)
	if err != nil {
		return "", err
	}
	var res struct {
		Windows []struct {
			BlockIDs []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	if err := json.Unmarshal(out, &res); err != nil || len(res.Windows) == 0 || len(res.Windows[0].BlockIDs) == 0 {
		return "", fmt.Errorf("rex new printed no block id: %s", firstLine(string(out)))
	}
	block := res.Windows[0].BlockIDs[0]
	// rex new labels no block. drive knows the block by its id; the label
	// only finds a block whose open its driver never recorded.
	_, _ = c.run("block", "rename", "-s", o.Session, block, o.Label)
	return block, nil
}

// EnsureSession implements Terminal.
func (c CLI) EnsureSession(ref string) (string, bool, error) {
	out, err := c.run("session", "ls", "--json")
	if err != nil {
		return "", false, err
	}
	var ls struct {
		Sessions []struct {
			ID    string `json:"session_id"`
			Label string `json:"label"`
		} `json:"sessions"`
	}
	if err := json.Unmarshal(out, &ls); err != nil {
		return "", false, fmt.Errorf("rex session ls: %w", err)
	}
	for _, s := range ls.Sessions {
		if s.ID == ref || s.Label == ref {
			return s.Label, false, nil
		}
	}
	if IsSessionID(ref) {
		return "", false, fmt.Errorf("no Rex session has id %s: %w", ref, ErrNoSession)
	}
	if _, err := c.run("session", "new", ref); err != nil {
		return "", false, err
	}
	return ref, true, nil
}

// Close implements Terminal.
func (c CLI) Close(block string) error {
	_, err := c.run("block", "close", block)
	return err
}
