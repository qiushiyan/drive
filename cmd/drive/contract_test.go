package main

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/testkit"
)

// contract is a fake world on a fixed clock, under a home of its own, that
// runs drive as a driver does and pins what each command printed.
type contract struct {
	t    *testing.T
	w    *testkit.World
	home string
}

func newContract(t *testing.T) *contract {
	home := fixedHome(t)
	t.Setenv("HOME", home)
	t.Setenv("DRIVE_STATE_DIR", filepath.Join(home, "state"))
	t.Setenv("DRIVE_CONFIG_DIR", filepath.Join(home, "config"))
	t.Setenv("REX_SESSION", "")
	w := testkit.New(t)
	w.Root = filepath.Join(home, "world")
	w.Clock = testkit.NewClockAt(time.Date(2026, 10, 23, 1, 0, 0, 0, time.UTC))
	write(t, filepath.Join(home, ".agents/skills/drive-session/NOTICE.md"), "I am away.\n")
	write(t, filepath.Join(home, ".config/tabtype/config.toml"),
		"[[snippets]]\nkey = \"land-pr\"\nexpand = \"Open the PR and see it through code review.\"\n")
	newDriver = func(cfg config.Config) *drive.Driver {
		return &drive.Driver{Term: w, Host: w, Store: batch.Store{Dir: cfg.StateDir}, Cfg: cfg,
			Watches: &testkit.Watches{}, Self: "/bin/drive", Now: w.Clock.Now, Sleep: w.Clock.Sleep}
	}
	return &contract{t: t, w: w, home: home}
}

// fixedHome is a home whose path is as long on every machine and every
// run: drive clips long lines, and a path of another length would move
// where the clip falls.
func fixedHome(t *testing.T) string {
	t.Helper()
	home := fmt.Sprintf("/tmp/drive-contract-%08x", rand.Uint32())
	if err := os.Mkdir(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(home) })
	return home
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

var intentID = regexp.MustCompile(`"intent": ?"[0-9a-f]{8}"`)

// run runs drive and pins its exit status, stdout and stderr as the file
// testdata/contract/NAME.txt, returning stdout. Paths under the temporary
// directories read $HOME and $ROOT, and an intent's random id reads ….
func (c *contract) run(name string, args ...string) string {
	c.t.Helper()
	code, out, errs := cli(c.t, args...)
	clean := func(s string) string {
		s = strings.ReplaceAll(strings.ReplaceAll(s, c.w.Root, "$ROOT"), c.home, "$HOME")
		return intentID.ReplaceAllStringFunc(s, func(m string) string { return m[:len(m)-10] + `"…"` })
	}
	got := fmt.Sprintf("$ drive %s\nexit %d\n--- stdout\n%s--- stderr\n%s", clean(strings.Join(args, " ")), code, clean(out), clean(errs))
	pinContract(c.t, name, got)
	return out
}

// view is the view a read, wait or status output names.
func view(t *testing.T, out string) string {
	t.Helper()
	m := viewForm.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no view in %q", out)
	}
	return m[1]
}

func pinContract(t *testing.T, name, got string) {
	t.Helper()
	file := filepath.Join("testdata", "contract", name+".txt")
	if os.Getenv("DRIVE_CONTRACT_UPDATE") == "1" {
		write(t, file, got)
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Errorf("%s has no pinned output (DRIVE_CONTRACT_UPDATE=1 writes it):\n%s", name, got)
		return
	}
	if string(want) != got {
		t.Errorf("%s differs from %s; a driver reads this, so the drive-session skill changes in step (DRIVE_CONTRACT_UPDATE=1 rewrites it):\n--- want\n%s\n--- got\n%s", name, file, want, got)
	}
}

// What a driver reads, byte for byte: every command the drive-session skill
// teaches, and each command's refusals and failures, against the fake world
// on a fixed clock. A file under testdata/contract that changes changes what
// a driver reads, so the skill changes in the same pass.
func TestContracts(t *testing.T) {
	c := newContract(t)
	work := filepath.Join(c.w.Root, "work")
	t1 := c.w.Add("T1")
	t1.Busy("onboard")
	t1.Done("analysis: the pool drains in two steps")
	t2 := c.w.Add("T2")
	t2.Busy("land it")
	t2.Ask("pnpm install")
	block := func(cl *testkit.Claude) string { return cl.Block[len(cl.Block)-6:] }

	c.run("batch-new-webhook-unknown", "batch", "new", "b0", "--webhook", "nobody")
	c.run("batch-new", "batch", "new", "b1", "--tz", "Europe/London")
	c.run("batch", "batch")
	c.run("read-block-prompts", "read", block(t1), "--prompts", "10")
	v1 := view(t, c.run("read-block", "read", block(t1)))
	c.run("send-without-view", "send", block(t1), "go")
	c.run("send-two-steps", "send", v1, "go", "--file", filepath.Join(c.home, "nope.md"))
	c.run("send-first-snippet", "send", v1, "--as", "T1", "--snippet", "land-pr", "D3 waits for me.")
	c.run("wait", "wait", "T1")
	c.run("wait-json", "wait", "T1", "--json")
	c.run("read-label", "read", "T1")
	c.run("read-json", "read", "T1", "--json")
	c.run("read-user", "read", "T1", "--user")
	c.run("read-screen", "read", "T1", "--screen")
	c.run("send-stale-view", "send", "T1@00000000", "go")

	v2 := view(t, c.run("read-pending", "read", block(t2), "--pending"))
	c.run("respond-unknown-action", "respond", v2, "yes")
	c.run("respond-approve", "respond", v2, "approve")
	c.run("status", "status", "--quick")
	c.run("status-json", "status", "--quick", "--json")

	c.run("record-grant", "record", "grant", "--source", `"you can merge on my behalf" (02:08)`, "--scope", "merge this batch's PRs")
	c.run("record-grant-without-scope", "record", "grant", "--source", "go ahead")
	c.run("record-ask", "record", "ask", "T1", "--question", "Take the lab host down?", "--recommendation", "yes, after the read", "--if-unanswered", "it keeps running")
	c.run("record-ask-batch", "record", "ask", "-", "--question", "Release tonight?", "--if-unanswered", "it waits")
	c.run("record-answer", "record", "answer", "A1", "--source", "yes", "--took", "amended", "--explained")
	c.run("record-answer-unknown-took", "record", "answer", "A2", "--source", "no", "--took", "maybe")
	c.run("record-note", "record", "note", "plan: T1 land-pr; T2 approve and build")
	c.run("record-note-session", "record", "note", "--session", "T2", "told the user")

	v1 = view(t, c.run("read-label-again", "read", "T1"))
	c.run("park", "park", v1, "design checkpoint")
	c.run("watch-due", "watch", "--due")
	c.run("watch-session", "watch", "T1")
	c.run("watch-sleep", "watch", "--sleep", "300", "veto")
	c.run("snippet", "snippet", "land-pr", work, "D3", "waits")
	c.run("snippet-list", "snippet", "--list", work)
	c.run("snippet-unknown", "snippet", "nope", work)
	c.run("open", "open", "J1", "--dir", work, "Review the spec in docs/spec.md")
	c.run("open-label-taken", "open", "J1", "--dir", work, "again")
	v1 = view(t, c.run("read-label-parked", "read", "T1"))
	c.run("close", "close", v1)
	c.run("read-closed", "read", "T1")

	c.run("log", "log")
	c.run("log-since", "log", "--since", "2026-10-23 09:02")
	c.run("log-json", "log", "--json")
	c.run("metrics", "metrics")
	c.run("metrics-json", "metrics", "--json")
	c.run("batch-new-second", "batch", "new", "b2")
	c.run("batch-list", "batch", "list")
	c.run("log-other-batch-after", "log", "--batch", "b1", "--since", "2026-10-23 09:03")
	c.run("log-other-batch-before", "--batch", "b1", "log", "--since", "2026-10-23 09:03")
	c.run("batch-use", "batch", "use", "b1")
	c.run("batch-use-unknown", "batch", "use", "nope")
	c.run("unknown-command", "frobnicate")
}

// A step a rejected login answers exits 4 and prints the relaunch.
func TestContractLoginRejected(t *testing.T) {
	c := newContract(t)
	write(t, filepath.Join(c.home, ".claude-accounts", "other@example.com", "sessions", ".keep"), "")
	t4 := c.w.Add("T4")
	t4.Busy("onboard")
	t4.Done("analysis")
	t4.Reply = func(cl *testkit.Claude, _ string) {
		cl.Fail("authentication_failed", "Login expired · Please run /login")
		cl.Set("blocked", "auth", "Login expired · Please run /login")
	}
	c.run("login-batch-new", "batch", "new", "b4")
	v := view(t, c.run("login-read", "read", t4.Block[len(t4.Block)-6:]))
	c.run("login-send", "send", v, "go", "--as", "T4")
	c.run("login-read-after", "read", "T4")
}
