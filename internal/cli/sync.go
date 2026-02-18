package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/gsync"
)

func newSyncCmd() *cobra.Command {
	var repoName string
	var prune bool
	var pruneStale bool
	var staleAgeStr string
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "One-shot sync of all repos (or a specific repo)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("config validation: %w", err)
			}

			var staleAge time.Duration
			if staleAgeStr != "" {
				staleAge, err = config.ParseDuration(staleAgeStr)
				if err != nil {
					return fmt.Errorf("invalid stale-age: %w", err)
				}
			}

			s := gsync.New(slog.Default())
			ctx := context.Background()
			opts := gsync.SyncOptions{
				Prune:      prune,
				PruneStale: pruneStale,
				StaleAge:   staleAge,
				DryRun:     dryRun,
			}

			if repoName != "" {
				repo := findRepo(cfg, repoName)
				if repo == nil {
					return fmt.Errorf("repo %q not found in config", repoName)
				}
				result := s.SyncRepo(ctx, repo, opts)
				printResult(cmd, result, dryRun)
				if result.Err != nil {
					os.Exit(1)
				}
				return nil
			}

			results := s.SyncAll(ctx, cfg, opts)
			hasErr := false
			for _, r := range results {
				printResult(cmd, r, dryRun)
				if r.Err != nil {
					hasErr = true
				}
			}
			if hasErr {
				os.Exit(1)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoName, "repo", "", "sync a specific repo by name")
	cmd.Flags().BoolVar(&prune, "prune", false, "delete obsolete local branches and tags that no longer match any configured pattern")
	cmd.Flags().BoolVar(&pruneStale, "prune-stale", false, "delete local branches that match patterns but have no commits in the last 6 months (or custom stale-age)")
	cmd.Flags().StringVar(&staleAgeStr, "stale-age", "", "custom age threshold for stale pruning (e.g., 30d, 6m, 1y)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be pruned without deleting")
	return cmd
}

func findRepo(cfg *config.Config, name string) *config.RepoConfig {
	for i := range cfg.Repos {
		if cfg.Repos[i].Name == name {
			return &cfg.Repos[i]
		}
	}
	return nil
}

func printResult(cmd *cobra.Command, r gsync.Result, dryRun bool) {
	cmd.Printf("Repo: %s\n", r.RepoName)
	if len(r.BranchesSynced) > 0 {
		cmd.Printf("  Branches synced: %v\n", r.BranchesSynced)
	}
	if len(r.BranchesUpToDate) > 0 {
		cmd.Printf("  Branches up-to-date: %v\n", r.BranchesUpToDate)
	}
	if len(r.BranchesFailed) > 0 {
		cmd.Printf("  Branches failed: %v\n", r.BranchesFailed)
	}
	if len(r.BranchesObsolete) > 0 {
		cmd.Printf("  Branches obsolete: %v\n", r.BranchesObsolete)
	}
	if len(r.BranchesStale) > 0 {
		cmd.Printf("  Branches stale: %v\n", r.BranchesStale)
	}
	if len(r.BranchesPruned) > 0 {
		if dryRun {
			cmd.Printf("  Branches to prune: %v\n", r.BranchesPruned)
		} else {
			cmd.Printf("  Branches pruned: %v\n", r.BranchesPruned)
		}
	}
	if len(r.TagsFetched) > 0 {
		cmd.Printf("  Tags fetched: %v\n", r.TagsFetched)
	}
	if len(r.TagsUpToDate) > 0 {
		cmd.Printf("  Tags up-to-date: %v\n", r.TagsUpToDate)
	}
	if len(r.TagsObsolete) > 0 {
		cmd.Printf("  Tags obsolete: %v\n", r.TagsObsolete)
	}
	if len(r.TagsPruned) > 0 {
		if dryRun {
			cmd.Printf("  Tags to prune: %v\n", r.TagsPruned)
		} else {
			cmd.Printf("  Tags pruned: %v\n", r.TagsPruned)
		}
	}
	if r.Checkout != "" {
		cmd.Printf("  Checkout: %s\n", r.Checkout)
	}
	if r.Err != nil {
		cmd.Printf("  Error: %v\n", r.Err)
	}
}
