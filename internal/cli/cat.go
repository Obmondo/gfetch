package cli

import (
	"fmt"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newCatCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cat",
		Short: "Print the resolved configuration as YAML",
		Long:  "Loads the configuration (file or directory), applies global defaults, validates, and prints the fully resolved config to stdout.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return fmt.Errorf("loading config: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("validating config: %w", err)
			}
			out, err := yaml.Marshal(cfg)
			if err != nil {
				return fmt.Errorf("marshaling config: %w", err)
			}
			fmt.Print(string(out))
			return nil
		},
	}
}
