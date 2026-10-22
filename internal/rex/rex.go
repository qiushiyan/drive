// Package rex is drive's view of Rex, the terminal multiplexer the sessions
// run in: the status Claude Code publishes for each block, the screen, typing
// and keys, and opening and closing blocks. CLI is the implementation that
// runs the `rex` command; tests substitute their own Terminal.
package rex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

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
	// Run opens a block and returns its id.
	Run(o Open) (string, error)
	// Close closes a block.
	Close(block string) error
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

func (c CLI) run(args ...string) ([]byte, error) {
	cmd := exec.Command(c.bin(), args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return out, fmt.Errorf("rex %s: %s", args[0], firstLine(msg))
	}
	return out, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
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

// Close implements Terminal.
func (c CLI) Close(block string) error {
	_, err := c.run("block", "close", block)
	return err
}
