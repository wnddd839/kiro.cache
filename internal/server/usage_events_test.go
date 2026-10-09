package server

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/meter"
	"kiro-proxy/internal/turn"
	"kiro-proxy/internal/usage"
)

func TestPartialUsageEventsBilling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		events []string
		want   usageBody
	}{
		{
			name: "output update preserves input",
			events: []string{
				`{"uncachedInputTokens":5000,"cacheReadInputTokens":2000,"cacheWriteInputTokens":3000,"outputTokens":9}`,
				`{"outputTokens":11}`,
				`{"totalTokens":10011,"normalizedTokenUsage":0.5}`,
			},
			want: usageBody{Input: 5000, CacheRead: 2000, CacheWrite: 3000, Output: 11},
		},
		{
			name: "input update preserves output",
			events: []string{
				`{"outputTokens":9}`,
				`{"uncachedInputTokens":5000,"cacheReadInputTokens":2000,"cacheWriteInputTokens":3000}`,
			},
			want: usageBody{Input: 5000, CacheRead: 2000, CacheWrite: 3000, Output: 9},
		},
		{
			name: "cache update recomputes uncached input",
			events: []string{
				`{"inputTokens":10000,"cacheReadInputTokens":2000,"cacheWriteInputTokens":3000,"outputTokens":9}`,
				`{"cacheReadInputTokens":4000}`,
			},
			want: usageBody{Input: 3000, CacheRead: 4000, CacheWrite: 3000, Output: 9},
		},
		{
			name: "explicit zero overrides previous values",
			events: []string{
				`{"uncachedInputTokens":5000,"cacheReadInputTokens":2000,"cacheWriteInputTokens":3000,"outputTokens":9}`,
				`{"uncachedInputTokens":0,"cacheReadInputTokens":0,"cacheWriteInputTokens":0,"outputTokens":0}`,
			},
		},
	} {
		for _, mode := range []string{kiro.ReportedRaw, kiro.ReportedConservative, kiro.ReportedSum} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", tc.name, mode, streaming), func(t *testing.T) {
					h := newHarness(t, 1, func(c *config.Config) { c.ReportedUsage = mode })
					h.up.stream = func(string) []byte {
						b := frame("assistantResponseEvent", `{"content":"hello world"}`)
						for _, event := range tc.events {
							b = append(b, frame("metadataEvent", `{"tokenUsage":`+event+`}`)...)
						}
						return append(b, frame("meteringEvent", `{"unit":"credit","usage":0.5}`)...)
					}
					want := tc.want
					if mode == kiro.ReportedSum {
						// Sum consumes each event as a delta; missing fields contribute zero.
						want = usageBody{Input: 5000, CacheRead: 2000, CacheWrite: 3000, Output: 9}
						switch tc.name {
						case "output update preserves input":
							want.Output = 20
						case "cache update recomputes uncached input":
							want.CacheRead = 6000
						}
					}
					want.Credits = 0.5
					body := convo("hi")
					if streaming {
						body = strings.Replace(body, `"messages"`, `"stream":true,"messages"`, 1)
					}
					res := h.post(t, "/v1/messages", body, nil)
					var got usageBody
					if !streaming {
						got = decodeUsage(t, res)
					} else {
						var sawUsage bool
						sc := bufio.NewScanner(res.Body)
						for sc.Scan() {
							data, ok := strings.CutPrefix(sc.Text(), "data: ")
							if !ok || !strings.Contains(data, `"message_delta"`) {
								continue
							}
							var event struct{ Usage usageBody }
							if err := json.Unmarshal([]byte(data), &event); err != nil {
								t.Fatal(err)
							}
							got, sawUsage = event.Usage, true
						}
						if err := sc.Err(); err != nil || !sawUsage {
							t.Fatalf("stream usage missing: %v", err)
						}
					}
					if got != want {
						t.Fatalf("response usage = %+v, want %+v", got, want)
					}
					e := waitRequests(t, h, usage.Query{}, 1).Entries[0]
					cost := h.s.pricer.Cost(e.Model, turn.Usage{Input: want.Input, CacheRead: want.CacheRead, CacheWrite: want.CacheWrite, Output: want.Output, Credits: want.Credits})
					if e.Input != want.Input || e.CacheRead != want.CacheRead || e.CacheWrite != want.CacheWrite || e.Output != want.Output || math.Abs(e.CostUSD-cost) > 1e-12 {
						t.Fatalf("journal usage differs from response: %+v, want cost %v", e, cost)
					}
				})
			}
		}
	}
}

func TestFailedAttemptCacheBillingAcrossRequests(t *testing.T) {
	for _, ttl := range []string{"5m", "1h"} {
		for _, stage := range []string{"before usage", "credits before content", "input before content", "after content"} {
			t.Run(ttl+"/"+stage, func(t *testing.T) {
				h := newHarness(t, 2, func(c *config.Config) { c.CacheMode = meter.ModeExplicit })
				var clock atomic.Int64
				clock.Store(time.Now().Unix())
				h.s.cache = meter.NewCache(func() time.Time { return time.Unix(clock.Load(), 0) })
				var failed atomic.Bool
				h.up.stream = func(string) []byte {
					if failed.CompareAndSwap(false, true) {
						var b []byte
						if stage == "after content" {
							b = frame("assistantResponseEvent", `{"content":"partial answer"}`)
						}
						if stage == "input before content" {
							b = frame("metadataEvent", `{"tokenUsage":{"uncachedInputTokens":5000,"cacheWriteInputTokens":3000}}`)
							b = append(b, frame("metadataEvent", `{"tokenUsage":{"outputTokens":0}}`)...)
						}
						if stage != "before usage" {
							b = append(b, frame("meteringEvent", `{"unit":"credit","usage":0.25}`)...)
						}
						return append(b, frame("throttlingError", `{"message":"Rate limited"}`)...)
					}
					b := frame("assistantResponseEvent", `{"content":"hello world"}`)
					return append(b, frame("meteringEvent", `{"unit":"credit","usage":0.5}`)...)
				}
				body := cached(convo("fix the bug"), ttl)
				u1 := decodeUsage(t, h.post(t, "/v1/messages", body, nil))
				v := waitRequests(t, h, usage.Query{}, 1)
				if u1.CacheRead != 0 || u1.CacheWrite == 0 {
					t.Fatalf("successful account inherited failed account's cache: %+v", u1)
				}
				if stage != "before usage" {
					if v.TotalEntries != 2 || v.Totals.Retried != 1 || v.Totals.RetryCredits != 0.25 || v.Entries[1].CostUSD != 0 {
						t.Fatalf("failed attempt cost missing or billed downstream: %+v, entries %+v", v.Totals, v.Entries)
					}
					if stage == "input before content" {
						e := v.Entries[1]
						wantLong := 0
						if ttl == "1h" {
							wantLong = 3000
						}
						if e.Input != 5000 || e.CacheWrite != 3000 || e.CacheWrite1h != wantLong || e.Output != 0 {
							t.Fatalf("partial reported input lost before failover: %+v", e)
						}
					}
				} else if v.TotalEntries != 1 || v.Totals.Retried != 0 {
					t.Fatalf("unused attempt billed: %+v", v)
				}
				if v.Totals.CostUSD != v.Entries[0].CostUSD {
					t.Fatalf("failed attempt changed downstream cost: %+v", v.Totals)
				}

				// Bring the failed account back and force the next request onto it.
				tokens := h.up.tokens()
				badID := "a" + strings.TrimPrefix(tokens[0], "tok")
				goodID := "a" + strings.TrimPrefix(tokens[1], "tok")
				if err := h.pool.SetDisabled(goodID, true, "test cache isolation"); err != nil {
					t.Fatal(err)
				}
				if err := h.pool.SetDisabled(badID, false, ""); err != nil {
					t.Fatal(err)
				}
				u2 := decodeUsage(t, h.post(t, "/v1/messages", body, nil))
				if stage == "after content" || stage == "input before content" {
					if u2.CacheRead != u1.CacheWrite || u2.CacheWrite != 0 {
						t.Fatalf("processed prefix should be warm on failed account: %+v after %+v", u2, u1)
					}
				} else if u2 != u1 {
					t.Fatalf("failure without input usage warmed cache: %+v after %+v", u2, u1)
				}
				waitRequests(t, h, usage.Query{}, 2)

				clock.Add(int64((6 * time.Minute) / time.Second))
				u3 := decodeUsage(t, h.post(t, "/v1/messages", body, nil))
				if ttl == "1h" {
					if u3.CacheRead != u1.CacheWrite || u3.CacheWrite != 0 {
						t.Fatalf("1h prefix expired early: %+v", u3)
					}
				} else if u3 != u1 {
					t.Fatalf("5m prefix still warm after expiry: %+v after %+v", u3, u1)
				}
				waitRequests(t, h, usage.Query{}, 3)
			})
		}
	}
}

func TestUsageBeforeFirstContentTerminalBilling(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprint("stream=", streaming), func(t *testing.T) {
			h := newHarness(t, 2, nil)
			h.up.stream = func(string) []byte {
				b := frame("meteringEvent", `{"unit":"credit","usage":0.25}`)
				return append(b, frame("validationError", `{"message":"Input is too long"}`)...)
			}
			body := cached(convo("hi"), "1h")
			if streaming {
				body = strings.Replace(body, `"messages"`, `"stream":true,"messages"`, 1)
			}
			res := h.post(t, "/v1/messages", body, nil)
			if _, err := io.Copy(io.Discard, res.Body); err != nil {
				t.Fatal(err)
			}
			if res.StatusCode != http.StatusBadRequest || strings.HasPrefix(res.Header.Get("Content-Type"), "text/event-stream") {
				t.Fatalf("terminal error started a reply: status %d, content-type %q", res.StatusCode, res.Header.Get("Content-Type"))
			}
			v := waitRequests(t, h, usage.Query{}, 1)
			e := v.Entries[0]
			if v.TotalEntries != 1 || e.Attempts != 1 || e.Retried || !e.Aborted || e.Credits != 0.25 || e.CostUSD != 0 || e.UpstreamUSD != h.s.pricer.CreditsUSD(0.25) || e.Input+e.CacheRead+e.CacheWrite+e.Output != 0 {
				t.Fatalf("terminal usage not settled: %+v", e)
			}
			if stats := h.pool.Totals(); stats.Credits != 0.25 || stats.Requests != 0 {
				t.Fatalf("terminal pool cost not recorded: %+v", stats)
			}
		})
	}
}
