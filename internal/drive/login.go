package drive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
)

// gone says a session could not be read because it no longer runs: its
// block ended, as every block does when Rex's server restarts, or its Claude
// Code did. A read that merely failed says neither.
func gone(err error) bool {
	return errors.Is(err, rex.ErrNoBlock) || errors.Is(err, session.ErrNoClaude)
}

// reopen says how to bring back a member whose session is gone: its own
// conversation resumed under its label, where it worked, on its account, or
// the launcher's choice when the primary's launcher is unknown here, since
// every account resumes it from the shared transcript. Its park and the
// record of what was sent to it carry over.
func (d *Driver) reopen(m *batch.Member) string {
	opt := OpenOptions{Dir: m.Dir, Resume: m.SID, Account: m.Account}
	if opt.Account == "primary" && d.Cfg.PrimaryName == "" {
		opt.Account = ""
	}
	return "bring it back under its label, its conversation resumed where it stopped: " + d.openCommand(m.Label, opt, Step{})
}

// otherAccounts names the accounts a session could be relaunched on: not
// the one it runs on, and none whose login this batch saw rejected. The
// primary (~/.claude, which a session reports as "primary") is one when the
// name its launcher takes is known.
func (d *Driver) otherAccounts(b *batch.Batch, current string) []string {
	rejected := map[string]bool{current: true}
	if b != nil {
		evs, _ := b.Events()
		for _, e := range evs {
			if e.Account != "" && (e.Kind == batch.KindFailure || e.Kind == batch.KindRefused) {
				rejected[e.Account] = true
			}
		}
	}
	dirs, _ := filepath.Glob(filepath.Join(d.Cfg.Home, ".claude-accounts", "*"))
	var out []string
	for _, dir := range dirs {
		if st, err := os.Stat(filepath.Join(dir, "sessions")); err == nil && st.IsDir() && !rejected[filepath.Base(dir)] {
			out = append(out, filepath.Base(dir))
		}
	}
	sort.Strings(out)
	if d.Cfg.PrimaryName != "" && !rejected["primary"] {
		if st, err := os.Stat(filepath.Join(d.Cfg.Home, ".claude", "sessions")); err == nil && st.IsDir() {
			out = append(out, "primary")
		}
	}
	return out
}

// relaunchHint says how to carry a session whose login was rejected over to
// another account, for a driver that has its step to hand.
func (d *Driver) relaunchHint(o *Observation) string { return d.relaunch(o, lastStep(o.in, o.Label)) }

// lastStep is the step a session was last sent in this batch, as drive open
// takes it: what a session whose login was rejected was to take. A file or
// a snippet goes again as the copy drive saved of it.
func lastStep(b *batch.Batch, label string) Step {
	if b != nil {
		evs, _ := b.Events()
		for _, e := range slices.Backward(evs) {

			if e.Kind != batch.KindSendIntent || e.Session != label || e.Mode == "notice" {
				continue
			}
			if e.Path != "" {
				return Step{File: e.Path}
			}
			return Step{Line: stepOf(e)}
		}
	}
	return Step{}
}

// relaunch says how to carry a session whose login was rejected over to
// another account: close it on its view as it stands now, and resume it
// there with step, as drive open takes it, on the open.
func (d *Driver) relaunch(o *Observation, step Step) string {
	others := d.otherAccounts(o.in, o.Session.Account)
	if len(others) == 0 {
		return "every other account's login was rejected in this batch too: tell the owner, and park the session until they log in again"
	}
	// Claude Code finds a session to resume under its working directory's project.
	cmd := d.run("close", viewNow(o)) + " && " + d.openCommand(o.Label, OpenOptions{Dir: o.Session.Dir, Resume: o.Session.ID, Account: others[0]}, step)
	what := "relaunch it on another account with the step it was to take"
	if step.empty() {
		what = "relaunch it on another account, then send it the step it was to take on the view the open prints"
	}
	if jobs := d.Host.Shells(o.Session.Status.PID); len(jobs) > 0 {
		return fmt.Sprintf("%s once its background job (%s) ends, as close refuses until then: %s", what, clip(jobs[0].Command, 120), cmd)
	}
	return what + ": " + cmd
}

func (d *Driver) retryHint(o *Observation, kind string) string {
	if kind == "authentication_failed" {
		return d.relaunchHint(o)
	}
	if webhook, _ := d.wakes(o); webhook {
		// No wake comes for an API recovering: a timer brings the driver back.
		return "the session is idle again; send the step again once the API recovers, and have a timer bring you back to it: " + d.run("timer", "900", text("retry "+o.Label))
	}
	return fmt.Sprintf("the session is idle again; read it (%s) and send the step again when the API recovers", d.run("read", o.Label))
}
