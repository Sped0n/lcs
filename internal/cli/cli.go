package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/spedon/lcs/internal/config"
	"github.com/spedon/lcs/internal/server"
	"github.com/spedon/lcs/internal/shell"
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: lcsctl {serve|shell|status|clear}")
	}
	switch args[0] {
	case "serve", "shell", "status", "clear":
	default:
		return errors.New("usage: lcsctl {serve|shell|status|clear}")
	}
	paths, err := config.ResolvePaths()
	if err != nil {
		return err
	}
	if args[0] == "serve" {
		flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		flags.StringVar(&paths.Config, "config", paths.Config, "configuration file")
		if err := flags.Parse(args[1:]); errors.Is(err, flag.ErrHelp) {
			_, err = fmt.Fprintf(stderr, "usage: lcsctl %s [--config path]\n", args[0])
			return err
		} else if err != nil {
			return fmt.Errorf("%s: %w", args[0], err)
		}
		if flags.NArg() != 0 {
			return fmt.Errorf("%s does not accept positional arguments", args[0])
		}
	} else if len(args) != 1 {
		return fmt.Errorf("%s does not accept arguments", args[0])
	}
	if args[0] == "status" || args[0] == "clear" {
		return server.Control(ctx, paths, args[0], stdout)
	}
	if args[0] == "shell" {
		c, err := server.ShellConfig(ctx, paths)
		if err != nil {
			return err
		}
		return shell.Generate(c, paths, stdout, stderr)
	}
	c, err := config.Load(paths.Config)
	if err != nil {
		return err
	}
	return server.Serve(ctx, c, paths)
}
