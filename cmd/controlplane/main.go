// Command controlplane serves the action catalog to GitHub Actions runners.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/cloudyhome/controlplane/internal/api"
	"github.com/cloudyhome/controlplane/internal/auth"
	"github.com/cloudyhome/controlplane/internal/config"
	"github.com/cloudyhome/controlplane/internal/github"
	"github.com/cloudyhome/controlplane/internal/jobs"
	"github.com/cloudyhome/controlplane/internal/source"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfgFile := flag.String("config", "/etc/controlplane/catalog.yml", "catalog file")
	listen := flag.String("listen", "127.0.0.1:8080", "listen address")
	dataDir := flag.String("data-dir", "/var/lib/controlplane", "state directory")
	devAuth := flag.Bool("insecure-dev-auth", false, "accept unsigned base64 JSON claims as tokens (loopback only)")
	check := flag.Bool("check", false, "validate the catalog and exit")
	flag.Parse()

	cfg, err := config.Load(*cfgFile)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if *check {
		fmt.Printf("%s: %d actions OK\n", *cfgFile, len(cfg.Actions))
		return nil
	}

	var verifier auth.Verifier
	if *devAuth {
		if !isLoopback(*listen) {
			return errors.New("-insecure-dev-auth requires a loopback listen address")
		}
		slog.Warn("INSECURE dev auth enabled: tokens are not verified")
		verifier = auth.Dev{AllowedOrg: cfg.Server.AllowedOrg}
	} else {
		verifier = auth.NewOIDC(context.Background(), cfg.Server.OIDCIssuer, cfg.Server.OIDCJWKSURL,
			cfg.Server.OIDCAudience, cfg.Server.AllowedOrg)
	}

	if err := os.MkdirAll(*dataDir, 0o750); err != nil {
		return err
	}
	store, err := jobs.OpenStore(filepath.Join(*dataDir, "controlplane.db"))
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer store.Close()
	if count, err := store.FailInterrupted(context.Background()); err != nil {
		return err
	} else if count > 0 {
		slog.Warn("marked interrupted jobs as failed", "count", count)
	}

	src := source.New(*dataDir, cfg.Repos)
	mgr := jobs.NewManager(cfg, store, src, *dataDir)
	srv := &http.Server{
		Addr: *listen,
		Handler: api.New(cfg, store, mgr, src, verifier, github.Client{
			APIURL: cfg.Server.GitHubAPIURL,
			Token:  os.Getenv(cfg.Server.GitHubTokenEnv),
		}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// Submit may fetch from git, so allow a generous write timeout.
		WriteTimeout: 3 * time.Minute,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", *listen, "actions", len(cfg.Actions))
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down; cancelling active jobs")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(shutdownCtx)
	mgr.Shutdown()
	return nil
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}
