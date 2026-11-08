package main

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// The model catalogue is what OpenCode itself reads: Zen's live list, the
// models.dev v2 catalog's metadata and OpenCode's own protocol table. The
// bootstrap tables below are used only while one of those cannot be read, so a
// model Zen made free today is offered the hour it is listed rather than when
// this plugin ships a release.

// sdkChat, sdkResponses and sdkAnthropic are the packages models.dev gives each
// protocol. This executor speaks chat completions only, so a model whose
// package is not sdkChat is not advertised.
const (
	sdkChat      = "@ai-sdk/openai-compatible"
	sdkResponses = "@ai-sdk/openai"
	sdkAnthropic = "@ai-sdk/anthropic"
)

// zenModel is one normalized listing entry. Key is what the upstream endpoint
// expects; ID is the lowercase routing name CPA publishes.
type zenModel struct {
	Key         string
	Name        string
	Context     int64
	InputLimit  int64
	Output      int64
	Inputs      []string
	Outputs     []string
	Reasoning   bool
	ToolCall    bool
	Efforts     []string
	Status      string
	ReleaseDate string
}

// zenSeed is a catalogue entry the plugin ships when Zen's own sources cannot
// be read. It mirrors the fields the request builder and model card need.
type zenSeed struct {
	Key        string
	Name       string
	Context    int64
	InputLimit int64
	Output     int64
	Inputs     []string
	Outputs    []string
	Reasoning  bool
	ToolCall   bool
	Efforts    []string
}

// bootstrapProtocols is what each free model is asked at, from the table
// OpenCode's console docs carried. The live list's own `api` map is
// authoritative, and the catalog's package name is the next answer; this table
// only stands in for an older listing that carries ids alone. Jev is a SystemOne
// decision model, not one to chat with, so it is never offered.
var bootstrapProtocols = map[string]string{
	"big-pickle":                  "chat",
	"beam-free":                   "chat",
	"flint-40b-free":              "chat",
	"mimo-v2.6-flash-free":        "chat",
	"nemotron-3.5-super-vl-free":  "chat",
	"nemotron-3.5-lightning-free": "chat",
	"deepseek-v4.1-flash-free":    "chat",
	"glm-5.3-flash-free":          "chat",
	"iceman-free":                 "chat",
	"inkling-2-small-free":        "chat",
	"crouton-alpha-free":          "responses",
	"muse-spark-1.3":              "responses",
	"jev-1.13-free":               "systemone",
}

// bootstrapCatalogue is the free chat list at the time of writing, with the
// metadata the live list gave it. It is used only while the live list cannot be
// read.
var bootstrapCatalogue = []zenSeed{
	{Key: "big-pickle", Name: "Big Pickle", Context: 200000, InputLimit: 160000, Output: 32000, Inputs: []string{"text"}, Outputs: []string{"text"}, Reasoning: true, ToolCall: true},
	{Key: "beam-free", Name: "Beam Free", Context: 1048576, Output: 131072, Inputs: []string{"text"}, Outputs: []string{"text"}, Reasoning: true, ToolCall: true, Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{Key: "flint-40b-free", Name: "Flint 40B Free", Context: 262144, Output: 65536, Inputs: []string{"text"}, Outputs: []string{"text"}, Reasoning: true, ToolCall: true, Efforts: []string{"low", "medium", "high", "xhigh", "max"}},
	{Key: "mimo-v2.6-flash-free", Name: "MiMo-V2.6-Flash Free", Context: 200000, Output: 32000, Inputs: []string{"text", "image", "audio", "video"}, Outputs: []string{"text"}, Reasoning: true, ToolCall: true},
	{Key: "nemotron-3.5-super-vl-free", Name: "Nemotron 3.5 Super VL Free", Context: 1048576, InputLimit: 1048576, Output: 131072, Inputs: []string{"text", "image"}, Outputs: []string{"text"}, Reasoning: true, ToolCall: true, Efforts: []string{"none", "minimal", "low", "medium", "high", "xhigh"}},
	{Key: "nemotron-3.5-lightning-free", Name: "Nemotron 3.5 Lightning Free", Context: 262144, Output: 262144, Inputs: []string{"text"}, Outputs: []string{"text"}, Reasoning: true, ToolCall: true},
	{Key: "deepseek-v4.1-flash-free", Name: "DeepSeek V4.1 Flash Free", Context: 200000, Output: 32000, Inputs: []string{"text", "image"}, Outputs: []string{"text"}, Reasoning: true, ToolCall: true, Efforts: []string{"low", "high", "max"}},
	{Key: "glm-5.3-flash-free", Name: "GLM-5.3-Flash Free", Context: 200000, Output: 32000, Inputs: []string{"text", "image", "video"}, Outputs: []string{"text"}, Reasoning: true, ToolCall: true, Efforts: []string{"low", "high", "max"}},
	{Key: "iceman-free", Name: "Iceman Free", Context: 262144, Output: 131072, Inputs: []string{"text"}, Outputs: []string{"text"}, Reasoning: true, ToolCall: true},
	{Key: "inkling-2-small-free", Name: "Inkling 2 Small Free", Context: 1048576, Output: 131072, Inputs: []string{"text", "image"}, Outputs: []string{"text"}, Reasoning: true, ToolCall: true, Efforts: []string{"none", "minimal", "low", "medium", "high", "max"}},
}

// contextFloor is a measured floor for a model the live catalog understates.
// deepseek-v4.1-flash-free is listed at 200000, but a request whose upstream
// usage was 258,624 tokens was served on 2026-10-26. Reporting more than the
// vendor accepts routes requests that then fail, so the floor is used until a
// source says more.
var contextFloor = map[string]int64{"deepseek-v4.1-flash-free": 258624}

// catalogFile is models.dev's v2 catalog, of which only the OpenCode provider is
// read.
type catalogFile struct {
	OpenCode *catalogProvider `json:"opencode"`
}

type catalogProvider struct {
	NPM    string                  `json:"npm"`
	Models map[string]catalogModel `json:"models"`
}

type catalogModel struct {
	Name string `json:"name"`
	Cost struct {
		Input  *float64 `json:"input"`
		Output *float64 `json:"output"`
	} `json:"cost"`
	Limit struct {
		Context float64 `json:"context"`
		Input   float64 `json:"input"`
		Output  float64 `json:"output"`
	} `json:"limit"`
	Modalities struct {
		Input  []string `json:"input"`
		Output []string `json:"output"`
	} `json:"modalities"`
	Reasoning        bool   `json:"reasoning"`
	ToolCall         *bool  `json:"tool_call"`
	Attachment       bool   `json:"attachment"`
	Status           string `json:"status"`
	Lifecycle        string `json:"lifecycle"`
	Deprecated       bool   `json:"deprecated"`
	DeprecatedAt     any    `json:"deprecated_at"`
	RetirementDate   any    `json:"retirement_date"`
	ReleaseDate      string `json:"release_date"`
	ReasoningOptions []struct {
		Type   string   `json:"type"`
		Values []string `json:"values"`
	} `json:"reasoning_options"`
	Provider struct {
		NPM string `json:"npm"`
	} `json:"provider"`
}

// isRetired reports a model Zen no longer serves.
func (m catalogModel) isRetired() bool {
	switch strings.ToLower(strings.TrimSpace(m.Status)) {
	case "deprecated", "retired", "disabled":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(m.Lifecycle)) {
	case "deprecated", "retired", "disabled":
		return true
	}
	return m.Deprecated || m.DeprecatedAt != nil || m.RetirementDate != nil
}

// isFree is Zen's own word: an id that says free, or a catalog entry whose input
// and output cost nothing.
func isFree(id string, m catalogModel) bool {
	if m.isRetired() {
		return false
	}
	if strings.Contains(strings.ToLower(id), "free") {
		return true
	}
	return m.Cost.Input != nil && m.Cost.Output != nil && *m.Cost.Input == 0 && *m.Cost.Output == 0
}

// listedFree is an id Zen names free whose package neither the catalog nor the
// docs describe yet: Zen made it free after they were last written, or they
// could not be read. It is asked as OpenCode asks a model with no package of its
// own, through the provider's chat protocol.
func listedFree(id string) bool { return strings.HasSuffix(strings.ToLower(id), "-free") }

// sdkProtocol maps a models.dev package to the protocol this executor knows.
func sdkProtocol(npm string) string {
	switch {
	case strings.Contains(npm, "anthropic"):
		return "anthropic"
	case npm == sdkResponses:
		return "responses"
	case strings.Contains(npm, "openai-compatible"):
		return "chat"
	}
	return ""
}

// listedModel is one entry of Zen's live list. The endpoint carries the model's
// limits, modalities, reasoning levels and the protocol it is asked at, so the
// list is the primary catalogue; models.dev's catalog and OpenCode's protocol
// table stand in only when an older listing carries ids alone.
type listedModel struct {
	ID             string                     `json:"id"`
	Name           string                     `json:"name"`
	Type           string                     `json:"type"`
	Status         string                     `json:"status"`
	Lifecycle      string                     `json:"lifecycle"`
	Deprecated     bool                       `json:"deprecated"`
	DeprecatedAt   any                        `json:"deprecated_at"`
	RetirementDate any                        `json:"retirement_date"`
	ReleaseDate    string                     `json:"release_date"`
	Limit          listedLimit                `json:"limit"`
	Modalities     listedModalities           `json:"modalities"`
	Cost           listedCost                 `json:"cost"`
	Capabilities   listedCaps                 `json:"capabilities"`
	API            map[string]json.RawMessage `json:"api"`
}

type listedLimit struct {
	Context float64 `json:"context"`
	Input   float64 `json:"input"`
	Output  float64 `json:"output"`
}

type listedModalities struct {
	Input  []string `json:"input"`
	Output []string `json:"output"`
}

type listedCost struct {
	Text *struct {
		Input *struct {
			Price float64 `json:"price"`
		} `json:"input"`
		Output *struct {
			Price float64 `json:"price"`
		} `json:"output"`
	} `json:"text"`
	Input  *float64 `json:"input"`
	Output *float64 `json:"output"`
}

type listedCaps struct {
	Tools struct {
		Supported *bool `json:"supported"`
	} `json:"tools"`
	Reasoning struct {
		Supported bool     `json:"supported"`
		Toggle    bool     `json:"toggle"`
		Effort    []string `json:"effort"`
	} `json:"reasoning"`
}

// prices are the model's input and output price per million tokens, when the
// list names both.
func (m listedModel) prices() (input, output float64, known bool) {
	if text := m.Cost.Text; text != nil && text.Input != nil && text.Output != nil {
		return text.Input.Price, text.Output.Price, true
	}
	if m.Cost.Input != nil && m.Cost.Output != nil {
		return *m.Cost.Input, *m.Cost.Output, true
	}
	return 0, 0, false
}

// retired reports a model Zen no longer serves.
func (m listedModel) retired() bool {
	switch strings.ToLower(strings.TrimSpace(m.Status)) {
	case "deprecated", "retired", "disabled":
		return true
	}
	switch strings.ToLower(strings.TrimSpace(m.Lifecycle)) {
	case "deprecated", "retired", "disabled":
		return true
	}
	return m.Deprecated || m.DeprecatedAt != nil || m.RetirementDate != nil
}

// protocol is the API the live list says the model is asked at. A model that
// offers chat completions is served here even if it offers another API too.
func (m listedModel) protocol() string {
	for _, key := range []string{"chat-completions", "responses", "messages", "generate-content"} {
		if _, ok := m.API[key]; !ok {
			continue
		}
		switch key {
		case "chat-completions":
			return "chat"
		case "responses":
			return "responses"
		case "messages":
			return "anthropic"
		default:
			return "gemini"
		}
	}
	return ""
}

// isFree is Zen's own word: an id that says free, or a model whose input and
// output cost nothing. A listing that names no price falls back to the catalog.
func (m listedModel) isFree(catalog catalogModel, inCatalog bool) bool {
	if m.retired() || (inCatalog && catalog.isRetired()) {
		return false
	}
	if strings.Contains(strings.ToLower(m.ID), "free") {
		return true
	}
	if input, output, known := m.prices(); known {
		return input == 0 && output == 0
	}
	return inCatalog && isFree(m.ID, catalog)
}

// buildModels turns a listing into the free chat models this executor can serve.
// A model served on Responses or Anthropic Messages is left out: a chat request
// to one is refused, and listing a model that cannot be served is the worse
// answer.
func buildModels(items []listedModel, catalog map[string]catalogModel, protocols map[string]string, providerNPM string) map[string]zenModel {
	out := make(map[string]zenModel, len(items))
	for _, item := range items {
		id := strings.TrimSpace(item.ID)
		if id == "" || id == "__proto__" {
			continue
		}
		raw, inCatalog := catalog[id]
		protocol := item.protocol()
		if protocol == "" {
			protocol = protocols[id]
		}
		if protocol == "" && inCatalog {
			protocol = sdkProtocol(firstNonEmpty(raw.Provider.NPM, providerNPM))
		}
		if protocol == "" && listedFree(id) {
			protocol = firstNonEmpty(sdkProtocol(providerNPM), "chat")
		}
		if protocol != "chat" || !item.isFree(raw, inCatalog) {
			continue
		}
		item.ID = id
		out[id] = modelFromListing(item, raw, inCatalog)
	}
	return out
}

// modelFromListing is one model's metadata: the live list's own when it carries
// it, the catalog's when it does not, and the safe defaults when neither does.
func modelFromListing(m listedModel, catalog catalogModel, inCatalog bool) zenModel {
	context := int64(m.Limit.Context)
	if context <= 0 && inCatalog {
		context = int64(catalog.Limit.Context)
	}
	if floor := contextFloor[m.ID]; floor > context {
		context = floor
	}
	if context <= 0 {
		context = 32768
	}
	inputLimit := int64(m.Limit.Input)
	if inputLimit <= 0 && inCatalog {
		inputLimit = int64(catalog.Limit.Input)
	}
	output := int64(m.Limit.Output)
	if output <= 0 && inCatalog {
		output = int64(catalog.Limit.Output)
	}
	if output <= 0 {
		output = 4096
	}
	inputs := modalityList(m.Modalities.Input, nil)
	if len(inputs) == 0 && inCatalog {
		inputs = modalityList(catalog.Modalities.Input, nil)
	}
	if len(inputs) == 0 {
		inputs = []string{"text"}
	}
	outputs := modalityList(m.Modalities.Output, nil)
	if len(outputs) == 0 && inCatalog {
		outputs = modalityList(catalog.Modalities.Output, nil)
	}
	if len(outputs) == 0 {
		outputs = []string{"text"}
	}
	reasoning := m.Capabilities.Reasoning.Supported
	if !reasoning && inCatalog {
		reasoning = catalog.Reasoning
	}
	efforts := append([]string(nil), m.Capabilities.Reasoning.Effort...)
	if len(efforts) == 0 && inCatalog {
		for _, option := range catalog.ReasoningOptions {
			if option.Type == "effort" {
				efforts = append(efforts, option.Values...)
			}
		}
	}
	toolCall := true
	if m.Capabilities.Tools.Supported != nil {
		toolCall = *m.Capabilities.Tools.Supported
	} else if inCatalog && catalog.ToolCall != nil {
		toolCall = *catalog.ToolCall
	}
	status := firstNonEmpty(m.Status, catalog.Status, "active")
	return zenModel{
		Key:         m.ID,
		Name:        firstNonEmpty(m.Name, catalog.Name, m.ID),
		Context:     context,
		InputLimit:  inputLimit,
		Output:      output,
		Inputs:      inputs,
		Outputs:     outputs,
		Reasoning:   reasoning,
		ToolCall:    toolCall,
		Efforts:     dedupeStrings(efforts),
		Status:      status,
		ReleaseDate: firstNonEmpty(m.ReleaseDate, catalog.ReleaseDate),
	}
}

// modalityList keeps the modalities the host understands, in a stable order. A
// live listing names MIME types ("image/png", "video/mp4"), which map to their
// family; the catalog names the family itself.
func modalityList(values, fallback []string) []string {
	order := []string{"text", "image", "audio", "video", "pdf"}
	have := make(map[string]bool, len(values))
	for _, v := range values {
		v = strings.ToLower(strings.TrimSpace(v))
		switch {
		case v == "text":
			have["text"] = true
		case strings.HasPrefix(v, "image"):
			have["image"] = true
		case strings.HasPrefix(v, "audio"):
			have["audio"] = true
		case strings.HasPrefix(v, "video"):
			have["video"] = true
		case v == "pdf" || strings.HasPrefix(v, "application/pdf"):
			have["pdf"] = true
		}
	}
	var out []string
	for _, name := range order {
		if have[name] {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return append([]string(nil), fallback...)
	}
	return out
}

func modelFromSeed(s zenSeed) zenModel {
	inputs := append([]string(nil), s.Inputs...)
	if len(inputs) == 0 {
		inputs = []string{"text"}
	}
	outputs := append([]string(nil), s.Outputs...)
	if len(outputs) == 0 {
		outputs = []string{"text"}
	}
	context := s.Context
	if floor := contextFloor[s.Key]; floor > context {
		context = floor
	}
	if context <= 0 {
		context = 32768
	}
	output := s.Output
	if output <= 0 {
		output = 4096
	}
	return zenModel{
		Key:        s.Key,
		Name:       s.Name,
		Context:    context,
		InputLimit: s.InputLimit,
		Output:     output,
		Inputs:     inputs,
		Outputs:    outputs,
		Reasoning:  s.Reasoning,
		ToolCall:   s.ToolCall,
		Efforts:    append([]string(nil), s.Efforts...),
		Status:     "active",
	}
}

// bootstrapChatModels is the shipped list, already filtered to what this
// executor can serve.
func bootstrapChatModels() map[string]zenModel {
	out := make(map[string]zenModel, len(bootstrapCatalogue))
	for _, seed := range bootstrapCatalogue {
		if bootstrapProtocols[seed.Key] != "chat" {
			continue
		}
		out[seed.Key] = modelFromSeed(seed)
	}
	return out
}

func modelInfo(m zenModel) pluginapi.ModelInfo {
	id := strings.ToLower(strings.TrimSpace(m.Key))
	inputLimit := m.InputLimit
	if inputLimit <= 0 {
		inputLimit = m.Context
	}
	info := pluginapi.ModelInfo{
		ID:                         id,
		Name:                       m.Name,
		DisplayName:                m.Name,
		Object:                     "model",
		OwnedBy:                    credentialProvider,
		Type:                       credentialProvider,
		ContextLength:              m.Context,
		InputTokenLimit:            inputLimit,
		OutputTokenLimit:           m.Output,
		MaxCompletionTokens:        m.Output,
		SupportedInputModalities:   append([]string(nil), m.Inputs...),
		SupportedOutputModalities:  append([]string(nil), m.Outputs...),
		SupportedGenerationMethods: []string{"chat.completions"},
	}
	if len(info.SupportedInputModalities) == 0 {
		info.SupportedInputModalities = []string{"text"}
	}
	if len(info.SupportedOutputModalities) == 0 {
		info.SupportedOutputModalities = []string{"text"}
	}
	if len(m.Efforts) > 0 {
		info.Thinking = &pluginapi.ThinkingSupport{Levels: append([]string(nil), m.Efforts...)}
	}
	return info
}

func modelResponse(models []zenModel) pluginapi.ModelResponse {
	sort.Slice(models, func(i, j int) bool {
		return strings.ToLower(models[i].Key) < strings.ToLower(models[j].Key)
	})
	out := pluginapi.ModelResponse{Provider: credentialProvider, Models: make([]pluginapi.ModelInfo, 0, len(models))}
	for _, m := range models {
		if strings.TrimSpace(m.Key) == "" {
			continue
		}
		out.Models = append(out.Models, modelInfo(m))
	}
	return out
}

// offerable is the account's list without what the configuration excludes.
func offerable(models []zenModel) []zenModel {
	out := make([]zenModel, 0, len(models))
	for _, m := range models {
		if modelExcluded(m.Key) {
			continue
		}
		out = append(out, m)
	}
	return out
}

// modelPrefix matches a provider prefix a client may put on a model id.
var modelPrefix = regexp.MustCompile(`^[a-z0-9-]+/(.+)$`)

func trimModelPrefix(model string) string {
	if m := modelPrefix.FindStringSubmatch(model); m != nil {
		return m[1]
	}
	return model
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func dedupeStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	return out
}

// parseListing reads Zen's model list. The endpoint answers {"data":[...]}, and
// a bare array is accepted too so a proxy that unwraps the envelope still works.
// An empty list is an answer: every free model may have been retired.
func parseListing(body []byte) ([]listedModel, error) {
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) == nil && len(envelope.Data) > 0 && string(envelope.Data) != "null" {
		var items []listedModel
		if json.Unmarshal(envelope.Data, &items) == nil {
			return items, nil
		}
	}
	if strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		var items []listedModel
		if json.Unmarshal(body, &items) == nil {
			return items, nil
		}
	}
	return nil, errNoModelList
}

// parseCatalog reads models.dev's v2 catalog. A catalog without the OpenCode
// provider is not an answer.
func parseCatalog(body []byte) (catalogProvider, error) {
	var file catalogFile
	if err := json.Unmarshal(body, &file); err != nil || file.OpenCode == nil {
		return catalogProvider{}, errNoCatalog
	}
	if file.OpenCode.Models == nil {
		file.OpenCode.Models = map[string]catalogModel{}
	}
	return *file.OpenCode, nil
}
