package usage

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var utc8 = time.FixedZone("UTC+8", 8*3600)

// clock 是可推进的假时钟。
type clock struct{ ns atomic.Int64 }

func newClock(t time.Time) *clock {
	c := &clock{}
	c.ns.Store(t.UnixNano())
	return c
}

func (c *clock) Now() time.Time      { return time.Unix(0, c.ns.Load()).In(utc8) }
func (c *clock) Set(t time.Time)     { c.ns.Store(t.UnixNano()) }
func (c *clock) Add(d time.Duration) { c.ns.Add(int64(d)) }
func at(h, m int) time.Time          { return time.Date(2026, 10, 6, h, m, 0, 0, utc8) }
func open(t *testing.T, path string, c *clock) *Journal {
	t.Helper()
	j, err := Open(path, Options{Now: c.Now, Location: utc8, Recent: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { j.Close() })
	return j
}

func entry(tm time.Time, key, model string, status int, in, out int, cost float64) Entry {
	return Entry{
		Time: tm, Key: key, KeyName: "name-" + key, Account: "acc1", Model: model,
		Protocol: "anthropic", Status: status, Input: in, CacheRead: in, Output: out, CostUSD: cost, MS: 100,
	}
}

func TestRecordQueryTotalsAndFilters(t *testing.T) {
	c := newClock(at(12, 30))
	j := open(t, "", c)
	j.Record(entry(at(10, 5), "k1", "m1", 200, 100, 10, 1))
	j.Record(entry(at(11, 5), "k2", "m2", 500, 0, 0, 0))
	e := entry(at(12, 5), "k1", "m2", 200, 50, 5, 0.5)
	e.Account, e.Protocol = "acc2", "openai-chat"
	j.Record(e)

	v := j.Query(Query{})
	if v.Totals.Requests != 3 || v.Totals.Errors != 1 || v.Totals.Input != 150 || v.Totals.Output != 15 || v.Totals.CostUSD != 1.5 {
		t.Fatalf("totals = %+v", v.Totals)
	}
	if hr := v.Totals.HitRate(); hr != 0.5 {
		t.Fatalf("hit rate = %v", hr)
	}
	cases := []struct {
		q    Query
		want int64
	}{
		{Query{Key: "k1"}, 2},
		{Query{Key: "k2"}, 1},
		{Query{Model: "m2"}, 2},
		{Query{Account: "acc2"}, 1},
		{Query{Protocol: "openai-chat"}, 1},
		{Query{Status: "ok"}, 2},
		{Query{Status: "error"}, 1},
		{Query{Key: "k1", Model: "m1"}, 1},
		{Query{Key: "nope"}, 0},
		{Query{From: at(11, 0), To: at(12, 0)}, 1},
		{Query{From: at(10, 30), To: at(11, 30)}, 2}, // 按小时取整
	}
	for _, tc := range cases {
		if got := j.Query(tc.q).Totals.Requests; got != tc.want {
			t.Errorf("%+v: requests = %d, want %d", tc.q, got, tc.want)
		}
	}
}

func TestSeriesHourZeroFill(t *testing.T) {
	c := newClock(at(12, 30))
	j := open(t, "", c)
	j.Record(entry(at(9, 10), "k", "m", 200, 1, 1, 0))
	j.Record(entry(at(9, 50), "k", "m", 200, 1, 1, 0))
	j.Record(entry(at(11, 0), "k", "m", 200, 1, 1, 0))

	v := j.Query(Query{From: at(8, 0), To: at(12, 0)})
	if v.Bucket != "hour" || len(v.Series) != 4 {
		t.Fatalf("bucket=%s series=%d", v.Bucket, len(v.Series))
	}
	want := []int64{0, 2, 0, 1}
	for i, p := range v.Series {
		if !p.Time.Equal(at(8+i, 0)) || p.Requests != want[i] {
			t.Errorf("series[%d] = %v %d", i, p.Time, p.Requests)
		}
	}

	// 默认范围：过去 24h，含当前小时
	v = j.Query(Query{})
	if len(v.Series) != 25 || !v.Series[24].Time.Equal(at(12, 0)) || v.Totals.Requests != 3 {
		t.Fatalf("default series=%d last=%v total=%d", len(v.Series), v.Series[len(v.Series)-1].Time, v.Totals.Requests)
	}
}

func TestSeriesDayZeroFill(t *testing.T) {
	now := at(12, 0)
	c := newClock(now)
	j := open(t, "", c)
	day := func(d int, h int) time.Time { return time.Date(2026, 10, d, h, 0, 0, 0, utc8) }
	j.Record(entry(day(1, 1), "k", "m", 200, 1, 1, 1))
	j.Record(entry(day(1, 23), "k", "m", 200, 1, 1, 1))
	j.Record(entry(day(4, 0), "k", "m", 200, 1, 1, 1))

	v := j.Query(Query{From: day(1, 5), To: day(5, 0)})
	if v.Bucket != "day" || len(v.Series) != 4 {
		t.Fatalf("bucket=%s series=%d", v.Bucket, len(v.Series))
	}
	want := []int64{2, 0, 0, 1}
	for i, p := range v.Series {
		if !p.Time.Equal(day(1+i, 0)) || p.Requests != want[i] {
			t.Errorf("series[%d] = %v %d", i, p.Time, p.Requests)
		}
	}

	// 显式 day 桶、仅给 From
	v = j.Query(Query{From: day(3, 0), Bucket: "day"})
	if len(v.Series) != 4 || v.Totals.Requests != 1 {
		t.Fatalf("series=%d total=%d", len(v.Series), v.Totals.Requests)
	}
}

func TestGroupsOrdering(t *testing.T) {
	c := newClock(at(12, 0))
	j := open(t, "", c)
	rec := func(key, model string, n int, credits float64) {
		for range n {
			e := entry(at(10, 0), key, model, 200, 1, 1, credits)
			e.Credits = credits
			j.Record(e)
		}
	}
	rec("a", "cheap", 2, 0.1)
	rec("b", "pricey", 1, 5)
	rec("c", "free", 1, 0)
	rec("c", "free2", 2, 0)
	v := j.Query(Query{})
	names := func(gs []Group) string {
		var s []string
		for _, g := range gs {
			s = append(s, g.Name)
		}
		return strings.Join(s, ",")
	}
	if got := names(v.ByModel); got != "pricey,cheap,free2,free" {
		t.Errorf("by model = %s", got)
	}
	if got := names(v.ByKey); got != "b,a,c" {
		t.Errorf("by key = %s", got)
	}
	if v.ByKey[0].Label != "name-b" {
		t.Errorf("label = %q", v.ByKey[0].Label)
	}
	if len(v.ByAccount) != 1 || v.ByAccount[0].Requests != 6 {
		t.Errorf("by account = %+v", v.ByAccount)
	}
}

func TestSpend(t *testing.T) {
	c := newClock(at(12, 0))
	j := open(t, "", c)
	j.Record(entry(at(8, 0), "k1", "m", 200, 1, 1, 1))
	j.Record(entry(at(10, 0), "k1", "m", 200, 1, 1, 2))
	j.Record(entry(at(10, 30), "k2", "m", 200, 1, 1, 4))
	j.Record(entry(at(11, 0), "", "m", 200, 1, 1, 8))

	cases := []struct {
		key   string
		since time.Time
		want  float64
	}{
		{"k1", at(0, 0), 3},
		{"k1", at(9, 0), 2},
		{"k1", at(10, 59), 2}, // 向下取整到小时
		{"k2", at(0, 0), 4},
		{"", at(0, 0), 8},
		{"*", at(0, 0), 15},
		{"*", at(10, 0), 14},
		{"none", at(0, 0), 0},
	}
	for _, tc := range cases {
		if got := j.Spend(tc.key, tc.since).CostUSD; got != tc.want {
			t.Errorf("Spend(%q, %v) = %v, want %v", tc.key, tc.since, got, tc.want)
		}
	}
}

func TestPaginationAndStatus(t *testing.T) {
	c := newClock(at(12, 0))
	j := open(t, "", c)
	for i := range 25 {
		st := 200
		if i%5 == 0 {
			st = 429
		}
		e := entry(at(11, i), "k", "m", st, 1, 1, 0)
		e.ID = fmt.Sprint(i)
		j.Record(e)
	}

	v := j.Query(Query{PageSize: 10, Page: 2})
	if v.TotalEntries != 25 || len(v.Entries) != 10 || v.Entries[0].ID != "14" || v.Entries[9].ID != "5" {
		t.Fatalf("page2: total=%d len=%d first=%v", v.TotalEntries, len(v.Entries), v.Entries)
	}
	v = j.Query(Query{PageSize: 10, Page: 3})
	if len(v.Entries) != 5 || v.Entries[4].ID != "0" {
		t.Fatalf("page3 len=%d", len(v.Entries))
	}
	v = j.Query(Query{Page: 9})
	if len(v.Entries) != 0 || v.PageSize != defaultPageSize {
		t.Fatalf("page9 len=%d size=%d", len(v.Entries), v.PageSize)
	}
	if v = j.Query(Query{PageSize: 10000}); v.PageSize != maxPageSize || v.Page != 1 {
		t.Fatalf("page size = %d page = %d", v.PageSize, v.Page)
	}

	v = j.Query(Query{Status: "error"})
	if v.TotalEntries != 5 || v.Totals.Requests != 5 || v.Totals.Errors != 5 {
		t.Fatalf("errors: entries=%d totals=%+v", v.TotalEntries, v.Totals)
	}
	for _, e := range v.Entries {
		if e.OK() {
			t.Fatalf("ok entry in error filter: %+v", e)
		}
	}
	if v = j.Query(Query{Status: "ok"}); v.TotalEntries != 20 || v.Totals.Errors != 0 {
		t.Fatalf("ok: entries=%d errors=%d", v.TotalEntries, v.Totals.Errors)
	}

	// 环形缓冲覆盖旧记录，聚合不受影响
	for i := range 100 {
		j.Record(entry(at(11, 30+i%30), "k", "m", 200, 1, 1, 0))
	}
	if v = j.Query(Query{}); v.TotalEntries != 100 || v.Totals.Requests != 125 {
		t.Fatalf("ring: entries=%d requests=%d", v.TotalEntries, v.Totals.Requests)
	}
}

func TestPersistRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "usage.jsonl")
	c := newClock(at(12, 0))
	j, err := Open(path, Options{Now: c.Now, Location: utc8})
	if err != nil {
		t.Fatal(err)
	}
	j.Record(entry(at(10, 0), "k1", "m", 200, 10, 1, 1.25))
	j.Record(entry(at(11, 0), "k2", "m", 502, 0, 0, 0))
	if err := j.Flush(); err != nil {
		t.Fatal(err)
	}
	j.Record(entry(at(11, 30), "k1", "m", 200, 10, 1, 1.25))
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS != "windows" {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("perm: %v %v", fi.Mode(), err)
		}
	}
	before := readFile(t, path)

	j2 := open(t, path, c)
	v := j2.Query(Query{})
	if v.Totals.Requests != 3 || v.Totals.Errors != 1 || v.Totals.CostUSD != 2.5 || v.Totals.Input != 20 {
		t.Fatalf("restored totals = %+v", v.Totals)
	}
	if v.TotalEntries != 3 || v.Entries[0].Time.Unix() != at(11, 30).Unix() || v.Entries[0].ID == "" {
		t.Fatalf("restored entries = %+v", v.Entries)
	}
	if j2.Spend("k1", at(0, 0)).CostUSD != 2.5 {
		t.Fatal("restored spend")
	}
	if v.ByKey[0].Label != "name-k1" {
		t.Fatalf("restored label = %q", v.ByKey[0].Label)
	}
	if got := readFile(t, path); got != before {
		t.Fatal("clean file was rewritten")
	}
}

func TestRetentionPruneRewrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	c := newClock(at(12, 0))
	j, err := Open(path, Options{Now: c.Now, Location: utc8, Retention: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	j.Record(entry(at(12, 0).Add(-72*time.Hour), "k", "m", 200, 1, 1, 1)) // 已过期
	j.Record(entry(at(10, 0), "k", "m", 200, 1, 1, 2))
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readFile(t, path), "\n"); n != 2 {
		t.Fatalf("lines before = %d", n)
	}

	j2, err := Open(path, Options{Now: c.Now, Location: utc8, Retention: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer j2.Close()
	if n := strings.Count(readFile(t, path), "\n"); n != 1 {
		t.Fatalf("lines after = %d", n)
	}
	if s := j2.Spend("*", time.Time{}); s.Requests != 1 || s.CostUSD != 2 {
		t.Fatalf("spend = %+v", s)
	}

	// 内存桶随时间推移被修剪
	c.Add(72 * time.Hour)
	j2.prune()
	if s := j2.Spend("*", time.Time{}); s.Requests != 0 {
		t.Fatalf("after prune spend = %+v", s)
	}
}

func TestMalformedLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	good, _ := json.Marshal(entry(at(10, 0), "k", "m", 200, 1, 1, 1))
	data := "not json\n" + string(good) + "\r\n\n{\"id\":\"x\"}\n" + string(good) + "\n{\"time\":\"2026-10-06T"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c := newClock(at(12, 0))
	j := open(t, path, c)
	if s := j.Spend("k", time.Time{}); s.Requests != 2 {
		t.Fatalf("spend = %+v", s)
	}
	if got, want := readFile(t, path), string(good)+"\n"+string(good)+"\n"; got != want {
		t.Fatalf("rewritten = %q", got)
	}
	// 重写后追加正常
	j.Record(entry(at(11, 0), "k", "m", 200, 1, 1, 1))
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j2 := open(t, path, c)
	if s := j2.Spend("k", time.Time{}); s.Requests != 3 {
		t.Fatalf("reopen spend = %+v", s)
	}
}

func TestBackgroundFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	j := open(t, path, newClock(at(12, 0)))
	j.Record(entry(at(11, 0), "k", "m", 200, 1, 1, 1))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, _ := os.ReadFile(path); len(b) > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("background flush did not write")
}

func TestJSONShape(t *testing.T) {
	p := Point{Time: at(1, 0), Totals: Totals{Requests: 2, Input: 1, CacheRead: 3}}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["requests"] != 2.0 || m["hit_rate"] != 0.75 || m["time"] == nil || m["cache_read"] != 3.0 {
		t.Fatalf("point json = %s", b)
	}
	b, _ = json.Marshal(Group{Name: "n", Label: "l", Totals: Totals{CostUSD: 1}})
	if !strings.Contains(string(b), `"name":"n"`) || !strings.Contains(string(b), `"cost_usd":1`) || !strings.Contains(string(b), `"hit_rate":0`) {
		t.Fatalf("group json = %s", b)
	}
	b, _ = json.Marshal(View{Totals: Totals{Requests: 1}})
	if !strings.Contains(string(b), `"totals":{"requests":1`) {
		t.Fatalf("view json = %s", b)
	}
	b, _ = json.Marshal(Entry{ID: "x", Status: 200})
	if strings.Contains(string(b), "key") || strings.Contains(string(b), "input") {
		t.Fatalf("entry omitzero = %s", b)
	}
}

func TestTPS(t *testing.T) {
	c := newClock(at(12, 0))
	j := open(t, "", c)
	e := entry(at(12, 0), "k", "m", 200, 10, 200, 0)
	e.MS, e.FirstMS = 1100, 100
	j.Record(e)
	bad := entry(at(12, 1), "k", "m", 500, 0, 50, 0)
	bad.MS, bad.FirstMS = 800, 100
	j.Record(bad)
	v := j.Query(Query{})
	if v.Totals.GenMS != 1000 {
		t.Fatalf("gen_ms = %d, want 1000 (error 不计入)", v.Totals.GenMS)
	}
	if got := v.Totals.TPS(); got != 200 {
		t.Fatalf("tps = %v, want 200 (200 tok / 1s)", got)
	}
	b, _ := json.Marshal(v.Totals)
	if !strings.Contains(string(b), `"tps":200`) || !strings.Contains(string(b), `"gen_ms":1000`) {
		t.Fatalf("totals json = %s", b)
	}
}

func TestConcurrentRecordQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	c := newClock(at(12, 0))
	j := open(t, path, c)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 200 {
				j.Record(entry(at(11, i%60), fmt.Sprint("k", w%3), "m", 200+(i%2)*300, 1, 1, 0.01))
			}
		})
	}
	for range 4 {
		wg.Go(func() {
			for range 50 {
				_ = j.Query(Query{Status: "ok", PageSize: 20})
				_ = j.Spend("*", at(0, 0))
				_ = j.Flush()
			}
		})
	}
	wg.Go(func() {
		for range 20 {
			j.prune()
		}
	})
	wg.Wait()
	if s := j.Spend("*", at(0, 0)); s.Requests != 1600 || s.Errors != 800 {
		t.Fatalf("spend = %+v", s)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readFile(t, path), "\n"); n != 1600 {
		t.Fatalf("lines = %d", n)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
