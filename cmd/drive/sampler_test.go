package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/testkit"
)

// sampled is drive wired to a fake world, as fake is, with the machine's
// sampler s.
func sampled(w *testkit.World, s *testkit.Sampler) clikit.App {
	a := app
	a.Options = append(slices.Clone(app.Options), kong.Bind(wiring(func(cfg config.Config) *drive.Driver {
		return &drive.Driver{Term: w, Host: w, Store: batch.Store{Dir: cfg.StateDir}, Cfg: cfg, Progress: gwtWorld{removed: map[string]bool{}},
			Watches: &testkit.Watches{}, Sampler: s, Self: "/bin/drive", Now: w.Clock.Now, Sleep: w.Clock.Sleep}
	})))
	return a
}

// samplerHome gives a test drive directories of its own, with config.toml
// holding conf.
func samplerHome(t *testing.T, conf string) (configDir string) {
	t.Helper()
	configDir = t.TempDir()
	t.Setenv("DRIVE_CONFIG_DIR", configDir)
	t.Setenv("DRIVE_STATE_DIR", t.TempDir())
	t.Setenv("REX_SESSION", "") // the Rex session these tests run in, which the fake world has not
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	return configDir
}

// The machine's sampler runs while any batch is live, whichever driver's,
// and stops once none is: the first batch going live starts it, another
// driver's leaves that one running, ending one of two leaves it, and ending
// the last stops it. One killed by hand while a batch is live runs again
// after the next command, a refused one included.
func TestTheSamplerRunsWhileAnyBatchIsLive(t *testing.T) {
	samplerHome(t, `sampler = ["~/bin/sample", "--interval", "5"]`+"\n")
	s := &testkit.Sampler{}
	a := sampled(testkit.New(t), s)
	step := func(what string, exit int, running bool, starts int, args ...string) {
		t.Helper()
		if code, out, errs := cli(a, args...); code != exit {
			t.Fatalf("%s: exit %d, want %d\n%s%s", what, code, exit, out, errs)
		}
		if s.Running() != running || s.Starts != starts {
			t.Fatalf("%s: running %v after %d starts, want %v after %d", what, s.Running(), s.Starts, running, starts)
		}
	}

	step("no batch is live", 0, false, 0, "batch", "list")
	step("a batch goes live", 0, true, 1, "--driver", "ada", "batch", "new", "ada-1")
	home, _ := os.UserHomeDir()
	if want := []string{filepath.Join(home, "bin/sample"), "--interval", "5"}; !slices.Equal(s.Runs(), want) {
		t.Fatalf("it runs the configured command, ~/ resolved: %q, want %q", s.Runs(), want)
	}
	step("another driver's batch goes live", 0, true, 1, "--driver", "bea", "batch", "new", "bea-1")
	step("one of the two ends", 0, true, 1, "--driver", "bea", "batch", "end")
	s.Kill()
	step("killed by hand, it runs after the live batch's next command", 0, true, 2, "--driver", "ada", "status")
	s.Kill()
	step("a refused command brings it back too", 1, true, 3, "--driver", "ada", "read", "nosuch")
	step("the last one ends", 0, false, 3, "--driver", "ada", "batch", "end")
	step("and it stays stopped", 0, false, 3, "batch", "list")
}

// With no sampler configured none runs; one the configuration no longer
// names stops at the next command, though a batch is live.
func TestNoSamplerConfiguredRunsNone(t *testing.T) {
	configDir := samplerHome(t, `sampler = ["/bin/sample"]`+"\n")
	s := &testkit.Sampler{}
	a := sampled(testkit.New(t), s)
	if code, out, errs := cli(a, "--driver", "ada", "batch", "new", "ada-1"); code != 0 || !s.Running() {
		t.Fatalf("a batch goes live, so it runs: exit %d, running %v\n%s%s", code, s.Running(), out, errs)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out, errs := cli(a, "--driver", "ada", "status"); code != 0 || s.Running() {
		t.Fatalf("unconfigured, it stops: exit %d, running %v\n%s%s", code, s.Running(), out, errs)
	}
	if code, out, errs := cli(a, "--driver", "bea", "batch", "new", "bea-1"); code != 0 || s.Running() || s.Starts != 1 {
		t.Fatalf("and none starts for a batch going live: exit %d, running %v after %d starts\n%s%s", code, s.Running(), s.Starts, out, errs)
	}
}
