package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type fakeSender struct {
	mu    sync.Mutex
	calls []pluginapi.HTTPRequest
	reply func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

func (f *fakeSender) Do(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	f.mu.Unlock()
	return f.reply(req)
}

func (f *fakeSender) last() pluginapi.HTTPRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSender) requests() []pluginapi.HTTPRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]pluginapi.HTTPRequest(nil), f.calls...)
}

func jsonReply(status int, body string) pluginapi.HTTPResponse {
	return pluginapi.HTTPResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json"}}, Body: []byte(body)}
}

func testCredential() []byte {
	raw, _ := json.Marshal(publicCredential())
	return raw
}

// resetPluginState clears the caches one test's fixture would otherwise leak into
// the next.
func resetPluginState() {
	zen.mu.Lock()
	zen.known = bootstrapChatModels()
	zen.protocols = cloneStrings(bootstrapProtocols)
	zen.catalog = map[string]catalogModel{}
	zen.providerNPM = sdkChat
	zen.listed = false
	zen.listedAt = time.Time{}
	zen.attempted = time.Time{}
	zen.version = defaultOpenCodeVersion
	zen.versionAt = time.Time{}
	zen.versionFlight = nil
	zen.mu.Unlock()
	sessionMu.Lock()
	sessions = map[string]string{}
	sessionMu.Unlock()
	traceMu.Lock()
	traces = map[string]string{}
	traceMu.Unlock()
	gateMu.Lock()
	gates = map[string]*credentialGate{}
	gateMu.Unlock()
	idMu.Lock()
	lastStamp = 0
	idCounter = 0
	idMu.Unlock()
	authDirState.Lock()
	authDirState.dir = ""
	authDirState.Unlock()
	storeMu.Lock()
	storeLoaded = false
	storeMu.Unlock()
	_ = configure(nil)
}

// discoveryReply answers the four documents model discovery reads, with the
// fixtures a test gives it.
type discoveryReply struct {
	version       string
	listing       string
	catalog       string
	listingStatus int
	chat          func(pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error)
}

func (d discoveryReply) sender() *fakeSender {
	return &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		switch {
		case strings.HasPrefix(req.URL, versionURL):
			return jsonReply(200, `{"version":"`+firstNonEmpty(d.version, defaultOpenCodeVersion)+`"}`), nil
		case strings.HasSuffix(req.URL, "/inference/v1/models"):
			status := d.listingStatus
			if status == 0 {
				status = 200
			}
			return jsonReply(status, d.listing), nil
		case req.URL == catalogURL:
			return jsonReply(200, d.catalog), nil
		case d.chat != nil:
			return d.chat(req)
		default:
			return jsonReply(404, `{"error":{"message":"unexpected URL `+req.URL+`"}}`), nil
		}
	}}
}

const testCatalog = `{"opencode":{"npm":"@ai-sdk/openai-compatible","models":{
  "big-pickle":{"name":"Big Pickle","cost":{"input":0,"output":0},"limit":{"context":200000,"output":32000},
    "modalities":{"input":["text"],"output":["text"]},"reasoning":true,"tool_call":true,
    "reasoning_options":[{"type":"effort","values":["low","high"]}],
    "provider":{"npm":"@ai-sdk/openai-compatible"}},
  "space-bunny-free":{"name":"Space Bunny Free","cost":{"input":0,"output":0},"limit":{"context":1048576,"output":524288},
    "provider":{"npm":"@ai-sdk/openai-compatible"}},
  "muse-spark-1.3-contributor-free":{"name":"Muse Spark","cost":{"input":0,"output":0},
    "limit":{"context":1048576,"output":131072},"provider":{"npm":"@ai-sdk/openai"},"reasoning":true},
  "paid-model":{"name":"Paid","cost":{"input":1,"output":2},"limit":{"context":128000,"output":4096}},
  "retired-free":{"name":"Retired","cost":{"input":0,"output":0},"status":"deprecated"}
}}}`

const testListing = `{"data":[
  {"id":"big-pickle"},
  {"id":"space-bunny-free"},
  {"id":"muse-spark-1.3-contributor-free"},
  {"id":"paid-model"},
  {"id":"retired-free"},
  {"id":"brand-new-free"}
]}`

func testDiscovery() discoveryReply {
	return discoveryReply{listing: testListing, catalog: testCatalog}
}

func TestStartLoginMintsAKeylessState(t *testing.T) {
	resetPluginState()
	start, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: credentialProvider})
	if err != nil {
		t.Fatal(err)
	}
	if start.Provider != credentialProvider || start.URL != "" {
		t.Fatalf("start = %+v", start)
	}
	if !strings.HasPrefix(start.State, loginStatePrefix) || len(start.State) > 128 {
		t.Fatalf("state = %q", start.State)
	}
	if !start.ExpiresAt.After(time.Now()) {
		t.Fatalf("expiry = %v", start.ExpiresAt)
	}
	if _, ok := parseLoginState(start.State, time.Now()); !ok {
		t.Fatal("the state the plugin minted did not parse")
	}
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusSuccess || poll.Auth.Provider != credentialProvider {
		t.Fatalf("poll = %+v", poll)
	}
	if poll.Auth.Label != authLabel || poll.Auth.ID != authFileName || poll.Auth.FileName != authFileName {
		t.Fatalf("auth = %+v", poll.Auth)
	}
	var c credential
	if err := json.Unmarshal(poll.Auth.StorageJSON, &c); err != nil {
		t.Fatal(err)
	}
	if c.Access != publicKey {
		t.Fatalf("credential = %+v", c)
	}
	// A panel that polls again, or a plugin restarted between start and poll,
	// still receives the same public credential.
	again, err := pollLogin(pluginapi.AuthLoginPollRequest{State: start.State})
	if err != nil || again.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("second poll = %+v, %v", again, err)
	}
}

func TestStartLoginRejectsAnUnknownEntry(t *testing.T) {
	resetPluginState()
	_, err := startLogin(pluginapi.AuthLoginStartRequest{Provider: "workbuddy"})
	if err == nil || !strings.Contains(err.Error(), "unknown login entry") {
		t.Fatalf("err = %v", err)
	}
}

func TestPollLoginRejectsAStateItDidNotMint(t *testing.T) {
	resetPluginState()
	poll, err := pollLogin(pluginapi.AuthLoginPollRequest{State: "forged-state"})
	if err != nil {
		t.Fatal(err)
	}
	if poll.Status != pluginapi.AuthLoginStatusError || !strings.Contains(poll.Message, "sign-in expired") {
		t.Fatalf("poll = %+v", poll)
	}
	expired := loginState(time.Now().Add(-time.Minute), make([]byte, 16))
	poll, _ = pollLogin(pluginapi.AuthLoginPollRequest{State: expired})
	if poll.Status != pluginapi.AuthLoginStatusError {
		t.Fatalf("expired poll = %+v", poll)
	}
}

func TestRefreshReturnsTheSamePublicCredential(t *testing.T) {
	resetPluginState()
	resp, err := refreshCredential(pluginapi.AuthRefreshRequest{AuthID: authFileName, StorageJSON: testCredential()})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Auth.ID != authFileName || resp.Auth.Provider != credentialProvider || resp.NextRefreshAfter.IsZero() {
		t.Fatalf("refresh = %+v", resp)
	}
	var c credential
	if json.Unmarshal(resp.Auth.StorageJSON, &c) != nil || c.Access != publicKey {
		t.Fatalf("credential = %+v", c)
	}
	_, err = refreshCredential(pluginapi.AuthRefreshRequest{AuthID: authFileName, StorageJSON: []byte(`{"access":"personal-key"}`)})
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusUnauthorized {
		t.Fatalf("err = %#v", err)
	}
}

func TestModelsComeFromTheLiveList(t *testing.T) {
	resetPluginState()
	sender := testDiscovery().sender()
	out, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential()}, sender)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "big-pickle,brand-new-free,space-bunny-free" {
		t.Fatalf("models = %v", ids)
	}
	big := out.Models[0]
	if big.ContextLength != 200000 || big.OutputTokenLimit != 32000 || big.Thinking == nil {
		t.Fatalf("big-pickle = %+v", big)
	}
	if strings.Join(big.Thinking.Levels, ",") != "low,high" {
		t.Fatalf("levels = %v", big.Thinking.Levels)
	}
	// A model Zen lists as free that neither the catalog nor the docs describe
	// is offered on chat, as OpenCode asks a model with no package of its own.
	fresh := out.Models[1]
	if fresh.ContextLength != 32768 || fresh.OutputTokenLimit != 4096 {
		t.Fatalf("brand-new-free = %+v", fresh)
	}
	listing := sender.requests()[1]
	if !strings.HasPrefix(listing.Headers.Get("User-Agent"), "opencode/latest/") ||
		listing.Headers.Get("Authorization") != "Bearer public" ||
		listing.Headers.Get("X-Opencode-Client") != "cli" {
		t.Fatalf("listing headers = %v", listing.Headers)
	}
}

func TestTheLiveListCarriesTheMetadata(t *testing.T) {
	resetPluginState()
	listing := `{"object":"list","data":[
	  {"id":"rich-free","name":"Rich Free","type":"chat","release_date":"2026-11-01",
	   "limit":{"context":500000,"input":400000,"output":64000},
	   "modalities":{"input":["text","image/png","video/mp4"],"output":["text"]},
	   "capabilities":{"tools":{"supported":true},"reasoning":{"supported":true,"effort":["low","high"]}},
	   "cost":{"text":{"input":{"price":0},"output":{"price":0}}},
	   "api":{"chat-completions":{"base_url":"https://opencode.ai/inference/openai/v1"}}},
	  {"id":"rich-responses-free","name":"Rich Responses","type":"chat",
	   "cost":{"text":{"input":{"price":0},"output":{"price":0}}},"api":{"responses":{}}},
	  {"id":"rich-paid","name":"Paid","type":"chat",
	   "cost":{"text":{"input":{"price":1},"output":{"price":2}}},"api":{"chat-completions":{}}}
	]}`
	sender := discoveryReply{listing: listing}.sender()
	out, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential()}, sender)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "rich-free" {
		t.Fatalf("models = %v", ids)
	}
	m := out.Models[0]
	if m.Name != "Rich Free" || m.ContextLength != 500000 || m.InputTokenLimit != 400000 || m.OutputTokenLimit != 64000 {
		t.Fatalf("model = %+v", m)
	}
	// A listing that describes its models itself does not ask the catalog or the
	// docs: only the version and the list were read.
	if calls := len(sender.requests()); calls != 2 {
		t.Fatalf("discovery made %d requests, want version + list", calls)
	}
	if strings.Join(m.SupportedInputModalities, ",") != "text,image,video" {
		t.Fatalf("modalities = %v", m.SupportedInputModalities)
	}
	if m.Thinking == nil || strings.Join(m.Thinking.Levels, ",") != "low,high" {
		t.Fatalf("thinking = %+v", m.Thinking)
	}
}

func TestTheLastGoodListOutlivesTheProcess(t *testing.T) {
	resetPluginState()
	dir := t.TempDir()
	setAuthDir(dir)
	defer setAuthDir("")
	sender := testDiscovery().sender()
	if _, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential()}, sender); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, modelsFileName)); err != nil {
		t.Fatalf("the list was not persisted: %v", err)
	}
	// Forget everything the process holds, as a restart would, and make Zen's
	// list unreadable. The stored list is what answers, not the bootstrap.
	zen.mu.Lock()
	zen.known = bootstrapChatModels()
	zen.listed = false
	zen.listedAt = time.Time{}
	zen.attempted = time.Time{}
	zen.mu.Unlock()
	storeMu.Lock()
	storeLoaded = false
	storeMu.Unlock()
	failed := discoveryReply{listingStatus: 503}.sender()
	out, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential()}, failed)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "big-pickle,brand-new-free,space-bunny-free" {
		t.Fatalf("models after a restart = %v", ids)
	}
}

func TestModelsKeepTheLastListWhenZenCannotBeRead(t *testing.T) {
	resetPluginState()
	reply := testDiscovery()
	sender := reply.sender()
	if _, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential()}, sender); err != nil {
		t.Fatal(err)
	}
	// A failed listing keeps the last good list instead of the bootstrap one.
	reply.listingStatus = 500
	failed := reply.sender()
	out, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential()}, failed)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "big-pickle,brand-new-free,space-bunny-free" {
		t.Fatalf("models after a failed listing = %v", ids)
	}
}

func TestModelsFallBackToTheShippedList(t *testing.T) {
	resetPluginState()
	sender := testDiscovery().sender()
	sender.reply = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, "/inference/v1/models") {
			return jsonReply(503, `{"error":{"message":"down"}}`), nil
		}
		if strings.HasPrefix(req.URL, versionURL) {
			return jsonReply(200, `{"version":"2.2.5"}`), nil
		}
		return jsonReply(200, `{}`), nil
	}
	out, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential()}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != len(bootstrapCatalogue) {
		t.Fatalf("models = %d", len(out.Models))
	}
}

func TestModelsApplyTheContextFloor(t *testing.T) {
	resetPluginState()
	catalog := `{"opencode":{"models":{"deepseek-v4.1-flash-free":{"name":"DeepSeek","cost":{"input":0,"output":0},"limit":{"context":200000,"output":32000}}}}}`
	sender := discoveryReply{
		listing: `{"data":[{"id":"deepseek-v4.1-flash-free"}]}`,
		catalog: catalog,
	}.sender()
	out, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential()}, sender)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0].ContextLength != 258624 {
		t.Fatalf("models = %+v", out.Models)
	}
}

func TestExcludedModelsAreNeverAdvertised(t *testing.T) {
	resetPluginState()
	if err := configure([]byte("exclude_models: [big-pickle, \"Space-Bunny-Free\"]\n")); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = configure(nil) }()
	sender := testDiscovery().sender()
	out, err := modelsForAuth(pluginapi.AuthModelRequest{StorageJSON: testCredential()}, sender)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range out.Models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "brand-new-free" {
		t.Fatalf("models = %v", ids)
	}
	body, _ := json.Marshal(map[string]any{"model": "big-pickle", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	_, err = execute(pluginapi.ExecutorRequest{Model: "big-pickle", Payload: body, StorageJSON: testCredential()}, sender)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.Code != "unknown_model" {
		t.Fatalf("err = %#v", err)
	}
}

func TestAnExcludeEntryThatIsNotAModelIdIsRefused(t *testing.T) {
	resetPluginState()
	defer func() { _ = configure(nil) }()
	for _, raw := range []string{
		"exclude_models: [\"not a model id\"]\n",
		"exclude_models: [\"big-pickle/../x\"]\n",
	} {
		if err := configure([]byte(raw)); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
}

func TestPrepareRequestSendsOpenCodesIdentity(t *testing.T) {
	resetPluginState()
	sender := testDiscovery().sender()
	body, _ := json.Marshal(map[string]any{
		"model":    "big-pickle",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	plan, err := prepareRequest(pluginapi.ExecutorRequest{
		AuthID:      authFileName,
		Model:       "big-pickle",
		Payload:     body,
		StorageJSON: testCredential(),
		Headers:     http.Header{"X-Session-Id": {"sess-1"}},
	}, sender)
	if err != nil {
		t.Fatal(err)
	}
	h := plan.HTTP.Headers
	if h.Get("Authorization") != "Bearer public" || h.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", h)
	}
	if h.Get("User-Agent") != "opencode/latest/2.2.5/cli" || h.Get("X-Opencode-Client") != "cli" {
		t.Fatalf("identity headers = %v", h)
	}
	if h.Get("X-Opencode-Session") != h.Get("X-Opencode-Session-Id") ||
		h.Get("X-Session-Id") != h.Get("X-Session-Affinity") ||
		h.Get("X-Opencode-Session") != h.Get("X-Session-Id") {
		t.Fatalf("session headers = %v", h)
	}
	if !strings.HasPrefix(h.Get("B3"), "") || strings.Count(h.Get("B3"), "-") != 3 ||
		!strings.HasPrefix(h.Get("Traceparent"), "00-") {
		t.Fatalf("trace headers = %v", h)
	}
	if len(h.Get("X-Opencode-Project")) != 40 {
		t.Fatalf("project = %q", h.Get("X-Opencode-Project"))
	}
	var sent map[string]any
	if err := json.Unmarshal(plan.HTTP.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["stream"] != true || sent["model"] != "big-pickle" {
		t.Fatalf("body = %v", sent)
	}
	options, _ := sent["stream_options"].(map[string]any)
	if options["include_usage"] != true {
		t.Fatalf("stream options = %v", sent["stream_options"])
	}
	tools, _ := sent["tools"].([]any)
	names := toolNames(tools)
	for _, core := range []string{"edit", "glob", "grep", "read", "shell"} {
		if !names[core] {
			t.Fatalf("core tool %q missing from %v", core, names)
		}
	}
	messages, _ := sent["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", messages)
	}
	text := textOf(messages[0].(map[string]any)["content"])
	if !strings.Contains(text, "<env>") || !strings.Contains(text, "  Working directory: ") ||
		strings.Contains(text, "Working directory: unknown") {
		t.Fatalf("environment = %q", text)
	}
}

func TestPrepareRequestKeepsAnExistingEnvironmentBlock(t *testing.T) {
	resetPluginState()
	sender := testDiscovery().sender()
	body, _ := json.Marshal(map[string]any{
		"model": "big-pickle",
		"messages": []map[string]any{
			{"role": "system", "content": "prompt\n\n<env>\n  Working directory: /work\n  Is directory a git repo: no\n</env>"},
			{"role": "user", "content": "hi"},
		},
	})
	plan, err := prepareRequest(pluginapi.ExecutorRequest{Model: "big-pickle", Payload: body, StorageJSON: testCredential()}, sender)
	if err != nil {
		t.Fatal(err)
	}
	text := textOf(plan.Chat["messages"].([]any)[0].(map[string]any)["content"])
	if strings.Count(text, "<env>") != 1 {
		t.Fatalf("the block was added twice: %q", text)
	}
	if plan.HTTP.Headers.Get("X-Opencode-Project") != sha1Hex("directory:/work") {
		t.Fatalf("project = %q", plan.HTTP.Headers.Get("X-Opencode-Project"))
	}
}

func TestExecuteAggregatesTheStreamAndMapsTools(t *testing.T) {
	resetPluginState()
	var upstream pluginapi.HTTPRequest
	sender := testDiscovery().sender()
	sender.reply = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasSuffix(req.URL, "/inference/openai/v1/chat/completions") {
			upstream = req
			return pluginapi.HTTPResponse{
				StatusCode: 200,
				Headers:    http.Header{"Content-Type": {"text/event-stream"}},
				Body: []byte("data: {\"id\":\"chat-1\",\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello \"}}]}\n\n" +
					"data: {\"id\":\"chat-1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"findings_report\",\"arguments\":\"{\\\"a\\\"\"}}]}}]}\n\n" +
					"data: {\"id\":\"chat-1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
					"data: {\"id\":\"chat-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":2,\"total_tokens\":11}}\n\n" +
					"data: [DONE]\n\n"),
			}, nil
		}
		return testDiscovery().sender().reply(req)
	}
	body, _ := json.Marshal(map[string]any{
		"model": "big-pickle",
		"messages": []map[string]any{
			{"role": "system", "content": "You are a test agent."},
			{"role": "user", "content": "hi"},
		},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "ReportFindings", "parameters": map[string]any{"type": "object"}}},
			{"type": "function", "function": map[string]any{"name": "ScheduleWakeup", "parameters": map[string]any{"type": "object"}}},
		},
	})
	resp, err := execute(pluginapi.ExecutorRequest{
		AuthID:      authFileName,
		Model:       "big-pickle",
		Payload:     body,
		StorageJSON: testCredential(),
		Headers:     http.Header{"X-Session-Id": {"sess-1"}},
	}, sender)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role      string `json:"role"`
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatal(err)
	}
	if out.Object != "chat.completion" || out.Model != "big-pickle" || len(out.Choices) != 1 {
		t.Fatalf("completion = %s", resp.Payload)
	}
	if out.Choices[0].Message.Content != "Hello " || out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("choice = %+v", out.Choices[0])
	}
	calls := out.Choices[0].Message.ToolCalls
	if len(calls) != 1 || calls[0].Function.Name != "ReportFindings" || calls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("tool calls = %+v", calls)
	}
	if out.Usage["total_tokens"] != float64(11) {
		t.Fatalf("usage = %v", out.Usage)
	}
	if upstream.Headers.Get("Authorization") != "Bearer public" {
		t.Fatalf("upstream headers = %v", upstream.Headers)
	}
	var sent map[string]any
	if err := json.Unmarshal(upstream.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if names := toolNames(sent["tools"].([]any)); !names["findings_report"] || !names["wakeup_schedule"] {
		t.Fatalf("refused names were not renamed: %v", names)
	}
}

func TestAModelTheLiveListLeftOutIsNotServedFromTheBootstrap(t *testing.T) {
	resetPluginState()
	// big-pickle is in the shipped list, but the live list no longer gives it
	// free. Serving it from the bootstrap would earn the same refusal again.
	sender := discoveryReply{listing: `{"data":[{"id":"cat-jam-free"}]}`}.sender()
	body, _ := json.Marshal(map[string]any{"model": "big-pickle", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	_, err := execute(pluginapi.ExecutorRequest{Model: "big-pickle", Payload: body, StorageJSON: testCredential()}, sender)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.Code != "unknown_model" {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(perr.Message, "cat-jam-free") {
		t.Fatalf("message = %q", perr.Message)
	}
}

func TestExecuteMapsRefusals(t *testing.T) {
	resetPluginState()
	cases := []struct {
		status   int
		body     string
		want     int
		code     string
		contains string
	}{
		{429, `{"error":{"message":"Free models work in OpenCode","type":"FreeUsageLimitError"}}`, 429, "rate_limited", "00:00 UTC"},
		{403, `{"error":{"message":"can only be used from within OpenCode","type":"FreeTierError"}}`, 403, "forbidden", "Update the plugin"},
		{401, `{"error":{"message":"unauthorized"}}`, 401, "reauth_required", "sign in again"},
		{500, `{"error":{"message":"boom"}}`, 502, "upstream_error", "boom"},
		{404, `{"error":{"message":"model not found"}}`, 404, "not_found", "model not found"},
	}
	for _, tc := range cases {
		sender := testDiscovery().sender()
		sender.reply = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			if strings.HasSuffix(req.URL, "/inference/openai/v1/chat/completions") {
				return jsonReply(tc.status, tc.body), nil
			}
			return testDiscovery().sender().reply(req)
		}
		body, _ := json.Marshal(map[string]any{"model": "big-pickle", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
		_, err := execute(pluginapi.ExecutorRequest{Model: "big-pickle", Payload: body, StorageJSON: testCredential()}, sender)
		var perr *pluginabi.Error
		if !errors.As(err, &perr) {
			t.Fatalf("%d: err = %#v", tc.status, err)
		}
		if perr.HTTPStatus != tc.want || perr.Code != tc.code || !strings.Contains(perr.Message, tc.contains) {
			t.Fatalf("%d: %d %s %q", tc.status, perr.HTTPStatus, perr.Code, perr.Message)
		}
	}
}

func TestExecuteRejectsUnknownModelsWithTheFreeList(t *testing.T) {
	resetPluginState()
	sender := testDiscovery().sender()
	body, _ := json.Marshal(map[string]any{"model": "gpt-9", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	_, err := execute(pluginapi.ExecutorRequest{Model: "gpt-9", Payload: body, StorageJSON: testCredential()}, sender)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusBadRequest || perr.Code != "unknown_model" {
		t.Fatalf("err = %#v", err)
	}
	if !strings.Contains(perr.Message, "big-pickle") || !strings.Contains(perr.Message, "brand-new-free") {
		t.Fatalf("message = %q", perr.Message)
	}
}

func TestACanceledRequestIsTheCallersNotTheCredentials(t *testing.T) {
	resetPluginState()
	cases := []struct {
		cause    string
		status   int
		code     string
		contains string
	}{
		{"context canceled", 499, "client_closed", "caller closed the request"},
		{"Post \"https://opencode.ai\": context deadline exceeded", http.StatusGatewayTimeout, "upstream_timeout", "time limit"},
		{"upstream connect error", 503, "transport_error", "transport failed"},
	}
	for _, tc := range cases {
		sender := testDiscovery().sender()
		sender.reply = func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
			if strings.HasSuffix(req.URL, "/inference/openai/v1/chat/completions") {
				return pluginapi.HTTPResponse{}, &transportError{cause: tc.cause}
			}
			return testDiscovery().sender().reply(req)
		}
		body, _ := json.Marshal(map[string]any{"model": "big-pickle", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
		_, err := execute(pluginapi.ExecutorRequest{Model: "big-pickle", Payload: body, StorageJSON: testCredential()}, sender)
		var perr *pluginabi.Error
		if !errors.As(err, &perr) || perr.HTTPStatus != tc.status || perr.Code != tc.code {
			t.Fatalf("%q: %#v", tc.cause, err)
		}
		if !strings.Contains(perr.Message, tc.contains) {
			t.Fatalf("%q: message %q", tc.cause, perr.Message)
		}
	}
}

func TestExecuteStreamForwardsBareFramesAndMapsTools(t *testing.T) {
	resetPluginState()
	sender := testDiscovery().sender()
	chunks := [][]byte{
		[]byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"}}]}\n"),
		[]byte("\ndata: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"findings_report\",\"arguments\":\"{\\\"a\\\"\"}}]}}]}\n"),
		[]byte("\ndata: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":1}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"),
	}
	next := 0
	var emitted [][]byte
	finished := make(chan struct{})
	pumpFinished = func(string) { close(finished) }
	openHTTPStream = func(plan requestPlan, callbackID string) (hostStreamResponse, error) {
		return hostStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, StreamID: "stream-1"}, nil
	}
	readStreamChunk = func(id string) (hostReadResponse, error) {
		if next >= len(chunks) {
			return hostReadResponse{Done: true}, nil
		}
		chunk := chunks[next]
		next++
		return hostReadResponse{Payload: chunk, Done: next == len(chunks)}, nil
	}
	emitStreamFrame = func(id string, payload []byte) error {
		emitted = append(emitted, append([]byte(nil), payload...))
		return nil
	}
	defer func() {
		openHTTPStream = openUpstreamStream
		readStreamChunk = readHostStream
		emitStreamFrame = emitStreamChunk
		pumpFinished = nil
	}()
	body, _ := json.Marshal(map[string]any{
		"model":    "big-pickle",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{
			{"type": "function", "function": map[string]any{"name": "ReportFindings"}},
			{"type": "function", "function": map[string]any{"name": "ScheduleWakeup"}},
		},
	})
	out, err := executeStream(pluginapi.ExecutorRequest{Model: "big-pickle", Payload: body, StorageJSON: testCredential()}, "cb", "stream-out", sender)
	if err != nil {
		t.Fatal(err)
	}
	if out["headers"] == nil {
		t.Fatal("no response headers")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream pump never returned")
	}
	joined := string(bytesJoin(emitted))
	if !strings.Contains(joined, "\"content\":\"Hi\"") {
		t.Fatalf("frames = %q", joined)
	}
	if !strings.Contains(joined, "\"name\":\"ReportFindings\"") || !strings.Contains(joined, "\"arguments\":\"{\\\"a\\\":1}\"") {
		t.Fatalf("tool call was not mapped back: %q", joined)
	}
	// The pump forwards bare data payloads: the host's handler owns the SSE
	// framing, and a framed payload would be wrapped again ("data: data: ...").
	if strings.Contains(joined, "data:") || strings.Contains(joined, "[DONE]") {
		t.Fatalf("frames carry SSE framing = %q", joined)
	}
}

func TestExecuteStreamEndsACutShortReply(t *testing.T) {
	resetPluginState()
	previous := firstFrameWindow
	firstFrameWindow = 40 * time.Millisecond
	defer func() { firstFrameWindow = previous }()
	sender := testDiscovery().sender()
	openHTTPStream = func(plan requestPlan, callbackID string) (hostStreamResponse, error) {
		return hostStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, StreamID: "stream-1"}, nil
	}
	reads := 0
	readStreamChunk = func(string) (hostReadResponse, error) {
		reads++
		if reads == 1 {
			return hostReadResponse{Payload: []byte("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")}, nil
		}
		return hostReadResponse{Done: true}, nil
	}
	var emitted [][]byte
	emitStreamFrame = func(id string, payload []byte) error {
		emitted = append(emitted, append([]byte(nil), payload...))
		return nil
	}
	finished := make(chan struct{})
	pumpFinished = func(string) { close(finished) }
	defer func() {
		openHTTPStream = openUpstreamStream
		readStreamChunk = readHostStream
		emitStreamFrame = emitStreamChunk
		pumpFinished = nil
	}()
	body, _ := json.Marshal(map[string]any{"model": "big-pickle", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	if _, err := executeStream(pluginapi.ExecutorRequest{Model: "big-pickle", Payload: body, StorageJSON: testCredential()}, "cb", "stream-out", sender); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("the stream pump never returned")
	}
	joined := string(bytesJoin(emitted))
	if !strings.Contains(joined, "cut short") || !strings.Contains(joined, "\"finish_reason\":\"stop\"") {
		t.Fatalf("cut-short ending = %q", joined)
	}
}

func TestExecuteStreamRejectsARefusalInsideAnOKStream(t *testing.T) {
	resetPluginState()
	sender := testDiscovery().sender()
	openHTTPStream = func(plan requestPlan, callbackID string) (hostStreamResponse, error) {
		return hostStreamResponse{StatusCode: 200, Headers: http.Header{"Content-Type": {"text/event-stream"}}, StreamID: "stream-1"}, nil
	}
	readStreamChunk = func(id string) (hostReadResponse, error) {
		return hostReadResponse{Payload: []byte("data: {\"error\":{\"message\":\"Free models work in OpenCode\",\"type\":\"FreeUsageLimitError\"}}\n\n"), Done: true}, nil
	}
	emitStreamFrame = func(id string, payload []byte) error { return nil }
	defer func() {
		openHTTPStream = openUpstreamStream
		readStreamChunk = readHostStream
		emitStreamFrame = emitStreamChunk
	}()
	body, _ := json.Marshal(map[string]any{"model": "big-pickle", "messages": []map[string]any{{"role": "user", "content": "hi"}}})
	_, err := executeStream(pluginapi.ExecutorRequest{Model: "big-pickle", Payload: body, StorageJSON: testCredential()}, "cb", "stream-out", sender)
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusTooManyRequests {
		t.Fatalf("err = %#v", err)
	}
}

func TestCountTokensIsNotInvented(t *testing.T) {
	_, err := countTokens()
	var perr *pluginabi.Error
	if !errors.As(err, &perr) || perr.HTTPStatus != http.StatusNotImplemented {
		t.Fatalf("err = %#v", err)
	}
}

func TestQuotaDescribesThePlanWithoutBuckets(t *testing.T) {
	resetPluginState()
	describe := quotaDescription()
	if describe.SupportedProviders[0] != credentialProvider || describe.SupportsReset {
		t.Fatalf("describe = %+v", describe)
	}
	quota, err := quotaFetch(pluginapi.QuotaFetchRequest{StorageJSON: testCredential()})
	if err != nil {
		t.Fatal(err)
	}
	if quota.Subscription == nil || quota.Subscription.Plan != authLabel || len(quota.Groups) != 0 {
		t.Fatalf("quota = %+v", quota)
	}
	if _, err := quotaReset(); err == nil {
		t.Fatal("a reset was offered")
	}
}

func TestVersionIsRaisedByOpenCodesUpdater(t *testing.T) {
	resetPluginState()
	sender := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		if strings.HasPrefix(req.URL, versionURL) {
			if !strings.Contains(req.URL, "current=2.2.5") {
				t.Fatalf("version URL = %s", req.URL)
			}
			return jsonReply(200, `{"version":"2.3.1"}`), nil
		}
		return jsonReply(404, `{}`), nil
	}}
	if got := currentVersion(sender); got != "2.3.1" {
		t.Fatalf("version = %q", got)
	}
	if sender.count() != 1 {
		t.Fatalf("version asked %d times", sender.count())
	}
	// A stale mirror that answers an older version is never taken.
	zen.mu.Lock()
	zen.versionAt = time.Time{}
	zen.mu.Unlock()
	stale := &fakeSender{reply: func(req pluginapi.HTTPRequest) (pluginapi.HTTPResponse, error) {
		return jsonReply(200, `{"version":"2.0.0"}`), nil
	}}
	if got := currentVersion(stale); got != "2.3.1" {
		t.Fatalf("version after a stale answer = %q", got)
	}
}

func TestConfigurationValidation(t *testing.T) {
	resetPluginState()
	valid := []string{
		"",
		"max_concurrency: 5",
		"opencode_version: 2.3.0",
		"exclude_models: [big-pickle]",
	}
	for _, raw := range valid {
		if err := configure([]byte(raw)); err != nil {
			t.Fatalf("%q: %v", raw, err)
		}
	}
	invalid := []string{
		"max_concurrency: 0",
		"max_concurrency: 101",
		"opencode_version: \"\"",
		"opencode_version: 2.2.5 ; rm -rf /",
	}
	for _, raw := range invalid {
		if err := configure([]byte(raw)); err == nil {
			t.Fatalf("%q was accepted", raw)
		}
	}
	if err := configure(nil); err != nil {
		t.Fatal(err)
	}
}

func toolNames(tools []any) map[string]bool {
	out := map[string]bool{}
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				out[name] = true
			}
		}
	}
	return out
}

func bytesJoin(parts [][]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
