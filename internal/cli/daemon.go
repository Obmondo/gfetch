package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/daemon"
	"github.com/obmondo/gfetch/pkg/gsync"
)

func newDaemonCmd() *cobra.Command {
	var listenAddr string

	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run as a foreground polling daemon",
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("config validation: %w", err)
			}

			s := gsync.New()
			sched := daemon.NewScheduler(s, listenAddr)
			sched.Run(context.Background(), cfg)
			return nil
		},
	}

	cmd.Flags().StringVar(&listenAddr, "listen-addr", ":8080", "Address for the HTTP server (health, metrics, sync endpoints)")

	return cmd
}
