package cli

import (
	"context"
	"time"

	"github.com/spf13/cobra"
	"github.com/windmilleng/tilt/internal/analytics"
	engineanalytics "github.com/windmilleng/tilt/internal/engine/analytics"
	"github.com/windmilleng/tilt/internal/tiltfile"
	"github.com/windmilleng/tilt/pkg/logger"
	"golang.org/x/sync/errgroup"
)

type ciCmd struct {
	fileName string
}

func (c *ciCmd) register() *cobra.Command {
	cmd := &cobra.Command{
		Use:                   "ci [-- <Tiltfile args>]",
		DisableFlagsInUseLine: true,
		Short:                 "start Tilt in ci mode with the given Tiltfile args",
		Long: `
Starts Tilt and runs services defined in the Tiltfile.
`,
	}
	cmd.Flags().StringVar(&c.fileName, "file", tiltfile.FileName, "Path to Tiltfile")
	// the other settings default to false, which is what we want

	return cmd
}

func (c *ciCmd) run(ctx context.Context, args []string) error {
	a := analytics.Get(ctx)
	deferred := logger.NewDeferredLogger(ctx)
	ctx = redirectLogs(ctx, deferred)

	cmdUpTags := engineanalytics.CmdUpTags(map[string]string{
		"mode": string(updateModeFlag),
	})
	a.Incr("cmd.up", cmdUpTags.AsMap())
	a.IncrIfUnopted("analytics.up.optdefault")

	defer a.Flush(time.Second)

	threads, err := wireCmdUp(ctx, false, a, cmdUpTags)
	if err != nil {
		deferred.SetOutput(deferred.Original())
		return err
	}

	g, ctx := errgroup.WithContext(ctx)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	upper := threads.upper

	g.Go(func() error {
		defer cancel()
		return upper.Start(ctx, args, threads.tiltBuild, false, c.fileName, false, a.UserOpt(), threads.token, string(threads.cloudAddress))
	})

	err = g.Wait()
	if err != context.Canceled {
		return err
	} else {
		return nil
	}

	return nil
}
