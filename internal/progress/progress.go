// Package progress reads how far a session's work has come from the tools
// that already know: claude-steps for the stage it is in and the pull
// request it opened, gh for that pull request's state and checks.
package progress

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"slices"
	"sort"
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
	// CommitDirs are the directories the session committed in, each once,
	// the newest last: its worktree, however it reached it.
	CommitDirs []string
}

// PRRef names a pull request.
type PRRef struct {
	Repo   string
	Number int
	URL    string
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

// Landing is how a session's branches stand against their repository's
// trunk, which is how a change merged without a pull request is known.
type Landing struct {
	Repo     string          // the repository's main checkout
	Trunk    string          // the trunk's name, such as main
	Branches []string        // the branches judged, the newest worked on last
	Merged   map[string]bool // each branch that could be judged: merged into the trunk or not
}

// Source answers these questions; tests substitute their own.
type Source interface {
	Steps(sessionID string) (Steps, error)
	PR(ref PRRef) (PR, error)
	// Landed judges a session's branches in the repository dir is a
	// checkout of: those it names, then those checked out in the
	// directories it committed in that still exist.
	Landed(dir string, branches, commitDirs []string) (Landing, error)
}

// Tools runs claude-steps and gh.
type Tools struct{}

func run(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("%s: %s", name, strings.TrimSpace(strings.SplitN(string(ee.Stderr), "\n", 2)[0]))
		}
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// Landed implements Source through gwt, which owns every merged verdict, a
// squash or rebase merge included. gwt exits 1 when a branch cannot be
// judged, such as one already removed, and still judges the rest.
func (Tools) Landed(dir string, branches, commitDirs []string) (Landing, error) {
	branches = append([]string{}, branches...)
	for _, d := range commitDirs {
		git := exec.Command("git", "branch", "--show-current")
		git.Dir = d
		if out, err := git.Output(); err == nil {
			if b := strings.TrimSpace(string(out)); b != "" {
				branches = append(slices.DeleteFunc(branches, func(x string) bool { return x == b }), b)
			}
		}
	}
	if len(branches) == 0 {
		return Landing{}, fmt.Errorf("no branch to judge")
	}
	cmd := exec.Command("gwt", append(append([]string{"merged"}, branches...), "--json")...)
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
	l := Landing{Trunk: v.Trunk.Name, Branches: branches, Merged: map[string]bool{}}
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
			At     time.Time `json:"at"`
			Repo   string    `json:"repo"`
			Number int       `json:"number"`
			URL    string    `json:"url"`
		} `json:"pull_requests"`
		Events []struct {
			At   time.Time `json:"at"`
			Kind string    `json:"kind"`
			Dir  string    `json:"dir"`
		} `json:"events"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Steps{}, fmt.Errorf("claude-steps show --json: %w", err)
	}
	s := Steps{Branch: v.Branch}
	commits := v.Events[:0:0]
	for _, e := range v.Events {
		if e.Kind == "commit" && e.Dir != "" {
			commits = append(commits, e)
		}
	}
	sort.SliceStable(commits, func(i, j int) bool { return commits[i].At.Before(commits[j].At) })
	for _, e := range commits {
		s.CommitDirs = append(slices.DeleteFunc(s.CommitDirs, func(x string) bool { return x == e.Dir }), e.Dir)
	}
	for _, l := range v.Labels {
		if l.Latest != nil && l.Latest.At.After(s.StageAt) {
			s.Stage, s.StageAt = l.Name, l.Latest.At
		}
	}
	var newest time.Time
	seen := map[PRRef]bool{}
	for _, p := range v.PullRequests {
		if p.Number <= 0 {
			continue
		}
		ref := PRRef{Repo: p.Repo, Number: p.Number, URL: p.URL}
		if !p.At.Before(newest) {
			newest, s.PR = p.At, &ref
		}
		if key := (PRRef{Repo: p.Repo, Number: p.Number}); !seen[key] {
			seen[key] = true
			s.PRs = append(s.PRs, ref)
		}
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
