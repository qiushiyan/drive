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
	"github.com/qiushiyan/drive/internal/progress"
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

// gwtWorld answers as gwt does for the branches the contract's sessions were
// on: pool-drain merged, pool-drain-spike not. A removed branch has no verdict
// any more. Nothing has steps or pull requests to read.
type gwtWorld struct{ removed map[string]bool }

func (gwtWorld) Steps(string) (progress.Steps, error)   { return progress.Steps{}, nil }
func (gwtWorld) PR(progress.PRRef) (progress.PR, error) { return progress.PR{}, nil }
func (g gwtWorld) Remove(_, branch string) error {
	g.removed[branch] = true
	return nil
}
func (g gwtWorld) Landed(_ string, branches []string, _ []progress.Commit) (progress.Landing, error) {
	merged := map[string]bool{"pool-drain-spike": false}
	if !g.removed["pool-drain"] {
		merged["pool-drain"] = true
	}
	return progress.Landing{Trunk: "main", Branches: branches, Merged: merged}, nil
}

// fake is drive wired to a fake world, as main wires it to this machine.
func fake(w *testkit.World) clikit.App {
	a := app
	gwt := gwtWorld{removed: map[string]bool{}}
	a.Options = append(slices.Clone(app.Options), kong.Bind(wiring(func(cfg config.Config) *drive.Driver {
		return &drive.Driver{Term: w, Host: w, Store: batch.Store{Dir: cfg.StateDir}, Cfg: cfg, Progress: gwt,
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
