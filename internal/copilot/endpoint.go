package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/logging"
)

const discoverTimeout = 10 * time.Second

// graphqlURL is mutable so tests can point discovery at a local server.
var graphqlURL = "https://api.github.com/graphql"

const discoverQuery = `{
  "query": "query { viewer { copilotEndpoints { api } } }"
}`

// discoverAPIEndpoint queries GitHub GraphQL for the Copilot API endpoint and
// returns DefaultEndpoint together with the discovery error on failure.
func discoverAPIEndpoint(ctx context.Context, hc *http.Client, githubToken string) (string, error) {
	return discoverAPIEndpointWithDefault(ctx, hc, githubToken, DefaultEndpoint)
}

func discoverAPIEndpointWithDefault(ctx context.Context, hc *http.Client, githubToken, fallbackEndpoint string) (string, error) {
	log := logging.FromContext(ctx)
	started := time.Now()
	dctx, cancel := context.WithTimeout(ctx, discoverTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(dctx, http.MethodPost, graphqlURL, strings.NewReader(discoverQuery))
	if err != nil {
		log.Warn("Copilot endpoint 发现：构造请求失败，回退默认地址",
			"error", err, "default", fallbackEndpoint, "elapsed", time.Since(started).String())
		return fallbackEndpoint, err
	}
	req.Header.Set("Authorization", "Bearer "+githubToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := hc.Do(req)
	if err != nil {
		log.Warn("Copilot endpoint 发现：GraphQL 请求失败，回退默认地址",
			"error", err, "default", fallbackEndpoint, "elapsed", time.Since(started).String())
		return fallbackEndpoint, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Warn("Copilot endpoint 发现：GraphQL 返回非 200，回退默认地址",
			"status", resp.StatusCode, "default", fallbackEndpoint, "elapsed", time.Since(started).String())
		return fallbackEndpoint, fmt.Errorf("graphql status %d", resp.StatusCode)
	}

	var result struct {
		Data struct {
			Viewer struct {
				CopilotEndpoints struct {
					API string `json:"api"`
				} `json:"copilotEndpoints"`
			} `json:"viewer"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		log.Warn("Copilot endpoint 发现：解析响应失败，回退默认地址",
			"error", err, "default", fallbackEndpoint, "elapsed", time.Since(started).String())
		return fallbackEndpoint, err
	}
	if len(result.Errors) > 0 {
		log.Warn("Copilot endpoint 发现：GraphQL 返回错误，回退默认地址",
			"error", result.Errors[0].Message, "default", fallbackEndpoint, "elapsed", time.Since(started).String())
		return fallbackEndpoint, fmt.Errorf("graphql error: %s", result.Errors[0].Message)
	}

	endpoint := result.Data.Viewer.CopilotEndpoints.API
	if endpoint == "" {
		log.Warn("Copilot endpoint 发现：响应中 api 字段为空，回退默认地址",
			"default", fallbackEndpoint, "elapsed", time.Since(started).String())
		return fallbackEndpoint, fmt.Errorf("empty api endpoint")
	}
	log.Debug("Copilot endpoint 发现成功", "endpoint", endpoint, "elapsed", time.Since(started).String())
	return endpoint, nil
}
