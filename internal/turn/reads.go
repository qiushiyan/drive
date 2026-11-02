package turn

import (
	"regexp"
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
	askingWords = regexp.MustCompile(`(?i)\bsay\s+["“'*]|\bon your (word|go|ok)\b|\bwait(s|ing)?\s+(on|for)\s+(you|your)\b|\bneeds?\s+you\b` +
		`|\byour\s+(call|decision|answer|go-ahead|approval|ok)\b|\bfor you to (decide|choose|pick|answer|approve)\b|\b(is|are)\s+(still\s+)?yours\b` +
		`|\blet me know\b|\btell me\b|\b(once|when|after|if)\s+you\b|\byou('d| would) (like|prefer)\b`)
	// What a session says of a job it is waiting on.
	waitingWords = regexp.MustCompile(`(?i)\bwait(s|ing)?\s+(on|for|until)\b` +
		`|\b(still|now|is|are)\s+(\w+\s+)?(re-?)?(running|going|driving|compiling|replaying|in progress|queued|pending)\b` +
		`|\bwakes?\s+(me|this session)\b|\bbring(s)? me back\b|\bi'?m watching\b` +
		`|\b(wait|waiter|watch|watcher|monitor|poll)\w*\b[^.\n]*\b(armed|set|running|watching|covers|reports?)\b` +
		`|\b(once|when|after|as soon as)\b[^.\n]*\b(i'?ll|i will)\b|\b(i'?ll|i will)\b[^.\n]*\b(once|when|after|as soon as)\b` +
		`|\bfollows?\s+(once|when|after)\b|\bnotification\b`)
	sentenceEnd = regexp.MustCompile(`[.!?:]\s+|\n+`)
)

// waitingTail is how many of a reply's last sentences say whether it waits:
// a long report that mentions a wait along the way has not ended on one.
const waitingTail = 3

// askingTail bounds how far from its end a reply's asking words count, a
// report's history of who decided what being no ask. A question mark counts
// anywhere.
const askingTail = 1500

// ReadReply says what a reply says of its turn. It errs towards waking a
// driver: asking wins over waiting, and only a reply that says it waits reads
// as waiting. A wrong reading in that direction costs a run; the other way
// costs a question nobody hears.
func ReadReply(reply string) Reads {
	prose := codeSpans.ReplaceAllString(reply, " ")
	end := prose
	if len(end) > askingTail {
		end = end[len(end)-askingTail:]
	}
	if questionMark.MatchString(prose) || askingWords.MatchString(end) {
		return ReadsAsking
	}
	if waitingWords.MatchString(lastSentences(prose, waitingTail)) {
		return ReadsWaiting
	}
	return ReadsEnded
}

func lastSentences(s string, n int) string {
	var kept []string
	for _, p := range sentenceEnd.Split(strings.TrimSpace(s), -1) {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) > n {
		kept = kept[len(kept)-n:]
	}
	return strings.Join(kept, ". ")
}
