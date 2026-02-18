package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/obmondo/gfetch/pkg/config"
	"github.com/obmondo/gfetch/pkg/sync"
)

func newSyncCmd() *cobra.Command {
	var repoName string
	var prune bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "One-shot sync of all repos (or a specific repo)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(configPath)
			if err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("config validation: %w", err)
			}

			syncer := sync.New(slog.Default())
			ctx := context.Background()
			opts := sync.SyncOptions{Prune: prune, DryRun: dryRun}

			if repoName != "" {
				repo := findRepo(cfg, repoName)
				if repo == nil {
					return fmt.Errorf("repo %q not found in config", repoName)
				}
				result := syncer.SyncRepo(ctx, repo, opts)
				printResult(result, dryRun)
				if result.Err != nil {
					os.Exit(1)
				}
				return nil
			}

			results := syncer.SyncAll(ctx, cfg, opts)
			hasErr := false
			for _, r := range results {
				printResult(r, dryRun)
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

func printResult(r sync.Result, dryRun bool) {
	fmt.Printf("Repo: %s\n", r.RepoName)
	if len(r.BranchesSynced) > 0 {
		fmt.Printf("  Branches synced: %v\n", r.BranchesSynced)
	}
	if len(r.BranchesUpToDate) > 0 {
		fmt.Printf("  Branches up-to-date: %v\n", r.BranchesUpToDate)
	}
	if len(r.BranchesFailed) > 0 {
		fmt.Printf("  Branches failed: %v\n", r.BranchesFailed)
	}
	if len(r.BranchesObsolete) > 0 {
		fmt.Printf("  Branches obsolete: %v\n", r.BranchesObsolete)
	}
	if len(r.BranchesPruned) > 0 {
		if dryRun {
			fmt.Printf("  Branches to prune: %v\n", r.BranchesPruned)
		} else {
			fmt.Printf("  Branches pruned: %v\n", r.BranchesPruned)
		}
	}
	if len(r.TagsFetched) > 0 {
		fmt.Printf("  Tags fetched: %v\n", r.TagsFetched)
	}
	if len(r.TagsUpToDate) > 0 {
		fmt.Printf("  Tags up-to-date: %v\n", r.TagsUpToDate)
	}
	if len(r.TagsObsolete) > 0 {
		fmt.Printf("  Tags obsolete: %v\n", r.TagsObsolete)
	}
	if len(r.TagsPruned) > 0 {
		if dryRun {
			fmt.Printf("  Tags to prune: %v\n", r.TagsPruned)
		} else {
			fmt.Printf("  Tags pruned: %v\n", r.TagsPruned)
		}
	}
	if r.Checkout != "" {
		fmt.Printf("  Checkout: %s\n", r.Checkout)
	}
	if r.Err != nil {
		fmt.Printf("  Error: %v\n", r.Err)
	}
}
