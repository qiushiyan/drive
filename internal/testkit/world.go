// Package testkit is a fake world for drive's tests: blocks in a fake Rex
// whose Claude Code sessions write real transcript files in a temporary
// directory and keep a registry status, and behave as the real ones were
// seen to. The registry moves the moment the session does; Rex moves only
// when a hook would fire, so it lags a permission prompt by seconds, reads
// blocked through an approved command until its PostToolUse, reads done
// through a queued turn, hears nothing of a /compact, a deny or an
// interrupt, and can be told of a held message the session does not show.
// A slash command typed while busy is queued without its text, one behind
// spaces is words, an expired login answers with an API error and no Stop,
// typing can lose a line's head, which comes back as a line break.
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
	"github.com/qiushiyan/drive/internal/session"
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
	// StatusFails makes the next reads of Rex's records fail, as a busy
	// Rex server sometimes does.
	StatusFails int
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
	// What Claude Code's own registry says: idle, busy or waiting. It moves
	// the moment the session does; Rex moves only when a hook fires.
	Activity   string
	WaitingFor string
	ActivityAt time.Time
	ProcStart  string // as ps prints lstart in UTC

	Input      string // typed into the box, not sent
	Suggestion string // Claude Code's dimmed guess
	// Screen, when set, is what the block shows in place of the input box,
	// such as a full-screen view drawn over it: no ❯ line.
	Screen    string
	DropHead  int  // characters the next typing loses
	LoseEnter bool // Enter does not reach the session: the line stays in the box
	// TypeGate, when set, holds every Type until it is closed: a send caught mid-delivery.
	TypeGate chan struct{}
	Shells   []time.Time

	tool  string   // the tool call a permission prompt asks about
	stale bool     // the turn ended without a hook firing, so Rex's record is old
	queue []string // prompts typed while busy, oldest first; "" for a slash command
	// SlowTool keeps an approved tool call running until ToolDone: Rex
	// hears nothing until its PostToolUse, so it reads blocked meanwhile.
	SlowTool bool
	running  bool // an approved slow tool call runs
	// SlowCompact leaves a typed /compact running until Compacted.
	SlowCompact bool
	crashed     bool // the process is gone; Rex was never told

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
		State: "idle", Updated: w.now(), Running: true,
		Activity: "idle", ActivityAt: w.now(), ProcStart: w.now().UTC().Format("Mon Jan _2 15:04:05 2006")}
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

// command says whether Claude Code runs a typed line as a slash command:
// only one typed at the line's start. Behind spaces it is words.
func command(text string) bool { return strings.HasPrefix(text, "/") }

// Prompt records a typed prompt; a slash command as Claude Code marks it up.
func (c *Claude) Prompt(text string) {
	content := text
	if command(text) {
		name, args, _ := strings.Cut(text, " ")
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

// AskQuietly opens the permission prompt as Claude Code does: its registry
// reads waiting at once, and Rex hears of it only from the Notification
// hook some six seconds later, reading working until then.
func (c *Claude) AskQuietly(command string) {
	id := "toolu_" + uuid()
	c.Write(map[string]any{"type": "assistant", "uuid": uuid(), "message": map[string]any{"role": "assistant",
		"content": []map[string]any{{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{"command": command}}}}})
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.tool = id
	c.activity("waiting", "permission prompt")
}

// ToolDone ends an approved slow tool call: its result lands and its
// PostToolUse tells Rex the session works again.
func (c *Claude) ToolDone() {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	if !c.running {
		return
	}
	c.running = false
	c.Write(map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user",
		"content": []map[string]any{{"type": "tool_result", "tool_use_id": c.tool, "content": "done"}}}})
	c.set("working", "", "")
}

// HoldMessage is a message from another session waiting for approval: Rex
// is told the session is blocked on a permission prompt, while the session
// itself sits idle at its input box.
func (c *Claude) HoldMessage() {
	c.RexOnly("blocked", "permission", "A message from another session needs your approval")
}

// Fail records an API error ending the turn, as an expired login does: no
// Stop follows, so Rex keeps whatever it last heard unless told otherwise;
// the session itself is idle again.
func (c *Claude) Fail(kind, text string) {
	c.Write(map[string]any{"type": "assistant", "uuid": uuid(), "isApiErrorMessage": true, "error": kind,
		"message": map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": text}}}})
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.activity("idle", "")
}

// Set changes what the session publishes to Rex, and its own registry with
// it, as when a hook reports a change the session made.
func (c *Claude) Set(state, kind, msg string) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.set(state, kind, msg)
	switch {
	case state == "working":
		c.activity("busy", "")
	case state == "blocked" && kind == "permission":
		c.activity("waiting", "permission prompt")
	case state == "blocked" && kind == "question":
		c.activity("waiting", "question")
	default:
		c.activity("idle", "")
	}
}

// RexOnly changes what Rex reads and nothing else: a hook told Rex of
// something the session itself does not show.
func (c *Claude) RexOnly(state, kind, msg string) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.set(state, kind, msg)
}

func (c *Claude) set(state, kind, msg string) {
	c.State, c.Kind, c.Msg, c.Updated, c.stale = state, kind, msg, c.w.now(), false
}

func (c *Claude) activity(a, waitingFor string) {
	c.Activity, c.WaitingFor, c.ActivityAt = a, waitingFor, c.w.now()
}

// Busy puts the session mid-turn on a prompt.
func (c *Claude) Busy(prompt string) {
	c.Prompt(prompt)
	c.Set("working", "", "")
}

// Done ends the turn with a reply. A prompt typed while it ran is taken
// next: its prompt hook fired when it was queued, so Rex keeps reading the
// done this turn's Stop reported while the session works on it.
func (c *Claude) Done(reply string) {
	c.Say(reply)
	c.Set("done", "", "")
	c.takeQueued()
}

func (c *Claude) takeQueued() {
	c.w.mu.Lock()
	if len(c.queue) == 0 {
		c.w.mu.Unlock()
		return
	}
	text := c.queue[0]
	c.queue = c.queue[1:]
	c.activity("busy", "")
	reply := c.Reply
	c.w.mu.Unlock()
	c.Write(map[string]any{"type": "queue-operation", "operation": "dequeue"})
	c.Prompt(text)
	if reply != nil {
		reply(c, text)
	} else {
		c.Done("ok")
	}
}

// Compacted ends a /compact: the summary and the command's output land,
// and the session is idle. No hook tells Rex any of it.
func (c *Claude) Compacted() {
	c.Write(map[string]any{"type": "system", "subtype": "compact_boundary", "uuid": uuid()},
		map[string]any{"type": "user", "uuid": uuid(), "isCompactSummary": true, "message": map[string]any{"role": "user",
			"content": "This session is being continued from a previous conversation."}})
	c.Prompt("/compact")
	c.Write(map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user",
		"content": "<local-command-stdout>Compacted </local-command-stdout>"}})
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.activity("idle", "")
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
	if w.StatusFails > 0 {
		w.StatusFails--
		return nil, fmt.Errorf("rex block call: connection refused")
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
	case c.Screen != "":
		return c.Screen + "\r\n", nil
	case c.Activity == "waiting":
		// A prompt draws its options where the input box was, the chosen
		// one behind the ❯ the box uses, as seen live.
		return "Do you want to proceed?\r\n❯ 1. Yes\r\n  2. No, and tell Claude what to do differently (esc)\r\n", nil
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
		// A real lost head comes back as a line break in its place:
		// "\nopy-handoff-distill merged and deployed".
		text = "\n" + text[c.DropHead:]
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
	compact, interrupted := false, false
	for _, k := range keys {
		c.Keys = append(c.Keys, k)
		switch k {
		case "enter":
			if c.LoseEnter {
				continue
			}
			if c.Activity == "waiting" {
				// Enter approves; whatever was typed into the prompt is gone.
				c.Input = ""
				c.activity("busy", "")
				if c.SlowTool {
					c.running = true // Rex reads blocked until the tool's PostToolUse
					continue
				}
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
			if c.Activity == "busy" {
				// Queued: a slash command without its text. Its prompt hook fires now.
				op := map[string]any{"type": "queue-operation", "operation": "enqueue"}
				if !command(text) {
					op["content"] = text
				}
				c.Write(op)
				c.queue = append(c.queue, text)
				c.set("working", "", "")
				continue
			}
			if text == "/compact" || strings.HasPrefix(text, "/compact ") {
				// No prompt hook and no Stop: Rex keeps what it last heard. The
				// line is recorded as typed at once, the rest once it ends.
				c.Write(map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user", "content": text}})
				c.activity("busy", "")
				compact = true
				continue
			}
			c.Prompt(text)
			c.activity("busy", "")
			c.set("working", "", "")
			taken = append(taken, text)
		// No hook fires when a turn is interrupted, a denied prompt included:
		// the transcript records it and Rex keeps what it last heard. The
		// session itself is idle at once.
		case "escape":
			if c.Activity == "waiting" || c.running {
				c.Write(map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user",
					"content": []map[string]any{{"type": "tool_result", "tool_use_id": c.tool, "is_error": true,
						"content": "The user doesn't want to proceed with this tool use."}}}},
					map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user",
						"content": []map[string]any{{"type": "text", "text": "[Request interrupted by user for tool use]"}}}})
				c.activity("idle", "")
				c.running, c.stale = false, true
			}
		case "ctrl+c":
			if c.Activity == "busy" {
				c.Write(map[string]any{"type": "user", "uuid": uuid(), "message": map[string]any{"role": "user", "content": "[Request interrupted by user]"}})
				c.activity("idle", "")
				c.running, c.stale = false, true
				interrupted = true
			}
		case "ctrl+u":
			c.Input = ""
		}
	}
	reply, slow := c.Reply, c.SlowCompact
	w.mu.Unlock()
	for _, text := range taken {
		if reply != nil {
			reply(c, text)
		} else {
			c.Done("ok")
		}
	}
	if compact && !slow {
		c.Compacted()
	}
	if interrupted {
		c.takeQueued() // an interrupt takes the prompt queued behind it
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

func (w *World) Registry(pid int) (session.Registration, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.blocks {
		if c.PID == pid {
			return session.Registration{ID: c.SID, Config: c.Config, Status: c.Activity, WaitingFor: c.WaitingFor,
				StatusAt: c.ActivityAt, ProcStart: c.ProcStart}, nil
		}
	}
	return session.Registration{}, fmt.Errorf("no registry for %d", pid)
}

func (w *World) Started(pid int) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.blocks {
		if c.PID == pid && c.Running && !c.crashed {
			return c.ProcStart
		}
	}
	return ""
}

// Crash ends the process with no SessionEnd: Rex keeps its record.
func (c *Claude) Crash() {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	c.crashed = true
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
