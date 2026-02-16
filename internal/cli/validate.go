package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ashish1099/gitsync/pkg/config"
)

func newValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate-config",
		Short: "Validate the config file and exit",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("config validation: %w", err)
			}
			fmt.Println("Config is valid.")
			return nil
		},
	}
}
