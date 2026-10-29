package progress

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// claude-steps lists every pull request a session linked, each once, at the
// time it was first linked; a builder that landed two lists both.
func TestStepsListsEveryPullRequestTheSessionLinked(t *testing.T) {
	bin := t.TempDir()
	out := `{"branch":"b","labels":[],"pull_requests":[` +
		`{"at":"2026-10-21T15:00:00Z","repo":"planlab-ai/main","number":9520,"url":"https://github.com/planlab-ai/main/pull/9520","opened_here":true},` +
		`{"at":"2026-10-21T16:00:00Z","repo":"planlab-ai/main","number":9531,"url":"https://github.com/planlab-ai/main/pull/9531"}],` +
		`"events":[{"at":"2026-10-21T15:30:00Z","kind":"commit","text":"second","dir":"/w/b"},{"at":"2026-10-21T14:00:00Z","kind":"commit","text":"first","dir":"/w/a"},` +
		`{"at":"2026-10-21T15:40:00Z","kind":"snippet"},{"at":"2026-10-21T15:50:00Z","kind":"commit","text":"third","dir":"/w/a"}]}`
	shim := "#!/bin/sh\necho '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude-steps"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s, err := Tools{}.Steps("s-1")
	if err != nil {
		t.Fatal(err)
	}
	if s.PR == nil || s.PR.Number != 9531 {
		t.Fatalf("the newest is the one status shows: %+v", s.PR)
	}
	if len(s.PRs) != 2 || s.PRs[0].Number != 9520 || s.PRs[1].Number != 9531 {
		t.Fatalf("every pull request linked, in the order linked: %+v", s.PRs)
	}
	if !s.PRs[0].Opened || s.PRs[1].Opened {
		t.Fatalf("the one it opened is told from the one it only named: %+v", s.PRs)
	}
	if len(s.Commits) != 3 || s.Commits[0].Dir != "/w/a" || s.Commits[2].Dir != "/w/a" || s.Commits[1].Text != "second" {
		t.Fatalf("its commits, oldest first: %+v", s.Commits)
	}
}

// Every stage the configuration names comes back in its order, the ones the
// session has no step under among them, and the newest is its stage.
func TestStepsListsEveryStageInTheConfigurationsOrder(t *testing.T) {
	bin := t.TempDir()
	out := `{"branch":"b","labels":[{"name":"onboard","latest":{"at":"2026-10-28T16:59:46Z"}},{"name":"spec","latest":null},` +
		`{"name":"build","latest":{"at":"2026-10-29T11:46:53Z"}},{"name":"verify","latest":{"at":"2026-10-29T12:57:51Z"}},{"name":"docs"}],` +
		`"pull_requests":[],"events":[]}`
	shim := "#!/bin/sh\necho '" + out + "'\n"
	if err := os.WriteFile(filepath.Join(bin, "claude-steps"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	s, err := Tools{}.Steps("s-1")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, l := range s.Labels {
		names = append(names, l.Name)
	}
	if strings.Join(names, ",") != "onboard,spec,build,verify,docs" {
		t.Fatalf("every stage, in the configuration's order: %v", names)
	}
	if !s.Labels[1].At.IsZero() || !s.Labels[4].At.IsZero() || s.Labels[2].At.IsZero() {
		t.Fatalf("a stage with no step has no time: %+v", s.Labels)
	}
	if s.Stage != "verify" || !s.StageAt.Equal(s.Labels[3].At) {
		t.Fatalf("the newest step's stage is the session's: %s %v", s.Stage, s.StageAt)
	}
}

// gwt exits 1 when one branch cannot be judged, such as one already
// removed, and still prints its verdict on the rest.
func TestLandedReadsVerdictsWhenOneBranchCannotBeJudged(t *testing.T) {
	bin := t.TempDir()
	out := `{"trunk":{"name":"main"},"branches":[{"branch":"shout","merged":true},{"branch":"gone","merged":null,"error":"no local branch gone"},{"branch":"park","merged":false}]}`
	shim := "#!/bin/sh\necho '" + out + "'\nexit 1\n"
	if err := os.WriteFile(filepath.Join(bin, "gwt"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	l, err := Tools{}.Landed(t.TempDir(), []string{"shout", "gone", "park"}, nil)
	if err != nil || l.Trunk != "main" || !l.Merged["shout"] || l.Merged["park"] {
		t.Fatalf("the verdicts it gave: %+v %v", l, err)
	}
	if _, judged := l.Merged["gone"]; judged {
		t.Fatalf("a branch it could not judge has no verdict: %+v", l)
	}
}

// A session's newest commit is found by its subject in the repository and
// judged by its id, which a closeout's removal of the branch leaves in place.
func TestLandedJudgesTheNewestCommitByItsId(t *testing.T) {
	repo := t.TempDir()
	t0 := time.Now().Add(-time.Hour).Truncate(time.Second)
	at := t0
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		date := at.Format(time.RFC3339)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
			"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "Add --shout")
	sha := git("rev-parse", "HEAD")
	at = t0.Add(time.Minute) // another session's commit, nearer the time the transcript saw this one
	git("commit", "-q", "--allow-empty", "-m", "Add CHANGELOG.md")
	bin := t.TempDir()
	// gwt answers each target it is given as merged.
	shim := "#!/bin/sh\nshift\nprintf '{\"trunk\":{\"name\":\"main\"},\"branches\":['\nsep=\nfor a in \"$@\"; do [ \"$a\" = --json ] && continue; printf '%s{\"branch\":\"%s\",\"merged\":true}' \"$sep\" \"$a\"; sep=,; done\nprintf ']}\\n'\n"
	if err := os.WriteFile(filepath.Join(bin, "gwt"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	l, err := Tools{}.Landed(repo, nil, []Commit{{At: t0.Add(50 * time.Second), Text: "Add --shout", Dir: filepath.Join(repo, "gone")}})
	if err != nil || l.Commit != sha || !l.Merged[sha] {
		t.Fatalf("its newest commit, by id: %+v %v (want %s)", l, err, sha)
	}
}

// A lookup that hangs is killed at its limit and says so, rather than
// holding whatever waits on it.
func TestAHungLookupIsKilled(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "claude-steps"), []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	defer func(was time.Duration) { lookupLimit = was }(lookupLimit)
	lookupLimit = 200 * time.Millisecond
	start := time.Now()
	_, err := Tools{}.Steps("s-1")
	if err == nil || !strings.Contains(err.Error(), "no answer within") {
		t.Fatalf("a hung lookup fails saying so: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("it is killed at its limit, not waited out: %s", took)
	}
}
