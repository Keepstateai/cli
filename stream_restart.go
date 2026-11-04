// stream_restart.go: a live view outlasts a restart of the service.
//
// When the control plane is replaced (a deploy), it tells every open event
// stream so before it goes: an end frame {reason: service_restarting,
// reconnect: true, after_seq, retry_after_ms}. That is not the end of the
// conversation. The view waits as long as it was told, reconnects from the
// last event it showed (so nothing is shown twice and nothing is missed),
// and keeps trying while the new instance starts: a refused connection, a
// 502, or a 503 from the instance that is still draining. Only when the
// restart lasts longer than restartQuiet does it give up and say the
// connection was lost, with the command that reconnects.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// restartQuiet is how long a restart of the service is waited out.
var restartQuiet = 60 * time.Second

// restartBackoff is the first wait after a reconnect that failed; it doubles
// to restartBackoffMax.
var (
	restartBackoff    = time.Second
	restartBackoffMax = 5 * time.Second
)

// restartSignal is the service's word that it restarts and when to come back.
type restartSignal struct {
	reason string
	wait   time.Duration
}

// restartOf reads an end frame: the restart it announces, or nil for an end
// that is an end.
func restartOf(data []byte) *restartSignal {
	var e struct {
		Reason       string `json:"reason"`
		Reconnect    bool   `json:"reconnect"`
		RetryAfterMs int64  `json:"retry_after_ms"`
	}
	if json.Unmarshal(data, &e) != nil || !e.Reconnect {
		return nil
	}
	wait := time.Duration(e.RetryAfterMs) * time.Millisecond
	if wait <= 0 {
		wait = time.Second
	}
	if wait > 10*time.Second {
		wait = 10 * time.Second
	}
	return &restartSignal{reason: e.Reason, wait: wait}
}

// streamStatusError is a refused stream, with its status, so a restart can
// tell the draining instance's 503 from a refusal that will not change.
type streamStatusError struct {
	status int
	err    error
}

func (e *streamStatusError) Error() string { return e.err.Error() }

// retryableDuringRestart: what the gap between two instances looks like.
func retryableDuringRestart(err error) bool {
	var se *streamStatusError
	if errors.As(err, &se) {
		return se.status >= 500 || se.status == http.StatusRequestTimeout || se.status == http.StatusTooManyRequests
	}
	return true // the connection itself failed: nothing is listening yet
}

// cursorJSON is the data of the reconnecting and reconnected frames.
func cursorJSON(cursor int64, reason string) []byte {
	b, _ := json.Marshal(map[string]any{"after_seq": cursor, "reason": reason})
	return b
}

// followRestarts runs one connection after another: once normally, and again
// after each restart the service announces, from the cursor. It hands the
// caller two frames of its own around a restart: "reconnecting" (the
// service said it restarts) and "reconnected" (frames arrive again).
func followRestarts(ctx context.Context, cursor *int64, once func(reconnecting bool) (*restartSignal, bool, error), onFrame func(kind string, data []byte) bool) error {
	var since time.Time
	reason := ""
	backoff := restartBackoff
	sleep := func(d time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(d):
			return true
		}
	}
	for {
		sig, finished, err := once(!since.IsZero())
		switch {
		case finished:
			return err
		case sig != nil:
			if since.IsZero() {
				since, reason = time.Now(), sig.reason
				if onFrame("reconnecting", cursorJSON(*cursor, reason)) {
					return nil
				}
			}
			backoff = restartBackoff
			if !sleep(sig.wait) {
				return ctx.Err()
			}
		case err != nil:
			if since.IsZero() || !retryableDuringRestart(err) {
				return err
			}
			if time.Since(since) > restartQuiet {
				return fmt.Errorf("the live connection was lost: the service began restarting %s ago and has not come back (%v); run the command again to reconnect", time.Since(since).Round(time.Second), err)
			}
			if !sleep(backoff) {
				return ctx.Err()
			}
			if backoff *= 2; backoff > restartBackoffMax {
				backoff = restartBackoffMax
			}
		default:
			return nil // the stream closed on its own terms
		}
	}
}

// eventStreamOnce reads one connection of the session's event stream from
// the cursor. It moves the cursor with each event and shows an event once,
// hands every other frame to onFrame, and answers a restart the service
// announced, or whether the caller finished, or why the connection failed.
func eventStreamOnce(cr hostedCreds, sessionID string, cursor *int64, withStatus, reconnecting bool, onFrame func(kind string, data []byte) bool) (*restartSignal, bool, error) {
	q := url.Values{"after_seq": {strconv.FormatInt(*cursor, 10)}}
	if withStatus {
		q.Set("status", "1")
	}
	path := "/api/v2/sessions/" + url.PathEscape(sessionID) + "/events/stream?" + q.Encode()
	resp, err := hostedDo(cr, "GET", path, "", nil)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, false, &streamStatusError{status: resp.StatusCode, err: hostedError("GET", path, resp, raw)}
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	kind := "message"
	var data []string
	// frame hands one frame on: an event once, by its sequence; the hello of a
	// reconnection is the same stream going on, so it is not announced again
	frame := func(kind string, b []byte) (*restartSignal, bool) {
		if reconnecting {
			reconnecting = false
			if onFrame("reconnected", cursorJSON(*cursor, "")) {
				return nil, true
			}
			if kind == "hello" {
				return nil, false
			}
		}
		switch kind {
		case "event":
			var e struct {
				StreamSeq *int64 `json:"stream_seq"`
			}
			if json.Unmarshal(b, &e) == nil && e.StreamSeq != nil {
				if *e.StreamSeq <= *cursor {
					return nil, false
				}
				*cursor = *e.StreamSeq
			}
		case "end":
			if sig := restartOf(b); sig != nil {
				return sig, false
			}
		}
		return nil, onFrame(kind, b)
	}
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case line == "":
			if len(data) > 0 {
				sig, done := frame(kind, []byte(strings.Join(data, "\n")))
				if sig != nil || done {
					return sig, done, nil
				}
			}
			kind, data = "message", nil
		case strings.HasPrefix(line, ":"): // a comment, which keep-alives are
		case strings.HasPrefix(line, "event:"):
			kind = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return nil, false, err
	}
	// a stream that ended on a frame the blank line never followed: the
	// frame was still sent, so it is still rendered
	if len(data) > 0 {
		if sig, _ := frame(kind, []byte(strings.Join(data, "\n"))); sig != nil {
			return sig, false, nil
		}
	}
	return nil, true, nil
}
