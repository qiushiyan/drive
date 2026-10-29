package rex

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCLIAgainstAPrivateServer drives the real rex binary against a Rex
// server of its own, under a short temporary HOME (the server's socket lives
// there and must fit a Unix socket path), with every REX_ variable removed so
// nothing can reach the live server.
func TestCLIAgainstAPrivateServer(t *testing.T) {
	bin, err := exec.LookPath("rex")
	if err != nil {
		t.Skip("rex not on PATH")
	}
	home, err := os.MkdirTemp("/tmp", "drx.")
	if err != nil {
		t.Fatal(err)
	}
	home, _ = filepath.EvalSymlinks(home)
	t.Cleanup(func() {
		if strings.HasPrefix(home, "/private/tmp/drx.") || strings.HasPrefix(home, "/tmp/drx.") {
			os.RemoveAll(home)
		}
	})
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(k, "REX_") {
			v := os.Getenv(k)
			os.Unsetenv(k)
			t.Cleanup(func() { os.Setenv(k, v) })
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	_ = os.MkdirAll(filepath.Join(home, ".config/rex"), 0o755)

	// With no server running, a call fails and starts none: a server takes
	// the caller's environment into every pane it opens.
	c := CLI{Bin: bin}
	if _, err := c.Blocks(""); !errors.Is(err, ErrNoServer) {
		t.Fatalf("a call with no server says so: %v", err)
	}
	if exec.Command(bin, "--autostart=false", "server", "status").Run() == nil {
		t.Fatal("a call started a server")
	}

	server := exec.Command(bin, "--autostart=false", "server", "run")
	server.Dir = home
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = exec.Command(bin, "--autostart=false", "server", "stop").Run()
		done := make(chan struct{})
		go func() { server.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			server.Process.Kill()
		}
	})
	eventually(t, "server up", func() bool { return exec.Command(bin, "--autostart=false", "server", "status").Run() == nil })
	out, err := exec.Command(bin, "--autostart=false", "new", "drive-test", "--json", "--shell", "none", "-c", home, "--", "/bin/sh").Output()
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		Windows []struct {
			Blocks []string `json:"block_ids"`
		} `json:"initial_windows"`
	}
	if err := json.Unmarshal(out, &created); err != nil || len(created.Windows) == 0 {
		t.Fatalf("rex new: %s", out)
	}
	block := created.Windows[0].Blocks[0]

	blocks, err := c.Blocks("drive-test")
	if err != nil || len(blocks) != 1 || blocks[0].ID != block || blocks[0].Window == "" {
		t.Fatalf("blocks: %+v %v", blocks, err)
	}
	if all, err := c.Blocks(""); err != nil || len(all) != 1 {
		t.Fatalf("blocks across sessions: %+v %v", all, err)
	}
	if st, err := c.Status(block); err != nil || st != nil {
		t.Fatalf("a shell has no Claude record: %+v %v", st, err)
	}
	// What rex-agent writes for a Claude Code hook.
	if err := c.Type(block, `printf '\033]7501;state=blocked:kind=auth:id=claude:app=claude\033\\'`); err != nil {
		t.Fatal(err)
	}
	if err := c.Key(block, "enter"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "claude record", func() bool {
		st, _ := c.Status(block)
		return st != nil && st.State == "blocked" && st.Kind == "auth" && st.PID > 0 && !st.UpdatedAt.IsZero()
	})
	_ = c.Type(block, "echo drive-was-here")
	_ = c.Key(block, "enter")
	eventually(t, "typed output on screen", func() bool {
		s, _ := c.Capture(block, false)
		return strings.Count(s, "drive-was-here") >= 2
	})
	if vt, err := c.Capture(block, true); err != nil || !strings.Contains(vt, "drive-was-here") {
		t.Fatalf("vt capture: %v", err)
	}
	opened, err := c.Run(Open{Session: "drive-test", Dir: home, Label: "second", Command: []string{"/bin/sh"}})
	if err != nil || !strings.HasPrefix(opened, "block:") {
		t.Fatalf("run: %q %v", opened, err)
	}
	if blocks, _ := c.Blocks("drive-test"); len(blocks) != 2 || blocks[1].Label != "second" && blocks[0].Label != "second" {
		t.Fatalf("the new block is listed with its label: %+v", blocks)
	}
	if err := c.Close(opened); err != nil {
		t.Fatal(err)
	}
	eventually(t, "closed block gone", func() bool {
		blocks, _ := c.Blocks("drive-test")
		return len(blocks) == 1
	})
	if _, err := c.Status(opened); err == nil {
		t.Fatal("a closed block reads as an error")
	}

	// Run from a pane in another session, whose variables scope the rex
	// CLI's lookups to that pane, drive still finds its blocks.
	other, err := c.New(Open{Session: "elsewhere", Dir: home, Label: "pane", Command: []string{"/bin/sh"}})
	if err != nil {
		t.Fatal(err)
	}
	if blocks, _ := c.Blocks("elsewhere"); len(blocks) != 1 || blocks[0].ID != other || blocks[0].Label != "pane" {
		t.Fatalf("a new session's first block is the one asked for, under its label: %+v", blocks)
	}
	label, started, err := c.EnsureSession(sessionOf(t, bin, "elsewhere"))
	if err != nil || started || label != "elsewhere" {
		t.Fatalf("a session's id resolves to its label: %q %v %v", label, started, err)
	}
	t.Setenv("REX_SESSION", sessionOf(t, bin, "elsewhere"))
	t.Setenv("REX_BLOCK", other)
	if _, err := c.Capture(block, false); err != nil {
		t.Fatalf("a block in another session than the pane's: %v", err)
	}
	if _, err := c.Run(Open{Session: "gone", Dir: home, Label: "x", Command: []string{"/bin/sh"}}); !errors.Is(err, ErrNoSession) {
		t.Fatalf("a session that does not run: %v", err)
	}
	if _, _, err := c.EnsureSession("session:01a00000-0000-7000-8000-000000000000"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("an id names only a session that runs: %v", err)
	}
}

// sessionOf is the id of the Rex session label.
func sessionOf(t *testing.T, bin, label string) string {
	t.Helper()
	out, err := exec.Command(bin, "--autostart=false", "session", "ls", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var ls struct {
		Sessions []struct {
			ID    string `json:"session_id"`
			Label string `json:"label"`
		} `json:"sessions"`
	}
	_ = json.Unmarshal(out, &ls)
	for _, s := range ls.Sessions {
		if s.Label == label {
			return s.ID
		}
	}
	t.Fatalf("no session %s: %s", label, out)
	return ""
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(8 * time.Second); time.Now().Before(end); time.Sleep(50 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
