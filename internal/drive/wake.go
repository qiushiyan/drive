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
	Failures int       // times its watch ended without a wake in this batch
	Again    time.Time // for a wake nobody acted on that is not due yet: when it is
}

// Due arms the wake of every session in a webhook batch that has none and
// needs one: never watched, acted on with no wake armed, a watch that ended
// without a wake or is stuck, a wake nothing was done about (once per view),
// a parked session waiting for a hold that is free. It is a chat driver's
// watchdog in one command. later are the wakes nobody acted on that it posts
// again on a later run, with when.
func (d *Driver) Due() (armed, later []Rearmed, err error) {
	b, err := d.Batch()
	if err != nil {
		return nil, nil, &Refused{Reason: err.Error()}
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
	argv := []string{d.Self, "--batch", b.Meta.Name, "wait", "--sleep", secs, "--note", note, "--notify"}
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
		// The watchdog posts a wake nobody acted on once more, for the same view.
		again := ""
		if evs, err := b.Events(); err == nil {
			if m := batch.Find(batch.Members(evs), label); m != nil && view != "" && m.WokenView == view && m.Woken.After(m.LastAction) {
				again = fmt.Sprintf("posted again: nothing was done after its wake at %s\n", m.Woken.In(b.Zone()).Format("15:04"))
			}
		}
		e.Waiting = d.waiterLines(b, e.Label) // ahead of next:, which the wake ends on
		payload["output"] = tail(again+e.Text(), 6000)
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
