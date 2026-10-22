package session

import (
	"os"
	"testing"
	"time"
)

// A registration as Claude Code 2.1.308 writes it (ids and paths invented).
const registration = `{"pid":94469,"sessionId":"e484432e-0000-4000-8000-000000000001","cwd":"/Users/example/dev/drive","startedAt":1792682702245,"procStart":"Thu Oct 22 15:25:01 2026","version":"2.1.308","kind":"interactive","entrypoint":"cli","status":"waiting","waitingFor":"permission prompt","updatedAt":1792685100347,"statusUpdatedAt":1792685100347}`

func TestARegistrationSaysWhatTheSessionDoes(t *testing.T) {
	reg, err := ParseRegistration([]byte(registration))
	if err != nil {
		t.Fatal(err)
	}
	if reg.ID != "e484432e-0000-4000-8000-000000000001" || reg.Status != "waiting" || reg.WaitingFor != "permission prompt" ||
		!reg.StatusAt.Equal(time.UnixMilli(1792685100347)) {
		t.Fatalf("%+v", reg)
	}
}

// Claude Code records procStart as ps prints lstart in UTC; Started reads
// the same, so a live process matches its own registration and a reused pid
// does not.
func TestAProcessIsKnownByItsStart(t *testing.T) {
	started := Machine{}.Started(os.Getpid())
	if _, err := time.ParseInLocation(lstart, started, time.UTC); err != nil {
		t.Fatalf("ps lstart %q does not read as a start: %v", started, err)
	}
	if !sameStart(started, started) || !sameStart("Thu Oct  2 15:25:01 2026", "Thu Oct 2 15:25:01 2026") {
		t.Fatal("one process's start must match itself")
	}
	if sameStart("Thu Oct 22 15:25:01 2026", "Thu Oct 22 15:25:09 2026") {
		t.Fatal("another start is another process")
	}
	if !sameStart("1792682702245", "Thu Oct 22 15:25:09 2026") || !sameStart("", started) {
		t.Fatal("a start in a form drive cannot read is not taken for another process")
	}
}
