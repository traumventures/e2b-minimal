// Command e2b-minimal serves the minimal E2B API in front of one orchestrator.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/traumventures/e2b-minimal/internal/server"
	"github.com/traumventures/e2b-minimal/internal/store"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err := run(); err != nil {
		slog.Error("e2b-minimal stopped", "err", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}

	return def
}

func envInt(key string, def int64) int64 {
	v, err := strconv.ParseInt(env(key, ""), 10, 64)
	if err != nil {
		return def
	}

	return v
}

func run() error {
	cfg := server.Config{
		APIKey:             env("E2B_API_KEY", ""),
		PublicURL:          env("PUBLIC_URL", ""),
		Domain:             env("SANDBOX_DOMAIN", ""),
		OrchestratorGRPC:   env("ORCHESTRATOR_GRPC", "127.0.0.1:5008"),
		OrchestratorHTTP:   env("ORCHESTRATOR_HTTP", env("ORCHESTRATOR_GRPC", "127.0.0.1:5008")),
		OrchestratorProxy:  env("ORCHESTRATOR_PROXY", "127.0.0.1:5007"),
		MaxSandboxHours:    envInt("MAX_SANDBOX_HOURS", 24),
		BuildDiskMB:        envInt("BUILD_DISK_MB", 10240),
		DefaultFreeDiskMB:  envInt("DEFAULT_FREE_DISK_MB", 1024),
		DefaultCPUCount:    envInt("DEFAULT_CPU_COUNT", 2),
		DefaultMemoryMB:    envInt("DEFAULT_MEMORY_MB", 512),
		HugePages:          env("HUGE_PAGES", "true") == "true",
		CgroupRoot:         env("SANDBOX_CGROUP_ROOT", ""),
		MemoryMinMB:        envInt("SANDBOX_MEMORY_MIN", 0),
		ColdBoot:           env("COLD_BOOT", "false") == "true",
		BaseImage:          env("BASE_IMAGE", "e2bdev/base"),
		BootstrapBase:      env("BOOTSTRAP_BASE", "true") == "true",
		KernelVersion:      env("E2B_KERNEL_VERSION", ""),
		FirecrackerVersion: env("E2B_FIRECRACKER_VERSION", ""),
	}
	if len(cfg.APIKey) < 16 {
		return errors.New("E2B_API_KEY must be set (at least 16 characters, e.g. e2b_$(openssl rand -hex 20))")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	st, err := store.Open(ctx, store.Options{
		Dir:         env("DATA_DIR", "/var/lib/e2b-minimal"),
		DatabaseURL: env("DATABASE_URL", ""),
	})
	if err != nil {
		return err
	}
	defer st.Close()
	srv, err := server.New(cfg, st)
	if err != nil {
		return err
	}
	defer srv.Close()

	httpSrv := &http.Server{
		Addr:              env("LISTEN_ADDR", ":3000"),
		Handler:           srv,
		ReadHeaderTimeout: 30 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", httpSrv.Addr)
		errc <- httpSrv.ListenAndServe()
	}()

	go func() {
		if err := srv.WaitForOrchestrator(ctx); err != nil {
			return
		}
		slog.Info("orchestrator is up")
		if err := srv.Reconcile(ctx); err != nil {
			slog.Error("reconcile", "err", err)
		}
		srv.ResumeWatches()
		if cfg.BootstrapBase {
			if err := srv.BootstrapBase(ctx); err != nil {
				slog.Error("bootstrap base template", "err", err)
			}
		}
		go srv.RunMemoryWatchdog(ctx)
		srv.RunReaper(ctx)
	}()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return httpSrv.Shutdown(shutdown)
}
