package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestRetryHintReadsWhatTheVendorStates pins the three answers a limit can give:
// a Retry-After in seconds, one as an HTTP date, and no header at all, where a
// spent free allowance still has Zen's published 00:00 UTC reset.
func TestRetryHintReadsWhatTheVendorStates(t *testing.T) {
	now := time.Date(2026, 11, 9, 21, 30, 0, 0, time.UTC)
	tests := []struct {
		name      string
		headers   http.Header
		freeLimit bool
		want      int
	}{
		{"delta seconds", http.Header{"Retry-After": {"42"}}, false, 42},
		{"http date", http.Header{"Retry-After": {"Mon, 09 Nov 2026 21:45:00 GMT"}}, false, 900},
		{"date already past", http.Header{"Retry-After": {"Mon, 09 Nov 2026 21:00:00 GMT"}}, false, 0},
		{"unreadable header", http.Header{"Retry-After": {"soon"}}, false, 0},
		{"free allowance without a header", nil, true, 9000},
		{"header outranks the free reset", http.Header{"Retry-After": {"7"}}, true, 7},
		{"no statement at all", nil, false, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryHint(tc.headers, tc.freeLimit, now); got != tc.want {
				t.Fatalf("retryHint() = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestClassifyCarriesTheRetryHint pins the contract with CPA: the cooldown CPA
// applies is what the error states, so a limit cannot be retried before Zen says
// the allowance is back.
func TestClassifyCarriesTheRetryHint(t *testing.T) {
	headers := http.Header{"Retry-After": {"120"}}
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"free allowance", `{"error":{"message":"Free models work in OpenCode","type":"FreeUsageLimitError"}}`, 120},
		{"generic rate limit", `{"error":{"message":"rate_limit reached","type":"rate_limit_error"}}`, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyUpstream(http.StatusTooManyRequests, headers, []byte(tc.body))
			var perr *pluginabi.Error
			if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusTooManyRequests {
				t.Fatalf("err = %#v", err)
			}
			if perr.RetryAfterSeconds != tc.want {
				t.Fatalf("RetryAfterSeconds = %d, want %d", perr.RetryAfterSeconds, tc.want)
			}
		})
	}
}

// TestExecuteCarriesTheFreeAllowanceReset is the end of the path an operator
// sees: a refusal that arrives through the executor, not through classify
// directly, still states when the allowance returns.
func TestExecuteCarriesTheFreeAllowanceReset(t *testing.T) {
	resetPluginState()
	sender := testDiscovery().sender()
	sender.reply = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, "/inference/openai/v1/chat/completions") {
			return jsonReply(http.StatusTooManyRequests, `{"error":{"message":"Free models work in OpenCode","type":"FreeUsageLimitError"}}`), nil
		}
		return testDiscovery().sender().reply(req)
	}
	body, _ := json.Marshal(map[string]any{"model": "big-pickle", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	_, err := execute(pluginapi.ExecutorRequest{Model: "big-pickle", Payload: body, StorageJSON: testCredential()}, sender)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %#v", err)
	}
	want := retryHint(nil, true, time.Now())
	if perr.RetryAfterSeconds < want-5 || perr.RetryAfterSeconds > want {
		t.Fatalf("RetryAfterSeconds = %d, want about %d", perr.RetryAfterSeconds, want)
	}
}
