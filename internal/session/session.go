// Package session finds the Claude Code session running in a Rex block:
// its process, its session id and transcript, the account it runs on, its
// working directory and the background shells it has started.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/rex"
	"github.com/qiushiyan/drive/internal/turn"
)

// Session is one Claude Code session in a block.
type Session struct {
	Block   string // full block id
	PID     int
	ID      string // the session id; "" until Claude Code registers it
	Account string // the account directory's name; "primary" for ~/.claude
	Config  string // the config directory the session runs under
	Dir     string // working directory
	Status  rex.Status
	// Activity is what Claude Code itself says the session is doing, from
	// its registry: idle, busy, waiting or shell; "" from a version that
	// does not say. It changes the moment the session does, where Rex hears
	// only what a hook reports.
	Activity   string
	WaitingFor string    // with waiting: "permission prompt", …
	ActivityAt time.Time // when Activity last changed
	Started    time.Time // when the process started
}

// ErrNoClaude is a block with no Claude Code session in it.
var ErrNoClaude = errors.New("no Claude Code session")

// Registration is what a Claude Code process writes about itself to
// <config>/sessions/<pid>.json.
type Registration struct {
	ID         string
	Config     string // the config directory the file was found under
	Status     string // idle, busy, waiting, shell; "" from older versions
	WaitingFor string
	StatusAt   time.Time
	ProcStart  string // the process's start, as ps prints lstart in UTC
}

// Host is what Locate reads besides Rex: the registries of running sessions
// and the processes. Tests substitute their own.
type Host interface {
	// Registry reads a Claude Code process's registration.
	Registry(pid int) (Registration, error)
	// Started is when a process started, as ps prints lstart in UTC, the
	// form Claude Code's registry keeps as procStart; "" when no such
	// process runs.
	Started(pid int) string
	// Cwd is a process's working directory.
	Cwd(pid int) string
	// Shells are the background shells a Claude Code process runs.
	Shells(pid int) []Shell
	// Transcript is the transcript of the session with this id, which
	// outlives its process and its block; "" when none is found.
	Transcript(id string) string
}

// Shell is a background shell a Claude Code session runs: a Bash call it
// left running, such as a wait on checks.
type Shell struct {
	Started time.Time `json:"started"`
	Command string    `json:"command"` // what it runs
}

// Locate finds the session in a block. A record whose process has gone, or
// whose pid now belongs to another process, is no session: Claude Code that
// crashed never told Rex it ended.
func Locate(t rex.Terminal, h Host, block string) (*Session, error) {
	st, err := t.Status(block)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, ErrNoClaude
	}
	started := h.Started(st.PID)
	if started == "" {
		return nil, ErrNoClaude
	}
	s := &Session{Block: block, PID: st.PID, Status: *st}
	if reg, err := h.Registry(st.PID); err == nil {
		if !sameStart(reg.ProcStart, started) {
			return nil, ErrNoClaude // the pid is another process's now
		}
		s.ID, s.Config = reg.ID, reg.Config
		s.Activity, s.WaitingFor, s.ActivityAt = reg.Status, reg.WaitingFor, reg.StatusAt
	}
	s.Started, _ = time.ParseInLocation(lstart, started, time.UTC)
	s.Account = accountOf(s.Config)
	s.Dir = h.Cwd(st.PID)
	return s, nil
}

const lstart = "Mon Jan 2 15:04:05 2006"

// sameStart says whether the start a registration recorded is this
// process's. Claude Code writes it in UTC; a start in another zone or a form
// that does not parse is not taken as a different process, since a wrong
// "gone" would hide every live session.
func sameStart(recorded, started string) bool {
	at, err := time.ParseInLocation(lstart, Lstart(started), time.UTC)
	if recorded == "" || err != nil {
		return true
	}
	for _, zone := range []*time.Location{time.UTC, time.Local} {
		if r, err := time.ParseInLocation(lstart, Lstart(recorded), zone); err != nil {
			return true
		} else if r.Equal(at) {
			return true
		}
	}
	return false
}

// Lstart normalizes a process start as ps prints its lstart ("Thu Oct  2
// 10:52:29 2026" pads the day), so two readings of one process compare equal.
func Lstart(s string) string { return strings.Join(strings.Fields(s), " ") }

// Since is when the session's state last changed: Claude Code's own record
// when it keeps one, else Rex's.
func (s *Session) Since() time.Time {
	if s.Activity != "" && !s.ActivityAt.IsZero() {
		return s.ActivityAt
	}
	return s.Status.UpdatedAt
}

func accountOf(config string) string {
	switch {
	case config == "":
		return ""
	case filepath.Base(config) == ".claude":
		return "primary"
	default:
		return filepath.Base(config)
	}
}

// Transcript is the session's transcript path; "" until its first prompt
// creates the file.
func (s *Session) Transcript() string {
	if s.ID == "" || s.Config == "" {
		return ""
	}
	found, _ := filepath.Glob(filepath.Join(s.Config, "projects", "*", s.ID+".jsonl"))
	if len(found) == 0 {
		return ""
	}
	return found[0]
}

// Frontier reads where the session's conversation stands.
func (s *Session) Frontier() (turn.Frontier, error) {
	p := s.Transcript()
	if p == "" {
		return turn.Frontier{}, nil
	}
	return turn.ReadFile(p)
}

// Machine is the Host of this machine.
type Machine struct {
	Home string
}

// Registry implements Host: the primary config directory, then each account's.
func (m Machine) Registry(pid int) (Registration, error) {
	dirs := []string{filepath.Join(m.Home, ".claude")}
	accounts, _ := filepath.Glob(filepath.Join(m.Home, ".claude-accounts", "*"))
	dirs = append(dirs, accounts...)
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "sessions", strconv.Itoa(pid)+".json"))
		if err != nil {
			continue
		}
		if reg, err := ParseRegistration(b); err == nil {
			reg.Config = d
			return reg, nil
		}
	}
	return Registration{}, fmt.Errorf("no session registry for process %d", pid)
}

// ParseRegistration reads a sessions/<pid>.json file.
func ParseRegistration(b []byte) (Registration, error) {
	var raw struct {
		SessionID       string `json:"sessionId"`
		Status          string `json:"status"`
		WaitingFor      string `json:"waitingFor"`
		StatusUpdatedAt int64  `json:"statusUpdatedAt"` // milliseconds
		ProcStart       string `json:"procStart"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return Registration{}, err
	}
	if raw.SessionID == "" {
		return Registration{}, errors.New("no session id")
	}
	reg := Registration{ID: raw.SessionID, Status: raw.Status, WaitingFor: raw.WaitingFor, ProcStart: raw.ProcStart}
	if raw.StatusUpdatedAt > 0 {
		reg.StatusAt = time.UnixMilli(raw.StatusUpdatedAt)
	}
	return reg, nil
}

// Started implements Host.
func (Machine) Started(pid int) string {
	cmd := exec.Command("ps", "-o", "lstart=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return Lstart(string(out))
}

// Transcript implements Host. Every account's projects directory is one
// directory (~/.claude-accounts/*/projects links to ~/.claude/projects), but
// each is looked in, in case one is not linked.
func (m Machine) Transcript(id string) string {
	for _, pattern := range []string{
		filepath.Join(m.Home, ".claude", "projects", "*", id+".jsonl"),
		filepath.Join(m.Home, ".claude-accounts", "*", "projects", "*", id+".jsonl"),
	} {
		if found, _ := filepath.Glob(pattern); len(found) > 0 {
			return found[0]
		}
	}
	return ""
}

// Cwd implements Host.
func (Machine) Cwd(pid int) string {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:]
		}
	}
	return ""
}

// Shells implements Host: Claude Code runs each Bash call as a child shell
// that sources its snapshot, so a turn that ended with background jobs still
// has such children.
func (Machine) Shells(pid int) []Shell {
	kids, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	var out []Shell
	for k := range strings.FieldsSeq(string(kids)) {
		cmd := exec.Command("ps", "-ww", "-o", "lstart=,command=", "-p", k)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		line, err := cmd.Output()
		if err != nil || !strings.Contains(string(line), "shell-snapshots") {
			continue
		}
		f := strings.Fields(string(line))
		if len(f) < 6 {
			continue
		}
		// lstart reads like "Thu Oct 22 10:52:29 2026".
		at, err := time.ParseInLocation(lstart, strings.Join(f[:5], " "), time.Local)
		if err != nil {
			continue
		}
		out = append(out, Shell{Started: at, Command: JobCommand(strings.Join(f[5:], " "))})
	}
	return out
}

// JobCommand is the command a Claude Code Bash shell runs, from its command
// line: the snapshot it sources and the setup around it are Claude Code's,
// and the job is what it evals.
func JobCommand(line string) string {
	i := strings.Index(line, "eval '")
	j := strings.LastIndex(line, "' < /dev/null")
	if i < 0 || j <= i {
		return line
	}
	return strings.ReplaceAll(line[i+len("eval '"):j], `'\''`, "'")
}
