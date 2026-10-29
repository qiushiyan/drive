package drive_test

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/testkit"
)

// What a refusal or a failure tells the driver to do is read by an agent
// and run as written: whole commands, each on the view the session stands
// at when the message is built. A chat driver, woken by webhook, cannot
// hold a command open, so it is never told to drive wait.

var printedView = regexp.MustCompile(`drive (?:respond|park|close|send) (\S+@[0-9a-f]{8})`)

func nextOf(err error) string {
	var r *drive.Refused
	var f *drive.Failed
	switch {
	case errors.As(err, &r):
		return r.Next
	case errors.As(err, &f):
		return f.Next
	}
	return ""
}

func TestEveryNextRunsAsWritten(t *testing.T) {
	for _, tc := range []struct {
		name, wake string
		act        func(r *rig, c *testkit.Claude) string // the Next to read
		want       []string
	}{
		{"a step into a permission prompt", "none", func(r *rig, c *testkit.Claude) string {
			c.Busy("clean up")
			c.Ask("rm -rf build")
			_, err := r.d.Send(r.view("T1"), drive.Step{Line: "go on"}, drive.SendOptions{})
			return nextOf(err)
		}, []string{" approve", " deny", "drive --batch b1 read T1 --pending"}},
		{"a step into a question", "none", func(r *rig, c *testkit.Claude) string {
			c.Busy("plan it")
			c.Set("blocked", "question", "Which database?")
			_, err := r.d.Send(r.view("T1"), drive.Step{Line: "go on"}, drive.SendOptions{})
			return nextOf(err)
		}, []string{" approve", " deny", "drive --batch b1 read T1 --screen"}},
		{"a wait that ends on a prompt", "none", func(r *rig, c *testkit.Claude) string {
			c.Busy("clean up")
			c.Ask("rm -rf build")
			e, _ := r.d.Wait("T1", time.Minute, time.Hour)
			return e.Next
		}, []string{" approve", " deny"}},
		{"a step while it works", "webhook", func(r *rig, c *testkit.Claude) string {
			c.Busy("build")
			_, err := r.d.Send(r.view("T1"), drive.Step{Line: "go on"}, drive.SendOptions{})
			return nextOf(err)
		}, []string{"--queue 'go on'"}},
		{"a step on a prompt nobody answered", "webhook", func(r *rig, c *testkit.Claude) string {
			c.Prompt("the owner typed this")
			_, err := r.d.Send(r.view("T1"), drive.Step{Line: "go on"}, drive.SendOptions{})
			return nextOf(err)
		}, []string{"its wake comes"}},
		{"a step another session holds", "webhook", func(r *rig, c *testkit.Claude) string {
			h := adopted(r.t, r, "T5")
			h.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
			if _, err := r.d.Send(r.view("T5"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"}); err != nil {
				r.t.Fatal(err)
			}
			_, err := r.d.Send(r.view("T1"), drive.Step{Line: "release"}, drive.SendOptions{Hold: "release"})
			return nextOf(err)
		}, []string{"drive --batch b1 park T1@", "drive --batch b1 watch --due"}},
		{"a close while it works, its wake spent", "webhook", func(r *rig, c *testkit.Claude) string {
			woken(r, c)
			c.Busy("build")
			return nextOf(r.d.Close(r.view("T1")))
		}, []string{"drive --batch b1 watch T1"}},
		{"a close over an unsent line", "none", func(r *rig, c *testkit.Claude) string {
			c.Edit(func(c *testkit.Claude) { c.Input = "hold off on the merge" })
			return nextOf(r.d.Close(r.view("T1")))
		}, []string{"drive --batch b1 park T1@"}},
		{"a close over a background job", "webhook", func(r *rig, c *testkit.Claude) string {
			c.Edit(func(c *testkit.Claude) {
				c.Shells = []session.Shell{{Started: r.w.Clock.Now(), Command: "pnpm test --watch"}}
			})
			return nextOf(r.d.Close(r.view("T1")))
		}, []string{"drive --batch b1 park T1@"}},
		{"a send whose Enter was lost", "none", func(r *rig, c *testkit.Claude) string {
			c.Edit(func(c *testkit.Claude) { c.LoseEnter = true })
			_, err := r.d.Send(r.view("T1"), drive.Step{Line: "go on"}, drive.SendOptions{})
			c.Edit(func(c *testkit.Claude) { c.LoseEnter = false })
			return nextOf(err)
		}, []string{" clear"}},
		{"a line that lost its start twice", "none", func(r *rig, c *testkit.Claude) string {
			c.Edit(func(c *testkit.Claude) {
				c.DropHead = 3
				// The garbled turn runs on, and the retype loses more than its pad.
				c.Reply = func(c *testkit.Claude, _ string) { c.Edit(func(c *testkit.Claude) { c.DropHead = 10 }) }
			})
			_, err := r.d.Send(r.view("T1"), drive.Step{Line: "update the changelog"}, drive.SendOptions{})
			return nextOf(err)
		}, []string{" interrupt", "drive --batch b1 read T1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.wake)
			c := adopted(t, r, "T1")
			next := tc.act(r, c)
			if next == "" {
				t.Fatal("a message with something left to do says it in its next")
			}
			for _, w := range tc.want {
				if !strings.Contains(next, w) {
					t.Errorf("%q is missing from: %s", w, next)
				}
			}
			for _, bad := range []string{"VIEW", "approve|deny", "rex "} {
				if strings.Contains(next, bad) {
					t.Errorf("%q does not run as written: %s", bad, next)
				}
			}
			if tc.wake == "webhook" && strings.Contains(next, "drive --batch b1 wait") {
				t.Errorf("a chat driver cannot hold a wait open: %s", next)
			}
			for _, m := range printedView.FindAllStringSubmatch(next, -1) {
				label, _, _ := strings.Cut(m[1], "@")
				if now := r.view(label); m[1] != now {
					t.Errorf("printed view %s, but %s stands at %s: %s", m[1], label, now, next)
				}
			}
		})
	}
}

// A driver refused because another holds the session hears what that one
// is doing, in words.
func TestABusyRefusalSaysWhatTheOtherDriverIsDoing(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.Busy("clean up")
	c.Ask("rm -rf build")
	v := r.view("T1")
	var other error
	r.w.OnCapture = func() { _, other = r.d.Send(v, drive.Step{Line: "go on"}, drive.SendOptions{}) }
	if _, err := r.d.Respond(v, "approve"); err != nil {
		t.Fatal(err)
	}
	if !isRefused(other) || !strings.Contains(other.Error(), "another driver is approving T1") {
		t.Fatalf("the other driver's action, in words: %v", other)
	}
}

// A read that fails before anything reached the session, such as a
// transcript that cannot be opened, is a refusal: nothing was done, and
// the same command can simply be run again.
func TestAReadThatFailsBeforeAnyEffectIsARefusal(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	v := r.view("T1")
	if err := os.Chmod(c.Transcript(), 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(c.Transcript(), 0o644)
	_, err := r.d.Send(v, drive.Step{Line: "go on"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(nextOf(err), "drive --batch b1 send "+v+" 'go on'") {
		t.Fatalf("a send refused, with itself to run again: %v", err)
	}
	_, err = r.d.Respond(v, "interrupt")
	if !isRefused(err) || !strings.Contains(nextOf(err), "drive --batch b1 respond "+v+" interrupt") {
		t.Fatalf("a respond refused, with itself to run again: %v", err)
	}
	if typed, keys := c.Seen(); len(typed) != 1 || len(keys) != 1 {
		t.Fatalf("nothing typed or pressed: %q %q", typed, keys)
	}
}

// A step decided on a view another run has since acted on is refused naming
// the step that run sent, so the driver knows the session did not move by
// itself: the prompt it took, and the one it is still answering.
func TestAStaleViewNamesTheStepAnotherRunSent(t *testing.T) {
	r := newRig(t, "none")
	a := adopted(t, r, "T1")
	old := r.view("T1")
	a.Edit(func(c *testkit.Claude) { c.Reply = func(*testkit.Claude, string) {} })
	if _, err := r.d.Send(old, drive.Step{Line: "recap please"}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := r.d.Send(old, drive.Step{Line: "recap please"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), `received another prompt since: the step "recap please" drive sent at`) {
		t.Fatalf("the prompt it took since: %v", err)
	}
	mid := r.view("T1")
	a.Say("Recap: built the flag.")
	_, err = r.d.Send(mid, drive.Step{Line: "next"}, drive.SendOptions{})
	if !isRefused(err) || !strings.Contains(err.Error(), `written more since, answering the step "recap please" drive sent at`) {
		t.Fatalf("the step it is still answering: %v", err)
	}
}

// A command a message gives runs as written wherever the driver reads it:
// free text, which may start with a dash, goes after every flag and behind
// --, and the batch the action ran in is named, current or not, since the
// line may run after its driver has started another.
func TestANextCommandCarriesItsTextAndBatch(t *testing.T) {
	r := newRig(t, "webhook")
	c := adopted(t, r, "T1")
	c.Busy("build")
	_, err := r.d.Send(r.view("T1"), drive.Step{Line: "-v is broken"}, drive.SendOptions{Hold: "release"})
	if next := nextOf(err); !strings.Contains(next, "drive --batch b1 send T1@") || !strings.Contains(next, "--queue --hold release -- '-v is broken'") {
		t.Fatalf("the line goes behind -- after the flags, its batch named: %s", next)
	}

	if _, _, err := r.d.Store.New(batch.Meta{Name: "b2", Started: r.w.Clock.Now(), Wake: "none"}); err != nil {
		t.Fatal(err)
	}
	r.d.BatchName = "b1"
	_, err = r.d.Send("T1@00000000", drive.Step{Line: "go on"}, drive.SendOptions{})
	if next := nextOf(err); !strings.Contains(next, "drive --batch b1 read T1") {
		t.Fatalf("a refusal in a batch that is not current names it: %s", next)
	}
}
