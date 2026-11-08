package main

import (
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Zen's free tier publishes no remaining-quota data: availability and rate
// limits are set by Zen, and an account has no credit balance to read. The
// plugin therefore describes the plan and reports no buckets rather than
// inventing a fraction. A client that needs to know whether an allowance is
// spent learns it from the 429 FreeUsageLimitError on the next request.
func quotaDescription() pluginapi.QuotaDescribeResponse {
	return pluginapi.QuotaDescribeResponse{
		SupportedProviders: []string{credentialProvider},
		DisplayName:        authLabel,
		SupportsReset:      false,
	}
}

func quotaFetch(req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	if _, err := readCredential(req.StorageJSON); err != nil {
		return pluginapi.QuotaFetchResponse{}, pluginabi.NewError("invalid_auth", "invalid OpenCode Zen Free credential", http.StatusUnauthorized)
	}
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: authLabel},
		Groups:       []pluginapi.QuotaGroup{},
	}, nil
}

func quotaReset() (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{}, pluginabi.NewError("not_supported", "OpenCode Zen's free tier has no reset to request; its daily allowance resets at 00:00 UTC", http.StatusNotImplemented)
}
