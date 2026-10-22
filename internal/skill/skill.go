// Package skill answers whether a slash command starts only when typed: a
// skill whose SKILL.md sets `disable-model-invocation: true` cannot be
// started by the model, so a file that tells the session to run it is
// refused inside the session, silently from the driver's side.
package skill

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var slash = regexp.MustCompile(`^\s*/([A-Za-z0-9][A-Za-z0-9_.:-]*)`)

// Command is the skill a line starts, such as "strategic-review" for
// "/strategic-review 9531"; "" when the line is not a slash command.
func Command(line string) string {
	if m := slash.FindStringSubmatch(line); m != nil {
		return m[1]
	}
	return ""
}

// TypedOnly says whether a session working in dir under config would find
// the skill name with model invocation turned off. A skill it cannot find
// is not typed-only: it may be a built-in command or a plugin's.
func TypedOnly(home, config, dir, name string) bool {
	name = strings.TrimPrefix(name[strings.LastIndex(name, ":")+1:], "/")
	for _, base := range searchPath(home, config, dir) {
		if typedOnly(filepath.Join(base, name, "SKILL.md")) {
			return true
		}
	}
	return false
}

func searchPath(home, config, dir string) []string {
	var out []string
	if dir != "" {
		out = append(out, filepath.Join(dir, ".claude/skills"))
		if top, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output(); err == nil {
			out = append(out, filepath.Join(strings.TrimSpace(string(top)), ".claude/skills"))
		}
	}
	if config != "" {
		out = append(out, filepath.Join(config, "skills"))
	}
	return append(out, filepath.Join(home, ".claude/skills"), filepath.Join(home, ".agents/skills"))
}

func typedOnly(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return false
	}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "---" {
			return false
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(k) == "disable-model-invocation" {
			return strings.TrimSpace(v) == "true"
		}
	}
	return false
}
