package rex

import (
	"encoding/json"
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

	c := CLI{Bin: bin}
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
