package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// signInTimeout is how long a sign-in request may be polled. The flow itself
// finishes on the first poll; the window only bounds a state a panel holds.
const signInTimeout = 5 * time.Minute

// loginStatePrefix marks a state this plugin minted. The rest of the state is
// the expiry and a nonce, all of it public: the credential being handed out is
// the shared public key, so the state protects nothing and only lets the poll
// recognize its own flow and expire it.
const loginStatePrefix = credentialProvider + "."

// signInPageURL is what the panel opens when a user starts the sign-in. The
// free tier has no browser page: the first poll already returns the public
// credential. CPA's OAuth panel nevertheless refuses a plugin flow whose URL is
// empty ("the plugin did not return complete browser authorization details"),
// so the plugin names its own explanation of the keyless identity. Opening it is
// optional; the account is added without leaving the panel.
const signInPageURL = "https://github.com/omc-community/omc-opencode-zen-free#one-public-identity-no-key"

// startLogin mints a keyless sign-in. There is no vendor call to make: Zen's
// free tier accepts one public key, so the flow is complete before the panel
// asks. The URL points at the plugin's own explanation because the panel
// requires a non-empty one; the first poll receives the credential.
func startLogin(req pluginapi.AuthLoginStartRequest) (pluginapi.AuthLoginStartResponse, error) {
	var out pluginapi.AuthLoginStartResponse
	if err := loginEntry(req.Provider); err != nil {
		return out, err
	}
	nonce, err := newNonce()
	if err != nil {
		return out, errors.New("cannot generate login state")
	}
	expires := time.Now().Add(signInTimeout)
	return pluginapi.AuthLoginStartResponse{
		Provider:  credentialProvider,
		URL:       signInPageURL,
		State:     loginState(expires, nonce),
		ExpiresAt: expires.UTC(),
	}, nil
}

// loginEntry accepts the entry a panel names. An empty provider is a host that
// knows only the plugin key; the store identifier is not a login entry.
func loginEntry(provider string) error {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "", credentialProvider, pluginStoreID:
		return nil
	default:
		return fmt.Errorf("unknown login entry: use %s", credentialProvider)
	}
}

// newNonce is the random part of a login state.
func newNonce() ([]byte, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}

func loginState(expires time.Time, nonce []byte) string {
	return loginStatePrefix + strconv.FormatInt(expires.Unix(), 10) + "." + hex.EncodeToString(nonce)
}

// parseLoginState reads a state this plugin minted, and reports whether it is
// still inside its window.
func parseLoginState(state string, now time.Time) (time.Time, bool) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(state), loginStatePrefix)
	if !ok {
		return time.Time{}, false
	}
	at, nonce, ok := strings.Cut(rest, ".")
	if !ok || len(nonce) != 32 {
		return time.Time{}, false
	}
	if _, err := hex.DecodeString(nonce); err != nil {
		return time.Time{}, false
	}
	seconds, err := strconv.ParseInt(at, 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}, false
	}
	expires := time.Unix(seconds, 0).UTC()
	if now.After(expires) {
		return time.Time{}, false
	}
	return expires, true
}

// pollLogin answers the public credential on the first poll. The answer is
// idempotent: a panel that asks again, or a plugin restarted between start and
// poll, still receives the same credential, because nothing about the flow
// depends on process state.
func pollLogin(req pluginapi.AuthLoginPollRequest) (pluginapi.AuthLoginPollResponse, error) {
	if _, ok := parseLoginState(req.State, time.Now()); !ok {
		return pluginapi.AuthLoginPollResponse{
			Status:  pluginapi.AuthLoginStatusError,
			Message: "OpenCode Zen Free sign-in expired; start a new sign-in",
		}, nil
	}
	return pluginapi.AuthLoginPollResponse{
		Status: pluginapi.AuthLoginStatusSuccess,
		Auth:   authFor(publicCredential()),
	}, nil
}

// refreshCredential answers the same public credential. There is nothing to
// renew, and a host that asks anyway gets a valid auth rather than an error.
func refreshCredential(req pluginapi.AuthRefreshRequest) (pluginapi.AuthRefreshResponse, error) {
	c, err := readCredential(req.StorageJSON)
	if err != nil {
		return pluginapi.AuthRefreshResponse{}, pluginabi.NewError("reauth_required", "invalid OpenCode Zen Free credential; sign in again", http.StatusUnauthorized)
	}
	raw, _ := json.Marshal(c)
	metadata := refreshMetadata(req, c)
	metadata["email"] = authLabel
	metadata["type"] = credentialProvider
	metadata["oauth_provider"] = credentialProvider
	return pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:         credentialProvider,
			ID:               req.AuthID,
			FileName:         req.AuthID,
			Label:            authLabel,
			StorageJSON:      raw,
			Metadata:         metadata,
			NextRefreshAfter: noRefreshAfter(),
		},
		NextRefreshAfter: noRefreshAfter(),
	}, nil
}

// parseAuth recognizes an existing "type": "opencode-zen-free" auth file and
// republishes it.
func parseAuth(req pluginapi.AuthParseRequest) pluginapi.AuthParseResponse {
	if err := loginEntry(req.Provider); err != nil {
		return pluginapi.AuthParseResponse{}
	}
	var persisted struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(req.RawJSON, &persisted) != nil || persisted.Type != credentialProvider {
		return pluginapi.AuthParseResponse{}
	}
	c, err := readCredential(req.RawJSON)
	if err != nil {
		return pluginapi.AuthParseResponse{}
	}
	name := req.FileName
	if name == "" {
		name = req.Path
	}
	if name == "" {
		name = authFileName
	}
	a := authFor(c)
	a.ID = name
	a.FileName = req.FileName
	// Keep the file byte-for-byte: the credential is one public key, but the
	// same JSON object also holds the host-managed metadata the management API
	// writes (note, prefix, priority, and the rest). CPA rebuilds the runtime
	// record from this map on every reload, so returning identity keys alone
	// would make those fields look unpersisted the moment the file is re-read.
	a.StorageJSON = append([]byte(nil), req.RawJSON...)
	a.Metadata = persistedMetadata(req.RawJSON, c)
	a.Metadata["email"] = authLabel
	a.Metadata["type"] = credentialProvider
	a.Metadata["oauth_provider"] = credentialProvider
	return pluginapi.AuthParseResponse{Handled: true, Auth: a}
}

// persistedMetadata returns the host-managed fields of an auth file: everything
// the management API writes next to the credential (note, prefix, priority,
// proxy_url, headers, excluded models, model aliases, request retry, ...). CPA
// rebuilds the runtime record from this map on every reload, so a parser that
// answers with identity keys alone makes those fields read back as unpersisted.
//
// The credential's own fields are removed: they are carried by StorageJSON, and
// the host merges metadata over storage when it saves, so echoing them here
// would let a value captured at parse time overwrite a refreshed credential.
func persistedMetadata(raw []byte, own any) map[string]any {
	var metadata map[string]any
	if json.Unmarshal(raw, &metadata) != nil || metadata == nil {
		return make(map[string]any)
	}
	for _, key := range credentialJSONKeys(own) {
		delete(metadata, key)
	}
	return metadata
}

// refreshMetadata carries the host-managed fields through a token refresh. CPA
// saves the record returned here over the persisted file and treats a field the
// refresh omits as deleted, so dropping the note (or prefix, priority, ...)
// would erase it from the credential. The runtime map wins; fields only the
// file still holds fill the gaps.
func refreshMetadata(req pluginapi.AuthRefreshRequest, own any) map[string]any {
	metadata := make(map[string]any, len(req.Metadata))
	for key, value := range req.Metadata {
		metadata[key] = value
	}
	for key, value := range persistedMetadata(req.StorageJSON, own) {
		if _, exists := metadata[key]; !exists {
			metadata[key] = value
		}
	}
	for _, key := range credentialJSONKeys(own) {
		delete(metadata, key)
	}
	return metadata
}

// credentialJSONKeys lists the JSON names a credential struct owns, so
// persistedMetadata can keep them out of the host-managed map.
func credentialJSONKeys(own any) []string {
	t := reflect.TypeOf(own)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	keys := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		keys = append(keys, name)
	}
	return keys
}
