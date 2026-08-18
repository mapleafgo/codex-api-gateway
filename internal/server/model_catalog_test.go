package server

import (
	"encoding/json"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

func TestServerModelCatalogJSON(t *testing.T) {
	cfg := &config.Config{
		ModelOverrides: map[string]config.ModelOverride{
			"alpha": {},
			"beta":  {},
		},
		ModelSlugOrder: []string{"alpha", "beta"},
	}
	srv := New(cfg)
	defer srv.Close()

	body, err := srv.ModelCatalogJSON()
	if err != nil {
		t.Fatalf("ModelCatalogJSON: %v", err)
	}
	var resp model.CodexModelsResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(resp.Models) != 2 {
		t.Fatalf("models=%d, want 2: %s", len(resp.Models), body)
	}
	if resp.Models[0].Slug != "alpha" || resp.Models[1].Slug != "beta" {
		t.Fatalf("slug order=%s,%s, want alpha,beta", resp.Models[0].Slug, resp.Models[1].Slug)
	}
}
