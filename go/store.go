package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

// The last good list outlives the process. A restart that could not read Zen's
// live list would otherwise fall back to the shipped bootstrap, which goes stale
// as Zen retires models and makes others free. Keeping the list beside the auth
// material means a network hiccup at start-up costs nothing, and the bootstrap is
// only the first-run answer.
//
// The document holds no credential: model ids, names, limits and reasoning
// levels are public. Every failure here is silent; a list that cannot be saved
// still serves this process, and losing it costs one live read.

// modelsFileName is the document's name inside the auth directory.
const modelsFileName = ".opencode-zen-free-models.json"

type storedModels struct {
	Version int        `json:"version"`
	SavedAt time.Time  `json:"saved_at"`
	Models  []zenModel `json:"models"`
}

var storeMu sync.Mutex
var storeLoaded bool

func modelsFilePath() string {
	dir := knownAuthDir()
	if strings.TrimSpace(dir) == "" {
		return ""
	}
	return filepath.Join(dir, modelsFileName)
}

// ensureStoredModels reads the last good list once per process, unless a live
// list has already been read. It never overwrites a fresher answer.
func ensureStoredModels() {
	path := modelsFilePath()
	storeMu.Lock()
	if storeLoaded {
		storeMu.Unlock()
		return
	}
	storeLoaded = true
	if path == "" {
		storeMu.Unlock()
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		storeMu.Unlock()
		return
	}
	var document storedModels
	if json.Unmarshal(raw, &document) != nil || len(document.Models) == 0 {
		storeMu.Unlock()
		return
	}
	loaded := make(map[string]zenModel, len(document.Models))
	for _, m := range document.Models {
		if key := strings.TrimSpace(m.Key); key != "" {
			loaded[key] = m
		}
	}
	if len(loaded) == 0 {
		storeMu.Unlock()
		return
	}
	zen.mu.Lock()
	if !zen.listed {
		zen.known = loaded
		zen.stored = true
	}
	zen.mu.Unlock()
	storeMu.Unlock()
}

// saveStoredModels writes the list that was just read. The document is replaced
// atomically so a reader never sees half a file.
func saveStoredModels(models map[string]zenModel) {
	path := modelsFilePath()
	if path == "" {
		return
	}
	list := modelSlice(models)
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	document := storedModels{Version: 1, SavedAt: time.Now().UTC(), Models: list}
	raw, err := json.Marshal(document)
	if err != nil {
		return
	}
	storeMu.Lock()
	defer storeMu.Unlock()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, modelsFileName+".*")
	if err != nil {
		return
	}
	name := tmp.Name()
	if _, err = tmp.Write(raw); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(name)
		return
	}
	if err = os.Chmod(name, 0o600); err != nil {
		_ = os.Remove(name)
		return
	}
	if err = os.Rename(name, path); err != nil {
		_ = os.Remove(name)
	}
}

// sameModels reports a list that did not change, so an unchanged answer does not
// rewrite the document every minute.
func sameModels(a, b map[string]zenModel) bool { return reflect.DeepEqual(a, b) }
