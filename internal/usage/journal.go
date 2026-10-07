// Package usage 记录每次下游请求的用量：按小时聚合供统计与限额，最近若干条明细供翻页查看。
// 持久化为追加写的 JSONL，启动时重放。
package usage

import (
	"cmp"
	"crypto/rand"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
)

// Entry 是一次上游尝试的记账。换号重试时，没有成为最终回复的尝试各自一行（Retried），
// 同一下游请求的各行 Request 相同。不含消息内容。
//
// 两本账：CostUSD 是对下游按 Anthropic 口径收的费；UpstreamUSD 是上游成本（credits × credit_usd）。
type Entry struct {
	ID       string    `json:"id"`
	Time     time.Time `json:"time"`
	Key      string    `json:"key,omitzero"` // 下游 key id；匿名为空
	KeyName  string    `json:"key_name,omitzero"`
	Account  string    `json:"account,omitzero"`
	Model    string    `json:"model"`    // Kiro 模型 id
	Protocol string    `json:"protocol"` // anthropic | openai-chat | openai-responses
	Stream   bool      `json:"stream,omitzero"`
	Status   int       `json:"status"`
	Error    string    `json:"error,omitzero"`
	Attempts int       `json:"attempts,omitzero"`
	Thread   string    `json:"thread,omitzero"`
	Pinned   bool      `json:"pinned,omitzero"`
	// Request 把同一下游请求的各次尝试连起来；只有一次尝试时可以为空
	Request string `json:"request,omitzero"`
	// Retried 是没有成为最终回复、换号重试了的尝试。不向下游收费（CostUSD = 0），
	// 只记 UpstreamUSD，全额计入重试亏损；不计请求数、不扣 key 预算。不同时标 Aborted。
	Retried bool `json:"retried,omitzero"`
	// Aborted 是没跑完的尝试（客户端中断 / 中途断流）；用量是已消耗的部分
	Aborted bool `json:"aborted,omitzero"`
	// CreditsEstimated 表示 Credits 是按 token 估的（上游没来得及报 meteringEvent）
	CreditsEstimated bool `json:"credits_estimated,omitzero"`

	Input      int `json:"input,omitzero"`
	CacheRead  int `json:"cache_read,omitzero"`
	CacheWrite int `json:"cache_write,omitzero"`
	// CacheWrite1h 是 CacheWrite 里按 1h TTL（2 倍价）计的部分
	CacheWrite1h int     `json:"cache_write_1h,omitzero"`
	Output       int     `json:"output,omitzero"`
	Reasoning    int     `json:"reasoning,omitzero"`
	Context      int     `json:"context,omitzero"` // Kiro 报告的上下文 token（含 Kiro 自身 system）
	Credits      float64 `json:"credits,omitzero"`
	CostUSD      float64 `json:"cost_usd,omitzero"` // 对下游收费（Anthropic 口径）
	// UpstreamUSD 是上游成本：credits × credit_usd（记账时的汇率）
	UpstreamUSD float64 `json:"upstream_usd,omitzero"`

	MS      int64 `json:"ms"`
	FirstMS int64 `json:"first_ms,omitzero"` // 首个内容事件（TTFT）
}

// OK 报告请求是否成功（2xx）。
func (e Entry) OK() bool { return e.Status/100 == 2 }

// Margin 是毛利：对下游收费减上游成本。
func (e Entry) Margin() float64 { return e.CostUSD - e.UpstreamUSD }

// Loss 报告这笔是否亏了（上游成本高于对下游收费）。
func (e Entry) Loss() bool { return e.UpstreamUSD > e.CostUSD+1e-12 }

// Totals 是一组记录的累计。
type Totals struct {
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	Input        int64   `json:"input"`
	CacheRead    int64   `json:"cache_read"`
	CacheWrite   int64   `json:"cache_write"`
	CacheWrite1h int64   `json:"cache_write_1h"`
	Output       int64   `json:"output"`
	Reasoning    int64   `json:"reasoning"`
	Credits      float64 `json:"credits"`
	CostUSD      float64 `json:"cost_usd"`
	UpstreamUSD  float64 `json:"upstream_usd"`
	// Losses / LossUSD 是亏损的笔数与亏损金额（上游成本−收费，只累加亏的那些笔）。
	// 含两类：重试亏损（RetryLosses：换号重试的失败尝试，不收费）与其它（小请求等，按 Anthropic 价收得比成本少）。
	Losses       int64   `json:"losses"`
	LossUSD      float64 `json:"loss_usd"`
	RetryLosses  int64   `json:"retry_losses"`
	RetryLossUSD float64 `json:"retry_loss_usd"`
	// RetryCredits 是 Credits 里换号重试尝试的部分：号上实际扣了，但不算进 key 的 credits 预算
	RetryCredits float64 `json:"retry_credits"`
	// Aborted / Retried 是中断与换号重试的尝试笔数（计在 Requests 里）
	Aborted int64 `json:"aborted"`
	Retried int64 `json:"retried"`
	GenMS   int64 `json:"gen_ms,omitzero"` // 成功请求的生成耗时累计（ms - first_ms）
	genOut  int64 // 成功请求的 output，只用于 TPS
}

// HitRate 是缓存命中占全部输入的比例。
func (t Totals) HitRate() float64 {
	n := t.Input + t.CacheRead + t.CacheWrite
	if n == 0 {
		return 0
	}
	return float64(t.CacheRead) / float64(n)
}

// BillableCredits 是计入 key 预算的 credits：不含换号重试失败尝试扣的。
func (t Totals) BillableCredits() float64 { return t.Credits - t.RetryCredits }

// MarginRate 是毛利率：(收费 − 上游成本) / 收费。
func (t Totals) MarginRate() float64 {
	if t.CostUSD <= 0 {
		return 0
	}
	return (t.CostUSD - t.UpstreamUSD) / t.CostUSD
}

func (t Totals) TPS() float64 {
	if t.GenMS <= 0 {
		return 0
	}
	return float64(t.genOut) * 1000 / float64(t.GenMS)
}

// Add 累加 o。
func (t *Totals) Add(o Totals) {
	t.Requests += o.Requests
	t.Errors += o.Errors
	t.Input += o.Input
	t.CacheRead += o.CacheRead
	t.CacheWrite += o.CacheWrite
	t.CacheWrite1h += o.CacheWrite1h
	t.Output += o.Output
	t.Reasoning += o.Reasoning
	t.Credits += o.Credits
	t.CostUSD += o.CostUSD
	t.UpstreamUSD += o.UpstreamUSD
	t.Losses += o.Losses
	t.LossUSD += o.LossUSD
	t.RetryLosses += o.RetryLosses
	t.RetryLossUSD += o.RetryLossUSD
	t.RetryCredits += o.RetryCredits
	t.Aborted += o.Aborted
	t.Retried += o.Retried
	t.GenMS += o.GenMS
	t.genOut += o.genOut
}

// addEntry 累加一行。换号重试的尝试（Retried）只计用量与费用，不计请求数 / 错误数：
// Requests 是下游请求数（key 的请求数预算按它算），不因为上游换号而多算。
func (t *Totals) addEntry(e Entry) {
	if !e.Retried {
		t.Requests++
		if !e.OK() {
			t.Errors++
		}
	}
	t.Input += int64(e.Input)
	t.CacheRead += int64(e.CacheRead)
	t.CacheWrite += int64(e.CacheWrite)
	t.CacheWrite1h += int64(e.CacheWrite1h)
	t.Output += int64(e.Output)
	t.Reasoning += int64(e.Reasoning)
	t.Credits += e.Credits
	t.CostUSD += e.CostUSD
	t.UpstreamUSD += e.UpstreamUSD
	if e.Loss() {
		t.Losses++
		t.LossUSD += e.UpstreamUSD - e.CostUSD
		if e.Retried {
			t.RetryLosses++
			t.RetryLossUSD += e.UpstreamUSD - e.CostUSD
		}
	}
	if e.Aborted {
		t.Aborted++
	}
	if e.Retried {
		t.Retried++
		t.RetryCredits += e.Credits
	}
	if e.OK() {
		gen := e.MS
		if e.FirstMS > 0 && e.FirstMS < e.MS {
			gen = e.MS - e.FirstMS
		}
		if gen > 0 {
			t.GenMS += gen
		}
		t.genOut += int64(e.Output)
	}
}

// totalsJSON 是 Totals 的线上形状，多 hit_rate / tps；可嵌入 Point / Group 展平。
type totalsJSON struct {
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	Input        int64   `json:"input"`
	CacheRead    int64   `json:"cache_read"`
	CacheWrite   int64   `json:"cache_write"`
	CacheWrite1h int64   `json:"cache_write_1h"`
	Output       int64   `json:"output"`
	Reasoning    int64   `json:"reasoning"`
	Credits      float64 `json:"credits"`
	CostUSD      float64 `json:"cost_usd"`
	UpstreamUSD  float64 `json:"upstream_usd"`
	MarginUSD    float64 `json:"margin_usd"`
	MarginRate   float64 `json:"margin_rate"` // 毛利 / 收费；没有收费时为 0
	Losses       int64   `json:"losses"`
	LossUSD      float64 `json:"loss_usd"`
	RetryLosses  int64   `json:"retry_losses"`
	RetryLossUSD float64 `json:"retry_loss_usd"`
	SmallLosses  int64   `json:"small_losses"`
	SmallLossUSD float64 `json:"small_loss_usd"`
	Aborted      int64   `json:"aborted"`
	Retried      int64   `json:"retried"`
	GenMS        int64   `json:"gen_ms,omitzero"`
	HitRate      float64 `json:"hit_rate"`
	TPS          float64 `json:"tps,omitzero"`
}

func (t Totals) wire() totalsJSON {
	return totalsJSON{
		Requests: t.Requests, Errors: t.Errors,
		Input: t.Input, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite, CacheWrite1h: t.CacheWrite1h,
		Output: t.Output, Reasoning: t.Reasoning,
		Credits: t.Credits, CostUSD: t.CostUSD, UpstreamUSD: t.UpstreamUSD,
		MarginUSD: t.CostUSD - t.UpstreamUSD, MarginRate: t.MarginRate(),
		Losses: t.Losses, LossUSD: t.LossUSD, Aborted: t.Aborted, Retried: t.Retried,
		RetryLosses: t.RetryLosses, RetryLossUSD: t.RetryLossUSD,
		SmallLosses: t.Losses - t.RetryLosses, SmallLossUSD: t.LossUSD - t.RetryLossUSD,
		GenMS:   t.GenMS,
		HitRate: t.HitRate(), TPS: t.TPS(),
	}
}

// MarshalJSON 附带 hit_rate。
func (t Totals) MarshalJSON() ([]byte, error) { return json.Marshal(t.wire()) }

// Point 是时间序列的一个桶。
type Point struct {
	Time time.Time
	Totals
}

// MarshalJSON 展平为 {"time":..., 累计字段...}。
func (p Point) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Time time.Time `json:"time"`
		totalsJSON
	}{p.Time, p.wire()})
}

// Group 是按某维度分组的累计。
type Group struct {
	Name  string
	Label string // ByKey 时为 key 名
	Totals
}

// MarshalJSON 展平为 {"name":..., "label":..., 累计字段...}。
func (g Group) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Name  string `json:"name"`
		Label string `json:"label,omitzero"`
		totalsJSON
	}{g.Name, g.Label, g.wire()})
}

// Query 是统计查询条件。
//
// 聚合按小时存储，From/To 按桶粒度取整：From 向下、To 向上取到小时（day 桶取到当天零点）。
// To 为零时取到当前桶的末尾（含当前小时/天）。
type Query struct {
	From, To                      time.Time // [From, To)；To 零值 = 现在；From 零值 = To-24h
	Key, Account, Model, Protocol string    // 精确匹配，空 = 不限
	Status                        string    // "" 不限 | "ok" | "error"
	Bucket                        string    // "hour" | "day" | "" 自动（<=48h 按小时，否则按天）
	Page, PageSize                int       // 明细分页，来自内存中的最近记录（新的在前）；PageSize 默认 50，最大 500
}

// View 是查询结果。
type View struct {
	From         time.Time `json:"from"`
	To           time.Time `json:"to"`
	Bucket       string    `json:"bucket"`
	Totals       Totals    `json:"totals"`
	Series       []Point   `json:"series"` // 每桶一个，零填充，升序
	ByModel      []Group   `json:"by_model"`
	ByKey        []Group   `json:"by_key"`
	ByAccount    []Group   `json:"by_account"` // 均按 Credits 降序、再按 Requests 降序
	Entries      []Entry   `json:"entries"`
	TotalEntries int       `json:"total_entries"` // 最近记录中满足条件的条数，供翻页
	Page         int       `json:"page"`
	PageSize     int       `json:"page_size"`
}

// Options 配置 Journal。零值可用。
type Options struct {
	Retention time.Duration    // 保留期，默认 90 天
	Recent    int              // 内存中保留的最近明细条数，默认 2000
	Now       func() time.Time // 测试用
	Location  *time.Location   // 小时/天边界所在时区，默认 time.Local
}

const (
	defaultRetention = 90 * 24 * time.Hour
	defaultRecent    = 2000
	defaultPageSize  = 50
	maxPageSize      = 500
	maxPending       = 32 << 20 // 写盘持续失败时待写缓冲的上限
)

// dim 是 key 之下的聚合维度。
type dim struct {
	hour     int64 // 小时起点（Location 下），unix 秒
	account  string
	model    string
	protocol string
	ok       bool
}

// keyIndex 是一个下游 key 的全部小时桶。
type keyIndex struct {
	name    string // 最近一次见到的 key 名
	buckets map[dim]*Totals
}

// ring 是固定容量的最近明细。
type ring struct {
	n    int
	buf  []Entry
	head int // 满后最旧一条的位置
}

func (r *ring) push(e Entry) {
	if r.n <= 0 {
		return
	}
	if len(r.buf) < r.n {
		r.buf = append(r.buf, e)
		return
	}
	r.buf[r.head] = e
	r.head = (r.head + 1) % r.n
}

// newest 返回第 i 新的一条（0 最新）。
func (r *ring) newest(i int) Entry {
	n := len(r.buf)
	return r.buf[(r.head+n-1-i)%n]
}

// Journal 是用量账本。并发安全。
type Journal struct {
	path      string
	retention time.Duration
	now       func() time.Time
	loc       *time.Location

	mu      sync.Mutex
	keys    map[string]*keyIndex
	recent  ring
	pending []byte // 待追加到文件的 JSONL

	ioMu sync.Mutex // 串行化写盘，保证追加顺序

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newJournal(path string, opts Options) *Journal {
	j := &Journal{
		path:      path,
		retention: opts.Retention,
		now:       opts.Now,
		loc:       opts.Location,
		keys:      map[string]*keyIndex{},
		recent:    ring{n: opts.Recent},
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	if j.retention <= 0 {
		j.retention = defaultRetention
	}
	if j.recent.n <= 0 {
		j.recent.n = defaultRecent
	}
	if j.now == nil {
		j.now = time.Now
	}
	if j.loc == nil {
		j.loc = time.Local
	}
	return j
}

// Record 记一笔。内存聚合立即更新；落盘由后台在约 250ms 内完成，锁内不做磁盘 IO。
// 缺 ID 时生成，缺 Time 时取当前时间。
func (j *Journal) Record(e Entry) {
	if e.Time.IsZero() {
		e.Time = j.now()
	}
	if e.ID == "" {
		e.ID = rand.Text()
	}
	e.Credits = finite(e.Credits)
	e.CostUSD = finite(e.CostUSD)
	e.UpstreamUSD = finite(e.UpstreamUSD)

	var line []byte
	if j.path != "" {
		if b, err := json.Marshal(e); err == nil {
			line = append(b, '\n')
		}
	}

	j.mu.Lock()
	j.addLocked(e)
	if line != nil && len(j.pending) < maxPending {
		j.pending = append(j.pending, line...)
	}
	j.mu.Unlock()

}

func finite(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return f
}

func (j *Journal) addLocked(e Entry) {
	ki := j.keys[e.Key]
	if ki == nil {
		ki = &keyIndex{buckets: map[dim]*Totals{}}
		j.keys[e.Key] = ki
	}
	if e.KeyName != "" {
		ki.name = e.KeyName
	}
	d := dim{
		hour:     j.hourStart(e.Time).Unix(),
		account:  e.Account,
		model:    e.Model,
		protocol: e.Protocol,
		ok:       e.OK(),
	}
	t := ki.buckets[d]
	if t == nil {
		t = &Totals{}
		ki.buckets[d] = t
	}
	t.addEntry(e)
	j.recent.push(e)
}

// hourStart 是 t 在 Location 下所在小时的起点；按本地钟面减去分秒，兼容半小时时区与夏令时。
func (j *Journal) hourStart(t time.Time) time.Time {
	t = t.In(j.loc)
	return t.Add(-time.Duration(t.Minute())*time.Minute -
		time.Duration(t.Second())*time.Second -
		time.Duration(t.Nanosecond()))
}

func (j *Journal) nextHour(t time.Time) time.Time { return j.hourStart(t.Add(time.Hour)) }

func (j *Journal) dayStart(t time.Time) time.Time {
	t = t.In(j.loc)
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, j.loc)
}

func (j *Journal) nextDay(t time.Time) time.Time {
	t = t.In(j.loc)
	return time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, j.loc)
}

// Spend 返回 key 自 since 起的累计。key 为 "*" 表示全部，"" 表示匿名。
// since 向下取整到小时（对限额偏保守）。耗时与该 key 的桶数成正比。
func (j *Journal) Spend(key string, since time.Time) Totals {
	from := j.hourStart(since).Unix()
	var t Totals
	add := func(ki *keyIndex) {
		for d, b := range ki.buckets {
			if d.hour >= from {
				t.Add(*b)
			}
		}
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if key == "*" {
		for _, ki := range j.keys {
			add(ki)
		}
	} else if ki := j.keys[key]; ki != nil {
		add(ki)
	}
	return t
}

// Query 汇总 [From, To) 内满足条件的用量，并分页返回最近明细。
func (j *Journal) Query(q Query) View {
	now := j.now()
	to, from := q.To, q.From
	if !to.IsZero() && !from.IsZero() && to.Before(from) {
		from, to = to, from
	}
	span := 24 * time.Hour
	if !to.IsZero() && !from.IsZero() {
		span = to.Sub(from)
	} else if !from.IsZero() {
		span = now.Sub(from)
	}

	bucket := q.Bucket
	if bucket != "hour" && bucket != "day" {
		bucket = "day"
		if span <= 48*time.Hour {
			bucket = "hour"
		}
	}
	floor, next := j.hourStart, j.nextHour
	if bucket == "day" {
		floor, next = j.dayStart, j.nextDay
	}

	// 取整到桶边界
	if to.IsZero() {
		to = next(floor(now))
		if from.IsZero() {
			from = now.Add(-span)
		}
	} else {
		if s := floor(to); s.Before(to) {
			to = next(s)
		} else {
			to = s
		}
		if from.IsZero() {
			from = q.To.Add(-span)
		}
	}
	from = floor(from)
	// 早于保留期的部分没有数据，截掉以免序列过长
	if minFrom := floor(now.Add(-j.retention)); from.Before(minFrom) {
		from = minFrom
	}
	if !from.Before(to) {
		to = next(from)
	}

	v := View{From: from, To: to, Bucket: bucket}
	slot := map[int64]int{}
	for t := from; t.Before(to); t = next(t) {
		slot[t.Unix()] = len(v.Series)
		v.Series = append(v.Series, Point{Time: t})
	}

	match := func(account, model, protocol string, ok bool) bool {
		return (q.Account == "" || q.Account == account) &&
			(q.Model == "" || q.Model == model) &&
			(q.Protocol == "" || q.Protocol == protocol) &&
			(q.Status == "" || (q.Status == "ok") == ok)
	}

	fromU, toU := from.Unix(), to.Unix()
	byModel, byKey, byAccount := map[string]*Totals{}, map[string]*Totals{}, map[string]*Totals{}
	labels := map[string]string{}
	dayOf := map[int64]int64{} // 小时起点 → 天起点，day 桶时缓存

	j.mu.Lock()
	for key, ki := range j.keys {
		if q.Key != "" && q.Key != key {
			continue
		}
		for d, b := range ki.buckets {
			if d.hour < fromU || d.hour >= toU || !match(d.account, d.model, d.protocol, d.ok) {
				continue
			}
			v.Totals.Add(*b)
			sk := d.hour
			if bucket == "day" {
				ds, ok := dayOf[d.hour]
				if !ok {
					ds = j.dayStart(time.Unix(d.hour, 0)).Unix()
					dayOf[d.hour] = ds
				}
				sk = ds
			}
			if i, ok := slot[sk]; ok {
				v.Series[i].Totals.Add(*b)
			}
			addGroup(byModel, d.model, b)
			addGroup(byKey, key, b)
			addGroup(byAccount, d.account, b)
			labels[key] = ki.name
		}
	}

	v.Page = max(q.Page, 1)
	v.PageSize = q.PageSize
	if v.PageSize <= 0 {
		v.PageSize = defaultPageSize
	}
	v.PageSize = min(v.PageSize, maxPageSize)
	skip := (v.Page - 1) * v.PageSize
	for i := range len(j.recent.buf) {
		e := j.recent.newest(i)
		if e.Time.Before(from) || !e.Time.Before(to) ||
			(q.Key != "" && q.Key != e.Key) ||
			!match(e.Account, e.Model, e.Protocol, e.OK()) {
			continue
		}
		if v.TotalEntries >= skip && len(v.Entries) < v.PageSize {
			v.Entries = append(v.Entries, e)
		}
		v.TotalEntries++
	}
	j.mu.Unlock()

	v.ByModel = groups(byModel, nil)
	v.ByKey = groups(byKey, labels)
	v.ByAccount = groups(byAccount, nil)
	return v
}

func addGroup(m map[string]*Totals, name string, b *Totals) {
	t := m[name]
	if t == nil {
		t = &Totals{}
		m[name] = t
	}
	t.Add(*b)
}

func groups(m map[string]*Totals, labels map[string]string) []Group {
	out := make([]Group, 0, len(m))
	for name, t := range m {
		out = append(out, Group{Name: name, Label: labels[name], Totals: *t})
	}
	slices.SortFunc(out, func(a, b Group) int {
		return cmp.Or(
			cmp.Compare(b.Credits, a.Credits),
			cmp.Compare(b.Requests, a.Requests),
			strings.Compare(a.Name, b.Name),
		)
	})
	return out
}

// prune 丢弃保留期之前的小时桶。
func (j *Journal) prune() {
	cutoff := j.hourStart(j.now().Add(-j.retention)).Unix()
	j.mu.Lock()
	defer j.mu.Unlock()
	for key, ki := range j.keys {
		for d := range ki.buckets {
			if d.hour < cutoff {
				delete(ki.buckets, d)
			}
		}
		if len(ki.buckets) == 0 {
			delete(j.keys, key)
		}
	}
}
