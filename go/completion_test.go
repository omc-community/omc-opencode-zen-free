package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func completionPolicy(t *testing.T, names ...string) toolPolicy {
	t.Helper()
	chat := map[string]any{"messages": []any{}, "tools": []any{}}
	for _, name := range names {
		chat["tools"] = append(chat["tools"].([]any), toolDef(name))
	}
	policy, err := normalizeTools(chat)
	if err != nil {
		t.Fatal(err)
	}
	return policy
}

func TestAggregateStreamMapsAndDropsCalls(t *testing.T) {
	policy := completionPolicy(t, "ReportFindings", "ScheduleWakeup")
	body := "data: {\"id\":\"c1\",\"model\":\"big-pickle\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hello \"}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"findings_report\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"call_2\",\"type\":\"function\",\"function\":{\"name\":\"\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[],\"usage\":{\"total_tokens\":11}}\n\n" +
		"data: [DONE]\n\n"
	payload, err := aggregateStream([]byte(body), "big-pickle", policy)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name string `json:"name"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage map[string]any `json:"usage"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	message := out.Choices[0].Message
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "ReportFindings" {
		t.Fatalf("tool calls = %+v", message.ToolCalls)
	}
	if !strings.Contains(message.Content, "Hello") || !strings.Contains(message.Content, "without a name") {
		t.Fatalf("content = %q", message.Content)
	}
	if out.Choices[0].FinishReason != "tool_calls" {
		t.Fatalf("finish = %q", out.Choices[0].FinishReason)
	}
	if out.Usage["total_tokens"] != float64(11) {
		t.Fatalf("usage = %v", out.Usage)
	}
}

func TestAggregateStreamEndsAsAStopWhenEveryCallWasLeftOut(t *testing.T) {
	policy := completionPolicy(t, "Bash")
	body := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	payload, err := aggregateStream([]byte(body), "big-pickle", policy)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []any  `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(payload, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices[0].Message.ToolCalls) != 0 || out.Choices[0].FinishReason != "stop" {
		t.Fatalf("choice = %+v", out.Choices[0])
	}
	if !strings.Contains(out.Choices[0].Message.Content, "left out") {
		t.Fatalf("content = %q", out.Choices[0].Message.Content)
	}
}

func TestANonStreamingReplyCutShortFails(t *testing.T) {
	policy := completionPolicy(t, "Bash")
	body := "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"
	if _, err := aggregateStream([]byte(body), "big-pickle", policy); err == nil {
		t.Fatal("a reply without [DONE] was returned as a whole answer")
	}
}

func TestFrameOfReadsTheEventName(t *testing.T) {
	frame, ok := frameOf([]byte("event: error\ndata: boom\n\n"))
	if !ok || frame.Name != "error" || string(frame.Data) != "boom" {
		t.Fatalf("frame = %+v", frame)
	}
}

func TestStreamGuardHoldsToolCallsUntilFinish(t *testing.T) {
	guard := newStreamGuard(completionPolicy(t, "ReportFindings", "ScheduleWakeup"))
	frames, err := guard.apply([]byte(`{"id":"c1","choices":[{"index":0,"delta":{"content":"Hi"}}]}`))
	if err != nil || len(frames) != 1 {
		t.Fatalf("content frames = %v, %v", frames, err)
	}
	frames, err = guard.apply([]byte(`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"findings_report","arguments":"{\"a\""}}]}}]}`))
	if err != nil || len(frames) != 0 {
		t.Fatalf("fragment frames = %v, %v", frames, err)
	}
	frames, err = guard.apply([]byte(`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":":1}"}}]},"finish_reason":"tool_calls"}]}`))
	if err != nil || len(frames) != 1 {
		t.Fatalf("finish frames = %v, %v", frames, err)
	}
	if !strings.Contains(string(frames[0]), `"name":"ReportFindings"`) || !strings.Contains(string(frames[0]), `"arguments":"{\"a\":1}"`) {
		t.Fatalf("frame = %s", frames[0])
	}
}

func TestStreamGuardEndsAnUnfinishedCallAtDone(t *testing.T) {
	guard := newStreamGuard(completionPolicy(t, "Bash"))
	if _, err := guard.apply([]byte(`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{}"}}]}}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.apply([]byte(`[DONE]`)); err != nil {
		t.Fatal(err)
	}
	tail := guard.finishEvent()
	if tail == nil || !strings.Contains(string(tail), `"name":"Bash"`) {
		t.Fatalf("tail = %s", tail)
	}
}

func TestStreamGuardCutShort(t *testing.T) {
	guard := newStreamGuard(completionPolicy(t, "Bash"))
	if _, err := guard.apply([]byte(`{"id":"c1","choices":[{"index":0,"delta":{"content":"partial"}}]}`)); err != nil {
		t.Fatal(err)
	}
	frame, err := guard.cutShort()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(frame), "cut short") || !strings.Contains(string(frame), `"finish_reason":"stop"`) {
		t.Fatalf("frame = %s", frame)
	}
}

func TestStreamGuardCutShortFailsWithAPendingCall(t *testing.T) {
	guard := newStreamGuard(completionPolicy(t, "Bash"))
	if _, err := guard.apply([]byte(`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\""}}]}}]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := guard.cutShort(); err == nil {
		t.Fatal("a half-written tool call was ended as an answer")
	}
}

func TestStreamGuardRejectsARefusalFrame(t *testing.T) {
	guard := newStreamGuard(completionPolicy(t, "Bash"))
	_, err := guard.apply([]byte(`{"error":{"message":"Free models work in OpenCode","type":"FreeUsageLimitError"}}`))
	if err == nil {
		t.Fatal("a refusal frame was treated as an answer")
	}
}

func TestRefusalOfReadsBothShapes(t *testing.T) {
	for _, body := range []string{
		`{"error":{"message":"nope","type":"FreeTierError"}}`,
		`{"code":"14018","msg":"usage exceeds frequency limit"}`,
		`{"type":"error","error":{"message":"nope"}}`,
	} {
		if refusalOf([]byte(body)) == nil {
			t.Fatalf("%s was not read as a refusal", body)
		}
	}
	for _, body := range []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"code":"0","msg":"ok"}`,
		`[DONE]`,
	} {
		if refusalOf([]byte(body)) != nil {
			t.Fatalf("%s was read as a refusal", body)
		}
	}
}
