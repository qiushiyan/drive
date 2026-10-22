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
}

// ErrNoClaude is a block with no Claude Code session in it.
var ErrNoClaude = errors.New("no Claude Code session")

// Host is what Locate reads besides Rex: the registries of running sessions
// and the processes. Tests substitute their own.
type Host interface {
	// Registry returns the session id and the config directory for a Claude
	// Code process, from <config>/sessions/<pid>.json.
	Registry(pid int) (id, config string, err error)
	// Cwd is a process's working directory.
	Cwd(pid int) string
	// Shells are the background shells a Claude Code process runs, with
	// when each started.
	Shells(pid int) []time.Time
}

// Locate finds the session in a block.
func Locate(t rex.Terminal, h Host, block string) (*Session, error) {
	st, err := t.Status(block)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return nil, ErrNoClaude
	}
	s := &Session{Block: block, PID: st.PID, Status: *st}
	s.ID, s.Config, _ = h.Registry(st.PID)
	s.Account = accountOf(s.Config)
	s.Dir = h.Cwd(st.PID)
	return s, nil
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
func (m Machine) Registry(pid int) (string, string, error) {
	dirs := []string{filepath.Join(m.Home, ".claude")}
	accounts, _ := filepath.Glob(filepath.Join(m.Home, ".claude-accounts", "*"))
	dirs = append(dirs, accounts...)
	for _, d := range dirs {
		b, err := os.ReadFile(filepath.Join(d, "sessions", strconv.Itoa(pid)+".json"))
		if err != nil {
			continue
		}
		var reg struct {
			SessionID string `json:"sessionId"`
		}
		if json.Unmarshal(b, &reg) == nil && reg.SessionID != "" {
			return reg.SessionID, d, nil
		}
	}
	return "", "", fmt.Errorf("no session registry for process %d", pid)
}

// Cwd implements Host.
func (Machine) Cwd(pid int) string {
	out, err := exec.Command("lsof", "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "n") {
			return line[1:]
		}
	}
	return ""
}

// Shells implements Host: Claude Code runs each Bash call as a child shell
// that sources its snapshot, so a turn that ended with background jobs still
// has such children.
func (Machine) Shells(pid int) []time.Time {
	kids, err := exec.Command("pgrep", "-P", strconv.Itoa(pid)).Output()
	if err != nil {
		return nil
	}
	var starts []time.Time
	for _, k := range strings.Fields(string(kids)) {
		out, err := exec.Command("ps", "-o", "lstart=,command=", "-p", k).Output()
		if err != nil || !strings.Contains(string(out), "shell-snapshots") {
			continue
		}
		f := strings.Fields(string(out))
		if len(f) < 5 {
			continue
		}
		// lstart reads like "Thu Oct 22 10:52:29 2026".
		if at, err := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(f[:5], " "), time.Local); err == nil {
			starts = append(starts, at)
		}
	}
	return starts
}
