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

// exited says process pid has ended: gone, or a zombie its parent, this
// test, has not reaped.
func exited(pid int) bool {
	out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	stat := strings.TrimSpace(string(out))
	return stat == "" || strings.HasPrefix(stat, "Z")
}
