package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// credentialGate bounds simultaneous requests per credential, as Zen's shared
// free tier expects.
type credentialGate struct {
	cond   *sync.Cond
	active int
}

var gateMu sync.Mutex
var gates = map[string]*credentialGate{}

func acquire(id string) func() {
	gateMu.Lock()
	g := gates[id]
	if g == nil {
		g = &credentialGate{cond: sync.NewCond(&gateMu)}
		gates[id] = g
	}
	for g.active >= getConfig().MaxConcurrency {
		g.cond.Wait()
	}
	g.active++
	gateMu.Unlock()
	return func() { gateMu.Lock(); g.active--; g.cond.Broadcast(); gateMu.Unlock() }
}

type requestPlan struct {
	HTTP       pluginapi.HTTPRequest
	Chat       map[string]any
	Model      zenModel
	Policy     toolPolicy
	Credential credential
}

// prepareRequest turns an OpenAI chat completion into the request OpenCode 2
// sends Zen: the five core tools, OpenCode's environment block, its session and
// project ids, and its headers in its own order.
func prepareRequest(req pluginapi.ExecutorRequest, sender httpSender) (requestPlan, error) {
	var plan requestPlan
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return plan, pluginabi.NewError("invalid_auth", "invalid OpenCode Zen Free credential", 401)
	}
	model := trimModelPrefix(strings.TrimSpace(req.Model))
	if model == "" {
		return plan, pluginabi.NewError("invalid_request", "model is required", 400)
	}
	chat, err := parseChat(req.Payload)
	if err != nil {
		return plan, pluginabi.NewError("invalid_request", err.Error(), 400)
	}
	m, ok := findModel(sender, model)
	if !ok {
		return plan, pluginabi.NewError("unknown_model", unknownModelMessage(model), 400)
	}
	policy, err := normalizeTools(chat)
	if err != nil {
		return plan, pluginabi.NewError("invalid_request", err.Error(), 400)
	}
	// The ids are read from the agent's own prompt before OpenCode's block is
	// added, so a block this plugin wrote never becomes the agent's own answer.
	session := sessionID(req.Headers, chat)
	project := projectID(req.Headers, chat)
	addEnvironment(chat)
	chat["stream"] = true
	options, _ := chat["stream_options"].(map[string]any)
	if options == nil {
		options = map[string]any{}
	}
	options["include_usage"] = true
	chat["stream_options"] = options
	chat["model"] = m.Key
	body, err := json.Marshal(chat)
	if err != nil {
		return plan, pluginabi.NewError("invalid_request", "invalid chat completion request", 400)
	}
	version := currentVersion(sender)
	b3, traceparent := traceHeaders(session)
	headers := http.Header{
		"Authorization":         {"Bearer " + c.Access},
		"Content-Type":          {"application/json"},
		"User-Agent":            {userAgent(version)},
		"B3":                    {b3},
		"Traceparent":           {traceparent},
		"X-Opencode-Client":     {"cli"},
		"X-Opencode-Project":    {project},
		"X-Opencode-Session":    {session},
		"X-Opencode-Session-Id": {session},
		"X-Session-Affinity":    {session},
		"X-Session-Id":          {session},
	}
	return requestPlan{
		HTTP:       pluginapi.HTTPRequest{Method: "POST", URL: zenChatBase + zenChatPath, Headers: headers, Body: body},
		Chat:       chat,
		Model:      m,
		Policy:     policy,
		Credential: c,
	}, nil
}

// unknownModelMessage names the free models a client could ask for instead.
func unknownModelMessage(model string) string {
	free := make([]string, 0, 16)
	for _, m := range offerable(modelSlice(currentModels())) {
		free = append(free, strings.ToLower(m.Key))
	}
	sort.Strings(free)
	message := fmt.Sprintf("model %q is not available as an OpenCode Zen free model now", model)
	if len(free) > 0 {
		message += "; free models: " + strings.Join(free, ", ")
	}
	return message
}

// execute answers one non-streaming chat completion. Zen always streams, so the
// buffered body is read as SSE either way.
func execute(req pluginapi.ExecutorRequest, sender httpSender) (pluginapi.ExecutorResponse, error) {
	plan, err := prepareRequest(req, sender)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	release := acquire(req.AuthID)
	defer release()
	resp, err := sender.Do(plan.HTTP)
	if err != nil {
		return pluginapi.ExecutorResponse{}, transportFailure(err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return pluginapi.ExecutorResponse{}, classifyUpstream(resp.StatusCode, resp.Body)
	}
	if !bytes.Contains(resp.Body, []byte("data:")) {
		// A 200 that is not an event stream is Zen refusing in JSON.
		return pluginapi.ExecutorResponse{}, classifyUpstream(http.StatusBadGateway, resp.Body)
	}
	payload, err := aggregateStream(resp.Body, plan.Model.Key, plan.Policy)
	if err != nil {
		var refusal *streamRefusal
		if errors.As(err, &refusal) {
			return pluginapi.ExecutorResponse{}, classifyUpstream(0, refusal.Data)
		}
		return pluginapi.ExecutorResponse{}, pluginabi.NewError("upstream_error", err.Error(), 502)
	}
	return pluginapi.ExecutorResponse{
		Payload: payload,
		Headers: http.Header{"Content-Type": {"application/json"}},
		Metadata: map[string]any{
			"upstream_host":  "opencode-zen-free",
			"upstream_model": plan.Model.Key,
		},
	}, nil
}

type hostStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    http.Header `json:"headers"`
	StreamID   string      `json:"stream_id"`
}

type hostReadResponse struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

func readHostStream(id string) (hostReadResponse, error) {
	var r hostReadResponse
	if err := hostRPC("host.http.stream_read", map[string]any{"stream_id": id}, &r); err != nil {
		return r, &transportError{cause: err.Error()}
	}
	if r.Error != "" {
		return r, &transportError{cause: r.Error}
	}
	return r, nil
}

func closeHostStream(id string) {
	_ = hostRPC("host.http.stream_close", map[string]any{"stream_id": id}, nil)
}

func emitStreamChunk(id string, payload []byte) error {
	return hostRPC("host.stream.emit", map[string]any{"stream_id": id, "payload": payload}, nil)
}

func closeStream(id, message string) {
	_ = hostRPC("host.stream.close", map[string]any{"stream_id": id, "error": message}, nil)
}

// closeStreamWithError ends a stream with the structured error the host's router
// reads: without error_detail the host sees a statusless plugin failure and
// answers every mid-stream refusal as a 500, so a 403 or a 429 never reaches the
// routing and cooldown decisions.
func closeStreamWithError(id, message string, err error) {
	var detail *pluginabi.Error
	_ = errors.As(err, &detail)
	if detail == nil {
		closeStream(id, message)
		return
	}
	_ = hostRPC("host.stream.close", map[string]any{"stream_id": id, "error": message, "error_detail": detail}, nil)
}

// readWholeStream drains and closes a host HTTP stream, with a bound so a
// misbehaving upstream cannot exhaust memory.
func readWholeStream(id string) ([]byte, error) {
	defer closeHostStream(id)
	var out []byte
	for {
		r, err := readHostStream(id)
		if err != nil {
			return nil, err
		}
		out = append(out, r.Payload...)
		if len(out) > 1<<20 {
			return nil, errors.New("upstream error body too large")
		}
		if r.Done {
			return out, nil
		}
	}
}

// Streaming seams. Tests replace them to exercise the pump without a plugin host.
var (
	readStreamChunk     = readHostStream
	emitStreamFrame     = emitStreamChunk
	openHTTPStream      = openUpstreamStream
	closeHostStreamWith = closeStreamWithError
	// pumpFinished is nil in production; a test sets it to learn that a pump has
	// returned before it puts the seams above back.
	pumpFinished func(source string)
)

func openUpstreamStream(plan requestPlan, callbackID string) (hostStreamResponse, error) {
	var response hostStreamResponse
	err := hostRPC("host.http.do_stream", map[string]any{"host_callback_id": callbackID, "request": plan.HTTP}, &response)
	return response, err
}

// streamPump copies one Zen event stream into the host stream. A frame is
// forwarded as its bare data payload: the host's OpenAI handler adds the
// "data: " prefix, the blank line and the [DONE] marker itself.
type streamPump struct {
	source string
	target string
	buffer sseBuffer
	guard  *streamGuard
	// ended records that first saw the source's last chunk, so run does not
	// ask a stream that is already finished for more.
	ended bool
}

// pumpStart is what the pump found before the client was answered: the frames
// to send first, or the refusal that ended the stream.
type pumpStart struct {
	frames [][]byte
	err    error
}

// firstFrameWindow is how long a streaming request waits for Zen's first event
// before it answers the client. Zen sends nothing while a reasoning model
// thinks, so waiting for the first event before replying held the client's
// headers for as long as the model thought. Two seconds still catches a refusal
// written as the stream's first event.
var firstFrameWindow = 2 * time.Second

// first reads until the stream has produced its first frames, tells the caller
// what it found, and returns it. A refusal in those first bytes keeps its real
// status.
func (p *streamPump) first(ready chan<- pumpStart) pumpStart {
	var frames [][]byte
	for attempt := 0; attempt < 8 && len(frames) == 0; attempt++ {
		r, err := readStreamChunk(p.source)
		if err != nil {
			start := pumpStart{err: transportFailure(err)}
			ready <- start
			return start
		}
		next, err := p.consume(p.buffer.push(r.Payload))
		if err != nil {
			start := pumpStart{err: err}
			ready <- start
			return start
		}
		frames = append(frames, next...)
		if r.Done {
			p.ended = true
			next, err = p.consume(p.buffer.flush())
			if err != nil {
				start := pumpStart{err: err}
				ready <- start
				return start
			}
			frames = append(frames, next...)
			tail, err := p.end()
			if err != nil {
				start := pumpStart{err: err}
				ready <- start
				return start
			}
			frames = append(frames, tail...)
			break
		}
	}
	start := pumpStart{frames: frames}
	ready <- start
	return start
}

// consume applies the guard to complete events and answers the frames to send.
// A refusal carried in an event ends the stream with its own status.
func (p *streamPump) consume(events []sseFrame) ([][]byte, error) {
	var out [][]byte
	for _, event := range events {
		if event.Name == "error" {
			return out, classifyUpstream(0, event.Data)
		}
		if isDone(event.Data) {
			if tail := p.guard.finishEvent(); tail != nil {
				out = append(out, tail)
			}
			continue
		}
		frames, err := p.guard.apply(event.Data)
		if err != nil {
			return out, classifyUpstream(0, event.Data)
		}
		out = append(out, frames...)
	}
	return out, nil
}

// end is what a stream that stopped producing events ends as: the ordinary
// close when [DONE] was seen, the cut-short stop when the reply said something
// and no call is unfinished, and an error otherwise.
func (p *streamPump) end() ([][]byte, error) {
	if p.guard.finished {
		return nil, nil
	}
	if tail, err := p.guard.cutShort(); err != nil {
		return nil, pluginabi.NewError("upstream_error", "OpenCode Zen stream failed: "+err.Error(), 502)
	} else if tail != nil {
		return [][]byte{tail}, nil
	}
	return nil, nil
}

// deliver forwards complete frames, and reports false when the client's stream
// is gone.
func (p *streamPump) deliver(frames [][]byte) bool {
	for _, frame := range frames {
		if emitStreamFrame(p.target, frame) != nil {
			closeStream(p.target, "client stream closed")
			return false
		}
	}
	return true
}

func (p *streamPump) run() {
	for {
		r, err := readStreamChunk(p.source)
		if err != nil {
			closeHostStreamWith(p.target, streamFailureMessage(err), transportFailure(err))
			return
		}
		frames, err := p.consume(p.buffer.push(r.Payload))
		if !p.deliver(frames) {
			return
		}
		if err != nil {
			closeHostStreamWith(p.target, err.Error(), err)
			return
		}
		if r.Done {
			frames, err = p.consume(p.buffer.flush())
			if !p.deliver(frames) {
				return
			}
			if err != nil {
				closeHostStreamWith(p.target, err.Error(), err)
				return
			}
			tail, err := p.end()
			if err != nil {
				closeHostStreamWith(p.target, err.Error(), err)
				return
			}
			if !p.deliver(tail) {
				return
			}
			// The host writes the terminal marker itself.
			closeStream(p.target, "")
			return
		}
	}
}

// executeStream opens the upstream stream, rejects a refusal before any chunk
// reaches the client, and then pumps asynchronously.
func executeStream(req pluginapi.ExecutorRequest, callbackID, streamID string, sender httpSender) (map[string]any, error) {
	if streamID == "" {
		return nil, pluginabi.NewError("invalid_request", "stream ID required", 400)
	}
	plan, err := prepareRequest(req, sender)
	if err != nil {
		return nil, err
	}
	release := acquire(req.AuthID)
	response, err := openHTTPStream(plan, callbackID)
	if err != nil {
		release()
		return nil, transportFailure(err)
	}
	if response.StreamID == "" {
		release()
		return nil, pluginabi.NewError("transport_error", "OpenCode Zen upstream stream unavailable", 503)
	}
	contentType := strings.ToLower(response.Headers.Get("Content-Type"))
	if response.StatusCode < 200 || response.StatusCode >= 300 || strings.Contains(contentType, "application/json") {
		raw, readErr := readWholeStream(response.StreamID)
		release()
		if readErr != nil {
			return nil, pluginabi.NewError("transport_error", "OpenCode Zen upstream stream failed", 503)
		}
		return nil, classifyUpstream(response.StatusCode, raw)
	}
	pump := &streamPump{source: response.StreamID, target: streamID, guard: newStreamGuard(plan.Policy)}
	started := make(chan bool, 1)
	ready := make(chan pumpStart, 1)
	go func() {
		defer release()
		defer closeHostStream(pump.source)
		defer func() {
			if pumpFinished != nil {
				pumpFinished(pump.source)
			}
		}()
		start := pump.first(ready)
		answered := <-started
		if start.err != nil {
			if answered {
				closeHostStreamWith(pump.target, start.err.Error(), start.err)
			}
			return
		}
		if !pump.deliver(start.frames) {
			return
		}
		if pump.ended {
			closeStream(pump.target, "")
			return
		}
		pump.run()
	}()
	select {
	case start := <-ready:
		if start.err != nil {
			started <- false
			return nil, start.err
		}
		started <- true
	case <-time.After(firstFrameWindow):
		started <- true
	}
	return map[string]any{"headers": http.Header{"Content-Type": {"text/event-stream"}}}, nil
}

// streamFailureMessage says why a stream stopped, for the client's error frame.
func streamFailureMessage(err error) string {
	var e *pluginabi.Error
	if errors.As(transportFailure(err), &e) && e.HTTPStatus == 499 {
		return "the caller closed the request"
	}
	return "OpenCode Zen stream failed"
}
