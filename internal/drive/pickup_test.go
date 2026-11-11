package drive_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
	if rest, _ := os.ReadFile(sent.Path); !strings.HasPrefix(string(rest), goal+"\n\nStop at the end of onboarding.") {
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
