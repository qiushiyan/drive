package drive

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/turn"
	"github.com/qiushiyan/drive/internal/webhook"
)

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
	if err := d.Watches.Start(name, d.rerun(b, "notify", label), watchLog(b)); err != nil {
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
		return nil, err
	}
	if b.Meta.Wake != "webhook" {
		return nil, &Refused{Reason: fmt.Sprintf("batch %s wakes no one (wake: %s)", b.Meta.Name, b.Meta.Wake),
			Next: "wait for it yourself, in the background: " + d.run("wait", ref)}
	}
	if err := d.canPost(b); err != nil {
		var nw *noWebhook
		errors.As(err, &nw)
		return nil, &Refused{Reason: err.Error(), Next: nw.next(d.run("watch", ref))}
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
	release, err := d.own(b, label, block, "arming "+label+"'s wake")
	if err != nil {
		return nil, err
	}
	defer release()
	if err := d.ownedHere(b, label, block); err != nil {
		return nil, err
	}
	o, err := d.observe(b, members, ref)
	if err != nil {
		return nil, err
	}
	name := watchName(b, o.Session.Block)
	if d.Watches.Alive(name) {
		m := batch.Find(members, o.Session.Block)
		stable := o.Session.Since()
		if m != nil && m.Watched.After(stable) {
			stable = m.Watched
		}
		if !factsOf(o, d.turnNow(o), m, stable, WakeQuiet).plan().stuck(d.now()) {
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
	Failures int       // times its watch ended without a wake in this batch
	Again    time.Time // for a wake nobody acted on that is not due yet: when it is
	Gone     string    // for a session that is gone: how to bring it back
	Park     *parkNeed // for a park whose --until has come or that nothing ends
}

// Due arms the wake of every session in a webhook batch that has none and
// needs one: never watched, acted on with no wake armed, a watch that ended
// without a wake, was stopped or is stuck, a wake nothing was done about
// (once per view), a parked session waiting for a hold that is free. It is a
// chat driver's watchdog in one command. later are the wakes nobody acted on
// that it posts again on a later run, with when, and the sessions that are
// gone, with the open that brings each back and arms its wake.
func (d *Driver) Due() (armed, later []Rearmed, err error) {
	b, err := d.Batch()
	if err != nil {
		return nil, nil, err
	}
	if b.Meta.Wake != "webhook" {
		return nil, nil, &Refused{Reason: fmt.Sprintf("batch %s wakes no one (wake: %s)", b.Meta.Name, b.Meta.Wake)}
	}
	st, err := d.Status(false)
	if err != nil {
		return nil, nil, err
	}
	evs, err := b.Events()
	if err != nil {
		return nil, nil, err
	}
	var out []Rearmed
	for _, r := range st.Rows {
		if !r.Again.IsZero() {
			later = append(later, Rearmed{Label: r.Label, Why: r.Watch, Again: r.Again})
		}
		if r.Next != "" {
			later = append(later, Rearmed{Label: r.Label, Gone: r.Next})
		}
		if n := d.parkNeedOf(r); n != nil {
			later = append(later, Rearmed{Label: r.Label, Park: n})
		}
		if r.Due == "" || r.Error != "" {
			continue
		}
		if _, err := d.Watch(r.Label); err != nil {
			if len(out) == 0 {
				return out, later, err
			}
			// The wakes armed so far stay armed, so this is no refusal.
			why, next := explain(err)
			return out, later, &Failed{What: fmt.Sprintf("armed the wakes above, then %s's did not arm: %s", r.Label, why), Next: next}
		}
		n := 0
		for _, e := range evs {
			if e.Kind == batch.KindFailure && e.Session == r.Label && strings.HasPrefix(e.Reason, "watch ended without a wake") {
				n++
			}
		}
		out = append(out, Rearmed{Label: r.Label, Why: r.Due, Failures: n})
	}
	return out, later, nil
}

// Stopped is a live batch whose wakes StopAll stopped, by its sessions' labels.
type Stopped struct {
	Batch  *batch.Batch
	Labels []string
	Due    string // the watch --due that arms them again
}

// StopAll stops the wake of every session in every live batch, whichever
// driver drives it, before Rex's server restarts: a watch that meets the
// restart wakes its driver about sessions the owner is bringing back. Timers
// run on, as they never reach Rex. watch --due in each batch arms the wakes
// again once a server runs.
func (d *Driver) StopAll() ([]Stopped, error) {
	live, err := d.Store.Live()
	if err != nil || d.Watches == nil {
		return nil, err
	}
	var out []Stopped
	for _, b := range live {
		evs, err := b.Events()
		if err != nil {
			return out, err
		}
		s := Stopped{Batch: b, Due: d.runIn(b.Meta.Name, "watch", "--due")}
		for _, m := range batch.Members(evs) {
			if name := watchName(b, m.Block); !m.Closed && d.Watches.Alive(name) {
				if err := d.Watches.Stop(name); err != nil {
					return out, &Failed{What: fmt.Sprintf("stopped the wakes above, then %s's in batch %s did not stop: %v", m.Label, b.Meta.Name, err),
						Next: "try it again: " + d.runIn("", "watch", "--stop-all")}
				}
				_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindWakeStop, Session: m.Label, Block: m.Block})
				s.Labels = append(s.Labels, m.Label)
			}
		}
		out = append(out, s)
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
		return err
	}
	secs := fmt.Sprint(int(after.Seconds()))
	if b.Meta.Wake != "webhook" {
		return &Refused{Reason: fmt.Sprintf("batch %s wakes no one (wake: %s)", b.Meta.Name, b.Meta.Wake),
			Next: "wait it out yourself, in the background: sleep " + secs}
	}
	if err := d.canPost(b); err != nil {
		var nw *noWebhook
		errors.As(err, &nw)
		return &Refused{Reason: err.Error(), Next: nw.next(d.run("timer", secs, text(note)))}
	}
	name := timerName(b, note)
	if d.Watches.Alive(name) {
		return &Refused{Reason: fmt.Sprintf("a timer named %q is already running", note), Next: "give this one another note"}
	}
	if err := d.Watches.Start(name, d.rerun(b, "ring", secs, "--", note), watchLog(b)); err != nil {
		return err
	}
	return b.Append(batch.Event{At: d.now(), Kind: batch.KindWatch, Note: "timer " + note})
}

func watchLog(b *batch.Batch) string { return filepath.Join(b.Dir, "watches.log") }

// rerun is the command line a watch or timer runs: this drive's own hidden
// command, in batch b, so a wake from an old batch never acts in a new one.
func (d *Driver) rerun(b *batch.Batch, words ...string) []string {
	return append([]string{d.Self, "--batch", b.Meta.Name}, words...)
}

// WatchFailed records a watch that ended without posting its wake.
func (d *Driver) WatchFailed(label string, err error) {
	if b, berr := d.Batch(); berr == nil {
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindFailure, Session: label, Reason: "watch ended without a wake: " + firstLine(err.Error())})
	}
}

// Notify is a watch's own run: wait for the session's turn to end, take the
// watch off the books so a step sent from now on arms a fresh one, then wake
// the driver. A turn that ended while its jobs run keeps its watch: the job's
// end usually resumes the session, and that turn's end is what the driver is
// waiting to hear, so nobody has to arm it again. watch is the watch's name,
// "" when run by hand.
func (d *Driver) Notify(watch, ref string, quiet time.Duration) (*Ending, error) {
	b, err := d.Batch()
	if err != nil {
		return nil, err
	}
	for {
		var p pace
		e, err := d.wait(ref, func(o *Observation, t Turn, stable time.Time) wakePlan {
			return factsOf(o, t, d.memberNow(b, o.Label), stable, quiet).plan()
		}, func(e *Ending) bool {
			if p.ready.IsZero() {
				p.ready = d.now()
			}
			return d.claim(b, e, &p)
		})
		stays := err == nil && e.State == "done" && len(e.Jobs) > 0 && !e.Covered
		if watch != "" && d.Watches != nil && !stays {
			d.Watches.Retire(watch)
		}
		if err != nil {
			d.WatchFailed(ref, err)
			return nil, err
		}
		if e.Covered {
			return e, nil
		}
		err = d.Wake(e.Label, e, "")
		if !stays {
			if err != nil {
				d.WatchFailed(e.Label, err)
			}
			return e, err
		}
	}
}

// memberNow reads a member afresh from the log, since a watch outlasts what it
// read when it began.
func (d *Driver) memberNow(b *batch.Batch, label string) *batch.Member {
	evs, err := b.Events()
	if err != nil {
		return nil
	}
	return batch.Find(batch.Members(evs), label)
}

// pace is a wake waiting its turn to post: when it was ready, and what last held
// it back.
type pace struct {
	ready  time.Time
	heldBy string
}

// held notes what holds the wake, and says whether anything does.
func (p *pace) held(evs []batch.Event, now time.Time) bool {
	by := wakeHeld(evs, now, p.ready)
	if by != "" {
		p.heldBy = by
	}
	return by != ""
}

// claim says whether e's wake posts now, which only one watch of a batch does
// at a time. It leaves e carrying the other sessions whose turns have ended, so
// they share the post.
func (d *Driver) claim(b *batch.Batch, e *Ending, p *pace) bool {
	evs, err := b.Events()
	if err != nil {
		return true // an unreadable log costs an extra wake, never a lost one
	}
	if p.held(evs, d.now()) {
		return false
	}
	also := d.carried(b, evs, e)
	posts := false
	err = b.Locked(func(evs []batch.Event) error {
		if p.held(evs, d.now()) {
			return nil
		}
		posts = true
		e.post.heldBy = p.heldBy
		view := ""
		if e.View.Session != "" {
			view = e.View.String()
		}
		rows := []batch.Event{{At: d.now(), Kind: batch.KindPosting, Session: e.Label, Block: e.Block, View: view, Why: e.post.why, Due: e.post.due, HeldBy: e.post.heldBy}}
		for _, c := range also {
			rows = append(rows, batch.Event{At: d.now(), Kind: batch.KindPosting, Session: c.Label, Block: c.Block, View: c.View, Via: e.Label, Why: c.post.why, Due: c.post.due})
		}
		return b.Append(rows...)
	})
	if err != nil {
		return true
	}
	if posts {
		e.Also, e.claimed = also, true
	}
	return posts
}

// carried is the other sessions whose turns are over at a view no wake has
// carried, each with its own watch armed: this post tells the driver of them
// all. A turn whose job still runs goes along only when it asks.
func (d *Driver) carried(b *batch.Batch, evs []batch.Event, e *Ending) []Carried {
	var out []Carried
	members := batch.Members(evs)
	for _, m := range members {
		if m.Label == e.Label || m.Closed || m.Parked != "" || d.Watches == nil || !d.Watches.Alive(watchName(b, m.Block)) {
			continue
		}
		o, err := d.observe(b, members, m.Label)
		if err != nil {
			continue
		}
		view := o.View.String()
		t := d.turnNow(o)
		switch {
		case !t.Over, m.WokenTimes > 0 && m.WokenView == view:
			continue
		case !t.State.onPrompt() && t.Shells+t.Orphans > 0 && replyReads(o) != turn.ReadsAsking:
			continue
		}
		jobs := d.Host.Shells(o.Session.Status.PID)
		next := d.run("read", m.Label) // the post carries no reply of its
		if n := d.nextFor(o, t.State, jobs); n != "" {
			next = "read its reply first (" + next + "); then " + n
		}
		plan := factsOf(o, t, m, o.Session.Since(), WakeQuiet).plan()
		out = append(out, Carried{Label: m.Label, Block: m.Block, State: t.State, View: view, Next: next, Jobs: len(jobs), post: posting{why: plan.why, due: plan.at}})
	}
	return out
}

// Ring is a timer's own run once its delay is over. While the driver is not
// answering it waits too, since a timer posted into that is a timer lost, and
// stays on the books meanwhile, so ending its batch still stops it.
func (d *Driver) Ring(watch, note string) error {
	p := posting{why: whyTimer, due: d.now()}
	if b, err := d.Batch(); err == nil {
		for {
			evs, err := b.Events()
			if err != nil || !d.now().Before(stalled(batch.Hearings(evs, d.now()))) {
				break
			}
			if p.heldBy == "" {
				_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindHeld, Note: note})
				p.heldBy = heldStalled
			}
			d.sleep(d.poll() * 15)
		}
	}
	if watch != "" && d.Watches != nil {
		d.Watches.Retire(watch)
	}
	if err := d.post("", nil, note, p); err != nil {
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
	var p posting
	if e != nil {
		p = e.post
	}
	return d.post(label, e, note, p)
}

// post is Wake with what its record says of it.
func (d *Driver) post(label string, e *Ending, note string, p posting) error {
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
		// The watchdog posts a wake nobody acted on once more, for the same view.
		again := ""
		if evs, err := b.Events(); err == nil {
			if m := batch.Find(batch.Members(evs), label); m != nil && view != "" && m.WokenView == view && m.Woken.After(m.LastAction) {
				again = fmt.Sprintf("posted again: nothing was done after its wake at %s\n", m.Woken.In(b.Zone()).Format("15:04"))
			}
		}
		e.Waiting = d.waiterLines(b, e.Label) // ahead of next:, which the wake ends on
		if e.Next == "" && e.State == "done" {
			e.Next = fmt.Sprintf("decide from its reply: %s; %s (when it waits on the owner, record its ask and leave out --until); or %s",
				d.run("send", view, text("…")), d.run("park", view, "--until", "WHEN", text("…")), d.run("close", view))
		}
		if strings.HasPrefix(e.Reply, "…") && e.Next != "" {
			// What it asks may stand in what the wake cut.
			e.Next = fmt.Sprintf("its reply is cut above: read all of it first (%s); then %s", d.run("read", e.Label), e.Next)
		}
		first := d.briefLine() + again
		payload["output"] = first + tail(e.Text(), 6000-len(first))
		if !e.claimed {
			_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindPosting, Session: label, Block: e.Block, View: view, Why: p.why, Due: p.due, HeldBy: p.heldBy})
		}
		if len(e.Also) > 0 {
			var also []string
			for _, c := range e.Also {
				also = append(also, c.Label)
			}
			payload["also"] = also
		}
	} else {
		if what, ok := strings.CutPrefix(note, watchdogNote); ok {
			payload["kind"], payload["output"] = "watchdog", d.briefLine()+what
		} else {
			payload["kind"], payload["output"] = "timer", d.briefLine()+"timer done: "+note
		}
		_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindPosting, Why: p.why, Due: p.due, HeldBy: p.heldBy})
	}
	body, _ := json.Marshal(payload)
	hook, _ := d.Cfg.LoadWebhook(b.Meta.Webhook)
	poster := webhook.Poster{URL: hook.URL, Auth: hook.Auth, Sleep: d.Sleep}
	var last webhook.Attempt
	err = poster.Post(body, func(a webhook.Attempt) { last = a })
	ev := batch.Event{At: d.now(), Kind: batch.KindWake, Session: label, HTTP: last.HTTP, Try: last.Try, Note: clip(note, 300), Why: p.why}
	if e != nil {
		ev.Block, ev.Outcome, ev.View, ev.Jobs = e.Block, string(e.State), e.View.String(), len(e.Jobs)
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
	if e != nil {
		for _, c := range e.Also {
			_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindWake, Session: c.Label, Block: c.Block, HTTP: last.HTTP, Try: last.Try,
				Outcome: string(c.State), View: c.View, Via: e.Label, Jobs: c.Jobs, Why: c.post.why})
		}
	}
	d.runAfterWake(b)
	return nil
}

// briefLine heads every wake: a routine's prompt, not the bot's persona,
// starts the run a wake starts, so without this it may never read how a chat
// driver talks. It says so while the file it names exists.
func (d *Driver) briefLine() string {
	if d.Cfg.Brief == "" {
		return ""
	}
	if _, err := os.Stat(d.Cfg.Brief); err != nil {
		return ""
	}
	return fmt.Sprintf("Before you message anyone, read %s § The chat: one message per event, and the result is the first thing the user reads.\n", d.Cfg.Brief)
}

// AfterWakeEvery is how often a batch's after-wake commands run: they archive
// a chat whose window holds 200 entries, which no wake's own run has even
// added to yet, so they need no run per wake.
const AfterWakeEvery = time.Hour

// runAfterWake runs the batch's after-wake commands, unless they ran within
// AfterWakeEvery.
func (d *Driver) runAfterWake(b *batch.Batch) {
	cmds := append(append([]string{}, b.Meta.AfterWake...), d.Cfg.AfterWake...)
	if len(cmds) == 0 {
		return
	}
	evs, err := b.Events()
	if err != nil {
		return
	}
	for _, e := range slices.Backward(evs) {
		if e.Kind == batch.KindAfterWake {
			if d.now().Sub(e.At) < AfterWakeEvery {
				return
			}
			break
		}
	}
	_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindAfterWake})
	for _, c := range cmds {
		if out, cerr := afterWake(c, AfterWakeLimit); cerr != nil {
			_ = b.Append(batch.Event{At: d.now(), Kind: batch.KindNote, Note: fmt.Sprintf("after_wake %q failed: %v: %s", c, cerr, clip(out, 200))})
		}
	}
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

// A batch with open asks wakes its driver this often to recheck them, since
// the session an ask holds is parked and wakes no one. The rest of what the
// watchdog finds is posted as it is found.
const askRecheck = 6 * time.Hour

// DueReport is what the machine's watchdog did for one live batch.
type DueReport struct {
	Batch  string
	Armed  []Rearmed
	Later  []Rearmed
	Posted string // what it woke the driver about; "" when nothing needed a model
	Held   string // what needs the driver and waits, since the driver is not answering
	Err    error
}

// DueAll is a machine's watchdog, which runs no model: for every live batch
// that wakes its driver, it arms what is due, and posts a wake only for what
// needs the driver's judgment and no session's own wake will carry: a session
// that is gone, a watch that keeps failing, open asks to recheck. Nothing due
// means no post, so it can run every few minutes for free.
func (d *Driver) DueAll() ([]DueReport, error) {
	live, err := d.Store.Live()
	if err != nil {
		return nil, err
	}
	var out []DueReport
	for _, b := range live {
		if b.Meta.Wake != "webhook" {
			continue
		}
		in := *d
		in.BatchName, in.DriverName, in.bound, in.boundFor = b.Meta.Name, "", nil, ""
		r := DueReport{Batch: b.Meta.Name}
		r.Armed, r.Later, r.Err = in.Due()
		r.Posted, r.Held = in.postDue(b, r.Armed, r.Later)
		out = append(out, r)
	}
	return out, nil
}

// postDue wakes the driver about what the watchdog cannot settle itself, each
// thing once, on a post of its own. It reports what it posted, or what waits.
func (d *Driver) postDue(b *batch.Batch, armed, later []Rearmed) (posted, held string) {
	evs, err := b.Events()
	if err != nil {
		return "", ""
	}
	members := batch.Members(evs)
	now := d.now()
	var lines []string
	var rows []batch.Event
	var parks []parkNeed
	for _, r := range later {
		m := batch.Find(members, r.Label)
		if r.Gone != "" && m != nil && noticeDue(m, "gone", now) {
			lines = append(lines, fmt.Sprintf("%s is gone: %s", r.Label, r.Gone))
			rows = append(rows, batch.Event{Kind: batch.KindTold, Session: m.Label, Block: m.Block, Outcome: "gone"})
		}
		// A park nothing ends, or whose time has come, is told once, and
		// again only while no run has heard it, since parking it again or
		// sending it a step clears it.
		if r.Park != nil && m != nil && noticeDue(m, batch.NoticePark, now) {
			parks = append(parks, *r.Park)
			rows = append(rows, batch.Event{Kind: batch.KindTold, Session: m.Label, Block: m.Block, Outcome: batch.NoticePark})
		}
	}
	lines = append(lines, d.parkLines(parks, b.Zone())...)
	for _, r := range armed {
		if m := batch.Find(members, r.Label); r.Failures >= 2 && m != nil && noticeDue(m, "watch-failing", now) {
			lines = append(lines, fmt.Sprintf("%s's watch has ended without a wake %d times this batch; %s says why, so tell the owner once and record that you did: %s", r.Label, r.Failures,
				filepath.Join(b.Dir, "watches.log"), d.run("record", "note", "--session", r.Label, text("told the owner: …"))))
			rows = append(rows, batch.Event{Kind: batch.KindTold, Session: m.Label, Block: m.Block, Outcome: "watch-failing"})
		}
	}
	_, asks, _ := batch.Records(evs)
	var open []string
	newest := b.Meta.Started
	for _, a := range asks {
		if a.Answer == nil {
			open = append(open, a.ID)
		}
		if a.At.After(newest) {
			newest = a.At
		}
	}
	for _, e := range evs {
		if e.Kind == batch.KindWake && e.At.After(newest) {
			newest = e.At
		}
	}
	if len(open) > 0 && d.now().Sub(newest) >= askRecheck {
		lines = append(lines, fmt.Sprintf("no wake for %s while %s stay open: recheck each, since another session or a teammate may have settled it, and record what no longer holds: %s",
			ago(d.now().Sub(newest)), strings.Join(open, ", "), d.run("status")))
	}
	if len(lines) == 0 {
		return "", ""
	}
	what := strings.Join(lines, "\n")
	if wakeHeld(evs, d.now(), d.now()) != "" {
		return "", what
	}
	// On record before the post, as a run can answer before it returns.
	for i := range rows {
		rows[i].At = d.now()
	}
	_ = b.Append(rows...)
	if err := d.post("", nil, watchdogNote+what, posting{why: whyWatchdog, due: d.now()}); err != nil {
		for i := range rows {
			rows[i].At, rows[i].Reason = d.now(), clip(err.Error(), 200)
		}
		_ = b.Append(rows...)
		return "", what
	}
	return what, ""
}

// watchdogNote heads what the watchdog wakes a driver about, which a timer's
// note never does.
const watchdogNote = "watchdog: "
