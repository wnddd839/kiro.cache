package server

import (
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"kiro-proxy/internal/meter"
)

// atomicString 是可并发读写的字符串。
type atomicString struct{ v atomic.Pointer[string] }

func (a *atomicString) Store(s string) { a.v.Store(&s) }

func (a *atomicString) Load() string {
	if p := a.v.Load(); p != nil {
		return *p
	}
	return ""
}

// priceFetch 拉在线价格；变量便于测试替换。
var priceFetch = meter.FetchPrices

// syncPrices 启动时（缓存缺失或过期）拉一次，之后按 PriceSync 周期刷新。
func (s *Server) syncPrices() {
	every := time.Duration(s.cfg.PriceSync)
	if _, at := s.pricer.Online(); time.Since(at) >= every {
		s.refreshPrices(s.bg)
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-s.bg.Done():
			return
		case <-t.C:
			s.refreshPrices(s.bg)
		}
	}
}

// wantPrice 决定 OpenRouter 的哪些模型保留：号池模型目录里的、内置表精确键。
func (s *Server) wantPrice() func(string) bool {
	ids := map[string]bool{}
	for _, m := range s.models.list() {
		ids[meter.CanonModel(m.ID)] = true
	}
	return func(id string) bool {
		return ids[id] || meter.IsBuiltinID(id)
	}
}

// refreshPrices 拉一次 OpenRouter 价格。失败时保留旧表。
func (s *Server) refreshPrices(ctx context.Context) error {
	if !s.priceBusy.TryLock() {
		return nil
	}
	defer s.priceBusy.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	got, err := priceFetch(ctx, http.DefaultClient, s.wantPrice())
	if err != nil {
		s.priceErr.Store(err.Error())
		s.log.Warn("price sync", "err", err, "models", len(got))
	} else {
		s.priceErr.Store("")
	}
	if len(got) == 0 {
		return err
	}
	if err != nil {
		// 部分来源失败：保留旧表里新表没覆盖到的条目
		old, _ := s.pricer.Online()
		for k, v := range old {
			if _, ok := got[k]; !ok {
				got[k] = v
			}
		}
	}
	s.pricer.SetOnline(got, time.Now())
	s.log.Info("prices synced", "models", len(got))
	if s.cfg.PricesFile != "" {
		if err := s.pricer.SaveOnline(s.cfg.PricesFile); err != nil {
			s.log.Warn("prices save", "file", s.cfg.PricesFile, "err", err)
		}
	}
	return err
}

// modelRow 是管理台模型表的一行：上游模型信息 + 倍率 + 生效单价。
type modelRow struct {
	ID            string      `json:"id"`
	Name          string      `json:"name"`
	Context       int         `json:"context"`
	Output        int         `json:"output"`
	Images        bool        `json:"images"`
	Multiplier    float64     `json:"multiplier,omitzero"`
	RateUnit      string      `json:"rate_unit,omitzero"`
	Description   string      `json:"description,omitzero"`
	Default       bool        `json:"default,omitzero"`
	PromptCaching any         `json:"prompt_caching,omitzero"`
	Price         meter.Price `json:"price"`
	Priced        bool        `json:"priced"` // false = 未匹配到价格，按 fallback 计
	CreditUSD     float64     `json:"credit_usd,omitzero"`
}

func (s *Server) modelRows() []modelRow {
	list := s.models.list()
	rows := make([]modelRow, 0, len(list))
	for _, m := range list {
		p, ok := s.pricer.PriceOf(m.ID)
		r := modelRow{
			ID: m.ID, Name: m.Name, Context: m.Context, Output: m.Output, Images: m.Images,
			Multiplier: m.Multiplier, RateUnit: m.RateUnit, Description: m.Description, Default: m.Default,
			Price: p, Priced: ok,
		}
		if m.PromptCaching != nil {
			r.PromptCaching = m.PromptCaching
		}
		if m.Multiplier > 0 {
			r.CreditUSD = s.pricer.CreditUSD
		}
		rows = append(rows, r)
	}
	return rows
}

func (s *Server) adminPrices(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if err := s.refreshPrices(r.Context()); err != nil {
			if online, _ := s.pricer.Online(); len(online) == 0 {
				writeError(w, http.StatusBadGateway, "price sync: "+err.Error())
				return
			}
		}
	}
	_, at := s.pricer.Online()
	resp := map[string]any{
		"prices": s.pricer.Table(), "cost_basis": s.pricer.Basis, "credit_usd": s.pricer.CreditUSD,
		"cache_mode": s.cfg.CacheMode, "unit": "USD per million tokens",
		"sync_interval": s.cfg.PriceSync, "sources": meter.PriceSources,
	}
	if !at.IsZero() {
		resp["synced_at"] = at
	}
	if e := s.priceErr.Load(); e != "" {
		resp["sync_error"] = e
	}
	writeJSON(w, http.StatusOK, resp)
}
