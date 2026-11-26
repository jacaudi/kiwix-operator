// Package notify posts archive events to a user-configured webhook as {"title","message"} JSON.
package notify

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// maxDrain bounds how much of a response body is read so the connection can be reused.
const maxDrain = 64 << 10

// requestTimeout bounds each attempt; it is a variable only so a test can shorten it, and it is applied with a context so an injected HTTP client cannot remove it.
var requestTimeout = 10 * time.Second

// Notifier posts to a webhook. The zero value sends once with http.DefaultClient and no User-Agent.
type Notifier struct {
	// HTTP is the client to send with; nil means http.DefaultClient.
	HTTP *http.Client
	// Retries is how many times a failed send is repeated, so Send makes at most Retries+1 requests.
	Retries int
	// UserAgent is sent on every request.
	UserAgent string
	// Sleep waits between attempts; nil waits on a timer that ends early when the context is done.
	Sleep func(time.Duration)
}

type payload struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}

// Send posts the notification and returns nil on a 2xx. It retries after 1 s, 2 s, then 4 s for every later wait, and returns the last error when it gives up.
// The webhook URL may carry a secret, so errors name the host only.
func (n *Notifier) Send(ctx context.Context, webhookURL, title, message string) error {
	u, err := url.Parse(webhookURL)
	if err != nil {
		return errors.New("webhook URL is not a valid URL")
	}
	// Invalid UTF-8 in a reason is replaced rather than failing the notification.
	body, err := json.Marshal(payload{Title: title, Message: message}, jsontext.AllowInvalidUTF8(true))
	if err != nil {
		return fmt.Errorf("encode notification: %w", err)
	}

	var last error
	for attempt := 0; ; attempt++ {
		if last = n.post(ctx, webhookURL, u.Host, body); last == nil {
			return nil
		}
		if attempt >= n.Retries {
			return last
		}
		if err := n.wait(ctx, time.Second<<min(attempt, 2)); err != nil {
			return errors.Join(err, last)
		}
	}
}

// post makes one bounded request. Errors carry the host, never the URL, which net/http would otherwise include.
func (n *Notifier) post(ctx context.Context, webhookURL, host string, body []byte) error {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request for %s: %w", host, unwrapURL(err))
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", n.UserAgent)

	client := n.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("post to %s: %w", host, unwrapURL(err))
	}
	defer resp.Body.Close() //nolint:errcheck // read-only
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("webhook %s returned %s", host, resp.Status)
	}
	return nil
}

// wait sleeps for d and reports a done context, whichever comes first.
func (n *Notifier) wait(ctx context.Context, d time.Duration) error {
	if n.Sleep != nil {
		n.Sleep(d)
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// unwrapURL drops net/http's *url.Error wrapper, which embeds the full request URL, and keeps the cause.
func unwrapURL(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		return ue.Err
	}
	return err
}

// Updated is the notification for a promoted archive; first marks an archive that had no previous version.
func Updated(archive string, articles, prevArticles, sizeKiB int64, first bool) (title, message string) {
	was := "first version"
	if !first {
		was = "was " + formatInt(prevArticles)
	}
	return archive + " updated", fmt.Sprintf("%s pages, %d MB (%s)", formatInt(articles), sizeKiB/1024, was)
}

// Rejected is the notification for a refresh the health gate kept out; gateMessage is the gate's reason.
func Rejected(archive, gateMessage string) (title, message string) {
	return archive + " refresh rejected", gateMessage
}

// Failed is the notification for an archive whose attempt failed.
func Failed(archive, reason string) (title, message string) {
	return archive + " failed", reason
}

// AdoptMissing is the notification for an adoptFile that is not on the library volume.
func AdoptMissing(archive, file string) (title, message string) {
	return archive + " waiting for adoptFile", file + " not found on the library volume; remove adoptFile to fetch instead"
}

// formatInt renders n with comma thousands separators.
func formatInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}
