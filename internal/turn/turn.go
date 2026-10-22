// Package turn reads where a Claude Code session's turn stands from its
// transcript: the prompts it took, one still queued, its newest reply, an API
// error that ended the turn, and a tool call it waits on. It is pure: a path or
// bytes in, facts out.
package turn

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

// Prompt is one prompt the session received.
type Prompt struct {
	ID     string
	At     time.Time
	Text   string
	Known  bool // false for a slash command typed while busy: Claude Code queues it without its text
	Queued bool // typed while the session worked and not taken yet
}

// Entry is one assistant entry.
type Entry struct {
	ID   string
	At   time.Time
	Text string
}

// APIError is an API failure that ended the turn after the newest prompt.
type APIError struct {
	At   time.Time
	Kind string // "authentication_failed", "rate_limit", …
	Text string // what the session showed, such as "Login expired · Please run /login"
}

// ToolCall is a tool call the session has made and not received a result for.
type ToolCall struct {
	ID    string
	Name  string
	Input string // the command for Bash, else the input as JSON
}

// Frontier is where the session's conversation stands.
type Frontier struct {
	Prompts     []Prompt  // taken, oldest first
	Queued      *Prompt   // typed while busy, not yet taken
	Reply       *Entry    // newest assistant text
	Last        *Entry    // newest answer of any kind: assistant text, tool call, API error, a local command's output
	Error       *APIError // an API error after the newest prompt
	Pending     *ToolCall
	Interrupted bool // the newest prompt's turn was interrupted, a denied prompt included, and nothing followed
}

// Newest is the newest prompt the session received, queued or taken.
func (f Frontier) Newest() *Prompt {
	if f.Queued != nil {
		return f.Queued
	}
	if n := len(f.Prompts); n > 0 {
		return &f.Prompts[n-1]
	}
	return nil
}

// Answered says whether the newest prompt's turn is over: an answer came
// after it, or it was interrupted.
func (f Frontier) Answered() bool {
	p := f.Newest()
	return p == nil || (!p.Queued && (f.Interrupted || f.Last != nil && !f.Last.At.Before(p.At)))
}

var (
	commandName = regexp.MustCompile(`<command-name>(/[^<]*)</command-name>`)
	commandArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// Prompts Claude Code writes under the user role that nobody typed.
var notTyped = []string{"<local-command-stdout>", "<local-command-stderr>", "<local-command-caveat>",
	"<task-notification>", "<system-reminder>"}

// AsTyped gives a slash command recorded as markup back as it was typed.
func AsTyped(text string) string {
	name := commandName.FindStringSubmatch(text)
	if name == nil {
		return text
	}
	args := ""
	if a := commandArgs.FindStringSubmatch(text); a != nil {
		args = strings.TrimSpace(a[1])
	}
	return strings.TrimSpace(name[1] + " " + args)
}

type row struct {
	Type             string                 `json:"type"`
	Subtype          string                 `json:"subtype"`
	UUID             string                 `json:"uuid"`
	Timestamp        string                 `json:"timestamp"`
	IsMeta           bool                   `json:"isMeta"`
	IsCompactSummary bool                   `json:"isCompactSummary"`
	IsAPIError       bool                   `json:"isApiErrorMessage"`
	Error            string                 `json:"error"`
	Origin           *struct{ Kind string } `json:"origin"`
	Message          *struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Operation  string          `json:"operation"`
	Content    json.RawMessage `json:"content"`
	Attachment json.RawMessage `json:"attachment"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

func (r row) at() time.Time {
	t, _ := time.Parse(time.RFC3339Nano, r.Timestamp)
	return t
}

// text is a queue operation's or a system row's own content; ok is false
// when it has none.
func (r row) text() (s string, ok bool) {
	if len(r.Content) == 0 || string(r.Content) == "null" {
		return "", false
	}
	return s, json.Unmarshal(r.Content, &s) == nil
}

func (r row) human() bool { return r.Origin == nil || r.Origin.Kind == "" || r.Origin.Kind == "human" }

func (r row) blocks() []block {
	if r.Message == nil || len(r.Message.Content) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(r.Message.Content, &s) == nil {
		return []block{{Type: "text", Text: s}}
	}
	var bs []block
	_ = json.Unmarshal(r.Message.Content, &bs)
	return bs
}

func typed(text string) bool {
	t := strings.TrimLeft(text, " \t\n")
	for _, p := range notTyped {
		if strings.HasPrefix(t, p) {
			return false
		}
	}
	return text != ""
}

// ReadFile reads a transcript; a missing file is a session that has taken no prompt yet.
func ReadFile(path string) (Frontier, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return Frontier{}, nil
	}
	if err != nil {
		return Frontier{}, err
	}
	defer f.Close()
	return Read(f)
}

// Read reads a transcript stream.
func Read(r io.Reader) (Frontier, error) {
	var f Frontier
	pending := map[string]ToolCall{}
	var order []string
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e row
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		switch e.Type {
		case "assistant":
			var texts []string
			for _, b := range e.blocks() {
				switch b.Type {
				case "text":
					if strings.TrimSpace(b.Text) != "" {
						texts = append(texts, b.Text)
					}
				case "tool_use":
					pending[b.ID] = ToolCall{ID: b.ID, Name: b.Name, Input: toolInput(b.Input)}
					order = append(order, b.ID)
				}
			}
			entry := &Entry{ID: e.UUID, At: e.at(), Text: strings.Join(texts, "\n")}
			// Whatever ended the turn before, the session has moved on since.
			f.Last, f.Interrupted = entry, false
			if e.IsAPIError {
				kind := e.Error
				if kind == "" {
					kind = "api_error"
				}
				f.Error = &APIError{At: e.at(), Kind: kind, Text: entry.Text}
				continue
			}
			f.Error = nil
			if len(texts) > 0 {
				f.Reply = entry
			}
		case "user":
			var texts []string
			for _, b := range e.blocks() {
				switch b.Type {
				case "tool_result":
					delete(pending, b.ToolUseID)
				case "text":
					texts = append(texts, b.Text)
				}
			}
			text := strings.Join(texts, "\n")
			if strings.HasPrefix(text, "[Request interrupted by user") {
				f.Interrupted = true
				continue
			}
			if strings.HasPrefix(strings.TrimLeft(text, " \t\n"), "<local-command-stdout>") {
				// A local command such as /compact answers with its output, not an assistant entry.
				f.Last = &Entry{ID: e.UUID, At: e.at(), Text: text}
				continue
			}
			if e.human() && !e.IsMeta && !e.IsCompactSummary && typed(text) {
				f.take(Prompt{ID: e.UUID, At: e.at(), Text: AsTyped(text), Known: true})
			}
		case "system":
			text, _ := e.text()
			if e.Subtype == "local_command" {
				// Some local commands record themselves and their output as
				// system rows: /clear's output, a refused /compact's, and /cd
				// or /wt whole.
				if strings.HasPrefix(strings.TrimLeft(text, " \t\n"), "<command-name>") {
					f.take(Prompt{ID: e.UUID, At: e.at(), Text: AsTyped(text), Known: true})
				} else {
					f.Last = &Entry{ID: e.UUID, At: e.at(), Text: text}
				}
			}
		case "queue-operation":
			content, ok := e.text()
			switch e.Operation {
			case "enqueue":
				if !ok {
					// A slash command typed while busy is enqueued without its text.
					f.Queued = &Prompt{ID: "queued:" + e.Timestamp, At: e.at(), Queued: true}
				} else if typed(content) {
					f.Queued = &Prompt{ID: "queued:" + e.Timestamp, At: e.at(), Text: content, Known: true, Queued: true}
				}
			case "dequeue":
				f.Queued = nil
			case "remove":
				if f.Queued != nil && ok && f.Queued.Known && f.Queued.Text == content {
					f.Queued = nil
				}
			}
		case "attachment":
			var a struct {
				Type   string                 `json:"type"`
				Prompt string                 `json:"prompt"`
				Origin *struct{ Kind string } `json:"origin"`
			}
			if json.Unmarshal(e.Attachment, &a) == nil && a.Type == "queued_command" &&
				(a.Origin == nil || a.Origin.Kind == "" || a.Origin.Kind == "human") && typed(a.Prompt) {
				f.take(Prompt{ID: e.UUID, At: e.at(), Text: a.Prompt, Known: true})
			}
		}
	}
	for i := len(order) - 1; i >= 0; i-- {
		if c, ok := pending[order[i]]; ok {
			f.Pending = &c
			break
		}
	}
	return f, sc.Err()
}

func (f *Frontier) take(p Prompt) {
	// A typed /compact is recorded as text, then as markup once it ends: one prompt.
	if n := len(f.Prompts); n > 0 && f.Prompts[n-1].Text == p.Text {
		f.Prompts[n-1] = p
	} else {
		f.Prompts = append(f.Prompts, p)
	}
	f.Queued, f.Error, f.Interrupted = nil, nil, false
}

func toolInput(raw json.RawMessage) string {
	var bash struct {
		Command string `json:"command"`
	}
	if json.Unmarshal(raw, &bash) == nil && bash.Command != "" {
		return bash.Command
	}
	var pretty bytes.Buffer
	if json.Indent(&pretty, raw, "", " ") == nil {
		return pretty.String()
	}
	return string(raw)
}
