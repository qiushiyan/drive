// Package webhook wakes a chat driver: it posts what happened to the
// routine that starts the driver's next run.
package webhook

import (
	"bytes"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
)

// Attempt is one post and how it went.
type Attempt struct {
	Try  int
	HTTP int // 0 when the request never got an answer
	Err  string
}

// Poster posts a body and reports each attempt.
type Poster struct {
	URL    string
	Auth   string // the Authorization header's value
	Client *http.Client
	Tries  int
	Sleep  func(time.Duration)
}

// Post sends body until a 2xx answer or the tries run out. A failed answer
// such as a 400 "rate limit exceeded" is often transient, so it backs off
// from 15 s to about 8 minutes. Each attempt is passed to seen as it ends.
func (p Poster) Post(body []byte, seen func(Attempt)) error {
	if p.URL == "" {
		return fmt.Errorf("no webhook configured")
	}
	client, tries, sleep := p.Client, p.Tries, p.Sleep
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second}
	}
	if tries == 0 {
		tries = 6
	}
	if sleep == nil {
		sleep = time.Sleep
	}
	var last Attempt
	for i := 1; i <= tries; i++ {
		last = Attempt{Try: i}
		req, err := http.NewRequest(http.MethodPost, p.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if p.Auth != "" {
			req.Header.Set("Authorization", p.Auth)
		}
		resp, err := client.Do(req)
		if err != nil {
			last.Err = err.Error()
		} else {
			last.HTTP = resp.StatusCode
			msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
			resp.Body.Close()
			if resp.StatusCode/100 != 2 {
				last.Err = strings.TrimSpace(string(msg))
			}
		}
		if seen != nil {
			seen(last)
		}
		if last.HTTP/100 == 2 {
			return nil
		}
		if i < tries {
			sleep(time.Duration(15<<(i-1))*time.Second + time.Duration(rand.IntN(10))*time.Second)
		}
	}
	return fmt.Errorf("webhook post failed after %d tries: http %d %s", tries, last.HTTP, last.Err)
}
