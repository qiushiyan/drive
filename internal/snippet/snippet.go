// Package snippet expands the owner's TabType snippets the way TabType does
// for a session working in a directory, so a driver sends the words the owner
// has already tuned rather than a paraphrase.
//
// Layers, lowest precedence first; in each, a TOML file wins over a JSON sibling:
//
//	~/.config/tabtype/config          global
//	~/.config/tabtype/config.local
//	<root>/.tabtype                   project, committed (the worktree's own)
//	<primary>/.tabtype.local          personal, from the main checkout when <root> is a linked worktree
//	<root>/.tabtype.local             personal, the worktree's own
//
// <root> is the directory's git toplevel.
package snippet

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Snippet is one expanded snippet and the file its key came from: a project
// can give a key a different meaning (planlab's `closeout` is a post-merge
// step; the global one merges and installs).
type Snippet struct {
	Key  string
	Text string
	From string
}

func git(dir string, args ...string) string {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Layers lists the files TabType reads for a directory, lowest precedence first.
func Layers(home, dir string) []string {
	conf := filepath.Join(home, ".config/tabtype")
	root := git(dir, "rev-parse", "--show-toplevel")
	if root == "" {
		root, _ = filepath.Abs(dir)
	}
	type layer struct{ base, name string }
	layers := []layer{{conf, "config"}, {conf, "config.local"}, {root, ".tabtype"}}
	common := git(dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if strings.HasSuffix(common, "/.git") {
		primary := filepath.Dir(common)
		if real(primary) != real(root) {
			layers = append(layers, layer{primary, ".tabtype.local"})
		}
	}
	layers = append(layers, layer{root, ".tabtype.local"})
	var files []string
	for _, l := range layers {
		for _, ext := range []string{"toml", "json"} {
			p := filepath.Join(l.base, l.name+"."+ext)
			if _, err := os.Stat(p); err == nil {
				files = append(files, p)
				break
			}
		}
	}
	return files
}

func real(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return p
}

type file struct {
	Snippets []struct {
		Key    string `toml:"key" json:"key"`
		Expand string `toml:"expand" json:"expand"`
	} `toml:"snippets" json:"snippets"`
}

func load(path string) (map[string]string, error) {
	var f file
	var err error
	if strings.HasSuffix(path, ".toml") {
		_, err = toml.DecodeFile(path, &f)
	} else {
		var b []byte
		if b, err = os.ReadFile(path); err == nil {
			err = json.Unmarshal(b, &f)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("%s does not load, so no key from it can be trusted: %w", path, err)
	}
	out := map[string]string{}
	for _, s := range f.Snippets {
		if s.Key != "" {
			out[s.Key] = s.Expand
		}
	}
	return out, nil
}

// Expand returns key's snippet for dir, with text where its $0 caret stop is,
// or after it when it has none: what the owner would have typed after it.
func Expand(home, dir, key, text string) (Snippet, error) {
	var found *Snippet
	for _, path := range Layers(home, dir) {
		keys, err := load(path)
		if err != nil {
			return Snippet{}, err
		}
		if body, ok := keys[key]; ok {
			found = &Snippet{Key: key, Text: body, From: path}
		}
	}
	if found == nil {
		return Snippet{}, fmt.Errorf("no snippet %q for %s; `drive snippets %s` lists the keys", key, dir, dir)
	}
	body := strings.Trim(found.Text, "\n")
	switch {
	case strings.Contains(body, "$0"):
		body = strings.Replace(body, "$0", text, 1)
	case text != "":
		body += "\n\n" + text // typed after an insert with no caret stop
	}
	found.Text = strings.TrimRight(body, " \n")
	return *found, nil
}

// List maps each key that applies in dir to the file it comes from.
func List(home, dir string) (map[string]string, []string, error) {
	effective := map[string]string{}
	for _, path := range Layers(home, dir) {
		keys, err := load(path)
		if err != nil {
			return nil, nil, err
		}
		for k := range keys {
			effective[k] = path
		}
	}
	names := make([]string, 0, len(effective))
	for k := range effective {
		names = append(names, k)
	}
	sort.Strings(names)
	return effective, names, nil
}
