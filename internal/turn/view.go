package turn

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
)

// View names a session and the point its conversation had reached when a
// driver read it: "T1@3fa91c07". An action carries the view it was decided
// on, and is refused once the session has moved past it, so a decision made
// on an old turn never lands on a new one.
type View struct {
	Session string // a label or block id
	Prompt  string // 4 hex: the session and its newest prompt
	Reply   string // 4 hex: its newest assistant entry
}

func short(parts ...string) string {
	h := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:2])
}

// ViewOf is the view of a frontier read from session sid.
func ViewOf(session, sid string, f Frontier) View {
	prompt, reply := "none", "none"
	if p := f.Newest(); p != nil {
		prompt = p.ID
	}
	if f.Last != nil {
		reply = f.Last.ID
	}
	return View{Session: session, Prompt: short(sid, prompt), Reply: short(sid, reply)}
}

func (v View) String() string { return v.Session + "@" + v.Prompt + v.Reply }

var viewForm = regexp.MustCompile(`^(.+)@([0-9a-f]{4})([0-9a-f]{4})$`)

// ParseView reads "T1@3fa91c07". A bare session name is an error that says
// where the view comes from.
func ParseView(s string) (View, error) {
	m := viewForm.FindStringSubmatch(s)
	if m == nil {
		if !strings.Contains(s, "@") {
			return View{}, fmt.Errorf("%q is a session, not a view: act on what you read — `drive read %s` prints the view to pass (%s@…)", s, s, s)
		}
		return View{}, fmt.Errorf("%q is not a view; a view looks like T1@3fa91c07, as `drive read` and `drive wait` print it", s)
	}
	return View{Session: m[1], Prompt: m[2], Reply: m[3]}, nil
}
