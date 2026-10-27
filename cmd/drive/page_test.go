package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alecthomas/kong"

	"github.com/qiushiyan/drive/internal/batch"
	"github.com/qiushiyan/drive/internal/config"
	"github.com/qiushiyan/drive/internal/drive"
	"github.com/qiushiyan/drive/internal/testkit"
)

// An install renames a new binary over the one a running page serves
// from. The page stops then, for its supervisor to start the new one,
// rather than serving the old code until the machine restarts.
func TestThePageStopsWhenItsBinaryIsReplaced(t *testing.T) {
	w := testkit.New(t)
	self := filepath.Join(t.TempDir(), "drive")
	write(t, self, "the old build")
	a := app
	a.Options = append(slices.Clone(app.Options), kong.Bind(wiring(func(cfg config.Config) *drive.Driver {
		return &drive.Driver{Term: w, Host: w, Store: batch.Store{Dir: cfg.StateDir}, Cfg: cfg, Self: self, Now: w.Clock.Now}
	})))
	defer func(was time.Duration) { replacedEvery = was }(replacedEvery)
	replacedEvery = 20 * time.Millisecond

	stopped := make(chan string)
	go func() {
		code, out, errs := cli(a, "page", "--listen", "127.0.0.1:0")
		stopped <- fmt.Sprint(code) + "\n" + out + errs
	}()
	time.Sleep(200 * time.Millisecond) // serving, and holding the binary it started from
	next := self + ".new"
	write(t, next, "the new build")
	if err := os.Rename(next, self); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-stopped:
		if !strings.HasPrefix(got, "0\n") || !strings.Contains(got, "was replaced") {
			t.Fatalf("it stops cleanly, saying why:\n%s", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the page still serves from a replaced binary")
	}
}
