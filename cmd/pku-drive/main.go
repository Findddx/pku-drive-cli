package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/Findddx/pku-drive-cli/internal/cli"
	versionpkg "github.com/Findddx/pku-drive-cli/internal/version"
)

var (
	version   = "dev"
	commit    = "none"
	buildDate = "unknown"
)

func main() {
	syscall.Umask(0o077)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, wireDependencies(), versionpkg.Info{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
	})
	os.Exit(code)
}
