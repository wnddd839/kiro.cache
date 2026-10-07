// Package tokenizer 是 Anthropic 公开的 Claude 分词器（@anthropic-ai/tokenizer，MIT）的纯 Go 实现。
//
// 它是 Claude 2 时代的 BPE，与现行模型的分词并不相同，但两者的比例很稳定；
// 换算系数在 meter 包里按 Kiro 上游的实测值校准。这里只给原始计数。
//
// 词表 claude.bpe.gz 由 gen.go 从 npm 包的 claude.json 生成。
package tokenizer

import (
	"bufio"
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

//go:embed claude.bpe.gz
var vocabGz []byte

// ranks 是 字节串 → 合并优先级（越小越先合并）。
var ranks = sync.OnceValue(func() map[string]int32 {
	m, err := load(vocabGz)
	if err != nil {
		panic("tokenizer: embedded vocabulary: " + err.Error())
	}
	return m
})

func load(gz []byte) (map[string]int32, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(zr)
	magic := make([]byte, 4)
	if _, err := io.ReadFull(r, magic); err != nil || string(magic) != "BPE1" {
		return nil, errors.New("bad header")
	}
	offset, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	n, err := binary.ReadUvarint(r)
	if err != nil {
		return nil, err
	}
	m := make(map[string]int32, n)
	buf := make([]byte, 0, 64)
	for i := range n {
		l, err := binary.ReadUvarint(r)
		if err != nil {
			return nil, fmt.Errorf("token %d: %w", i, err)
		}
		if cap(buf) < int(l) {
			buf = make([]byte, l)
		}
		buf = buf[:l]
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, fmt.Errorf("token %d: %w", i, err)
		}
		m[string(buf)] = int32(offset + i)
	}
	return m, nil
}

// Count 返回 s 的 token 数（先做兼容字符折叠，对应原实现的 NFKC）。
func Count(s string) int {
	if s == "" {
		return 0
	}
	s = fold(s)
	r := ranks()
	n := 0
	for len(s) > 0 {
		l := nextPiece(s)
		n += pieceTokens(r, s[:l])
		s = s[l:]
	}
	return n
}

// fold 做 NFKC 里对计数有影响的常见部分：全角 ASCII、各种兼容空格、省略号、连字、圈号数字、上下标。
// 不做组合分解（é 等与 NFKC 结果一致，组合后计数相同）。省掉 x/text 依赖。
func fold(s string) string {
	i := strings.IndexFunc(s, func(r rune) bool { _, ok := foldOf(r); return ok })
	if i < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 8)
	b.WriteString(s[:i])
	for _, r := range s[i:] {
		if f, ok := foldOf(r); ok {
			b.WriteString(f)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func foldOf(r rune) (string, bool) {
	if r < 0xA0 {
		return "", false
	}
	switch {
	case r >= 0xFF01 && r <= 0xFF5E:
		return string(r - 0xFEE0), true
	case r == 0xA0, r == 0x3000, r >= 0x2000 && r <= 0x200A, r == 0x202F, r == 0x205F:
		return " ", true
	case r >= 0x2460 && r <= 0x2473: // ①..⑳
		return strconv.Itoa(int(r-0x2460) + 1), true
	case r >= 0x2080 && r <= 0x2089: // ₀..₉
		return string('0' + r - 0x2080), true
	}
	f, ok := foldTable[r]
	return f, ok
}

var foldTable = map[rune]string{
	'…': "...", '‥': "..", '™': "TM", '℃': "°C",
	'ﬀ': "ff", 'ﬁ': "fi", 'ﬂ': "fl", 'ﬃ': "ffi", 'ﬄ': "ffl",
	'½': "1⁄2", '¼': "1⁄4", '¾': "3⁄4",
	'²': "2", '³': "3", '¹': "1", '⁰': "0", '⁴': "4", '⁵': "5", '⁶': "6", '⁷': "7", '⁸': "8", '⁹': "9",
	'\u00AA': "a", '\u00BA': "o", '\u00B5': "μ",
}

// nextPiece 返回下一段预切分的字节长度。等价于正则
//
//	's|'t|'re|'ve|'m|'ll|'d| ?\p{L}+| ?\p{N}+| ?[^\s\p{L}\p{N}]+|\s+(?!\S)|\s+
func nextPiece(s string) int {
	if s[0] == '\'' && len(s) >= 2 {
		switch s[1] {
		case 's', 't', 'm', 'd':
			return 2
		case 'r', 'v':
			if len(s) >= 3 && s[2] == 'e' {
				return 3
			}
		case 'l':
			if len(s) >= 3 && s[2] == 'l' {
				return 3
			}
		}
	}

	// " ?" 前缀：一个空格后接字母 / 数字 / 符号时并入下一段
	start := 0
	if s[0] == ' ' && len(s) > 1 {
		if r, _ := utf8.DecodeRuneInString(s[1:]); classOf(r) != classSpace {
			start = 1
		}
	}
	r, n := utf8.DecodeRuneInString(s[start:])
	c := classOf(r)
	if c != classSpace {
		return start + n + runOf(s[start+n:], c)
	}

	// 空白串：后面跟着非空白时留下最后一个空白字符，给下一段当前缀
	end := n + runOf(s[n:], classSpace)
	if end == len(s) {
		return end
	}
	_, last := utf8.DecodeLastRuneInString(s[:end])
	if end-last > 0 {
		return end - last
	}
	return end
}

type class uint8

const (
	classSpace class = iota
	classLetter
	classNumber
	classOther
)

func classOf(r rune) class {
	if r < utf8.RuneSelf {
		return asciiClass[r]
	}
	switch {
	case unicode.IsLetter(r):
		return classLetter
	case unicode.IsNumber(r):
		return classNumber
	case unicode.Is(unicode.White_Space, r):
		return classSpace
	}
	return classOther
}

var asciiClass = func() (t [utf8.RuneSelf]class) {
	for i := range t {
		r := rune(i)
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			t[i] = classLetter
		case r >= '0' && r <= '9':
			t[i] = classNumber
		case r == ' ', r >= '\t' && r <= '\r':
			t[i] = classSpace
		default:
			t[i] = classOther
		}
	}
	return t
}()

// runOf 返回 s 开头连续属于 c 类的字节数。
func runOf(s string, c class) int {
	i := 0
	for i < len(s) {
		if b := s[i]; b < utf8.RuneSelf {
			if asciiClass[b] != c {
				break
			}
			i++
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		if classOf(r) != c {
			break
		}
		i += n
	}
	return i
}

// pieceTokens 是一段的 BPE 结果长度。
func pieceTokens(r map[string]int32, p string) int {
	if len(p) <= 1 {
		return len(p)
	}
	if _, ok := r[p]; ok {
		return 1
	}
	if len(p) <= shortPiece {
		return mergeShort(r, p)
	}
	return mergeLong(r, p)
}

const (
	shortPiece = 128
	noRank     = int32(1<<31 - 1)
)

// mergeShort 是 tiktoken 的 _byte_pair_merge：每轮合并优先级最小（并列取最左）的相邻对。O(n²)，短段最快。
func mergeShort(r map[string]int32, p string) int {
	type part struct {
		start int
		rank  int32
	}
	var stack [shortPiece + 1]part
	parts := stack[:0]
	rankAt := func(i int) int32 { // parts[i] 与 parts[i+1] 合并后的优先级
		if i+2 >= len(parts) {
			return noRank
		}
		if v, ok := r[p[parts[i].start:parts[i+2].start]]; ok {
			return v
		}
		return noRank
	}
	for i := range len(p) + 1 {
		parts = append(parts, part{start: i, rank: noRank})
	}
	for i := range len(parts) - 2 {
		parts[i].rank = rankAt(i)
	}
	for len(parts) > 2 {
		best, at := noRank, -1
		for i := range len(parts) - 1 {
			if parts[i].rank < best {
				best, at = parts[i].rank, i
			}
		}
		if at < 0 {
			break
		}
		parts = append(parts[:at+1], parts[at+2:]...)
		parts[at].rank = rankAt(at)
		if at > 0 {
			parts[at-1].rank = rankAt(at - 1)
		}
	}
	return len(parts) - 1
}

// mergeLong 与 mergeShort 结果相同，用双向链表 + 最小堆，O(n log n)，给超长段（长串同类字符）用。
func mergeLong(r map[string]int32, p string) int {
	n := len(p)
	// 节点 i 代表从字节 i 开始的当前片段；next/prev 链接存活节点，n 是哨兵
	next := make([]int, n+1)
	prev := make([]int, n+1)
	ver := make([]uint32, n+1) // 节点的片段变化时自增，作废堆里的旧条目
	for i := range n + 1 {
		next[i], prev[i] = i+1, i-1
	}
	h := &mergeHeap{}
	push := func(i int) {
		j := next[i]
		if j >= n {
			return
		}
		if v, ok := r[p[i:next[j]]]; ok {
			h.push(heapItem{rank: v, pos: i, ver: ver[i]})
		}
	}
	for i := range n {
		push(i)
	}
	count := n
	for h.len() > 0 {
		it := h.pop()
		i := it.pos
		if it.ver != ver[i] {
			continue
		}
		// 合并 i 与 next[i]
		j := next[i]
		next[i] = next[j]
		prev[next[j]] = i
		ver[i]++
		ver[j]++
		count--
		push(i)
		if pi := prev[i]; pi >= 0 {
			ver[pi]++
			push(pi)
		}
	}
	return count
}

type heapItem struct {
	rank int32
	pos  int
	ver  uint32
}

func (a heapItem) less(b heapItem) bool {
	if a.rank != b.rank {
		return a.rank < b.rank
	}
	return a.pos < b.pos
}

// mergeHeap 是按 (rank, pos) 排序的最小堆；手写以免 container/heap 的接口装箱。
type mergeHeap struct{ a []heapItem }

func (h *mergeHeap) len() int { return len(h.a) }

func (h *mergeHeap) push(x heapItem) {
	h.a = append(h.a, x)
	for i := len(h.a) - 1; i > 0; {
		p := (i - 1) / 2
		if !h.a[i].less(h.a[p]) {
			break
		}
		h.a[i], h.a[p] = h.a[p], h.a[i]
		i = p
	}
}

func (h *mergeHeap) pop() heapItem {
	top := h.a[0]
	last := len(h.a) - 1
	h.a[0] = h.a[last]
	h.a = h.a[:last]
	for i := 0; ; {
		l, m := 2*i+1, i
		if l < len(h.a) && h.a[l].less(h.a[m]) {
			m = l
		}
		if l+1 < len(h.a) && h.a[l+1].less(h.a[m]) {
			m = l + 1
		}
		if m == i {
			break
		}
		h.a[i], h.a[m] = h.a[m], h.a[i]
		i = m
	}
	return top
}
