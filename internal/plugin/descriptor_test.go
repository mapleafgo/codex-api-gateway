package plugin

import (
	"net/http"
	"strings"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
)

func validDescriptor() Descriptor {
	return Descriptor{
		ID:           "test",
		Title:        "Test Source",
		Summary:      "test source",
		Streaming:    StreamingConverted,
		Capabilities: []Capability{CapabilityAnthropicMessages},
		Schema: []Field{
			{Name: "cache_enabled", Label: "Cache", Type: FieldTypeBoolean, Target: FieldTargetOption},
			{Name: "token", Label: "Token", Type: FieldTypeText, Required: true, Sensitive: true, Target: FieldTargetOption},
		},
	}
}

func TestValidateDescriptor(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Descriptor)
		wantErr string
	}{
		{name: "valid", mutate: func(*Descriptor) {}},
		{name: "empty id", mutate: func(d *Descriptor) { d.ID = "" }, wantErr: "id"},
		{name: "duplicate capability", mutate: func(d *Descriptor) {
			d.Capabilities = append(d.Capabilities, CapabilityAnthropicMessages)
		}, wantErr: "duplicate capability"},
		{name: "unknown streaming", mutate: func(d *Descriptor) { d.Streaming = "maybe" }, wantErr: "streaming"},
		{name: "unknown field type", mutate: func(d *Descriptor) { d.Schema[0].Type = "blob" }, wantErr: "field cache_enabled"},
		{name: "unknown target", mutate: func(d *Descriptor) { d.Schema[0].Target = "" }, wantErr: "target"},
		{name: "sensitive outside options", mutate: func(d *Descriptor) { d.Schema[1].Target = FieldTargetBaseURL }, wantErr: "sensitive"},
		{name: "duplicate action", mutate: func(d *Descriptor) {
			d.Actions = []Action{
				{ID: "auth", Kind: ActionKindDeviceCodeStatus, Routes: []ActionRoute{{ID: "start", Method: http.MethodPost, Path: "/start"}}},
				{ID: "auth", Kind: ActionKindDeviceCodeStatus, Routes: []ActionRoute{{ID: "status", Method: http.MethodGet, Path: "/status"}}},
			}
		}, wantErr: "duplicate action"},
		{name: "action without routes", mutate: func(d *Descriptor) {
			d.Actions = []Action{{ID: "auth", Kind: ActionKindDeviceCodeStatus}}
		}, wantErr: "route"},
		{name: "invalid action method", mutate: func(d *Descriptor) {
			d.Actions = []Action{{ID: "auth", Kind: ActionKindDeviceCodeStatus, Routes: []ActionRoute{{Method: "PATCH", Path: "/start"}}}}
		}, wantErr: "method"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := validDescriptor()
			tt.mutate(&d)
			err := d.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %v, want contains %q", err, tt.wantErr)
			}
		})
	}
}

func TestValidateSchemaForeignOptions(t *testing.T) {
	d := validDescriptor()
	if err := d.Validate(); err != nil {
		t.Fatalf("descriptor invalid: %v", err)
	}
	src := config.Source{Name: "source", Backend: string(d.ID), Options: map[string]any{
		"token":         "secret",
		"cache_enabled": true,
		"github_token":  "legacy",
	}}
	err := d.validateOptions(src)
	if err == nil || !strings.Contains(err.Error(), `options.github_token`) {
		t.Fatalf("validateOptions() = %v, want schema-foreign github_token", err)
	}
}
