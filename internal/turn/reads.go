package turn

import (
	"regexp"
	"slices"
	"strings"
)

// Reads is what a reply says of the turn it ended, while the session's own
// background jobs still run. Claude Code's registry reads alike for a session
// waiting on its job and for one that finished its report with an old job left
// behind, so only the reply tells them apart.
type Reads int

const (
	// ReadsEnded is a reply that neither asks nor says it waits: a report, or
	// anything unclear. It is the default, since a turn taken for waiting wakes
	// no one.
	ReadsEnded Reads = iota
	// ReadsAsking asks someone for something: a question, or a decision only
	// they can give.
	ReadsAsking
	// ReadsWaiting says, at its end, that it waits on what still runs.
	ReadsWaiting
)

var (
	codeSpans = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")
	// A question mark closing a sentence, outside code.
	questionMark = regexp.MustCompile(`\?["”')*_]*(\s|$)`)
	// Who a session leaves something to: the driver stands in for that person.
	askingWords = regexp.MustCompile(`(?i)\bsay\s+["“'*]|\bon your (word|go|ok)\b|\bwait(s|ing)?\s+(on|for)\s+(you|your)\b|\bawaiting\s+(you|your|approval|sign)` +
		`|\bneeds?\s+(you|your|approval|a decision|sign-?off)\b|\byour\s+(call|decision|answer|go-ahead|approval|ok)\b|\bfor you to (decide|choose|pick|answer|approve)\b` +
		`|\b(is|are)\s+(still\s+)?yours\b|\blet me know\b|\btell me\b|\b(once|when|after|if)\s+you\b|\byou('d| would) (like|prefer)\b|\bplease\b` +
		`|(^|[.!?:\n]\s*)(approve|confirm|decide|choose|sign off)\b` +
		`|\bwait(s|ing)?\s+(on|for|until)\b[^.\n]*\b(approval|sign-?off|decision|answer|go-ahead|confirmation|permission|input)\b`)
	// What a session says of a job it is waiting on, as the whole of how it ends.
	waitingWords = regexp.MustCompile(`(?i)\bwait(s|ing)?\s+(on|for|until)\b` +
		`|\b(still|now|is|are)\s+(\w+\s+)?(re-?)?(running|going|driving|compiling|replaying|in progress|queued|pending)\b` +
		`|\bwakes?\s+(me|this session)\b|\bbring(s)? me back\b|\bi'?m watching\b` +
		`|\b(wait|waiter|watch|watcher|monitor|poll)\w*\b[^.\n]*\b(armed|set|running|watching|covers|reports?)\b` +
		`|\b(once|when|as soon as)\b[^.\n]*\b(i'?ll|i will)\b|\b(i'?ll|i will)\b[^.\n]*\b(once|when|as soon as)\b` +
		`|\bfollows?\s+(once|when|after)\b|\bnext notification\b`)
	// What says a wait is over, or was never this one.
	notWaiting  = regexp.MustCompile(`(?i)\b(no longer|not|isn'?t|aren'?t|wasn'?t|weren'?t|never|stopped|finished|over|done|cancelled|canceled)\b`)
	sentenceEnd = regexp.MustCompile(`[.!?:]\s+|\n+`)
)

// askingTail bounds how far from its end a reply's asking words count, a
// report's history of who decided what being no ask. A question mark counts
// anywhere.
const askingTail = 1500

// ReadReply says what a reply says of its turn. It errs towards waking a
// driver: asking wins over waiting, and a reply reads as waiting only when its
// last sentence is that and nothing there says the wait is over. A wrong reading
// in that direction costs a run; the other way holds a question back until the
// job ends or the turn's backstop.
func ReadReply(reply string) Reads {
	prose := codeSpans.ReplaceAllString(reply, " ")
	end := prose
	if len(end) > askingTail {
		end = end[len(end)-askingTail:]
	}
	if questionMark.MatchString(prose) || askingWords.MatchString(end) {
		return ReadsAsking
	}
	if last := lastSentence(prose); waitingWords.MatchString(last) && !notWaiting.MatchString(last) {
		return ReadsWaiting
	}
	return ReadsEnded
}

// lastSentence is the last sentence with words in it.
func lastSentence(s string) string {
	parts := sentenceEnd.Split(strings.TrimSpace(s), -1)
	for _, part := range slices.Backward(parts) {
		if strings.TrimSpace(part) != "" {
			return part
		}
	}
	return ""
}
