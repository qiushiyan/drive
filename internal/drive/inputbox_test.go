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
// earlier drawing. Claude Code's redraw paints them over, and the clear says
// the box was empty.
func TestAClearRedrawsTextNoKeyRemoves(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	c.VimMode()
	c.Edit(func(c *testkit.Claude) { c.Smear = "?2026l" })
	res, err := r.d.Respond(r.view("T1"), "clear")
	if err != nil || !res.Redrawn || !strings.Contains(res.Text(), "its box was empty already") {
		t.Fatalf("cleared by a redraw, and said so: %+v %v", res, err)
	}
	c.Edit(func(c *testkit.Claude) {
		if c.Smear != "" || !slices.Contains(c.Keys, "ctrl+l") {
			t.Fatalf("the screen was drawn again: %q %v", c.Smear, c.Keys)
		}
	})
	// Text the box does hold needs no redraw.
	c.Edit(func(c *testkit.Claude) { c.Input, c.Keys = "a line of mine", nil })
	res, err = r.d.Respond(r.view("T1"), "clear")
	c.Edit(func(c *testkit.Claude) {
		if err != nil || res.Redrawn || slices.Contains(c.Keys, "ctrl+l") {
			t.Fatalf("typed text goes with ctrl+u alone: %+v %v %v", res, err, c.Keys)
		}
	})
}

// What drive or the terminal left on a session's screen is no one's line. A
// send clears it and goes; a close goes past it. Text that could be the
// owner's is still refused, for them to say.
func TestASendAndACloseGoPastALeftoverThatIsNoOnesLine(t *testing.T) {
	r := newRig(t, "none")
	c := adopted(t, r, "T1")
	first, err := r.d.Send(r.view("T1"), drive.Step{Line: "# land the pull request, then stop at ready to merge"}, drive.SendOptions{})
	if err != nil || first.Path == "" {
		t.Fatalf("a line with a leading # goes as a file: %+v %v", first, err)
	}
	tail := first.Path[len(first.Path)/2:]
	c.Edit(func(c *testkit.Claude) { c.Smear = tail })
	sent, err := r.d.Send(r.view("T1"), drive.Step{Line: "Then write the changelog."}, drive.SendOptions{})
	if err != nil || sent.Outcome != "arrived" || sent.Cleared != tail || !strings.Contains(sent.Text(), "its input box was cleared first") || !strings.Contains(sent.Text(), "the end of a line drive typed into it earlier") {
		t.Fatalf("the step goes once the leftover is cleared, and says so: %+v %v", sent, err)
	}
	c.Edit(func(c *testkit.Claude) { c.Smear = "?25h" })
	// A read and status say what it is, so no one is asked whose line it is.
	const says = `its input box shows "?25h", the end of a terminal sequence, which a send clears and a close passes`
	if rd, err := r.d.Read("T1", drive.ReadLast, 0); err != nil || !strings.Contains(rd.Text(), says) || strings.Contains(rd.Text(), "nobody sent") {
		t.Fatalf("a read names the leftover: %v\n%s", err, rd.Text())
	}
	if st, err := r.d.Status(false); err != nil || !strings.Contains(st.Text(r.w.Clock.Now()), "T1's "+says[4:]) {
		t.Fatalf("status names it too: %v\n%s", err, st.Text(r.w.Clock.Now()))
	}
	if err := r.d.Close(r.view("T1")); err != nil {
		t.Fatalf("a close goes past a terminal sequence's tail: %v", err)
	}

	c2 := adopted(t, r, "T2")
	c2.Edit(func(c *testkit.Claude) { c.Input = "ask it about the migration first" })
	if _, err := r.d.Send(r.view("T2"), drive.Step{Line: "Then write the changelog."}, drive.SendOptions{}); !isRefused(err) || !strings.Contains(err.Error(), "it may be the owner's") {
		t.Fatalf("a line someone may have typed stays: %v", err)
	}
	if err := r.d.Close(r.view("T2")); !isRefused(err) || !strings.Contains(err.Error(), "holds an unsent line") {
		t.Fatalf("and keeps its block open: %v", err)
	}
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
