package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/pool"
	"kiro-proxy/internal/probe"
)

const probeUsage = `kiro-proxy probe <experiment> [flags]

experiments:
  usage       上游报不报 tokenUsage、报几次、是否累计（3 次调用）
  ttl         缓存存活时间：每个间隔一份新前缀，同 / 新 conversationId 两组（默认约 70 分钟）
  cachepoint  显式 cachePoint：不发 / 首条 user / assistant / tools 末尾 / 全发，冷热对比
  credits     credits 折算：5k/20k/50k 前缀冷、热（只回 ok）、长输出
  continuation  agentContinuationId A/B：发 / 不发由 conversationId 派生的固定值
  longctx     60k 前缀同一 conversationId 连发两次，看命中时 contextUsagePercentage 会不会变小
  all         依次跑 usage、cachepoint、credits、ttl
  analyze     只分析已有结果文件（-out）

每个实验前后各拉一次 Get-Usage-Limits，核对 meteringEvent 之和与账户实际扣的差。
结果追加写到 -out（JSONL），结束时打印分析。会花该号的 credits。

flags:
`

func runProbe(p *pool.Pool, args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, probeUsage); fs.PrintDefaults() }
	account := fs.String("account", "", "account id (default: the first enabled account)")
	model := fs.String("model", "claude-sonnet-4.5", "Kiro model id")
	out := fs.String("out", "probe.jsonl", "results file (appended)")
	gaps := fs.String("gaps", "", "ttl gaps, comma separated (default 0,4m,6m,10m,15m,30m,45m,65m)")
	sizes := fs.String("sizes", "", "credits prefix sizes in tokens, comma separated (default 5000,20000,50000)")
	prefix := fs.Int("prefix", 0, "ttl / cachepoint prefix size in tokens (default 6000)")
	window := fs.Int("window", 0, "model context window for percentage->tokens (default 200000)")
	if len(args) == 0 {
		fs.Usage()
		return errors.New("probe: experiment required")
	}
	exp := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	if exp == "analyze" {
		return analyzeFile(*out)
	}

	opts := probe.Options{Prefix: *prefix}
	for _, g := range split(*gaps) {
		d, err := time.ParseDuration(g)
		if err != nil {
			return fmt.Errorf("-gaps: %w", err)
		}
		opts.Gaps = append(opts.Gaps, d)
	}
	for _, s := range split(*sizes) {
		n, err := strconv.Atoi(s)
		if err != nil || n <= 0 {
			return fmt.Errorf("-sizes: bad size %q", s)
		}
		opts.Sizes = append(opts.Sizes, n)
	}

	id := *account
	if id == "" {
		for _, v := range p.List() {
			if !v.Disabled {
				id = v.ID
				break
			}
		}
	}
	if _, ok := p.Get(id); !ok {
		return errors.New("probe: no usable account (add one, or pass -account)")
	}
	f, err := os.OpenFile(*out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r := &probe.Runner{
		T: probe.Target{
			Client: p.Client(), Model: *model, Window: *window,
			Cred: func(ctx context.Context) (kiro.Cred, error) { return p.Cred(ctx, id, "") },
		},
		Out: f,
		Log: func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) },
	}
	exps := []string{exp}
	if exp == "all" {
		exps = []string{"usage", "cachepoint", "credits", "continuation", "longctx", "ttl"}
	}
	fmt.Fprintf(os.Stderr, "probe: account %s model %s -> %s\n", id, *model, *out)
	for _, e := range exps {
		s, err := r.Run(ctx, e, opts)
		b, _ := json.Marshal(s)
		fmt.Fprintf(os.Stderr, "probe %s: %s\n", e, b)
		if err != nil {
			return err
		}
	}
	return analyzeFile(*out)
}

func analyzeFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	recs, err := probe.Read(f)
	if err != nil {
		return err
	}
	probe.Analyze(recs).Text(os.Stdout)
	return nil
}

func split(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
