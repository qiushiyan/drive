package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"

	"github.com/alecthomas/kong"
	"github.com/qiushiyan/clikit"
	"github.com/qiushiyan/clikit/clikittest"

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

// fake is drive wired to a fake world, as main wires it to this machine.
func fake(w *testkit.World) clikit.App {
	a := app
	a.Options = append(slices.Clone(app.Options), kong.Bind(wiring(func(cfg config.Config) *drive.Driver {
		return &drive.Driver{Term: w, Host: w, Store: batch.Store{Dir: cfg.StateDir}, Cfg: cfg,
			Watches: &testkit.Watches{}, Self: "/bin/drive", Now: w.Clock.Now, Sleep: w.Clock.Sleep}
	})))
	return a
}

// cli runs drive as main does, returning its exit status and output.
func cli(a clikit.App, args ...string) (int, string, string) {
	r := clikittest.Run(a, upgrade(args)...)
	return r.Code, r.Stdout, r.Stderr
}

var viewForm = regexp.MustCompile(`view: (\S+@[0-9a-f]{8})`)
