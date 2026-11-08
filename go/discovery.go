package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// zenBase is the inference API Zen's free tier answers at. The same request
	// OpenCode sends answers 403 FreeTierError at the old /zen/v1 path.
	zenBase = "https://opencode.ai/inference/v1"
	// zenChatBase is where OpenCode 2 sends a chat request: models.dev's v2
	// catalog gives every OpenCode model this base, free ones included.
	zenChatBase = "https://opencode.ai/inference/openai/v1"
	// zenChatPath is the Chat Completions path under zenChatBase.
	zenChatPath = "/chat/completions"
	// catalogURL is models.dev's v2 catalog, which carries each model's limits,
	// modalities, reasoning options and package.
	catalogURL = "https://models.opencode.ai/api.json"
	// versionURL is OpenCode's updater. Zen's free tier refuses the exact
	// User-Agent older releases sent, so the version is asked of this endpoint
	// rather than frozen in a release.
	versionURL = "https://opencode.ai/update/api/latest/cli/npm"
)

// listingTTL is how long a read list is reused. Zen's list changes when a model
// is made free or retired; one minute keeps the panel current without asking
// three upstream documents on every request. A request for a model the last list
// did not have re-reads sooner (relistAfter).
const listingTTL = time.Minute

// relistAfter is how long a request for a model the last list did not have
// waits before it asks Zen's list again: a model Zen made free since is served
// at once, and a model typed in by hand does not ask the list on every request.
const relistAfter = 60 * time.Second

// versionEvery is how often OpenCode's updater is asked. A fixed version goes
// stale as OpenCode updates itself, so six hours matches the reference plugin.
const versionEvery = 6 * time.Hour

var (
	errNoModelList = errors.New("OpenCode Zen returned no model list")
	errNoCatalog   = errors.New("models.dev returned no OpenCode catalog")
)

// discoveryState is the process's view of Zen's catalogue. The live list is
// authoritative; the bootstrap tables stand in only when it cannot be read.
type discoveryState struct {
	mu          sync.Mutex
	known       map[string]zenModel
	protocols   map[string]string
	catalog     map[string]catalogModel
	providerNPM string
	listed      bool
	listedAt    time.Time
	attempted   time.Time

	version       string
	versionAt     time.Time
	versionFlight chan struct{}
}

var zen = &discoveryState{
	known:       bootstrapChatModels(),
	protocols:   cloneStrings(bootstrapProtocols),
	catalog:     map[string]catalogModel{},
	providerNPM: sdkChat,
	version:     defaultOpenCodeVersion,
}

func cloneModels(in map[string]zenModel) map[string]zenModel {
	out := make(map[string]zenModel, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneStrings(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneCatalog(in map[string]catalogModel) map[string]catalogModel {
	out := make(map[string]catalogModel, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// currentModels is the list the last read gave, for a caller that must not
// perform I/O.
func currentModels() map[string]zenModel {
	zen.mu.Lock()
	defer zen.mu.Unlock()
	return cloneModels(zen.known)
}

// discover reads Zen's list, the catalog and the docs, and answers the free
// chat models they describe. A failed listing keeps the last good list (and the
// bootstrap list when there is none). force skips the reuse window.
func discover(sender httpSender, force bool) map[string]zenModel {
	ensureStoredModels()
	zen.mu.Lock()
	if !force && zen.listed && time.Since(zen.listedAt) < listingTTL {
		out := cloneModels(zen.known)
		zen.mu.Unlock()
		return out
	}
	previous := cloneModels(zen.known)
	zen.mu.Unlock()

	version := currentVersion(sender)
	listed, err := fetchListing(sender, version)
	if err != nil {
		zen.mu.Lock()
		zen.attempted = time.Now()
		out := cloneModels(zen.known)
		zen.mu.Unlock()
		return out
	}

	zen.mu.Lock()
	protocols := cloneStrings(zen.protocols)
	catalog := cloneCatalog(zen.catalog)
	providerNPM := zen.providerNPM
	zen.mu.Unlock()

	// A current listing carries each model's protocol, limits, modalities and
	// price, so the catalog and the docs are only read for an older listing
	// that carries ids alone. The catalog is several megabytes; asking it every
	// minute would be a waste of Zen's bandwidth and the host's.
	needMetadata := false
	for _, item := range listed {
		if len(item.API) == 0 {
			needMetadata = true
			break
		}
	}
	partial := false
	if needMetadata {
		if provider, err := fetchCatalog(sender); err == nil {
			catalog = provider.Models
			providerNPM = firstNonEmpty(provider.NPM, providerNPM)
		} else {
			partial = true
		}
	}

	discovered := buildModels(listed, catalog, protocols, providerNPM)
	if partial {
		// Keep a model verified earlier only while it is still in Zen's live
		// list and neither metadata source describes it. Said as a fallback,
		// the reference kept the last list whole instead, and a user who cannot
		// reach the catalog or the docs never saw a model made free since.
		for _, item := range listed {
			id := item.ID
			if _, ok := discovered[id]; ok {
				continue
			}
			if _, ok := catalog[id]; ok {
				continue
			}
			if _, ok := protocols[id]; ok {
				continue
			}
			if known, ok := previous[id]; ok {
				discovered[id] = known
			}
		}
	}

	zen.mu.Lock()
	zen.known = discovered
	zen.protocols = protocols
	zen.catalog = catalog
	zen.providerNPM = providerNPM
	zen.listed = true
	zen.listedAt = time.Now()
	zen.attempted = time.Now()
	out := cloneModels(discovered)
	zen.mu.Unlock()
	if !sameModels(previous, discovered) {
		saveStoredModels(out)
	}
	return out
}

// fetchListing reads Zen's live model list as OpenCode asks it.
func fetchListing(sender httpSender, version string) ([]listedModel, error) {
	headers := http.Header{
		"Authorization":     {"Bearer " + publicKey},
		"User-Agent":        {userAgent(version)},
		"X-Opencode-Client": {"cli"},
		"Accept":            {"application/json"},
	}
	status, body, err := fetch(sender, zenBase+"/models", headers)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, errNoModelList
	}
	return parseListing(body)
}

func fetchCatalog(sender httpSender) (catalogProvider, error) {
	status, body, err := fetch(sender, catalogURL, http.Header{"Accept": {"application/json"}})
	if err != nil {
		return catalogProvider{}, err
	}
	if status < 200 || status >= 300 {
		return catalogProvider{}, errNoCatalog
	}
	return parseCatalog(body)
}

func fetch(sender httpSender, target string, headers http.Header) (int, []byte, error) {
	resp, err := sender.Do(pluginapi.HTTPRequest{Method: http.MethodGet, URL: target, Headers: headers})
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, resp.Body, nil
}

// userAgent is the string OpenCode sends on every protocol:
// opencode/<channel>/<version>/<name>.
func userAgent(version string) string {
	return "opencode/latest/" + version + "/cli"
}

// currentVersion is the OpenCode release this request claims to be. It is the
// configured floor, raised by what OpenCode's updater names, and asked at most
// every six hours. A failed ask keeps the last answer and does not hammer the
// endpoint.
func currentVersion(sender httpSender) string {
	zen.mu.Lock()
	if !zen.versionAt.IsZero() && time.Since(zen.versionAt) < versionEvery {
		v := zen.version
		zen.mu.Unlock()
		return v
	}
	if flight := zen.versionFlight; flight != nil {
		zen.mu.Unlock()
		<-flight
		zen.mu.Lock()
		v := zen.version
		zen.mu.Unlock()
		return v
	}
	flight := make(chan struct{})
	zen.versionFlight = flight
	current := zen.version
	zen.mu.Unlock()

	found, ok := latestVersion(sender, current)

	zen.mu.Lock()
	if ok && newer(found, zen.version) {
		zen.version = found
	}
	zen.versionAt = time.Now()
	zen.versionFlight = nil
	answer := zen.version
	close(flight)
	zen.mu.Unlock()
	return answer
}

// latestVersion asks OpenCode's updater for the latest release. A stale mirror
// that answers an older version is never taken.
func latestVersion(sender httpSender, current string) (string, bool) {
	target := versionURL + "?current=" + url.QueryEscape(current)
	status, body, err := fetch(sender, target, http.Header{"Accept": {"application/json"}})
	if err != nil || status < 200 || status >= 300 {
		return "", false
	}
	var answer struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &answer) != nil {
		return "", false
	}
	version := strings.TrimSpace(answer.Version)
	if !versionLike.MatchString(version) || !newer(version, defaultOpenCodeVersion) {
		return "", false
	}
	return version, true
}

// newer reports whether a is b or later, comparing three numeric parts.
func newer(a, b string) bool {
	x := strings.Split(a, ".")
	y := strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var xi, yi int
		if i < len(x) {
			for _, r := range x[i] {
				if r < '0' || r > '9' {
					return false
				}
				xi = xi*10 + int(r-'0')
			}
		}
		if i < len(y) {
			for _, r := range y[i] {
				if r < '0' || r > '9' {
					return false
				}
				yi = yi*10 + int(r-'0')
			}
		}
		if xi != yi {
			return xi > yi
		}
	}
	return true
}

// adoptConfiguredVersion raises the claimed version to one the configuration
// names. It never lowers the answer OpenCode's updater gave.
func adoptConfiguredVersion() {
	configured := getConfig().OpenCodeVersion
	zen.mu.Lock()
	if newer(configured, zen.version) {
		zen.version = configured
	}
	zen.mu.Unlock()
}

// staticModels publishes the shipped list, which is what a host shows before a
// credential has been signed in.
func staticModels() pluginapi.ModelResponse {
	return modelResponse(offerable(modelSlice(currentModels())))
}

// modelsForAuth reads the credential's list from Zen. The public credential is
// the only one, so every credential sees the same list.
func modelsForAuth(req pluginapi.AuthModelRequest, sender httpSender) (pluginapi.ModelResponse, error) {
	if _, err := readCredential(req.StorageJSON); err != nil {
		return pluginapi.ModelResponse{}, err
	}
	return modelResponse(offerable(modelSlice(discover(sender, false)))), nil
}

// findModel looks a requested model up by key, case-insensitively. A miss
// refreshes the list once, at most once a minute, before falling back to the
// shipped list. A model the configuration excludes is never found: a client
// that still asks for it (CPA may hold a cached list) is answered as an unknown
// model, which the host reads as a credential-model mismatch and routes
// elsewhere.
func findModel(sender httpSender, model string) (zenModel, bool) {
	want := strings.ToLower(strings.TrimSpace(model))
	if want == "" || modelExcluded(want) {
		return zenModel{}, false
	}
	look := func(models map[string]zenModel) (zenModel, bool) {
		for key, m := range models {
			if strings.ToLower(key) == want {
				return m, true
			}
		}
		return zenModel{}, false
	}
	if m, ok := look(discover(sender, false)); ok {
		return m, true
	}
	zen.mu.Lock()
	attempted := zen.attempted
	listed := zen.listed
	zen.mu.Unlock()
	if time.Since(attempted) >= relistAfter {
		if m, ok := look(discover(sender, true)); ok {
			return m, true
		}
	}
	// The shipped list stands in only while Zen's own list has never been read.
	// Once a live list has been read, a model it left out is not free now, and
	// serving it from the bootstrap would earn the same refusal again.
	if !listed {
		for _, seed := range bootstrapCatalogue {
			if strings.ToLower(seed.Key) == want && bootstrapProtocols[seed.Key] == "chat" {
				return modelFromSeed(seed), true
			}
		}
	}
	return zenModel{}, false
}

// modelSlice is a map's models as a slice, so a response can be sorted.
func modelSlice(models map[string]zenModel) []zenModel {
	out := make([]zenModel, 0, len(models))
	for _, m := range models {
		out = append(out, m)
	}
	return out
}
