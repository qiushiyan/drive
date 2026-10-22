// Package testkit is a fake world for drive's tests: blocks in a fake Rex
// whose Claude Code sessions write real transcript files in a temporary
// directory, and behave as the real ones were seen to — a slash command
// typed while busy is queued without its text, an expired login answers
// with an API error and no Stop, typing can lose a line's head.
package testkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/rex"
)

// World is the fake Rex and the fake machine under it.
type World struct {
	T    testing.TB
	Root string

	Clock *Clock

	mu      sync.Mutex
	blocks  map[string]*Claude
	order   []string
	nextPID int
	seq     int
	// OnOpen configures a session as Rex opens it.
	OnOpen func(c *Claude)
	// OnCapture runs once, the next time a screen is captured: another
	// driver acting between a driver's read and its action.
	OnCapture func()
}

// New makes a world under a temporary directory.
func New(t testing.TB) *World {
	return &World{T: t, Root: t.TempDir(), Clock: NewClock(), blocks: map[string]*Claude{}, nextPID: 1000}
}

func (w *World) now() time.Time { return w.Clock.Now() }

// Claude is one fake Claude Code session in a block.
type Claude struct {
	w       *World
	Block   string
	Label   string
	PID     int
	SID     string
	Config  string
	Cwd     string
	State   string
	Kind    string
	Msg     string
	Updated time.Time
	Running bool

	Input      string // typed into the box, not sent
	Suggestion string // Claude Code's dimmed guess
	DropHead   int    // characters the next typing loses
	LoseEnter  bool   // Enter does not reach the session: the line stays in the box
	// TypeGate, when set, holds every Type until it is closed: a send caught mid-delivery.
	TypeGate chan struct{}
	Shells   []time.Time

	tool  string // the tool call a permission prompt asks about
	stale bool   // the turn ended without a hook firing, so Rex's record is old

	// Reply decides what a taken prompt does. The default answers "ok" and ends the turn.
	Reply func(c *Claude, prompt string)

	Typed []string
	Keys  []string
}

// Add puts a running session in a new block.
func (w *World) Add(label string) *Claude {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.add(label, filepath.Join(w.Root, "work"), "acct@example.com")
}

// add starts a session on an account. Accounts share one projects
// directory, as the owner's do, so a session resumed on another account
// carries on in the same transcript.
func (w *World) add(label, cwd, account string) *Claude {
	w.seq++
	w.nextPID++
	id := fmt.Sprintf("block:00000000-0000-0000-0000-%012x", 0xa00000+w.seq)
	c := &Claude{w: w, Block: id, Label: label, PID: w.nextPID, SID: fmt.Sprintf("sid-%d", w.seq),
		Config: filepath.Join(w.Root, "accounts", account), Cwd: cwd,
		State: "idle", Updated: w.now(), Running: true}
	_ = os.MkdirAll(cwd, 0o755)
	_ = os.MkdirAll(filepath.Join(c.Config, "sessions"), 0o755)
	_ = os.MkdirAll(filepath.Join(w.Root, "projects", "proj"), 0o755)
	_ = os.Symlink(filepath.Join(w.Root, "projects"), filepath.Join(c.Config, "projects"))
	_ = os.WriteFile(filepath.Join(c.Config, "sessions", fmt.Sprint(c.PID)+".json"),
		[]byte(fmt.Sprintf(`{"sessionId":%q}`, c.SID)), 0o644)
	if w.OnOpen != nil {
		w.OnOpen(c)
	}
	w.blocks[id] = c
	w.order = append(w.order, id)
	return c
}

// Session finds the fake session in a block.
func (w *World) Session(block string) *Claude {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.blocks[block]
}

// Transcript is the session's transcript path.
func (c *Claude) Transcript() string {
	return filepath.Join(c.Config, "projects", "proj", c.SID+".jsonl")
}

// Write appends rows to the transcript as Claude Code would.
func (c *Claude) Write(rows ...map[string]any) {
	f, err := os.OpenFile(c.Transcript(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		c.w.T.Fatal(err)
	}
	defer f.Close()
	for _, r := range rows {
		if _, ok := r["timestamp"]; !ok {
			r["timestamp"] = c.w.now().Format(time.RFC3339Nano)
		}
		b, _ := json.Marshal(r)
		_, _ = f.Write(append(b, '\n'))
	}
}

var uuidN int
var uuidMu sync.Mutex

func uuid() string {
	uuidMu.Lock()
	defer uuidMu.Unlock()
	uuidN++
	return fmt.Sprintf("u-%06d", uuidN)
}

// Prompt records a typed prompt; a slash command as Claude Code marks it up.
func (c *Claude) Prompt(text string) {
	content := text
	if strings.HasPrefix(strings.TrimLeft(text, " "), "/") {
		t := strings.TrimLeft(text, " ")
		name, args, _ := strings.Cut(t, " ")
		content = fmt.Sprintf("<command-message>%s</command-message>\n<command-name>%s</command-name>\n<command-args>%s</command-args>",
			strings.TrimPrefix(name, "/"), name, args)
	}
	c.Write(map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user", "content": content}})
}

// Say records an assistant reply.
func (c *Claude) Say(text string) {
	c.Write(map[string]any{"type": "assistant", "uuid": uuid(), "message": map[string]any{"role": "assistant",
		"content": []map[string]any{{"type": "text", "text": text}}}})
}

// Ask records a tool call and puts the session on its permission prompt.
func (c *Claude) Ask(command string) {
	id := "toolu_" + uuid()
	c.Write(map[string]any{"type": "assistant", "uuid": uuid(), "message": map[string]any{"role": "assistant",
		"content": []map[string]any{{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}}}})
	c.w.mu.Lock()
	c.tool = id
	c.w.mu.Unlock()
	c.Set("blocked", "permission", "Claude needs your permission to use Bash")
}

// Fail records an API error ending the turn, as an expired login does: no
// Stop follows, so Rex keeps whatever it last heard unless told otherwise.
func (c *Claude) Fail(kind, text string) {
	c.Write(map[string]any{"type": "assistant", "uuid": uuid(), "isApiErrorMessage": true, "error": kind,
		"message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": text}}}})
}

// Set changes what the session publishes to Rex.
func (c *Claude) Set(state, kind, msg string) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.set(state, kind, msg)
}

func (c *Claude) set(state, kind, msg string) {
	c.State, c.Kind, c.Msg, c.Updated, c.stale = state, kind, msg, c.w.now(), false
}

// Busy puts the session mid-turn on a prompt.
func (c *Claude) Busy(prompt string) {
	c.Prompt(prompt)
	c.Set("working", "", "")
}

// Done ends the turn with a reply.
func (c *Claude) Done(reply string) {
	c.Say(reply)
	c.Set("done", "", "")
}

// Edit changes the session's fields under the world's lock.
func (c *Claude) Edit(fn func(c *Claude)) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	fn(c)
}

// Seen returns what was typed and pressed so far.
func (c *Claude) Seen() (typed, keys []string) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	return append([]string{}, c.Typed...), append([]string{}, c.Keys...)
}

// --- rex.Terminal ---

func (w *World) Status(block string) (*rex.Status, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.blocks[block]
	if !ok {
		return nil, fmt.Errorf("rex block call: no block %s", block)
	}
	if !c.Running {
		return nil, nil
	}
	return &rex.Status{State: c.State, Kind: c.Kind, Msg: c.Msg, UpdatedAt: c.Updated, PID: c.PID}, nil
}

func (w *World) Blocks(string) ([]rex.Block, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []rex.Block
	for i, id := range w.order {
		if c, ok := w.blocks[id]; ok {
			out = append(out, rex.Block{ID: id, Label: c.Label, Window: fmt.Sprint(i + 1), Session: "work"})
		}
	}
	return out, nil
}

func (w *World) Capture(block string, vt bool) (string, error) {
	w.mu.Lock()
	hook := w.OnCapture
	w.OnCapture = nil
	w.mu.Unlock()
	if hook != nil {
		hook()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	c, ok := w.blocks[block]
	if !ok {
		return "", fmt.Errorf("no block")
	}
	switch {
	case c.Input != "":
		return "╭───\n❯ " + c.Input + "\r\n╰───\n", nil
	case c.Suggestion != "" && vt:
		return "❯ \x1b[0m\x1b[2m" + c.Suggestion + "\x1b[0m\r\n", nil
	default:
		return "❯ \r\n", nil
	}
}

func (w *World) Type(block, text string) error {
	w.mu.Lock()
	c, ok := w.blocks[block]
	gate := (*Claude)(nil)
	if ok && c.TypeGate != nil {
		gate = c
	}
	w.mu.Unlock()
	if gate != nil {
		<-gate.TypeGate
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !ok {
		return fmt.Errorf("no block")
	}
	c.Typed = append(c.Typed, text)
	if c.DropHead > 0 && len(text) > c.DropHead {
		text = text[c.DropHead:]
		c.DropHead = 0
	}
	c.Input += text
	return nil
}

func (w *World) Key(block string, keys ...string) error {
	w.mu.Lock()
	c, ok := w.blocks[block]
	if !ok {
		w.mu.Unlock()
		return fmt.Errorf("no block")
	}
	var taken []string
	for _, k := range keys {
		c.Keys = append(c.Keys, k)
		switch k {
		case "enter":
			if c.LoseEnter {
				continue
			}
			if c.State == "blocked" && c.Kind == "permission" && !c.stale {
				c.Write(map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user",
					"content": []map[string]any{{"type": "tool_result", "tool_use_id": c.tool, "content": "approved"}}}})
				c.set("working", "", "") // PostToolUse, once the command ran
				continue
			}
			text := c.Input
			c.Input, c.Suggestion = "", ""
			if text == "" {
				continue
			}
			if c.State == "working" && !c.stale {
				op := map[string]any{"type": "queue-operation", "operation": "enqueue"}
				if !strings.HasPrefix(strings.TrimLeft(text, " "), "/") {
					op["content"] = text
				}
				c.Write(op)
				continue
			}
			c.Prompt(text)
			c.set("working", "", "")
			taken = append(taken, text)
		// No hook fires when a turn is interrupted, a denied prompt included:
		// the transcript records it and Rex keeps what it last heard.
		case "escape":
			if c.State == "blocked" && !c.stale {
				c.Write(map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user",
					"content": []map[string]any{{"type": "tool_result", "tool_use_id": c.tool, "is_error": true,
						"content": "The user doesn't want to proceed with this tool use."}}}},
					map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user",
						"content": []map[string]any{{"type": "text", "text": "[Request interrupted by user for tool use]"}}}})
				c.stale = true
			}
		case "ctrl+c":
			if c.State == "working" && !c.stale {
				c.Write(map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user", "content": "[Request interrupted by user]"}})
				c.stale = true
			}
		case "ctrl+u":
			c.Input = ""
		}
	}
	reply := c.Reply
	w.mu.Unlock()
	for _, text := range taken {
		if reply != nil {
			reply(c, text)
		} else {
			c.Done("ok")
		}
	}
	return nil
}

// Run opens a block running Claude Code. A --resume carries on the session
// it names, in the same transcript, as accounts share one projects directory.
func (w *World) Run(o rex.Open) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	account := "acct@example.com"
	for _, f := range strings.Fields(strings.Join(o.Command, " ")) {
		if strings.HasPrefix(f, "x-") {
			account = strings.TrimSuffix(strings.TrimPrefix(f, "x-"), ";")
		}
	}
	c := w.add(o.Label, o.Dir, account)
	if cmd := strings.Join(o.Command, " "); strings.Contains(cmd, "--resume ") {
		id := strings.Fields(strings.SplitN(cmd, "--resume ", 2)[1])[0]
		c.SID = strings.Trim(strings.TrimSuffix(id, ";"), "'")
		_ = os.WriteFile(filepath.Join(c.Config, "sessions", fmt.Sprint(c.PID)+".json"),
			[]byte(fmt.Sprintf(`{"sessionId":%q}`, c.SID)), 0o644)
	}
	return c.Block, nil
}

func (w *World) Close(block string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.blocks[block]; !ok {
		return fmt.Errorf("no block")
	}
	delete(w.blocks, block)
	return nil
}

// --- session.Host ---

func (w *World) Registry(pid int) (string, string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.blocks {
		if c.PID == pid {
			return c.SID, c.Config, nil
		}
	}
	return "", "", fmt.Errorf("no registry for %d", pid)
}

func (w *World) Cwd(pid int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.blocks {
		if c.PID == pid {
			return c.Cwd
		}
	}
	return ""
}

func (w *World) Shells(pid int) []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.blocks {
		if c.PID == pid {
			return c.Shells
		}
	}
	return nil
}

// Watches records the watches drive starts. Like the real ones, a name
// already running is refused.
type Watches struct {
	mu      sync.Mutex
	Running map[string][]string
	Fail    error // what Start returns, when set
}

func (w *Watches) Start(name string, argv []string, log string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.Fail != nil {
		return w.Fail
	}
	if w.Running == nil {
		w.Running = map[string][]string{}
	}
	if _, ok := w.Running[name]; ok {
		return fmt.Errorf("a watch named %s is already running", name)
	}
	w.Running[name] = argv
	return nil
}

func (w *Watches) Alive(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.Running[name]
	return ok
}

func (w *Watches) Stop(name string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.Running, name)
	return nil
}

func (w *Watches) Retire(name string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	_, ok := w.Running[name]
	delete(w.Running, name)
	return ok
}
