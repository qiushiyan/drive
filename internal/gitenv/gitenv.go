// Package gitenv is the environment drive starts git in.
package gitenv

import (
	"os"
	"slices"
	"strings"
)

// local is every variable by which git is told which repository it is in
// (`git rev-parse --local-env-vars`). Git exports them to what it runs, a
// hook or the command of `git rebase --exec`, and a git started under them
// acts on that repository whatever directory it is given.
var local = []string{
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_OBJECT_DIRECTORY", "GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_GRAFT_FILE",
	"GIT_INDEX_FILE", "GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX", "GIT_SHALLOW_FILE",
	"GIT_COMMON_DIR",
}

// Env is the process's environment without those variables, so git finds
// its repository from the directory it runs in.
func Env() []string {
	return slices.DeleteFunc(os.Environ(), func(kv string) bool {
		name, _, _ := strings.Cut(kv, "=")
		return slices.Contains(local, name)
	})
}
