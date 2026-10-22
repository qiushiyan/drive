package drive

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
	"github.com/qiushiyan/drive/internal/webhook"
)

// Ending is how a session's turn ended.
type Ending struct {
	Label   string
	Block   string
	State   string // done, blocked:permission, blocked:question, blocked:auth, error, gone
	Msg     string // what Rex was told, such as the prompt's question
	Error   *turn.APIError
	Shells  int // background shells started in this turn, still running
	Orphans int // background shells older than this turn's prompt
	Capped  bool
	Reply   string // the newest reply's end
	Pending *turn.ToolCall
	Screen  string // the screen's end, for a prompt the transcript does not show
	View    turn.View
	Account string
}

// Wait blocks until a session's turn ends and has stayed ended for quiet: it
// is done with no shell this turn started still running, it waits on a
// prompt, or an API error ended it. A turn that ended while its own
// background jobs run usually resumes when they finish but may also have
// ended on a question, so after bgCap the wait returns anyway.
func (d *Driver) Wait(ref string, quiet, bgCap time.Duration) (*Ending, error) {
	_, _, members, err := d.members()
	if err != nil {
		return nil, err
	}
	var last string
	stable := d.now()
	for {
		o, err := d.observe(members, ref)
		if err != nil {
			if _, ok := err.(*Refused); ok {
				return &Ending{Label: ref, State: "gone", Msg: err.Error()}, nil
			}
			return nil, err
		}
		st := o.Session.Status
		key := st.State + st.UpdatedAt.String()
		if key != last {
			last, stable = key, d.now()
		}
		held := d.now().Sub(stable)
		var promptAt time.Time
		if p := o.Frontier.Newest(); p != nil {
			promptAt = p.At
		}
		shells, orphans := 0, 0
		for _, at := range d.Host.Shells(st.PID) {
			if !promptAt.IsZero() && at.Before(promptAt) {
				orphans++
			} else {
				shells++
			}
		}
		state := o.State()
		ended := false
		switch {
		case o.Frontier.Error != nil || st.Kind == "auth" || state == "error":
			ended = true // nothing will happen until someone acts
		case strings.HasPrefix(state, "blocked"):
			ended = held >= quiet
		case state == "done" && shells == 0:
			ended = held >= quiet
		case state == "done":
			ended = held >= bgCap
		}
		if ended {
			e := &Ending{Label: o.Label, Block: o.Session.Block, State: state, Msg: st.Msg, Error: o.Frontier.Error,
				Shells: shells, Orphans: orphans, Capped: shells > 0, Pending: o.Frontier.Pending, View: o.View,
				Account: o.Session.Account}
			if o.Frontier.Reply != nil {
				e.Reply = tail(o.Frontier.Reply.Text, 2500)
			}
			if strings.HasPrefix(state, "blocked") {
				if screen, err := d.Term.Capture(o.Session.Block, false); err == nil {
					e.Screen = lastLines(screen, 25)
				}
			}
			return e, nil
		}
		d.sleep(5 * d.poll() / 2)
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// Text renders an ending for a reader deciding the next step.
func (e *Ending) Text() string {
	var b strings.Builder
	what := map[string]string{
		"done": "its turn ended", "blocked:permission": "it waits on a permission prompt",
		"blocked:question": "it waits on a question", "blocked:auth": "its login was rejected",
		"error": "an API error ended its turn", "gone": "its session is gone",
	}[e.State]
	if what == "" {
		what = e.State
	}
	fmt.Fprintf(&b, "%s (%s): %s\n", e.Label, rex.Short(e.Block), what)
	if e.Error != nil {
		fmt.Fprintf(&b, "error: %s — %q (account %s)\n", e.Error.Kind, e.Error.Text, e.Account)
	}
	if e.Capped {
		fmt.Fprintf(&b, "background: %d shell(s) this turn started still run; the turn may resume when they finish, or it ended on a question — read the reply\n", e.Shells)
	}
	if e.Orphans > 0 {
		fmt.Fprintf(&b, "background: %d shell(s) older than this turn's prompt (left over, not waited on)\n", e.Orphans)
	}
	if e.Pending != nil && strings.HasPrefix(e.State, "blocked") {
		fmt.Fprintf(&b, "pending %s: %s\n", e.Pending.Name, clip(e.Pending.Input, 400))
	}
	if e.Msg != "" && strings.HasPrefix(e.State, "blocked") {
		fmt.Fprintf(&b, "prompt: %s\n", e.Msg)
	}
	if e.Reply != "" {
		fmt.Fprintf(&b, "--- reply (end) ---\n%s\n", e.Reply)
	}
	if e.Screen != "" {
		fmt.Fprintf(&b, "--- screen (end) ---\n%s\n", e.Screen)
	}
	if e.View.Session != "" {
		fmt.Fprintf(&b, "view: %s\n", e.View)
	}
	return b.String()
}

// Watches runs a wait in the background so it outlives the command that
// armed it, and stops it. What the wait prints goes to log, so one that
// ends early says why.
type Watches interface {
	Start(name string, argv []string, log string) error
	Alive(name string) bool
	Stop(name string) error
}

// Tmux keeps each watch in a detached tmux session: a chat driver's shell
// commands end, and a process they started would end with them.
type Tmux struct{}

// carried are the variables a watch needs from the command that armed it; a
// tmux session otherwise starts from the tmux server's environment.
func carried() []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "HOME" || strings.HasPrefix(k, "DRIVE_") || k == "REX_SERVER" {
			out = append(out, "-e", kv)
		}
	}
	return out
}

func (Tmux) Start(name string, argv []string, log string) error {
	args := append([]string{"new-session", "-d", "-s", name}, carried()...)
	args = append(args, "--", "/bin/sh", "-c", `exec "$@" >>"$0" 2>&1`, log)
	out, err := exec.Command("tmux", append(args, argv...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux new-session %s: %s", name, strings.TrimSpace(string(out)))
	}
	return nil
}

func (Tmux) Alive(name string) bool {
	return exec.Command("tmux", "has-session", "-t", "="+name).Run() == nil
}

func (Tmux) Stop(name string) error {
	if !(Tmux{}).Alive(name) {
		return nil
	}
	return exec.Command("tmux", "kill-session", "-t", "="+name).Run()
}

func watchName(block string) string { return "drive-" + rex.Short(block) }

// arm starts the wake for a session unless one is already running.
func (d *Driver) arm(b *batch.Batch, label, block string) error {
	if d.Watches == nil {
		return fmt.Errorf("no watches")
	}
	name := watchName(block)
	if d.Watches.Alive(name) {
		return nil
	}
	if err := d.Watches.Start(name, []string{d.Self, "--batch", b.Meta.Name, "wait", label, "--notify"}, watchLog(b)); err != nil {
		return err
	}
	return b.Append(batch.Event{At: d.now(), Kind: batch.KindWatch, Session: label, Block: block})
}

// Watched reports a watch.
type Watched struct {
	Label   string
	Already bool
}

// Watch arms the wake for a session in a batch that wakes its driver by webhook.
func (d *Driver) Watch(ref string) (*Watched, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	if b.Meta.Wake != "webhook" {
		return nil, &Refused{Reason: fmt.Sprintf("batch %s wakes no one (wake: %s)", b.Meta.Name, b.Meta.Wake),
			Next: "run `drive wait SESSION` in the background instead"}
	}
	if d.Cfg.Webhook.URL == "" {
		return nil, &Refused{Reason: "no webhook is configured", Next: "write URL= and AUTH= lines to " + d.Cfg.WebhookPath()}
	}
	evs, err := b.Events()
	if err != nil {
		return nil, err
	}
	o, err := d.observe(batch.Members(evs), ref)
	if err != nil {
		return nil, err
	}
	if d.Watches.Alive(watchName(o.Session.Block)) {
		return &Watched{Label: o.Label, Already: true}, nil
	}
	return &Watched{Label: o.Label}, d.arm(b, o.Label, o.Session.Block)
}

// Timer arms a wake after a delay, such as the five-minute veto on a plan.
func (d *Driver) Timer(after time.Duration, note string) error {
	b, err := d.Batch()
	if err != nil {
		return &Refused{Reason: err.Error()}
	}
	name := "drive-timer-" + safeName(note)
	if d.Watches.Alive(name) {
		return &Refused{Reason: fmt.Sprintf("a timer named %q is already running", note), Next: "give this one another note"}
	}
	argv := []string{d.Self, "--batch", b.Meta.Name, "wait", "--sleep", fmt.Sprint(int(after.Seconds())), "--note", note, "--notify"}
	if err := d.Watches.Start(name, argv, watchLog(b)); err != nil {
		return err
	}
	return b.Append(batch.Event{At: d.now(), Kind: batch.KindWatch, Note: "timer " + note})
}

func watchLog(b *batch.Batch) string { return filepath.Join(b.Dir, "watches.log") }

// WatchFailed records a watch that ended without posting its wake.
func (d *Driver) WatchFailed(label string, err error) {
	if b, berr := d.Batch(); berr == nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Reason: "watch ended without a wake: " + firstLine(err.Error())})
	}
}

func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

// Wake posts an ending or a timer to the driver's webhook and records how
// the post went: an empty log line is never taken to mean the wake arrived.
func (d *Driver) Wake(label string, e *Ending, note string) error {
	b, err := d.Batch()
	if err != nil {
		return err
	}
	host, _ := os.Hostname()
	payload := map[string]any{"batch": b.Meta.Name, "host": host}
	if e != nil {
		payload["kind"], payload["session"], payload["state"], payload["view"] = "turn", e.Label, e.State, e.View.String()
		payload["output"] = tail(e.Text(), 6000)
	} else {
		payload["kind"], payload["output"] = "timer", "timer done: "+note
	}
	body, _ := json.Marshal(payload)
	poster := webhook.Poster{URL: d.Cfg.Webhook.URL, Auth: d.Cfg.Webhook.Auth, Sleep: d.Sleep}
	var last webhook.Attempt
	err = poster.Post(body, func(a webhook.Attempt) { last = a })
	ev := batch.Event{At: d.now(), Kind: batch.KindWake, Session: label, HTTP: last.HTTP, Try: last.Try, Note: note}
	if e != nil {
		ev.Block, ev.Outcome, ev.View = e.Block, e.State, e.View.String()
	}
	if err != nil {
		ev.HTTP, ev.Reason = 0, fmt.Sprintf("http %d %s", last.HTTP, clip(last.Err, 200))
	}
	_ = b.Append(ev)
	for _, c := range d.Cfg.AfterWake {
		if out, cerr := exec.Command("/bin/sh", "-c", c).CombinedOutput(); cerr != nil {
			_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindNote, Note: fmt.Sprintf("after_wake %q failed: %s", c, clip(string(out), 200))})
		}
	}
	return err
}

// holder says whether a hold is taken by a session other than label whose
// turn, started by the send that took it, has not ended yet.
func (d *Driver) holder(evs []batch.Event, members []*batch.Member, name, label string) (batch.Hold, bool) {
	for _, h := range batch.Holds(evs) {
		if h.Name != name || h.Session == label {
			continue
		}
		m := batch.Find(members, h.Session)
		if m == nil || m.Closed {
			return h, false
		}
		s, err := session.Locate(d.Term, d.Host, m.Block)
		if err != nil {
			return h, false // the session is gone
		}
		if s.Status.State == "working" || strings.HasPrefix(s.Status.State, "blocked") && s.Status.Kind != "auth" {
			return h, true
		}
		f, _ := s.Frontier()
		return h, f.Last == nil || f.Last.At.Before(h.Since)
	}
	return batch.Hold{}, false
}
