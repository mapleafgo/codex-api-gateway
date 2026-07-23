// Package metrics 在内存中聚合网关请求指标，供管理页展示。
//
// 设计要点（与 API 性能隔离）：
//   - API 请求路径只调用 Record，用 select+default 非阻塞投递到带缓冲 channel，
//     channel 满时直接丢弃事件，绝不阻塞或拖慢 API。
//   - 聚合（总量、按 供应商×模型 维度、环形历史）由独立 goroutine 消费 channel 完成。
//   - 管理端异常（HTTP handler panic）被 admin 包 recover 中间件隔离，不传播到 API。
package metrics

import (
	"sync"
	"sync/atomic"
	"time"
)

// EventKind 标识事件归属：客户端请求（KindClient）或单次上游尝试（KindUpstream）。
// 观测台按上游调用粒度展示，所以每次 trySource 都上报一条 KindUpstream 事件；
// 客户端请求只上报一条 KindClient 汇总（成功时记最终命中的源）。
type EventKind int

const (
	// KindClient 客户端请求汇总事件：一次 /v1/responses 调用一条。
	KindClient EventKind = iota
	// KindUpstream 单次上游尝试事件：每次 trySource 一条。
	KindUpstream
)

// RequestEvent 描述一次请求的观测数据（客户端请求或单次上游尝试）。
type RequestEvent struct {
	Kind      EventKind
	StartedAt time.Time
	Duration  time.Duration
	// TTFB 是上游首字节耗时：从开始尝试到收到第一个 SSE 事件。
	// 未收到首字节（建连失败/首字节超时）时为 0。
	// KindClient 通常为 0（客户端视角的首字节由对应 upstream 事件表达）。
	TTFB       time.Duration
	SourceName string
	Model      string
	// ResolvedModel 是该请求实际发送给上游的模型标识。
	// 当请求 Model 是别名时，ResolvedModel 记录映射后的真实模型；
	// 二者相同时表示未发生映射。
	ResolvedModel string
	Status        string
	Code          int // 语义化响应码：completed/incomplete=200，failed=500
	InputTokens   int
	OutputTokens  int
	CacheRead     int
	CacheCreate   int
	// Error 记录失败原因（Status=failed 时非空）。观测台每次调用独立记录，
	// 不依赖 lastSource/lastErr，所以错误必须随事件一起投递。
	Error string
	// Attempt 表示该次上游尝试在客户端请求内的序号（从 1 开始）。
	// 仅 KindUpstream 有意义；KindClient 为 0。
	Attempt int
	// BackendType 实际尝试/命中源的后端类型：a | c | r。
	BackendType string
}

// Snapshot 是管理端读取的聚合快照。
type Snapshot struct {
	TotalRequests    int64   `json:"total_requests"`
	TotalInput       int64   `json:"total_input"`
	TotalOutput      int64   `json:"total_output"`
	TotalCacheCreate int64   `json:"total_cache_create"`
	TotalCacheRead   int64   `json:"total_cache_read"`
	CacheHitRate     float64 `json:"cache_hit_rate"`

	ByGroup []GroupStat     `json:"by_group"`
	Recent  []RequestRecord `json:"recent"`
}

// GroupStat 是 供应商×模型 维度的聚合。
type GroupStat struct {
	Source          string `json:"source"`
	Model           string `json:"model"`
	Requests        int64  `json:"requests"`
	Completed       int64  `json:"completed"`
	Failed          int64  `json:"failed"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CacheRead       int64  `json:"cache_read"`
	CacheCreate     int64  `json:"cache_create"`
	TotalDurationMs int64  `json:"total_duration_ms"`
	// TotalTTFBMs 仅累加 TTFB>0 的样本，配合 TTFBSamples 算平均首字节。
	TotalTTFBMs int64 `json:"total_ttfb_ms"`
	// TTFBSamples 有首字节的上游尝试次数（用于平均，避免 0 值拉低均值）。
	TTFBSamples int64 `json:"ttfb_samples"`
}

// RequestRecord 是单条历史记录。
type RequestRecord struct {
	Time          string `json:"time"`
	TimeUnix      int64  `json:"time_unix"`
	Kind          string `json:"kind"`    // "client" | "upstream"
	Attempt       int    `json:"attempt"` // upstream 的尝试序号；client 为 0
	Model         string `json:"model"`
	ResolvedModel string `json:"resolved_model"`
	Source        string `json:"source"`
	Code          int    `json:"code"`
	InputTokens   int    `json:"input_tokens"`
	OutputTokens  int    `json:"output_tokens"`
	CacheRead     int    `json:"cache_read"`
	CacheCreate   int    `json:"cache_create"`
	DurationMs    int64  `json:"duration_ms"`
	// TTFBMs 首字节耗时毫秒；0 表示无首字节或未测量。
	TTFBMs      int64  `json:"ttfb_ms"`
	Status      string `json:"status"`
	Error       string `json:"error,omitempty"`
	BackendType string `json:"backend_type,omitempty"`
}

// HistorySize 是环形历史缓冲容量。
const HistorySize = 1000

const eventBufSize = 2048

// Collector 收集与聚合请求指标。零值不可用，须 New 构造。
type Collector struct {
	events chan RequestEvent
	mu     sync.RWMutex

	total struct {
		requests    int64
		input       int64
		output      int64
		cacheRead   int64
		cacheCreate int64
	}
	groups map[groupKey]*groupAgg

	history [HistorySize]RequestRecord
	histIdx int
	histLen int

	closed atomic.Bool
}

type groupKey struct {
	source string
	model  string
}

type groupAgg struct {
	requests    int64
	completed   int64
	failed      int64
	input       int64
	output      int64
	cacheRead   int64
	cacheCreate int64
	totalDurMs  int64
	totalTTFBMs int64
	ttfbSamples int64
}

// New 构造 Collector 并启动消费 goroutine。
func New() *Collector {
	c := &Collector{
		events: make(chan RequestEvent, eventBufSize),
		groups: map[groupKey]*groupAgg{},
	}
	go c.run()
	return c
}

// Record 非阻塞投递一个请求事件。channel 满或已 Stop 时静默丢弃。
func (c *Collector) Record(ev RequestEvent) {
	if c.closed.Load() {
		return
	}
	select {
	case c.events <- ev:
	default:
	}
}

// Stop 关闭投递入口并等待 consumer 退出。
func (c *Collector) Stop() {
	if !c.closed.CompareAndSwap(false, true) {
		return
	}
	close(c.events)
	for ev := range c.events {
		c.apply(ev) // drain 期间继续消费，避免丢数据
	}
}

func (c *Collector) run() {
	defer func() { _ = recover() }()
	for ev := range c.events {
		c.apply(ev)
	}
}

func (c *Collector) apply(ev RequestEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 总量与 by_group 聚合只统计上游尝试：一次客户端请求可能触发多次上游
	// 调用（主备切换、重试），按上游粒度统计才能反映真实流量与失败率。
	if ev.Kind == KindUpstream {
		c.total.requests++
		c.total.input += int64(ev.InputTokens)
		c.total.output += int64(ev.OutputTokens)
		c.total.cacheRead += int64(ev.CacheRead)
		c.total.cacheCreate += int64(ev.CacheCreate)

		// 聚合按"实际发送给上游的真实模型"分组，这样同一真实模型的多条别名
		// 会归并到同一行，供应商列表展示的也是真实模型而非别名。
		resolved := ev.ResolvedModel
		if resolved == "" {
			resolved = ev.Model
		}
		key := groupKey{source: ev.SourceName, model: resolved}
		g := c.groups[key]
		if g == nil {
			g = &groupAgg{}
			c.groups[key] = g
		}
		g.requests++
		switch ev.Status {
		case "completed":
			g.completed++
		case "failed":
			g.failed++
		}
		g.input += int64(ev.InputTokens)
		g.output += int64(ev.OutputTokens)
		g.cacheRead += int64(ev.CacheRead)
		g.cacheCreate += int64(ev.CacheCreate)
		g.totalDurMs += ev.Duration.Milliseconds()
		if ms := ev.TTFB.Milliseconds(); ms > 0 {
			g.totalTTFBMs += ms
			g.ttfbSamples++
		}
	}

	kindStr := "client"
	if ev.Kind == KindUpstream {
		kindStr = "upstream"
	}
	bt := ev.BackendType
	if bt == "" {
		bt = "a" // 历史兼容：缺省视为 Anthropic
	}
	rec := RequestRecord{
		Time:          ev.StartedAt.UTC().Format(time.RFC3339),
		TimeUnix:      ev.StartedAt.Unix(),
		Kind:          kindStr,
		Attempt:       ev.Attempt,
		Model:         ev.Model,
		ResolvedModel: ev.ResolvedModel,
		Source:        ev.SourceName,
		Code:          ev.Code,
		InputTokens:   ev.InputTokens,
		OutputTokens:  ev.OutputTokens,
		CacheRead:     ev.CacheRead,
		CacheCreate:   ev.CacheCreate,
		DurationMs:    ev.Duration.Milliseconds(),
		TTFBMs:        ev.TTFB.Milliseconds(),
		Status:        ev.Status,
		Error:         ev.Error,
		BackendType:   bt,
	}
	c.history[c.histIdx] = rec
	c.histIdx = (c.histIdx + 1) % HistorySize
	if c.histLen < HistorySize {
		c.histLen++
	}
}

// Snapshot 返回当前聚合快照。
func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()

	s := Snapshot{
		TotalRequests:    c.total.requests,
		TotalInput:       c.total.input,
		TotalOutput:      c.total.output,
		TotalCacheCreate: c.total.cacheCreate,
		TotalCacheRead:   c.total.cacheRead,
	}
	// 缓存命中率：所有输入 token 中从缓存读取的比例。
	// 旧公式 cacheRead/(cacheRead+cacheCreate) 在很多 Anthropic 兼容后端
	// （它们只填 cache_read、从不填 cache_creation）上恒为 100%，无参考价值。
	// 改为 token 维度：cacheRead / (inputTokens + cacheRead + cacheCreate)，
	// 含义是"输入侧有多少比例命中了 prompt 缓存"。
	denom := s.TotalInput + s.TotalCacheRead + s.TotalCacheCreate
	if denom > 0 {
		s.CacheHitRate = float64(s.TotalCacheRead) / float64(denom)
	}

	keys := make([]groupKey, 0, len(c.groups))
	for k := range c.groups {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			ki, kj := keys[i], keys[j]
			if ki.source > kj.source || (ki.source == kj.source && ki.model > kj.model) {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	s.ByGroup = make([]GroupStat, 0, len(keys))
	for _, k := range keys {
		g := c.groups[k]
		s.ByGroup = append(s.ByGroup, GroupStat{
			Source: k.source, Model: k.model,
			Requests: g.requests, Completed: g.completed, Failed: g.failed,
			InputTokens: g.input, OutputTokens: g.output,
			CacheRead: g.cacheRead, CacheCreate: g.cacheCreate,
			TotalDurationMs: g.totalDurMs,
			TotalTTFBMs:     g.totalTTFBMs,
			TTFBSamples:     g.ttfbSamples,
		})
	}

	n := c.histLen
	s.Recent = make([]RequestRecord, n)
	if n < HistorySize {
		for i := 0; i < n; i++ {
			s.Recent[n-1-i] = c.history[i]
		}
	} else {
		for i := 0; i < HistorySize; i++ {
			s.Recent[i] = c.history[(c.histIdx+HistorySize-1-i)%HistorySize]
		}
	}
	return s
}
