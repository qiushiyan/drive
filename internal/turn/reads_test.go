package turn

import (
	"strings"
	"testing"
)

// Replies shaped as real ones were, from sessions whose wakes were replayed:
// each a turn that ended with a background job still running. Getting a real
// question or report wrong swallows it, so those lead.
func TestReadReply(t *testing.T) {
	report := strings.Repeat("A paragraph of what the round found and how it was settled. ", 30)
	for _, tc := range []struct {
		name, reply string
		want        Reads
	}{
		// What must wake a driver, whatever still runs.
		{"a question", "The gate passes three of five readings. Should run 5 count as a pass?", ReadsAsking},
		{"a question with a wait after it", "Is the heads-up enough? Waiting on CI meanwhile.", ReadsAsking},
		{"a question early in a report that ends on a wait", "Do you know when it comes off?\n\n" + report + "\n\nWaiting on the checks.", ReadsAsking},
		{"a word of consent it needs", report + ` Say "fold these in" and I'll build the fixes on the branch.`, ReadsAsking},
		{"on the owner's word", "Each round is mostly follow-ups now. On your word I'll end the loop here.", ReadsAsking},
		{"what waits for the owner", report + "\n\n**Waiting on you:** the repair's --commit on production.", ReadsAsking},
		{"a decision that is theirs", "Round 2 is judged. The merge and the two open decisions are still yours.", ReadsAsking},
		{"once they say so", "The fix is ready. Once you say so I'll merge it.", ReadsAsking},
		{"a final report with a job left behind", report + "\n\nI posted progress on the card, which stays in progress. The scratch worktrees are removed.", ReadsEnded},
		{"a short report", "PR #41 is open and its description is written.", ReadsEnded},
		{"a report that only mentions a wait early", "I was waiting on the CI run earlier.\n\n" + report + "\n\nEverything is pushed. Nothing else is open.", ReadsEnded},
		{"a wait that ended in its last sentence", "CI was slow. Done waiting for it, and merged.", ReadsEnded},
		{"a wait before the last sentence", "Waiting on the replays. The report is written.", ReadsEnded},
		{"a decision in its imperative", "The fix is ready.\nApprove the migration to continue. Waiting on CI.", ReadsAsking},
		{"what it needs", "The fix is ready, and it needs your sign-off. CI is still running.", ReadsAsking},
		{"a wait for approval", "Waiting for approval before applying the production migration.", ReadsAsking},
		{"a wait for someone's decision", "The options are in the report. Waiting on the owner's decision.", ReadsAsking},
		{"a wait for a review bot is a job's", "Pushed. Waiting on CI and the reviewer for the new head.", ReadsWaiting},
		{"nothing to read", "", ReadsEnded},
		{"a notification it sent", "Merged and deployed. I sent the notification.", ReadsEnded},
		{"a wait that is over", "No longer waiting for CI. The change is merged and released.", ReadsEnded},
		{"an approval it asks for", "Please approve the production migration. Waiting on CI meanwhile.", ReadsAsking},
		{"a report that says what it did after a wait", "After waiting on CI I merged it. Everything is released.", ReadsEnded},
		{"a wait mentioned before its last sentence", "Waiting on CI is over. Merged and released.", ReadsEnded},
		{"a question mark inside code is no question", "Fixed the `?25h` left in the box. The rest passes.", ReadsEnded},

		// What need not wake a driver: its job's end brings the session back.
		{"plainly waiting", "Waiting on the Postgres suites and the app type-check.", ReadsWaiting},
		{"waiting at a report's end", report + "\n\nWaiting on CI and the reviewer for the new head.", ReadsWaiting},
		{"what comes when it ends", "The review is pushed. When the replays finish, I'll score them and report.", ReadsWaiting},
		{"after the job", "CI is re-running. Once it's green I'll squash-merge.", ReadsWaiting},
		{"a wake of its own", "A background wait will bring me back when CI finishes.", ReadsWaiting},
		{"a waiter", "The waiter is set for the 10x drive.", ReadsWaiting},
		{"still running", "Round 3 is decided. The watch on the new head is still running.", ReadsWaiting},
		{"a loop watching", "Fixed and pushed. The wait loop is watching the new head.", ReadsWaiting},
		{"the next notification", "All arms are running or done. I'll pick up on the next notification.", ReadsWaiting},
		{"a report that follows", "The final CI run is going. The report follows once it settles.", ReadsWaiting},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadReply(tc.reply); got != tc.want {
				t.Fatalf("read %d, want %d: %q", got, tc.want, tc.reply)
			}
		})
	}
}
