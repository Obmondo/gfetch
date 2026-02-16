package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/ashish1099/gitsync/pkg/config"
	"github.com/ashish1099/gitsync/pkg/daemon"
	"github.com/ashish1099/gitsync/pkg/sync"
)

func newDaemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run as a foreground polling daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("config validation: %w", err)
			}

			logger := slog.Default()
			syncer := sync.New(logger)
			sched := daemon.NewScheduler(syncer, logger)
			sched.Run(context.Background(), cfg)
			return nil
		},
	}
}
