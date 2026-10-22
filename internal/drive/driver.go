// Package drive is what a driver does to the Claude Code sessions it drives:
// read where each stands, send its next step, answer a prompt, wait for its
// turn, close it. Each action is decided on a view the driver read and is
// refused once the session has moved past it; it is admitted under the
// batch's lock, recorded before it touches the session, and confirmed from
// the session's transcript before it is reported done. What to send and
// when stays the driver's judgment.
package drive

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/progress"
	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// Driver holds what every action needs.
type Driver struct {
	Term     rex.Terminal
	Host     session.Host
	Store    batch.Store
	Cfg      config.Config
	Progress progress.Source
	Watches  Watches
	Self     string // this executable, which a watch runs
	Now      func() time.Time
	Sleep    func(time.Duration)
	Poll     time.Duration // how often a wait looks; 2 s when zero

	// BatchName pins the batch an action works in; "" is the current one.
	// A watch carries it, so a wake from an old batch never acts in a new one.
	BatchName string
}

// Refused is an action drive would not take in the state the session is in.
// Nothing was done; Next says what would make it possible.
type Refused struct {
	Reason string
	Next   string

	account string // an account whose login the refusal found rejected
}

func (r *Refused) Error() string {
	if r.Next == "" {
		return r.Reason
	}
	return r.Reason + "\nnext: " + r.Next
}

// Failed is an action that was tried and did not end as asked.
type Failed struct {
	What  string
	Next  string
	Login bool // the session's login or API was the failure, not the delivery
}

func (f *Failed) Error() string {
	if f.Next == "" {
		return f.What
	}
	return f.What + "\nnext: " + f.Next
}

func (d *Driver) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *Driver) sleep(t time.Duration) {
	if d.Sleep != nil {
		d.Sleep(t)
		return
	}
	time.Sleep(t)
}

func (d *Driver) poll() time.Duration {
	if d.Poll > 0 {
		return d.Poll
	}
	return 2 * time.Second
}

// zone is where times are shown: the batch's zone, else this machine's.
func (d *Driver) zone() *time.Location {
	if b, err := d.Batch(); err == nil {
		return b.Zone()
	}
	return time.Local
}

// Batch is the batch actions work in.
func (d *Driver) Batch() (*batch.Batch, error) {
	if d.BatchName != "" {
		return d.Store.Get(d.BatchName)
	}
	return d.Store.Current()
}

// Observation is one session as it stands.
type Observation struct {
	Label    string        // the batch's label, or the block's short id outside the batch
	Member   *batch.Member // nil outside the batch
	Session  *session.Session
	Frontier turn.Frontier
	View     turn.View
	Input    turn.InputKind
	Typed    string // the input box's text, typed or suggested
}

// State is where the session's turn stands, in one word.
func (o *Observation) State() string { return turnState(o.Session, o.Frontier) }

// turnState is where a session's turn stands, the one answer every action
// reads: working; done or idle; interrupted; error; blocked:permission,
// blocked:question or blocked:auth. Claude Code's own registry says what
// the session is doing the moment it changes: busy through a queued turn, a
// compaction and an approved command, waiting on a prompt the moment it
// opens, idle once a turn is over however it ended. The transcript then says
// how it ended, and Rex what kind of prompt it waits on. Rex alone hears
// only what a hook reports, which misses all of those; it decides only for a
// Claude Code that keeps no such record.
func turnState(s *session.Session, f turn.Frontier) string {
	st := s.Status
	switch s.Activity {
	case "busy":
		return "working"
	case "waiting":
		if st.State == "blocked" && st.Kind != "" && st.Kind != "auth" {
			return "blocked:" + st.Kind
		}
		if strings.Contains(s.WaitingFor, "permission") {
			return "blocked:permission"
		}
		return "blocked:question"
	case "idle":
		e := liveError(s, f)
		switch {
		case e != nil && e.Kind == "authentication_failed", st.State == "blocked" && st.Kind == "auth":
			return "blocked:auth"
		case e != nil:
			return "error"
		case f.Interrupted:
			return "interrupted"
		case f.Newest() == nil:
			return "idle"
		}
		return "done"
	}
	// No registry status: fold what no hook reports into Rex's record.
	open := st.State == "working" || st.State == "blocked" && st.Kind != "auth"
	e := liveError(s, f)
	switch {
	case e != nil && e.Kind == "authentication_failed":
		return "blocked:auth"
	case e != nil && open:
		return "error"
	case f.Interrupted && open:
		return "interrupted"
	case st.State == "blocked" && st.Kind != "":
		return "blocked:" + st.Kind
	}
	return st.State
}

// liveError is the API error that ended the newest prompt's turn, unless it
// ended an earlier process: accounts share one transcript directory, so a
// session resumed on another account reads the error that ended the one it
// replaced, written before this process started.
func liveError(s *session.Session, f turn.Frontier) *turn.APIError {
	e := f.Error
	switch {
	case e == nil:
		return nil
	case !s.Started.IsZero():
		if e.At.Before(s.Started) {
			return nil
		}
	case s.Status.State == "idle" && s.Status.UpdatedAt.After(e.At):
		return nil // Rex has heard the new process start since
	}
	return e
}

// LoginRejected says whether the session's login was refused.
func (o *Observation) LoginRejected() bool { return o.State() == "blocked:auth" }

// target resolves a label in the batch or a block id to the session's
// label and block, without reading the session.
func (d *Driver) target(members []*batch.Member, ref string) (label, block string, err error) {
	if m := batch.Find(members, ref); m != nil {
		return m.Label, m.Block, nil
	}
	id, err := d.findBlock(ref)
	if err != nil {
		return "", "", err
	}
	return rex.Short(id), id, nil
}

// own takes a session for one action, from the read it is decided against
// to the record of how it went. A second driver acting on the same session
// meanwhile is refused, never queued behind the first.
func (d *Driver) own(label, key, what string) (func(), error) {
	release, err := d.Store.Own(key, what)
	var busy *batch.Busy
	if errors.As(err, &busy) {
		return nil, &Refused{Reason: fmt.Sprintf("another driver is %s (pid %d, since %s)", busy.What, busy.PID, busy.Since.In(d.zone()).Format("15:04:05")),
			Next: fmt.Sprintf("leave %s to it; once it is done, drive read %s shows where it stands", label, label)}
	}
	return release, err
}

// observe reads one session. ref is a label in the batch or a block id.
func (d *Driver) observe(members []*batch.Member, ref string) (*Observation, error) {
	o := &Observation{}
	label, block, err := d.target(members, ref)
	if err != nil {
		return nil, err
	}
	o.Label, o.Member = label, batch.Find(members, label)
	s, err := session.Locate(d.Term, d.Host, block)
	if errors.Is(err, session.ErrNoClaude) {
		return nil, &Refused{Reason: fmt.Sprintf("%s (block %s) has no Claude Code session running", o.Label, rex.Short(block)),
			Next: "it may have exited; `drive status` lists the sessions that are running"}
	}
	if err != nil {
		return nil, &Refused{Reason: fmt.Sprintf("%s (block %s) cannot be read: %v", o.Label, rex.Short(block), err),
			Next: "the block may be gone; `drive status` lists what is running"}
	}
	o.Session = s
	if o.Frontier, err = s.Frontier(); err != nil {
		return nil, fmt.Errorf("reading %s's transcript: %w", o.Label, err)
	}
	o.Frontier.Error = liveError(s, o.Frontier)
	o.View = turn.ViewOf(o.Label, s.ID, o.Frontier)
	if screen, err := d.Term.Capture(block, true); err == nil {
		o.Input, o.Typed = turn.InputBox(screen)
	}
	return o, nil
}

func (d *Driver) findBlock(ref string) (string, error) {
	blocks, err := d.Term.Blocks("")
	if err != nil {
		return "", err
	}
	var hits []string
	for _, b := range blocks {
		if b.ID == ref || (len(ref) >= 6 && strings.HasSuffix(b.ID, ref)) {
			hits = append(hits, b.ID)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", &Refused{Reason: fmt.Sprintf("no session or block named %q", ref),
			Next: "`drive status` lists the batch's sessions by label and the other Claude sessions by block"}
	default:
		return "", &Refused{Reason: fmt.Sprintf("%q names %d blocks", ref, len(hits)), Next: "give more of the block id"}
	}
}

// members reads the batch's sessions; with no batch there are none.
func (d *Driver) members() (*batch.Batch, []batch.Event, []*batch.Member, error) {
	b, err := d.Batch()
	if errors.Is(err, batch.ErrNoBatch) {
		return nil, nil, nil, nil
	}
	if err != nil {
		return nil, nil, nil, err
	}
	evs, err := b.Events()
	if err != nil {
		return nil, nil, nil, err
	}
	return b, evs, batch.Members(evs), nil
}

// checkView refuses an action decided on a view the session has moved past.
// Queued sends only need the session to have received nothing since.
func checkView(o *Observation, v turn.View, promptOnly bool, zone *time.Location) error {
	cur := o.View
	if v.Prompt == cur.Prompt && (promptOnly || v.Reply == cur.Reply) {
		return nil
	}
	what := "it has written more since"
	if v.Prompt != cur.Prompt {
		what = "it has received another prompt since"
		if p := o.Frontier.Newest(); p != nil {
			what += fmt.Sprintf(" (%s: %s)", p.At.In(zone).Format("15:04"), clip(promptText(*p), 80))
		}
	}
	return &Refused{Reason: fmt.Sprintf("%s has moved past view %s: %s", o.Label, v, what),
		Next: fmt.Sprintf("read it again and decide on what it shows now: drive read %s", o.Label)}
}

func promptText(p turn.Prompt) string {
	if !p.Known {
		return "(a slash command, queued)"
	}
	return p.Text
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func tail(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return "…" + string(r[len(r)-n:])
	}
	return s
}

// otherAccounts names the accounts a session could be relaunched on: not
// the one it runs on, and none whose login this batch saw rejected.
func (d *Driver) otherAccounts(current string) []string {
	rejected := map[string]bool{current: true}
	if _, evs, _, err := d.members(); err == nil {
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
	return out
}

// relaunchHint says how to carry a session whose login was rejected over to
// another account: close it and resume it there, its step on the open.
func (d *Driver) relaunchHint(o *Observation) string {
	others := d.otherAccounts(o.Session.Account)
	if len(others) == 0 {
		return "every other account's login was rejected in this batch too: tell the owner, and park the session until they log in again"
	}
	// Claude Code finds a session to resume under its working directory's project.
	return fmt.Sprintf("relaunch it on another account with the step it was to take: drive close %s && drive open %s --dir %s --resume %s --account %s '<the step>'",
		o.View, o.Label, shellQuote(o.Session.Dir), o.Session.ID, others[0])
}
