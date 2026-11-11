// mock-client logs normalized event metadata and ACKs it. It never submits actions.
package main

import (
	"context"
	"flag"
	"fmt"
	v1 "github.com/wenext-limited/github-botd/api/v1"
	"github.com/wenext-limited/github-botd/pkg/client"
	"log"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	endpoint := flag.String("endpoint", client.DefaultEndpoint, "local gateway endpoint")
	flag.Parse()
	c, err := client.New(client.Config{Endpoint: *endpoint})
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err = c.Subscribe(ctx, func(_ context.Context, d v1.Delivery) error {
		fmt.Printf("event=%q type=%q repo=%q attempt=%d; ACK\n", d.Event.ID, d.Event.Type, d.Event.Repository, d.Attempt)
		return nil
	})
	if err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}
