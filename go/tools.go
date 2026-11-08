package main

import (
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// OpenCode's own models call OpenCode's tool names (bash, todowrite, webfetch)
// whatever the agent declared. A call is given the agent's tool whose name
// differs only in case, or else only in case and separators (todowrite ->
// todo_write, webfetch -> web_fetch), when exactly one does. A name two of the
// agent's tools fold to is never guessed between.

// coreTools are OpenCode 2's own core tools, which Zen's free tier expects a
// request to carry. Each takes OpenCode's own first parameter. They are
// described as not available: a call of one the agent does not have reaches the
// agent like any other unknown tool.
var coreTools = []struct{ name, parameter string }{
	{"edit", "path"},
	{"glob", "pattern"},
	{"grep", "pattern"},
	{"read", "path"},
	{"shell", "command"},
}

// toolPolicy is what the agent declared and how a refused name was mapped.
type toolPolicy struct {
	// declared is the set of names the agent itself offered. The injected core
	// tools are not part of it: they are stand-ins, not the agent's tools.
	declared map[string]bool
	// renamed maps a name sent to Zen back to the agent's own name.
	renamed map[string]string
}

// normalizeTools makes the request the one Zen serves: the five core tools are
// present, and a name Zen refuses is sent under another name. It answers the
// policy a reply is read back through.
func normalizeTools(chat map[string]any) (toolPolicy, error) {
	if raw, present := chat["tools"]; present && raw != nil {
		if _, ok := raw.([]any); !ok {
			return toolPolicy{}, errors.New("tools must be an array")
		}
	}
	tools, _ := chat["tools"].([]any)
	present := make(map[string]bool, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		fn, ok := tool["function"].(map[string]any)
		if !ok {
			continue
		}
		if name, ok := fn["name"].(string); ok && name != "" {
			present[name] = true
		}
	}
	declared := make(map[string]bool, len(present))
	for name := range present {
		declared[name] = true
	}
	if functions, ok := chat["functions"].([]any); ok {
		for _, raw := range functions {
			if fn, ok := raw.(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && name != "" {
					declared[name] = true
				}
			}
		}
	}
	for _, core := range coreTools {
		if present[core.name] {
			continue
		}
		tools = append(tools, coreToolDefinition(core.name, core.parameter))
	}
	chat["tools"] = tools
	return toolPolicy{declared: declared, renamed: renameRefused(chat, declared)}, nil
}

func coreToolDefinition(name, parameter string) map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": "Not available. Never call this tool.",
			"parameters": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{parameter: map[string]any{"type": "string"}},
				"required":             []string{parameter},
				"additionalProperties": false,
			},
			"strict": false,
		},
	}
}

// refusedTogether are the tool names Zen's free tier refuses only when every
// name of the rule is offered, and the name each goes under instead. Each was
// found by bisecting a real agent's request against Zen.
var refusedTogether = []struct {
	names []string
	as    map[string]string
}{
	{
		names: []string{"ReportFindings", "ScheduleWakeup"},
		as:    map[string]string{"ReportFindings": "findings_report", "ScheduleWakeup": "wakeup_schedule"},
	},
	{
		names: []string{"subagent_fork"},
		as:    map[string]string{"subagent_fork": "conversation_delegate"},
	},
	{
		names: []string{"read_image", "working_directory"},
		as:    map[string]string{"working_directory": "workspace_directory"},
	},
	{
		names: []string{"CreateWorkflow", "GetWorkflowRun"},
		as:    map[string]string{"CreateWorkflow": "workflow_create", "GetWorkflowRun": "workflow_inspect"},
	},
	{
		names: []string{"AmendWorkflow", "FillWorkflowHole"},
		as:    map[string]string{"AmendWorkflow": "workflow_amend", "FillWorkflowHole": "workflow_answer"},
	},
}

// refusedWords are refused wherever they stand in a name, in any case. The
// underscore in run_code is needed: exec_program, runCode, run-code and
// code_run were served.
var refusedWords = []struct {
	word *regexp.Regexp
	as   string
}{
	{regexp.MustCompile(`(?i)terminal`), "console"},
	{regexp.MustCompile(`(?i)run_code`), "exec_program"},
}

func inCaseOf(found, word string) string {
	if found == strings.ToUpper(found) {
		return strings.ToUpper(word)
	}
	if first := found[0]; first >= 'A' && first <= 'Z' {
		parts := strings.Split(word, "_")
		for i, part := range parts {
			parts[i] = strings.ToUpper(part[:1]) + part[1:]
		}
		return strings.Join(parts, "_")
	}
	return word
}

// servedName is the name a refused tool goes to Zen under, or "" when the name
// is not refused.
func servedName(name string, taken map[string]bool) string {
	for _, rule := range refusedTogether {
		alias, ok := rule.as[name]
		if !ok {
			continue
		}
		every := true
		for _, other := range rule.names {
			if !taken[other] {
				every = false
				break
			}
		}
		if every {
			return alias
		}
	}
	served := name
	for _, rule := range refusedWords {
		served = rule.word.ReplaceAllStringFunc(served, func(found string) string {
			return inCaseOf(found, rule.as)
		})
	}
	if served != name {
		return served
	}
	return ""
}

// renameRefused gives each refused tool its served name and answers the served
// names, each to the agent's own. Past calls and tool_choice are renamed too,
// so a tool loop stays one conversation the model can follow.
func renameRefused(chat map[string]any, taken map[string]bool) map[string]string {
	names := make([]string, 0, len(taken))
	for name := range taken {
		names = append(names, name)
	}
	sort.Strings(names)
	served := make(map[string]string, len(names))
	renamed := make(map[string]string, len(names))
	for _, name := range names {
		wanted := servedName(name, taken)
		if wanted == "" {
			continue
		}
		alias := wanted
		for i := 2; taken[alias] || renamed[alias] != ""; i++ {
			alias = wanted + "_" + strconv.Itoa(i)
		}
		served[name] = alias
		renamed[alias] = name
	}
	if len(served) == 0 {
		return renamed
	}
	rename := func(name string) string {
		if alias, ok := served[name]; ok {
			return alias
		}
		return name
	}
	if tools, ok := chat["tools"].([]any); ok {
		for _, raw := range tools {
			tool, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := tool["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && served[name] != "" {
					fn["name"] = served[name]
				}
			}
		}
	}
	if functions, ok := chat["functions"].([]any); ok {
		for _, raw := range functions {
			if fn, ok := raw.(map[string]any); ok {
				if name, ok := fn["name"].(string); ok && served[name] != "" {
					fn["name"] = served[name]
				}
			}
		}
	}
	if messages, ok := chat["messages"].([]any); ok {
		for _, raw := range messages {
			message, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, rawCall := range calls {
					call, ok := rawCall.(map[string]any)
					if !ok {
						continue
					}
					if fn, ok := call["function"].(map[string]any); ok {
						if name, ok := fn["name"].(string); ok {
							fn["name"] = rename(name)
						}
					}
				}
			}
			if call, ok := message["function_call"].(map[string]any); ok {
				if name, ok := call["name"].(string); ok {
					call["name"] = rename(name)
				}
			}
		}
	}
	if choice, ok := chat["tool_choice"].(map[string]any); ok {
		if fn, ok := choice["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				fn["name"] = rename(name)
			}
		} else if name, ok := choice["name"].(string); ok {
			choice["name"] = rename(name)
		}
	}
	if call, ok := chat["function_call"].(map[string]any); ok {
		if name, ok := call["name"].(string); ok {
			call["name"] = rename(name)
		}
	}
	return renamed
}

// separators are what a name is folded by when case alone does not match.
var separators = strings.NewReplacer(" ", "", "_", "", ".", "", "-", "")

// foldsTo maps each folded name to the one declared name it folds to. A folded
// name two declared names share is ambiguous and maps to none.
func foldsTo(declared map[string]bool, key func(string) string) (map[string]string, map[string]bool) {
	folded := map[string]string{}
	ambiguous := map[string]bool{}
	for name := range declared {
		k := key(name)
		// A name of separators only folds to nothing, and would be handed any
		// other such call ("_" for "--"): it matches only itself.
		if k == "" {
			continue
		}
		if _, ok := folded[k]; ok {
			ambiguous[k] = true
			continue
		}
		folded[k] = name
	}
	for k := range ambiguous {
		delete(folded, k)
	}
	return folded, ambiguous
}

// guard answers the agent's name for a call: the agent's spelling when one
// folds to it, else the name as the model wrote it. A call of a tool the agent
// doesn't have goes to the agent as called, like any other provider's: the
// agent answers it with its own unknown-tool error, and the model corrects the
// name and goes on. Only a call with no name is left out.
func (p toolPolicy) guard(name string) (string, bool) {
	if name == "" {
		return "", false
	}
	if own, ok := p.renamed[name]; ok {
		return own, true
	}
	if p.declared[name] {
		return name, true
	}
	byCase, ambiguousCase := foldsTo(p.declared, strings.ToLower)
	byShape, ambiguousShape := foldsTo(p.declared, func(s string) string {
		return separators.Replace(strings.ToLower(s))
	})
	lower := strings.ToLower(name)
	if own, ok := byCase[lower]; ok {
		return own, true
	}
	if ambiguousCase[lower] {
		return name, true
	}
	shape := separators.Replace(lower)
	if own, ok := byShape[shape]; ok {
		return own, true
	}
	if ambiguousShape[shape] {
		return name, true
	}
	return name, true
}

// droppedNote is what a reply says in place of a call with no name, where the
// agent's user and the model's next turn both read it.
func droppedNote(name string) string {
	if name != "" {
		if len(name) > 128 {
			name = name[:128]
		}
		quoted, _ := json.Marshal(name)
		return "(The model called a tool this agent doesn't have: " + string(quoted) + ". The call was left out.)"
	}
	return "(The model called a tool without a name. The call was left out.)"
}

// appendNotes adds notes to a message's content, whatever shape it is.
func appendNotes(content any, notes []string) any {
	if len(notes) == 0 {
		return content
	}
	text := strings.Join(notes, "\n\n")
	switch c := content.(type) {
	case string:
		if c == "" {
			return text
		}
		return c + "\n\n" + text
	case []any:
		return append(append([]any(nil), c...), map[string]any{"type": "text", "text": text})
	case nil:
		return text
	default:
		return text
	}
}
