// kiro-proxy 是本地 Kiro 号池分发层：对外说 Anthropic Messages，对内说 Kiro。
//
//	kiro-proxy [-config kiro-proxy.json]                启动服务
//	kiro-proxy -config kiro-proxy.json import-ide        把 Kiro IDE 当前登录加入号池
//	kiro-proxy -config kiro-proxy.json login             浏览器登录 Kiro（Google / GitHub / Builder ID / IdC）并加入号池
//	kiro-proxy -config kiro-proxy.json list              列出号池
//	kiro-proxy -config kiro-proxy.json probe <exp>       探测 Kiro 缓存 / 计费行为（usage | ttl | cachepoint | credits | all | analyze）
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"kiro-proxy/internal/config"
	"kiro-proxy/internal/keys"
	"kiro-proxy/internal/kiro"
	"kiro-proxy/internal/pool"
	"kiro-proxy/internal/server"
	"kiro-proxy/internal/usage"
)

// version 由构建注入：-ldflags "-X main.version=v1.2.3"。
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kiro-proxy:", err)
		os.Exit(1)
	}
}

// legacyConfig 是改名前的默认配置文件名。
const legacyConfig = "kiro-go.json"

func run() error {
	cfgPath := flag.String("config", "kiro-proxy.json", "config file (missing file = defaults)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("kiro-proxy", version)
		return nil
	}

	path, legacy := configPath(*cfgPath, isFlagSet("config"))
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	if legacy {
		log.Warn("using legacy config file; rename it to kiro-proxy.json", "file", path)
	}

	client := kiro.NewClient(nil)
	client.Identity = cfg.Identity
	p, err := pool.Open(cfg.AccountsFile, pool.Options{
		Client:        client,
		Logger:        log,
		PinTTL:        time.Duration(cfg.SessionTTL),
		BreakerWindow: time.Duration(cfg.BreakerWindow),
		MaxConcurrent: cfg.MaxConcurrent,
	})
	if err != nil {
		return err
	}

	switch cmd := flag.Arg(0); cmd {
	case "", "serve":
		return serve(cfg, p, log)
	case "import-ide":
		return importIDE(p)
	case "login":
		return login(p)
	case "list":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(p.List())
	case "probe":
		return runProbe(p, flag.Args()[1:])
	default:
		return fmt.Errorf("unknown command %q (serve | import-ide | login | list | probe)", cmd)
	}
}

func serve(cfg config.Config, p *pool.Pool, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if host, _, err := net.SplitHostPort(cfg.Listen); err == nil && len(cfg.APIKeys) == 0 && !keysConfigured(cfg.KeysFile) {
		if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback()) {
			log.Warn("listening beyond loopback with no api_keys: anyone on the network can spend the pool", "listen", cfg.Listen)
		}
	}

	if host, _, err := net.SplitHostPort(cfg.Listen); err == nil && cfg.AdminToken == "" {
		if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback()) {
			log.Warn("listening beyond loopback with no admin_token: /admin only answers requests from this machine", "listen", cfg.Listen)
		}
	}

	ks, err := keys.Open(cfg.KeysFile, nil)
	if err != nil {
		return fmt.Errorf("keys: %w", err)
	}
	journal, err := usage.Open(cfg.UsageFile, usage.Options{Retention: time.Duration(cfg.UsageRetention)})
	if err != nil {
		return fmt.Errorf("usage: %w", err)
	}
	defer func() {
		if err := journal.Close(); err != nil {
			log.Error("usage journal close", "err", err)
		}
	}()

	// bg 是后台任务（缓存落盘、价格同步、额度轮询）的生命周期：等 HTTP 服务收尾后才结束，
	// 这样收尾期间跑完的请求仍能记账，最后一次缓存落盘看得到它们。
	bg, stopBG := context.WithCancel(context.Background())
	srv, err := server.New(bg, cfg, p, log, server.Stores{Keys: ks, Usage: journal})
	if err != nil {
		stopBG()
		return err
	}
	srv.Version = version
	if d := time.Duration(cfg.LimitsInterval); d > 0 {
		go p.PollLimits(bg, d)
	}

	// 请求不继承信号 ctx：Ctrl-C 时在途的流不会被立刻取消（那会记成中断的半截账），
	// 而是由 Shutdown 等它们跑完。
	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("kiro-proxy listening", "version", version, "addr", cfg.Listen, "accounts", p.Len(), "conversation_mode", cfg.ConversationMode,
		"console", "http://"+consoleHost(cfg.Listen)+"/")

	select {
	case err := <-errc:
		stopBG()
		srv.Wait()
		return err
	case <-ctx.Done():
	}
	stop() // 再按一次 Ctrl-C 直接退出
	log.Info("shutting down: waiting for in-flight requests", "timeout", shutdownGrace)
	shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	err = httpSrv.Shutdown(shutdown)
	if errors.Is(err, context.DeadlineExceeded) {
		log.Warn("shutdown grace expired; closing remaining connections")
		_ = httpSrv.Close() // 流被断开：按中断记账
	}
	// HTTP 已收尾：停后台任务，等缓存 / 会话最后一次落盘；账本在 defer 里刷盘关闭
	stopBG()
	srv.Wait()
	if err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}

// shutdownGrace 是退出时等在途请求跑完的上限。长 thinking 的流可能要一两分钟。
const shutdownGrace = 2 * time.Minute

// consoleHost 把监听地址变成浏览器能打开的 host:port：0.0.0.0 / :: / 空 → 127.0.0.1。
func consoleHost(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

func importIDE(p *pool.Pool) error {
	path, err := kiro.IDETokenPath()
	if err != nil {
		return err
	}
	cred, err := kiro.ReadIDE(path)
	if err != nil {
		return err
	}
	a, err := p.Add(pool.Account{Label: "kiro-ide", Source: pool.SourceIDE, SourcePath: path, Cred: cred})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	l, err := p.RefreshLimits(ctx, a.ID)
	if err != nil {
		fmt.Printf("added %s (usage check failed: %v)\n", a.ID, err)
		return nil
	}
	fmt.Printf("added %s  %s  %s  %.1f/%.0f credits\n", a.ID, l.Email, l.Plan, l.Used, l.Limit)
	return nil
}

func login(p *pool.Pool) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	s := &kiro.SignIn{
		Client: p.Client(),
		Open: func(u string) error {
			fmt.Printf("sign in to Kiro in your browser:\n%s\n", u)
			return kiro.OpenBrowser(u)
		},
	}
	l, err := s.Run(ctx)
	if err != nil {
		return err
	}
	label := l.Email
	if label == "" {
		label = "kiro"
	}
	a, err := p.Add(pool.Account{Label: label, Cred: l.Cred})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	lim, err := p.RefreshLimits(ctx, a.ID)
	if err != nil {
		fmt.Printf("added %s (usage check failed: %v)\n", a.ID, err)
		return nil
	}
	fmt.Printf("added %s  %s  %s  %.1f/%.0f credits\n", a.ID, lim.Email, lim.Plan, lim.Used, lim.Limit)
	return nil
}

// keysConfigured 报告 key 库里是否有 key：有就会校验下游。
func keysConfigured(path string) bool {
	ks, err := keys.Open(path, nil)
	return err == nil && ks.Len() > 0
}

// configPath 在没显式指定 -config、新文件不存在而旧名 kiro-go.json 存在时改用旧名。
func configPath(path string, explicit bool) (string, bool) {
	if explicit || fileExists(path) || !fileExists(legacyConfig) {
		return path, false
	}
	return legacyConfig, true
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func isFlagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}
