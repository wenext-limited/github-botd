package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wenext-limited/github-botd/internal/config"
	"github.com/wenext-limited/github-botd/internal/gateway"
	gh "github.com/wenext-limited/github-botd/internal/github"
	"github.com/wenext-limited/github-botd/internal/store"
	"github.com/wenext-limited/github-botd/internal/transport"
)

// A generation is immutable after publication. Each HTTP request and outbox
// action uses a matching configuration, adapter and set of handlers.
type configGeneration struct {
	gateway *gateway.Gateway
	ipc     http.Handler
	webhook http.Handler
	retired context.Context
	cancel  context.CancelFunc
}

type daemonConfig struct {
	current atomic.Pointer[configGeneration]
}

func githubAdapter(c *config.Config) (gateway.GitHub, error) {
	if len(c.GitHub.Apps) == 0 && c.GitHub.TokenEnv == "" && c.GitHub.TokenFile == "" {
		return nil, nil
	}
	return gh.New(c.Auth(), gh.Options{BaseURL: c.GitHub.BaseURL})
}

func newDaemonConfig(g *gateway.Gateway) (*daemonConfig, error) {
	d := &daemonConfig{}
	if err := d.install(g); err != nil {
		return nil, err
	}
	return d, nil
}

// install is called by a single writer, after validation and adapter creation.
// No live state changes until all fallible preparation has succeeded.
func (d *daemonConfig) install(g *gateway.Gateway) error {
	old := d.current.Load()
	if old != nil {
		previous, next := old.gateway.Config, g.Config
		if previous.StateDir != next.StateDir || previous.IPC != next.IPC || previous.Webhook.Bind != next.Webhook.Bind {
			return fmt.Errorf("state_dir and listener addresses cannot change during reload; restart required")
		}
	}
	bindings := map[string]string{}
	for alias, repo := range g.Config.Repositories {
		bindings[alias] = fmt.Sprintf("%s:%d", strings.ToLower(repo.FullName), repo.InstallationID)
	}
	if err := g.Store.Reconfigure(bindings, store.Options{MaxAttempts: g.Config.Limits.MaxAttempts, MaxPending: g.Config.Limits.MaxPending}); err != nil {
		return err
	}
	retired, cancel := context.WithCancel(context.Background())
	next := &configGeneration{gateway: g, ipc: transport.IPC(g), webhook: transport.Webhook(g), retired: retired, cancel: cancel}
	d.current.Store(next)
	if old != nil {
		// Wake long-poll subscriptions so removed clients/grants cannot keep
		// claiming new deliveries for the remainder of an old wait window.
		old.cancel()
	}
	return nil
}

func (d *daemonConfig) gateway() *gateway.Gateway { return d.current.Load().gateway }

func (d *daemonConfig) handler(webhook bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		generation := d.current.Load()
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(generation.retired, cancel)
		defer stop()
		defer cancel()
		h := generation.ipc
		if webhook {
			h = generation.webhook
		}
		h.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (d *daemonConfig) reload(path string, force bool) (bool, error) {
	c, err := config.Load(path)
	if err != nil {
		return false, err
	}
	old := d.gateway()
	if !force && reflect.DeepEqual(old.Config, c) {
		return false, nil
	}
	adapter, err := githubAdapter(c)
	if err != nil {
		return false, err
	}
	if err = d.install(gateway.New(c, old.Store, adapter)); err != nil {
		return false, err
	}
	return true, nil
}

func (d *daemonConfig) watch(ctx context.Context, path string, signals <-chan os.Signal, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var lastError string
	for {
		force := false
		select {
		case <-ctx.Done():
			d.current.Load().cancel()
			return
		case <-tick.C:
		case <-signals:
			force = true
		}
		changed, err := d.reload(path, force || lastError != "")
		if err != nil {
			// A broken edit should not flood logs every second. Retry it on
			// every tick, so fixing the config or a secret recovers unaided.
			if err.Error() != lastError || force {
				slog.Error("configuration reload rejected; keeping active configuration", "error", err)
			}
			lastError = err.Error()
			continue
		}
		lastError = ""
		if changed {
			slog.Info("configuration reloaded", "dry_run", d.gateway().Config.DryRun)
		}
	}
}
