package main

import (
	"strings"
	"testing"
)

func TestNormalizeToolsAddsTheCoreTools(t *testing.T) {
	chat := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
		},
	}
	policy, err := normalizeTools(chat)
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(chat["tools"].([]any))
	for _, core := range []string{"edit", "glob", "grep", "read", "shell"} {
		if !names[core] {
			t.Fatalf("core tool %q missing from %v", core, names)
		}
	}
	if !names["Bash"] {
		t.Fatalf("the agent's own tool was lost: %v", names)
	}
	if policy.declared["edit"] {
		t.Fatal("an injected core tool was treated as the agent's own")
	}
	if !policy.declared["Bash"] {
		t.Fatal("the agent's tool was not declared")
	}
}

func TestNormalizeToolsRejectsANonArray(t *testing.T) {
	chat := map[string]any{"messages": []any{}, "tools": map[string]any{}}
	if _, err := normalizeTools(chat); err == nil {
		t.Fatal("a non-array tools field was accepted")
	}
}

func TestRefusedNamesAreRenamedEverywhere(t *testing.T) {
	chat := map[string]any{
		"tools": []any{
			toolDef("terminal_open"),
			toolDef("ReportFindings"),
			toolDef("ScheduleWakeup"),
			toolDef("run_code"),
			toolDef("Bash"),
		},
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "terminal_open", "arguments": "{}"}},
			}},
			map[string]any{"role": "user", "content": "go on"},
		},
		"tool_choice": map[string]any{"type": "function", "function": map[string]any{"name": "run_code"}},
	}
	policy, err := normalizeTools(chat)
	if err != nil {
		t.Fatal(err)
	}
	names := toolNames(chat["tools"].([]any))
	for _, want := range []string{"console_open", "findings_report", "wakeup_schedule", "exec_program", "Bash"} {
		if !names[want] {
			t.Fatalf("%q missing from %v", want, names)
		}
	}
	if names["terminal_open"] || names["ReportFindings"] || names["run_code"] {
		t.Fatalf("a refused name was left in place: %v", names)
	}
	call := chat["messages"].([]any)[0].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if name := call["function"].(map[string]any)["name"]; name != "console_open" {
		t.Fatalf("past call = %v", name)
	}
	choice := chat["tool_choice"].(map[string]any)["function"].(map[string]any)
	if name := choice["name"]; name != "exec_program" {
		t.Fatalf("tool_choice = %v", name)
	}
	if policy.renamed["console_open"] != "terminal_open" || policy.renamed["findings_report"] != "ReportFindings" {
		t.Fatalf("renamed = %v", policy.renamed)
	}
}

func TestASingleRefusedRuleNameStaysAsItIs(t *testing.T) {
	chat := map[string]any{"tools": []any{toolDef("ReportFindings")}, "messages": []any{}}
	if _, err := normalizeTools(chat); err != nil {
		t.Fatal(err)
	}
	names := toolNames(chat["tools"].([]any))
	if !names["ReportFindings"] || names["findings_report"] {
		t.Fatalf("a rule fired without every name it lists: %v", names)
	}
}

func TestGuardMapsServedAndFoldedNames(t *testing.T) {
	policy := toolPolicy{
		declared: map[string]bool{"Bash": true, "todo_write": true, "webfetch": true},
		renamed:  map[string]string{"console_open": "terminal_open"},
	}
	cases := map[string]string{
		"console_open": "terminal_open",
		"bash":         "Bash",
		"todowrite":    "todo_write",
		"web_fetch":    "webfetch",
		"Bash":         "Bash",
		"pwsh":         "pwsh",
	}
	for name, want := range cases {
		got, keep := policy.guard(name)
		if !keep || got != want {
			t.Fatalf("%q: got %q %v, want %q", name, got, keep, want)
		}
	}
	if _, keep := policy.guard(""); keep {
		t.Fatal("a call with no name was kept")
	}
}

func TestAnAmbiguousFoldIsNotGuessed(t *testing.T) {
	policy := toolPolicy{declared: map[string]bool{"Bash": true, "bash": true}}
	if got, _ := policy.guard("bash"); got != "bash" {
		t.Fatalf("got %q", got)
	}
}

func TestDroppedNoteNamesTheCall(t *testing.T) {
	if note := droppedNote(""); !strings.Contains(note, "without a name") {
		t.Fatalf("note = %q", note)
	}
	if note := droppedNote("weird_tool"); !strings.Contains(note, `"weird_tool"`) {
		t.Fatalf("note = %q", note)
	}
}

func toolDef(name string) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{"name": name}}
}
