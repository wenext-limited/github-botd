// github-botd is a local GitHub event/action gateway, not an agent runtime.
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
	"sync"
	"syscall"
	"time"

	"github.com/wenext-limited/github-botd/internal/config"
	"github.com/wenext-limited/github-botd/internal/gateway"
	"github.com/wenext-limited/github-botd/internal/store"
	"github.com/wenext-limited/github-botd/internal/transport"
)

var version = "0.1.0"

func main() {
	if err := run(); err != nil {
		slog.Error("gateway stopped", "error", err)
		os.Exit(1)
	}
}
func run() error {
	path := flag.String("config", "config/github-botd.json", "configuration file (paths resolve relative to this file)")
	check := flag.Bool("check", false, "validate config and local secret files without network calls")
	showVersion := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVersion {
		fmt.Println("github-botd", version)
		return nil
	}
	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	adapter, err := githubAdapter(c)
	if err != nil {
		return err
	}
	if *check {
		fmt.Println("configuration is valid; no GitHub connectivity verified")
		return nil
	}
	s, err := store.OpenWithOptions(c.StateDir, store.Options{MaxAttempts: c.Limits.MaxAttempts, MaxPending: c.Limits.MaxPending})
	if err != nil {
		return err
	}
	defer s.Close()
	dynamic, err := newDaemonConfig(gateway.New(c, s, adapter))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	reloadSignals := make(chan os.Signal, 1)
	signal.Notify(reloadSignals, syscall.SIGHUP)
	defer signal.Stop(reloadSignals)
	type endpoint struct {
		name     string
		listener net.Listener
		handler  http.Handler
	}
	var endpoints []endpoint
	// All listeners are opened before starting workers. Failure closes earlier listeners.
	cleanup := func() {
		for _, e := range endpoints {
			e.listener.Close()
		}
	}
	defer cleanup()
	if c.IPC.Socket != "" {
		ln, e := transport.ListenUnix(c.IPC.Socket)
		if e != nil {
			return e
		}
		endpoints = append(endpoints, endpoint{"ipc_unix", ln, dynamic.handler(false)})
	}
	if c.IPC.Bind != "" {
		ln, e := transport.ListenLoopback(c.IPC.Bind)
		if e != nil {
			return e
		}
		endpoints = append(endpoints, endpoint{"ipc_tcp", ln, dynamic.handler(false)})
	}
	if c.Webhook.Bind != "" {
		ln, e := net.Listen("tcp", c.Webhook.Bind)
		if e != nil {
			return e
		}
		endpoints = append(endpoints, endpoint{"webhook", ln, dynamic.handler(true)})
	}
	var servers []*http.Server
	var wg sync.WaitGroup
	fatal := make(chan error, len(endpoints)+2)
	for _, e := range endpoints {
		server := &http.Server{Handler: e.handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 65 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
		servers = append(servers, server)
		wg.Add(1)
		go func(ln net.Listener) {
			defer wg.Done()
			if e := server.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
				fatal <- e
			}
		}(e.listener)
		slog.Info("listener ready", "name", e.name, "address", e.listener.Addr().String())
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		if e := gateway.RunOutbox(ctx, dynamic.gateway); e != nil {
			fatal <- e
		}
	}()
	wg.Add(2)
	go func() {
		defer wg.Done()
		if e := gateway.RunPolling(ctx, dynamic.gateway); e != nil {
			fatal <- e
		}
	}()
	go func() {
		defer wg.Done()
		dynamic.watch(ctx, *path, reloadSignals, time.Second)
	}()
	slog.Info("github-botd ready", "version", version, "dry_run", c.DryRun, "github_connectivity", "not_live_verified")
	select {
	case <-ctx.Done():
	case err = <-fatal:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, server := range servers {
		if e := server.Shutdown(shutdown); e != nil {
			server.Close()
		}
	}
	wg.Wait()
	return err
}
