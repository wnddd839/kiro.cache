// Package keys 是下游 API Key 库：每把 key 有自己的预算、周期、模型白名单与每分钟请求上限。
// 用量由 usage.Journal 记，这里只存配置与做准入判断。
//
// secret 只存 sha256（keys.json 里是 secret_sha256 与用于辨认的前后缀）。明文只在新建与换 secret 的
// 返回值里出现一次；丢了只能换一个。旧版本的明文 keys.json 在第一次载入时自动改成哈希。
package keys

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// 预算周期。
const (
	PeriodDay   = "day"
	PeriodWeek  = "week"
	PeriodMonth = "month"
	PeriodTotal = "total"
)

// Key 是一把下游 key。
type Key struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	// Secret 是明文：只在 Create / Rotate 的返回值里有，其它时候为空，从不落盘。
	Secret string `json:"secret,omitzero"`
	// Hash 是 secret 的 sha256（十六进制），身份校验只看它。不对外输出。
	Hash string `json:"-"`
	// Hint 是辨认用的遮掩 secret（sk-kiro…ab12）。
	Hint      string    `json:"hint,omitzero"`
	Disabled  bool      `json:"disabled,omitzero"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	// LimitUSD 是一个周期内的费用上限；0 不限。
	LimitUSD float64 `json:"limit_usd,omitzero"`
	// LimitCredits 是一个周期内的 Kiro credits 上限；0 不限。
	LimitCredits float64 `json:"limit_credits,omitzero"`
	// LimitRequests 是一个周期内的请求数上限；0 不限。
	LimitRequests int64 `json:"limit_requests,omitzero"`
	// Period 是预算周期：day / week / month / total（默认 month）。
	Period string `json:"period,omitzero"`
	// RPM 是每分钟请求上限；0 不限。
	RPM int `json:"rpm,omitzero"`
	// Models 是允许的模型（Kiro 模型 id 或下游别名），支持末尾 * 通配；空不限。
	Models []string `json:"models,omitzero"`
	// Note 是备注。
	Note string `json:"note,omitzero"`
}

// PeriodOrDefault 是生效的周期。
func (k *Key) PeriodOrDefault() string {
	if k.Period == "" {
		return PeriodMonth
	}
	return k.Period
}

// PeriodStart 是 now 所在周期的起点（loc 时区）。total 返回创建时间。
func (k *Key) PeriodStart(now time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	t := now.In(loc)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, loc)
	switch k.PeriodOrDefault() {
	case PeriodDay:
		return day
	case PeriodWeek:
		off := (int(t.Weekday()) + 6) % 7 // 周一为起点
		return day.AddDate(0, 0, -off)
	case PeriodTotal:
		return k.CreatedAt
	}
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, loc)
}

// Allows 报告 key 是否允许该模型。names 是同一模型的各种叫法（下游名、Kiro id）。
func (k *Key) Allows(names ...string) bool {
	if len(k.Models) == 0 {
		return true
	}
	for _, pat := range k.Models {
		pat = strings.ToLower(strings.TrimSpace(pat))
		for _, n := range names {
			n = strings.ToLower(n)
			if p, ok := strings.CutSuffix(pat, "*"); ok && strings.HasPrefix(n, p) || pat == n {
				return true
			}
		}
	}
	return false
}

// Masked 是给日志 / 列表用的遮掩 secret。
func (k *Key) Masked() string {
	if k.Hint != "" {
		return k.Hint
	}
	return mask(k.Secret)
}

func mask(secret string) string {
	if len(secret) <= 12 {
		return "****"
	}
	return secret[:7] + "…" + secret[len(secret)-4:]
}

// HashSecret 是 secret 的 sha256 十六进制。
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// stored 是 keys.json 里的一把 key：只有哈希。Secret 只用于读旧版的明文文件。
type stored struct {
	Key
	Secret string `json:"secret,omitzero"`
	Hash   string `json:"secret_sha256,omitzero"`
}

// Spent 是一个周期内已用的量。
type Spent struct {
	Requests int64
	Credits  float64
	CostUSD  float64
}

// 准入失败的原因。
var (
	ErrUnknown  = errors.New("invalid api key")
	ErrDisabled = errors.New("api key disabled")
	ErrExpired  = errors.New("api key expired")
	ErrModel    = errors.New("model not allowed for this api key")
)

// LimitError 是超出预算或频率。RetryAfter 是建议的重试时间（周期末 / 下一分钟）。
type LimitError struct {
	What       string // "budget" | "credits" | "requests" | "rpm"
	Limit      float64
	Used       float64
	RetryAfter time.Duration
}

func (e *LimitError) Error() string {
	if e.What == "rpm" {
		return fmt.Sprintf("api key rate limit: %g requests per minute", e.Limit)
	}
	return fmt.Sprintf("api key %s limit reached: used %.4g of %.4g", e.What, e.Used, e.Limit)
}

// Check 做不含频率的准入判断：启用、过期、预算。
func (k *Key) Check(now time.Time, spent Spent, loc *time.Location) error {
	if k.Disabled {
		return ErrDisabled
	}
	if !k.ExpiresAt.IsZero() && !now.Before(k.ExpiresAt) {
		return ErrExpired
	}
	retry := func() time.Duration {
		if end := k.PeriodEnd(now, loc); !end.IsZero() {
			return end.Sub(now)
		}
		return 0
	}
	switch {
	case k.LimitUSD > 0 && spent.CostUSD >= k.LimitUSD:
		return &LimitError{What: "budget", Limit: k.LimitUSD, Used: spent.CostUSD, RetryAfter: retry()}
	case k.LimitCredits > 0 && spent.Credits >= k.LimitCredits:
		return &LimitError{What: "credits", Limit: k.LimitCredits, Used: spent.Credits, RetryAfter: retry()}
	case k.LimitRequests > 0 && spent.Requests >= k.LimitRequests:
		return &LimitError{What: "requests", Limit: float64(k.LimitRequests), Used: float64(spent.Requests), RetryAfter: retry()}
	}
	return nil
}

// PeriodEnd 是当前周期的终点；total 返回零值。
func (k *Key) PeriodEnd(now time.Time, loc *time.Location) time.Time {
	start := k.PeriodStart(now, loc)
	switch k.PeriodOrDefault() {
	case PeriodDay:
		return start.AddDate(0, 0, 1)
	case PeriodWeek:
		return start.AddDate(0, 0, 7)
	case PeriodMonth:
		return start.AddDate(0, 1, 0)
	}
	return time.Time{}
}

// Store 是 key 库。并发安全。
type Store struct {
	path string
	now  func() time.Time

	mu   sync.RWMutex
	keys []*Key
	rpm  map[string][]time.Time // id → 最近一分钟的请求时间
}

// Open 读取 path 处的 key 库；文件不存在时为空。path 为空时只在内存中。
func Open(path string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	s := &Store{path: path, now: now, rpm: map[string][]time.Time{}}
	if path == "" {
		return s, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	var f struct {
		Keys []*stored `json:"keys"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	seen := map[string]bool{}
	migrated := false
	for _, st := range f.Keys {
		if st == nil {
			continue
		}
		k := st.Key
		k.Hash = strings.ToLower(st.Hash)
		if st.Secret != "" {
			// 旧版明文：改存哈希
			k.Hash, k.Hint = HashSecret(st.Secret), mask(st.Secret)
			migrated = true
		}
		k.Secret = ""
		if len(k.Hash) != sha256.Size*2 || seen[k.Hash] {
			continue
		}
		seen[k.Hash] = true
		if k.ID == "" {
			k.ID = newID()
		}
		s.keys = append(s.keys, &k)
	}
	if migrated {
		if err := s.save(); err != nil {
			return nil, fmt.Errorf("%s: rewrite secrets as hashes: %w", path, err)
		}
	}
	return s, nil
}

// Len 是 key 数。
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.keys)
}

// Lookup 按 secret 找 key：比较 sha256（常数时间）。返回副本。
func (s *Store) Lookup(secret string) (Key, bool) {
	if secret == "" {
		return Key{}, false
	}
	h := []byte(HashSecret(secret))
	s.mu.RLock()
	defer s.mu.RUnlock()
	var found *Key
	for _, k := range s.keys {
		if subtle.ConstantTimeCompare([]byte(k.Hash), h) == 1 {
			found = k
		}
	}
	if found == nil {
		return Key{}, false
	}
	return clone(found), true
}

// Get 按 id 取 key。
func (s *Store) Get(id string) (Key, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if k := s.find(id); k != nil {
		return clone(k), true
	}
	return Key{}, false
}

// List 返回全部 key 的副本，按创建顺序。
func (s *Store) List() []Key {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Key, len(s.keys))
	for i, k := range s.keys {
		out[i] = clone(k)
	}
	return out
}

// Allow 记一次请求并做 RPM 判断。超限时不计入。
func (s *Store) Allow(k *Key) error {
	if k.RPM <= 0 {
		return nil
	}
	now := s.now()
	cut := now.Add(-time.Minute)
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.rpm[k.ID]
	i := 0
	for i < len(w) && !w[i].After(cut) {
		i++
	}
	w = w[i:]
	if len(w) >= k.RPM {
		s.rpm[k.ID] = w
		return &LimitError{What: "rpm", Limit: float64(k.RPM), Used: float64(len(w)), RetryAfter: w[0].Sub(cut)}
	}
	s.rpm[k.ID] = append(w, now)
	return nil
}

// Create 新建 key。Secret 为空时生成；ID / CreatedAt 总是新生成。
func (s *Store) Create(k Key) (Key, error) {
	if err := validate(&k); err != nil {
		return Key{}, err
	}
	if k.Secret == "" {
		k.Secret = NewSecret()
	}
	k.ID, k.CreatedAt = newID(), s.now().UTC()
	k.Hash, k.Hint = HashSecret(k.Secret), mask(k.Secret)
	s.mu.Lock()
	for _, o := range s.keys {
		if o.Hash == k.Hash {
			s.mu.Unlock()
			return Key{}, errors.New("secret already in use")
		}
	}
	p := clone(&k)
	p.Secret = "" // 库里只留哈希
	s.keys = append(s.keys, &p)
	s.mu.Unlock()
	return k, s.save() // 明文只在这次返回
}

// Update 用 fn 修改 id 对应的 key。ID / Secret / CreatedAt 不可改（用 Rotate 换 secret）。
func (s *Store) Update(id string, fn func(*Key)) (Key, error) {
	s.mu.Lock()
	k := s.find(id)
	if k == nil {
		s.mu.Unlock()
		return Key{}, ErrUnknown
	}
	next := clone(k)
	fn(&next)
	next.ID, next.Secret, next.Hash, next.Hint, next.CreatedAt = k.ID, "", k.Hash, k.Hint, k.CreatedAt
	if err := validate(&next); err != nil {
		s.mu.Unlock()
		return Key{}, err
	}
	*k = next
	out := clone(k)
	s.mu.Unlock()
	return out, s.save()
}

// Rotate 给 key 换一个新 secret。
func (s *Store) Rotate(id string) (Key, error) {
	s.mu.Lock()
	k := s.find(id)
	if k == nil {
		s.mu.Unlock()
		return Key{}, ErrUnknown
	}
	secret := NewSecret()
	k.Hash, k.Hint = HashSecret(secret), mask(secret)
	out := clone(k)
	out.Secret = secret // 明文只在这次返回
	s.mu.Unlock()
	return out, s.save()
}

// Delete 删除 key。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	i := slices.IndexFunc(s.keys, func(k *Key) bool { return k.ID == id })
	if i < 0 {
		s.mu.Unlock()
		return ErrUnknown
	}
	s.keys = slices.Delete(s.keys, i, i+1)
	delete(s.rpm, id)
	s.mu.Unlock()
	return s.save()
}

func (s *Store) find(id string) *Key {
	for _, k := range s.keys {
		if k.ID == id {
			return k
		}
	}
	return nil
}

func validate(k *Key) error {
	k.Name = strings.TrimSpace(k.Name)
	if k.Name == "" {
		return errors.New("name is required")
	}
	switch k.Period {
	case "", PeriodDay, PeriodWeek, PeriodMonth, PeriodTotal:
	default:
		return fmt.Errorf("period must be day, week, month or total, not %q", k.Period)
	}
	if k.LimitUSD < 0 || k.LimitCredits < 0 || k.LimitRequests < 0 || k.RPM < 0 {
		return errors.New("limits must not be negative")
	}
	k.Models = slices.DeleteFunc(k.Models, func(m string) bool { return strings.TrimSpace(m) == "" })
	return nil
}

func clone(k *Key) Key {
	c := *k
	c.Models = slices.Clone(k.Models)
	return c
}

func (s *Store) save() error {
	if s.path == "" {
		return nil
	}
	s.mu.RLock()
	out := make([]stored, len(s.keys))
	for i, k := range s.keys {
		c := clone(k)
		c.Secret = ""
		out[i] = stored{Key: c, Hash: k.Hash}
	}
	raw, err := json.MarshalIndent(struct {
		Keys []stored `json:"keys"`
	}{out}, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	return writeAtomic(s.path, raw)
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".keys-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// NewSecret 生成一个下游 key：sk-kiro- + 40 位十六进制。
func NewSecret() string {
	var b [20]byte
	_, _ = rand.Read(b[:])
	return "sk-kiro-" + hex.EncodeToString(b[:])
}

func newID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "key_" + hex.EncodeToString(b[:])
}
