package plugin

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

type fakePlugin struct {
	descriptor Descriptor
	source     config.Source
	backend    Backend
}

func (p fakePlugin) Descriptor() Descriptor { return p.descriptor }

func (p fakePlugin) ValidateSource(src config.Source) error {
	p.source = src
	if v, ok := src.Options["token"].(string); !ok || v == "" {
		return errors.New("token required")
	}
	return nil
}

func (p fakePlugin) Backend() Backend { return p.backend }

type recordingBackend struct {
	events []UpstreamEvent
}

func (b *recordingBackend) Execute(context.Context, []byte, config.Source, *config.Config, func(model.SSEEvent) error, func(UpstreamEvent), int) error {
	b.events = append(b.events, UpstreamEvent{})
	return nil
}

func TestRegistryConstructionAndLookup(t *testing.T) {
	p := fakePlugin{descriptor: validDescriptor(), backend: &recordingBackend{}}
	reg, err := New(p)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	got, ok := reg.Get("test")
	if !ok || got.Descriptor().ID != p.Descriptor().ID {
		t.Fatalf("Get() = %v, %v", got, ok)
	}
	descs := reg.Descriptors()
	if len(descs) != 1 || descs[0].ID != "test" {
		t.Fatalf("Descriptors() = %+v", descs)
	}
}

func TestRegistryRejectsInvalidPlugins(t *testing.T) {
	tests := []string{"empty id", "nil backend"}
	for _, name := range tests {
		t.Run(name, func(t *testing.T) {
			d := validDescriptor()
			var backend Backend = &recordingBackend{}
			if name == "empty id" {
				d.ID = ""
			} else {
				backend = nil
			}
			if _, err := New(fakePlugin{descriptor: d, backend: backend}); err == nil {
				t.Fatal("New() expected error")
			}
		})
	}
}

func TestRegistryValidateSource(t *testing.T) {
	p := fakePlugin{descriptor: validDescriptor(), backend: &recordingBackend{}}
	reg, _ := New(p)
	if err := reg.ValidateSource(config.Source{Name: "source", Backend: "missing"}); err == nil ||
		!strings.Contains(err.Error(), `unknown backend "missing"`) {
		t.Fatalf("unknown backend error = %v", err)
	}
	if err := reg.ValidateSource(config.Source{Name: "", Backend: "test"}); err == nil ||
		!strings.Contains(err.Error(), "name") {
		t.Fatalf("empty name error = %v", err)
	}
	err := reg.ValidateSource(config.Source{Name: "source", Backend: "test", Options: map[string]any{"token": ""}})
	if err == nil || !strings.Contains(err.Error(), `source "source": token required`) {
		t.Fatalf("plugin validation error = %v", err)
	}
}
