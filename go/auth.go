package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	return pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:         credentialProvider,
			ID:               req.AuthID,
			FileName:         req.AuthID,
			Label:            authLabel,
			StorageJSON:      raw,
			Metadata:         map[string]any{"email": authLabel, "type": credentialProvider, "oauth_provider": credentialProvider},
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
	var metadata struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(req.RawJSON, &metadata) != nil || metadata.Type != credentialProvider {
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
	return pluginapi.AuthParseResponse{Handled: true, Auth: a}
}
