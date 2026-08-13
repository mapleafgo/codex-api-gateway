package toolcatalog

import "testing"

// TestResolveIdentityFromFlatFallback 覆盖上游丢弃 namespace 前缀时的回退匹配：
// 声明里只有 collaboration__spawn_agent，上游返回 spawn_agent，应按 name 回退还原
// namespace=collaboration + name=spawn_agent。
func TestResolveIdentityFromFlatFallback(t *testing.T) {
	declared := map[string]Identity{
		"collaboration__spawn_agent":   {OpenAIType: "function", Namespace: "collaboration", Name: "spawn_agent"},
		"collaboration__send_message":  {OpenAIType: "function", Namespace: "collaboration", Name: "send_message"},
		"collaboration__followup_task": {OpenAIType: "function", Namespace: "collaboration", Name: "followup_task"},
	}

	tests := []struct {
		flat   string
		wantNS string
		wantN  string
		wantOK bool
	}{
		{flat: "collaboration__spawn_agent", wantNS: "collaboration", wantN: "spawn_agent", wantOK: true},
		{flat: "spawn_agent", wantNS: "collaboration", wantN: "spawn_agent", wantOK: true},
		{flat: "send_message", wantNS: "collaboration", wantN: "send_message", wantOK: true},
		{flat: "followup_task", wantNS: "collaboration", wantN: "followup_task", wantOK: true},
		// 精确命中普通无 namespace 工具
		{flat: "get_weather", wantOK: false},
	}

	for _, tc := range tests {
		id, ok := ResolveIdentityFromFlat(declared, tc.flat)
		if ok != tc.wantOK {
			t.Errorf("flat=%q ok=%v want=%v", tc.flat, ok, tc.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if id.Namespace != tc.wantNS || id.Name != tc.wantN {
			t.Errorf("flat=%q got %s/%s want %s/%s", tc.flat, id.Namespace, id.Name, tc.wantNS, tc.wantN)
		}
	}
}

// TestResolveIdentityFromFlatAmbiguous 声明里同名存在于多个 namespace 时不回退
// （name 匹配非唯一，避免猜错）。
func TestResolveIdentityFromFlatAmbiguous(t *testing.T) {
	declared := map[string]Identity{
		"a__run": {Namespace: "a", Name: "run"},
		"b__run": {Namespace: "b", Name: "run"},
	}
	if id, ok := ResolveIdentityFromFlat(declared, "run"); ok {
		t.Fatalf("同名多个 namespace 不应回退，got %s/%s", id.Namespace, id.Name)
	}
}
