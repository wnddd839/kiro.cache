package server

import (
	"slices"
	"sync"
	"time"

	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/turn"
)

// 隐藏 token 漂移监控。
//
// Kiro 把自己的 system 与对话模板放进上下文（约 4052 token）。本地用这个常数从上游上下文百分比里扣掉它，
// 再校准下游 token。Kiro 一改 prompt 这个数就变，校准会整体偏移。
//
// 反推：小请求（本地 prompt+输出 ≤ hiddenProbeMax）时本地分词误差只有几个 token，
// 上游上下文 − 本地内容 ≈ 隐藏 token。按模型取最近 hiddenWindow 个反推值的中位数，
// 偏离常数超过 hiddenTolerance 就告警。
const (
	hiddenProbeMax  = 400
	hiddenWindow    = 15
	hiddenMinSample = 5
	hiddenTolerance = 100
)

// hiddenStat 是一个模型的反推结果，给管理台。
type hiddenStat struct {
	Model    string    `json:"model"`
	Expected int       `json:"expected"`
	Median   int       `json:"median"`
	Samples  int       `json:"samples"`
	Drift    int       `json:"drift"` // median - expected
	Alert    bool      `json:"alert"`
	At       time.Time `json:"at"`
}

type hiddenWatch struct {
	mu      sync.Mutex
	samples map[string][]int
	at      map[string]time.Time
	alerted map[string]bool
}

func newHiddenWatch() *hiddenWatch {
	return &hiddenWatch{samples: map[string][]int{}, at: map[string]time.Time{}, alerted: map[string]bool{}}
}

// observe 记一个反推值；返回该模型当前状态，以及是否刚进入 / 离开告警。
func (h *hiddenWatch) observe(model string, v int, now time.Time) (hiddenStat, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := append(h.samples[model], v)
	if len(s) > hiddenWindow {
		s = s[len(s)-hiddenWindow:]
	}
	h.samples[model], h.at[model] = s, now
	st := h.statLocked(model)
	changed := st.Alert != h.alerted[model] && st.Samples >= hiddenMinSample
	h.alerted[model] = st.Alert
	return st, changed
}

func (h *hiddenWatch) statLocked(model string) hiddenStat {
	s := slices.Sorted(slices.Values(h.samples[model]))
	st := hiddenStat{Model: model, Expected: kiro.HiddenTokens(model), Samples: len(s), At: h.at[model]}
	if len(s) == 0 {
		return st
	}
	st.Median = s[len(s)/2]
	st.Drift = st.Median - st.Expected
	st.Alert = len(s) >= hiddenMinSample && (st.Drift > hiddenTolerance || st.Drift < -hiddenTolerance)
	return st
}

func (h *hiddenWatch) stats() []hiddenStat {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]hiddenStat, 0, len(h.samples))
	for m := range h.samples {
		out = append(out, h.statLocked(m))
	}
	slices.SortFunc(out, func(a, b hiddenStat) int {
		if a.Model < b.Model {
			return -1
		}
		if a.Model > b.Model {
			return 1
		}
		return 0
	})
	return out
}

// watchHidden 在上游报了上下文占用的小请求上反推隐藏 token。u 是本地计量（校准前）。
func (s *Server) watchHidden(model string, u turn.Usage, k kiro.Usage) {
	local := u.PromptTokens() + u.Output
	if k.Reported || k.Input <= 0 || local <= 0 || local > hiddenProbeMax {
		return
	}
	st, changed := s.hidden.observe(model, k.Input-local, time.Now())
	if !changed {
		return
	}
	if st.Alert {
		s.log.Warn("Kiro hidden context drifted: token calibration is off; update kiro.HiddenTokens",
			"model", model, "expected", st.Expected, "median", st.Median, "drift", st.Drift, "samples", st.Samples)
	} else {
		s.log.Info("Kiro hidden context back within tolerance", "model", model, "median", st.Median)
	}
}

// reportedSampleEvery 是上游报了 tokenUsage 时，对比上游值与本地估算的采样间隔（每 N 次记一条）。
const reportedSampleEvery = 20

// noteReported 在上游报了 tokenUsage 时：多条先告警并把每条原值写调试日志；
// 按采样间隔记一条上游与本地估算的对比。上游目前不报，这是为上游开始报时准备的。
func (s *Server) noteReported(c *call, account string, local turn.Usage, k kiro.Usage) {
	if k.UsageEvents == 0 {
		return
	}
	s.reportsSeen.Store(true)
	if k.UsageEvents > 1 {
		s.log.Warn("upstream sent several tokenUsage in one reply; using the last one (not summed)",
			"model", c.model, "account", account, "events", k.UsageEvents, "mode", s.cfg.ReportedUsageMode())
		if k.UsageRaw != nil {
			for i, raw := range *k.UsageRaw {
				s.log.Debug("tokenUsage", "model", c.model, "index", i, "raw", raw)
			}
		}
	}
	if s.reportedSeen.Add(1)%reportedSampleEvery != 1 {
		return
	}
	s.log.Info("tokenUsage sample: upstream vs local", "model", c.model, "mode", s.cfg.ReportedUsageMode(),
		"up_input", k.Input, "up_cache_read", k.CacheRead, "up_cache_write", k.CacheWrite, "up_output", k.Output,
		"local_input", local.Input, "local_cache_read", local.CacheRead, "local_cache_write", local.CacheWrite,
		"hidden", kiro.HiddenTokens(c.model), "context", k.Input+k.CacheRead+k.CacheWrite)
}
