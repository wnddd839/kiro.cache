package keys

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStorePersistAndLookup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	k, err := s.Create(Key{Name: "alice", LimitUSD: 5, Models: []string{"claude-sonnet*", " "}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k.Secret, "sk-kiro-") || len(k.Models) != 1 {
		t.Fatalf("created %+v", k)
	}
	if _, err := s.Create(Key{Name: "dup", Secret: k.Secret}); err == nil {
		t.Fatal("duplicate secret accepted")
	}
	if _, err := s.Create(Key{Name: ""}); err == nil {
		t.Fatal("empty name accepted")
	}
	if _, err := s.Create(Key{Name: "x", Period: "year"}); err == nil {
		t.Fatal("bad period accepted")
	}

	r, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := r.Lookup(k.Secret)
	if !ok || got.ID != k.ID || got.Name != "alice" || got.LimitUSD != 5 {
		t.Fatalf("reopened lookup = %+v %v", got, ok)
	}
	if _, ok := r.Lookup("nope"); ok {
		t.Fatal("unknown secret found")
	}

	if got.Secret != "" || got.Hint != k.Hint || got.Masked() != got.Hint {
		t.Fatalf("stored key exposes secret: %+v", got)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), k.Secret) || !strings.Contains(string(raw), HashSecret(k.Secret)) {
		t.Fatalf("keys.json must hold only the hash: %s", raw)
	}

	up, err := r.Update(k.ID, func(k *Key) { k.Disabled = true; k.Secret = "hack"; k.Hash = "x"; k.Name = "bob" })
	if err != nil || up.Secret != "" || up.Name != "bob" || !up.Disabled {
		t.Fatalf("update = %+v %v", up, err)
	}
	if _, ok := r.Lookup(k.Secret); !ok {
		t.Fatal("update must not change the secret")
	}
	rot, err := r.Rotate(k.ID)
	if err != nil || rot.Secret == "" || rot.Secret == k.Secret {
		t.Fatalf("rotate = %+v %v", rot, err)
	}
	if _, ok := r.Lookup(k.Secret); ok {
		t.Fatal("old secret still valid")
	}
	if _, ok := r.Lookup(rot.Secret); !ok {
		t.Fatal("new secret not valid")
	}
	if err := r.Delete(k.ID); err != nil || r.Len() != 0 {
		t.Fatalf("delete: %v len %d", err, r.Len())
	}
	if err := r.Delete(k.ID); !errors.Is(err, ErrUnknown) {
		t.Fatalf("delete twice: %v", err)
	}
}

func TestCheck(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 8, 15, 0, 0, 0, loc) // 周四
	k := Key{Name: "k", LimitUSD: 1, LimitRequests: 10}
	if err := k.Check(now, Spent{CostUSD: 0.5, Requests: 3}, loc); err != nil {
		t.Fatal(err)
	}
	err := k.Check(now, Spent{CostUSD: 1}, loc)
	le, ok := errors.AsType[*LimitError](err)
	if !ok || le.What != "budget" || le.RetryAfter != time.Date(2026, 11, 1, 0, 0, 0, 0, loc).Sub(now) {
		t.Fatalf("budget err = %v", err)
	}
	if le, ok := errors.AsType[*LimitError](k.Check(now, Spent{Requests: 10}, loc)); !ok || le.What != "requests" {
		t.Fatal("requests limit not enforced")
	}
	k.Disabled = true
	if err := k.Check(now, Spent{}, loc); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	k.Disabled, k.ExpiresAt = false, now
	if err := k.Check(now, Spent{}, loc); !errors.Is(err, ErrExpired) {
		t.Fatal(err)
	}

	for period, want := range map[string]time.Time{
		PeriodDay:   time.Date(2026, 10, 8, 0, 0, 0, 0, loc),
		PeriodWeek:  time.Date(2026, 10, 5, 0, 0, 0, 0, loc),
		PeriodMonth: time.Date(2026, 10, 1, 0, 0, 0, 0, loc),
		"":          time.Date(2026, 10, 1, 0, 0, 0, 0, loc),
	} {
		k := Key{Period: period}
		if got := k.PeriodStart(now, loc); !got.Equal(want) {
			t.Errorf("%q start = %v, want %v", period, got, want)
		}
	}
	total := Key{Period: PeriodTotal, CreatedAt: now.Add(-time.Hour)}
	if !total.PeriodStart(now, loc).Equal(total.CreatedAt) || !total.PeriodEnd(now, loc).IsZero() {
		t.Error("total period")
	}
}

func TestAllows(t *testing.T) {
	k := Key{Models: []string{"claude-sonnet*", "auto"}}
	for _, tc := range []struct {
		names []string
		want  bool
	}{
		{[]string{"claude-sonnet-4.5"}, true},
		{[]string{"Claude-Sonnet-4"}, true},
		{[]string{"AUTO"}, true},
		{[]string{"gpt-4o", "claude-sonnet-4.5"}, true},
		{[]string{"claude-opus-4.5"}, false},
	} {
		if got := k.Allows(tc.names...); got != tc.want {
			t.Errorf("Allows(%v) = %v", tc.names, got)
		}
	}
	if !(&Key{}).Allows("anything") {
		t.Error("empty list should allow all")
	}
}

func TestRPM(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	s, _ := Open("", func() time.Time { mu.Lock(); defer mu.Unlock(); return now })
	k, _ := s.Create(Key{Name: "r", RPM: 2})
	if s.Allow(&k) != nil || s.Allow(&k) != nil {
		t.Fatal("first two refused")
	}
	le, ok := errors.AsType[*LimitError](s.Allow(&k))
	if !ok || le.What != "rpm" || le.RetryAfter != time.Minute {
		t.Fatalf("third = %+v", le)
	}
	mu.Lock()
	now = now.Add(61 * time.Second)
	mu.Unlock()
	if err := s.Allow(&k); err != nil {
		t.Fatal(err)
	}

	// 并发
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { _ = s.Allow(&k); s.List(); s.Lookup(k.Secret) })
	}
	wg.Wait()
}

// 旧版 keys.json 存的是明文：载入时改成哈希并重写文件，原 secret 仍然有效。
func TestLegacyPlaintextMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.json")
	legacy := `{"keys":[{"id":"key_1","name":"old","secret":"sk-kiro-0123456789abcdef0123456789abcdef01234567","created_at":"2026-01-01T00:00:00Z"}]}`
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if k, ok := s.Lookup("sk-kiro-0123456789abcdef0123456789abcdef01234567"); !ok || k.ID != "key_1" || k.Secret != "" || k.Hint != "sk-kiro…4567" {
		t.Fatalf("migrated lookup = %+v %v", k, ok)
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), `"secret"`) || !strings.Contains(string(raw), `"secret_sha256"`) {
		t.Fatalf("file not rewritten: %s", raw)
	}
}
