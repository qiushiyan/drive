package drive_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/drive"
)

// A pidfile names its watch by more than a pid: once the watch is gone its
// pid goes to some other process, another drive's watch among them. A
// pidfile naming such a process names no watch, and Stop leaves the process
// alone while taking the pidfile off the books.
func TestAWatchIsKnownByMoreThanItsPid(t *testing.T) {
	w := drive.Detached{Dir: t.TempDir()}
	log := filepath.Join(t.TempDir(), "watches.log")
	argv := []string{"/bin/sleep", "30"}
	if err := w.Start("drive-test", argv, log); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	pidfile := filepath.Join(w.Dir, "drive-test.pid")
	own, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.WriteFile(pidfile, own, 0o644)
		_ = w.Stop("drive-test")
	})
	if !w.Alive("drive-test") {
		t.Fatal("the watch runs")
	}
	_, identity, _ := strings.Cut(string(own), "\n")

	cases := []struct {
		name  string
		argv  []string
		later bool // started in a later second than the watch
		file  func(pid int) string
	}{
		{name: "another command line, the same binary", argv: []string{"/bin/sleep", "31"},
			file: func(pid int) string { return fmt.Sprintf("%d\n%s", pid, identity) }},
		{name: "the same command line, started later", argv: argv, later: true,
			file: func(pid int) string { return fmt.Sprintf("%d\n%s", pid, identity) }},
		{name: "a pidfile naming only a pid and a binary", argv: []string{"/bin/sleep", "31"},
			file: func(pid int) string { return fmt.Sprintf("%d\n%s\n", pid, argv[0]) }},
	}
	for _, c := range cases {
		if c.later {
			time.Sleep(time.Until(started.Truncate(time.Second).Add(time.Second + 10*time.Millisecond)))
		}
		pid, ended := decoy(t, c.argv)
		if err := os.WriteFile(pidfile, []byte(c.file(pid)), 0o644); err != nil {
			t.Fatal(err)
		}
		if w.Alive("drive-test") {
			t.Errorf("%s: the pid names no watch", c.name)
		}
		if err := w.Stop("drive-test"); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		select {
		case <-ended:
			t.Errorf("%s: Stop signalled a process that is not the watch", c.name)
		case <-time.After(200 * time.Millisecond):
		}
		if _, err := os.Stat(pidfile); !os.IsNotExist(err) {
			t.Errorf("%s: Stop takes the stale pidfile off the books: %v", c.name, err)
		}
	}
}

// decoy starts a process in a group of its own, as a watch runs, so a
// signal to the group a pidfile names would reach it. ended closes once it
// exits.
func decoy(t *testing.T, argv []string) (pid int, ended <-chan struct{}) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-done })
	return cmd.Process.Pid, done
}

// A detached watch is a real process outside any terminal: it runs, a second
// one by its name is refused, Retire takes only its own off the books, and
// Stop ends its process, not only its pidfile.
func TestADetachedWatchRunsAndStops(t *testing.T) {
	w := drive.Detached{Dir: t.TempDir()}
	log := filepath.Join(t.TempDir(), "watches.log")
	if err := w.Start("drive-test", []string{"/bin/sleep", "30"}, log); err != nil {
		t.Fatal(err)
	}
	if !w.Alive("drive-test") {
		t.Fatal("the watch runs")
	}
	if err := w.Start("drive-test", []string{"/bin/sleep", "30"}, log); err == nil {
		t.Fatal("a second watch by one name is refused")
	}
	if w.Retire("drive-test") {
		t.Fatal("only the watch's own process retires it")
	}
	own, _ := os.ReadFile(filepath.Join(w.Dir, "drive-test.pid"))
	pid, _ := strconv.Atoi(strings.SplitN(string(own), "\n", 2)[0])
	if err := w.Stop("drive-test"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100 && !exited(pid); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if w.Alive("drive-test") || !exited(pid) {
		t.Fatalf("stopped: process %d still runs", pid)
	}
	if err := w.Start("drive-test", []string{"/bin/sleep", "30"}, log); err != nil {
		t.Fatalf("the name is free again: %v", err)
	}
	_ = w.Stop("drive-test")
}

// A timer's watch is named by its whole note, which can run past the 255
// bytes a file name may (10 November: a 230-byte note failed with "file name
// too long"). A long name runs as a watch like any other, and two names that
// differ only past where a file name would end are two watches.
func TestAWatchByALongNameRuns(t *testing.T) {
	w := drive.Detached{Dir: t.TempDir()}
	log := filepath.Join(t.TempDir(), "watches.log")
	long := "drive-2026-11-10-timer-" + strings.Repeat("S2-is-due--read-it-", 15)
	a, b := long+"a", long+"b"
	for _, name := range []string{a, b} {
		if err := w.Start(name, []string{"/bin/sleep", "30"}, log); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = w.Stop(name) })
	}
	if !w.Alive(a) || !w.Alive(b) {
		t.Fatal("both watches run")
	}
	if err := w.Stop(a); err != nil {
		t.Fatal(err)
	}
	if w.Alive(a) || !w.Alive(b) {
		t.Fatal("stopping one watch leaves the other running")
	}
}

// A watch whose pidfile cannot be written is off the books, where nothing
// would stop it, so a Start that fails leaves no process running: a failed
// timer that rang anyway would wake its driver unasked.
func TestAFailedStartLeavesNoWatchRunning(t *testing.T) {
	dir := t.TempDir()
	w := drive.Detached{Dir: filepath.Join(dir, "watches")}
	if err := os.Mkdir(w.Dir, 0o555); err != nil {
		t.Fatal(err)
	}
	ran := filepath.Join(dir, "ran")
	argv := []string{"/bin/sh", "-c", `echo $$ > "$0"; exec sleep 30`, ran}
	if err := w.Start("drive-test", argv, filepath.Join(dir, "watches.log")); err == nil {
		t.Fatal("a pidfile in a directory nobody may write to fails")
	}
	// The process may have been stopped before it wrote its pid.
	for range 50 {
		b, err := os.ReadFile(ran)
		pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil || pid == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
		for j := 0; j < 100 && !exited(pid); j++ {
			time.Sleep(10 * time.Millisecond)
		}
		if !exited(pid) {
			t.Fatalf("process %d still runs, its watch off the books", pid)
		}
		return
	}
}

// exited says process pid has ended: gone, or a zombie its parent, this
// test, has not reaped.
func exited(pid int) bool {
	out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	stat := strings.TrimSpace(string(out))
	return stat == "" || strings.HasPrefix(stat, "Z")
}

// The detached sampler is a real process outside any terminal, run in its
// directory without a watch's name, whatever command starts it. Stop returns
// only once it has exited, killing one that ignores SIGTERM after its grace,
// and the log says when drive started and stopped each.
func TestADetachedSamplerRunsAndStops(t *testing.T) {
	dir := t.TempDir()
	s := drive.DetachedSampler{Pidfile: filepath.Join(dir, "state", "sampler.pid"), Log: filepath.Join(dir, "sampler.log"), Dir: dir, Grace: 300 * time.Millisecond}
	t.Setenv("DRIVE_WATCH", "drive-b1-7c1e2a") // a watch's own command keeps the sampler too
	for what, script := range map[string]string{
		"stops on SIGTERM": `echo "watch [$DRIVE_WATCH] in $(pwd -P)"; while :; do sleep 1; done`,
		"ignores SIGTERM":  `trap "" TERM; while :; do sleep 1; done`,
	} {
		if err := s.Start([]string{"/bin/sh", "-c", script}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		own, _ := os.ReadFile(s.Pidfile)
		pid, _ := strconv.Atoi(strings.SplitN(string(own), "\n", 2)[0])
		t.Cleanup(func() { _ = syscall.Kill(-pid, syscall.SIGKILL) })
		if !s.Running() {
			t.Fatalf("%s: it runs", what)
		}
		time.Sleep(100 * time.Millisecond) // what it prints first
		if err := s.Stop(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if s.Running() || !exited(pid) {
			t.Fatalf("%s: Stop returned with process %d still running", what, pid)
		}
		if _, err := os.Stat(s.Pidfile); !os.IsNotExist(err) {
			t.Fatalf("%s: the pidfile is gone with it: %v", what, err)
		}
		if err := s.Stop(); err != nil {
			t.Fatalf("%s: stopping none is no error: %v", what, err)
		}
	}
	err := s.Start([]string{filepath.Join(dir, "no-such-sampler")})
	log, _ := os.ReadFile(s.Log)
	where, _ := filepath.EvalSymlinks(dir)
	for _, want := range []string{"watch [] in " + where + "\n", " drive: started pid ", " drive: stopped pid ", "no-such-sampler did not start: "} {
		if !strings.Contains(string(log), want) {
			t.Errorf("the log says %q:\n%s", want, log)
		}
	}
	if err == nil {
		t.Error("a sampler that cannot start fails its start")
	}
}
