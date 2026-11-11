package drive_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/drive"
)

// pickups is brief as an open sees it: each slug's placement, or the
// refusal brief prints for it.
type pickups struct {
	placed  map[string]drive.Pickup
	refused map[string]string
	asked   []string // "dir slug", in the order asked
}

func (p *pickups) Start(dir, slug string) (drive.Pickup, error) {
	p.asked = append(p.asked, dir+" "+slug)
	if why, ok := p.refused[slug]; ok {
		return drive.Pickup{}, errors.New(why)
	}
	return p.placed[slug], nil
}

// modelSkill writes a skill the model may start itself, as planlab's
// onboarding and debug skills are.
func modelSkill(t *testing.T, r *rig, name string) {
	t.Helper()
	sk := filepath.Join(r.d.Cfg.Home, ".claude/skills", name)
	_ = os.MkdirAll(sk, 0o755)
	if err := os.WriteFile(filepath.Join(sk, "SKILL.md"), []byte("---\nname: "+name+"\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// firstPrompt is what the session opened as block was typed first, and the
// text of the file that line attaches.
func firstPrompt(t *testing.T, r *rig, block string) (typed, attached string) {
	t.Helper()
	seen, _ := r.w.Session(block).Seen()
	if len(seen) == 0 {
		t.Fatal("nothing was typed")
	}
	typed = strings.TrimSpace(seen[0])
	if i := strings.LastIndex(typed, " @"); i >= 0 {
		data, err := os.ReadFile(typed[i+2:])
		if err != nil {
			t.Fatalf("the mention names no file: %q", typed)
		}
		attached = string(data)
	}
	return typed, attached
}

const pointer = "/pl-loopy-onboarding infra triage — the closing reads now due get verdicts\n\n" +
	"Brief: /h/.handoffs/p/infra/closing-reads.md\nDrift: ~/.local/bin/brief drift closing-reads\nPickup gate: /h/.claude/skills/handoff/pickup/build.md\n"

// A brief's session opens in the worktree brief placed, its first prompt
// the pointer's skill line typed as the owner's would be, and the rest
// attached: the notice, what the driver added, then the pointer's Brief:,
// Drift: and Pickup gate: lines, the gate last.
func TestAnOpenFromABriefStartsInItsWorktreeOnItsPointer(t *testing.T) {
	r := newRig(t, "none")
	modelSkill(t, r, "pl-loopy-onboarding")
	wt := filepath.Join(r.w.Root, "worktrees", "infra", "closing-reads")
	_ = os.MkdirAll(wt, 0o755)
	p := &pickups{placed: map[string]drive.Pickup{"closing-reads": {Dir: wt, Pointer: pointer, Placement: "created"}}}
	r.d.Pickups = p
	main := filepath.Join(r.w.Root, "main")
	o, err := r.d.Open("closing-reads", drive.Step{Line: "This turn is onboarding only."}, drive.OpenOptions{Dir: main, Brief: "closing-reads"})
	if err != nil || o.Sent == nil {
		t.Fatalf("%+v %v", o, err)
	}
	if len(p.asked) != 1 || p.asked[0] != main+" closing-reads" {
		t.Fatalf("brief is asked once, from the project's checkout: %q", p.asked)
	}
	if got := r.w.Session(o.Block).Cwd; got != wt {
		t.Fatalf("the session opens in the brief's worktree: %s", got)
	}
	typed, attached := firstPrompt(t, r, o.Block)
	if !strings.HasPrefix(typed, "/pl-loopy-onboarding infra triage — the closing reads now due get verdicts @") {
		t.Fatalf("the prompt opens with the pointer's skill line: %q", typed)
	}
	notice, added, gate := strings.Index(attached, noticeText), strings.Index(attached, "This turn is onboarding only."), strings.Index(attached, "Brief: ")
	if notice != 0 || added < notice || gate < added || !strings.HasSuffix(attached, "Pickup gate: /h/.claude/skills/handoff/pickup/build.md\n") {
		t.Fatalf("the notice, what the driver added, then the pointer's lines, the gate last: %q", attached)
	}
	if strings.Contains(attached, "shared") {
		t.Fatalf("a session in its own worktree is not told to make one: %q", attached)
	}
	if evs := r.events(batch.KindOpenIntent); len(evs) != 1 || evs[0].Brief != "closing-reads" || evs[0].Text != "This turn is onboarding only." {
		t.Fatalf("the open's record names the brief and the step as given: %+v", evs)
	}
	if evs := r.events(batch.KindOpen); evs[0].Dir != wt {
		t.Fatalf("the block's record names the worktree: %+v", evs[0])
	}
}

// A start note says how its session opens. On a line, the session opens in
// the project's checkout on that line, the note's goal its goal, the note's
// text attached, and in a main checkout it is told to work in a worktree
// of its own. On a brief, brief places it as --brief does.
func TestAnOpenFromAStartNote(t *testing.T) {
	r := newRig(t, "none")
	modelSkill(t, r, "pl-loopy-debug")
	modelSkill(t, r, "pl-loopy-onboarding")
	main := filepath.Join(r.w.Root, "main")
	_ = os.MkdirAll(main, 0o755)
	if out, err := exec.Command("git", "init", "-q", main).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	notes := filepath.Join(r.w.Root, "notes")
	_ = os.MkdirAll(notes, 0o755)
	line := filepath.Join(notes, "skill-page-titles.md")
	_ = os.WriteFile(line, []byte("# skill-page-titles\n\nopens: /pl-loopy-debug 37439cd4 — the migration titled skill pages with their description\ngoal: No migration titles a skill page with anything but its name.\n\n## Evidence\n\n- 11 customer pages are mistitled.\n"), 0o644)
	o, err := r.d.Open("skill-page-titles", drive.Step{Line: "End on your diagnosis."}, drive.OpenOptions{Dir: main, From: line})
	if err != nil || o.Sent == nil {
		t.Fatalf("%+v %v", o, err)
	}
	typed, attached := firstPrompt(t, r, o.Block)
	if !strings.HasPrefix(typed, "/pl-loopy-debug 37439cd4 — the migration titled skill pages with their description @") {
		t.Fatalf("the prompt opens with the note's line: %q", typed)
	}
	for _, want := range []string{"End on your diagnosis.", "This checkout is shared: work in a worktree of your own", "# skill-page-titles", "11 customer pages are mistitled.", line} {
		if !strings.Contains(attached, want) {
			t.Fatalf("the attachment holds %q: %q", want, attached)
		}
	}
	if strings.Contains(attached, "opens:") || strings.Contains(attached, "goal:") {
		t.Fatalf("the note's head is drive's, not the session's: %q", attached)
	}
	if evs := r.events(batch.KindOpenIntent); evs[0].Goal != "No migration titles a skill page with anything but its name." || evs[0].From != line {
		t.Fatalf("the note's goal is the session's, and the open names the note: %+v", evs[0])
	}

	wt := filepath.Join(r.w.Root, "worktrees", "closing-reads")
	_ = os.MkdirAll(wt, 0o755)
	p := &pickups{placed: map[string]drive.Pickup{"closing-reads": {Dir: wt, Pointer: pointer}}}
	r.d.Pickups = p
	brief := filepath.Join(notes, "closing-reads.md")
	_ = os.WriteFile(brief, []byte("opens: brief closing-reads\ngoal: Take every owed read now due.\n\nThe frustration detector's precision fell to 0.6.\n"), 0o644)
	o, err = r.d.Open("closing-reads", drive.Step{}, drive.OpenOptions{Dir: main, From: brief})
	if err != nil || o.Sent == nil {
		t.Fatalf("%+v %v", o, err)
	}
	typed, attached = firstPrompt(t, r, o.Block)
	if len(p.asked) != 1 || r.w.Session(o.Block).Cwd != wt || !strings.HasPrefix(typed, "/pl-loopy-onboarding infra triage") ||
		!strings.Contains(attached, "precision fell to 0.6") || strings.Contains(attached, "shared") || !strings.HasSuffix(attached, "pickup/build.md\n") {
		t.Fatalf("a note naming a brief opens as --brief does, the note attached: asked %q, typed %q\n%s", p.asked, typed, attached)
	}
}

// What an open from a note or a brief cannot do it refuses before a block
// opens: a note that says nothing of its opening, and a brief brief will not
// start.
func TestAnOpenThatCannotComposeItsStepOpensNothing(t *testing.T) {
	r := newRig(t, "none")
	r.d.Pickups = &pickups{refused: map[string]string{"stale": "stale: error pickup gate missing\nstart: refused — fix the brief (`brief check stale`), then start again"}}
	dir := filepath.Join(r.w.Root, "main")
	bare := filepath.Join(r.w.Root, "bare.md")
	_ = os.WriteFile(bare, []byte("# a note\n\nWork on the titles.\n"), 0o644)
	if _, err := r.d.Open("bare", drive.Step{}, drive.OpenOptions{Dir: dir, From: bare}); !isRefused(err) || !strings.Contains(err.Error(), "says nothing about how the session opens") {
		t.Fatalf("a note with no opening is refused: %v", err)
	}
	_, err := r.d.Open("stale", drive.Step{}, drive.OpenOptions{Dir: dir, Brief: "stale"})
	var f *drive.Failed
	if !errors.As(err, &f) || !strings.Contains(f.What, "brief check stale") || !strings.Contains(f.Next, "--brief stale") {
		t.Fatalf("brief's refusal is the open's failure, and its next: opens it again whole: %v", err)
	}
	if blocks, _ := r.w.Blocks(""); len(blocks) != 0 {
		t.Fatalf("no block opened: %d", len(blocks))
	}
}

// A skill the model may start is typed as its line with the rest attached,
// as a typed-only one is, so the session's prompt opens with the skill; a
// line too long to sit beside the mention is cut at its dash. Queued, the
// skill stays in its file, since a mention typed mid-turn attaches nothing.
func TestASkillOnAStepsFirstLineIsTyped(t *testing.T) {
	r := newRig(t, "none")
	modelSkill(t, r, "pl-loopy-onboarding")
	c := adopted(t, r, "T1")
	goal := strings.TrimSpace(strings.Repeat("planners keep their place ", 18))
	f := filepath.Join(r.w.Root, "step.md")
	_ = os.WriteFile(f, []byte("/pl-loopy-onboarding ux — "+goal+"\n\nStop at the end of onboarding.\n"), 0o644)
	sent, err := r.d.Send(r.view("T1"), drive.Step{File: f}, drive.SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sent.Typed, "/pl-loopy-onboarding ux @") || len(sent.Typed) > 500 {
		t.Fatalf("a line too long is cut at its dash: %q", sent.Typed)
	}
	if rest, _ := os.ReadFile(sent.Path); !strings.HasPrefix(string(rest), "The typed line goes on: "+goal+"\n\nStop at the end of onboarding.") {
		t.Fatalf("the cut words open the attachment: %q", rest)
	}
	c.Busy("onboarding")
	sent, err = r.d.Send(r.view("T1"), drive.Step{File: f}, drive.SendOptions{Queue: true})
	if err != nil || !strings.HasPrefix(sent.Typed, "Read ") {
		t.Fatalf("queued, the skill stays in its file: %+v %v", sent, err)
	}
}

// BriefCLI reads brief start's --json document, and on a refusal carries
// what brief printed on stderr, where its mend is.
func TestBriefCLIReadsStartsDocument(t *testing.T) {
	bin := t.TempDir()
	shim := "#!/bin/sh\n[ \"$1 $3\" = 'start --json' ] || exit 9\ncase \"$2\" in\n" +
		" good) printf '%s\\n' '{\"slug\":\"good\",\"branch\":\"infra/good\",\"dir\":\"/w/infra/good\",\"placement\":\"created\",\"pointer\":\"/pl-loopy-onboarding infra — g\\n\\nBrief: /b.md\\n\"}' ;;\n" +
		" *) echo 'good: error pickup gate missing' >&2; echo 'start: refused — fix the brief' >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bin, "brief"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	pk, err := drive.BriefCLI{}.Start(t.TempDir(), "good")
	if err != nil || pk.Dir != "/w/infra/good" || pk.Branch != "infra/good" || pk.Placement != "created" || !strings.HasPrefix(pk.Pointer, "/pl-loopy-onboarding infra — g\n\nBrief: /b.md") {
		t.Fatalf("%+v %v", pk, err)
	}
	if _, err := (drive.BriefCLI{}).Start(t.TempDir(), "stale"); err == nil || !strings.Contains(err.Error(), "start: refused — fix the brief") {
		t.Fatalf("a refusal carries brief's words: %v", err)
	}
}

// writeNote writes a start note named label.md under the rig's notes folder.
func writeNote(t *testing.T, r *rig, label, text string) string {
	t.Helper()
	dir := filepath.Join(r.w.Root, "notes")
	_ = os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, label+".md")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// A note's head is read as its writer meant it: a value wrapped onto the
// next line runs on, a goal: in the note's own text stays there, the note
// lacking a goal is refused unless the open gives one, and --goal beats the
// note's. Outside a main checkout no worktree sentence is added.
func TestANoteHeadIsReadWhole(t *testing.T) {
	r := newRig(t, "none")
	modelSkill(t, r, "pl-loopy-debug")
	dir := filepath.Join(r.w.Root, "plain") // no repository, so no main checkout
	_ = os.MkdirAll(dir, 0o755)
	wrapped := writeNote(t, r, "wrapped", "opens: /pl-loopy-debug 37439cd4 — the migration titled skill pages\n  with their description\ngoal: No migration titles a skill page\n  with anything but its name.\n\nThe evidence.\ngoal: a heading in the note's own text\n")
	o, err := r.d.Open("wrapped", drive.Step{}, drive.OpenOptions{Dir: dir, From: wrapped})
	if err != nil {
		t.Fatal(err)
	}
	typed, attached := firstPrompt(t, r, o.Block)
	if !strings.HasPrefix(typed, "/pl-loopy-debug 37439cd4 — the migration titled skill pages with their description @") {
		t.Fatalf("a wrapped opens: runs on: %q", typed)
	}
	if evs := r.events(batch.KindOpenIntent); evs[0].Goal != "No migration titles a skill page with anything but its name." {
		t.Fatalf("a wrapped goal: runs on: %q", evs[0].Goal)
	}
	if !strings.Contains(attached, "goal: a heading in the note's own text") || strings.Contains(attached, "shared") || !strings.Contains(attached, "(The start note ends here.)") {
		t.Fatalf("a later goal: is the note's text, no worktree sentence outside a main checkout, and the note's end is marked: %q", attached)
	}
	bare := writeNote(t, r, "nogoal", "opens: /pl-loopy-debug 37439cd4 — titles\n\nThe evidence.\n")
	if _, err := r.d.Open("nogoal", drive.Step{}, drive.OpenOptions{Dir: dir, From: bare}); !isRefused(err) || !strings.Contains(err.Error(), "no goal: line") {
		t.Fatalf("a note with no goal is refused: %v", err)
	}
	mine := "Mine, as the owner said it."
	if _, err := r.d.Open("nogoal", drive.Step{}, drive.OpenOptions{Dir: dir, From: bare, Goal: &mine}); err != nil {
		t.Fatal(err)
	}
	if evs := r.events(batch.KindOpenIntent); evs[len(evs)-1].Goal != mine {
		t.Fatalf("--goal is the goal: %q", evs[len(evs)-1].Goal)
	}
	if _, err := r.d.Open("own", drive.Step{}, drive.OpenOptions{Dir: dir, From: wrapped, Goal: &mine}); err != nil {
		t.Fatal(err)
	}
	if evs := r.events(batch.KindOpenIntent); evs[len(evs)-1].Goal != mine {
		t.Fatalf("--goal beats the note's goal: %q", evs[len(evs)-1].Goal)
	}
}

// What is not a usable note is refused with what to do instead: a brief
// handed over as a note opens with --brief, a note naming two briefs names
// one, and --brief beside a note naming another brief is a contradiction;
// beside the same brief it is the note's own open.
func TestANoteThatCannotOpenSaysWhatWould(t *testing.T) {
	r := newRig(t, "none")
	modelSkill(t, r, "pl-loopy-onboarding")
	wt := filepath.Join(r.w.Root, "wt")
	_ = os.MkdirAll(wt, 0o755)
	r.d.Pickups = &pickups{placed: map[string]drive.Pickup{"closing-reads": {Dir: wt, Pointer: pointer, Slug: "closing-reads"}}}
	dir := filepath.Join(r.w.Root, "main")
	brief := writeNote(t, r, "sandbox-glob-folders-only", "---\ngoal: folders only\nanchored: 2026-11-11\npickup: design\n---\n\n## At pickup\n")
	if _, err := r.d.Open("glob", drive.Step{}, drive.OpenOptions{Dir: dir, From: brief}); !isRefused(err) || !strings.Contains(nextOf(err), "--brief sandbox-glob-folders-only") {
		t.Fatalf("a brief as a note says to open it with --brief: %v", err)
	}
	two := writeNote(t, r, "two", "opens: brief closing-reads thread-titles\ngoal: g\n")
	if _, err := r.d.Open("two", drive.Step{}, drive.OpenOptions{Dir: dir, From: two}); !isRefused(err) || !strings.Contains(err.Error(), "a brief is one slug") {
		t.Fatalf("a note naming two briefs is refused: %v", err)
	}
	note := writeNote(t, r, "closing-reads", "opens: brief closing-reads\ngoal: g\n")
	if _, err := r.d.Open("other", drive.Step{}, drive.OpenOptions{Dir: dir, From: note, Brief: "thread-titles"}); !isRefused(err) || !strings.Contains(err.Error(), "differ") {
		t.Fatalf("--brief naming another brief than the note's is refused: %v", err)
	}
	if blocks, _ := r.w.Blocks(""); len(blocks) != 0 {
		t.Fatalf("nothing opened: %d", len(blocks))
	}
	o, err := r.d.Open("closing-reads", drive.Step{}, drive.OpenOptions{Dir: dir, From: note, Brief: "closing-reads"})
	if err != nil || o.Placed == nil {
		t.Fatalf("--brief beside its own note's brief opens it: %+v %v", o, err)
	}
}

// The open's result says where brief placed the session and how, and each
// gate brief proceeds past; a brief's refusal is on record as the open's
// failure.
func TestABriefsOpenSaysWhereItPlacedTheSession(t *testing.T) {
	r := newRig(t, "none")
	modelSkill(t, r, "pl-loopy-onboarding")
	wt := filepath.Join(r.w.Root, "wt")
	_ = os.MkdirAll(wt, 0o755)
	r.d.Pickups = &pickups{
		placed:  map[string]drive.Pickup{"closing-reads": {Dir: wt, Pointer: pointer, Slug: "closing-reads", Placement: "resumed", Gates: []string{"#11167 lands first: open"}}},
		refused: map[string]string{"stale": "start: refused — fix the brief (`brief check stale`), then start again"},
	}
	dir := filepath.Join(r.w.Root, "main")
	o, err := r.d.Open("closing-reads", drive.Step{}, drive.OpenOptions{Dir: dir, Brief: "closing-reads"})
	if err != nil {
		t.Fatal(err)
	}
	text := o.Text()
	if !strings.Contains(text, "brief closing-reads placed it in "+wt+", on the worktree it already had, where another session may be working\n") ||
		!strings.Contains(text, "gated on #11167 lands first: open") {
		t.Fatalf("the result names the place, how, and the gate: %s", text)
	}
	if _, err := r.d.Open("stale", drive.Step{}, drive.OpenOptions{Dir: dir, Brief: "stale"}); err == nil {
		t.Fatal("a brief brief refuses opens nothing")
	}
	var failed bool
	for _, e := range r.events(batch.KindFailure) {
		failed = failed || e.Session == "stale" && strings.Contains(e.Reason, "brief check stale")
	}
	if !failed {
		t.Fatalf("brief's refusal is on record as the open's failure: %+v", r.events(batch.KindFailure))
	}
}

// An open from a note whose Claude Code never starts is asked for again
// whole, the note and what the driver added on it, and the log names the
// note.
func TestANotesOpenThatNeverStartsIsAskedForAgainWhole(t *testing.T) {
	r := newRig(t, "none")
	modelSkill(t, r, "pl-loopy-debug")
	r.w.NoStart = map[string]bool{"titles": true}
	dir := filepath.Join(r.w.Root, "work")
	note := writeNote(t, r, "titles", "opens: /pl-loopy-debug 37439cd4 — titles\ngoal: g\n")
	again := "drive --batch b1 open titles --dir " + dir + " --from " + note + " 'Build nothing yet.'"
	_, err := r.d.Open("titles", drive.Step{Line: "Build nothing yet."}, drive.OpenOptions{Dir: dir, From: note})
	var failed *drive.Failed
	if !errors.As(err, &failed) || !strings.HasSuffix(failed.Next, again) {
		t.Fatalf("the failed open is asked for again with its note: %v", err)
	}
	st, err := r.d.Status(false)
	if err != nil || len(st.Rows) != 1 || !strings.HasSuffix(st.Rows[0].Next, again) {
		t.Fatalf("status asks for it again whole: %+v %v", st.Rows, err)
	}
	b, evs, err := r.d.Log(time.Time{})
	if lg := drive.LogText(b, evs, r.w.Clock.Now()); err != nil || !strings.Contains(lg, "from "+note[:40]) {
		t.Fatalf("the log names the note: %v\n%s", err, lg)
	}
}

// A skill's line that neither fits beside the mention nor cuts at a dash
// stays whole in the file when the model may start the skill, its words
// still the skill's; a pointer line past 500 bytes is cut at its dash; a
// line naming no skill the session has goes as a file and says why.
func TestALongSkillLineIsCutOnlyAtItsDash(t *testing.T) {
	r := newRig(t, "none")
	modelSkill(t, r, "review")
	modelSkill(t, r, "pl-loopy-onboarding")
	adopted(t, r, "T1")
	long := "/review fable full review, then " + strings.TrimSpace(strings.Repeat("take the branch to a ready PR ", 14))
	f := filepath.Join(r.w.Root, "review.md")
	_ = os.WriteFile(f, []byte(long+"\n\nFix what you confirm.\n"), 0o644)
	sent, err := r.d.Send(r.view("T1"), drive.Step{File: f}, drive.SendOptions{})
	if err != nil || !strings.HasPrefix(sent.Typed, "@") {
		t.Fatalf("a long line with no dash stays in the file: %+v %v", sent, err)
	}
	if rest, _ := os.ReadFile(sent.Path); !strings.Contains(string(rest), long+"\n\nFix what you confirm.") {
		t.Fatalf("whole: %q", rest)
	}
	goal := strings.TrimSpace(strings.Repeat("planners keep their place ", 20))
	_ = os.WriteFile(f, []byte("/pl-loopy-onboarding tool-design ux — "+goal+"\n\nBrief: /b.md\n"), 0o644)
	sent, err = r.d.Send(r.view("T1"), drive.Step{File: f}, drive.SendOptions{})
	if err != nil || !strings.HasPrefix(sent.Typed, "/pl-loopy-onboarding tool-design ux @") {
		t.Fatalf("a pointer line past 500 bytes is cut at its dash: %+v %v", sent, err)
	}
	_ = os.WriteFile(f, []byte("/emil-prototype the reply card\n\nThe checklist variant.\n"), 0o644)
	sent, err = r.d.Send(r.view("T1"), drive.Step{File: f}, drive.SendOptions{})
	if err != nil || !strings.Contains(sent.Text(), "sent as a file since its first line names /emil-prototype, which no skill of the session's is called") {
		t.Fatalf("a line naming no skill says so: %+v %v", sent, err)
	}
}
