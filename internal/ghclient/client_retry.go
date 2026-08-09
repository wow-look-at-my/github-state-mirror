package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// doJSON -- the shared HTTP exchange behind every REST/GraphQL helper -- and
// its transient-failure retry policy. What is and is not retried is a
// correctness decision, not a tuning knob: see the doJSON comment.

// fail a whole multi-page owner fetch. Authoritative statuses (401/403/404/...)
// are never retried: the reveal layer and deny-cache semantics depend on them.
// GraphQL-level errors[] bodies arrive as HTTP 200 and stay fail-fast too (the
// callers inspect them after doJSON returns).
const doJSONAttempts = 3

// retryAfterCap bounds how long a parseable Retry-After header can stretch one
// backoff, so a huge value can't wedge a deadline-bounded fetch.
const retryAfterCap = 10 * time.Second

// defaultRetryBackoff is the sleep before attempts 2, 3, ... (the last entry
// repeats).
var defaultRetryBackoff = []time.Duration{500 * time.Millisecond, 2 * time.Second}

// SetRetryBackoff overrides doJSON's transient-retry backoff schedule (tests
// use zero delays so retries don't really sleep). The i-th entry is the sleep
// before attempt i+2; the last entry repeats. Call it during wiring, like
// SetRateObserver: the field is read without synchronization.
func (c *Client) SetRetryBackoff(delays []time.Duration) { c.retryBackoff = delays }

// retryableStatus reports whether an HTTP status is a transient upstream
// failure worth retrying (never an authoritative 4xx answer).
func retryableStatus(status int) bool {
	switch status {
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusTooManyRequests:
		return true
	}
	return false
}

// retryDelay returns the sleep before the given attempt (attempt >= 2): the
// configured backoff, raised to a retryable response's parseable Retry-After
// (capped at retryAfterCap).
func (c *Client) retryDelay(attempt int, resp *http.Response) time.Duration {
	backoff := c.retryBackoff
	if len(backoff) == 0 {
		backoff = defaultRetryBackoff
	}
	i := attempt - 2
	if i >= len(backoff) {
		i = len(backoff) - 1
	}
	d := backoff[i]
	if ra := retryAfterDelay(resp); ra > d {
		d = ra
	}
	return d
}

// retryAfterDelay parses a response's Retry-After header (seconds form; zero
// when absent/unparseable), capped at retryAfterCap.
func retryAfterDelay(resp *http.Response) time.Duration {
	if resp == nil {
		return 0
	}
	secs, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || secs <= 0 {
		return 0
	}
	if d := time.Duration(secs) * time.Second; d < retryAfterCap {
		return d
	}
	return retryAfterCap
}

// sleepBeforeRetry sleeps the backoff before the given attempt, cut short by
// ctx cancellation (false = don't retry).
func (c *Client) sleepBeforeRetry(ctx context.Context, attempt int, resp *http.Response) bool {
	d := c.retryDelay(attempt, resp)
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func (c *Client) doJSON(ctx context.Context, method, path string, body io.Reader, out interface{}) error {
	// Buffer the body once so every retry attempt can resend it from the
	// start (an io.Reader is consumed by the first attempt).
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = io.ReadAll(body)
		if err != nil {
			return err
		}
	}
	url := c.baseURL + path
	// Authenticate with the token carried in the context (caller's bearer token
	// or a GitHub App installation token). Requests without one are sent
	// unauthenticated and will be rejected by GitHub.
	token := tokenFromContext(ctx)

	var resp *http.Response
	for attempt := 1; ; attempt++ {
		var reqBody io.Reader
		if bodyBytes != nil {
			reqBody = bytes.NewReader(bodyBytes)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if bodyBytes != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err = c.httpClient.Do(req)
		if err != nil {
			// Network errors are transient EXCEPT the caller's own
			// cancellation/deadline -- retrying those only delays the exit.
			if attempt >= doJSONAttempts || ctx.Err() != nil ||
				errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}
			if !c.sleepBeforeRetry(ctx, attempt+1, nil) {
				return err
			}
			continue
		}
		c.observeRate(ctx, token, resp)

		if attempt < doJSONAttempts && retryableStatus(resp.StatusCode) {
			// Drain a little and close so the connection is reusable, then
			// back off (honoring a capped Retry-After) and resend.
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			resp.Body.Close()
			if !c.sleepBeforeRetry(ctx, attempt+1, resp) {
				return fmt.Errorf("github api %s %s: %d (retry canceled: %w)", method, path, resp.StatusCode, ctx.Err())
			}
			continue
		}
		break
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github api %s %s: %d %s", method, path, resp.StatusCode, string(data))
	}

	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
