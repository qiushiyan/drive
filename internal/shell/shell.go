// Package shell writes the command lines drive prints for a driver, or a
// Rex block, to run as written: one rule for what a word needs quoting.
package shell

import "strings"

// plain are the characters no shell treats specially in a word.
const plain = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@._-/=:"

// Quote makes s one shell word: as it is when every character is plain,
// single-quoted otherwise.
func Quote(s string) string {
	if s != "" && strings.Trim(s, plain) == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Join quotes each word and joins them with spaces.
func Join(words ...string) string {
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = Quote(w)
	}
	return strings.Join(quoted, " ")
}
