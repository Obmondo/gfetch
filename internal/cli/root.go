package cli

import (
	"log/slog"
	"os"

	"github.com/spf13/cobra"
)

var (
	configPath string
	logLevel   string
)

// NewRootCmd creates the root command.
func NewRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "gfetch",
		Short: "Sync git repositories based on a YAML config",
		PersistentPreRun: func(_ *cobra.Command, _ []string) {
			setupLogger(logLevel)
		},
	}

	root.PersistentFlags().StringVarP(&configPath, "config", "c", "config.yaml", "path to config file or directory")
	root.PersistentFlags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")

	root.AddCommand(newSyncCmd())
	root.AddCommand(newDaemonCmd())
	root.AddCommand(newValidateCmd())
	root.AddCommand(newVersionCmd())
	root.AddCommand(newCatCmd())

	return root
}

func setupLogger(level string) {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	slog.SetDefault(slog.New(handler))
}
