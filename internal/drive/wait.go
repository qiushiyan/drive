package drive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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
	Next    string // what the driver does about it, where the ending decides that
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
	misses := 0
	for {
		o, err := d.observe(members, ref)
		if err != nil {
			// One unreadable look is often Rex busy for a moment; three in a row is a session gone.
			var r *Refused
			if !errors.As(err, &r) {
				return nil, err
			}
			if misses++; misses < 3 {
				d.sleep(d.poll())
				continue
			}
			return &Ending{Label: ref, State: "gone", Msg: r.Reason,
				Next: fmt.Sprintf("drive status lists what runs; if %s still runs there, watch it again: drive watch %s", ref, ref)}, nil
		}
		misses = 0
		st := o.Session.Status
		key := o.State() + o.Session.Since().String()
		if key != last {
			last, stable = key, d.now()
		}
		held := d.now().Sub(stable)
		var promptAt time.Time
		if p := o.Frontier.Newest(); p != nil {
			promptAt = p.At
		}
		t := d.turnOf(o.Session, o.Frontier, promptAt)
		state := t.State
		ended := false
		switch {
		case !t.Over:
		case state == "blocked:auth" || state == "error" || state == "interrupted":
			ended = true // nothing will happen until someone acts
		case t.waiting() || t.Shells == 0:
			ended = held >= quiet
		default:
			ended = held >= bgCap
		}
		if ended {
			e := &Ending{Label: o.Label, Block: o.Session.Block, State: state, Msg: st.Msg, Error: o.Frontier.Error,
				Shells: t.Shells, Orphans: t.Orphans, Capped: t.Shells > 0, Pending: o.Frontier.Pending, View: o.View,
				Account: o.Session.Account}
			if a, local := o.Frontier.Answer(); a != nil {
				e.Reply = tail(answerText(o.Frontier, a, local), 2500)
			}
			if strings.HasPrefix(state, "blocked") {
				if screen, err := d.Term.Capture(o.Session.Block, false); err == nil {
					e.Screen = lastLines(screen, 25)
				}
			}
			e.Next = d.nextFor(o, state, e.Capped)
			return e, nil
		}
		d.sleep(5 * d.poll() / 2)
	}
}

// nextFor is what a driver runs about a session whose turn ended in state,
// where the ending decides it: the one answer the wake and drive read give.
func (d *Driver) nextFor(o *Observation, state string, capped bool) string {
	switch {
	case state == "blocked:auth":
		return d.relaunchHint(o)
	case strings.HasPrefix(state, "blocked"):
		return answerNext(o.Label, o.View.String(), state)
	case state == "error" && o.Frontier.Error != nil && o.Frontier.Error.Kind == "rate_limit":
		back := "read it again after the reset to see it under way"
		if webhook, _ := d.wakes(o.Session.Block); webhook {
			back = "drive watch --due arms its wake once it is under way again"
		}
		return fmt.Sprintf("if its screen says it continues automatically once the limit resets (drive read %s --screen), leave it: the turn resumes by itself, and %s; otherwise %s",
			o.Label, back, d.retryHint(o, "rate_limit"))
	case state == "error":
		kind := ""
		if o.Frontier.Error != nil {
			kind = o.Frontier.Error.Kind
		}
		return d.retryHint(o, kind)
	case state == "interrupted":
		return fmt.Sprintf("its turn was interrupted; read where it stopped (drive read %s) before its next step", o.Label)
	case capped:
		return fmt.Sprintf("if the reply says it waits on its own job, watch it again (drive watch %s); otherwise send its next step", o.Label)
	}
	return ""
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
		"interrupted": "its turn was interrupted",
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
	if e.Msg != "" && e.State == "gone" {
		fmt.Fprintf(&b, "why: %s\n", e.Msg)
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
	if e.Next != "" {
		fmt.Fprintf(&b, "next: %s\n", e.Next)
	}
	return b.String()
}

// watchName is a session's watch in a batch. A batch's watches are its own:
// one from an old batch neither blocks a new one's nor is taken for it.
func watchName(b *batch.Batch, block string) string {
	return "drive-" + safeName(b.Meta.Name) + "-" + rex.Short(block)
}

func timerName(b *batch.Batch, note string) string {
	return "drive-" + safeName(b.Meta.Name) + "-timer-" + safeName(note)
}

// arm starts the wake for a session unless one is already waiting on it. A
// watch takes itself off the books before it posts, so one still running
// waits on a turn that has not ended.
func (d *Driver) arm(b *batch.Batch, label, block string) error {
	if d.Watches == nil {
		return fmt.Errorf("this driver runs no watches")
	}
	if err := d.canPost(b); err != nil {
		return err
	}
	name := watchName(b, block)
	if d.Watches.Alive(name) {
		return nil
	}
	if err := d.Watches.Start(name, []string{d.Self, "--batch", b.Meta.Name, "wait", label, "--notify"}, watchLog(b)); err != nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Reason: "its wake did not start: " + err.Error()})
		return err
	}
	return b.Append(batch.Event{At: d.now(), Kind: batch.KindWatch, Session: label, Block: block})
}

// Watched reports a watch.
type Watched struct {
	Label     string
	Already   bool
	Restarted bool // the watch ran long after the turn ended and was started again
}

// Watch arms the wake for a session in a batch that wakes its driver by
// webhook, parked or not. It owns the session meanwhile, so it never races
// a send's own arming into two watches.
func (d *Driver) Watch(ref string) (*Watched, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	if b.Meta.Wake != "webhook" {
		return nil, &Refused{Reason: fmt.Sprintf("batch %s wakes no one (wake: %s)", b.Meta.Name, b.Meta.Wake),
			Next: "wait for it yourself, in the background: drive wait " + ref}
	}
	if err := d.canPost(b); err != nil {
		var nw *noWebhook
		errors.As(err, &nw)
		return nil, &Refused{Reason: err.Error(), Next: nw.next("drive watch " + ref)}
	}
	evs, err := b.Events()
	if err != nil {
		return nil, err
	}
	members := batch.Members(evs)
	label, block, err := d.target(members, ref)
	if err != nil {
		return nil, err
	}
	l, err := d.own(label, block, "arming "+label+"'s wake")
	if err != nil {
		return nil, err
	}
	defer l.release()
	o, err := d.observe(members, ref)
	if err != nil {
		return nil, err
	}
	name := watchName(b, o.Session.Block)
	if d.Watches.Alive(name) {
		var watched time.Time
		if m := batch.Find(members, o.Session.Block); m != nil {
			watched = m.Watched
		}
		if !stuck(o.State(), o.Session.Since(), watched, d.now()) {
			return &Watched{Label: o.Label, Already: true}, nil
		}
		if err := d.Watches.Stop(name); err != nil {
			return nil, err
		}
		return &Watched{Label: o.Label, Restarted: true}, d.arm(b, o.Label, o.Session.Block)
	}
	return &Watched{Label: o.Label}, d.arm(b, o.Label, o.Session.Block)
}

// noWebhook is a webhook batch whose driver's webhook holds no URL: no wake
// of it can post.
type noWebhook struct {
	batch, path string
}

func (e *noWebhook) Error() string {
	return fmt.Sprintf("batch %s has no webhook to wake its driver: %s holds no URL", e.batch, e.path)
}

// next is what to run once the webhook is written.
func (e *noWebhook) next(then string) string {
	return fmt.Sprintf("the owner writes its URL= and AUTH= lines, mode 0600, to %s; then: %s", e.path, then)
}

// canPost says whether a wake of the batch can reach its driver.
func (d *Driver) canPost(b *batch.Batch) error {
	if hook, err := d.Cfg.LoadWebhook(b.Meta.Webhook); err == nil && hook.URL != "" {
		return nil
	}
	return &noWebhook{batch: b.Meta.Name, path: d.Cfg.WebhookPath(b.Meta.Webhook)}
}

// Rearmed is a session whose wake Due armed, and why it was due.
type Rearmed struct {
	Label    string
	Why      string
	Failures int // times its watch ended without a wake in this batch
}

// Due arms the wake of every session in a webhook batch that has none and
// needs one: never watched, acted on with no wake armed, a watch that ended
// without a wake or is stuck, a wake nothing was done about (once per view),
// a parked session waiting for a hold that is free. It is a chat driver's
// watchdog in one command.
func (d *Driver) Due() ([]Rearmed, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, &Refused{Reason: err.Error()}
	}
	if b.Meta.Wake != "webhook" {
		return nil, &Refused{Reason: fmt.Sprintf("batch %s wakes no one (wake: %s)", b.Meta.Name, b.Meta.Wake)}
	}
	st, err := d.Status(false)
	if err != nil {
		return nil, err
	}
	evs, err := b.Events()
	if err != nil {
		return nil, err
	}
	var out []Rearmed
	for _, r := range st.Rows {
		if r.Due == "" || r.Error != "" {
			continue
		}
		if _, err := d.Watch(r.Label); err != nil {
			return out, err
		}
		n := 0
		for _, e := range evs {
			if e.Kind == batch.KindFailure && e.Session == r.Label && strings.HasPrefix(e.Reason, "watch ended without a wake") {
				n++
			}
		}
		out = append(out, Rearmed{Label: r.Label, Why: r.Due, Failures: n})
	}
	return out, nil
}

// StopWatches stops a batch's watches and timers once another batch has
// started: a wake from the old batch would reach the driver about sessions
// it now drives in the new one.
func (d *Driver) StopWatches(b *batch.Batch) (int, error) {
	if d.Watches == nil {
		return 0, nil
	}
	evs, err := b.Events()
	if err != nil {
		return 0, err
	}
	names := map[string]bool{}
	for _, e := range evs {
		switch {
		case e.Kind == batch.KindWatch && e.Block != "":
			names[watchName(b, e.Block)] = true
		case e.Kind == batch.KindWatch && strings.HasPrefix(e.Note, "timer "):
			names[timerName(b, strings.TrimPrefix(e.Note, "timer "))] = true
		}
	}
	n := 0
	for name := range names {
		if d.Watches.Alive(name) {
			if err := d.Watches.Stop(name); err != nil {
				return n, err
			}
			n++
		}
	}
	if n > 0 {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindNote, Note: fmt.Sprintf("stopped %d watches: a new batch started", n)})
	}
	return n, nil
}

// Timer arms a wake after a delay, such as the five-minute veto on a plan.
func (d *Driver) Timer(after time.Duration, note string) error {
	b, err := d.Batch()
	if err != nil {
		return &Refused{Reason: err.Error()}
	}
	name := timerName(b, note)
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

// Notify is a watch's own run: wait for the session's turn to end, take the
// watch off the books so a step sent from now on arms a fresh one, then wake
// the driver. watch is the watch's name, "" when run by hand.
func (d *Driver) Notify(watch, ref string, quiet, bgCap time.Duration) (*Ending, error) {
	e, err := d.Wait(ref, quiet, bgCap)
	if watch != "" && d.Watches != nil {
		d.Watches.Retire(watch)
	}
	if err != nil {
		d.WatchFailed(ref, err)
		return nil, err
	}
	if err := d.Wake(e.Label, e, ""); err != nil {
		d.WatchFailed(e.Label, err)
		return e, err
	}
	return e, nil
}

// Ring is a timer's own run once its delay is over.
func (d *Driver) Ring(watch, note string) error {
	if watch != "" && d.Watches != nil {
		d.Watches.Retire(watch)
	}
	if err := d.Wake("", nil, note); err != nil {
		d.WatchFailed("", err)
		return err
	}
	return nil
}

// Killed records a watch ended by a signal, unless drive stopped it: Stop
// takes a watch off the books before signalling it. It says whether the
// signal came from elsewhere.
func (d *Driver) Killed(watch, label string, sig os.Signal) bool {
	if d.Watches == nil || !d.Watches.Retire(watch) {
		return false
	}
	d.WatchFailed(label, fmt.Errorf("killed by %s", sig))
	return true
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
		view := ""
		if e.View.Session != "" {
			view = e.View.String()
		}
		payload["kind"], payload["session"], payload["state"], payload["view"] = "turn", e.Label, e.State, view
		payload["output"] = tail(e.Text()+d.waiterLines(b, e.Label), 6000)
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindPosting, Session: label, Block: e.Block, View: view})
	} else {
		payload["kind"], payload["output"] = "timer", "timer done: "+note
	}
	body, _ := json.Marshal(payload)
	hook, _ := d.Cfg.LoadWebhook(b.Meta.Webhook)
	poster := webhook.Poster{URL: hook.URL, Auth: hook.Auth, Sleep: d.Sleep}
	var last webhook.Attempt
	err = poster.Post(body, func(a webhook.Attempt) { last = a })
	ev := batch.Event{At: d.now(), Kind: batch.KindWake, Session: label, HTTP: last.HTTP, Try: last.Try, Note: note}
	if e != nil {
		ev.Block, ev.Outcome, ev.View = e.Block, e.State, e.View.String()
	}
	if err != nil {
		ev.HTTP, ev.Reason = 0, clip(err.Error(), 200)
		if last.Try > 0 {
			ev.Reason = fmt.Sprintf("http %d %s", last.HTTP, clip(last.Err, 200))
		}
	}
	_ = b.Append(ev)
	if err != nil {
		return err // no wake reached the chat, so there is nothing to follow up
	}
	for _, c := range append(append([]string{}, b.Meta.AfterWake...), d.Cfg.AfterWake...) {
		if out, cerr := afterWake(c, AfterWakeLimit); cerr != nil {
			_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindNote, Note: fmt.Sprintf("after_wake %q failed: %v: %s", c, cerr, clip(out, 200))})
		}
	}
	return nil
}

// AfterWakeLimit bounds each after-wake command: a watch that has posted
// its wake is off the books, so one left hanging would never be seen.
var AfterWakeLimit = 2 * time.Minute

// afterWake runs one of the batch's after-wake commands, killing it and
// whatever it started once limit has passed.
func afterWake(c string, limit time.Duration) (string, error) {
	cmd := exec.Command("/bin/sh", "-c", c)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return out.String(), err
	case <-time.After(limit):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		return out.String(), fmt.Errorf("still running after %s, killed", limit)
	}
}

// waiterLines names, for a session whose turn has ended, each session
// waiting on a hold it held, with the send to run once the hold is free.
func (d *Driver) waiterLines(b *batch.Batch, label string) string {
	evs, err := b.Events()
	if err != nil {
		return ""
	}
	members := batch.Members(evs)
	var out strings.Builder
	for _, h := range batch.Holds(evs) {
		if h.Session != label {
			continue
		}
		for _, w := range batch.Waiting(evs) {
			if w.Name != h.Name {
				continue
			}
			if w.Open {
				fmt.Fprintf(&out, "waiting for %s, which %s held: %s; open it once %s is free (drive status): %s\n",
					h.Name, label, w.Session, h.Name, w.Command())
				continue
			}
			view := w.Session + "@VIEW"
			if o, err := d.observe(members, w.Session); err == nil {
				view = o.View.String()
			}
			fmt.Fprintf(&out, "waiting for %s, which %s held: %s; send it once %s is free (drive status): drive send %s %s --hold %s\n",
				h.Name, label, w.Session, h.Name, view, w.Command(), h.Name)
		}
	}
	return out.String()
}

// holder says whether a hold is taken by a session other than label: its
// send is still under way, or the turn that send started has not ended. A
// holder that cannot be read holds; only one that is gone does not.
// heldBy is a hold as it stands: who holds it and why, and, where a job it
// started is what holds it, that job and the holder's view.
type heldBy struct {
	batch.Hold
	why  string
	job  *session.Shell
	view string
}

// holder says whether a hold is taken by a session other than label. It
// holds while the send that took it is under way, while its holder works,
// later steps included, since a follow-up to a closeout is still the
// closeout, while it waits on a prompt, and while background jobs it started
// since run. A holder that cannot be read holds; only one that is gone or
// never received the send does not.
func (d *Driver) holder(evs []batch.Event, members []*batch.Member, name, label string) (heldBy, bool) {
	for _, h := range batch.Holds(evs) {
		if h.Name != name || h.Session == label {
			continue
		}
		hb := heldBy{Hold: h, why: "whose turn is still running"}
		m := batch.Find(members, h.Session)
		if m == nil || m.Closed {
			return hb, false
		}
		if h.Pending && d.Store.Busy(m.Block) != nil {
			hb.why = "whose send is still under way"
			return hb, true
		}
		s, err := session.Locate(d.Term, d.Host, m.Block)
		if errors.Is(err, session.ErrNoClaude) || err != nil && !d.blockExists(m.Block) {
			return hb, false
		}
		if err != nil {
			hb.why = "whose session cannot be read"
			return hb, true
		}
		f, err := s.Frontier()
		if err != nil {
			hb.why = "whose session cannot be read"
			return hb, true
		}
		t := d.turnOf(s, f, h.Since)
		switch {
		case t.waiting():
			hb.why = "who waits on a prompt"
			return hb, true
		case t.State == "working" && sentSince(evs, h):
			hb.why = "who works on a later step"
			return hb, true
		case t.State == "working":
			return hb, true
		}
		if p := f.Newest(); p == nil || p.At.Before(h.Since) {
			return hb, false // the send never reached it
		}
		// A turn that ended while a job it started runs, such as a wait on
		// checks before a merge, resumes when the job ends.
		if t.Over && len(t.Jobs) > 0 {
			hb.job, hb.view = &t.Jobs[0], turn.ViewOf(m.Label, s.ID, f).String()
			hb.why = "whose turn ended, but a background job it started still runs: " + clip(t.Jobs[0].Command, 120)
		}
		return hb, !t.Over || t.Shells > 0
	}
	return heldBy{}, false
}

// sentSince says whether the holder was sent a step after the one that
// took the hold.
func sentSince(evs []batch.Event, h batch.Hold) bool {
	var took time.Time
	for _, e := range evs {
		switch {
		case e.Kind != batch.KindSend || e.Session != h.Session || e.Outcome == "":
		case e.Intent == h.Intent:
			took = e.At
		case !took.IsZero() && e.Mode != "notice" && e.At.After(took):
			return true
		}
	}
	return false
}

func (d *Driver) blockExists(block string) bool {
	blocks, err := d.Term.Blocks("")
	if err != nil {
		return true
	}
	for _, b := range blocks {
		if b.ID == block {
			return true
		}
	}
	return false
}
