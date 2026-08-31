package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gemini-fly/etcd-studio/internal/app"
	"github.com/gemini-fly/etcd-studio/internal/auth"
	"github.com/gemini-fly/etcd-studio/internal/config"
	"github.com/gemini-fly/etcd-studio/internal/store"
)

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{}))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	clusters, err := store.NewRegistry(cfg.ClustersFile, cfg.DialTimeout)
	if err != nil {
		logger.Error("initialize cluster registry", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := clusters.Close(); err != nil {
			logger.Warn("close etcd clients", "error", err)
		}
	}()
	history, err := store.NewHistoryManager(cfg.HistoryConfigFile, cfg.HistoryFile, cfg.DialTimeout)
	if err != nil {
		logger.Error("initialize local value history", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := history.Close(); err != nil {
			logger.Warn("close local value history", "error", err)
		}
	}()
	authentication, err := auth.NewManager(cfg.AuthFile, cfg.DialTimeout)
	if err != nil {
		logger.Error("initialize authentication", "error", err)
		os.Exit(1)
	}
	listener, err := net.Listen("tcp", cfg.ListenAddress)
	if err != nil {
		logger.Error("listen for HTTP connections", "listen", cfg.ListenAddress, "error", err)
		os.Exit(1)
	}
	defer listener.Close()
	temporaryPassword, temporaryAdminCreated, err := authentication.EnsureTemporaryAdmin()
	if err != nil {
		logger.Error("create temporary administrator", "error", err)
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           app.NewServer(clusters, history, logger, authentication).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdownSignal, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownSignal.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err)
		}
	}()

	logger.Info("etcd studio started", "listen", cfg.ListenAddress, "clusters_file", cfg.ClustersFile, "history_configured", history.Status().Configured, "auth_configured", authentication.Status(nil).Configured, "clusters", len(clusters.ListClusters()))
	if temporaryAdminCreated {
		fmt.Fprintln(os.Stdout, "")
		fmt.Fprintln(os.Stdout, "============================================================")
		fmt.Fprintln(os.Stdout, "Etcd Studio 首次启动临时管理员")
		fmt.Fprintln(os.Stdout, "用户名: admin")
		fmt.Fprintf(os.Stdout, "临时密码: %s\n", temporaryPassword)
		fmt.Fprintln(os.Stdout, "该密码只显示一次，首次登录后必须立即设置新的强密码。")
		fmt.Fprintln(os.Stdout, "============================================================")
		fmt.Fprintln(os.Stdout, "")
	}
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("HTTP server stopped", "error", err)
		os.Exit(1)
	}
}
