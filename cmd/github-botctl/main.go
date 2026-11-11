// github-botctl is a gateway client. It never executes external commands.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/pkg/client"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	fs := flag.NewFlagSet("github-botctl", flag.ContinueOnError)
	endpoint := fs.String("endpoint", client.DefaultEndpoint, "unix:///absolute/socket or http://127.0.0.1:port")
	tokenEnv := fs.String("token-env", "", "gateway token environment variable (default GITHUB_BOTD_TOKEN)")
	tokenFile := fs.String("token-file", "", "gateway token file; never pass the token as an argument")
	input := fs.String("input", "-", "JSON input file, or - for stdin")
	timeout := fs.Duration("timeout", 35*time.Second, "request timeout")
	limit := fs.Int("limit", 1, "events batch size")
	wait := fs.Int("wait", 25, "events long-poll seconds")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: github-botctl [flags] info|status|events|deadletters|ack ID|replay ID|action|receipt ID|context\nFlags must precede the command. ack, action, context read JSON from -input or stdin.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return err
	}
	args := fs.Args()
	if len(args) == 0 {
		fs.Usage()
		return errors.New("command required")
	}
	cmd := args[0]
	want := 1
	switch cmd {
	case "ack", "replay", "receipt":
		want = 2
	case "info", "status", "events", "deadletters", "action", "context":
	default:
		return errors.New("unknown command")
	}
	if len(args) != want {
		return errors.New("wrong number of command arguments")
	}
	c, err := client.New(client.Config{Endpoint: *endpoint, TokenEnv: *tokenEnv, TokenFile: *tokenFile, Timeout: *timeout})
	if err != nil {
		return err
	}
	defer c.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var out any
	switch cmd {
	case "info":
		out, err = c.Info(ctx)
	case "status":
		out, err = c.Status(ctx)
	case "events":
		var ds []v1.Delivery
		ds, err = c.Events(ctx, *limit, *wait)
		out = struct {
			Deliveries []v1.Delivery `json:"deliveries"`
		}{ds}
	case "deadletters":
		var ds []v1.DeadLetter
		ds, err = c.DeadLetters(ctx, *limit)
		out = struct {
			Deliveries []v1.DeadLetter `json:"deliveries"`
		}{ds}
	case "ack":
		var a v1.AckRequest
		if err = readJSON(*input, &a); err == nil {
			err = c.Ack(ctx, args[1], a)
		}
		out = map[string]bool{"acknowledged": err == nil}
	case "replay":
		err = c.Replay(ctx, args[1])
		out = map[string]bool{"replayed": err == nil}
	case "action":
		var a v1.Action
		if err = readJSON(*input, &a); err == nil {
			out, err = c.Submit(ctx, a)
		}
	case "receipt":
		out, err = c.Receipt(ctx, args[1])
	case "context":
		var r v1.ContextRequest
		if err = readJSON(*input, &r); err == nil {
			out, err = c.Context(ctx, r)
		}
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
func readJSON(path string, out any) error {
	var r io.Reader = os.Stdin
	if path != "-" {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, 1024*1024+1))
	if err != nil {
		return err
	}
	if len(b) > 1024*1024 {
		return errors.New("JSON input exceeds 1 MiB")
	}
	return json.Unmarshal(b, out)
}
