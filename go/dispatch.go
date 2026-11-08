package main

import (
	"encoding/json"
	"net/http"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

var pluginVersion = "0.1.3"

type rpcAuthStart struct {
	pluginapi.AuthLoginStartRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcAuthPoll struct {
	pluginapi.AuthLoginPollRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcAuthRefresh struct {
	pluginapi.AuthRefreshRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcModel struct {
	pluginapi.AuthModelRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcQuota struct {
	pluginapi.QuotaFetchRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcManagement struct {
	pluginapi.ManagementRequest
	CallbackID string `json:"host_callback_id"`
}
type rpcExecution struct {
	pluginapi.ExecutorRequest
	CallbackID string `json:"host_callback_id"`
	StreamID   string `json:"stream_id"`
}

func unmarshal(raw []byte, out any) error {
	if len(raw) == 0 {
		raw = []byte("{}")
	}
	if json.Unmarshal(raw, out) != nil {
		return pluginabi.NewError("invalid_request", "invalid plugin request", 400)
	}
	return nil
}

// authEntries publishes the one keyless entry. Its identifier is also the value
// persisted as oauth_provider, so a console can attribute the credential to its
// card and a re-login finds the same entry.
func authEntries() []pluginapi.AuthProviderEntry {
	return []pluginapi.AuthProviderEntry{
		{Identifier: credentialProvider, Name: authLabel, Logo: resourceAssetURL(zenMark)},
	}
}

func registration() any {
	return map[string]any{
		"schema_version": pluginabi.SchemaVersion,
		"metadata": pluginapi.Metadata{
			Name:             "OMC OpenCode Zen Free",
			Version:          pluginVersion,
			Author:           "JUNZE",
			GitHubRepository: "https://github.com/omc-community/omc-opencode-zen-free",
			Logo:             resourceAssetURL(pluginLogo),
			ConfigFields: []pluginapi.ConfigField{
				{Name: "max_concurrency", Type: pluginapi.ConfigFieldTypeInteger, Default: defaultMaxConcurrency, Description: "Maximum simultaneous requests per credential (default 2). Zen's free tier is shared by every user, so a small ceiling is the neighbourly default."},
				{Name: "opencode_version", Type: pluginapi.ConfigFieldTypeString, Default: defaultOpenCodeVersion, Description: "OpenCode release the client claims in its User-Agent when OpenCode's updater cannot be read (default 2.2.5). A newer release the updater names is used instead; this value is never downgraded."},
				{Name: "exclude_models", Type: pluginapi.ConfigFieldTypeArray, Default: []string{}, Description: "Model ids this plugin never advertises. Zen's free list changes; naming an id here keeps CPA from routing it to this credential at all."},
			},
		},
		"capabilities": map[string]any{
			"auth_provider":           true,
			"model_provider":          true,
			"executor":                true,
			"executor_model_scope":    "oauth",
			"executor_input_formats":  []string{"openai"},
			"executor_output_formats": []string{"openai"},
			"quota_provider":          true,
			"management_api":          true,
		},
	}
}

func handleMethod(method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		var r struct {
			ConfigYAML []byte `json:"config_yaml"`
		}
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		if err := configure(r.ConfigYAML); err != nil {
			return nil, pluginabi.NewError("invalid_config", err.Error(), 400)
		}
		return registration(), nil
	case "plugin.quiesce", "plugin.shutdown":
		return map[string]any{}, nil
	case "auth.identifier":
		// entries is schema 10; identifiers alone keep older hosts working.
		// credential_providers tells the host that credentials typed
		// "opencode-zen-free" belong to this plugin.
		return map[string]any{
			"identifier":           credentialProvider,
			"identifiers":          []string{credentialProvider},
			"credential_providers": []string{credentialProvider},
			"entries":              authEntries(),
		}, nil
	case "executor.identifier", "quota.identifier":
		return map[string]string{"identifier": credentialProvider}, nil
	case "auth.parse":
		var r pluginapi.AuthParseRequest
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		return parseAuth(r), nil
	case "auth.login.start":
		var r rpcAuthStart
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		v, err := startLogin(r.AuthLoginStartRequest)
		if err != nil {
			return nil, pluginabi.NewError("auth_error", err.Error(), 400)
		}
		return v, nil
	case "auth.login.poll":
		var r rpcAuthPoll
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		return pollLogin(r.AuthLoginPollRequest)
	case "auth.refresh":
		var r rpcAuthRefresh
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		return refreshCredential(r.AuthRefreshRequest)
	case "management.register":
		return map[string]any{"resources": []map[string]any{
			{"path": "/" + pluginLogo, "menu": "", "description": "Plugin sticker mark: the plugin's face in a store."},
			{"path": "/" + zenMark, "menu": "", "description": "OpenCode Zen mark for the sign-in entry."},
			{"path": "/omc.json", "menu": "", "description": "Public, optional OMC plugin declaration."},
		}}, nil
	case "management.handle":
		var r rpcManagement
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return handleManagementResource(r.ManagementRequest)
	case "model.static":
		return staticModels(), nil
	case "model.for_auth":
		var r rpcModel
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		v, err := modelsForAuth(r.AuthModelRequest, cpaSender{callbackID: r.CallbackID})
		if err != nil {
			return nil, pluginabi.NewError("model_error", err.Error(), 502)
		}
		return v, nil
	case "quota.describe":
		return quotaDescription(), nil
	case "quota.fetch":
		var r rpcQuota
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		setAuthDir(r.Host.AuthDir)
		return quotaFetch(r.QuotaFetchRequest)
	case "quota.reset":
		return quotaReset()
	case "executor.execute":
		var r rpcExecution
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return execute(r.ExecutorRequest, cpaSender{callbackID: r.CallbackID})
	case "executor.execute_stream":
		var r rpcExecution
		if err := unmarshal(raw, &r); err != nil {
			return nil, err
		}
		return executeStream(r.ExecutorRequest, r.CallbackID, r.StreamID, cpaSender{callbackID: r.CallbackID})
	case "executor.count_tokens":
		return countTokens()
	case "executor.http_request":
		return nil, pluginabi.NewError("not_supported", "unsupported executor HTTP request", http.StatusNotImplemented)
	default:
		return nil, pluginabi.NewError("unknown_method", "unknown plugin method", http.StatusNotImplemented)
	}
}

// setAuthDir records where CPA keeps auth material, for the persisted model list.
// An empty value never overwrites a known directory; a new one re-reads the
// stored list on its next use.
var authDirState struct {
	sync.RWMutex
	dir string
}

func setAuthDir(dir string) {
	if dir == "" {
		return
	}
	authDirState.Lock()
	changed := authDirState.dir != dir
	authDirState.dir = dir
	authDirState.Unlock()
	if changed {
		storeMu.Lock()
		storeLoaded = false
		storeMu.Unlock()
	}
}

func knownAuthDir() string {
	authDirState.RLock()
	defer authDirState.RUnlock()
	return authDirState.dir
}
