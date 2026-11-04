package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/maniack/tgaia/internal/command"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := command.Run(ctx, os.Args, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}
