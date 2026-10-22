package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/testkit"
)

// TestMain points HOME and drive's directories at a sandbox.
func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "drive-cmd-home")
	os.Setenv("HOME", dir)
	os.Setenv("DRIVE_STATE_DIR", filepath.Join(dir, "state"))
	os.Setenv("DRIVE_CONFIG_DIR", filepath.Join(dir, "config"))
	os.Unsetenv("CLAUDE_PRIMARY_NAME") // the owner's shell's, not the tests'
	os.Unsetenv("HEADROOM_PRIMARY_NAME")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// cli runs the command as a shell would, returning its exit status and output.
func cli(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	outR, outW, _ := os.Pipe()
	errR, errW, _ := os.Pipe()
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	got := make(chan [2]string)
	go func() {
		o, _ := io.ReadAll(outR)
		e, _ := io.ReadAll(errR)
		got <- [2]string{string(o), string(e)}
	}()
	code := run(args)
	outW.Close()
	errW.Close()
	os.Stdout, os.Stderr = stdout, stderr
	out := <-got
	return code, out[0], out[1]
}

var viewForm = regexp.MustCompile(`view: (\S+@[0-9a-f]{8})`)

func TestTheHotPathThroughTheCommand(t *testing.T) {
	w := testkit.New(t)
	notice := filepath.Join(os.Getenv("HOME"), ".agents/skills/drive-session/NOTICE.md")
	_ = os.MkdirAll(filepath.Dir(notice), 0o755)
	_ = os.WriteFile(notice, []byte("I am away.\n"), 0o644)
	newDriver = func(cfg config.Config) *drive.Driver {
		return &drive.Driver{Term: w, Host: w, Store: batch.Store{Dir: cfg.StateDir}, Cfg: cfg,
			Watches: &testkit.Watches{}, Self: "/bin/drive", Now: w.Clock.Now, Sleep: w.Clock.Sleep}
	}
	c := w.Add("T1")
	c.Busy("onboard")
	c.Done("analysis")
	short := c.Block[len(c.Block)-6:]

	if code, _, errs := cli(t, "batch", "new", "b1", "--wake", "none", "--tz", "Asia/Shanghai"); code != 0 {
		t.Fatalf("batch new: %d %s", code, errs)
	}
	if code, _, errs := cli(t, "send", short, "go"); code != 1 || !strings.Contains(errs, "drive read") {
		t.Fatalf("a send without a view is refused, exit 1: %d %s", code, errs)
	}
	code, out, _ := cli(t, "read", short)
	m := viewForm.FindStringSubmatch(out)
	if code != 0 || m == nil || !strings.Contains(out, "analysis") || !strings.Contains(out, "(session "+c.SID+")") {
		t.Fatalf("read prints the reply, the session id and the view: %d %q", code, out)
	}
	if code, out, errs := cli(t, "send", m[1], "go", "--as", "T1"); code != 0 || !strings.Contains(out, "arrived T1: go, notice first") {
		t.Fatalf("send: %d %q %s", code, out, errs)
	}
	if code, out, _ := cli(t, "wait", "T1"); code != 0 || !strings.Contains(out, "its turn ended") || viewForm.FindString(out) == "" {
		t.Fatalf("wait prints the ending and its view: %d %q", code, out)
	}
	if code, out, _ := cli(t, "status", "--quick"); code != 0 || !strings.Contains(out, "T1") || !strings.Contains(out, "done") {
		t.Fatalf("status: %d %q", code, out)
	}
	if code, out, _ := cli(t, "log"); code != 0 || !strings.Contains(out, "arrived: go") {
		t.Fatalf("log: %d %q", code, out)
	}
	if code, _, _ := cli(t, "frobnicate"); code != 2 {
		t.Fatalf("an unknown command is a usage error: %d", code)
	}
}
