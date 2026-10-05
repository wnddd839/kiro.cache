// kiro-go 是本地 Kiro 号池分发层：对外说 Anthropic Messages，对内说 Kiro。
//
//	kiro-go [-config kiro-go.json]                启动服务
//	kiro-go -config kiro-go.json import-ide        把 Kiro IDE 当前登录加入号池
//	kiro-go -config kiro-go.json login             浏览器登录 Kiro（Google / GitHub / Builder ID / IdC）并加入号池
//	kiro-go -config kiro-go.json list              列出号池
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

	"kiro-go/internal/config"
	"kiro-go/internal/kiro"
	"kiro-go/internal/pool"
	"kiro-go/internal/server"
)

// version 由构建注入：-ldflags "-X main.version=v1.2.3"。
var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "kiro-go:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "kiro-go.json", "config file (missing file = defaults)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("kiro-go", version)
		return nil
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		return fmt.Errorf("log_level: %w", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	p, err := pool.Open(cfg.AccountsFile, pool.Options{
		Client: kiro.NewClient(nil),
		Logger: log,
		PinTTL: time.Duration(cfg.SessionTTL),
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
	default:
		return fmt.Errorf("unknown command %q (serve | import-ide | login | list)", cmd)
	}
}

func serve(cfg config.Config, p *pool.Pool, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if host, _, err := net.SplitHostPort(cfg.Listen); err == nil && len(cfg.APIKeys) == 0 {
		if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback()) {
			log.Warn("listening beyond loopback with no api_keys: anyone on the network can spend the pool", "listen", cfg.Listen)
		}
	}

	srv, err := server.New(ctx, cfg, p, log)
	if err != nil {
		return err
	}
	if d := time.Duration(cfg.LimitsInterval); d > 0 {
		go p.PollLimits(ctx, d)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("kiro-go listening", "version", version, "addr", cfg.Listen, "accounts", p.Len(), "conversation_mode", cfg.ConversationMode)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
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
