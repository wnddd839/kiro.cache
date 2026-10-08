package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kiro-proxy/internal/kiro"
)

const testArn = "arn:aws:codewhisperer:us-east-1:111111111111:profile/TEST"

// fakeKiro 是本地替身：refresh、List-Available-Profiles、Get-Usage-Limits。
type fakeKiro struct {
	srv *httptest.Server

	refreshHits  atomic.Int64
	profileHits  atomic.Int64
	refreshCode  atomic.Int64 // 0 = 200
	refreshBody  string       // 非 200 时的响应体；空 = 默认 JSON
	onRefresh    func()       // 每次 refresh 请求时调用（测并发）
	refreshDelay time.Duration

	mu     sync.Mutex
	usage  map[string]string // access token -> Get-Usage-Limits JSON
	nextAT atomic.Int64
}

func newFakeKiro(t *testing.T) *fakeKiro {
	t.Helper()
	f := &fakeKiro{usage: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /refreshToken", func(w http.ResponseWriter, r *http.Request) {
		f.refreshHits.Add(1)
		if f.onRefresh != nil {
			f.onRefresh()
		}
		if f.refreshDelay > 0 {
			time.Sleep(f.refreshDelay)
		}
		if c := f.refreshCode.Load(); c != 0 {
			body := f.refreshBody
			if body == "" {
				body = `{"message":"Invalid refresh token"}`
			}
			if strings.HasPrefix(body, "<") {
				w.Header().Set("Content-Type", "text/html")
			}
			w.WriteHeader(int(c))
			_, _ = w.Write([]byte(body))
			return
		}
		n := f.nextAT.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"accessToken":  fmt.Sprintf("new-access-%d", n),
			"refreshToken": fmt.Sprintf("new-refresh-%d", n),
			"expiresIn":    3600,
		})
	})
	mux.HandleFunc("POST /List-Available-Profiles", func(w http.ResponseWriter, r *http.Request) {
		f.profileHits.Add(1)
		_, _ = w.Write([]byte(`{"profiles":[{"arn":"` + testArn + `"}]}`))
	})
	mux.HandleFunc("GET /Get-Usage-Limits", func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		f.mu.Lock()
		body, ok := f.usage[tok]
		f.mu.Unlock()
		if !ok {
			http.Error(w, `{"message":"unknown token"}`, http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKiro) client() *kiro.Client {
	c := kiro.NewClient(f.srv.Client())
	u := f.srv.URL
	c.RuntimeURL = func(string) string { return u }
	c.ManagementURL = func(string) string { return u }
	c.RefreshURL = func(string) string { return u + "/refreshToken" }
	c.OIDCURL = func(string) string { return u }
	c.AuthService = u
	return c
}

func (f *fakeKiro) setUsage(token string, used, limit float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usage[token] = fmt.Sprintf(`{"usageBreakdownList":[{"currentUsageWithPrecision":%g,"usageLimitWithPrecision":%g}]}`, used, limit)
}

func quiet() *slog.Logger { return slog.New(slog.DiscardHandler) }

func openPool(t *testing.T, c *kiro.Client) (*Pool, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pool.json")
	p, err := Open(path, Options{Client: c, Logger: quiet()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return p, path
}

func freshCred(token string) kiro.Cred {
	return kiro.Cred{
		Method:       kiro.MethodSocial,
		AccessToken:  token,
		RefreshToken: "refresh-" + token,
		ExpiresAt:    time.Now().Add(time.Hour),
		ProfileArn:   testArn,
	}
}

func mustAdd(t *testing.T, p *Pool, a Account) Account {
	t.Helper()
	got, err := p.Add(a)
	if err != nil {
		t.Fatalf("Add(%q): %v", a.ID, err)
	}
	return got
}

func mustAcquire(t *testing.T, p *Pool, key string, exclude ...string) *Lease {
	t.Helper()
	l, err := p.Acquire(key, exclude)
	if err != nil {
		t.Fatalf("Acquire(%q): %v", key, err)
	}
	return l
}

func readFile(t *testing.T, path string) file {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pool file: %v", err)
	}
	var f file
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse pool file: %v", err)
	}
	return f
}

func (p *Pool) slotFor(t *testing.T, id string) *slot {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	s := p.slots[id]
	if s == nil {
		t.Fatalf("no slot %q", id)
	}
	return s
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	p, path := openPool(t, nil)
	if p.Len() != 0 {
		t.Fatalf("Len = %d, want 0", p.Len())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open must not create file, stat err = %v", err)
	}
}

func TestOpenRejectsDuplicateIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pool.json")
	raw := `{"accounts":[{"id":"a","cred":{"method":"social"}},{"id":"a","cred":{"method":"social"}}]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{Logger: quiet()}); err == nil {
		t.Fatal("Open with duplicate ids: want error")
	}
}

func TestAddPersists(t *testing.T) {
	p, path := openPool(t, nil)
	a := mustAdd(t, p, Account{ID: "a1", Label: "one", Cred: kiro.Cred{AccessToken: "tok"}})
	if a.Cred.Method != kiro.MethodSocial {
		t.Errorf("default method = %q, want social", a.Cred.Method)
	}

	// 原始 JSON 结构：accounts[].cred
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var generic struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if len(generic.Accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(generic.Accounts))
	}
	var cred map[string]any
	if err := json.Unmarshal(generic.Accounts[0]["cred"], &cred); err != nil {
		t.Fatalf("accounts[0].cred: %v (raw %s)", err, raw)
	}
	if cred["access_token"] != "tok" {
		t.Errorf("cred.access_token = %v, want tok", cred["access_token"])
	}

	p2, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	v, ok := p2.Get("a1")
	if !ok || v.Label != "one" {
		t.Fatalf("re-Open Get = %+v, %v", v, ok)
	}
}

func TestAddGeneratesID(t *testing.T) {
	p, _ := openPool(t, nil)
	a := mustAdd(t, p, Account{Cred: kiro.Cred{RefreshToken: "r"}})
	if !strings.HasPrefix(a.ID, "acc_") {
		t.Errorf("generated id = %q", a.ID)
	}
}

func TestAddRejectsDuplicateAndTokenless(t *testing.T) {
	p, path := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: kiro.Cred{AccessToken: "x"}})
	if _, err := p.Add(Account{ID: "a", Cred: kiro.Cred{AccessToken: "y"}}); err == nil {
		t.Error("duplicate id: want error")
	}
	if _, err := p.Add(Account{ID: "b"}); err == nil {
		t.Error("no tokens: want error")
	}
	if p.Len() != 1 {
		t.Errorf("Len = %d, want 1", p.Len())
	}
	if got := readFile(t, path); len(got.Accounts) != 1 || got.Accounts[0].Cred.AccessToken != "x" {
		t.Errorf("file = %+v", got)
	}
}

func TestRemovePersists(t *testing.T) {
	p, path := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: kiro.Cred{AccessToken: "x"}})
	mustAdd(t, p, Account{ID: "b", Cred: kiro.Cred{AccessToken: "y"}})
	if err := p.Remove("a"); err != nil {
		t.Fatal(err)
	}
	if err := p.Remove("a"); err == nil {
		t.Error("second Remove: want error")
	}
	f := readFile(t, path)
	if len(f.Accounts) != 1 || f.Accounts[0].ID != "b" {
		t.Fatalf("file accounts = %+v", f.Accounts)
	}
	p2, err := Open(path, Options{Logger: quiet()})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p2.Get("a"); ok {
		t.Error("removed account still present after re-Open")
	}
}

func TestAcquireSticky(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	mustAdd(t, p, Account{ID: "b", Cred: freshCred("b")})

	// a 更忙，但会话钉在 a 上
	busy := mustAcquire(t, p, "", "b")
	defer busy.Release()
	p.Pin("sess", "a")

	l := mustAcquire(t, p, "sess")
	defer l.Release()
	if l.ID != "a" || !l.Pinned {
		t.Fatalf("lease = %s pinned=%v, want a pinned", l.ID, l.Pinned)
	}

	p.Unpin("sess")
	l2 := mustAcquire(t, p, "sess")
	defer l2.Release()
	if l2.ID != "b" || l2.Pinned {
		t.Fatalf("after Unpin lease = %s pinned=%v, want b unpinned", l2.ID, l2.Pinned)
	}
}

func TestAcquirePinnedUnavailableFallsBack(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, p *Pool)
		exclude []string
	}{
		{name: "excluded", setup: func(*testing.T, *Pool) {}, exclude: []string{"a"}},
		{name: "disabled", setup: func(t *testing.T, p *Pool) {
			if err := p.SetDisabled("a", true, "off"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "cooling", setup: func(t *testing.T, p *Pool) {
			p.Fail("a", kiro.Failure{Status: 429, Class: kiro.ClassThrottle})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := openPool(t, nil)
			mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
			mustAdd(t, p, Account{ID: "b", Cred: freshCred("b")})
			p.Pin("sess", "a")
			tc.setup(t, p)
			l := mustAcquire(t, p, "sess", tc.exclude...)
			defer l.Release()
			if l.ID != "b" || l.Pinned {
				t.Fatalf("lease = %s pinned=%v, want b unpinned", l.ID, l.Pinned)
			}
		})
	}
}

func TestAcquireLeastInflight(t *testing.T) {
	p, _ := openPool(t, nil)
	for _, id := range []string{"a", "b", "c"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
	}
	var leases []*Lease
	for range 6 {
		leases = append(leases, mustAcquire(t, p, ""))
	}
	for _, v := range p.List() {
		if v.InFlight != 2 {
			t.Errorf("%s in flight = %d, want 2 (even spread)", v.ID, v.InFlight)
		}
	}
	leases[0].Release()
	l := mustAcquire(t, p, "")
	if l.ID != leases[0].ID {
		t.Errorf("after release picked %s, want least loaded %s", l.ID, leases[0].ID)
	}
}

func TestAcquireTieBreakByRemainingCredits(t *testing.T) {
	fk := newFakeKiro(t)
	p, _ := openPool(t, fk.client())
	mustAdd(t, p, Account{ID: "low", Cred: freshCred("tok-low")})
	mustAdd(t, p, Account{ID: "high", Cred: freshCred("tok-high")})
	fk.setUsage("tok-low", 90, 100)
	fk.setUsage("tok-high", 10, 100)
	for _, id := range []string{"low", "high"} {
		if _, err := p.RefreshLimits(t.Context(), id); err != nil {
			t.Fatalf("RefreshLimits(%s): %v", id, err)
		}
	}
	if v, _ := p.Get("high"); v.Limits.Remaining() < 0.89 {
		t.Fatalf("high remaining = %v", v.Limits.Remaining())
	}
	l := mustAcquire(t, p, "")
	defer l.Release()
	if l.ID != "high" {
		t.Fatalf("picked %s, want high (more credits)", l.ID)
	}
	// 并发数优先于额度
	l2 := mustAcquire(t, p, "")
	defer l2.Release()
	if l2.ID != "low" {
		t.Fatalf("second pick %s, want low (fewer in flight)", l2.ID)
	}
}

// 同一额度档（每 20% 一档）内随机：新号不会把新会话全吸走。
func TestAcquireSpreadsWithinQuotaTier(t *testing.T) {
	fk := newFakeKiro(t)
	p, _ := openPool(t, fk.client())
	for _, id := range []string{"old", "new", "low"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred("tok-" + id)})
	}
	fk.setUsage("tok-old", 15, 100) // 剩 85%
	fk.setUsage("tok-new", 0, 100)  // 剩 100%（新号）
	fk.setUsage("tok-low", 50, 100) // 剩 50%：低一档
	for _, id := range []string{"old", "new", "low"} {
		if _, err := p.RefreshLimits(t.Context(), id); err != nil {
			t.Fatal(err)
		}
	}
	count := map[string]int{}
	for range 400 {
		l := mustAcquire(t, p, "")
		count[l.ID]++
		l.Release()
	}
	if count["low"] != 0 || count["old"] < 120 || count["new"] < 120 {
		t.Fatalf("picks %v: want old/new (same 80%%+ tier) shared, low never", count)
	}
}

func TestRefreshLimitsExhaustedCoolsAndRelabels(t *testing.T) {
	fk := newFakeKiro(t)
	p, path := openPool(t, fk.client())
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("tok")})
	fk.mu.Lock()
	fk.usage["tok"] = `{"userInfo":{"email":"me@example.com"},"usageBreakdownList":[{"currentUsageWithPrecision":50,"usageLimitWithPrecision":50}]}`
	fk.mu.Unlock()
	if _, err := p.RefreshLimits(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	v, _ := p.Get("a")
	if v.Label != "me@example.com" {
		t.Errorf("label = %q", v.Label)
	}
	if v.CooldownUntil.Before(time.Now().Add(59 * time.Minute)) {
		t.Errorf("cooldown = %v, want ~1h", v.CooldownUntil)
	}
	if f := readFile(t, path); f.Accounts[0].Label != "me@example.com" {
		t.Errorf("label not persisted: %+v", f.Accounts[0])
	}
}

func TestMaxConcurrent(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", MaxConcurrent: 2, Cred: freshCred("a")})
	l1 := mustAcquire(t, p, "")
	l2 := mustAcquire(t, p, "")
	_, err := p.Acquire("", nil)
	ue, ok := errors.AsType[*UnavailableError](err)
	if !ok || !errors.Is(err, ErrNoAccount) {
		t.Fatalf("third Acquire err = %v, want UnavailableError", err)
	}
	if !ue.RetryAt.IsZero() {
		t.Errorf("busy (not cooling) RetryAt = %v, want zero", ue.RetryAt)
	}
	l1.Release()
	l3 := mustAcquire(t, p, "")
	l2.Release()
	l3.Release()
}

// 号上没设 max_concurrent 时用号池默认上限；号上设了的优先。
func TestMaxConcurrentPoolDefault(t *testing.T) {
	p, err := Open(filepath.Join(t.TempDir(), "pool.json"), Options{Logger: quiet(), MaxConcurrent: 3, PinWait: -1})
	if err != nil {
		t.Fatal(err)
	}
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	for range 3 {
		mustAcquire(t, p, "")
	}
	if _, err := p.Acquire("", nil); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("4th Acquire err = %v, want busy", err)
	}
	if err := p.SetMaxConcurrent("a", 5); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		mustAcquire(t, p, "")
	}
	if _, err := p.Acquire("", nil); !errors.Is(err, ErrNoAccount) {
		t.Fatalf("6th Acquire err = %v, want busy", err)
	}
}

func TestLeaseReleaseIdempotent(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	l1 := mustAcquire(t, p, "")
	l2 := mustAcquire(t, p, "")
	for range 3 {
		l1.Release()
	}
	if v, _ := p.Get("a"); v.InFlight != 1 {
		t.Fatalf("in flight = %d, want 1", v.InFlight)
	}
	l2.Release()
	if v, _ := p.Get("a"); v.InFlight != 0 {
		t.Fatalf("in flight = %d, want 0", v.InFlight)
	}
}

func TestLeaseReleaseConcurrent(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			l, err := p.Acquire("", nil)
			if err != nil {
				t.Error(err)
				return
			}
			l.Release()
			l.Release()
		})
	}
	wg.Wait()
	if v, _ := p.Get("a"); v.InFlight != 0 {
		t.Fatalf("in flight = %d, want 0", v.InFlight)
	}
}

func TestFailQuota(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	before := time.Now()
	if !p.Fail("a", kiro.Failure{Status: 429, Message: "USAGE_LIMIT", Class: kiro.ClassQuota}) {
		t.Error("quota: want retry=true")
	}
	v, _ := p.Get("a")
	if v.CooldownUntil.Before(before.Add(time.Hour - time.Second)) {
		t.Errorf("cooldown until %v, want >= ~1h", v.CooldownUntil)
	}
	if !strings.Contains(v.CooldownWhy, "usage limit") {
		t.Errorf("cooldown reason = %q", v.CooldownWhy)
	}
	if v.Stats.Errors != 1 {
		t.Errorf("errors = %d", v.Stats.Errors)
	}
	_, err := p.Acquire("", nil)
	ue, ok := errors.AsType[*UnavailableError](err)
	if !ok {
		t.Fatalf("Acquire err = %v, want *UnavailableError", err)
	}
	if !errors.Is(err, ErrNoAccount) {
		t.Error("errors.Is(err, ErrNoAccount) = false")
	}
	if !ue.RetryAt.Equal(v.CooldownUntil) {
		t.Errorf("RetryAt = %v, want %v", ue.RetryAt, v.CooldownUntil)
	}
}

func TestFailQuotaUsesResetAt(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	reset := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	p.slotFor(t, "a").limits = kiro.Limits{Used: 1, Limit: 10, ResetAt: reset}
	p.Fail("a", kiro.Failure{Class: kiro.ClassQuota})
	if v, _ := p.Get("a"); !v.CooldownUntil.Equal(reset) {
		t.Errorf("cooldown = %v, want reset %v", v.CooldownUntil, reset)
	}
}

// 429 "Rate exceeded" 是限流：只短冷却（有 Retry-After 按它，上限 5 分钟；没有就退避）。
// 402 与带 MONTHLY_REQUEST_COUNT 的返回才冷却到 ResetAt。
func TestRateExceededIsNotQuota(t *testing.T) {
	reset := time.Now().Add(20 * 24 * time.Hour).Truncate(time.Second)
	fresh := func(t *testing.T) (*Pool, time.Time) {
		p, _ := openPool(t, nil)
		mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
		clock := time.Now()
		p.now = func() time.Time { return clock }
		p.slotFor(t, "a").limits = kiro.Limits{Used: 1, Limit: 10, ResetAt: reset}
		return p, clock
	}
	fail := func(p *Pool, status int, body string, h http.Header) {
		f := kiro.Classify(status, []byte(body))
		f.RetryAfter = kiro.RetryAfter(h)
		p.Fail("a", f)
	}
	for _, tc := range []struct {
		name string
		body string
		h    http.Header
		want time.Duration
	}{
		{"no retry-after", `{"message":"Rate exceeded"}`, nil, 15 * time.Second},
		{"retry-after", `{"__type":"ThrottlingException","message":"Rate exceeded"}`, http.Header{"Retry-After": {"7"}}, 7 * time.Second},
		{"retry-after capped", `Too many requests`, http.Header{"Retry-After": {"86400"}}, 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, clock := fresh(t)
			fail(p, 429, tc.body, tc.h)
			if d := p.slotFor(t, "a").cooldown.Sub(clock); d != tc.want {
				t.Fatalf("cooldown = %v, want %v (reason %q)", d, tc.want, p.slotFor(t, "a").cooldownWhy)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"402", 402, `{"message":"Payment required"}`},
		{"429 monthly", 429, `{"message":"You have reached the limit.","reason":"MONTHLY_REQUEST_COUNT"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := fresh(t)
			fail(p, tc.status, tc.body, nil)
			if got := p.slotFor(t, "a").cooldown; !got.Equal(reset) {
				t.Fatalf("cooldown = %v, want ResetAt %v", got, reset)
			}
		})
	}
}

func TestFailThrottleBackoffGrows(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	clock := time.Now()
	p.now = func() time.Time { return clock }
	var prev time.Duration
	want := []time.Duration{15 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 5 * time.Minute, 5 * time.Minute}
	for i, w := range want {
		if !p.Fail("a", kiro.Failure{Status: 429, Class: kiro.ClassThrottle}) {
			t.Fatal("throttle: want retry=true")
		}
		d := p.slotFor(t, "a").cooldown.Sub(clock)
		if d != w {
			t.Errorf("strike %d backoff = %v, want %v", i+1, d, w)
		}
		if d < prev {
			t.Errorf("backoff shrank: %v < %v", d, prev)
		}
		prev = d
		clock = clock.Add(d) // 冷却结束后再次被限流
	}
	// Success 清零 strikes
	p.Success("a", kiro.Usage{}, 0, false)
	p.Fail("a", kiro.Failure{Class: kiro.ClassThrottle})
	if d := p.slotFor(t, "a").cooldown.Sub(clock); d != 15*time.Second {
		t.Errorf("after Success backoff = %v, want 15s", d)
	}
}

func TestFailFatalNoCooldown(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	if p.Fail("a", kiro.Failure{Status: 400, Class: kiro.ClassFatal}) {
		t.Error("fatal: want retry=false")
	}
	if v, _ := p.Get("a"); !v.CooldownUntil.IsZero() {
		t.Errorf("fatal cooldown = %v, want none", v.CooldownUntil)
	}
	mustAcquire(t, p, "").Release()
	if p.Fail("missing", kiro.Failure{Class: kiro.ClassFatal}) {
		t.Error("unknown account fatal: want retry=false")
	}
	if !p.Fail("missing", kiro.Failure{Class: kiro.ClassTransient}) {
		t.Error("unknown account transient: want retry=true")
	}
}

func TestSetDisabledFalseClearsCooldown(t *testing.T) {
	p, path := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	p.Fail("a", kiro.Failure{Class: kiro.ClassQuota})
	if err := p.SetDisabled("a", true, "manual"); err != nil {
		t.Fatal(err)
	}
	if f := readFile(t, path); !f.Accounts[0].Disabled || f.Accounts[0].Note != "manual" {
		t.Errorf("disabled not persisted: %+v", f.Accounts[0])
	}
	if err := p.SetDisabled("a", false, ""); err != nil {
		t.Fatal(err)
	}
	v, _ := p.Get("a")
	if v.Disabled || v.Note != "" || !v.CooldownUntil.IsZero() {
		t.Fatalf("after enable view = %+v", v)
	}
	if s := p.slotFor(t, "a"); s.strikes != 0 || !s.cooldown.IsZero() {
		t.Errorf("slot not reset: strikes=%d cooldown=%v", s.strikes, s.cooldown)
	}
	mustAcquire(t, p, "").Release()
	if err := p.SetDisabled("nope", true, ""); err == nil {
		t.Error("SetDisabled unknown: want error")
	}
}

func TestAcquireEmptyPool(t *testing.T) {
	p, _ := openPool(t, nil)
	_, err := p.Acquire("k", nil)
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("err = %v, want ErrNoAccount", err)
	}
	ue, ok := errors.AsType[*UnavailableError](err)
	if !ok {
		t.Fatalf("err %T, want *UnavailableError", err)
	}
	if !ue.RetryAt.IsZero() || !strings.Contains(ue.Error(), "empty") {
		t.Errorf("err = %+v", ue)
	}
}

func TestUnavailablePicksSoonestCooldown(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	mustAdd(t, p, Account{ID: "b", Cred: freshCred("b")})
	mustAdd(t, p, Account{ID: "c", Disabled: true, Cred: freshCred("c")})
	p.Fail("a", kiro.Failure{Class: kiro.ClassQuota})
	p.Fail("b", kiro.Failure{Class: kiro.ClassThrottle, Message: "slow down"})
	_, err := p.Acquire("", nil)
	ue, ok := errors.AsType[*UnavailableError](err)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	vb, _ := p.Get("b")
	if !ue.RetryAt.Equal(vb.CooldownUntil) || !strings.Contains(ue.Reason, "slow down") {
		t.Errorf("err = %+v, want b's cooldown %v", ue, vb.CooldownUntil)
	}
	// 排除 b 后只剩 a 的冷却
	_, err = p.Acquire("", []string{"b"})
	ue, _ = errors.AsType[*UnavailableError](err)
	va, _ := p.Get("a")
	if ue == nil || !ue.RetryAt.Equal(va.CooldownUntil) {
		t.Errorf("exclude b err = %+v, want a's cooldown", ue)
	}
}

func TestCredSingleFlightRefresh(t *testing.T) {
	fk := newFakeKiro(t)
	fk.refreshDelay = 20 * time.Millisecond
	p, path := openPool(t, fk.client())
	c := freshCred("old")
	c.ExpiresAt = time.Now().Add(-time.Minute)
	mustAdd(t, p, Account{ID: "a", Cred: c})

	var wg sync.WaitGroup
	tokens := make([]string, 16)
	for i := range 16 {
		wg.Go(func() {
			got, err := p.Cred(t.Context(), "a", "")
			if err != nil {
				t.Error(err)
				return
			}
			tokens[i] = got.AccessToken
		})
	}
	wg.Wait()
	if n := fk.refreshHits.Load(); n != 1 {
		t.Fatalf("refresh hits = %d, want 1", n)
	}
	for i, tok := range tokens {
		if tok != "new-access-1" {
			t.Errorf("caller %d got %q", i, tok)
		}
	}
	f := readFile(t, path)
	if got := f.Accounts[0].Cred; got.AccessToken != "new-access-1" || got.RefreshToken != "new-refresh-1" || !got.ExpiresAt.After(time.Now()) {
		t.Errorf("persisted cred = %+v", got)
	}
}

func TestCredFreshNoRefresh(t *testing.T) {
	fk := newFakeKiro(t)
	p, _ := openPool(t, fk.client())
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("cur")})
	got, err := p.Cred(t.Context(), "a", "")
	if err != nil || got.AccessToken != "cur" {
		t.Fatalf("Cred = %q, %v", got.AccessToken, err)
	}
	if n := fk.refreshHits.Load() + fk.profileHits.Load(); n != 0 {
		t.Errorf("upstream hits = %d, want 0", n)
	}
	if _, err := p.Cred(t.Context(), "missing", ""); err == nil {
		t.Error("unknown account: want error")
	}
}

func TestCredStaleForcesRefreshOnce(t *testing.T) {
	fk := newFakeKiro(t)
	p, path := openPool(t, fk.client())
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("cur")})

	got, err := p.Cred(t.Context(), "a", "cur")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-access-1" || fk.refreshHits.Load() != 1 {
		t.Fatalf("stale==current: token %q hits %d", got.AccessToken, fk.refreshHits.Load())
	}
	if f := readFile(t, path); f.Accounts[0].Cred.AccessToken != "new-access-1" {
		t.Errorf("not persisted: %+v", f.Accounts[0].Cred)
	}

	// 另一个请求带着旧 token 回来：已经刷新过，不再花 refresh token
	got, err = p.Cred(t.Context(), "a", "cur")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-access-1" || fk.refreshHits.Load() != 1 {
		t.Fatalf("stale!=current: token %q hits %d", got.AccessToken, fk.refreshHits.Load())
	}
}

func TestCredConcurrentStaleRefreshesOnce(t *testing.T) {
	fk := newFakeKiro(t)
	fk.refreshDelay = 10 * time.Millisecond
	p, _ := openPool(t, fk.client())
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("cur")})
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			if _, err := p.Cred(t.Context(), "a", "cur"); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if n := fk.refreshHits.Load(); n != 1 {
		t.Fatalf("refresh hits = %d, want 1", n)
	}
}

func TestForceRefresh(t *testing.T) {
	fk := newFakeKiro(t)
	p, _ := openPool(t, fk.client())
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("cur")})
	got, err := p.ForceRefresh(t.Context(), "a")
	if err != nil || got.AccessToken != "new-access-1" {
		t.Fatalf("ForceRefresh = %q, %v", got.AccessToken, err)
	}
}

// 只给 refresh token 的号：首次 Cred 必须先换 access token，不能返回空 token。
func TestCredRefreshOnlyAccount(t *testing.T) {
	fk := newFakeKiro(t)
	p, _ := openPool(t, fk.client())
	c := freshCred("")
	c.ExpiresAt = time.Time{}
	mustAdd(t, p, Account{ID: "a", Cred: c})
	got, err := p.Cred(t.Context(), "a", "")
	if err != nil || got.AccessToken != "new-access-1" {
		t.Fatalf("Cred = %q, %v", got.AccessToken, err)
	}
	if got, err = p.ForceRefresh(t.Context(), "a"); err != nil || got.AccessToken != "new-access-2" {
		t.Fatalf("ForceRefresh = %q, %v", got.AccessToken, err)
	}
}

func TestCredRefreshRejectedDisables(t *testing.T) {
	fk := newFakeKiro(t)
	fk.refreshCode.Store(http.StatusBadRequest)
	fk.refreshBody = `{"error":"invalid_grant","error_description":"Invalid refresh token provided"}`
	p, path := openPool(t, fk.client())
	c := freshCred("old")
	c.ExpiresAt = time.Now().Add(-time.Minute)
	mustAdd(t, p, Account{ID: "a", Cred: c})

	_, err := p.Cred(t.Context(), "a", "")
	if err == nil || !kiro.IsGone(err) {
		t.Fatalf("err = %v, want gone", err)
	}
	v, _ := p.Get("a")
	if !v.Disabled || !strings.Contains(v.Note, "sign-in expired") {
		t.Fatalf("view = disabled %v note %q", v.Disabled, v.Note)
	}
	if f := readFile(t, path); !f.Accounts[0].Disabled || f.Accounts[0].Note == "" {
		t.Errorf("disable not persisted: %+v", f.Accounts[0])
	}
	if _, err := p.Acquire("", nil); !errors.Is(err, ErrNoAccount) {
		t.Errorf("Acquire after disable err = %v", err)
	}
}

func TestCredRefreshServerErrorKeepsEnabled(t *testing.T) {
	fk := newFakeKiro(t)
	fk.refreshCode.Store(http.StatusInternalServerError)
	p, _ := openPool(t, fk.client())
	c := freshCred("old")
	c.ExpiresAt = time.Now().Add(-time.Minute)
	mustAdd(t, p, Account{ID: "a", Cred: c})
	if _, err := p.Cred(t.Context(), "a", ""); err == nil {
		t.Fatal("want error")
	}
	if v, _ := p.Get("a"); v.Disabled {
		t.Error("5xx refresh must not disable the account")
	}
}

func TestCredMissingProfileFetchedAndPersisted(t *testing.T) {
	fk := newFakeKiro(t)
	p, path := openPool(t, fk.client())
	c := freshCred("cur")
	c.ProfileArn = ""
	mustAdd(t, p, Account{ID: "a", Cred: c})

	got, err := p.Cred(t.Context(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfileArn != testArn || fk.profileHits.Load() != 1 {
		t.Fatalf("profile %q hits %d", got.ProfileArn, fk.profileHits.Load())
	}
	if fk.refreshHits.Load() != 0 {
		t.Errorf("fresh token refreshed")
	}
	if f := readFile(t, path); f.Accounts[0].Cred.ProfileArn != testArn {
		t.Errorf("profile not persisted: %+v", f.Accounts[0].Cred)
	}
	if _, err := p.Cred(t.Context(), "a", ""); err != nil {
		t.Fatal(err)
	}
	if fk.profileHits.Load() != 1 {
		t.Errorf("profile fetched again: hits %d", fk.profileHits.Load())
	}
}

func writeIDE(t *testing.T, path, access, refresh string, expires time.Time) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"accessToken":  access,
		"refreshToken": refresh,
		"expiresAt":    expires.UTC().Format(time.RFC3339),
		"region":       "us-east-1",
		"authMethod":   "social",
		"provider":     "Google",
		"extra":        "keep-me",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCredIDEPrefersNewerFile(t *testing.T) {
	fk := newFakeKiro(t)
	p, path := openPool(t, fk.client())
	ide := filepath.Join(t.TempDir(), "kiro-auth-token.json")
	writeIDE(t, ide, "ide-access", "ide-refresh", time.Now().Add(2*time.Hour))

	stored := freshCred("stored")
	stored.ExpiresAt = time.Now().Add(30 * time.Minute)
	mustAdd(t, p, Account{ID: "a", Source: SourceIDE, SourcePath: ide, Cred: stored})

	got, err := p.Cred(t.Context(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "ide-access" || got.RefreshToken != "ide-refresh" {
		t.Fatalf("cred = %+v, want IDE file token", got)
	}
	if got.ProfileArn != testArn {
		t.Errorf("profile arn lost: %q", got.ProfileArn)
	}
	if fk.refreshHits.Load()+fk.profileHits.Load() != 0 {
		t.Errorf("unexpected upstream calls")
	}
	if f := readFile(t, path); f.Accounts[0].Cred.AccessToken != "ide-access" {
		t.Errorf("IDE token not persisted to pool: %+v", f.Accounts[0].Cred)
	}
}

func TestCredIDEOlderFileIgnored(t *testing.T) {
	fk := newFakeKiro(t)
	p, _ := openPool(t, fk.client())
	ide := filepath.Join(t.TempDir(), "kiro-auth-token.json")
	writeIDE(t, ide, "ide-access", "ide-refresh", time.Now().Add(10*time.Minute))
	mustAdd(t, p, Account{ID: "a", Source: SourceIDE, SourcePath: ide, Cred: freshCred("stored")})
	got, err := p.Cred(t.Context(), "a", "")
	if err != nil || got.AccessToken != "stored" {
		t.Fatalf("Cred = %q, %v; want stored", got.AccessToken, err)
	}
}

func TestCredIDEWriteBackAfterRefresh(t *testing.T) {
	fk := newFakeKiro(t)
	p, path := openPool(t, fk.client())
	ide := filepath.Join(t.TempDir(), "kiro-auth-token.json")
	writeIDE(t, ide, "ide-old", "ide-refresh", time.Now().Add(-time.Hour))

	stored := freshCred("stored-old")
	stored.ExpiresAt = time.Now().Add(-2 * time.Hour)
	mustAdd(t, p, Account{ID: "a", Source: SourceIDE, SourcePath: ide, Cred: stored})

	got, err := p.Cred(t.Context(), "a", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "new-access-1" || fk.refreshHits.Load() != 1 {
		t.Fatalf("token %q hits %d", got.AccessToken, fk.refreshHits.Load())
	}

	raw, err := os.ReadFile(ide)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["accessToken"] != "new-access-1" || m["refreshToken"] != "new-refresh-1" || m["extra"] != "keep-me" {
		t.Errorf("IDE file = %v", m)
	}
	if ts, err := time.Parse(time.RFC3339, fmt.Sprint(m["expiresAt"])); err != nil || !ts.After(time.Now()) {
		t.Errorf("IDE expiresAt = %v (%v)", m["expiresAt"], err)
	}
	if f := readFile(t, path); f.Accounts[0].Cred.AccessToken != "new-access-1" {
		t.Errorf("pool file not updated: %+v", f.Accounts[0].Cred)
	}
}

func TestStatsHitRate(t *testing.T) {
	cases := []struct {
		s    Stats
		want float64
	}{
		{Stats{}, 0},
		{Stats{InputTokens: 100}, 0},
		{Stats{InputTokens: 25, CacheRead: 50, CacheWrite: 25}, 0.5},
		{Stats{CacheRead: 10}, 1},
	}
	for _, tc := range cases {
		if got := tc.s.HitRate(); got != tc.want {
			t.Errorf("%+v HitRate = %v, want %v", tc.s, got, tc.want)
		}
	}
}

func TestSuccessStatsAndTotals(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	mustAdd(t, p, Account{ID: "b", Cred: freshCred("b")})
	p.Success("a", kiro.Usage{Input: 10, Output: 5, CacheRead: 30, CacheWrite: 0}, 0, false)
	p.Success("a", kiro.Usage{Input: 10, Output: 5, CacheRead: 0, CacheWrite: 10}, 0, false)
	p.Success("b", kiro.Usage{Input: 1, Output: 2, CacheRead: 3, CacheWrite: 4}, 0, false)
	p.Fail("b", kiro.Failure{Class: kiro.ClassFatal})
	p.Success("missing", kiro.Usage{Input: 1000}, 0, false)

	va, _ := p.Get("a")
	if va.Stats.Requests != 2 || va.Stats.InputTokens != 20 || va.HitRate != 0.5 {
		t.Errorf("a stats = %+v hit %v", va.Stats, va.HitRate)
	}
	want := Stats{Requests: 3, Errors: 1, InputTokens: 21, OutputTokens: 12, CacheRead: 33, CacheWrite: 14}
	if got := p.Totals(); got != want {
		t.Errorf("Totals = %+v, want %+v", got, want)
	}
}

func TestListOrderAndRedaction(t *testing.T) {
	p, _ := openPool(t, nil)
	for _, id := range []string{"z", "a", "m"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred("secret-" + id)})
	}
	vs := p.List()
	var ids []string
	for _, v := range vs {
		ids = append(ids, v.ID)
	}
	if strings.Join(ids, ",") != "z,a,m" {
		t.Errorf("order = %v", ids)
	}
	raw, _ := json.Marshal(vs)
	if strings.Contains(string(raw), "secret-") {
		t.Errorf("View leaks token: %s", raw)
	}
}

// 钉的号只是并发打满：先短暂排队等它；等到了就还用它（命中粘滞），不换号。
func TestAcquirePinnedBusyWaits(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", MaxConcurrent: 1, Cred: freshCred("a")})
	mustAdd(t, p, Account{ID: "b", Cred: freshCred("b")})
	p.Pin("sess", "a")
	busy := mustAcquire(t, p, "sess")
	if busy.ID != "a" {
		t.Fatalf("first lease %s", busy.ID)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		busy.Release()
	}()
	l := mustAcquire(t, p, "sess")
	defer l.Release()
	if l.ID != "a" || !l.Pinned {
		t.Fatalf("lease = %+v, want a pinned after waiting", l)
	}
}

// 等不到时不借号：取消返回 ctx 错误，超时返回可重试的忙状态。
func TestAcquirePinnedBusyDoesNotSwitch(t *testing.T) {
	p, _ := openPool(t, nil)
	mustAdd(t, p, Account{ID: "a", MaxConcurrent: 1, Cred: freshCred("a")})
	mustAdd(t, p, Account{ID: "b", Cred: freshCred("b")})
	p.Pin("sess", "a")
	busy := mustAcquire(t, p, "sess")
	defer busy.Release()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	l, err := p.AcquireContext(ctx, "sess", nil)
	if err == nil || l != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled wait = %+v %v", l, err)
	}
	p.pinWait = time.Millisecond
	l, err = p.AcquireContext(t.Context(), "sess", nil)
	if _, ok := errors.AsType[*UnavailableError](err); !ok || l != nil {
		t.Fatalf("busy account should remain pinned: %+v %v", l, err)
	}
	if id, _ := p.pins.Lookup("sess"); id != "a" {
		t.Fatalf("pin moved to %s", id)
	}
}

// 未归属 credits：额度增量 − 账本记的；重置后从新基线累计。
func TestRecon(t *testing.T) {
	s := &slot{}
	t0 := time.Unix(1_700_000_000, 0)
	s.reconLocked(kiro.Limits{Used: 10}, t0)
	s.metered += 1.5 // 账本记了 1.5
	s.reconLocked(kiro.Limits{Used: 12, Overage: 0.5}, t0.Add(time.Minute))
	if r := s.recon; r.Upstream != 2.5 || r.Recorded != 1.5 || r.Reported != 1.5 || r.Estimated != 0 || r.Unassigned != 1 {
		t.Fatalf("recon = %+v", r)
	}
	s.estimated += 0.4 // 中断的尝试：按 token 估的
	s.reconLocked(kiro.Limits{Used: 12.4, Overage: 0.5}, t0.Add(90*time.Second))
	if r := s.recon; math.Abs(r.Estimated-0.4) > 1e-9 || math.Abs(r.Recorded-1.9) > 1e-9 || math.Abs(r.Unassigned-1) > 1e-9 {
		t.Fatalf("recon with estimate = %+v", r)
	}
	s.reconLocked(kiro.Limits{Used: 0.2}, t0.Add(2*time.Minute)) // 重置
	s.metered += 0.3
	s.reconLocked(kiro.Limits{Used: 0.5}, t0.Add(3*time.Minute))
	if r := s.recon; r.Resets != 1 || math.Abs(r.Unassigned-1) > 1e-9 || math.Abs(r.Upstream-3.2) > 1e-9 {
		t.Fatalf("after reset = %+v", r)
	}
}

// 刷新返回 403 HTML 防火墙页：不停号，只计一次失败并冷却。
func TestRefreshFirewall403DoesNotDisable(t *testing.T) {
	fk := newFakeKiro(t)
	fk.refreshCode.Store(http.StatusForbidden)
	fk.refreshBody = `<html><body><h1>403 Forbidden</h1>Request blocked by firewall</body></html>`
	p, _ := openPool(t, fk.client())
	c := freshCred("old")
	c.ExpiresAt = time.Now().Add(-time.Minute)
	mustAdd(t, p, Account{ID: "a", Cred: c})
	if _, err := p.Cred(t.Context(), "a", ""); err == nil {
		t.Fatal("want error")
	}
	v, _ := p.Get("a")
	if v.Disabled || v.RefreshFails != 1 || v.CooldownUntil.IsZero() {
		t.Fatalf("view = disabled %v fails %d cooldown %v", v.Disabled, v.RefreshFails, v.CooldownUntil)
	}
}

// 400 invalid_grant：立即停号（见 TestCredRefreshRejectedDisables）；连续 3 次 401 才停号，成功一次清零。
func TestRefresh401ThreeStrikes(t *testing.T) {
	fk := newFakeKiro(t)
	p, _ := openPool(t, fk.client())
	c := freshCred("old")
	c.ExpiresAt = time.Now().Add(-time.Minute)
	mustAdd(t, p, Account{ID: "a", Cred: c})

	fk.refreshCode.Store(http.StatusUnauthorized)
	for i := 1; i <= 2; i++ {
		_, _ = p.Cred(t.Context(), "a", "")
		if v, _ := p.Get("a"); v.Disabled || v.RefreshFails != i {
			t.Fatalf("after %d: disabled %v fails %d", i, v.Disabled, v.RefreshFails)
		}
	}
	// 成功一次清零
	fk.refreshCode.Store(0)
	if _, err := p.Cred(t.Context(), "a", ""); err != nil {
		t.Fatal(err)
	}
	if v, _ := p.Get("a"); v.RefreshFails != 0 {
		t.Fatalf("success did not reset: %d", v.RefreshFails)
	}
	fk.refreshCode.Store(http.StatusUnauthorized)
	for i := 1; i <= 3; i++ {
		if _, err := p.ForceRefresh(t.Context(), "a"); err == nil {
			t.Fatal("want error")
		}
		v, _ := p.Get("a")
		if want := i == 3; v.Disabled != want {
			t.Fatalf("after %d consecutive 401: disabled %v (note %q)", i, v.Disabled, v.Note)
		}
	}
}

// 全局熔断：5 个号同时刷新拿到 403 HTML（防火墙 / 上游异常），反复多轮也一个都不停。
func TestBreakerRefresh403StormDisablesNone(t *testing.T) {
	fk := newFakeKiro(t)
	fk.refreshCode.Store(http.StatusForbidden)
	fk.refreshBody = `<html><body><h1>403 Forbidden</h1>Request blocked</body></html>`
	p, path := openPool(t, fk.client())
	clock := time.Now()
	p.now = func() time.Time { return clock }
	ids := []string{"a", "b", "c", "d", "e"}
	for _, id := range ids {
		c := freshCred(id)
		c.ExpiresAt = time.Now().Add(-time.Minute)
		mustAdd(t, p, Account{ID: id, Cred: c})
	}
	// 持续约 3 分钟：每轮间隔一个刷新冷却，每轮 5 个号都刷新失败
	for round := range 4 {
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Go(func() {
				if _, err := p.ForceRefresh(t.Context(), id); err == nil {
					t.Error("want error")
				}
			})
		}
		wg.Wait()
		if b := p.Breaker(); !b.Open || b.Kinds[0].Kind != FailForbidden || b.Kinds[0].Accounts != 5 {
			t.Fatalf("round %d breaker = %+v", round, b)
		}
		clock = clock.Add(refreshCooldown)
	}
	for _, v := range p.List() {
		if v.Disabled || v.RefreshFails >= maxRefreshFailures {
			t.Errorf("%s: disabled %v fails %d note %q", v.ID, v.Disabled, v.RefreshFails, v.Note)
		}
	}
	for _, a := range readFile(t, path).Accounts {
		if a.Disabled {
			t.Errorf("%s disabled on disk", a.ID)
		}
	}
	// 任意请求成功就解除熔断
	p.Success("a", kiro.Usage{}, 0, false)
	if b := p.Breaker(); b.Open {
		t.Fatalf("breaker after success = %+v", b)
	}
}

// 熔断有下限：只有一个号在失败时（哪怕它就是全部的号）按单号逻辑走，连续 3 次 401 照样停号。
func TestBreakerSingleAccount401StillDisables(t *testing.T) {
	for _, accounts := range []int{1, 5} {
		t.Run(fmt.Sprint(accounts), func(t *testing.T) {
			fk := newFakeKiro(t)
			fk.refreshCode.Store(http.StatusUnauthorized)
			p, _ := openPool(t, fk.client())
			for i := range accounts {
				mustAdd(t, p, Account{ID: fmt.Sprint("a", i), Cred: freshCred(fmt.Sprint("a", i))})
			}
			for i := 1; i <= maxRefreshFailures; i++ {
				_, _ = p.ForceRefresh(t.Context(), "a0")
				if v, _ := p.Get("a0"); v.Disabled != (i == maxRefreshFailures) {
					t.Fatalf("after %d: disabled %v fails %d", i, v.Disabled, v.RefreshFails)
				}
			}
			if b := p.Breaker(); b.Open {
				t.Fatalf("breaker = %+v", b)
			}
		})
	}
}

// 熔断期间 invalid_grant 与封号文字照样立即停号。
func TestBreakerDoesNotShieldDefiniteEvidence(t *testing.T) {
	fk := newFakeKiro(t)
	fk.refreshCode.Store(http.StatusForbidden)
	fk.refreshBody = `<html>blocked</html>`
	p, _ := openPool(t, fk.client())
	for _, id := range []string{"a", "b", "c"} {
		mustAdd(t, p, Account{ID: id, Cred: freshCred(id)})
		_, _ = p.ForceRefresh(t.Context(), id)
	}
	if !p.Breaker().Open {
		t.Fatal("want breaker open")
	}
	fk.refreshCode.Store(http.StatusBadRequest)
	fk.refreshBody = `{"error":"invalid_grant"}`
	_, _ = p.ForceRefresh(t.Context(), "a")
	p.Fail("b", kiro.Classify(403, []byte(`{"message":"Your account is TemporarilySuspended"}`)))
	for id, want := range map[string]bool{"a": true, "b": true, "c": false} {
		if v, _ := p.Get(id); v.Disabled != want {
			t.Errorf("%s disabled = %v, want %v (%q)", id, v.Disabled, want, v.Note)
		}
	}
}

// 每个号一个固定机器码：加号时生成（来源带了就沿用），落盘，重新打开后不变；旧号第一次用时补上。
func TestMachineIDStable(t *testing.T) {
	fk := newFakeKiro(t)
	p, path := openPool(t, fk.client())
	a := mustAdd(t, p, Account{ID: "a", Cred: freshCred("a")})
	b := mustAdd(t, p, Account{ID: "b", Cred: func() kiro.Cred { c := freshCred("b"); c.MachineID = "feedfacefeedfacefeedfacefeedface"; return c }()})
	if len(a.Cred.MachineID) != 32 || b.Cred.MachineID != "feedfacefeedfacefeedfacefeedface" || a.Cred.MachineID == b.Cred.MachineID {
		t.Fatalf("machine ids %q %q", a.Cred.MachineID, b.Cred.MachineID)
	}
	c1, _ := p.Cred(t.Context(), "a", "")
	c2, _ := p.ForceRefresh(t.Context(), "a")
	if c1.MachineID != a.Cred.MachineID || c2.MachineID != a.Cred.MachineID {
		t.Fatalf("machine id changed: %q %q want %q", c1.MachineID, c2.MachineID, a.Cred.MachineID)
	}
	if f := readFile(t, path); f.Accounts[0].Cred.MachineID != a.Cred.MachineID {
		t.Fatalf("not persisted: %+v", f.Accounts[0].Cred)
	}
	// 旧号（文件里没有机器码）：第一次用时生成并落盘
	legacy := filepath.Join(t.TempDir(), "accounts.json")
	raw, _ := json.Marshal(file{Accounts: []Account{{ID: "old", Cred: freshCred("old")}}})
	if err := os.WriteFile(legacy, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	q, err := Open(legacy, Options{Client: fk.client()})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := q.Cred(t.Context(), "old", "")
	if len(got.MachineID) != 32 || readFile(t, legacy).Accounts[0].Cred.MachineID != got.MachineID {
		t.Fatalf("legacy account machine id %q not persisted", got.MachineID)
	}
}

// 全局刷新并发上限：8 个号同时到期，同一时刻最多 2 个在刷。
func TestRefreshConcurrencyLimit(t *testing.T) {
	fk := newFakeKiro(t)
	var cur, peak atomic.Int64
	fk.onRefresh = func() {
		n := cur.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(30 * time.Millisecond)
		cur.Add(-1)
	}
	p, err := Open(filepath.Join(t.TempDir(), "accounts.json"), Options{Client: fk.client(), Logger: quiet(), RefreshConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		c := freshCred(fmt.Sprint("t", i))
		c.ExpiresAt = time.Now().Add(-time.Minute)
		mustAdd(t, p, Account{ID: fmt.Sprint("a", i), Cred: c})
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() { _, _ = p.Cred(t.Context(), fmt.Sprint("a", i), "") })
	}
	wg.Wait()
	if got := fk.refreshHits.Load(); got != 8 {
		t.Fatalf("refreshes = %d", got)
	}
	if peak.Load() > 2 {
		t.Fatalf("peak concurrent refreshes = %d, want <= 2", peak.Load())
	}
}
