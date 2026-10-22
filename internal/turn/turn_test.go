package turn

import (
	"strings"
	"testing"
)

func read(t *testing.T, rows ...string) Frontier {
	t.Helper()
	f, err := Read(strings.NewReader(strings.Join(rows, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

const (
	prompt  = `{"type":"user","uuid":"p1","timestamp":"2026-10-21T09:43:30Z","message":{"role":"user","content":"Read /x/T4-brief.md and do what it asks."}}`
	expired = `{"type":"assistant","uuid":"a1","timestamp":"2026-10-21T09:43:37Z","isApiErrorMessage":true,"error":"authentication_failed","message":{"role":"assistant","content":[{"type":"text","text":"Login expired · Please run /login"}]}}`
	reply   = `{"type":"assistant","uuid":"a2","timestamp":"2026-10-21T10:00:00Z","message":{"role":"assistant","content":[{"type":"text","text":"Design ready."}]}}`
)

func TestLoginExpiredIsAnErrorAfterThePrompt(t *testing.T) {
	f := read(t, prompt, expired)
	if f.Error == nil || f.Error.Kind != "authentication_failed" || f.Error.Text != "Login expired · Please run /login" {
		t.Fatalf("error: %+v", f.Error)
	}
	if f.Reply != nil {
		t.Fatalf("an API error is not a reply: %+v", f.Reply)
	}
	// A later prompt clears it: a recovered session is not poisoned by its history.
	f = read(t, prompt, expired, `{"type":"user","uuid":"p2","timestamp":"2026-10-21T10:20:00Z","message":{"role":"user","content":"again"}}`)
	if f.Error != nil {
		t.Fatalf("an error before the newest prompt is history: %+v", f.Error)
	}
}

// A slash command typed while busy: enqueue with no content, dequeue, then
// the command markup (seen in a real transcript eight minutes apart).
func TestQueuedSlashCommandHasNoText(t *testing.T) {
	enqueue := `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-21T12:32:15Z"}`
	f := read(t, prompt, enqueue)
	if f.Queued == nil || f.Queued.Known || !f.Queued.Queued {
		t.Fatalf("queued without text: %+v", f.Queued)
	}
	if f.Newest() != f.Queued || f.Answered() {
		t.Fatal("the queued command is the newest prompt and is not answered")
	}
	taken := `{"type":"user","uuid":"p3","timestamp":"2026-10-21T12:40:30Z","message":{"role":"user","content":"<command-message>write-spec</command-message>\n<command-name>/write-spec</command-name>\n<command-args>Write the answers in.</command-args>"}}`
	f = read(t, prompt, enqueue, `{"type":"queue-operation","operation":"dequeue","timestamp":"2026-10-21T12:40:30Z"}`, taken)
	if f.Queued != nil || f.Newest().Text != "/write-spec Write the answers in." {
		t.Fatalf("taken as typed: %+v %+v", f.Queued, f.Newest())
	}
}

func TestTaskNotificationsAreNotPrompts(t *testing.T) {
	note := `{"type":"queue-operation","operation":"enqueue","timestamp":"2026-10-21T12:33:28Z","content":"<task-notification>\n<task-id>b7</task-id>"}`
	harness := `{"type":"user","uuid":"h","timestamp":"2026-10-21T12:34:00Z","origin":{"kind":"task-notification"},"message":{"role":"user","content":"done"}}`
	f := read(t, prompt, note, harness)
	if f.Queued != nil || len(f.Prompts) != 1 {
		t.Fatalf("only the typed prompt counts: %+v %+v", f.Queued, f.Prompts)
	}
}

func TestPendingToolCallAndAnswered(t *testing.T) {
	call := `{"type":"assistant","uuid":"a3","timestamp":"2026-10-21T10:01:00Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"rm -rf x"}}]}}`
	f := read(t, prompt, reply, call)
	if f.Pending == nil || f.Pending.Input != "rm -rf x" || !f.Answered() {
		t.Fatalf("pending: %+v", f.Pending)
	}
	result := `{"type":"user","uuid":"r1","timestamp":"2026-10-21T10:01:05Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`
	if f = read(t, prompt, reply, call, result); f.Pending != nil {
		t.Fatalf("answered call: %+v", f.Pending)
	}
}

func TestCompactIsOnePrompt(t *testing.T) {
	typed := `{"type":"user","uuid":"c1","timestamp":"2026-10-21T11:00:00Z","message":{"role":"user","content":"/compact"}}`
	marked := `{"type":"user","uuid":"c2","timestamp":"2026-10-21T11:05:00Z","message":{"role":"user","content":"<command-name>/compact</command-name>"}}`
	if f := read(t, prompt, typed, marked); len(f.Prompts) != 2 || f.Newest().ID != "c2" {
		t.Fatalf("prompts: %+v", f.Prompts)
	}
}

func TestViewMovesWithTheConversation(t *testing.T) {
	a := ViewOf("T1", "sid", read(t, prompt))
	b := ViewOf("T1", "sid", read(t, prompt, reply))
	if a.Prompt != b.Prompt || a.Reply == b.Reply {
		t.Fatalf("a reply moves only the reply half: %s %s", a, b)
	}
	if other := ViewOf("T1", "sid2", read(t, prompt)); other.Prompt == a.Prompt {
		t.Fatal("another session's view differs")
	}
	if _, err := ParseView("T1"); err == nil || !strings.Contains(err.Error(), "drive read T1") {
		t.Fatalf("a bare name says where a view comes from: %v", err)
	}
}

func TestInputBox(t *testing.T) {
	cases := map[string]InputKind{
		"❯ \x1b[0m\x1b[2mRead /x/T4-strat-3.md and…\x1b[0m\r": InputSuggestion,
		"❯ Go heavy, take your recommendations\r":             InputTyped,
		"❯ \r": InputEmpty,
		"❯ \x1b[0m\x1b[38;2;136;136;136m─────\x1b[0m\r": InputEmpty,
		"no prompt here": InputUnknown,
	}
	for screen, want := range cases {
		if got, _ := InputBox("header\n" + screen + "\nfooter"); got != want {
			t.Errorf("%q: got %s, want %s", screen, got, want)
		}
	}
}

func TestATurnEndsWithoutAnAssistantEntry(t *testing.T) {
	rows := []string{
		`{"type":"user","uuid":"p1","timestamp":"2026-10-21T17:20:14Z","message":{"role":"user","content":"<command-name>/compact</command-name>\n<command-args></command-args>"}}`,
		`{"type":"user","uuid":"o1","timestamp":"2026-10-21T17:22:01Z","message":{"role":"user","content":"<local-command-stdout>Compacted </local-command-stdout>"}}`,
	}
	f, _ := Read(strings.NewReader(strings.Join(rows, "\n")))
	if !f.Answered() || len(f.Prompts) != 1 {
		t.Fatalf("a local command's output answers it: %+v", f)
	}
	rows = []string{
		`{"type":"user","uuid":"p2","timestamp":"2026-10-21T19:50:00Z","message":{"role":"user","content":"go"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-10-21T19:57:10Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"rm -rf x"}}]}}`,
		`{"type":"user","uuid":"r1","timestamp":"2026-10-21T19:57:16.918Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"The user doesn't want to proceed with this tool use."}]}}`,
		`{"type":"user","uuid":"i1","timestamp":"2026-10-21T19:57:16.922Z","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user for tool use]"}]}}`,
	}
	f, _ = Read(strings.NewReader(strings.Join(rows, "\n")))
	if !f.Interrupted || f.Pending != nil || !f.Answered() {
		t.Fatalf("a denied call ends the turn: %+v", f)
	}
	rows = append(rows,
		`{"type":"user","uuid":"n1","timestamp":"2026-10-21T19:58:00Z","message":{"role":"user","content":"<task-notification>done</task-notification>"}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-10-21T19:58:05Z","message":{"role":"assistant","content":[{"type":"text","text":"the build finished"}]}}`)
	f, _ = Read(strings.NewReader(strings.Join(rows, "\n")))
	if f.Interrupted {
		t.Fatal("an answer after the interrupt means the session moved on")
	}
}

// Rows cut from Claude Code 2.1.308 transcripts on this machine (a probe
// session, and a /cd in a working one), ids shortened, paths invented.
const pong = `{"type":"user","uuid":"p0","timestamp":"2026-10-22T15:41:46.248Z","origin":{"kind":"human"},"promptSource":"typed","message":{"role":"user","content":"Reply with the single word: pong"}}
{"type":"assistant","uuid":"a0","timestamp":"2026-10-22T15:41:47.305Z","message":{"role":"assistant","content":[{"type":"text","text":"pong"}]}}
{"type":"system","subtype":"stop_hook_summary","uuid":"s0","timestamp":"2026-10-22T15:41:47.369Z"}
{"type":"system","subtype":"turn_duration","durationMs":1137,"messageCount":16,"uuid":"d0","timestamp":"2026-10-22T15:41:47.370Z","isMeta":false}`

// Some local commands answer with a system row, not a user row: a /compact
// with nothing to compact, /clear (the first rows of the new session's
// transcript), and /cd or /wt, which record the command itself as one too.
func TestALocalCommandAnswersWithASystemRow(t *testing.T) {
	refused := read(t, pong,
		`{"type":"user","message":{"role":"user","content":"/compact"},"uuid":"c1","timestamp":"2026-10-22T15:43:14.824Z"}`,
		`{"type":"user","message":{"role":"user","content":"<local-command-caveat>The command below was run directly in Claude Code, not sent to you as a request, and its output goes straight to the user. It's recorded here as context for later messages.</local-command-caveat>"},"isMeta":true,"uuid":"c2","timestamp":"2026-10-22T15:43:14.867Z"}`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/compact</command-name>\n            <command-message>compact</command-message>\n            <command-args></command-args>"},"uuid":"c3","timestamp":"2026-10-22T15:43:14.840Z"}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>Not enough messages to compact.</local-command-stdout>","level":"info","timestamp":"2026-10-22T15:43:14.866Z","uuid":"c4","isMeta":false,"commandRun":{"command":"compact","args":""}}`)
	if p := refused.Newest(); p == nil || p.ID != "c3" || !refused.Answered() {
		t.Fatalf("the refused /compact is one prompt, answered: newest %+v last %+v", p, refused.Last)
	}
	cleared := read(t,
		`{"type":"mode","mode":"normal"}`,
		`{"type":"user","message":{"role":"user","content":"<local-command-caveat>The command below was run directly in Claude Code, not sent to you as a request, and its output goes straight to the user. It's recorded here as context for later messages.</local-command-caveat>"},"isMeta":true,"uuid":"k1","timestamp":"2026-10-22T15:45:12.114Z"}`,
		`{"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n            <command-message>clear</command-message>\n            <command-args></command-args>"},"uuid":"k2","timestamp":"2026-10-22T15:45:12.071Z"}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout></local-command-stdout>","level":"info","timestamp":"2026-10-22T15:45:12.113Z","uuid":"k3","isMeta":false,"commandRun":{"command":"clear","args":""}}`)
	if p := cleared.Newest(); len(cleared.Prompts) != 1 || p.Text != "/clear" || !cleared.Answered() {
		t.Fatalf("/clear is the new session's one prompt, answered: %+v last %+v", cleared.Prompts, cleared.Last)
	}
	moved := read(t, pong,
		`{"type":"system","subtype":"local_command","content":"<command-name>/cd</command-name>\n            <command-message>cd</command-message>\n            <command-args>/x/drive/</command-args>","level":"info","timestamp":"2026-10-22T15:46:02.822Z","uuid":"m1","isMeta":false}`,
		`{"type":"system","subtype":"local_command","content":"<local-command-stdout>Moved to \u001b[1m/x/drive\u001b[22m</local-command-stdout>","level":"info","timestamp":"2026-10-22T15:46:02.822Z","uuid":"m2","isMeta":false,"commandRun":{"command":"cd","args":"/x/drive/"}}`,
		`{"type":"user","message":{"role":"user","content":"<system-reminder>\nThe session's working directory has changed to /x/drive (via /cd)."},"isMeta":true,"uuid":"m3","timestamp":"2026-10-22T15:46:02.822Z"}`)
	if p := moved.Newest(); p == nil || p.Text != "/cd /x/drive/" || !moved.Answered() {
		t.Fatalf("a /cd recorded as system rows is a prompt, answered: newest %+v last %+v", p, moved.Last)
	}
}
