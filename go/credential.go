package main

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// credentialProvider is the provider key every credential this plugin writes
// carries, and the identifier CPA routes executions and quota by.
const credentialProvider = "opencode-zen-free"

// publicKey is the one shared credential Zen's free tier accepts. It is not a
// secret: every OpenCode install sends it, and a key made under a personal
// OpenCode account does nothing on the free tier.
const publicKey = "public"

// authFileName is the one credential this plugin stores. A second sign-in
// replaces it rather than adding an account, because there is only one public
// identity.
const authFileName = credentialProvider + "-public.json"

// credential is the persisted auth storage. There is no token to renew: the
// public key never expires, so the file records only which identity it is.
type credential struct {
	Access string `json:"access"`
}

func readCredential(b []byte) (credential, error) {
	var c credential
	if json.Unmarshal(b, &c) != nil {
		return c, errors.New("invalid OpenCode Zen Free credential")
	}
	if strings.TrimSpace(c.Access) != publicKey {
		return c, errors.New("invalid OpenCode Zen Free credential")
	}
	return c, nil
}

func publicCredential() credential { return credential{Access: publicKey} }

// authFor maps the public credential to the auth record CPA persists. The ID is
// fixed, so signing in again replaces the same auth instead of adding a second
// one for an identity that cannot differ.
func authFor(c credential) pluginapi.AuthData {
	raw, _ := json.Marshal(c)
	return pluginapi.AuthData{
		Provider:    credentialProvider,
		ID:          authFileName,
		FileName:    authFileName,
		Label:       authLabel,
		StorageJSON: raw,
		Metadata: map[string]any{
			"email":          authLabel,
			"type":           credentialProvider,
			"oauth_provider": credentialProvider,
		},
		NextRefreshAfter: noRefreshAfter(),
	}
}

// authLabel is what the credential card shows. There is one public identity, so
// the card names the service rather than an account.
const authLabel = "OpenCode Zen Free"

// noRefreshAfter keeps the host from scheduling a renewal this credential never
// needs. It is re-stated by every refresh answer.
func noRefreshAfter() time.Time { return time.Now().Add(10 * 365 * 24 * time.Hour) }
