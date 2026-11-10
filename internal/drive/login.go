package drive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/skill"
	"github.com/qiushiyan/drive/internal/turn"
)

// gone says a session could not be read because it no longer runs: its
// block ended, as every block does when Rex's server restarts, or its Claude
// Code did. A read that merely failed says neither.
func gone(err error) bool {
	return errors.Is(err, rex.ErrNoBlock) || errors.Is(err, session.ErrNoClaude)
}

// workedIn is where m's session last stood, by its transcript f, read here
// when the caller has none: a session that moved into a worktree left the
// directory it opened in, and resumed there it would stand on the main
// checkout. A directory since removed leaves the one it opened in.
func (d *Driver) workedIn(m *batch.Member, f *turn.Frontier) string {
	if f == nil {
		read, err := turn.ReadFile(d.Host.Transcript(m.SID))
		if err != nil {
			return m.Dir
		}
		f = &read
	}
	if f.Dir == "" {
		return m.Dir
	}
	if _, err := os.Stat(f.Dir); err != nil {
		return m.Dir
	}
	return f.Dir
}

// reopen says how to bring back a member of b whose session is gone: its
// own conversation resumed under its label, where it worked, on the model
// and at the effort level it was opened with, on its account unless b found
// that account taking no step, or the launcher's choice when the primary's
// launcher is unknown here, since every account resumes it from the shared
// transcript. Its park and the record of what was sent to it carry over.
// With no account that can take it, it says so, and the open waits until
// one can.
func (d *Driver) reopen(b *batch.Batch, m *batch.Member, f *turn.Frontier) string {
	opt := OpenOptions{Dir: d.workedIn(m, f), Resume: m.SID, Account: m.Account, Model: m.Model, Effort: m.Effort}
	until, out := d.unable(b)[m.Account]
	stuck := false
	if out {
		if others := d.otherAccounts(b, m.Account); len(others) > 0 {
			opt.Account = others[0]
		} else {
			stuck = true
		}
	}
	if opt.Account == "primary" && d.Cfg.PrimaryName == "" {
		opt.Account = "" // the launcher's choice: open refuses a primary whose launcher it does not know
	}
	open := d.openCommand(m.Label, opt, Step{})
	if stuck {
		when := "its owner logs in again"
		if !until.IsZero() {
			when = "its limit lifts"
		}
		return fmt.Sprintf("account %s takes no step %s, and no other account can take it here: tell the owner; once %s, bring it back under its label: %s",
			m.Account, untilWords(until, zoneOf(b)), when, open)
	}
	return "bring it back under its label, its conversation resumed where it stopped: " + open
}

// unable is the accounts batch b found taking no step as of now, each to
// when it can again (batch.Unable).
func (d *Driver) unable(b *batch.Batch) map[string]time.Time {
	if b == nil {
		return map[string]time.Time{}
	}
	evs, _ := b.Events()
	return batch.Unable(evs, d.now())
}

// untilWords says until when an account takes no step: a limit's reset, in
// zone, or, for a rejected login, until its owner logs in again.
func untilWords(until time.Time, zone *time.Location) string {
	if until.IsZero() {
		return "until its owner logs in again"
	}
	return "until " + until.In(zone).Format("Mon 2 Jan 15:04")
}

// noteLimit puts the usage limit a session met mid-turn on b's record, as a
// step that meets one is, so no relaunch or reopen offers its account until
// the limit lifts.
func (d *Driver) noteLimit(b *batch.Batch, o *Observation) {
	e := o.Frontier.Error
	if b == nil || e == nil || !e.Limit || o.Session.Account == "" {
		return
	}
	_ = b.Locked(func(evs []batch.Event) error {
		if until, out := batch.Unable(evs, d.now())[o.Session.Account]; out && (until.IsZero() || !until.Before(e.Resets)) {
			return nil
		}
		return b.Append(batch.Event{At: d.now(), Kind: batch.KindLimit, Session: o.Label, Account: o.Session.Account, Due: e.Resets})
	})
}

// otherAccounts names the accounts a session could be relaunched on: not
// the one it runs on, and none this batch found taking no step. The primary
// (~/.claude, which a session reports as "primary") is one when the name
// its launcher takes is known.
func (d *Driver) otherAccounts(b *batch.Batch, current string) []string {
	out := d.unable(b)
	out[current] = time.Time{}
	taken := func(account string) bool { _, ok := out[account]; return ok }
	dirs, _ := filepath.Glob(filepath.Join(d.Cfg.Home, ".claude-accounts", "*"))
	var others []string
	for _, dir := range dirs {
		if st, err := os.Stat(filepath.Join(dir, "sessions")); err == nil && st.IsDir() && !taken(filepath.Base(dir)) {
			others = append(others, filepath.Base(dir))
		}
	}
	sort.Strings(others)
	if d.Cfg.PrimaryName != "" && !taken("primary") {
		if st, err := os.Stat(filepath.Join(d.Cfg.Home, ".claude", "sessions")); err == nil && st.IsDir() {
			others = append(others, "primary")
		}
	}
	return others
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
			if e.Path != "" && skill.Command(e.Text) != "" {
				return Step{Line: e.Text} // a typed-only skill's line, its file attached
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
	return d.relaunchOn(o, step, others[0], "")
}

// openedWith is the model and the effort level o's session was opened with,
// which a relaunch asks for again: the log's, since an open's own first step
// reads no member.
func openedWith(o *Observation) (model, effort string) {
	m := o.Member
	if m == nil && o.in != nil {
		evs, _ := o.in.Events()
		m = batch.Find(batch.Members(evs), o.Label)
	}
	if m == nil {
		return "", ""
	}
	return m.Model, m.Effort
}

// relaunchOn is the relaunch on account, after first, a command the close
// waits for, when there is one.
func (d *Driver) relaunchOn(o *Observation, step Step, account, first string) string {
	// Claude Code finds a session to resume under its working directory's project.
	model, effort := openedWith(o)
	opt := OpenOptions{Dir: o.Session.Dir, Resume: o.Session.ID, Account: account, Model: model, Effort: effort}
	cmd := first + d.run("close", viewNow(o)) + " && " + d.openCommand(o.Label, opt, step)
	what := "relaunch it on another account with the step it was to take"
	if step.empty() {
		what = "relaunch it on another account, then send it the step it was to take on the view the open prints"
	}
	if jobs := d.Host.Shells(o.Session.Status.PID); len(jobs) > 0 {
		return fmt.Sprintf("%s once its background job (%s) ends, as close refuses until then: %s", what, clip(jobs[0].Command, 120), cmd)
	}
	return what + ": " + cmd
}

// limitNext says how to carry on a session whose account reached its usage
// limit, which lifts only at its reset, days away for a weekly one: on
// another account, which takes step now. On the menu Claude Code shows
// meanwhile, approve takes its first option, Stop, which ends the turn and
// writes nothing, so the close is decided on the same view.
func (d *Driver) limitNext(o *Observation, step Step) string {
	e := o.Frontier.Error
	until := "until it resets"
	if !e.Resets.IsZero() {
		until = untilWords(e.Resets, o.zone())
	}
	head := fmt.Sprintf("account %s takes no step %s", o.Session.Account, until)
	view, stop := viewNow(o), ""
	if s, err := session.Locate(d.Term, d.Host, o.Session.Block); err == nil && turnState(s, o.Frontier).onPrompt() {
		stop = d.run("respond", view, "approve") + " && "
	}
	others := d.otherAccounts(o.in, o.Session.Account)
	if len(others) == 0 {
		if e.Resets.IsZero() {
			return head + ", and no other account can take it here: tell the owner"
		}
		return head + ", and no other account can take it here: tell the owner, and park it until then: " +
			stop + d.run("park", view, "--until", e.Resets.In(o.zone()).Format("2006-01-02 15:04"), text("its account is at its usage limit"))
	}
	return head + "; " + d.relaunchOn(o, step, others[0], stop)
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
