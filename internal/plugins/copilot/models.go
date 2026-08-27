package copilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/mapleafgo/codex-api-gateway/internal/logging"
	"github.com/mapleafgo/codex-api-gateway/internal/upstreamhttp"
	"golang.org/x/sync/singleflight"
)

const modelsTTL = 5 * time.Minute

// ModelInfo is a filtered model record used for Copilot protocol routing.
type ModelInfo struct {
	ID                 string   `json:"id"`
	SupportedEndpoints []string `json:"supported_endpoints"`
}

type modelsResponse struct {
	Data []json.RawMessage `json:"data"`
}

type modelRecord struct {
	ID                 string   `json:"id"`
	ModelPickerEnabled *bool    `json:"model_picker_enabled"`
	SupportedEndpoints []string `json:"supported_endpoints"`
	Capabilities       *struct {
		Type string `json:"type"`
	} `json:"capabilities"`
	Policy *struct {
		State string `json:"state"`
	} `json:"policy"`
}

type modelCache struct {
	http     *http.Client
	sf       singleflight.Group
	mu       sync.RWMutex
	models   map[string]*ModelInfo
	cachedAt time.Time
	ttl      time.Duration
	valid    bool
}

func newModelCache(hc *http.Client, ttl time.Duration) *modelCache {
	if hc == nil {
		hc = &http.Client{}
	}
	if ttl <= 0 {
		ttl = modelsTTL
	}
	return &modelCache{http: hc, ttl: ttl}
}

// Get returns the model directory within its TTL and collapses concurrent
// refreshes into one upstream request. Malformed individual entries are skipped.
func (c *modelCache) Get(ctx context.Context, endpoint, token string) (map[string]*ModelInfo, error) {
	c.mu.RLock()
	if c.valid && time.Since(c.cachedAt) < c.ttl {
		models := c.models
		c.mu.RUnlock()
		return models, nil
	}
	c.mu.RUnlock()

	result, err, _ := c.sf.Do("fetch", func() (any, error) {
		c.mu.RLock()
		if c.valid && time.Since(c.cachedAt) < c.ttl {
			models := c.models
			c.mu.RUnlock()
			return models, nil
		}
		c.mu.RUnlock()

		models, err := c.fetch(ctx, endpoint, token)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.models = models
		c.valid = true
		c.cachedAt = time.Now()
		c.mu.Unlock()
		return models, nil
	})
	if err != nil {
		return nil, err
	}
	return result.(map[string]*ModelInfo), nil
}

func (c *modelCache) fetch(ctx context.Context, endpoint, token string) (map[string]*ModelInfo, error) {
	started := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, upstreamhttp.ModelsURL(endpoint), nil)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	ApplyHeaders(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch models: status %d", resp.StatusCode)
	}

	var raw modelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}

	log := logging.FromContext(ctx)
	models := make(map[string]*ModelInfo, len(raw.Data))
	for i, item := range raw.Data {
		var m modelRecord
		if err := json.Unmarshal(item, &m); err != nil {
			log.Warn("Copilot 模型条目解码失败，已跳过", "index", i, "error", err)
			continue
		}
		// Zed filters picker-visible chat models; plan entitlement is left to
		// Copilot because restricted_to does not prove request authorization.
		if m.ModelPickerEnabled == nil || !*m.ModelPickerEnabled {
			continue
		}
		if m.Capabilities == nil || m.Capabilities.Type != "chat" {
			continue
		}
		if m.Policy != nil && m.Policy.State != "enabled" {
			continue
		}
		models[m.ID] = &ModelInfo{
			ID:                 m.ID,
			SupportedEndpoints: m.SupportedEndpoints,
		}
	}
	log.Debug("Copilot 模型目录拉取完成",
		"endpoint", endpoint,
		"upstream_models", len(raw.Data),
		"available_models", len(models),
		"elapsed", time.Since(started).String())
	return models, nil
}
