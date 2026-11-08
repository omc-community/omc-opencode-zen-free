package main

import (
	_ "embed"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const pluginStoreID = "omc-opencode-zen-free"
const resourcePrefix = "/v0/resource/plugins/" + pluginStoreID

// publicAssetBaseURL is the published location of the same artwork the resource
// routes serve. OAuth entry logos use it because a panel reads a plugin logo as a
// document, and the store manifests are URLs too.
const publicAssetBaseURL = "https://raw.githubusercontent.com/omc-community/omc-opencode-zen-free/refs/heads/main/assets/"

// pluginLogo is the plugin's own face in a store: the OMC sticker that carries
// the OpenCode Zen mark.
const pluginLogo = "logo.png"

// zenMark is the service's own mark, carried by the sign-in card.
const zenMark = "zen.png"

//go:embed assets/logo.png
var pluginLogoPNG []byte

//go:embed assets/zen.png
var zenMarkPNG []byte

// resourceAssetURL is the public address of one embedded mark. Only the names
// registered below are addressable, so the set of URLs the plugin can publish stays
// equal to the set of bytes it ships.
func resourceAssetURL(name string) string {
	switch name {
	case pluginLogo, zenMark:
		return publicAssetBaseURL + name
	default:
		return ""
	}
}

// handleManagementResource serves only deployed public bytes. It deliberately has
// no host callback, auth directory, configuration or OAuth-state dependency.
func handleManagementResource(req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	var body []byte
	contentType := "image/png"
	cacheControl := "public, max-age=86400"
	switch req.Path {
	case resourcePrefix + "/" + pluginLogo:
		body = pluginLogoPNG
	case resourcePrefix + "/" + zenMark:
		body = zenMarkPNG
	case omcResourcePath:
		body = []byte(omcDescriptorJSON)
		contentType = "application/json; charset=utf-8"
		cacheControl = "no-cache"
	default:
		return pluginapi.ManagementResponse{StatusCode: http.StatusNotFound, Body: []byte("not found")}, nil
	}
	if req.Method != http.MethodGet {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusMethodNotAllowed,
			Headers:    http.Header{"Allow": {http.MethodGet}},
			Body:       []byte("method not allowed"),
		}, nil
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":            {contentType},
			"Cache-Control":           {cacheControl},
			"X-Content-Type-Options":  {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'; style-src 'unsafe-inline'"},
		},
		Body: append([]byte(nil), body...),
	}, nil
}
