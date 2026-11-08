package main

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// hasEnvironment is OpenCode's own environment block: `<env>`, a
// `Working directory:` line indented two spaces, and `</env>`. Zen's free tier
// serves a request as OpenCode's only when a system message carries it. Another
// agent's own block is its own text and does not count, so it stays and
// OpenCode's follows it.
var hasEnvironment = regexp.MustCompile(`<env>\r?\n {2}Working directory: [^\n]*\n[\s\S]*?<\/env>`)

// absolutePath is how agents name a directory: an absolute POSIX path, a
// Windows drive path, or a UNC path. Only an absolute path counts.
const absolutePath = `((?:~?/|[A-Za-z]:[\\/]|\\\\)[^\s\x60'"<>]*)`

var statedPatterns = []*regexp.Regexp{
	regexp.MustCompile(`<cwd>\s*` + absolutePath + `\s*</cwd>`),
	regexp.MustCompile(`(?i)working directory(?: is)?:?\s+\x60?` + absolutePath),
}

// systemRoles are the roles whose text is an agent's prompt rather than a turn.
var systemRoles = map[string]bool{"system": true, "developer": true}

// textOf is a message's text: a string, or the text parts of a content array.
func textOf(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, raw := range c {
			if part, ok := raw.(map[string]any); ok {
				if text, ok := part["text"].(string); ok && text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// statedDirectory is the working directory the agent's own prompt names, in
// either spelling agents use (Codex's `<cwd>`, a `Working directory:` line, or
// "the current working directory is `...`"). The system messages are read
// first, then the first user message.
func statedDirectory(chat map[string]any) string {
	messages, _ := chat["messages"].([]any)
	var texts []string
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok || !systemRoles[roleOf(m)] {
			continue
		}
		texts = append(texts, textOf(m["content"]))
	}
	if first := firstUserMessage(messages); first != nil {
		texts = append(texts, textOf(first["content"]))
	}
	for _, text := range texts {
		for _, pattern := range statedPatterns {
			found := pattern.FindStringSubmatch(text)
			if found == nil {
				continue
			}
			dir := strings.TrimRight(found[1], ".,;:)]")
			if dir == "" || dir == "/" || dir == "~/" {
				continue
			}
			return dir
		}
	}
	return ""
}

// environmentBlock is OpenCode 2.2.5's own block, word for word, for `dir`. Zen
// answers the literal placeholder `Working directory: unknown` with a 429, so
// the block always names a real path.
func environmentBlock(dir string) string {
	local := localDirectory(dir)
	root, _ := repository(local)
	workspace := dir
	if root != "" && root != local {
		workspace = root
	}
	gitRepo := "no"
	if root != "" {
		gitRepo = "yes"
	}
	return strings.Join([]string{
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: " + dir,
		"  Workspace root folder: " + workspace,
		"  Is directory a git repo: " + gitRepo,
		"  Platform: " + platformName(),
		"  Prefer " + opencodeTmp() + " over generic system temporary directories such as /tmp; it is pre-created and approved for external access.",
		"</env>",
	}, "\n")
}

// withEnvironment appends a block to a message's content, whatever shape it is.
func withEnvironment(content any, block string) any {
	switch c := content.(type) {
	case string:
		if c == "" {
			return block
		}
		return c + "\n\n" + block
	case []any:
		return append(append([]any(nil), c...), map[string]any{"type": "text", "text": block})
	default:
		return block
	}
}

// addEnvironment puts OpenCode's block in the agent's own system message, so the
// prompt stays one system message and its cache holds. A chat that has no
// system message gets one first. A prompt that already carries OpenCode's block
// goes as it is.
func addEnvironment(chat map[string]any) {
	messages, _ := chat["messages"].([]any)
	var system []map[string]any
	for _, raw := range messages {
		if m, ok := raw.(map[string]any); ok && systemRoles[roleOf(m)] {
			system = append(system, m)
		}
	}
	for _, m := range system {
		if hasEnvironment.MatchString(textOf(m["content"])) {
			return
		}
	}
	block := environmentBlock(firstNonEmpty(statedDirectory(chat), homeDirectory()))
	if len(system) > 0 {
		first := system[0]
		first["content"] = withEnvironment(first["content"], block)
		return
	}
	chat["messages"] = append([]any{map[string]any{"role": "system", "content": block}}, messages...)
}

func roleOf(m map[string]any) string {
	role, _ := m["role"].(string)
	return role
}

func firstUserMessage(messages []any) map[string]any {
	for _, raw := range messages {
		if m, ok := raw.(map[string]any); ok && roleOf(m) == "user" {
			return m
		}
	}
	return nil
}

func userTexts(messages []any) []string {
	var out []string
	for _, raw := range messages {
		if m, ok := raw.(map[string]any); ok && roleOf(m) == "user" {
			out = append(out, textOf(m["content"]))
		}
	}
	return out
}

// localDirectory expands a leading ~ and resolves symlinks when the path exists.
// A client's path usually does not exist on the server, so an unresolvable path
// is used as stated.
func localDirectory(directory string) string {
	dir := directory
	if dir == "~" || strings.HasPrefix(dir, "~/") {
		dir = filepath.Join(homeDirectory(), strings.TrimPrefix(dir, "~"))
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return resolved
	}
	return dir
}

func homeDirectory() string {
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return home
	}
	return "/"
}

// platformName is process.platform as Node names it, which is what OpenCode's
// block carries.
func platformName() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin", "linux", "freebsd", "openbsd", "netbsd", "android", "aix", "sunos":
		return runtime.GOOS
	default:
		return runtime.GOOS
	}
}

// opencodeTmp is OpenCode's temp folder, which its block names.
func opencodeTmp() string {
	if resolved, err := filepath.EvalSymlinks(os.TempDir()); err == nil {
		return filepath.Join(resolved, "opencode")
	}
	return filepath.Join(os.TempDir(), "opencode")
}

// repository finds the nearest .git up from directory and the remote OpenCode
// names it by (origin, else the first one listed). A directory that does not
// exist on this host has no repository: the plugin runs on a server and cannot
// read a client's working tree.
func repository(directory string) (root, remote string) {
	dir := directory
	if _, err := os.Stat(dir); err != nil {
		return "", ""
	}
	for {
		dotGit := filepath.Join(dir, ".git")
		info, err := os.Stat(dotGit)
		if err == nil {
			common := dotGit
			if info.IsDir() {
				return dir, readRemote(common)
			}
			content, err := os.ReadFile(dotGit)
			if err != nil {
				return dir, ""
			}
			line := strings.TrimSpace(string(content))
			gitdir, ok := strings.CutPrefix(line, "gitdir:")
			if !ok {
				return dir, ""
			}
			own := filepath.Join(dir, strings.TrimSpace(gitdir))
			common = own
			if shared, err := os.ReadFile(filepath.Join(own, "commondir")); err == nil {
				if resolved := strings.TrimSpace(string(shared)); resolved != "" {
					common = filepath.Join(own, resolved)
				}
			}
			return dir, readRemote(common)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", ""
		}
		dir = parent
	}
}

var (
	remoteSection = regexp.MustCompile(`(?m)^\s*\[remote\s+"([^"]+)"\]([^[]*)`)
	remoteURL     = regexp.MustCompile(`(?m)^\s*url\s*=\s*(.+)$`)
)

// readRemote reads the remote OpenCode would name the repository by.
func readRemote(common string) string {
	raw, err := os.ReadFile(filepath.Join(common, "config"))
	if err != nil {
		return ""
	}
	type entry struct{ name, url string }
	var remotes []entry
	for _, match := range remoteSection.FindAllStringSubmatch(string(raw), -1) {
		url := ""
		if u := remoteURL.FindStringSubmatch(match[2]); u != nil {
			url = strings.TrimSpace(u[1])
		}
		remotes = append(remotes, entry{name: match[1], url: url})
	}
	chosen := ""
	for _, r := range remotes {
		if r.name == "origin" {
			chosen = r.url
			break
		}
	}
	if chosen == "" && len(remotes) > 0 {
		chosen = remotes[0].url
	}
	return remotePath(chosen)
}

// remotePath is host/path for a remote URL, or empty when the value is not a
// network remote.
func remotePath(value string) string {
	input := strings.TrimSpace(value)
	if input == "" {
		return ""
	}
	parts := func(host, name string) string {
		path := strings.TrimSuffix(strings.TrimSuffix(strings.TrimLeft(name, "/"), ".git"), "/")
		if host == "" || path == "" {
			return ""
		}
		return strings.ToLower(host) + "/" + path
	}
	if strings.Contains(input, "://") {
		u, err := url.Parse(input)
		if err != nil || u.Scheme == "file" {
			return ""
		}
		return parts(u.Hostname(), u.Path)
	}
	// scp-like: [user@]host:path
	colon := strings.Index(input, ":")
	if colon <= 0 {
		return ""
	}
	host := input[:colon]
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	// A Windows drive path is not a remote; neither is a value with a slash
	// before its colon.
	if len(host) <= 1 || strings.ContainsAny(host, "/\\") {
		return ""
	}
	return parts(host, input[colon+1:])
}

// opencodeID is an id the way OpenCode makes one: 12 hex of
// Date.now() * 0x1000 + a per-millisecond counter (bit-inverted for a session,
// so newer sorts first), then 14 random base62 characters. Zen's free tier
// checks a session id has this shape, a small counter included.
const base62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var idMu sync.Mutex
var lastStamp int64
var idCounter uint64

func opencodeID(prefix string, descending bool, now time.Time) string {
	idMu.Lock()
	stamp := now.UnixMilli()
	if stamp != lastStamp {
		lastStamp = stamp
		idCounter = 0
	}
	idCounter++
	counter := idCounter
	idMu.Unlock()

	current := uint64(stamp)*0x1000 + counter
	value := current
	if descending {
		value = ^current
	}
	value &= 0xffffffffffff
	tail := make([]byte, 14)
	random := make([]byte, 14)
	if _, err := rand.Read(random); err != nil {
		for i := range random {
			random[i] = byte(i)
		}
	}
	for i, b := range random {
		tail[i] = base62[int(b)%len(base62)]
	}
	return prefix + "_" + hex.EncodeToString([]byte{
		byte(value >> 40), byte(value >> 32), byte(value >> 24),
		byte(value >> 16), byte(value >> 8), byte(value),
	}) + string(tail)
}

// realSessionID reports a session id OpenCode itself could have made: its
// counter (the low 12 bits, inverted) is small. A hash cut to the same shape is
// not one.
var sessionShape = regexp.MustCompile(`^ses_[0-9a-f]{9}([0-9a-f]{3})[0-9A-Za-z]{14}$`)

func realSessionID(value string) bool {
	match := sessionShape.FindStringSubmatch(value)
	if match == nil {
		return false
	}
	var counter int
	for _, r := range match[1] {
		counter = counter*16 + hexValue(r)
	}
	return 0xfff-counter < 16
}

func hexValue(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10
	}
	return 0
}

// sessionCache keeps one made-up session per conversation, so its turns keep one
// session. It is bounded, oldest first.
const keepSessions = 1000

var sessionMu sync.Mutex
var sessions = map[string]string{}

// sessionID is the OpenCode session this request belongs to. A client that is
// OpenCode sends its own; another client's session id is hashed into the same
// shape, one per conversation.
func sessionID(headers http.Header, chat map[string]any) string {
	signal := ""
	for _, name := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id", "conversation-id"} {
		if v := strings.TrimSpace(headers.Get(name)); v != "" {
			signal = v
			break
		}
	}
	if signal == "" {
		if metadata, ok := chat["metadata"].(map[string]any); ok {
			if v, ok := metadata["session_id"].(string); ok {
				signal = strings.TrimSpace(v)
			}
		}
	}
	if realSessionID(signal) {
		return signal
	}
	if signal == "" {
		if first := firstUserMessage(messagesOf(chat)); first != nil {
			signal = textOf(first["content"])
		}
	}
	if strings.TrimSpace(signal) == "" {
		return opencodeID("ses", true, time.Now())
	}
	sum := sha256.Sum256([]byte("ses\x00" + signal))
	key := hex.EncodeToString(sum[:])

	sessionMu.Lock()
	defer sessionMu.Unlock()
	if session, ok := sessions[key]; ok {
		return session
	}
	session := opencodeID("ses", true, time.Now())
	if len(sessions) >= keepSessions {
		for other := range sessions {
			delete(sessions, other)
			break
		}
	}
	sessions[key] = session
	return session
}

func messagesOf(chat map[string]any) []any {
	messages, _ := chat["messages"].([]any)
	return messages
}

// traceCache keeps one trace per session, so every turn of a conversation is
// one trace in OpenCode's own tracing.
var traceMu sync.Mutex
var traces = map[string]string{}

// traceHeaders is OpenCode's B3 and W3C trace pair: one trace a conversation, a
// new span each request.
func traceHeaders(session string) (b3, traceparent string) {
	traceMu.Lock()
	trace, ok := traces[session]
	if !ok {
		trace = randomHex(16)
		if len(traces) >= keepSessions {
			for other := range traces {
				delete(traces, other)
				break
			}
		}
	}
	traces[session] = trace
	traceMu.Unlock()
	span := randomHex(8)
	return trace + "-" + span + "-1-" + randomHex(8), "00-" + trace + "-" + span + "-01"
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// A trace that cannot be random still has to be well formed.
		for i := range b {
			b[i] = byte(i * 7)
		}
	}
	return hex.EncodeToString(b)
}

// projectID is OpenCode's project id: the client's own when it sends one, else
// sha1("git-remote:" + host/path) for a repository with a remote, else
// sha1("directory:" + the directory). The plugin runs on a server, so a
// client's repository is named by its directory: the same shape, and no git to
// run on the client.
func projectID(headers http.Header, chat map[string]any) string {
	if given := strings.TrimSpace(headers.Get("x-opencode-project")); given != "" {
		return given
	}
	dir := firstNonEmpty(statedDirectory(chat), homeDirectory())
	local := localDirectory(dir)
	root, remote := repository(local)
	if remote != "" {
		return sha1Hex("git-remote:" + remote)
	}
	if root == "" {
		root = local
	}
	return sha1Hex("directory:" + root)
}

func sha1Hex(text string) string {
	sum := sha1.Sum([]byte(text))
	return hex.EncodeToString(sum[:])
}

// parseChat reads a chat completion body, which the host hands over already
// translated to OpenAI's shape.
func parseChat(body []byte) (map[string]any, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, errors.New("a chat completion request is required")
	}
	var chat map[string]any
	if err := json.Unmarshal(body, &chat); err != nil {
		return nil, errors.New("invalid chat completion request")
	}
	if _, ok := chat["messages"].([]any); !ok {
		return nil, errors.New("invalid chat completion request")
	}
	return chat, nil
}
