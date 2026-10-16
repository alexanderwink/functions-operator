// Command fetcher is the init container that downloads the bootstrap binary.
package main

import (
	"context"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/alexanderwink/functions-operator/internal/fetcher"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := fetcher.Run(ctx, os.Getenv, runtime.GOARCH, os.Stderr)
	stop()
	os.Exit(code)
}
