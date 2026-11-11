package drive

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/qiushiyan/drive/internal/gitenv"
)

// Pickups places a brief's worktree and composes the first prompt of the
// session that picks it up. brief owns both; an open from a brief only asks
// it, so the pointer reaches the session without a model retyping it.
type Pickups interface {
	// Start picks the brief slug up from dir, a checkout of the project
	// whose briefs it is: the directory the session works in and its
	// first prompt.
	Start(dir, slug string) (Pickup, error)
}

// Pickup is a brief placed for a session.
type Pickup struct {
	Dir       string // the worktree the session works in
	Pointer   string // its first prompt: the skill and goal, then the Brief:, Drift: and Pickup gate: lines
	Branch    string
	Placement string // created, checked-out, tracking, resumed or review
	Slug      string
	// Gates are the brief's gates on world state, each with its live state
	// ("#11167 lands first: open"): advisory, as brief prints them.
	Gates []string
}

// BriefCLI picks briefs up with brief's own start, in the form it gives a
// program (`brief start <slug> --json`).
type BriefCLI struct {
	Home string // where ~/.local/bin/brief is looked for when brief is not on PATH
}

// pickupLimit bounds a pickup, which pulls the shared corpus and may create
// a worktree, copying what gwt copies into it.
var pickupLimit = 3 * time.Minute

// Start implements Pickups.
func (c BriefCLI) Start(dir, slug string) (Pickup, error) {
	bin, err := exec.LookPath("brief")
	if err != nil {
		bin = filepath.Join(c.Home, ".local/bin/brief")
	}
	ctx, cancel := context.WithTimeout(context.Background(), pickupLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "start", slug, "--json")
	cmd.Dir, cmd.Env = dir, gitenv.Env()
	cmd.WaitDelay = 2 * time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return Pickup{}, fmt.Errorf("brief start %s gave no answer within %s", slug, pickupLimit)
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return Pickup{}, errors.New(tail(msg, 600))
	}
	var doc struct {
		Dir, Pointer, Branch, Placement, Slug string
		BlockedBy                             []struct {
			Gate string
			Live []string
		} `json:"blocked_by"`
	}
	if err := json.Unmarshal(out, &doc); err != nil || doc.Dir == "" || doc.Pointer == "" {
		return Pickup{}, fmt.Errorf("brief start %s --json printed no placement (%q); is brief older than its --json?", slug, clip(string(out), 120))
	}
	pk := Pickup{Dir: doc.Dir, Pointer: doc.Pointer, Branch: doc.Branch, Placement: doc.Placement, Slug: cmp.Or(doc.Slug, slug)}
	for _, g := range doc.BlockedBy {
		if len(g.Live) > 0 {
			g.Gate += ": " + strings.Join(g.Live, ", ")
		}
		pk.Gates = append(pk.Gates, g.Gate)
	}
	return pk, nil
}

// A start note is what a planning session writes for one session of a
// batch: a head saying how the session opens and what it is for, then
// whatever the session should know. The head is the note's first lines,
// after any title:
//
//	opens: brief <slug>                     the brief that carries the work
//	opens: /pl-loopy-debug 37439cd4 — …     or the line the owner would type
//	goal: what the session sets out to do, for the owner
type note struct {
	path  string
	opens string // the line to type, when no brief carries the work
	brief string // the slug of the brief that does
	goal  string
	body  string // the note without its head
}

var noteHead = regexp.MustCompile(`^(opens|goal):\s*(.*)$`)

// readNote reads the start note at path. The head is its first lines after
// any title, each key's value running on over lines a writer wrapped until
// the next key or a blank line. A note that names no opening, or no goal
// when the open gives none, is refused, so an open never guesses where a
// session starts or what it is for; so is a brief handed over as a note.
func readNote(path string, goalGiven bool) (*note, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Refused{Reason: fmt.Sprintf("cannot read the start note %s: %v", path, err)}
	}
	text := string(data)
	if isBrief(text) {
		slug := strings.TrimSuffix(filepath.Base(path), ".md")
		return nil, &Refused{Reason: fmt.Sprintf("%s is a brief, not a start note", path),
			Next: "open it with --brief " + slug + " in place of --from, from the project's checkout"}
	}
	n := &note{path: path}
	var kept []string
	key := "" // the head key whose value a wrapped line goes on with
	head := true
	for l := range strings.SplitSeq(text, "\n") {
		t := strings.TrimSpace(l)
		switch m := noteHead.FindStringSubmatch(t); {
		case head && m != nil:
			key = m[1]
			n.set(key, m[2])
			continue
		case head && key != "" && t != "" && !strings.HasPrefix(t, "#"):
			n.set(key, t) // a value wrapped onto the next line
			continue
		case head && (t == "" || strings.HasPrefix(t, "#")):
			key = "" // a blank line or a heading ends a wrapped value
		case head:
			head = false // the head is over at the first line of the note's own text
		}
		kept = append(kept, l)
	}
	n.body = strings.TrimSpace(regexp.MustCompile(`\n{3,}`).ReplaceAllString(strings.Join(kept, "\n"), "\n\n"))
	if slug, ok := strings.CutPrefix(n.opens, "brief "); ok {
		n.brief, n.opens = strings.TrimSpace(slug), ""
		if n.brief == "" || strings.ContainsAny(n.brief, " \t") {
			return nil, &Refused{Reason: fmt.Sprintf("the start note %s opens on %q: a brief is one slug, as in opens: brief sandbox-grep-end-anchor", path, "brief "+n.brief),
				Next: "have the session that wrote the note name one brief there, and any other as reading in its text"}
		}
	}
	switch {
	case n.opens == "" && n.brief == "":
		return nil, &Refused{Reason: fmt.Sprintf("the start note %s says nothing about how the session opens", path),
			Next: "have the session that wrote it add, at its head, opens: brief <slug> when a brief carries the work, or opens: <the line the owner would type to start it>"}
	case n.goal == "" && !goalGiven:
		return nil, &Refused{Reason: fmt.Sprintf("the start note %s has no goal: line", path),
			Next: "have the session that wrote it add one under its opens: line, or give the open --goal"}
	}
	return n, nil
}

// set adds a head line's words to key's value.
func (n *note) set(key, words string) {
	v := &n.opens
	if key == "goal" {
		v = &n.goal
	}
	*v = strings.TrimSpace(*v + " " + strings.TrimSpace(words))
}

// isBrief says whether text is a brief rather than a start note: a brief's
// head is fenced, and names the gate its pickup runs.
func isBrief(text string) bool {
	head, ok := strings.CutPrefix(strings.TrimLeft(text, "\n"), "---\n")
	if !ok {
		return false
	}
	head, _, _ = strings.Cut(head, "\n---")
	return regexp.MustCompile(`(?m)^pickup:`).MatchString(head)
}

// sharedCheckout is the sentence a session opening in a project's main
// checkout gets: other sessions work there too.
const sharedCheckout = "This checkout is shared: work in a worktree of your own, and install nothing in this one."

// mainCheckout says whether dir is a repository's main checkout rather than
// a linked worktree, which is where other sessions of a batch work too.
func mainCheckout(dir string) bool {
	git := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	git.Dir, git.Env = dir, gitenv.Env()
	out, err := git.Output()
	if err != nil {
		return false
	}
	f := strings.Fields(string(out))
	return len(f) == 2 && f[0] == f[1]
}

// opening composes the first step of a session opened from a brief or a
// start note: the line that starts it, then what the driver added, then the
// note, and a brief's Brief:, Drift: and Pickup gate: lines last, as brief
// puts the gate last.
func opening(first, pointerLines, added string, shared bool, n *note) string {
	parts := []string{first, added}
	if shared {
		parts = append(parts, sharedCheckout)
	}
	if n != nil && n.body != "" {
		parts = append(parts, "The start note for this session, from "+n.path+":\n\n"+n.body+"\n\n(The start note ends here.)")
	}
	parts = append(parts, pointerLines)
	var kept []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, "\n\n") + "\n"
}

// addedText is the text of the step a driver gave beside --from or
// --brief: a line, or a file's contents.
func addedText(step Step) (string, error) {
	switch {
	case step.Key != "":
		return "", &Refused{Reason: "an open from a start note or a brief takes a line or --file beside it, not a snippet: send the snippet as the session's next step"}
	case step.File != "":
		data, err := os.ReadFile(step.File)
		if err != nil {
			return "", &Refused{Reason: fmt.Sprintf("cannot read %s: %v", step.File, err)}
		}
		return string(data), nil
	}
	return step.Line, nil
}
