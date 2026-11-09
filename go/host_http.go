package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type httpSender interface {
	Do(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

type cpaSender struct{ callbackID string }

// transportError keeps what the host said about a request that never got an
// answer. The cause is the difference between a caller that went away, a host
// timeout and a broken connection, and each of those deserves its own status.
type transportError struct{ cause string }

func (e *transportError) Error() string { return e.cause }

func (s cpaSender) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	var out pluginapi.HTTPResponse
	err := hostRPC("host.http.do", map[string]any{"host_callback_id": s.callbackID, "request": req}, &out)
	if err != nil {
		return out, &transportError{cause: err.Error()}
	}
	return out, nil
}

// transportFailure maps a request that got no answer onto the status a client
// should see. A cancellation is the caller's own, not the credential's: CPA reads
// 499 as "the client closed the request" and does not rest the account for it.
func transportFailure(err error) error {
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "context canceled"),
		strings.Contains(text, "client closed"),
		strings.Contains(text, "client disconnected"),
		strings.Contains(text, "broken pipe"),
		strings.Contains(text, "connection reset"):
		return pluginabi.NewError("client_closed",
			"the caller closed the request before OpenCode Zen answered; a reasoning model can take longer than a caller's timeout, so stream it instead",
			499)
	case strings.Contains(text, "deadline exceeded"),
		strings.Contains(text, "timeout"),
		strings.Contains(text, "timed out"):
		return pluginabi.NewError("upstream_timeout", "OpenCode Zen did not answer within the host's time limit", http.StatusGatewayTimeout)
	default:
		return pluginabi.NewError("transport_error", "OpenCode Zen upstream transport failed", 503)
	}
}

// zenError is a refusal OpenCode Zen answered with: its HTTP status when it had
// one, the code in its envelope, and its message.
type zenError struct {
	Status  int
	Code    string
	Message string
}

func (e *zenError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("HTTP %d, code %s: %s", e.Status, e.Code, e.Message)
	case e.Code != "":
		return fmt.Sprintf("HTTP %d, code %s", e.Status, e.Code)
	case e.Message != "":
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	default:
		return fmt.Sprintf("HTTP %d", e.Status)
	}
}

// upstreamMessage reads Zen's refusal text from either shape it sends: an
// OpenAI-style {error: {message, type, code}}, or its own {code, msg} envelope.
func upstreamMessage(body []byte) (code, message, kind string) {
	var payload struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
		} `json:"error"`
		Type    string          `json:"type"`
		Code    json.RawMessage `json:"code"`
		Message *string         `json:"msg"`
	}
	if json.Unmarshal(body, &payload) == nil {
		if payload.Message != nil {
			message = strings.TrimSpace(*payload.Message)
		}
		if len(payload.Code) > 0 {
			var s string
			if json.Unmarshal(payload.Code, &s) == nil {
				code = strings.TrimSpace(s)
			} else {
				var n json.Number
				if json.Unmarshal(payload.Code, &n) == nil {
					code = n.String()
				}
			}
		}
		if payload.Error != nil {
			kind = strings.TrimSpace(payload.Error.Type)
			if strings.TrimSpace(payload.Error.Message) != "" {
				message = strings.TrimSpace(payload.Error.Message)
			}
			switch v := payload.Error.Code.(type) {
			case string:
				if v != "" {
					code = v
				}
			case float64:
				code = fmt.Sprintf("%.0f", v)
			}
			if kind == "" {
				kind = strings.TrimSpace(payload.Type)
			}
		}
	}
	if message == "" {
		text := strings.TrimSpace(string(body))
		if len(text) > 400 {
			text = text[:400]
		}
		message = text
	}
	return code, message, kind
}

// classifyUpstream maps a Zen refusal onto the status the client should see. A
// 429 keeps its message, which says whether the shared daily allowance is spent,
// and carries how long that allowance still runs, so CPA rests the model instead
// of retrying it against a spent allowance. A 403 that names the OpenCode identity
// says the plugin needs updating rather than the credential.
func classifyUpstream(status int, headers http.Header, body []byte) error {
	code, message, kind := upstreamMessage(body)
	if message == "" {
		message = http.StatusText(status)
	}
	lower := strings.ToLower(message + " " + kind + " " + code)
	freeTier := strings.Contains(lower, "freetiererror") || strings.Contains(lower, "within opencode")
	freeLimit := strings.Contains(lower, "freeusagelimiterror") || strings.Contains(lower, "free models work in opencode")
	// A refusal carried inside an OK stream has no status of its own; its type
	// and code are what say which refusal it is.
	effective := status
	if effective == 0 {
		switch {
		case freeLimit:
			effective = http.StatusTooManyRequests
		case freeTier:
			effective = http.StatusForbidden
		case strings.Contains(lower, "rate_limit"), strings.Contains(lower, "too many requests"), code == "429":
			effective = http.StatusTooManyRequests
		case strings.Contains(lower, "unauthorized"), strings.Contains(lower, "authentication"), code == "401":
			effective = http.StatusUnauthorized
		case strings.Contains(lower, "not found"), code == "404":
			effective = http.StatusNotFound
		case code == "403", strings.Contains(lower, "forbidden"):
			effective = http.StatusForbidden
		}
	}
	switch {
	case effective == http.StatusUnauthorized:
		return pluginabi.NewError("reauth_required", "OpenCode Zen Free sign-in is no longer accepted; sign in again", http.StatusUnauthorized)
	case effective == http.StatusPaymentRequired:
		return pluginabi.NewError("payment_required", "OpenCode Zen: "+message, effective)
	case effective == http.StatusForbidden:
		if freeTier {
			message = "OpenCode Zen refused this client's identity: " + message + ". Update the plugin: Zen refuses the exact User-Agent older releases sent."
		}
		return pluginabi.NewError("forbidden", "OpenCode Zen: "+message, effective)
	case effective == http.StatusTooManyRequests:
		if freeLimit {
			message = message + ". Zen's shared free allowance is spent; it resets at 00:00 UTC."
		}
		err := pluginabi.NewError("rate_limited", "OpenCode Zen is limiting this request: "+message, effective)
		err.RetryAfterSeconds = retryHint(headers, freeLimit, time.Now())
		// The free allowance is shared by every model the public credential can
		// reach, so a spent allowance cools the credential; a plain rate limit
		// stays scoped to the requested model.
		err.CredentialScoped = freeLimit
		return err
	case effective == http.StatusNotFound:
		return pluginabi.NewError("not_found", "OpenCode Zen: "+message, effective)
	case effective >= 500:
		return pluginabi.NewError("upstream_error", "OpenCode Zen upstream failed: "+message, http.StatusBadGateway)
	case effective >= 400:
		return pluginabi.NewError("invalid_request", "OpenCode Zen: "+message, effective)
	default:
		return pluginabi.NewError("upstream_error", "OpenCode Zen: "+message, http.StatusBadGateway)
	}
}

// retryHint is how long a limit still runs, in whole seconds, or 0 when nothing
// says. A Retry-After the upstream sends is the vendor's own statement and wins;
// otherwise a spent free allowance is read from the reset Zen publishes for it,
// 00:00 UTC. The number reaches CPA as the cooldown it applies to the model.
func retryHint(headers http.Header, freeLimit bool, now time.Time) int {
	if headers != nil {
		if value := strings.TrimSpace(headers.Get("Retry-After")); value != "" {
			if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
				return seconds
			}
			if at, err := http.ParseTime(value); err == nil {
				if seconds := secondsUntil(at, now); seconds > 0 {
					return seconds
				}
				return 0
			}
		}
	}
	if !freeLimit {
		return 0
	}
	utc := now.UTC()
	return secondsUntil(time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(24*time.Hour), now)
}

// secondsUntil is the wait to at in whole seconds, 0 once it has passed. A wait
// under a second still counts as one: CPA treats zero as "no deadline".
func secondsUntil(at, now time.Time) int {
	wait := at.Sub(now)
	if wait <= 0 {
		return 0
	}
	if seconds := int(wait / time.Second); seconds > 0 {
		return seconds
	}
	return 1
}

// countTokens is not supported: Zen documents no token-count endpoint, and a
// guess would misbill clients that trust it.
func countTokens() (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, pluginabi.NewError("not_supported", "OpenCode Zen provides no verified token-count endpoint", http.StatusNotImplemented)
}
