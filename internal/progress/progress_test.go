package progress

import (
	"os"
	"path/filepath"
	"testing"
)

// claude-steps lists every pull request a session linked, each once, at the
// time it was first linked; a builder that landed two lists both.
func TestStepsListsEveryPullRequestTheSessionLinked(t *testing.T) {
	bin := t.TempDir()
	out := `{"branch":"b","labels":[],"pull_requests":[` +
		`{"at":"2026-10-21T15:00:00Z","repo":"planlab-ai/main","number":9520,"url":"https://github.com/planlab-ai/main/pull/9520","opened_here":true},` +
		`{"at":"2026-10-21T16:00:00Z","repo":"planlab-ai/main","number":9531,"url":"https://github.com/planlab-ai/main/pull/9531"}]}`
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
}
