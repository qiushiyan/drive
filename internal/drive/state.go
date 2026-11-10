package drive

import (
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/turn"
)

// State is where a session's turn stands, in the word drive prints: working,
// done, idle (no prompt yet), interrupted, error, blocked:permission,
// blocked:question, blocked:auth (a rejected login); gone or closed for a
// session that ended. A Claude Code that keeps no registry status passes
// Rex's own word through.
type State string

// The states turnState gives. A Claude Code that keeps no registry status can
// read as a word of Rex's that is none of them.
const (
	Working           State = "working"
	Done              State = "done"
	Idle              State = "idle" // no prompt yet
	Interrupted       State = "interrupted"
	Errored           State = "error"
	BlockedPermission State = "blocked:permission"
	BlockedQuestion   State = "blocked:question"
	BlockedAuth       State = "blocked:auth" // a rejected login
)

// What a session that could not be read live reads as. These are no turn's
// state: turnState never gives them.
const (
	Gone   State = "gone"   // its block or its Claude Code no longer runs
	Closed State = "closed" // its driver closed it
	Unread State = "unread" // it runs, as far as anything says, and the read failed
)

// blocked says the session is stopped on something only someone else
// moves: a prompt, or a rejected login.
func (s State) blocked() bool { return strings.HasPrefix(string(s), "blocked") }

// onPrompt says the turn stopped on a prompt someone answers. A rejected
// login reads blocked too, but only a relaunch moves it.
func (s State) onPrompt() bool { return s.blocked() && s != BlockedAuth }

// stopped says the turn ended on what nothing moves until someone acts: a
// rejected login, an API error or an interrupt.
func (s State) stopped() bool { return s == BlockedAuth || s == Errored || s == Interrupted }

// failed says it ended on what its driver did not choose: an API error or a
// rejected login. An interrupt is usually the driver's own.
func (s State) failed() bool { return s == BlockedAuth || s == Errored }

// promptKind is what a prompt asks: permission or question.
func (s State) promptKind() string { return strings.TrimPrefix(string(s), "blocked:") }

// turnState is where a session's turn stands, the one answer every action
// reads: working; done or idle; interrupted; error; blocked:permission,
// blocked:question or blocked:auth. Claude Code's own registry says what
// the session is doing the moment it changes: busy through a queued turn, a
// compaction and an approved command, waiting on a prompt the moment it
// opens, idle once a turn is over however it ended. The transcript then says
// how it ended, and Rex what kind of prompt it waits on. Rex alone hears
// only what a hook reports, which misses all of those; it decides only for a
// Claude Code that keeps no such record.
func turnState(s *session.Session, f turn.Frontier) State {
	st := s.Status
	switch s.Activity {
	case "busy":
		return Working
	case "waiting":
		if st.State == "blocked" && st.Kind != "" && st.Kind != "auth" {
			return State("blocked:" + st.Kind)
		}
		if strings.Contains(s.WaitingFor, "permission") {
			return BlockedPermission
		}
		return BlockedQuestion
	case "idle", "shell": // shell: idle at its prompt with a background job running
		e := liveError(s, f)
		switch {
		case e != nil && e.Kind == "authentication_failed", st.State == "blocked" && st.Kind == "auth":
			return BlockedAuth
		case e != nil:
			return Errored
		case f.Interrupted:
			return Interrupted
		case f.Newest() == nil:
			return Idle
		}
		return Done
	}
	// No registry status: fold what no hook reports into Rex's record.
	open := st.State == "working" || st.State == "blocked" && st.Kind != "auth"
	e := liveError(s, f)
	switch {
	case e != nil && e.Kind == "authentication_failed":
		return BlockedAuth
	case e != nil && open:
		return Errored
	case f.Interrupted && open:
		return Interrupted
	case st.State == "blocked" && st.Kind != "":
		return State("blocked:" + st.Kind)
	}
	return State(st.State)
}

// Turn is where the turn a session's newest prompt started stands: the one
// answer to whether it is over, which a wait waits for, a hold lasts
// through, and the notice's turn reaches before its step goes.
type Turn struct {
	State   State
	Over    bool            // it ended: answered, on a prompt, an API error, an interrupt
	Shells  int             // background shells started since the turn began, still running
	Orphans int             // older background shells, left over and not waited on
	Jobs    []session.Shell // the shells it started, still running
}

// turnOf reads a session's turn; shells started before since are not its.
func (d *Driver) turnOf(s *session.Session, f turn.Frontier, since time.Time) Turn {
	t := Turn{State: turnState(s, f)}
	switch t.State {
	case Working, Idle:
	case Done:
		t.Over = f.Answered()
	default:
		t.Over = true
	}
	for _, sh := range d.Host.Shells(s.Status.PID) {
		if !since.IsZero() && sh.Started.Before(since) {
			t.Orphans++
		} else {
			t.Shells++
			t.Jobs = append(t.Jobs, sh)
		}
	}
	return t
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
