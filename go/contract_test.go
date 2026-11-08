package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func TestRegistrationContract(t *testing.T) {
	out, err := handleMethod("plugin.register", nil)
	if err != nil {
		t.Fatal(err)
	}
	reg, ok := out.(map[string]any)
	if !ok {
		t.Fatalf("registration = %T", out)
	}
	if reg["schema_version"] != pluginabi.SchemaVersion || pluginabi.SchemaVersion != 10 {
		t.Fatalf("schema = %v", reg["schema_version"])
	}
	meta := reg["metadata"].(pluginapi.Metadata)
	if meta.Name != "OMC OpenCode Zen Free" || meta.Version != pluginVersion {
		t.Fatalf("metadata = %+v", meta)
	}
	if meta.Logo != publicAssetBaseURL+pluginLogo {
		t.Fatalf("logo = %q", meta.Logo)
	}
	if len(meta.ConfigFields) != 3 {
		t.Fatalf("config fields = %+v", meta.ConfigFields)
	}
	for _, field := range meta.ConfigFields {
		if field.Default == nil || field.Description == "" {
			t.Fatalf("field %q has no default or description", field.Name)
		}
	}
	caps := reg["capabilities"].(map[string]any)
	if caps["executor_input_formats"].([]string)[0] != "openai" || caps["executor_output_formats"].([]string)[0] != "openai" {
		t.Fatalf("formats = %v", caps)
	}
	if caps["executor_model_scope"] != "oauth" {
		t.Fatalf("scope = %v", caps["executor_model_scope"])
	}
}

func TestAuthIdentifierPublishesOneEntry(t *testing.T) {
	out, err := handleMethod("auth.identifier", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(map[string]any)
	if got["identifier"] != credentialProvider || got["credential_providers"].([]string)[0] != credentialProvider {
		t.Fatalf("identifier = %v", got)
	}
	ids := got["identifiers"].([]string)
	if len(ids) != 1 || ids[0] != credentialProvider {
		t.Fatalf("identifiers = %v", ids)
	}
	entries := got["entries"].([]pluginapi.AuthProviderEntry)
	if len(entries) != 1 || entries[0].Identifier != credentialProvider || entries[0].Name != authLabel {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].Logo != publicAssetBaseURL+zenMark {
		t.Fatalf("logo = %q", entries[0].Logo)
	}
	for _, method := range []string{"executor.identifier", "quota.identifier"} {
		out, err := handleMethod(method, nil)
		if err != nil {
			t.Fatal(err)
		}
		if out.(map[string]string)["identifier"] != credentialProvider {
			t.Fatalf("%s = %v", method, out)
		}
	}
}

func TestLoginRPCIsWired(t *testing.T) {
	start, err := handleMethod("auth.login.start", mustJSON(t, map[string]any{"Provider": credentialProvider}))
	if err != nil {
		t.Fatal(err)
	}
	started := start.(pluginapi.AuthLoginStartResponse)
	if started.State == "" || started.URL != signInPageURL || started.Provider != credentialProvider {
		t.Fatalf("start = %+v", started)
	}
	poll, err := handleMethod("auth.login.poll", mustJSON(t, map[string]any{"State": started.State}))
	if err != nil {
		t.Fatal(err)
	}
	finished := poll.(pluginapi.AuthLoginPollResponse)
	if finished.Status != pluginapi.AuthLoginStatusSuccess || finished.Auth.Provider != credentialProvider {
		t.Fatalf("poll = %+v", finished)
	}
}

func TestParseAuthReadsPersistedFile(t *testing.T) {
	body := []byte(`{"type":"opencode-zen-free","access":"public"}`)
	req, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: credentialProvider, FileName: "opencode-zen-free-public.json", RawJSON: body})
	out, err := handleMethod("auth.parse", req)
	if err != nil {
		t.Fatal(err)
	}
	resp := out.(pluginapi.AuthParseResponse)
	if !resp.Handled || resp.Auth.Provider != credentialProvider {
		t.Fatalf("parse = %+v", resp)
	}
	if resp.Auth.ID != "opencode-zen-free-public.json" || resp.Auth.Label != authLabel {
		t.Fatalf("auth = %+v", resp.Auth)
	}
	var c credential
	if json.Unmarshal(resp.Auth.StorageJSON, &c) != nil || c.Access != publicKey {
		t.Fatalf("credential = %+v", c)
	}
}

func TestParseAuthRejectsForeignFiles(t *testing.T) {
	cases := []string{
		`{"type":"workbuddy","access":"public"}`,
		`{"type":"opencode-zen-free","access":"personal"}`,
		`{"type":"opencode-zen-free"}`,
		`not json`,
	}
	for _, body := range cases {
		req, _ := json.Marshal(pluginapi.AuthParseRequest{RawJSON: []byte(body)})
		out, err := handleMethod("auth.parse", req)
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if out.(pluginapi.AuthParseResponse).Handled {
			t.Fatalf("%s was accepted", body)
		}
	}
	req, _ := json.Marshal(pluginapi.AuthParseRequest{Provider: "qoder", RawJSON: []byte(`{"type":"opencode-zen-free","access":"public"}`)})
	out, err := handleMethod("auth.parse", req)
	if err != nil {
		t.Fatal(err)
	}
	if out.(pluginapi.AuthParseResponse).Handled {
		t.Fatal("a qoder login entry claimed an OpenCode Zen Free auth file")
	}
}

func TestManagementServesPublishedBytes(t *testing.T) {
	for name, contentType := range map[string]string{"logo.png": "image/png", "zen.png": "image/png"} {
		want, err := os.ReadFile("../assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := handleMethod("management.handle", mustJSON(t, map[string]any{
			"Method": http.MethodGet,
			"Path":   resourcePrefix + "/" + name,
		}))
		if err != nil {
			t.Fatal(err)
		}
		got := resp.(pluginapi.ManagementResponse)
		if got.StatusCode != http.StatusOK || !bytes.Equal(got.Body, want) {
			t.Fatalf("%s: status %d, %d bytes", name, got.StatusCode, len(got.Body))
		}
		if got.Headers.Get("Content-Type") != contentType || got.Headers.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s headers = %v", name, got.Headers)
		}
	}
	resp, _ := handleMethod("management.handle", mustJSON(t, map[string]any{"Method": http.MethodGet, "Path": omcResourcePath}))
	descriptor := resp.(pluginapi.ManagementResponse)
	if descriptor.Headers.Get("Content-Type") != "application/json; charset=utf-8" || string(descriptor.Body) != omcDescriptorJSON {
		t.Fatal("the declaration is not served as the bytes the plugin embeds")
	}
	var document map[string]any
	if err := json.Unmarshal(descriptor.Body, &document); err != nil {
		t.Fatal(err)
	}
	if document["plugin_id"] != pluginStoreID || document["plugin_version"] != pluginVersion {
		t.Fatalf("declaration = %v", document)
	}
}

func TestManagementRejectsWritesAndUnknownPaths(t *testing.T) {
	resp, _ := handleMethod("management.handle", mustJSON(t, map[string]any{"Method": http.MethodPost, "Path": resourcePrefix + "/zen.png"}))
	if got := resp.(pluginapi.ManagementResponse); got.StatusCode != http.StatusMethodNotAllowed || got.Headers.Get("Allow") != http.MethodGet {
		t.Fatalf("write = %+v", got)
	}
	resp, _ = handleMethod("management.handle", mustJSON(t, map[string]any{"Method": http.MethodGet, "Path": resourcePrefix + "/other.png"}))
	if got := resp.(pluginapi.ManagementResponse); got.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path = %+v", got)
	}
}

func TestEmbeddedAssetsMatchPublishedOnes(t *testing.T) {
	// The declaration is authored where the plugin embeds it; the two marks are
	// published from assets/ and embedded from go/assets/.
	for _, name := range []string{"logo.png", "zen.png"} {
		published, err := os.ReadFile("../assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		embedded, err := os.ReadFile("assets/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(published, embedded) {
			t.Fatalf("%s differs between assets/ and go/assets/", name)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
