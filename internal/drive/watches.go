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

func (w Detached) pidfile(name string) string { return filepath.Join(w.Dir, name+".pid") }

func (w Detached) read(name string) (pid int, exe string, ok bool) {
	b, err := os.ReadFile(w.pidfile(name))
	if err != nil {
		return 0, "", false
	}
	first, rest, _ := strings.Cut(string(b), "\n")
	pid, err = strconv.Atoi(strings.TrimSpace(first))
	return pid, strings.TrimSpace(rest), err == nil && pid > 0
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
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	tmp := w.pidfile(name) + ".tmp"
	if err := os.WriteFile(tmp, []byte(fmt.Sprintf("%d\n%s\n", pid, argv[0])), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, w.pidfile(name))
}

// Alive checks the process as well as the pidfile, so a watch that died
// without cleaning up, or a pid reused since, reads as not running.
func (w Detached) Alive(name string) bool {
	pid, exe, ok := w.read(name)
	if !ok {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	out, err := exec.Command("ps", "-ww", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	return err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), exe)
}

func (w Detached) Stop(name string) error {
	pid, _, ok := w.read(name)
	if !ok {
		return nil
	}
	alive := w.Alive(name)
	if err := os.Remove(w.pidfile(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if !alive {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("stopping watch %s (pid %d): %w", name, pid, err)
	}
	return nil
}

func (w Detached) Retire(name string) bool {
	pid, _, ok := w.read(name)
	if !ok || pid != os.Getpid() {
		return false
	}
	return os.Remove(w.pidfile(name)) == nil
}
