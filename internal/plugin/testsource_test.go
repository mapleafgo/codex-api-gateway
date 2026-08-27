package plugin

import (
	"context"
	"fmt"
	"testing"

	"github.com/mapleafgo/codex-api-gateway/internal/config"
	"github.com/mapleafgo/codex-api-gateway/internal/model"
)

// testSource 是一个自包含的测试源插件：仅实现 SourcePlugin 契约与最小
// Responses 透传后端，用来证明"新增源只写自身包 + 注册即可接入"。
// 它只存在于测试包内，不触碰任何共享核心源码。
type testSource struct{}

func (testSource) Descriptor() Descriptor {
	return Descriptor{
		ID:           "test-source",
		Title:        "Test Source",
		Summary:      "US4 扩展性验证用的自包含源插件",
		Capabilities: []Capability{CapabilityResponsesPassthrough},
		Streaming:    StreamingPassthrough,
		Schema: []Field{
			{
				Name: "token", Label: "Token", Type: FieldTypePassword,
				Required: true, Sensitive: true, Target: FieldTargetOption,
			},
		},
	}
}

func (testSource) ValidateSource(src config.Source) error {
	token, _ := src.Options["token"].(string)
	if token == "" {
		return fmt.Errorf("test-source: missing required option token")
	}
	return nil
}

func (testSource) Backend() Backend { return testSourceBackend{} }

// testSourceBackend 透传最小 Responses SSE 事件序列，证明后端可经统一契约分发。
type testSourceBackend struct{}

func (testSourceBackend) Execute(
	_ context.Context,
	_ []byte,
	src config.Source,
	_ *config.Config,
	onEvent func(model.SSEEvent) error,
	onUpstream func(UpstreamEvent),
	_ int,
) error {
	if onEvent != nil {
		events := []model.SSEEvent{
			{Type: "response.created", Data: []byte(`{"type":"response.created","response":{"id":"resp-test","status":"in_progress"}}`)},
			{Type: "response.output_text.delta", Data: []byte(`{"type":"response.output_text.delta","item_id":"it_1","delta":"hi"}`)},
			{Type: "response.completed", Data: []byte(`{"type":"response.completed","response":{"id":"resp-test","status":"completed"}}`)},
		}
		for _, ev := range events {
			if err := onEvent(ev); err != nil {
				return err
			}
		}
	}
	if onUpstream != nil {
		onUpstream(UpstreamEvent{SourceName: src.Name, Status: "completed", Backend: "test-source"})
	}
	return nil
}

var _ SourcePlugin = testSource{}

// TestTestSource_ExtendsWithOnlyItsOwnPackage 验证 FR-014：一个全新的源插件
// 只需要实现 SourcePlugin 契约并传入 Registry，共享核心文件零改动。
func TestTestSource_RegistersAndExposesMetadata(t *testing.T) {
	reg, err := New(testSource{})
	if err != nil {
		t.Fatalf("register test-source: %v", err)
	}
	p, ok := reg.Get("test-source")
	if !ok {
		t.Fatal("test-source 未注册")
	}
	if got := p.Descriptor().Title; got != "Test Source" {
		t.Fatalf("title = %q", got)
	}
	descs := reg.Descriptors()
	if len(descs) != 1 || descs[0].ID != "test-source" {
		t.Fatalf("descriptors = %+v", descs)
	}
}

// TestTestSource_ValidateAndDispatch 验证已注册源可配置、可分发、未知选项与
// 缺必填项被拒；未注册源配置被拒。
func TestTestSource_ValidateAndDispatch(t *testing.T) {
	reg, err := New(testSource{})
	if err != nil {
		t.Fatalf("register test-source: %v", err)
	}

	valid := config.Source{Name: "ts", Backend: "test-source", Options: map[string]any{"token": "t1"}}
	if err := reg.ValidateSource(valid); err != nil {
		t.Fatalf("valid source rejected: %v", err)
	}
	missing := config.Source{Name: "ts", Backend: "test-source"}
	if err := reg.ValidateSource(missing); err == nil {
		t.Fatal("missing required option should be rejected")
	}
	foreign := config.Source{Name: "ts", Backend: "test-source", Options: map[string]any{"token": "t1", "unknown": 1}}
	if err := reg.ValidateSource(foreign); err == nil {
		t.Fatal("schema-foreign option should be rejected")
	}
	unregistered := config.Source{Name: "ts", Backend: "not-registered"}
	if err := reg.ValidateSource(unregistered); err == nil {
		t.Fatal("unregistered backend should be rejected")
	}

	var got []model.SSEEvent
	p, _ := reg.Get("test-source")
	err = p.Backend().Execute(context.Background(), []byte(`{"model":"m"}`), valid, nil,
		func(ev model.SSEEvent) error { got = append(got, ev); return nil },
		func(ev UpstreamEvent) {
			if ev.Backend != "test-source" {
				t.Fatalf("upstream backend = %q, want test-source", ev.Backend)
			}
		},
		1)
	if err != nil {
		t.Fatalf("execute test-source: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("events = %d, want 3", len(got))
	}
}
