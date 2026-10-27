// Package progress reads how far a session's work has come from the tools
// that already know: claude-steps for the stage it is in and the pull
// request it opened, gh for that pull request's state and checks.
package progress

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Steps is what claude-steps reads from a session's transcript.
type Steps struct {
	Stage   string    // the label whose latest step is newest, such as "review"
	StageAt time.Time // when that step happened
	Branch  string
	PR      *PRRef  // the newest pull request the session opened or named
	PRs     []PRRef // every one it opened or named, each once, in the order first named
	// Commits are the commits the session made, oldest first, each in the
	// checkout it was made in: its worktree, however it reached it.
	Commits []Commit
}

// Commit is a commit a session made, as its transcript shows it.
type Commit struct {
	At   time.Time
	Text string // its subject
	Dir  string
}

// PRRef names a pull request.
type PRRef struct {
	Repo   string
	Number int
	URL    string
	Opened bool // the session opened it, rather than only naming it
}

// PR is a pull request's state.
type PR struct {
	Number   int
	State    string // OPEN, MERGED, CLOSED
	Draft    bool
	Merge    string // GitHub's mergeStateStatus: CLEAN, DIRTY, BLOCKED, …
	Review   string // APPROVED, CHANGES_REQUESTED, …
	Passed   int
	Failed   int
	Pending  int
	MergedAt time.Time
}

// Landing is how a session's work stands against its repository's trunk,
// which is how a change merged without a pull request is known.
type Landing struct {
	Repo     string          // the repository's main checkout
	Trunk    string          // the trunk's name, such as main
	Branches []string        // the branches judged, the newest worked on last
	Commit   string          // the id of the session's newest commit, when the repository has it
	Merged   map[string]bool // each branch, and the commit, that could be judged: merged into the trunk or not
}

// Source answers these questions; tests substitute their own.
type Source interface {
	Steps(sessionID string) (Steps, error)
	PR(ref PRRef) (PR, error)
	// Landed judges a session's work in the repository dir is a checkout
	// of: its newest commit, and its branches, those it names then those
	// checked out where it committed and still are.
	Landed(dir string, branches []string, commits []Commit) (Landing, error)
}

// Tools runs claude-steps and gh.
type Tools struct{}

// lookupLimit bounds one lookup. These only read, so one that hangs, such
// as gh on a network that went away, is killed rather than holding a status
// or a page that waits on every lookup.
var lookupLimit = 30 * time.Second

func run(name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), lookupLimit)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second // a child it left holding the output does not hold the read
	out, err := cmd.Output()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%s: no answer within %s", name, lookupLimit)
	}
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s: %s", name, strings.TrimSpace(strings.SplitN(string(ee.Stderr), "\n", 2)[0]))
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Landed implements Source through gwt, which owns every merged verdict, a
// squash or rebase merge included, and judges a commit by its full id as it
// does a branch. The commit outlives the branch, which a closeout often
// removes itself. gwt exits 1 when a target cannot be judged, such as a
// removed branch, and still judges the rest.
func (Tools) Landed(dir string, branches []string, commits []Commit) (Landing, error) {
	branches = append([]string{}, branches...)
	for _, c := range commits {
		git := exec.Command("git", "branch", "--show-current")
		git.Dir = c.Dir
		if out, err := git.Output(); err == nil {
			if b := strings.TrimSpace(string(out)); b != "" {
				branches = append(slices.DeleteFunc(branches, func(x string) bool { return x == b }), b)
			}
		}
	}
	targets := append([]string{}, branches...)
	commit := commitID(dir, commits)
	if commit != "" {
		targets = append(targets, commit)
	}
	if len(targets) == 0 {
		return Landing{}, fmt.Errorf("no branch or commit to judge")
	}
	cmd := exec.Command("gwt", append(append([]string{"merged"}, targets...), "--json")...)
	cmd.Dir = dir
	out, runErr := cmd.Output()
	var v struct {
		Trunk struct {
			Name string `json:"name"`
		} `json:"trunk"`
		Branches []struct {
			Branch string `json:"branch"`
			Merged *bool  `json:"merged"`
		} `json:"branches"`
	}
	if err := json.Unmarshal(out, &v); err != nil || v.Trunk.Name == "" {
		if runErr != nil {
			return Landing{}, fmt.Errorf("gwt merged: %w", runErr)
		}
		return Landing{}, fmt.Errorf("gwt merged --json: %v", err)
	}
	l := Landing{Trunk: v.Trunk.Name, Branches: branches, Commit: commit, Merged: map[string]bool{}}
	for _, b := range v.Branches {
		if b.Merged != nil {
			l.Merged[b.Branch] = *b.Merged
		}
	}
	git := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir")
	git.Dir = dir
	if common, err := git.Output(); err == nil {
		l.Repo = strings.TrimSuffix(strings.TrimSpace(string(common)), "/.git")
	}
	return l, nil
}

// commitID finds the id of the newest of commits in the repository dir is a
// checkout of: the commit with its subject made within minutes of when the
// transcript saw it, the nearest if several. "" when the repository has none.
func commitID(dir string, commits []Commit) string {
	if len(commits) == 0 {
		return ""
	}
	newest := commits[len(commits)-1]
	git := exec.Command("git", "log", "--all", "--format=%H%x1f%ct%x1f%s", fmt.Sprintf("--since=%d", newest.At.Add(-time.Hour).Unix()))
	git.Dir = dir
	out, err := git.Output()
	if err != nil {
		return ""
	}
	best, gap := "", 5*time.Minute
	for line := range strings.SplitSeq(string(out), "\n") {
		f := strings.SplitN(line, "\x1f", 3)
		if len(f) != 3 || f[2] != newest.Text {
			continue
		}
		ct, err := strconv.ParseInt(f[1], 10, 64)
		if err != nil {
			continue
		}
		if d := time.Unix(ct, 0).Sub(newest.At).Abs(); d <= gap {
			best, gap = f[0], d
		}
	}
	return best
}

// Steps implements Source.
func (Tools) Steps(id string) (Steps, error) {
	out, err := run("claude-steps", "show", id, "--json")
	if err != nil {
		return Steps{}, err
	}
	var v struct {
		Branch string `json:"branch"`
		Labels []struct {
			Name   string `json:"name"`
			Latest *struct {
				At time.Time `json:"at"`
			} `json:"latest"`
		} `json:"labels"`
		PullRequests []struct {
			At         time.Time `json:"at"`
			Repo       string    `json:"repo"`
			Number     int       `json:"number"`
			URL        string    `json:"url"`
			OpenedHere bool      `json:"opened_here"`
		} `json:"pull_requests"`
		Events []struct {
			At   time.Time `json:"at"`
			Kind string    `json:"kind"`
			Text string    `json:"text"`
			Dir  string    `json:"dir"`
		} `json:"events"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Steps{}, fmt.Errorf("claude-steps show --json: %w", err)
	}
	s := Steps{Branch: v.Branch}
	for _, e := range v.Events {
		if e.Kind == "commit" && e.Text != "" {
			s.Commits = append(s.Commits, Commit{At: e.At, Text: e.Text, Dir: e.Dir})
		}
	}
	sort.SliceStable(s.Commits, func(i, j int) bool { return s.Commits[i].At.Before(s.Commits[j].At) })
	for _, l := range v.Labels {
		if l.Latest != nil && l.Latest.At.After(s.StageAt) {
			s.Stage, s.StageAt = l.Name, l.Latest.At
		}
	}
	var newest time.Time
	seen := map[PRRef]int{} // to its place in PRs
	for _, p := range v.PullRequests {
		if p.Number <= 0 {
			continue
		}
		ref := PRRef{Repo: p.Repo, Number: p.Number, URL: p.URL, Opened: p.OpenedHere}
		if !p.At.Before(newest) {
			newest, s.PR = p.At, &ref
		}
		key := PRRef{Repo: p.Repo, Number: p.Number}
		if i, ok := seen[key]; ok {
			s.PRs[i].Opened = s.PRs[i].Opened || p.OpenedHere
			continue
		}
		seen[key] = len(s.PRs)
		s.PRs = append(s.PRs, ref)
	}
	return s, nil
}

// PR implements Source.
func (Tools) PR(ref PRRef) (PR, error) {
	args := []string{"pr", "view", fmt.Sprint(ref.Number), "--json",
		"number,state,isDraft,mergeStateStatus,reviewDecision,statusCheckRollup,mergedAt"}
	if ref.Repo != "" {
		args = append(args, "-R", ref.Repo)
	}
	out, err := run("gh", args...)
	if err != nil {
		return PR{}, err
	}
	var v struct {
		Number           int       `json:"number"`
		State            string    `json:"state"`
		IsDraft          bool      `json:"isDraft"`
		MergeStateStatus string    `json:"mergeStateStatus"`
		ReviewDecision   string    `json:"reviewDecision"`
		MergedAt         time.Time `json:"mergedAt"`
		Checks           []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
			State      string `json:"state"`
		} `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return PR{}, fmt.Errorf("gh pr view %d: %w", ref.Number, err)
	}
	pr := PR{Number: v.Number, State: v.State, Draft: v.IsDraft, Merge: v.MergeStateStatus, Review: v.ReviewDecision, MergedAt: v.MergedAt}
	for _, c := range v.Checks {
		verdict := strings.ToUpper(c.Conclusion)
		if verdict == "" {
			verdict = strings.ToUpper(c.State) // status contexts carry a state, not a conclusion
		}
		switch {
		case c.Status != "" && c.Status != "COMPLETED", verdict == "PENDING", verdict == "EXPECTED":
			pr.Pending++
		case verdict == "SUCCESS", verdict == "NEUTRAL", verdict == "SKIPPED":
			pr.Passed++
		default:
			pr.Failed++
		}
	}
	return pr, nil
}
