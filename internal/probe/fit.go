package probe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strings"
	"time"
)

// Read 读 JSONL 结果。
func Read(r io.Reader) ([]Record, error) {
	var out []Record
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return out, err
		}
		out = append(out, rec)
	}
	return out, sc.Err()
}

// Report 是对探测结果的分析，对应方案第 5 节的决策。
type Report struct {
	Usage      UsageFinding  `json:"usage"`
	TTL        []TTLFinding  `json:"ttl,omitzero"`
	CachePoint []CPFinding   `json:"cache_point,omitzero"`
	Credits    *CreditFit    `json:"credits,omitzero"`
	Limits     []LimitsCheck `json:"limits,omitzero"`
}

// UsageFinding 是第 0 步：上游报不报 tokenUsage。
type UsageFinding struct {
	Calls          int    `json:"calls"`
	WithTokenUsage int    `json:"with_token_usage"`
	MaxEvents      int    `json:"max_events_per_call"`
	Cumulative     string `json:"cumulative"` // yes | no | unknown：多次事件时后一次是否 >= 前一次（累计值）
	HasNormalized  bool   `json:"has_normalized"`
	HasTotal       bool   `json:"has_total"`
}

// TTLFinding 是一个间隔的结果。Hit 判断：上游报了 cacheRead 就看它；没报就看 credits 是否明显低于写入。
type TTLFinding struct {
	Gap       string  `json:"gap"`
	SameConv  bool    `json:"same_conversation"`
	Write     float64 `json:"write_credits"`
	Read      float64 `json:"read_credits"`
	Ratio     float64 `json:"read_over_write"`
	CacheRead int     `json:"cache_read_tokens,omitzero"`
	Hit       string  `json:"hit"` // yes | no | unknown
	FirstMSW  int64   `json:"first_ms_write,omitzero"`
	FirstMSR  int64   `json:"first_ms_read,omitzero"`
}

// CPFinding 是一种 cachePoint 位置的结果。
type CPFinding struct {
	Points string  `json:"points"`
	Cold   float64 `json:"cold_credits"`
	Warm   float64 `json:"warm_credits"`
	Ratio  float64 `json:"warm_over_cold"`
	Error  string  `json:"error,omitzero"`
}

// CreditFit 是 credits 的线性拟合：credits ≈ Hidden×Context + Context×prefix + Output×out（冷）；
// 热请求的 Context 单价单独算，用来看缓存读有没有打折。单位：每百万 token 的 credits。
type CreditFit struct {
	ColdPerM     float64 `json:"cold_context_per_m"`
	WarmPerM     float64 `json:"warm_context_per_m"`
	OutputPerM   float64 `json:"output_per_m"`
	Fixed        float64 `json:"fixed_credits"` // 截距：隐藏上下文与每次固定开销
	WarmOverCold float64 `json:"warm_over_cold"`
	Samples      int     `json:"samples"`
	MaxRelErr    float64 `json:"max_rel_err"`
	// Suggest 是可以直接贴进配置 credit_rates 的系数（上下文按冷单价，偏保守）
	Suggest map[string]map[string]float64 `json:"suggest_credit_rates"`
}

// LimitsCheck 是第 4 步：一个实验前后的额度增量与 meteringEvent 之和。
type LimitsCheck struct {
	Experiment string  `json:"experiment"`
	Delta      float64 `json:"limits_delta"`
	Metered    float64 `json:"metered"`
	Diff       float64 `json:"diff"`
}

// Analyze 汇总探测结果。
func Analyze(recs []Record) Report {
	var rep Report
	usage(recs, &rep.Usage)
	rep.TTL = ttlFindings(recs)
	rep.CachePoint = cpFindings(recs)
	rep.Credits = fitCredits(recs)
	rep.Limits = limitsChecks(recs)
	return rep
}

func usage(recs []Record, u *UsageFinding) {
	cum := "unknown"
	for _, r := range recs {
		if r.Step == "limits-before" || r.Step == "limits-after" || r.Status != 200 {
			continue
		}
		u.Calls++
		if r.TokenUsages > 0 {
			u.WithTokenUsage++
		}
		u.MaxEvents = max(u.MaxEvents, r.TokenUsages)
		for _, raw := range r.AllUsage {
			var m map[string]float64
			_ = json.Unmarshal(raw, &m)
			u.HasNormalized = u.HasNormalized || m["normalizedTokenUsage"] > 0
			u.HasTotal = u.HasTotal || m["totalTokens"] > 0
		}
		if len(r.AllUsage) > 1 {
			grow := true
			var prev float64
			for _, raw := range r.AllUsage {
				var m map[string]float64
				_ = json.Unmarshal(raw, &m)
				v := m["outputTokens"] + m["uncachedInputTokens"] + m["cacheReadInputTokens"]
				if v < prev {
					grow = false
				}
				prev = v
			}
			if grow {
				cum = "yes"
			} else if cum == "unknown" {
				cum = "no"
			}
		}
	}
	u.Cumulative = cum
}

func cacheRead(r Record) int {
	var m map[string]float64
	if json.Unmarshal(r.TokenUsage, &m) != nil {
		return -1
	}
	if v, ok := m["cacheReadInputTokens"]; ok {
		return int(v)
	}
	return -1
}

// hitByCredits 在上游不报 cacheRead 时用 credits 判断命中：读的 credits 低于写的 70% 算命中。
// 门槛来自缓存读至少打九折、同样输出下读应明显更便宜；不确定时报 unknown。
func hitByCredits(write, read float64) string {
	if write <= 0 || read <= 0 {
		return "unknown"
	}
	switch r := read / write; {
	case r < 0.7:
		return "yes"
	case r > 0.9:
		return "no"
	}
	return "unknown"
}

func ttlFindings(recs []Record) []TTLFinding {
	type pair struct{ w, r *Record }
	by := map[string]*pair{}
	var order []string
	for i := range recs {
		r := &recs[i]
		if r.Experiment != "ttl" || r.Case == "" {
			continue
		}
		p := by[r.Case]
		if p == nil {
			p = &pair{}
			by[r.Case] = p
			order = append(order, r.Case)
		}
		switch r.Step {
		case "write":
			p.w = r
		case "read":
			p.r = r
		}
	}
	var out []TTLFinding
	for _, c := range order {
		p := by[c]
		if p.w == nil || p.r == nil {
			continue
		}
		f := TTLFinding{Gap: p.r.GapText, SameConv: p.r.SameConv, Write: p.w.Credits, Read: p.r.Credits,
			FirstMSW: p.w.FirstMS, FirstMSR: p.r.FirstMS}
		if p.w.Credits > 0 {
			f.Ratio = p.r.Credits / p.w.Credits
		}
		if cr := cacheRead(*p.r); cr >= 0 {
			f.CacheRead = cr
			f.Hit = map[bool]string{true: "yes", false: "no"}[cr > 0]
		} else {
			f.Hit = hitByCredits(p.w.Credits, p.r.Credits)
		}
		out = append(out, f)
	}
	slices.SortStableFunc(out, func(a, b TTLFinding) int {
		da, _ := time.ParseDuration(a.Gap)
		db, _ := time.ParseDuration(b.Gap)
		if da != db {
			return int(da - db)
		}
		if a.SameConv == b.SameConv {
			return 0
		}
		if a.SameConv {
			return -1
		}
		return 1
	})
	return out
}

func cpFindings(recs []Record) []CPFinding {
	by := map[string]*CPFinding{}
	var order []string
	for _, r := range recs {
		if r.Experiment != "cachepoint" || r.Case == "" {
			continue
		}
		f := by[r.Case]
		if f == nil {
			f = &CPFinding{Points: r.Case}
			by[r.Case] = f
			order = append(order, r.Case)
		}
		if r.Error != "" {
			f.Error = r.Error
		}
		switch r.Step {
		case "cold":
			f.Cold = r.Credits
		case "warm":
			f.Warm = r.Credits
		}
	}
	var out []CPFinding
	for _, c := range order {
		f := by[c]
		if f.Cold > 0 {
			f.Ratio = f.Warm / f.Cold
		}
		out = append(out, *f)
	}
	return out
}

// fitCredits 用 credits 实验的数据拟合。内容 token 用上游上下文（百分比×窗口，含隐藏部分）减输出字符/4；
// 没有百分比时退回目标前缀长度。变量：[1, 冷上下文, 热上下文, 输出]。
func fitCredits(recs []Record) *CreditFit {
	var X [][]float64
	var y []float64
	for _, r := range recs {
		if r.Experiment != "credits" || r.Status != 200 || r.Credits <= 0 {
			continue
		}
		out := float64(r.OutputChars) / 4
		ctx := float64(r.ContextTok) - out
		if r.ContextTok == 0 {
			ctx = float64(r.PrefixTokens)
		}
		cold, warm := ctx, 0.0
		if r.Step != "cold" {
			cold, warm = 0, ctx
		}
		X = append(X, []float64{1, cold, warm, out})
		y = append(y, r.Credits)
	}
	if len(X) < 5 {
		return nil
	}
	c, ok := leastSquares(X, y)
	if !ok {
		return nil
	}
	f := &CreditFit{Fixed: c[0], ColdPerM: c[1] * 1e6, WarmPerM: c[2] * 1e6, OutputPerM: c[3] * 1e6, Samples: len(X)}
	if c[1] != 0 {
		f.WarmOverCold = c[2] / c[1]
	}
	for i, x := range X {
		p := 0.0
		for j := range x {
			p += x[j] * c[j]
		}
		f.MaxRelErr = max(f.MaxRelErr, math.Abs(p-y[i])/y[i])
	}
	f.Suggest = map[string]map[string]float64{"claude": {"context": round3(f.ColdPerM), "output": round3(f.OutputPerM)}}
	return f
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// leastSquares 解正规方程（列主元高斯消元）。列全为 0 的变量系数为 0。
func leastSquares(X [][]float64, y []float64) ([]float64, bool) {
	n := len(X[0])
	A := make([][]float64, n)
	b := make([]float64, n)
	for i := range n {
		A[i] = make([]float64, n)
		for k := range X {
			b[i] += X[k][i] * y[k]
			for j := range n {
				A[i][j] += X[k][i] * X[k][j]
			}
		}
	}
	for i := range n {
		if A[i][i] == 0 {
			A[i][i], b[i] = 1, 0 // 这个变量没有数据
		}
	}
	for col := range n {
		piv := col
		for r := col + 1; r < n; r++ {
			if math.Abs(A[r][col]) > math.Abs(A[piv][col]) {
				piv = r
			}
		}
		if math.Abs(A[piv][col]) < 1e-18 {
			return nil, false
		}
		A[col], A[piv] = A[piv], A[col]
		b[col], b[piv] = b[piv], b[col]
		for r := range n {
			if r == col {
				continue
			}
			f := A[r][col] / A[col][col]
			for c := col; c < n; c++ {
				A[r][c] -= f * A[col][c]
			}
			b[r] -= f * b[col]
		}
	}
	out := make([]float64, n)
	for i := range n {
		out[i] = b[i] / A[i][i]
	}
	return out, true
}

func limitsChecks(recs []Record) []LimitsCheck {
	type acc struct {
		before, after float64
		seenB, seenA  bool
		metered       float64
	}
	by := map[string]*acc{}
	var order []string
	for _, r := range recs {
		a := by[r.Experiment]
		if a == nil {
			a = &acc{}
			by[r.Experiment] = a
			order = append(order, r.Experiment)
		}
		switch r.Step {
		case "limits-before":
			if r.Error == "" {
				a.before, a.seenB = r.LimitsUsed+r.LimitsOverage, true
			}
		case "limits-after":
			if r.Error == "" {
				a.after, a.seenA = r.LimitsUsed+r.LimitsOverage, true
			}
		default:
			a.metered += r.Credits
		}
	}
	var out []LimitsCheck
	for _, e := range order {
		a := by[e]
		if !a.seenA || !a.seenB {
			continue
		}
		d := a.after - a.before
		out = append(out, LimitsCheck{Experiment: e, Delta: d, Metered: a.metered, Diff: d - a.metered})
	}
	return out
}

// Text 把报告写成人读的摘要。
func (rep Report) Text(w io.Writer) {
	u := rep.Usage
	fmt.Fprintf(w, "[0] tokenUsage: %d/%d calls report it; max %d events/call; cumulative=%s; totalTokens=%v normalizedTokenUsage=%v\n",
		u.WithTokenUsage, u.Calls, u.MaxEvents, u.Cumulative, u.HasTotal, u.HasNormalized)
	if len(rep.TTL) > 0 {
		fmt.Fprintln(w, "[1] TTL (hit judged by cacheRead if reported, else credits read/write < 0.7):")
		for _, f := range rep.TTL {
			fmt.Fprintf(w, "    gap %-6s same_conv=%-5v write %.4f read %.4f ratio %.2f hit=%s\n", f.Gap, f.SameConv, f.Write, f.Read, f.Ratio, f.Hit)
		}
	}
	if len(rep.CachePoint) > 0 {
		fmt.Fprintln(w, "[2] cachePoint (warm/cold credits; lower = cached):")
		for _, f := range rep.CachePoint {
			fmt.Fprintf(w, "    %-26s cold %.4f warm %.4f ratio %.2f %s\n", f.Points, f.Cold, f.Warm, f.Ratio, f.Error)
		}
	}
	if c := rep.Credits; c != nil {
		fmt.Fprintf(w, "[3] credits per 1M tokens: cold context %.3f, warm context %.3f (x%.2f), output %.3f, fixed %.4f/call; %d samples, max rel err %.0f%%\n",
			c.ColdPerM, c.WarmPerM, c.WarmOverCold, c.OutputPerM, c.Fixed, c.Samples, c.MaxRelErr*100)
		b, _ := json.Marshal(c.Suggest)
		fmt.Fprintf(w, "    config: \"credit_rates\": %s\n", b)
	}
	for _, l := range rep.Limits {
		fmt.Fprintf(w, "[4] %s: Get-Usage-Limits delta %.4f, meteringEvent sum %.4f, unassigned %.4f\n", l.Experiment, l.Delta, l.Metered, l.Diff)
	}
}
