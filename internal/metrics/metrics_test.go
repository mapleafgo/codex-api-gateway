package metrics

import (
	"testing"
	"time"
)

func TestCollectorAggregates(t *testing.T) {
	c := New()
	defer c.Stop()

	c.Record(RequestEvent{
		Kind:      KindUpstream,
		StartedAt: time.Now(), Duration: 100 * time.Millisecond,
		SourceName: "zhipu", Model: "glm-latest", Status: "completed",
		InputTokens: 100, OutputTokens: 50, CacheRead: 80, CacheCreate: 20,
	})
	c.Record(RequestEvent{
		Kind:      KindUpstream,
		StartedAt: time.Now(), Duration: 200 * time.Millisecond,
		SourceName: "zhipu", Model: "glm-latest", Status: "failed",
		InputTokens: 200, OutputTokens: 0, CacheRead: 0, CacheCreate: 200,
	})
	c.Record(RequestEvent{
		Kind:      KindUpstream,
		StartedAt: time.Now(), Duration: 300 * time.Millisecond,
		SourceName: "volces", Model: "glm-latest", Status: "completed",
		InputTokens: 300, OutputTokens: 30, CacheRead: 300, CacheCreate: 0,
	})

	// 等待 consumer 处理完成
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s := c.Snapshot()
		if s.TotalRequests == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	s := c.Snapshot()
	if s.TotalRequests != 3 {
		t.Fatalf("TotalRequests = %d, want 3", s.TotalRequests)
	}
	if s.TotalInput != 600 {
		t.Errorf("TotalInput = %d, want 600", s.TotalInput)
	}
	if s.TotalCacheRead != 380 {
		t.Errorf("TotalCacheRead = %d, want 380", s.TotalCacheRead)
	}
	if s.TotalCacheCreate != 220 {
		t.Errorf("TotalCacheCreate = %d, want 220", s.TotalCacheCreate)
	}
	// 命中率（token 维度）= cacheRead / (input + cacheRead + cacheCreate)
	// = 380 / (600 + 380 + 220)
	want := 380.0 / 1200.0
	if absFloat(s.CacheHitRate-want) > 1e-9 {
		t.Errorf("CacheHitRate = %v, want %v", s.CacheHitRate, want)
	}

	// by_group：zhipu/glm-latest 与 volces/glm-latest
	if len(s.ByGroup) != 2 {
		t.Fatalf("ByGroup len = %d, want 2", len(s.ByGroup))
	}
	// volces 在 zhipu 之前（字母序）
	if s.ByGroup[0].Source != "volces" {
		t.Errorf("ByGroup[0].Source = %s, want volces", s.ByGroup[0].Source)
	}
	// zhipu 聚合：2 requests, 1 completed, 1 failed
	zhipu := s.ByGroup[1]
	if zhipu.Requests != 2 || zhipu.Completed != 1 || zhipu.Failed != 1 {
		t.Errorf("zhipu = %+v", zhipu)
	}
	if zhipu.TotalDurationMs != 300 { // 100 + 200
		t.Errorf("zhipu.TotalDurationMs = %d, want 300", zhipu.TotalDurationMs)
	}

	// 历史：3 条，最新在前
	if len(s.Recent) != 3 {
		t.Fatalf("Recent len = %d, want 3", len(s.Recent))
	}
}

func TestCollectorRingBuffer(t *testing.T) {
	c := New()
	defer c.Stop()

	// 投递 HistorySize + 500 条，验证只保留 HistorySize
	total := HistorySize + 500
	for i := 0; i < total; i++ {
		c.Record(RequestEvent{Kind: KindUpstream,
			StartedAt:  time.Unix(int64(i), 0),
			Duration:   time.Millisecond,
			SourceName: "s", Model: "m", Status: "completed",
		})
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Recent) == HistorySize {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	s := c.Snapshot()
	if len(s.Recent) != HistorySize {
		t.Fatalf("Recent len = %d, want %d", len(s.Recent), HistorySize)
	}
	// 最新（时间最大）应在最前
	if s.Recent[0].TimeUnix != int64(total-1) {
		t.Errorf("Recent[0].TimeUnix = %d, want %d", s.Recent[0].TimeUnix, total-1)
	}
	// 最旧应为 total - HistorySize
	if s.Recent[len(s.Recent)-1].TimeUnix != int64(total-HistorySize) {
		t.Errorf("Recent[-1].TimeUnix = %d, want %d",
			s.Recent[len(s.Recent)-1].TimeUnix, total-HistorySize)
	}
}

func TestRecordNonBlockingWhenStopped(t *testing.T) {
	c := New()
	c.Stop()
	// Stop 后 Record 不应 panic / 不应阻塞
	c.Record(RequestEvent{SourceName: "x"})
}

func TestRecordNonBlockingWhenFull(t *testing.T) {
	c := New()
	defer c.Stop()
	// 不消费（先暂停 consumer 不可行，这里直接灌满 + 大量）
	// 灌满 channel 后再 Record 应立即返回，验证不阻塞即可
	for i := 0; i < eventBufSize+50; i++ {
		c.Record(RequestEvent{SourceName: "x"})
	}
}

func absFloat(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestCollectorGroupsByResolvedModel 验证：当请求携带别名 Model 但提供了
// ResolvedModel（真实模型）时，by_group 应按真实模型分组。
func TestCollectorGroupsByResolvedModel(t *testing.T) {
	c := New()
	defer c.Stop()

	// 同一供应商下两条请求，别名不同但映射到同一真实模型
	c.Record(RequestEvent{
		Kind:      KindUpstream,
		StartedAt: time.Now(), Duration: 100 * time.Millisecond,
		SourceName: "zhipu", Model: "alias-a", ResolvedModel: "glm-4.5",
		Status: "completed", InputTokens: 10,
	})
	c.Record(RequestEvent{
		Kind:      KindUpstream,
		StartedAt: time.Now(), Duration: 100 * time.Millisecond,
		SourceName: "zhipu", Model: "alias-b", ResolvedModel: "glm-4.5",
		Status: "completed", InputTokens: 20,
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s := c.Snapshot()
		if s.TotalRequests == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	s := c.Snapshot()
	if len(s.ByGroup) != 1 {
		t.Fatalf("ByGroup len = %d, want 1 (grouped by resolved model)", len(s.ByGroup))
	}
	g := s.ByGroup[0]
	if g.Model != "glm-4.5" {
		t.Errorf("ByGroup[0].Model = %s, want glm-4.5", g.Model)
	}
	if g.Requests != 2 {
		t.Errorf("ByGroup[0].Requests = %d, want 2", g.Requests)
	}

	// 历史：保留原始 alias，resolved_model 记录真实模型
	if len(s.Recent) != 2 {
		t.Fatalf("Recent len = %d, want 2", len(s.Recent))
	}
	for _, r := range s.Recent {
		if r.ResolvedModel != "glm-4.5" {
			t.Errorf("Recent.ResolvedModel = %s, want glm-4.5", r.ResolvedModel)
		}
	}
}

// TestCollectorTTFB 验证首字节耗时进入历史记录与 by_group 聚合；
// 无首字节的失败样本不计入 ttfb_samples，避免把 0 拉低平均值。
func TestCollectorTTFB(t *testing.T) {
	c := New()
	defer c.Stop()

	c.Record(RequestEvent{
		Kind: KindUpstream, StartedAt: time.Now(),
		Duration: 500 * time.Millisecond, TTFB: 120 * time.Millisecond,
		SourceName: "zhipu", Model: "glm", Status: "completed",
	})
	c.Record(RequestEvent{
		Kind: KindUpstream, StartedAt: time.Now(),
		Duration: 800 * time.Millisecond, TTFB: 180 * time.Millisecond,
		SourceName: "zhipu", Model: "glm", Status: "completed",
	})
	// 建连失败：无首字节，TTFB 为 0
	c.Record(RequestEvent{
		Kind: KindUpstream, StartedAt: time.Now(),
		Duration: 50 * time.Millisecond, TTFB: 0,
		SourceName: "zhipu", Model: "glm", Status: "failed",
	})
	// 客户端汇总不参与 by_group，但历史应保留其 TTFB（通常为 0）
	c.Record(RequestEvent{
		Kind: KindClient, StartedAt: time.Now(),
		Duration: 900 * time.Millisecond, TTFB: 0,
		SourceName: "zhipu", Model: "glm", Status: "completed",
	})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if len(c.Snapshot().Recent) == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	s := c.Snapshot()
	if len(s.ByGroup) != 1 {
		t.Fatalf("ByGroup len = %d, want 1", len(s.ByGroup))
	}
	g := s.ByGroup[0]
	if g.TotalTTFBMs != 300 { // 120 + 180
		t.Errorf("TotalTTFBMs = %d, want 300", g.TotalTTFBMs)
	}
	if g.TTFBSamples != 2 {
		t.Errorf("TTFBSamples = %d, want 2", g.TTFBSamples)
	}
	if g.TotalDurationMs != 1350 { // 500+800+50
		t.Errorf("TotalDurationMs = %d, want 1350", g.TotalDurationMs)
	}

	// 历史最新在前：client、failed、completed180、completed120
	if s.Recent[0].TTFBMs != 0 || s.Recent[0].Kind != "client" {
		t.Errorf("Recent[0] = %+v, want client ttfb=0", s.Recent[0])
	}
	if s.Recent[1].TTFBMs != 0 || s.Recent[1].Status != "failed" {
		t.Errorf("Recent[1] = %+v, want failed ttfb=0", s.Recent[1])
	}
	if s.Recent[2].TTFBMs != 180 {
		t.Errorf("Recent[2].TTFBMs = %d, want 180", s.Recent[2].TTFBMs)
	}
	if s.Recent[3].TTFBMs != 120 {
		t.Errorf("Recent[3].TTFBMs = %d, want 120", s.Recent[3].TTFBMs)
	}
}
