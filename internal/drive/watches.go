package drive

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Watches runs a wait in the background so it outlives the command that
// armed it, and stops it. What the wait prints goes to log, so one that
// ends early says why.
type Watches interface {
	// Start runs argv as the watch name; a name already running is refused.
	Start(name string, argv []string, log string) error
	Alive(name string) bool
	Stop(name string) error
	// Retire takes the calling process's own watch off the books, so a step
	// sent from now on arms a new one. It says whether the watch was still
	// on them: false when Stop got there first.
	Retire(name string) bool
}

// Detached runs each watch as a process in a session of its own, under no
// terminal and no tmux server: a chat driver's shell commands end, tmux
// servers exit, and a watch has to outlive both. A pidfile per watch names
// its process.
type Detached struct {
	Dir string
}

// proc names a process as a pidfile does, a line each: its pid, when it
// started, and the command line it runs, the last two as ps prints them. A
// pid alone is not enough: once a watch is gone its pid goes to another
// process, another drive's watch among them.
type proc struct {
	pid            int
	start, command string
}

// identify reads the process a pid names now. ps runs in the C locale and
// UTC, so every caller reads one process alike, and it prints a control
// character in an argument as an escape, so the command line is one line.
func identify(pid int) (proc, bool) {
	cmd := exec.Command("ps", "-ww", "-o", "lstart=,command=", "-p", strconv.Itoa(pid))
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	out, err := cmd.Output()
	if err != nil {
		return proc{}, false
	}
	// lstart reads like "Thu Oct 22 10:52:29 2026", padded to its column.
	rest := strings.TrimRight(string(out), "\n")
	var start []string
	for len(start) < 5 {
		f, r, ok := strings.Cut(strings.TrimLeft(rest, " "), " ")
		if !ok {
			return proc{}, false
		}
		start, rest = append(start, f), r
	}
	p := proc{pid: pid, start: strings.Join(start, " "), command: strings.TrimLeft(rest, " ")}
	return p, p.command != ""
}

func (w Detached) pidfile(name string) string { return filepath.Join(w.Dir, name+".pid") }

// read takes a pidfile as Start wrote it. One written before pidfiles named
// the start time holds only a pid and a binary, which tell no process
// apart, so it reads as stale.
func (w Detached) read(name string) (p proc, ok bool) {
	b, err := os.ReadFile(w.pidfile(name))
	if err != nil {
		return p, false
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	p.pid, err = strconv.Atoi(strings.TrimSpace(lines[0]))
	if len(lines) == 3 {
		p.start, p.command = lines[1], lines[2]
	}
	return p, err == nil && p.pid > 0
}

// running says whether the process a pidfile names still runs: its pid is
// taken by a process that started then and runs that command line.
func (p proc) running() bool {
	if p.start == "" || p.command == "" {
		return false
	}
	now, ok := identify(p.pid)
	return ok && now == p
}

func (w Detached) Start(name string, argv []string, log string) error {
	if w.Alive(name) {
		return fmt.Errorf("a watch named %s is already running", name)
	}
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(log, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "DRIVE_WATCH="+name)
	cmd.Stdout, cmd.Stderr = out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("starting watch %s: %w", name, err)
	}
	// Start returns once the child runs argv, and until it is reaped its pid
	// is its own, so what ps reads now is the watch.
	pid := cmd.Process.Pid
	p, ok := identify(pid)
	if !ok {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return fmt.Errorf("starting watch %s: ps does not show its process %d", name, pid)
	}
	_ = cmd.Process.Release()
	tmp := w.pidfile(name) + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n%s\n%s\n", p.pid, p.start, p.command)), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.pidfile(name))
}

// Alive checks the process as well as the pidfile, so a watch that died
// without cleaning up, or a pid that has gone to another process since,
// reads as not running.
func (w Detached) Alive(name string) bool {
	p, ok := w.read(name)
	return ok && p.running()
}

// Stop takes the watch off the books, and signals its process group only
// while the pidfile names the watch: a stale one names another process.
func (w Detached) Stop(name string) error {
	p, ok := w.read(name)
	if !ok {
		return nil
	}
	alive := p.running()
	if err := os.Remove(w.pidfile(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if !alive {
		return nil
	}
	if err := syscall.Kill(-p.pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stopping watch %s (pid %d): %w", name, p.pid, err)
	}
	return nil
}

func (w Detached) Retire(name string) bool {
	p, ok := w.read(name)
	if !ok || p.pid != os.Getpid() {
		return false
	}
	return os.Remove(w.pidfile(name)) == nil
}
