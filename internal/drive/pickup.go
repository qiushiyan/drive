package drive

import (
	"bytes"
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
	var doc struct{ Dir, Pointer, Branch, Placement string }
	if err := json.Unmarshal(out, &doc); err != nil || doc.Dir == "" || doc.Pointer == "" {
		return Pickup{}, fmt.Errorf("brief start %s --json printed no placement (%q); is brief older than its --json?", slug, clip(string(out), 120))
	}
	return Pickup(doc), nil
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

// readNote reads the start note at path, refusing one whose head names no
// opening, so an open never guesses where a session starts.
func readNote(path string) (*note, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Refused{Reason: fmt.Sprintf("cannot read the start note %s: %v", path, err)}
	}
	n := &note{path: path}
	lines := strings.Split(string(data), "\n")
	var kept []string
	head := true
	for _, l := range lines {
		t := strings.TrimSpace(l)
		if m := noteHead.FindStringSubmatch(t); head && m != nil {
			if m[1] == "opens" {
				n.opens = strings.TrimSpace(m[2])
			} else {
				n.goal = strings.TrimSpace(m[2])
			}
			continue
		}
		if head && t != "" && !strings.HasPrefix(t, "#") {
			head = false // the head is over at the first line of the note's own text
		}
		kept = append(kept, l)
	}
	n.body = strings.TrimSpace(strings.Join(kept, "\n"))
	if slug, ok := strings.CutPrefix(n.opens, "brief "); ok {
		n.brief, n.opens = strings.TrimSpace(slug), ""
		if n.brief == "" || strings.ContainsAny(n.brief, " \t") {
			return nil, &Refused{Reason: fmt.Sprintf("the start note %s opens on %q: a brief is one slug, as in opens: brief sandbox-grep-end-anchor", path, "brief "+n.brief)}
		}
	}
	if n.opens == "" && n.brief == "" {
		return nil, &Refused{Reason: fmt.Sprintf("the start note %s says nothing about how the session opens", path),
			Next: "have the session that wrote it add, as the note's first line, opens: brief <slug> when a brief carries the work, or opens: <the line the owner would type to start it>"}
	}
	return n, nil
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
		parts = append(parts, "The start note for this session, from "+n.path+":\n\n"+n.body)
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
