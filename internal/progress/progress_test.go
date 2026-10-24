package progress

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// claude-steps lists every pull request a session linked, each once, at the
// time it was first linked; a builder that landed two lists both.
func TestStepsListsEveryPullRequestTheSessionLinked(t *testing.T) {
	bin := t.TempDir()
	out := `{"branch":"b","labels":[],"pull_requests":[` +
		`{"at":"2026-10-21T15:00:00Z","repo":"planlab-ai/main","number":9520,"url":"https://github.com/planlab-ai/main/pull/9520","opened_here":true},` +
		`{"at":"2026-10-21T16:00:00Z","repo":"planlab-ai/main","number":9531,"url":"https://github.com/planlab-ai/main/pull/9531"}],` +
		`"events":[{"at":"2026-10-21T15:30:00Z","kind":"commit","dir":"/w/b"},{"at":"2026-10-21T14:00:00Z","kind":"commit","dir":"/w/a"},` +
		`{"at":"2026-10-21T15:40:00Z","kind":"snippet"},{"at":"2026-10-21T15:50:00Z","kind":"commit","dir":"/w/a"}]}`
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
	if strings.Join(s.CommitDirs, ",") != "/w/b,/w/a" {
		t.Fatalf("where it committed, each once, the newest last: %v", s.CommitDirs)
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
