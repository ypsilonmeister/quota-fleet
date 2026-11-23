package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ypsilonmeister/quota-fleet/internal/api"
	"github.com/ypsilonmeister/quota-fleet/internal/config"
	"github.com/ypsilonmeister/quota-fleet/internal/store"
)

func runServe(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cfgPath string
		dbPath  string
		listen  string
	)
	fs.StringVar(&cfgPath, "config", resolveDefaultConfigPath(), "path to fleet.yaml")
	fs.StringVar(&cfgPath, "c", resolveDefaultConfigPath(), "path to fleet.yaml (shorthand)")
	fs.StringVar(&dbPath, "db", "", "override SQLite database path")
	fs.StringVar(&listen, "listen", "", "override HTTP listen address (default from api.listen or 127.0.0.1:8737)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		fmt.Fprintf(stderr, "serve failed: %v\n", err)
		return 1
	}
	if dbPath == "" {
		dbPath = cfg.DBPath()
	}
	if listen == "" {
		listen = cfg.API.ListenAddr()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, dbPath, store.Options{InstanceID: cfg.InstanceID})
	if err != nil {
		fmt.Fprintf(stderr, "open store %q: %v\n", dbPath, err)
		return 1
	}
	defer st.Close()

	if err := st.InitInstanceID(ctx, cfg.InstanceID); err != nil {
		fmt.Fprintf(stderr, "instance_id check failed: %v\n", err)
		return 1
	}

	logger := log.New(stderr, "quota-fleet: ", log.LstdFlags)
	apiSrv := api.NewServer(st, api.Options{
		InstanceID:  cfg.InstanceID,
		MaxClaimed:  cfg.Limits.MaxClaimed,
		Token:       cfg.API.Token,
		RequireAuth: cfg.API.RequireAuth,
		Logger:      logger,
	})

	httpSrv := &http.Server{
		Addr:              listen,
		Handler:           apiSrv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Fprintf(stdout, "quota-fleet API listening on %s (instance_id=%s, db=%s)\n", listen, cfg.InstanceID, dbPath)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutCtx)
		return 0
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintf(stderr, "serve error: %v\n", err)
			return 1
		}
		return 0
	}
}
