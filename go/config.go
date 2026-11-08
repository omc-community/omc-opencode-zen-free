package main

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// defaultOpenCodeVersion is the OpenCode release the client claims to be when
// its own updater cannot be read. Zen's free tier refuses the exact User-Agent
// older OpenCode releases sent, so the value is asked of OpenCode's updater at
// most every six hours and this one is only the floor.
const defaultOpenCodeVersion = "2.2.5"

// defaultMaxConcurrency matches the reference plugin's own limit. Zen's free
// tier is shared by every user, so a small per-credential ceiling is the
// neighbourly default.
const defaultMaxConcurrency = 2

type config struct {
	// MaxConcurrency bounds simultaneous requests per credential.
	MaxConcurrency int `yaml:"max_concurrency"`
	// OpenCodeVersion is the version the client claims when OpenCode's updater
	// cannot be read. A newer release the updater names is used instead; this
	// value is never downgraded below the one it names.
	OpenCodeVersion string `yaml:"opencode_version"`
	// ExcludeModels are model ids this plugin never advertises.
	ExcludeModels []string `yaml:"exclude_models"`
}

var configMu sync.RWMutex
var currentConfig = defaults()

func defaults() config {
	return config{
		MaxConcurrency:  defaultMaxConcurrency,
		OpenCodeVersion: defaultOpenCodeVersion,
	}
}

func getConfig() config { configMu.RLock(); defer configMu.RUnlock(); return currentConfig }

func configure(raw []byte) error {
	c := defaults()
	if len(raw) > 0 {
		if err := yaml.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("invalid plugin configuration")
		}
	}
	if c.MaxConcurrency < 1 || c.MaxConcurrency > 100 {
		return fmt.Errorf("invalid opencode-zen-free plugin configuration: max_concurrency must be 1-100")
	}
	var err error
	if c.OpenCodeVersion, err = cleanVersion(c.OpenCodeVersion, "opencode_version"); err != nil {
		return err
	}
	if c.ExcludeModels, err = cleanModelList(c.ExcludeModels); err != nil {
		return err
	}
	configMu.Lock()
	currentConfig = c
	configMu.Unlock()
	// A configuration that names a newer OpenCode release than the last answer
	// takes effect at once; the version cache never downgrades it.
	adoptConfiguredVersion()
	return nil
}

// cleanModelList normalizes the model ids a config names: trimmed, lowercased,
// deduplicated, and kept to ids a model can actually have. A value that cannot
// be a model id is a configuration error rather than a silently ignored line.
func cleanModelList(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, raw := range values {
		for _, part := range strings.Split(raw, ",") {
			id := strings.ToLower(strings.TrimSpace(part))
			if id == "" {
				continue
			}
			if len(id) > 128 || !modelIDLike.MatchString(id) {
				return nil, fmt.Errorf("invalid opencode-zen-free plugin configuration: exclude_models entry %q is not a model id", part)
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
	}
	return out, nil
}

// modelExcluded reports a model the configuration keeps out of the published
// list, whatever Zen's own listing says about it.
func modelExcluded(model string) bool {
	want := strings.ToLower(strings.TrimSpace(model))
	if want == "" {
		return false
	}
	for _, id := range getConfig().ExcludeModels {
		if id == want {
			return true
		}
	}
	return false
}

// modelIDLike is the shape of a model id: one leading alphanumeric, then the
// characters every Zen model id uses.
var modelIDLike = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// versionLike is the shape OpenCode's updater answers with. A value that is not
// a release version is never put in a User-Agent.
var versionLike = regexp.MustCompile(`^\d{1,4}\.\d{1,4}\.\d{1,6}$`)

// cleanVersion keeps a version string usable in a User-Agent: digits and dots,
// in the three-part shape OpenCode releases use.
func cleanVersion(value, field string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("invalid opencode-zen-free plugin configuration: %s must not be empty", field)
	}
	if !versionLike.MatchString(v) {
		return "", fmt.Errorf("invalid opencode-zen-free plugin configuration: %s must be a release version such as 2.2.5", field)
	}
	return v, nil
}
