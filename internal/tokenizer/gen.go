//go:build ignore

// gen 把 @anthropic-ai/tokenizer 的 claude.json 转成 claude.bpe.gz：
// gzip("BPE1" | uvarint 起始 rank | uvarint 条数 | 每条 uvarint 长度 + 字节)。
//
//	go run gen.go path/to/claude.json
package main

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func main() {
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var src struct {
		Pat   string `json:"pat_str"`
		Ranks string `json:"bpe_ranks"`
	}
	if err := json.Unmarshal(raw, &src); err != nil {
		return err
	}
	parts := strings.Split(src.Ranks, " ")
	if len(parts) < 3 {
		return fmt.Errorf("bpe_ranks: too short")
	}
	offset, err := strconv.Atoi(parts[1])
	if err != nil {
		return fmt.Errorf("bpe_ranks offset: %w", err)
	}
	tokens := parts[2:]

	var body bytes.Buffer
	body.WriteString("BPE1")
	body.Write(binary.AppendUvarint(nil, uint64(offset)))
	body.Write(binary.AppendUvarint(nil, uint64(len(tokens))))
	for _, t := range tokens {
		b, err := base64.StdEncoding.DecodeString(t)
		if err != nil {
			return fmt.Errorf("token %q: %w", t, err)
		}
		body.Write(binary.AppendUvarint(nil, uint64(len(b))))
		body.Write(b)
	}

	var out bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&out, gzip.BestCompression)
	if _, err := zw.Write(body.Bytes()); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	fmt.Printf("pattern: %s\n%d tokens from rank %d, %d bytes\n", src.Pat, len(tokens), offset, out.Len())
	return os.WriteFile("claude.bpe.gz", out.Bytes(), 0o644)
}
