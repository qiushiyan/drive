package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/alecthomas/kong"
)

var quoted = regexp.MustCompile("`drive ([^`]*)`")

// Every drive command a message names is one this grammar runs, with the
// flags it gives: a command renamed here and left in a message sends the
// driver to a usage error. A message builds each command with the driver's
// run, which quotes it and names the batch, or quotes it in backticks; this
// reads both out of the source, so it covers messages no test reaches.
func TestEveryCommandAMessageNamesExists(t *testing.T) {
	k, err := kong.New(&CLI{}, app.Options...)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range k.Model.Children {
		names = append(names, c.Name)
	}
	bare := regexp.MustCompile(`\bdrive (` + strings.Join(names, "|") + `)\b`)
	files, _ := filepath.Glob("../../internal/*/*.go")
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				// d.run with its words written out; one spreading a slice
				// (sendCommand and the like) builds it from words read here too.
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "run" && isIdent(sel.X, "d") && n.Ellipsis == token.NoPos {
					var words []string
					for _, a := range n.Args {
						words = append(words, literal(a)) // "" for a value only known at run time
					}
					if err := fault(k, words); err != "" {
						t.Errorf("%s: d.run(%s): %s", path, strings.Join(words, ", "), err)
					}
					checked++
				}
			case *ast.BasicLit:
				s := literal(n)
				for _, m := range quoted.FindAllStringSubmatch(s, -1) {
					if err := fault(k, strings.Fields(m[1])); err != "" {
						t.Errorf("%s: `drive %s`: %s", path, m[1], err)
					}
					checked++
				}
				if strings.Contains(path, "internal/drive/") && bare.MatchString(quoted.ReplaceAllString(s, "")) {
					t.Errorf("%s: %q names a command outside d.run, which quotes it and names the batch", path, s)
				}
			}
			return true
		})
	}
	if checked < 50 {
		t.Fatalf("read only %d commands out of the messages; the source has more", checked)
	}
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

// literal is a string literal's value, or "" for any other expression.
func literal(e ast.Expr) string {
	if lit, ok := e.(*ast.BasicLit); ok && lit.Kind == token.STRING {
		s, _ := strconv.Unquote(lit.Value)
		return s
	}
	return ""
}

// fault says what is wrong with a command line's words under the grammar:
// a command that does not run, or a flag it does not take.
func fault(k *kong.Kong, words []string) string {
	node := k.Model.Node
	if len(words) >= 2 && words[0] == "--batch" {
		words = words[2:]
	}
	i := 0
	for ; i < len(words); i++ {
		var next *kong.Node
		for _, c := range node.Children {
			if c.Name == words[i] && !c.Hidden {
				next = c
			}
		}
		if next == nil {
			break
		}
		node = next
	}
	if node == k.Model.Node {
		return "no such command"
	}
	if len(node.Children) > 0 && node.DefaultCmd == nil {
		return node.Name + " needs one of its commands"
	}
	if node.DefaultCmd != nil {
		node = node.DefaultCmd
	}
	var flags []string
	for _, group := range node.AllFlags(true) {
		for _, f := range group {
			flags = append(flags, "--"+f.Name)
		}
	}
	for _, w := range words[i:] {
		if w == "--" {
			break
		}
		if name, _, _ := strings.Cut(w, "="); strings.HasPrefix(name, "--") && !slices.Contains(flags, name) {
			return node.Name + " takes no " + name
		}
	}
	return ""
}
