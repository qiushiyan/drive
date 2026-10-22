// Package progress reads how far a session's work has come from the tools
// that already know: claude-steps for the stage it is in and the pull
// request it opened, gh for that pull request's state and checks.
package progress

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Steps is what claude-steps reads from a session's transcript.
type Steps struct {
	Stage   string    // the label whose latest step is newest, such as "review"
	StageAt time.Time // when that step happened
	Branch  string
	PR      *PRRef // the newest pull request the session opened or named
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

// Source answers both questions; tests substitute their own.
type Source interface {
	Steps(sessionID string) (Steps, error)
	PR(ref PRRef) (PR, error)
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
	}
	if err := json.Unmarshal(out, &v); err != nil {
		return Steps{}, fmt.Errorf("claude-steps show --json: %w", err)
	}
	s := Steps{Branch: v.Branch}
	for _, l := range v.Labels {
		if l.Latest != nil && l.Latest.At.After(s.StageAt) {
			s.Stage, s.StageAt = l.Name, l.Latest.At
		}
	}
	var newest time.Time
	for _, p := range v.PullRequests {
		if p.Number > 0 && !p.At.Before(newest) {
			newest = p.At
			s.PR = &PRRef{Repo: p.Repo, Number: p.Number, URL: p.URL}
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
