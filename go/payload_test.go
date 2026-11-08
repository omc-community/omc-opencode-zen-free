package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStatedDirectoryReadsTheAgentsPrompt(t *testing.T) {
	cases := []struct {
		name     string
		messages []any
		want     string
	}{
		{"codex cwd", []any{map[string]any{"role": "system", "content": "work here\n<cwd>/srv/app</cwd>"}}, "/srv/app"},
		{"working directory", []any{map[string]any{"role": "system", "content": "Working directory: /home/ada/project\nrest"}}, "/home/ada/project"},
		{"current working directory", []any{map[string]any{"role": "user", "content": "The current working directory is `/home/ada/project`."}}, "/home/ada/project"},
		{"windows", []any{map[string]any{"role": "system", "content": "<cwd>C:\\work\\app</cwd>"}}, `C:\work\app`},
		{"none", []any{map[string]any{"role": "user", "content": "hello"}}, ""},
		{"root is not a directory", []any{map[string]any{"role": "system", "content": "Working directory: /"}}, ""},
	}
	for _, tc := range cases {
		if got := statedDirectory(map[string]any{"messages": tc.messages}); got != tc.want {
			t.Fatalf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEnvironmentBlockIsOpenCodesOwn(t *testing.T) {
	block := environmentBlock("/srv/app")
	for _, want := range []string{
		"Here is some useful information about the environment you are running in:",
		"<env>",
		"  Working directory: /srv/app",
		"  Workspace root folder: ",
		"  Is directory a git repo: ",
		"  Platform: ",
		"  Prefer ",
		"</env>",
	} {
		if !strings.Contains(block, want) {
			t.Fatalf("block is missing %q: %s", want, block)
		}
	}
	if strings.Contains(block, "Working directory: unknown") {
		t.Fatalf("block uses the refused placeholder: %s", block)
	}
}

func TestAddEnvironmentKeepsOneBlock(t *testing.T) {
	chat := map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "prompt"},
		map[string]any{"role": "user", "content": "hi"},
	}}
	addEnvironment(chat)
	addEnvironment(chat)
	messages := chat["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v", messages)
	}
	if strings.Count(textOf(messages[0].(map[string]any)["content"]), "<env>") != 1 {
		t.Fatalf("content = %q", textOf(messages[0].(map[string]any)["content"]))
	}
}

func TestAddEnvironmentPutsASystemMessageFirst(t *testing.T) {
	chat := map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	addEnvironment(chat)
	messages := chat["messages"].([]any)
	if len(messages) != 2 || messages[0].(map[string]any)["role"] != "system" {
		t.Fatalf("messages = %v", messages)
	}
}

func TestOpencodeIDHasTheShapeZenChecks(t *testing.T) {
	id := opencodeID("ses", true, timeForTest())
	if !strings.HasPrefix(id, "ses_") || len(id) != 4+12+14 {
		t.Fatalf("id = %q", id)
	}
	if !realSessionID(id) {
		t.Fatalf("the id the plugin made is not one Zen accepts: %q", id)
	}
	if realSessionID("ses_" + strings.Repeat("0", 9) + "000" + strings.Repeat("A", 14)) {
		t.Fatal("a made-up id was accepted as a real session")
	}
}

func TestSessionIDPreservesARealOpenCodeSession(t *testing.T) {
	resetPluginState()
	real := opencodeID("ses", true, timeForTest())
	if got := sessionID(http.Header{"X-Session-Id": {real}}, map[string]any{}); got != real {
		t.Fatalf("session = %q", got)
	}
}

func TestSessionIDIsStablePerSignal(t *testing.T) {
	resetPluginState()
	headers := http.Header{"X-Session-Id": {"conversation-1"}}
	one := sessionID(headers, map[string]any{})
	two := sessionID(headers, map[string]any{})
	if one != two || !realSessionID(one) {
		t.Fatalf("sessions = %q %q", one, two)
	}
	other := sessionID(http.Header{"X-Session-Id": {"conversation-2"}}, map[string]any{})
	if other == one {
		t.Fatal("two conversations share one session")
	}
}

func TestProjectIDHonorsTheClientHeader(t *testing.T) {
	resetPluginState()
	got := projectID(http.Header{"X-Opencode-Project": {"client-project"}}, map[string]any{})
	if got != "client-project" {
		t.Fatalf("project = %q", got)
	}
}

func TestProjectIDNamesARepositoryByItsRemote(t *testing.T) {
	resetPluginState()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := "[remote \"origin\"]\n\turl = git@github.com:acme/app.git\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	chat := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "Working directory: " + dir}}}
	got := projectID(http.Header{}, chat)
	if want := sha1Hex("git-remote:github.com/acme/app"); got != want {
		t.Fatalf("project = %q, want %q", got, want)
	}
}

func TestProjectIDNamesAPlainDirectory(t *testing.T) {
	resetPluginState()
	// A repository with no remote is named by its directory. The fixture owns
	// the nearest .git and has no config in it, so the checkout's own remote
	// never decides the answer.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	local := localDirectory(abs)
	root, remote := repository(local)
	if remote != "" || root != local {
		t.Fatalf("fixture repository = %q %q", root, remote)
	}
	chat := map[string]any{"messages": []any{map[string]any{"role": "system", "content": "Working directory: " + abs}}}
	got := projectID(http.Header{}, chat)
	if want := sha1Hex("directory:" + root); got != want {
		t.Fatalf("project = %q, want %q", got, want)
	}
}

func TestRemotePathReadsEveryRemoteSpelling(t *testing.T) {
	cases := map[string]string{
		"https://github.com/acme/app.git":   "github.com/acme/app",
		"ssh://git@github.com/acme/app.git": "github.com/acme/app",
		"git@github.com:acme/app.git":       "github.com/acme/app",
		"git://github.com/acme/app":         "github.com/acme/app",
		"file:///srv/app":                   "",
		"/srv/app":                          "",
		"":                                  "",
	}
	for input, want := range cases {
		if got := remotePath(input); got != want {
			t.Fatalf("%q: got %q, want %q", input, got, want)
		}
	}
}

func timeForTest() time.Time { return time.Unix(1794123066, 0) }
