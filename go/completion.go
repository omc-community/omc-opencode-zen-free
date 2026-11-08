package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
)

// sseFrame is one complete server-sent event: the bytes as Zen sent them, and
// its data payload on its own.
type sseFrame struct {
	Raw  []byte
	Name string
	Data []byte
}

// sseBuffer collects a byte stream and cuts it into complete events, so a chunk
// boundary that falls inside an event never reaches a parser or a client.
type sseBuffer struct{ buf []byte }

func (b *sseBuffer) push(chunk []byte) []sseFrame {
	b.buf = append(b.buf, chunk...)
	var out []sseFrame
	for {
		end, sep := 0, 0
		if i := bytes.Index(b.buf, []byte("\r\n\r\n")); i >= 0 {
			end, sep = i, 4
		}
		if i := bytes.Index(b.buf, []byte("\n\n")); i >= 0 && (sep == 0 || i < end) {
			end, sep = i, 2
		}
		if sep == 0 {
			return out
		}
		raw := b.buf[:end+sep]
		if f, ok := frameOf(raw); ok {
			out = append(out, f)
		}
		b.buf = b.buf[end+sep:]
	}
}

func (b *sseBuffer) flush() []sseFrame {
	if len(bytes.TrimSpace(b.buf)) == 0 {
		b.buf = nil
		return nil
	}
	raw := b.buf
	b.buf = nil
	if f, ok := frameOf(raw); ok {
		return []sseFrame{f}
	}
	return nil
}

// frameOf reads one event's data lines. An event that carries none (a comment, a
// keep-alive) is not a frame.
func frameOf(raw []byte) (sseFrame, bool) {
	var data [][]byte
	name := ""
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if bytes.HasPrefix(line, []byte("event:")) {
			name = string(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("event:"))))
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		value := bytes.TrimPrefix(line, []byte("data:"))
		value = bytes.TrimPrefix(value, []byte(" "))
		data = append(data, value)
	}
	if len(data) == 0 {
		return sseFrame{}, false
	}
	return sseFrame{Raw: append([]byte(nil), raw...), Name: name, Data: bytes.Join(data, []byte("\n"))}, true
}

func isDone(data []byte) bool { return strings.TrimSpace(string(data)) == "[DONE]" }

// chatChunk is one streamed chat chunk, in the shape Zen sends.
type chatChunk struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created"`
	Model   string          `json:"model"`
	Choices []chatChoice    `json:"choices"`
	Usage   json.RawMessage `json:"usage"`
}

type chatChoice struct {
	Index        int        `json:"index"`
	Delta        *chatDelta `json:"delta"`
	Message      *chatDelta `json:"message"`
	FinishReason *string    `json:"finish_reason"`
}

type chatDelta struct {
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	ReasoningContent string          `json:"reasoning_content"`
	Reasoning        string          `json:"reasoning"`
	ToolCalls        []toolCallDelta `json:"tool_calls"`
	FunctionCall     *functionDelta  `json:"function_call"`
}

type toolCallDelta struct {
	Index    int           `json:"index"`
	ID       string        `json:"id"`
	Type     string        `json:"type"`
	Function functionDelta `json:"function"`
}

type functionDelta struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// refusalOf reports the refusal a stream event carries, if any. A chunk with a
// choice is an answer, not a refusal.
func refusalOf(data []byte) error {
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
	if json.Unmarshal(data, &payload) != nil {
		return nil
	}
	message := ""
	code := ""
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
		if strings.TrimSpace(payload.Error.Message) != "" {
			return &zenError{Status: 0, Code: code, Message: strings.TrimSpace(payload.Error.Message)}
		}
		if payload.Error.Type != "" || payload.Error.Code != nil {
			return &zenError{Status: 0, Code: code, Message: payload.Error.Type}
		}
	}
	if code != "" && code != "0" {
		if message == "" {
			message = "OpenCode Zen refused the request"
		}
		return &zenError{Status: 0, Code: code, Message: message}
	}
	if payload.Type == "error" && message != "" {
		return &zenError{Status: 0, Message: message}
	}
	return nil
}

// toolAcc is one tool call being assembled across chunks.
type toolAcc struct {
	ID        string
	Type      string
	Name      string
	Arguments strings.Builder
}

func (a *toolAcc) merge(fragment toolCallDelta) {
	if fragment.ID != "" {
		a.ID = fragment.ID
	}
	if fragment.Type != "" {
		a.Type = fragment.Type
	}
	if fragment.Function.Name != "" {
		a.Name += fragment.Function.Name
	}
	if fragment.Function.Arguments != "" {
		a.Arguments.WriteString(fragment.Function.Arguments)
	}
}

// choiceAcc assembles one choice of a non-streaming answer from the stream Zen
// sends.
type choiceAcc struct {
	index     int
	role      string
	content   strings.Builder
	reasoning strings.Builder
	calls     map[int]*toolAcc
	order     []int
	legacy    *toolAcc
	finish    string
	answered  bool
}

func newChoiceAcc(index int) *choiceAcc {
	return &choiceAcc{index: index, calls: map[int]*toolAcc{}}
}

// apply adds one choice's delta. A message-shaped chunk (a whole answer inside
// an event) is read the same way.
func (c *choiceAcc) apply(delta *chatDelta) {
	if delta == nil {
		return
	}
	if delta.Role != "" {
		c.role = delta.Role
	}
	if text := contentText(delta.Content); text != "" {
		c.content.WriteString(text)
		c.answered = true
	}
	if delta.ReasoningContent != "" {
		c.reasoning.WriteString(delta.ReasoningContent)
		c.answered = true
	} else if delta.Reasoning != "" {
		c.reasoning.WriteString(delta.Reasoning)
		c.answered = true
	}
	for _, fragment := range delta.ToolCalls {
		acc := c.calls[fragment.Index]
		if acc == nil {
			acc = &toolAcc{}
			c.calls[fragment.Index] = acc
			c.order = append(c.order, fragment.Index)
		}
		acc.merge(fragment)
		c.answered = true
	}
	if delta.FunctionCall != nil {
		if c.legacy == nil {
			c.legacy = &toolAcc{}
		}
		if delta.FunctionCall.Name != "" {
			c.legacy.Name += delta.FunctionCall.Name
		}
		if delta.FunctionCall.Arguments != "" {
			c.legacy.Arguments.WriteString(delta.FunctionCall.Arguments)
		}
		c.answered = true
	}
}

// contentText reads a chunk's content: a string, or the text of a content array.
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []map[string]any
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			if text, ok := p["text"].(string); ok {
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

// completionBuilder assembles Zen's chunks into one OpenAI completion. Zen
// streams every chat, so a client that asked for one answer gets the stream read
// out for it.
type completionBuilder struct {
	id      string
	model   string
	created int64
	usage   json.RawMessage
	choices map[int]*choiceAcc
	order   []int
	done    bool
}

func newCompletionBuilder(model string) *completionBuilder {
	return &completionBuilder{model: model, choices: map[int]*choiceAcc{}}
}

// feed adds one event's payload. A refusal is returned as an error and ends the
// answer; the partial text stays for the caller to decide on.
func (b *completionBuilder) feed(data []byte) error {
	if isDone(data) {
		b.done = true
		return nil
	}
	if err := refusalOf(data); err != nil {
		return err
	}
	var chunk chatChunk
	if json.Unmarshal(data, &chunk) != nil {
		return nil
	}
	if chunk.ID != "" {
		b.id = chunk.ID
	}
	if chunk.Model != "" {
		b.model = chunk.Model
	}
	if chunk.Created > 0 {
		b.created = chunk.Created
	}
	if len(chunk.Usage) > 0 && string(chunk.Usage) != "null" {
		b.usage = append(json.RawMessage(nil), chunk.Usage...)
	}
	for _, choice := range chunk.Choices {
		acc := b.choices[choice.Index]
		if acc == nil {
			acc = newChoiceAcc(choice.Index)
			b.choices[choice.Index] = acc
			b.order = append(b.order, choice.Index)
		}
		if choice.Delta != nil {
			acc.apply(choice.Delta)
		} else if choice.Message != nil {
			acc.apply(choice.Message)
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			acc.finish = *choice.FinishReason
		}
	}
	return nil
}

func (b *completionBuilder) saidSomething() bool {
	for _, acc := range b.choices {
		if acc.answered {
			return true
		}
	}
	return false
}

// completion is the one answer the chunks add up to, with the tool calls mapped
// back to the agent's own names.
func (b *completionBuilder) completion(policy toolPolicy) ([]byte, error) {
	if !b.saidSomething() {
		return nil, errNoAnswer
	}
	if !b.done {
		return nil, errIncompleteStream
	}
	choices := make([]any, 0, len(b.order))
	for _, index := range b.order {
		acc := b.choices[index]
		message := map[string]any{"role": firstNonEmpty(acc.role, "assistant")}
		if acc.content.Len() > 0 {
			message["content"] = acc.content.String()
		}
		if acc.reasoning.Len() > 0 {
			message["reasoning_content"] = acc.reasoning.String()
		}
		var notes []string
		var calls []any
		for _, callIndex := range acc.order {
			call := acc.calls[callIndex]
			if call == nil {
				continue
			}
			name, keep := policy.guard(call.Name)
			if !keep {
				notes = append(notes, droppedNote(call.Name))
				continue
			}
			id := call.ID
			if id == "" {
				id = "call_" + randomHex(6)
			}
			kind := firstNonEmpty(call.Type, "function")
			calls = append(calls, map[string]any{
				"id":   id,
				"type": kind,
				"function": map[string]any{
					"name":      name,
					"arguments": call.Arguments.String(),
				},
			})
		}
		if acc.legacy != nil {
			name, keep := policy.guard(acc.legacy.Name)
			if keep {
				message["function_call"] = map[string]any{"name": name, "arguments": acc.legacy.Arguments.String()}
			} else {
				notes = append(notes, droppedNote(acc.legacy.Name))
			}
		}
		if len(calls) > 0 {
			message["tool_calls"] = calls
			if message["content"] == nil {
				message["content"] = nil
			}
		}
		if len(notes) > 0 {
			message["content"] = appendNotes(message["content"], notes)
		}
		finish := acc.finish
		if finish == "" {
			if len(calls) > 0 || message["function_call"] != nil {
				finish = "tool_calls"
			} else {
				finish = "stop"
			}
		}
		if callFinish(finish) && len(calls) == 0 && message["function_call"] == nil {
			finish = "stop"
		}
		choices = append(choices, map[string]any{"index": index, "message": message, "finish_reason": finish})
	}
	out := map[string]any{
		"id":      firstNonEmpty(b.id, "chatcmpl-"+randomHex(12)),
		"object":  "chat.completion",
		"created": b.created,
		"model":   b.model,
		"choices": choices,
	}
	if b.created == 0 {
		delete(out, "created")
	}
	if len(b.usage) > 0 {
		out["usage"] = b.usage
	}
	return json.Marshal(out)
}

func callFinish(reason string) bool {
	return reason == "tool_calls" || reason == "function_call"
}

var errNoAnswer = errors.New("OpenCode Zen ended without an answer")

// errIncompleteStream is a reply collapsed for a client that does not stream
// when Zen closed the stream before its [DONE]. Nothing reached the client yet,
// so failing lets the caller ask again instead of presenting half an answer.
var errIncompleteStream = errors.New("OpenCode Zen ended the stream without a completion marker")

// streamRefusal is a refusal carried inside an OK stream, kept with the frame it
// arrived in so the caller can read Zen's own code from it.
type streamRefusal struct {
	Data []byte
	Err  error
}

func (e *streamRefusal) Error() string { return e.Err.Error() }
func (e *streamRefusal) Unwrap() error { return e.Err }

// aggregateStream reads a whole chat stream and answers with one completion.
func aggregateStream(body []byte, model string, policy toolPolicy) ([]byte, error) {
	var buf sseBuffer
	builder := newCompletionBuilder(model)
	for _, frame := range append(buf.push(body), buf.flush()...) {
		if err := builder.feed(frame.Data); err != nil {
			return nil, &streamRefusal{Data: frame.Data, Err: err}
		}
	}
	return builder.completion(policy)
}

// cutShortNote is what a chat stream Zen closed before its [DONE] says, when it
// has said something and no tool call of it is unfinished.
const cutShortNote = "(The reply was cut short: Zen closed the stream before it finished. Ask again to go on.)"

// streamGuard rewrites a live chat stream: a tool call is held until its
// arguments are whole, then sent once under the agent's own name. Holding it
// also lets a call with no name be left out and said so in the reply's text.
type streamGuard struct {
	policy    toolPolicy
	id        string
	model     string
	created   int64
	choices   map[int]*streamChoice
	order     []int
	open      map[int]bool
	openOrder []int
	wrote     map[int]bool
	ended     map[int]bool
	said      bool
	finished  bool
}

type streamChoice struct {
	calls  map[int]*toolAcc
	order  []int
	legacy *toolAcc
}

func newStreamGuard(policy toolPolicy) *streamGuard {
	return &streamGuard{
		policy:  policy,
		choices: map[int]*streamChoice{},
		open:    map[int]bool{},
		wrote:   map[int]bool{},
		ended:   map[int]bool{},
	}
}

func (g *streamGuard) state(index int) *streamChoice {
	state := g.choices[index]
	if state == nil {
		state = &streamChoice{calls: map[int]*toolAcc{}}
		g.choices[index] = state
		g.order = append(g.order, index)
	}
	return state
}

// apply reads one event and answers the frames to send for it. The frames are
// bare JSON payloads: the host's OpenAI handler owns the SSE framing. A nil
// answer means the event is held (a tool call still being assembled) or has
// nothing to forward.
func (g *streamGuard) apply(data []byte) ([][]byte, error) {
	if isDone(data) {
		g.finished = true
		return nil, nil
	}
	if err := refusalOf(data); err != nil {
		return nil, err
	}
	var chunk chatChunk
	if json.Unmarshal(data, &chunk) != nil {
		// Not a chunk this guard understands: forward it untouched rather than
		// swallowing a frame the client may need.
		return [][]byte{data}, nil
	}
	if chunk.ID != "" {
		g.id = chunk.ID
	}
	if chunk.Model != "" {
		g.model = chunk.Model
	}
	if chunk.Created > 0 {
		g.created = chunk.Created
	}
	if len(chunk.Choices) == 0 {
		return [][]byte{data}, nil
	}
	changed := false
	kept := make([]chatChoice, 0, len(chunk.Choices))
	for _, choice := range chunk.Choices {
		delta := choice.Delta
		if delta == nil {
			delta = choice.Message
		}
		if delta != nil {
			if text := contentText(delta.Content); text != "" {
				g.said = true
				g.wrote[choice.Index] = true
			}
			if delta.ReasoningContent != "" || delta.Reasoning != "" {
				g.said = true
			}
			if len(delta.ToolCalls) > 0 || delta.FunctionCall != nil {
				changed = true
				state := g.state(choice.Index)
				for _, fragment := range delta.ToolCalls {
					acc := state.calls[fragment.Index]
					if acc == nil {
						acc = &toolAcc{}
						state.calls[fragment.Index] = acc
						state.order = append(state.order, fragment.Index)
					}
					acc.merge(fragment)
				}
				if delta.FunctionCall != nil {
					if state.legacy == nil {
						state.legacy = &toolAcc{}
					}
					state.legacy.Name += delta.FunctionCall.Name
					state.legacy.Arguments.WriteString(delta.FunctionCall.Arguments)
				}
				delta.ToolCalls = nil
				delta.FunctionCall = nil
			}
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			changed = true
			state := g.choices[choice.Index]
			if state != nil {
				delta = g.complete(state, choice.Index, delta)
				choice.Delta = delta
				choice.Message = nil
				delete(g.choices, choice.Index)
			}
			delete(g.open, choice.Index)
			if callFinish(*choice.FinishReason) && g.ended[choice.Index] {
				stop := "stop"
				choice.FinishReason = &stop
			}
		} else if !g.open[choice.Index] {
			g.open[choice.Index] = true
			g.openOrder = append(g.openOrder, choice.Index)
		}
		if choice.FinishReason == nil && deltaEmpty(delta) {
			continue
		}
		kept = append(kept, choice)
	}
	if !changed {
		return [][]byte{data}, nil
	}
	chunk.Choices = kept
	if len(chunk.Choices) == 0 && len(chunk.Usage) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return [][]byte{raw}, nil
}

// complete turns a choice's buffered calls into the one delta that carries them,
// mapped back to the agent's names.
func (g *streamGuard) complete(state *streamChoice, index int, delta *chatDelta) *chatDelta {
	out := &chatDelta{}
	if delta != nil {
		out.Role = delta.Role
		out.Content = delta.Content
		out.ReasoningContent = delta.ReasoningContent
		out.Reasoning = delta.Reasoning
	}
	var notes []string
	var calls []toolCallDelta
	for i, callIndex := range state.order {
		call := state.calls[callIndex]
		if call == nil {
			continue
		}
		name, keep := g.policy.guard(call.Name)
		if !keep {
			notes = append(notes, droppedNote(call.Name))
			continue
		}
		calls = append(calls, toolCallDelta{
			Index: i,
			ID:    call.ID,
			Type:  firstNonEmpty(call.Type, "function"),
			Function: functionDelta{
				Name:      name,
				Arguments: call.Arguments.String(),
			},
		})
	}
	if len(calls) > 0 {
		out.ToolCalls = calls
	}
	if state.legacy != nil {
		name, keep := g.policy.guard(state.legacy.Name)
		if keep {
			out.FunctionCall = &functionDelta{Name: name, Arguments: state.legacy.Arguments.String()}
		} else {
			notes = append(notes, droppedNote(state.legacy.Name))
		}
	}
	if len(notes) > 0 {
		text := strings.Join(notes, "\n\n")
		if existing := contentText(out.Content); existing != "" {
			out.Content = json.RawMessage(strconv.Quote(existing + "\n\n" + text))
		} else {
			prefix := ""
			if g.wrote[index] {
				prefix = "\n\n"
			}
			out.Content = json.RawMessage(strconv.Quote(prefix + text))
		}
		if len(calls) == 0 && out.FunctionCall == nil {
			g.ended[index] = true
		}
	}
	return out
}

// deltaEmpty reports a delta that carries nothing a client would read.
func deltaEmpty(delta *chatDelta) bool {
	if delta == nil {
		return true
	}
	return delta.Role == "" && len(delta.Content) == 0 && delta.ReasoningContent == "" &&
		delta.Reasoning == "" && len(delta.ToolCalls) == 0 && delta.FunctionCall == nil
}

// finishEvent is the frame that carries calls still buffered when the stream's
// [DONE] arrives without a finish_reason for them.
func (g *streamGuard) finishEvent() []byte {
	if len(g.choices) == 0 {
		return nil
	}
	chunk := chatChunk{
		ID:      firstNonEmpty(g.id, "chatcmpl-"+randomHex(12)),
		Object:  "chat.completion.chunk",
		Created: g.created,
		Model:   g.model,
	}
	for _, index := range g.order {
		state := g.choices[index]
		if state == nil {
			continue
		}
		chunk.Choices = append(chunk.Choices, chatChoice{Index: index, Delta: g.complete(state, index, nil)})
	}
	g.choices = map[int]*streamChoice{}
	if len(chunk.Choices) == 0 {
		return nil
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return nil
	}
	return raw
}

// cutShort is what a stream Zen closed before [DONE] ends as: an ordinary stop
// that says it was cut short, when the reply said something and no tool call is
// unfinished. A reply that said nothing, or left a call unfinished, cannot be
// ended that way and is an error instead.
func (g *streamGuard) cutShort() ([]byte, error) {
	if !g.said || len(g.choices) > 0 {
		return nil, errors.New("OpenCode Zen ended the stream without a completion marker")
	}
	chunk := chatChunk{
		ID:      firstNonEmpty(g.id, "chatcmpl-"+randomHex(12)),
		Object:  "chat.completion.chunk",
		Created: g.created,
		Model:   g.model,
	}
	for _, index := range g.openOrder {
		if !g.open[index] {
			continue
		}
		content := cutShortNote
		if g.wrote[index] {
			content = "\n\n" + content
		}
		stop := "stop"
		chunk.Choices = append(chunk.Choices, chatChoice{
			Index:        index,
			Delta:        &chatDelta{Content: json.RawMessage(strconv.Quote(content))},
			FinishReason: &stop,
		})
	}
	if len(chunk.Choices) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(chunk)
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// pending reports a tool call still being assembled.
func (g *streamGuard) pending() bool { return len(g.choices) > 0 }
