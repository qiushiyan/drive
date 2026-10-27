package drive

import (
	"fmt"
	"strings"

	"github.com/qiushiyan/drive/internal/session"
	"github.com/qiushiyan/drive/internal/shell"
)

// text is free text in a command line, such as a step's line or a park's
// reason, which a driver's agent wrote and which may start with anything.
type text string

// run is a drive command line a message gives the driver to run as written.
// Each word is one shell word; the batch is named when the action ran in
// one that is not current, since a wake or a --batch refusal is read where
// the current batch may be another; and free text goes after every flag,
// behind -- when it starts with a dash, which drive would read as a flag.
// words are strings (commands, flags and their values) and texts.
func (d *Driver) run(words ...any) string {
	line := "drive "
	if d.BatchName != "" {
		if cur, err := d.Store.Current(); err != nil || cur.Meta.Name != d.BatchName {
			line += "--batch " + shell.Quote(d.BatchName) + " "
		}
	}
	return line + join(words...)
}

// join is the words of a command line, as run orders and quotes them.
func join(words ...any) string {
	var fixed, free []string
	dash := false
	for _, w := range words {
		switch w := w.(type) {
		case string:
			fixed = append(fixed, w)
		case text:
			if w != "" {
				free = append(free, string(w))
				dash = dash || strings.HasPrefix(string(w), "-")
			}
		default:
			panic(fmt.Sprintf("a command word is a string or a text, not %T", w))
		}
	}
	if dash {
		fixed = append(fixed, "--")
	}
	return shell.Join(append(fixed, free...)...)
}

// stepWords is a step as drive send and drive open take it.
func stepWords(s Step) []any {
	switch {
	case s.Key != "":
		return []any{"--snippet", s.Key, text(s.Text)}
	case s.File != "":
		return []any{"--file", s.File}
	}
	return []any{text(s.Line)}
}

// sendCommand is the drive send that delivers step on view with opt.
func (d *Driver) sendCommand(view string, step Step, opt SendOptions) string {
	words := append([]any{"send", view}, stepWords(step)...)
	if opt.Queue {
		words = append(words, "--queue")
	}
	if opt.Hold != "" {
		words = append(words, "--hold", opt.Hold)
	}
	if opt.Label != "" {
		words = append(words, "--as", opt.Label)
	}
	return d.run(words...)
}

// openCommand is the drive open that opens label with opt and step.
func (d *Driver) openCommand(label string, opt OpenOptions, step Step) string {
	words := []any{"open", label}
	for _, f := range [][2]string{{"--dir", opt.Dir}, {"--resume", opt.Resume}, {"--account", opt.Account}, {"--model", opt.Model}, {"--hold", opt.Hold}} {
		if f[1] != "" {
			words = append(words, f[0], f[1])
		}
	}
	if opt.Goal != nil {
		words = append(words, "--goal="+*opt.Goal) // one word, so a goal starting with a dash stays the flag's value
	}
	return d.run(append(words, stepWords(step)...)...)
}

// stopJob is the step that has a session stop a background job of its own:
// the session knows its jobs and can end them, where a driver's shell would
// be reaching into a session it only reads and sends to.
func (d *Driver) stopJob(view string, job session.Shell) string {
	return d.run("send", view, text(fmt.Sprintf("Stop your background job (%s): it should have ended.", clip(job.Command, 120))))
}
