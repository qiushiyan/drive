package drive_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/testkit"
)

// On 2026-11-14 three sessions' input boxes read as holding typed text that
// no key removed: "?2026l", the tail of a terminal sequence, and
// "/drive/batches/2026-11-13/sends/sr-loopy-agent-misc-4.md", the end of a
// line drive had sent an hour earlier. ctrl+u, ctrl+a ctrl+k and vim's dG
// all left it, since the box held nothing: the screen kept cells from an
// earlier drawing, which Claude Code's redraw paints over (seen live on
// 11-16, in INSERT and in NORMAL). Every send and close was refused over a
// line that "may be the owner's". So a send, a close and a clear have the
// screen drawn again before they judge what the box holds.
func TestWhatAnEarlierDrawingLeftGoesWithARedrawBeforeTheBoxIsJudged(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.VimMode()
	c.Edit(func(c *testkit.Claude) { c.Smear = "?2026l" })
	// A read presses nothing, so it says what a send will do about it.
	const says = `input box shows text nobody sent: "?2026l"; a send or a close has its screen drawn again first, and refuses only if the text is still there`
	if rd, err := r.d.Read("T1", drive.ReadLast, 0); err != nil || !strings.Contains(rd.Text(), "its "+says) {
		t.Fatalf("a read says what shows and what a send does: %v\n%s", err, rd.Text())
	}
	if st, err := r.d.Status(false); err != nil || !strings.Contains(st.Text(r.w.Clock.Now()), "T1's "+says) {
		t.Fatalf("status says it too: %v\n%s", err, st.Text(r.w.Clock.Now()))
	}
	sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "Then write the changelog."}, drive.SendOptions{})
	if err != nil || sent.Outcome != "arrived" || sent.Redrew != "?2026l" || !strings.Contains(sent.Text(), `its input box showed "?2026l", which an earlier drawing of its screen left`) {
		t.Fatalf("the step goes once the screen is drawn again, and says so: %+v %v", sent, err)
	}
	c.Edit(func(c *testkit.Claude) {
		if c.Smear != "" || !slices.Contains(c.Keys, "ctrl+l") || slices.Contains(c.Keys, "ctrl+u") {
			t.Fatalf("the screen was drawn again, and nothing was deleted: %q %v", c.Smear, c.Keys)
		}
		c.Smear = "/sends/T1-4.md"
	})
	res, err := r.d.Respond(r.view("T1"), "clear")
	if err != nil || !res.Redrawn || !strings.Contains(res.Text(), "its box was empty already") {
		t.Fatalf("a clear finds the box empty once it is drawn again, and says so: %+v %v", res, err)
	}
	c.Edit(func(c *testkit.Claude) { c.Smear = "?25h" })
	if err := r.d.Close(r.view("T1")); err != nil {
		t.Fatalf("a close goes once the redraw shows an empty box: %v", err)
	}
}

// Whose text a box holds is never judged by what it looks like. A step the
// owner recalls into the box to edit reads as the end of a line drive sent,
// and a few characters of theirs can read as the tail of a terminal
// sequence: both are in the box after a redraw, so both stay until someone
// says they can go.
func TestTextTheBoxHoldsStaysWhateverItLooksLike(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	const step = "Then write the changelog for the release."
	if _, err := r.d.Send(r.view("T1"), drive.Step{Line: step}, drive.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, typed := range []string{step, "?25h"} {
		c.Edit(func(c *testkit.Claude) { c.Input = typed })
		if _, err := r.d.Send(r.view("T1"), drive.Step{Line: "Now cut the release."}, drive.SendOptions{}); !isRefused(err) || !strings.Contains(err.Error(), "it may be the owner's") {
			t.Fatalf("%q is in the box, so the send is refused: %v", typed, err)
		}
		if err := r.d.Close(r.view("T1")); !isRefused(err) || !strings.Contains(err.Error(), "holds an unsent line") {
			t.Fatalf("%q keeps the block open: %v", typed, err)
		}
		c.Edit(func(c *testkit.Claude) {
			if c.Input != typed {
				t.Fatalf("the line is as it was: %q, want %q", c.Input, typed)
			}
		})
	}
	// On someone's word it goes, by the keys that delete it.
	res, err := r.d.Respond(r.view("T1"), "clear")
	c.Edit(func(c *testkit.Claude) {
		if err != nil || res.Redrawn || c.Input != "" || !slices.Contains(c.Keys, "ctrl+u") {
			t.Fatalf("a clear deletes what the box holds: %+v %v %q %v", res, err, c.Input, c.Keys)
		}
	})
}

// On 2026-11-15 three steps reached one session as "iGo with option C: …":
// its box was in INSERT mode with nothing on the status line saying so, and
// the i typed to leave NORMAL mode went in as text. The i is taken back
// before the step is typed.
func TestAStepNeverArrivesBehindTheITypedToLeaveNormalMode(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.VimMode()
	c.Edit(func(c *testkit.Claude) { c.HideMode = true })
	var got string
	c.Edit(func(c *testkit.Claude) {
		c.Reply = func(c *testkit.Claude, prompt string) { got = prompt; c.Done("ok") }
	})
	sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "Go with option C."}, drive.SendOptions{})
	if err != nil || sent.Outcome != "arrived" || got != "Go with option C." {
		t.Fatalf("the step arrives as written: %q %+v %v", got, sent, err)
	}
}

// A session whose worktree its closeout removed stands in a directory that
// is no checkout any more, and a project's key resolved to nothing there
// (2026-11-13: "no snippet merge-when-ready for …/.worktrees/.trash/…/1").
// Its key resolves where the session was opened, and the send says so.
func TestAKeyResolvesWhereTheSessionWasOpenedOnceItsWorktreeIsGone(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	opened := c.Cwd
	if out, err := exec.Command("git", "-C", opened, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(opened, ".tabtype.toml"), []byte("[[snippets]]\nkey = \"merge-when-ready\"\nexpand = \"Merge it once its checks pass.\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	trash := filepath.Join(r.w.Root, ".trash", "1")
	c.Edit(func(c *testkit.Claude) { c.Cwd = trash })
	sent, err := r.d.Send(r.view("T1"), drive.Step{Key: "merge-when-ready"}, drive.SendOptions{})
	if err != nil || !strings.Contains(sent.Text(), ".tabtype.toml, read for "+opened+", where it was opened, since its own directory "+trash+" is no longer a checkout") {
		t.Fatalf("the key resolves in the directory it was opened in, and says so: %v %+v", err, sent)
	}
}

// A resume of a label's own conversation that names less than drive's own
// reopen line opens as that line would: where the session last stood, on
// the model it was opened on. Written by hand on 2026-11-13, one reopen
// brought a session back on the main checkout and another brought a review
// session back off its model.
func TestAResumeOfALabelsOwnConversationOpensAsItWas(t *testing.T) {
	r := newRig(t, "none")
	var launched []string
	r.d.Term = &recordingTerm{World: r.w, run: func(cmd []string) { launched = cmd }}
	work := filepath.Join(r.w.Root, "work")
	o, err := r.d.Open("sr-search", drive.Step{}, drive.OpenOptions{Dir: work, Model: "fable", Effort: "xhigh"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.d.Close(r.view("sr-search")); err != nil {
		t.Fatal(err)
	}
	again, err := r.d.Open("sr-search", drive.Step{}, drive.OpenOptions{Dir: r.w.Root, AnyDir: true, Resume: o.Session})
	if err != nil || len(launched) != 3 || !strings.Contains(launched[2], "--model fable --effort xhigh --resume "+o.Session) {
		t.Fatalf("it resumes on its model and effort: %v %q", err, launched)
	}
	if !strings.Contains(again.Text(), "it is sr-search's own conversation, so it opened as it was: in "+work+", where it last stood, on fable, the model it was opened on, at effort xhigh") {
		t.Fatalf("and says what it took from the record: %s", again.Text())
	}
	// Another conversation under the label is opened as the command says.
	if err := r.d.Close(r.view("sr-search")); err != nil {
		t.Fatal(err)
	}
	if other, err := r.d.Open("sr-search", drive.Step{}, drive.OpenOptions{Dir: r.w.Root, AnyDir: true, Resume: "some-other-session"}); err == nil && other.AsWas != "" {
		t.Fatalf("only its own conversation opens as it was: %+v", other)
	}
}
