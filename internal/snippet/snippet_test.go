package snippet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiushiyan/drive/internal/gitenv"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(gitenv.Env(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s", args, out)
	}
}

// A planlab worktree sees the main checkout's personal file, which gives
// `closeout` its own meaning over the global one.
func TestWorktreeSeesTheMainCheckoutsPersonalKeys(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".config/tabtype/config.toml"), `
[[snippets]]
key = "closeout"
expand = "merge to main and install"

[[snippets]]
key = "recap"
expand = "Recap this session. $0 Thanks."
`)
	main := filepath.Join(t.TempDir(), "main")
	_ = os.MkdirAll(main, 0o755)
	gitT(t, main, "init", "-q")
	gitT(t, main, "commit", "-q", "--allow-empty", "-m", "x")
	write(t, filepath.Join(main, ".tabtype.local.toml"), `
[[snippets]]
key = "closeout"
expand = "/pl-loopy-handoff-distill merged and deployed"
`)
	wt := filepath.Join(t.TempDir(), "wt")
	gitT(t, main, "worktree", "add", "-q", wt)

	s, err := Expand(home, wt, "closeout", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.Text != "/pl-loopy-handoff-distill merged and deployed" || !strings.HasSuffix(s.From, filepath.Join("main", ".tabtype.local.toml")) {
		t.Fatalf("the main checkout's key wins in a worktree: %+v", s)
	}
	r, _ := Expand(home, wt, "recap", "Short.")
	if r.Text != "Recap this session. Short. Thanks." {
		t.Fatalf("text goes where $0 is: %q", r.Text)
	}
	if _, err := Expand(home, wt, "nope", ""); err == nil || !strings.Contains(err.Error(), "drive snippets "+wt) {
		t.Fatalf("a missing key names how to list them: %v", err)
	}
}

func TestABrokenLayerIsAnError(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, ".config/tabtype/config.toml"), "[[snippets]\nkey=")
	if _, err := Expand(home, t.TempDir(), "x", ""); err == nil || !strings.Contains(err.Error(), "does not load") {
		t.Fatalf("got %v", err)
	}
}

// Git exports GIT_DIR and GIT_WORK_TREE to whatever it runs: a hook, or the
// command of `git rebase --exec`. A git started under them acts on that
// repository whatever directory it is given, so a suite run that way made
// its fixture commits in the checkout being rebased.
func TestGitNamesItsOwnRepositoryUnderAnInheritedOne(t *testing.T) {
	outer, work := t.TempDir(), t.TempDir()
	gitT(t, outer, "init", "-q")
	t.Setenv("GIT_DIR", filepath.Join(outer, ".git"))
	t.Setenv("GIT_WORK_TREE", outer)

	gitT(t, work, "init", "-q")
	gitT(t, work, "commit", "-q", "--allow-empty", "-m", "x")
	if _, err := os.Stat(filepath.Join(work, ".git")); err != nil {
		t.Fatalf("the fixture's repository was not made in its own directory: %v", err)
	}
	commits, _ := exec.Command("git", "--git-dir", filepath.Join(outer, ".git"), "rev-list", "--all").Output()
	if got := strings.TrimSpace(string(commits)); got != "" {
		t.Fatalf("the fixture's commit landed in the repository git ran the tests under: %s", got)
	}

	want, err := filepath.EvalSymlinks(work)
	if err != nil {
		t.Fatal(err)
	}
	if got := git(work, "rev-parse", "--show-toplevel"); got != want {
		t.Fatalf("a snippet's repository is read from its directory: got %q, want %q", got, want)
	}
}
