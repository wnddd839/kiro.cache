// Package pool 是本地 Kiro 号池：账号持久化、单号单飞 refresh、粘滞优先选号、冷却与额度轮询。
//
// 选号原则：同一会话始终回到上次的号（上游 cache 跟号走）；只有该号不可用时才换。
// 新会话挑并发最少、剩余额度最多、最久没用的号。不做 round-robin。
package pool

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"kiro-go/internal/kiro"
	"kiro-go/internal/sessionpin"
)

// refreshLead 是提前刷新的窗口。
const refreshLead = 2 * time.Minute

// Source 是凭证的来源。
const (
	SourceStored = ""    // 凭证只存在号池文件里
	SourceIDE    = "ide" // 来自 Kiro IDE 的 token 文件；刷新后写回，IDE 可以继续用
)

// Account 是号池文件里的一条账号。
type Account struct {
	ID            string    `json:"id"`
	Label         string    `json:"label,omitzero"`
	Disabled      bool      `json:"disabled,omitzero"`
	Note          string    `json:"note,omitzero"` // 被停用的原因
	Source        string    `json:"source,omitzero"`
	SourcePath    string    `json:"source_path,omitzero"`
	MaxConcurrent int       `json:"max_concurrent,omitzero"` // 0 不限
	Cred          kiro.Cred `json:"cred"`
}

// Stats 是一个号的累计计数。
type Stats struct {
	Requests     int64 `json:"requests"`
	Errors       int64 `json:"errors"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CacheRead    int64 `json:"cache_read_tokens"`
	CacheWrite   int64 `json:"cache_write_tokens"`
}

// Add 累加一次成功请求的 usage。
func (s *Stats) Add(u kiro.Usage) {
	s.Requests++
	s.InputTokens += int64(u.Input)
	s.OutputTokens += int64(u.Output)
	s.CacheRead += int64(u.CacheRead)
	s.CacheWrite += int64(u.CacheWrite)
}

// HitRate 是 cacheRead / 全部输入。
func (s Stats) HitRate() float64 {
	total := s.InputTokens + s.CacheRead + s.CacheWrite
	if total == 0 {
		return 0
	}
	return float64(s.CacheRead) / float64(total)
}

// View 是一个号的脱敏快照，给管理 API 用。
type View struct {
	ID            string      `json:"id"`
	Label         string      `json:"label,omitzero"`
	Method        kiro.Method `json:"method"`
	Source        string      `json:"source,omitzero"`
	Region        string      `json:"region"`
	Disabled      bool        `json:"disabled"`
	Note          string      `json:"note,omitzero"`
	ExpiresAt     time.Time   `json:"expires_at,omitzero"`
	InFlight      int         `json:"in_flight"`
	CooldownUntil time.Time   `json:"cooldown_until,omitzero"`
	CooldownWhy   string      `json:"cooldown_reason,omitzero"`
	Limits        kiro.Limits `json:"limits"`
	LimitsAt      time.Time   `json:"limits_at,omitzero"`
	Stats         Stats       `json:"stats"`
	HitRate       float64     `json:"cache_hit_rate"`
}

type slot struct {
	acct Account

	refreshMu sync.Mutex // 同一号同时只有一次 refresh；refresh token 可能轮换

	// 以下字段由 Pool.mu 保护
	inflight    int
	cooldown    time.Time
	cooldownWhy string
	strikes     int
	lastUsed    time.Time
	limits      kiro.Limits
	limitsAt    time.Time
	stats       Stats
}

// Pool 是号池。零值不可用，用 Open。
type Pool struct {
	path   string
	client *kiro.Client
	log    *slog.Logger
	now    func() time.Time

	mu    sync.Mutex
	slots map[string]*slot
	order []string // 文件顺序
	pins  *sessionpin.Table

	saveMu sync.Mutex
}

// ErrNoAccount 表示当前没有可用账号。
var ErrNoAccount = errors.New("no usable Kiro account")

// UnavailableError 是没有可用号时的详情：RetryAt 是最早恢复时间（可能为零）。
type UnavailableError struct {
	RetryAt time.Time
	Reason  string
}

func (e *UnavailableError) Error() string {
	if e.Reason != "" {
		return ErrNoAccount.Error() + ": " + e.Reason
	}
	return ErrNoAccount.Error()
}

func (e *UnavailableError) Unwrap() error { return ErrNoAccount }

// Options 配置号池。
type Options struct {
	Client *kiro.Client
	Logger *slog.Logger
	PinTTL time.Duration // 会话→号粘滞时长
}

type file struct {
	Accounts []Account `json:"accounts"`
}

// Open 读号池文件；文件不存在时从空池开始。
func Open(path string, opts Options) (*Pool, error) {
	p := &Pool{
		path:   path,
		client: cmp.Or(opts.Client, kiro.NewClient(nil)),
		log:    cmp.Or(opts.Logger, slog.Default()),
		now:    time.Now,
		slots:  map[string]*slot{},
		pins:   sessionpin.New(opts.PinTTL),
	}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return p, nil
	case err != nil:
		return nil, err
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for _, a := range f.Accounts {
		if a.ID == "" || p.slots[a.ID] != nil {
			return nil, fmt.Errorf("%s: empty or duplicate account id %q", path, a.ID)
		}
		p.slots[a.ID] = &slot{acct: a}
		p.order = append(p.order, a.ID)
	}
	return p, nil
}

// Len 是账号数。
func (p *Pool) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.order)
}

// Add 加一个号并落盘。ID 为空时生成。
func (p *Pool) Add(a Account) (Account, error) {
	if a.Cred.AccessToken == "" && a.Cred.RefreshToken == "" {
		return Account{}, errors.New("account needs an access or refresh token")
	}
	if a.Cred.Method == "" {
		a.Cred.Method = kiro.MethodSocial
	}
	p.mu.Lock()
	if a.ID == "" {
		a.ID = newID()
	}
	if p.slots[a.ID] != nil {
		p.mu.Unlock()
		return Account{}, fmt.Errorf("account %q already exists", a.ID)
	}
	p.slots[a.ID] = &slot{acct: a}
	p.order = append(p.order, a.ID)
	p.mu.Unlock()
	return a, p.save()
}

// Remove 删一个号并落盘。
func (p *Pool) Remove(id string) error {
	p.mu.Lock()
	if p.slots[id] == nil {
		p.mu.Unlock()
		return fmt.Errorf("account %q not found", id)
	}
	delete(p.slots, id)
	p.order = slices.DeleteFunc(p.order, func(s string) bool { return s == id })
	p.mu.Unlock()
	return p.save()
}

// SetDisabled 停用 / 启用一个号。启用时清掉冷却与备注。
func (p *Pool) SetDisabled(id string, disabled bool, note string) error {
	p.mu.Lock()
	s := p.slots[id]
	if s == nil {
		p.mu.Unlock()
		return fmt.Errorf("account %q not found", id)
	}
	s.acct.Disabled, s.acct.Note = disabled, note
	if !disabled {
		s.cooldown, s.cooldownWhy, s.strikes = time.Time{}, "", 0
	}
	p.mu.Unlock()
	return p.save()
}

// List 是所有号的脱敏快照，按文件顺序。
func (p *Pool) List() []View {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]View, 0, len(p.order))
	for _, id := range p.order {
		out = append(out, p.viewLocked(p.slots[id]))
	}
	return out
}

// Get 是一个号的快照。
func (p *Pool) Get(id string) (View, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.slots[id]
	if s == nil {
		return View{}, false
	}
	return p.viewLocked(s), true
}

func (p *Pool) viewLocked(s *slot) View {
	v := View{
		ID: s.acct.ID, Label: s.acct.Label, Method: s.acct.Cred.Method, Source: s.acct.Source,
		Region: s.acct.Cred.APIRegion(), Disabled: s.acct.Disabled, Note: s.acct.Note,
		ExpiresAt: s.acct.Cred.ExpiresAt, InFlight: s.inflight,
		Limits: s.limits, LimitsAt: s.limitsAt, Stats: s.stats, HitRate: s.stats.HitRate(),
	}
	if p.now().Before(s.cooldown) {
		v.CooldownUntil, v.CooldownWhy = s.cooldown, s.cooldownWhy
	}
	return v
}

// Totals 是全池累计。
func (p *Pool) Totals() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	var t Stats
	for _, s := range p.slots {
		t.Requests += s.stats.Requests
		t.Errors += s.stats.Errors
		t.InputTokens += s.stats.InputTokens
		t.OutputTokens += s.stats.OutputTokens
		t.CacheRead += s.stats.CacheRead
		t.CacheWrite += s.stats.CacheWrite
	}
	return t
}

// Lease 是一次选号的结果。用完必须 Release。
type Lease struct {
	ID     string
	Pinned bool // 是否命中会话粘滞
	pool   *Pool
	once   sync.Once
}

// Release 归还并发名额。可重复调用。
func (l *Lease) Release() {
	l.once.Do(func() {
		l.pool.mu.Lock()
		if s := l.pool.slots[l.ID]; s != nil && s.inflight > 0 {
			s.inflight--
		}
		l.pool.mu.Unlock()
	})
}

// Acquire 为会话 key 选一个号，跳过 exclude。key 为空时不粘滞。
func (p *Pool) Acquire(key string, exclude []string) (*Lease, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if id, ok := p.pins.Lookup(key); ok && !slices.Contains(exclude, id) {
		if s := p.slots[id]; s != nil && p.usableLocked(s, now) {
			return p.leaseLocked(s, now, true), nil
		}
	}
	var candidates []*slot
	for _, id := range p.order {
		if s := p.slots[id]; !slices.Contains(exclude, id) && p.usableLocked(s, now) {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return nil, p.unavailableLocked(now, exclude)
	}
	best := slices.MinFunc(candidates, func(a, b *slot) int {
		return cmp.Or(
			cmp.Compare(a.inflight, b.inflight),
			cmp.Compare(b.limits.Remaining(), a.limits.Remaining()), // 剩余多的优先
			a.lastUsed.Compare(b.lastUsed),
		)
	})
	return p.leaseLocked(best, now, false), nil
}

func (p *Pool) usableLocked(s *slot, now time.Time) bool {
	return !s.acct.Disabled && !now.Before(s.cooldown) && (s.acct.MaxConcurrent <= 0 || s.inflight < s.acct.MaxConcurrent)
}

func (p *Pool) leaseLocked(s *slot, now time.Time, pinned bool) *Lease {
	s.inflight++
	s.lastUsed = now
	return &Lease{ID: s.acct.ID, Pinned: pinned, pool: p}
}

func (p *Pool) unavailableLocked(now time.Time, exclude []string) error {
	if len(p.order) == 0 {
		return &UnavailableError{Reason: "the pool is empty; add an account"}
	}
	var soonest time.Time
	var why string
	for _, id := range p.order {
		s := p.slots[id]
		if s.acct.Disabled || slices.Contains(exclude, id) {
			continue
		}
		if now.Before(s.cooldown) && (soonest.IsZero() || s.cooldown.Before(soonest)) {
			soonest, why = s.cooldown, s.cooldownWhy
		}
	}
	if soonest.IsZero() {
		return &UnavailableError{Reason: "all accounts are disabled, busy or already tried"}
	}
	return &UnavailableError{RetryAt: soonest, Reason: "all accounts cooling down: " + why}
}

// Pin 把会话钉到号上。成功向下游输出后调用。
func (p *Pool) Pin(key, id string) { p.pins.Remember(key, id) }

// Unpin 解除会话粘滞。
func (p *Pool) Unpin(key string) { p.pins.Forget(key) }

// Success 记一次成功，清掉限流计数。
func (p *Pool) Success(id string, u kiro.Usage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if s := p.slots[id]; s != nil {
		s.stats.Add(u)
		s.strikes = 0
	}
}

// Fail 按失败类别给号冷却。返回是否值得换号重试。
func (p *Pool) Fail(id string, f kiro.Failure) (retry bool) {
	p.mu.Lock()
	s := p.slots[id]
	if s == nil {
		p.mu.Unlock()
		return f.Class != kiro.ClassFatal
	}
	s.stats.Errors++
	now := p.now()
	switch f.Class {
	case kiro.ClassQuota:
		until := now.Add(time.Hour)
		if s.limits.ResetAt.After(now) {
			until = s.limits.ResetAt
		}
		p.coolLocked(s, until, "usage limit: "+f.Message)
	case kiro.ClassThrottle:
		backoff := min(15*time.Second<<s.strikes, 5*time.Minute)
		s.strikes = min(s.strikes+1, 8)
		p.coolLocked(s, now.Add(backoff), "throttled: "+f.Message)
	case kiro.ClassTransient:
		p.coolLocked(s, now.Add(10*time.Second), "upstream: "+f.Message)
	case kiro.ClassAuth:
		p.coolLocked(s, now.Add(time.Minute), "auth: "+f.Message)
	}
	p.mu.Unlock()
	p.log.Warn("account failure", "account", id, "status", f.Status, "class", f.Class, "msg", f.Message)
	return f.Class != kiro.ClassFatal
}

func (p *Pool) coolLocked(s *slot, until time.Time, why string) {
	if until.After(s.cooldown) {
		s.cooldown, s.cooldownWhy = until, why
	}
}

// Cred 返回号的可用凭证：快过期就刷新，缺 profile 就补。
// stale 非空表示上游刚拒了这个 access token（403）：若号上仍是它就强制刷新，
// 若已被并发请求刷新过则直接用新的，不重复花 refresh token。
// refresh 被拒（凭证失效）时停用该号。
func (p *Pool) Cred(ctx context.Context, id, stale string) (kiro.Cred, error) {
	p.mu.Lock()
	s := p.slots[id]
	p.mu.Unlock()
	if s == nil {
		return kiro.Cred{}, fmt.Errorf("account %q not found", id)
	}
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	p.mu.Lock()
	acct := s.acct
	p.mu.Unlock()
	cred := acct.Cred

	if acct.Source == SourceIDE && acct.SourcePath != "" {
		// IDE 可能自己刷新过：用文件里更新的 token，避免拿旧 refresh token 去换
		if fromFile, err := kiro.ReadIDE(acct.SourcePath); err == nil && fromFile.ExpiresAt.After(cred.ExpiresAt) {
			fromFile.ProfileArn = cmp.Or(fromFile.ProfileArn, cred.ProfileArn)
			cred = fromFile
		}
	}

	// 只有 refresh token 的号（手动加的 idc / social）没有 access token，也得先换
	force := cred.AccessToken == "" || (stale != "" && stale == cred.AccessToken)
	changed := cred != acct.Cred
	if force || !cred.Fresh(refreshLead) {
		n, err := p.client.Refresh(ctx, cred)
		if err != nil {
			if kiro.IsGone(err) {
				_ = p.SetDisabled(id, true, "sign-in expired: "+err.Error())
				p.log.Error("account disabled", "account", id, "err", err)
			}
			return kiro.Cred{}, err
		}
		cred, changed = n, true
		if acct.Source == SourceIDE && acct.SourcePath != "" {
			if err := kiro.WriteBackIDE(acct.SourcePath, cred); err != nil {
				p.log.Warn("write back IDE token", "account", id, "err", err)
			}
		}
		p.log.Info("token refreshed", "account", id, "expires", cred.ExpiresAt.Format(time.RFC3339))
	}
	if cred.ProfileArn == "" {
		arn, err := p.client.Profile(ctx, cred)
		if err != nil {
			return kiro.Cred{}, fmt.Errorf("find profile: %w", err)
		}
		cred.ProfileArn, changed = arn, true
	}
	if changed {
		p.mu.Lock()
		s.acct.Cred = cred
		p.mu.Unlock()
		if err := p.save(); err != nil {
			p.log.Error("save pool", "err", err)
		}
	}
	return cred, nil
}

// ForceRefresh 无论是否快过期都刷新一次 token。
func (p *Pool) ForceRefresh(ctx context.Context, id string) (kiro.Cred, error) {
	p.mu.Lock()
	s := p.slots[id]
	var current string
	if s != nil {
		current = s.acct.Cred.AccessToken
	}
	p.mu.Unlock()
	return p.Cred(ctx, id, cmp.Or(current, "-"))
}

// Client 是号池用的 Kiro 客户端。
func (p *Pool) Client() *kiro.Client { return p.client }

// RefreshLimits 拉一个号的额度；用尽则冷却到重置时间，没有名字就用邮箱。
func (p *Pool) RefreshLimits(ctx context.Context, id string) (kiro.Limits, error) {
	cred, err := p.Cred(ctx, id, "")
	if err != nil {
		return kiro.Limits{}, err
	}
	l, err := p.client.UsageLimits(ctx, cred)
	if err != nil {
		return kiro.Limits{}, err
	}
	p.mu.Lock()
	s := p.slots[id]
	if s == nil {
		p.mu.Unlock()
		return l, nil
	}
	s.limits, s.limitsAt = l, p.now()
	if l.Limit > 0 && l.Remaining() <= 0 {
		until := l.ResetAt
		if !until.After(p.now()) {
			until = p.now().Add(time.Hour)
		}
		p.coolLocked(s, until, "credits exhausted")
	}
	relabel := s.acct.Label == "" && l.Email != ""
	if relabel {
		s.acct.Label = l.Email
	}
	p.mu.Unlock()
	if relabel {
		return l, p.save()
	}
	return l, nil
}

// PollLimits 每隔 every 刷新所有启用号的额度，直到 ctx 结束。第一轮立即跑。
func (p *Pool) PollLimits(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		for _, v := range p.List() {
			if v.Disabled {
				continue
			}
			if _, err := p.RefreshLimits(ctx, v.ID); err != nil && ctx.Err() == nil {
				p.log.Warn("usage limits", "account", v.ID, "err", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *Pool) save() error {
	p.saveMu.Lock()
	defer p.saveMu.Unlock()
	p.mu.Lock()
	f := file{Accounts: make([]Account, 0, len(p.order))}
	for _, id := range p.order {
		f.Accounts = append(f.Accounts, p.slots[id].acct)
	}
	p.mu.Unlock()
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return kiro.WriteFileAtomic(p.path, raw)
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "acc_" + hex.EncodeToString(b[:])
}
