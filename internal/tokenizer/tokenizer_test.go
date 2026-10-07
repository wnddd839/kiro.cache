package tokenizer

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// testdata/parity.json 的期望值来自 @anthropic-ai/tokenizer 的 countTokens。
func TestParityWithReference(t *testing.T) {
	raw, err := os.ReadFile("testdata/parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Text   string `json:"text"`
		Tokens int    `json:"tokens"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 30 {
		t.Fatalf("only %d cases", len(cases))
	}
	for i, c := range cases {
		if got := Count(c.Text); got != c.Tokens {
			t.Errorf("case %d %.40q: got %d, want %d", i, c.Text, got, c.Tokens)
		}
	}
}

func TestEdges(t *testing.T) {
	for _, tc := range []struct {
		s    string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"hello", 1},
		{" hello", 1},
	} {
		if got := Count(tc.s); got != tc.want {
			t.Errorf("Count(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

// 长段走堆合并，结果必须与短段的朴素合并一致。
func TestMergeLongMatchesShort(t *testing.T) {
	r := ranks()
	for _, p := range []string{
		strings.Repeat("a", 100),
		strings.Repeat("ab", 60),
		strings.Repeat("xyz", 40),
		"supercalifragilisticexpialidociousantidisestablishmentarianism",
		strings.Repeat("中文", 20),
		strings.Repeat("0123456789", 12),
	} {
		if s, l := mergeShort(r, p), mergeLong(r, p); s != l {
			t.Errorf("%.20q: short %d, long %d", p, s, l)
		}
	}
}

func TestFold(t *testing.T) {
	for in, want := range map[string]string{
		"ｈｅｌｌｏ　世界": "hello 世界",
		"a\u00a0b": "a b",
		"ﬁne…":     "fine...",
		"①⑳":       "120",
		"plain":    "plain",
	} {
		if got := fold(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
}

func BenchmarkCount(b *testing.B) {
	s := strings.Repeat("func (s *Server) handle(w http.ResponseWriter, r *http.Request) { // 处理请求\n", 200)
	Count(s)
	b.SetBytes(int64(len(s)))
	for b.Loop() {
		Count(s)
	}
}
