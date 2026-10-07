package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/pool"
	"kiro-proxy/internal/usage"
)

// 管理接口：下游 key、用量查询、账单、号池批量操作、价格表。
//
//	GET    /admin/keys                     列表（含本周期已用）
//	POST   /admin/keys                     新建：{"name","limit_usd","limit_credits","limit_requests","period","rpm","models","expires_at","note"}
//	PATCH  /admin/keys/{id}                修改（字段同上，只改给出的；可带 "disabled"）
//	POST   /admin/keys/{id}/rotate         换 secret
//	DELETE /admin/keys/{id}                删除
//	GET    /admin/usage?from&to&key&account&model&protocol&status&bucket&page&page_size
//	GET    /admin/billing?from&to          账单：按 key / 号 / 模型的 credits 与 token
//	POST   /admin/accounts/batch           批量：{"action":"test|refresh|limits|enable|disable|delete","ids":[...]}（ids 空 = 全部）
//	GET    /admin/prices                   生效价格表与计费口径
func (s *Server) billingRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/keys", s.listKeys)
	mux.HandleFunc("POST /admin/keys", s.createKey)
	mux.HandleFunc("PATCH /admin/keys/{id}", s.updateKey)
	mux.HandleFunc("POST /admin/keys/{id}/rotate", func(w http.ResponseWriter, r *http.Request) {
		k, err := s.keys.Rotate(r.PathValue("id"))
		if err != nil {
			writeError(w, keyErrStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, s.keyView(k, time.Now()))
	})
	mux.HandleFunc("DELETE /admin/keys/{id}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.keys.Delete(r.PathValue("id")); err != nil {
			writeError(w, keyErrStatus(err), err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /admin/usage", s.adminUsage)
	mux.HandleFunc("GET /admin/billing", s.adminBilling)
	mux.HandleFunc("POST /admin/accounts/batch", s.batchAccounts)
	mux.HandleFunc("GET /admin/prices", s.adminPrices)
	mux.HandleFunc("POST /admin/prices/refresh", s.adminPrices)
}

func keyErrStatus(err error) int {
	if errors.Is(err, keys.ErrUnknown) {
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

// keyView 是管理台里的一把 key：配置加本周期已用。本地使用，secret 原样返回便于复制。
type keyView struct {
	keys.Key
	PeriodStart time.Time    `json:"period_start"`
	PeriodEnd   time.Time    `json:"period_end,omitzero"`
	Spent       usage.Totals `json:"spent"`
	Remaining   *float64     `json:"remaining_usd,omitzero"`
	LastUsed    time.Time    `json:"last_used,omitzero"`
}

func (s *Server) keyView(k keys.Key, now time.Time) keyView {
	start := k.PeriodStart(now, time.Local)
	v := keyView{Key: k, PeriodStart: start, PeriodEnd: k.PeriodEnd(now, time.Local), Spent: s.usage.Spend(k.ID, start)}
	if k.LimitUSD > 0 {
		r := max(0, k.LimitUSD-v.Spent.CostUSD)
		v.Remaining = &r
	}
	return v
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	list := s.keys.List()
	out := make([]keyView, 0, len(list))
	last := map[string]time.Time{}
	for _, e := range s.usage.Query(usage.Query{From: now.AddDate(0, 0, -30), PageSize: 500}).Entries {
		if e.Key != "" && e.Time.After(last[e.Key]) {
			last[e.Key] = e.Time
		}
	}
	for _, k := range list {
		v := s.keyView(k, now)
		v.LastUsed = last[k.ID]
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": out, "open": len(out) == 0 && len(s.cfg.APIKeys) == 0})
}

// keyPatch 是新建 / 修改 key 的请求体。指针字段只在给出时生效。
type keyPatch struct {
	Name          *string    `json:"name"`
	Secret        *string    `json:"secret"` // 只在新建时生效；空 = 生成
	Disabled      *bool      `json:"disabled"`
	LimitUSD      *float64   `json:"limit_usd"`
	LimitCredits  *float64   `json:"limit_credits"`
	LimitRequests *int64     `json:"limit_requests"`
	Period        *string    `json:"period"`
	RPM           *int       `json:"rpm"`
	Models        *[]string  `json:"models"`
	ExpiresAt     *time.Time `json:"expires_at"`
	Note          *string    `json:"note"`
}

func set[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

func (p *keyPatch) apply(k *keys.Key) {
	set(&k.Name, p.Name)
	set(&k.Disabled, p.Disabled)
	set(&k.LimitUSD, p.LimitUSD)
	set(&k.LimitCredits, p.LimitCredits)
	set(&k.LimitRequests, p.LimitRequests)
	set(&k.Period, p.Period)
	set(&k.RPM, p.RPM)
	set(&k.Models, p.Models)
	set(&k.ExpiresAt, p.ExpiresAt)
	set(&k.Note, p.Note)
}

func decodePatch(w http.ResponseWriter, r *http.Request) (*keyPatch, bool) {
	var p keyPatch
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return nil, false
	}
	return &p, true
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	p, ok := decodePatch(w, r)
	if !ok {
		return
	}
	var k keys.Key
	p.apply(&k)
	set(&k.Secret, p.Secret)
	created, err := s.keys.Create(k)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, s.keyView(created, time.Now()))
}

func (s *Server) updateKey(w http.ResponseWriter, r *http.Request) {
	p, ok := decodePatch(w, r)
	if !ok {
		return
	}
	k, err := s.keys.Update(r.PathValue("id"), p.apply)
	if err != nil {
		writeError(w, keyErrStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.keyView(k, time.Now()))
}

// parseQuery 读取 from / to：RFC3339、YYYY-MM-DD（本地时区）或相对时长（如 24h、7d，表示最近这么久）。
func parseQuery(r *http.Request) (usage.Query, error) {
	v := r.URL.Query()
	q := usage.Query{
		Key: v.Get("key"), Account: v.Get("account"), Model: v.Get("model"), Protocol: v.Get("protocol"),
		Status: v.Get("status"), Bucket: v.Get("bucket"),
	}
	q.Page, _ = strconv.Atoi(v.Get("page"))
	q.PageSize, _ = strconv.Atoi(v.Get("page_size"))
	now := time.Now()
	var err error
	if q.From, err = parseWhen(v.Get("from"), now); err != nil {
		return q, err
	}
	if q.To, err = parseWhen(v.Get("to"), now); err != nil {
		return q, err
	}
	switch q.Bucket {
	case "", "hour", "day":
	default:
		return q, errors.New("bucket must be hour or day")
	}
	return q, nil
}

func parseWhen(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation(time.DateOnly, s, time.Local); err == nil {
		return t, nil
	}
	if days, ok := cutDays(s); ok {
		return now.AddDate(0, 0, -days), nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, errors.New("time must be RFC3339, YYYY-MM-DD, or a span like 24h / 7d: " + s)
}

func cutDays(s string) (int, bool) {
	if n := len(s); n > 1 && s[n-1] == 'd' {
		if d, err := strconv.Atoi(s[:n-1]); err == nil && d > 0 {
			return d, true
		}
	}
	return 0, false
}

func (s *Server) adminUsage(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.usage.Query(q))
}

// billLine 是账单的一行。
type billLine struct {
	Name       string  `json:"name"`
	Label      string  `json:"label,omitzero"`
	Requests   int64   `json:"requests"`
	Errors     int64   `json:"errors"`
	Prompt     int64   `json:"prompt_tokens"`
	CacheRead  int64   `json:"cache_read"`
	CacheWrite int64   `json:"cache_write"`
	Output     int64   `json:"output"`
	HitRate    float64 `json:"hit_rate"`
	Credits    float64 `json:"credits"`
	TPS        float64 `json:"tps,omitzero"`
	Share      float64 `json:"share"` // 占总 credits 比例

	// 两本账：Charged 是对下游按 Anthropic 口径收的，Upstream 是 credits × credit_usd
	CacheWrite1h int64   `json:"cache_write_1h"`
	Charged      float64 `json:"charged_usd"`
	Upstream     float64 `json:"upstream_usd"`
	Margin       float64 `json:"margin_usd"`
	MarginRate   float64 `json:"margin_rate"`
	Losses       int64   `json:"losses"`
	LossUSD      float64 `json:"loss_usd"`
	RetryLosses  int64   `json:"retry_losses"`
	RetryLossUSD float64 `json:"retry_loss_usd"`
	SmallLosses  int64   `json:"small_losses"`
	SmallLossUSD float64 `json:"small_loss_usd"`
	Aborted      int64   `json:"aborted"`
	Retried      int64   `json:"retried"`
}

func (s *Server) line(name, label string, t usage.Totals, total float64) billLine {
	l := billLine{
		Name: name, Label: label, Requests: t.Requests, Errors: t.Errors,
		Prompt: t.Input + t.CacheRead + t.CacheWrite, CacheRead: t.CacheRead, CacheWrite: t.CacheWrite, Output: t.Output,
		HitRate: t.HitRate(), Credits: t.Credits, TPS: t.TPS(),
		CacheWrite1h: t.CacheWrite1h, Charged: t.CostUSD, Upstream: t.UpstreamUSD,
		Margin: t.CostUSD - t.UpstreamUSD, MarginRate: t.MarginRate(),
		Losses: t.Losses, LossUSD: t.LossUSD, Aborted: t.Aborted, Retried: t.Retried,
		RetryLosses: t.RetryLosses, RetryLossUSD: t.RetryLossUSD,
		SmallLosses: t.Losses - t.RetryLosses, SmallLossUSD: t.LossUSD - t.RetryLossUSD,
	}
	if total > 0 {
		l.Share = t.Credits / total
	}
	return l
}

func (s *Server) adminBilling(w http.ResponseWriter, r *http.Request) {
	q, err := parseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if r.URL.Query().Get("from") == "" {
		// 账单默认本月
		now := time.Now()
		q.From = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	}
	q.Bucket, q.PageSize = "day", 1
	v := s.usage.Query(q)
	total := v.Totals.Credits
	groups := func(gs []usage.Group) []billLine {
		out := make([]billLine, 0, len(gs))
		for _, g := range gs {
			label := g.Label
			if g.Name == "" && label == "" {
				label = "(anonymous)"
			}
			out = append(out, s.line(g.Name, label, g.Totals, total))
		}
		return out
	}
	byAccount := groups(v.ByAccount)
	for i := range byAccount {
		if a, ok := s.pool.Get(byAccount[i].Name); ok {
			byAccount[i].Label = a.Label
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"from": v.From, "to": v.To,
		"totals":     s.line("total", "", v.Totals, total),
		"by_key":     groups(v.ByKey),
		"by_account": byAccount,
		"by_model":   groups(v.ByModel),
		"daily":      v.Series,
		"credit_usd": s.pricer.CreditUSD,
		"recon":      s.reconView(),
		"hidden":     s.hidden.stats(),
	})
}

// reconRow 是一个号的未归属 credits 对账。
type reconRow struct {
	ID    string `json:"id"`
	Label string `json:"label,omitzero"`
	pool.Recon
	UnassignedUSD float64   `json:"unassigned_usd"`
	LimitsAt      time.Time `json:"limits_at,omitzero"`
}

// reconView 是各号与合计的对账。只覆盖本进程启动后、至少拉过两次额度的时段。
func (s *Server) reconView() map[string]any {
	var rows []reconRow
	var total pool.Recon
	for _, v := range s.pool.List() {
		if v.Recon.Since.IsZero() {
			continue
		}
		rows = append(rows, reconRow{ID: v.ID, Label: v.Label, Recon: v.Recon,
			UnassignedUSD: s.pricer.CreditsUSD(v.Recon.Unassigned), LimitsAt: v.LimitsAt})
		total.Upstream += v.Recon.Upstream
		total.Recorded += v.Recon.Recorded
		total.Unassigned += v.Recon.Unassigned
	}
	return map[string]any{
		"accounts":         rows,
		"upstream_credits": total.Upstream, "recorded_credits": total.Recorded,
		"unassigned_credits": total.Unassigned, "unassigned_usd": s.pricer.CreditsUSD(total.Unassigned),
	}
}

// batchResult 是批量操作里一个号的结果。
type batchResult struct {
	ID      string  `json:"id"`
	OK      bool    `json:"ok"`
	Error   string  `json:"error,omitzero"`
	MS      int64   `json:"ms,omitzero"`
	Credits float64 `json:"credits,omitzero"` // limits 后的已用
	Limit   float64 `json:"limit,omitzero"`
	Email   string  `json:"email,omitzero"`
}

// batchConcurrency 限制批量操作的并发，避免一次打爆上游。
const batchConcurrency = 4

func (s *Server) batchAccounts(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Action string   `json:"action"`
		IDs    []string `json:"ids"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !slices.Contains([]string{"test", "refresh", "limits", "enable", "disable", "delete"}, in.Action) {
		writeError(w, http.StatusBadRequest, "action must be test, refresh, limits, enable, disable or delete")
		return
	}
	ids := in.IDs
	if len(ids) == 0 {
		if in.Action == "delete" {
			writeError(w, http.StatusBadRequest, "delete needs explicit ids")
			return
		}
		for _, v := range s.pool.List() {
			ids = append(ids, v.ID)
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	results := make([]batchResult, len(ids))
	sem := make(chan struct{}, batchConcurrency)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			start := time.Now()
			res := s.batchOne(ctx, in.Action, id)
			res.ID, res.MS = id, time.Since(start).Milliseconds()
			results[i] = res
		})
	}
	wg.Wait()
	ok := 0
	for _, r := range results {
		if r.OK {
			ok++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"action": in.Action, "ok": ok, "failed": len(results) - ok, "results": results})
}

func (s *Server) batchOne(ctx context.Context, action, id string) batchResult {
	if _, ok := s.pool.Get(id); !ok {
		return batchResult{Error: "account not found"}
	}
	var err error
	var res batchResult
	switch action {
	case "enable":
		err = s.pool.SetDisabled(id, false, "")
	case "disable":
		err = s.pool.SetDisabled(id, true, "disabled by admin")
	case "delete":
		err = s.pool.Remove(id)
		s.cache.Forget(id)
	case "refresh":
		_, err = s.pool.ForceRefresh(ctx, id)
	case "limits", "test":
		// test：换取可用凭证（必要时刷新）并读额度——不花 credits 就能验证号是否可用
		if action == "test" {
			if _, err = s.pool.Cred(ctx, id, ""); err != nil {
				break
			}
		}
		l, lerr := s.pool.RefreshLimits(ctx, id)
		err = lerr
		res.Credits, res.Limit, res.Email = l.Used, l.Limit, l.Email
	}
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.OK = true
	return res
}
