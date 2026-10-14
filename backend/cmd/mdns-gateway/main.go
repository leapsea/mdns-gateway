package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mdns-gateway/internal/api"
	"mdns-gateway/internal/auth"
	"mdns-gateway/internal/config"
	"mdns-gateway/internal/database"
	"mdns-gateway/internal/mdns"
	"mdns-gateway/internal/openresty"
	"mdns-gateway/internal/sites"
)

//go:embed web/*
var webFiles embed.FS

func main() {
	flag.Parse()
	cfg := config.Load()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	ctx := context.Background()
	db, err := database.Open(ctx, cfg.DataDir)
	if err != nil {
		log.Error("database", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	authService, err := auth.New(db, cfg.DataDir, cfg.SessionTTL)
	if err != nil {
		log.Error("auth", "error", err)
		os.Exit(1)
	}
	if flag.NArg() == 3 && flag.Arg(0) == "user" && flag.Arg(1) == "reset-2fa" {
		reset(ctx, authService, flag.Arg(2))
		return
	}
	mdnsService, err := mdns.New(cfg.GatewayIPv4, cfg.MDNSProbeTimeout)
	if err != nil {
		log.Error("mdns", "error", err)
		os.Exit(1)
	}
	proxy := openresty.New(cfg.DataDir+"/openresty", cfg.OpenRestyBin)
	if err := proxy.Bootstrap(ctx); err != nil {
		log.Error("openresty startup", "error", err)
		os.Exit(1)
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if err := mdnsService.Start(runCtx); err != nil {
		log.Error("mdns startup", "error", err)
		_ = proxy.Stop(ctx)
		os.Exit(1)
	}
	siteService := sites.NewService(db, mdnsService, proxy, cfg.ListenAddr)
	if err := siteService.Reconcile(ctx); err != nil {
		log.Error("site restore", "error", err)
		os.Exit(1)
	}
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		log.Error("assets", "error", err)
		os.Exit(1)
	}
	handler := http.FileServer(http.FS(static))
	app := &api.Server{Auth: authService, Sites: siteService, MDNS: mdnsService, Proxy: proxy, DB: db, UI: handler, Log: log}
	server := &http.Server{Addr: cfg.ListenAddr, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		defer func() {
			if v := recover(); v != nil {
				log.Error("http panic", "value", v)
			}
		}()
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http server", "error", err)
			cancel()
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
	case <-runCtx.Done():
	}
	shutdownCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
	defer done()
	_ = server.Shutdown(shutdownCtx)
	cancel()
	_ = mdnsService.Stop()
	_ = proxy.Stop(shutdownCtx)
}

func reset(ctx context.Context, service *auth.Service, username string) {
	fmt.Printf("Reset TOTP for user %q? This invalidates the current TOTP. Continue? [y/N] ", username)
	var answer string
	_, _ = fmt.Scanln(&answer)
	if strings.ToLower(answer) != "y" {
		return
	}
	if err := service.Reset2FA(ctx, username); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("TOTP binding removed. The next login must bind a new authenticator.")
}
