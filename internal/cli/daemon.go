package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/daemon"
	"github.com/obmondo/gfetch/pkg/sync"
)

func newDaemonCmd() *cobra.Command {
	var listenAddr string

	cmd := &cobra.Command{
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
			sched := daemon.NewScheduler(syncer, logger, listenAddr)
			sched.Run(context.Background(), cfg)
			return nil
		},
	}

	cmd.Flags().StringVar(&listenAddr, "listen-addr", ":8080", "Address for the HTTP server (health, metrics, sync endpoints)")

	return cmd
}
