package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	_ "time/tzdata" // embed IANA timezone database so IANA names work in scratch containers

	"github.com/maniack/tgaia/internal/command"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := command.Run(ctx, os.Args, os.Stdout, os.Stderr); err != nil {
		os.Exit(1)
	}
}
