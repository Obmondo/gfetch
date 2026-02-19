package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
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
	cmd.Printf("Repo: %s%s\n", r.RepoName, getSummary(r))

	printSection(cmd, "Branches", []statusLine{
		{"✓", "synced", r.BranchesSynced, false},
		{"!", "failed", r.BranchesFailed, false},
		{"-", "up-to-date", r.BranchesUpToDate, true},
		{"!", "obsolete", r.BranchesObsolete, false},
		{"-", "stale", r.BranchesStale, true},
		{
			getPruneSymbol(dryRun),
			getPruneLabel(dryRun),
			r.BranchesPruned,
			false,
		},
	})

	printSection(cmd, "Tags", []statusLine{
		{"✓", "fetched", r.TagsFetched, false},
		{"!", "failed", r.TagsFailed, false},
		{"-", "up-to-date", r.TagsUpToDate, true},
		{"!", "obsolete", r.TagsObsolete, false},
		{
			getPruneSymbol(dryRun),
			getPruneLabel(dryRun),
			r.TagsPruned,
			false,
		},
	})

	if r.Checkout != "" {
		cmd.Printf("  ✓ Checkout: %s\n", r.Checkout)
	}
	if r.Err != nil {
		cmd.Printf("  ! Error: %v\n", r.Err)
	}
}

type statusLine struct {
	symbol  string
	label   string
	items   []string
	isQuiet bool
}

func getSummary(r gsync.Result) string {
	branchSuccess := len(r.BranchesSynced) + len(r.BranchesUpToDate)
	branchTotal := branchSuccess + len(r.BranchesFailed)
	tagSuccess := len(r.TagsFetched) + len(r.TagsUpToDate)
	tagTotal := tagSuccess + len(r.TagsFailed)

	if branchTotal > 0 && tagTotal > 0 {
		return fmt.Sprintf(" [%d/%d branches, %d/%d tags]", branchSuccess, branchTotal, tagSuccess, tagTotal)
	}
	if branchTotal > 0 {
		return fmt.Sprintf(" [%d/%d branches]", branchSuccess, branchTotal)
	}
	if tagTotal > 0 {
		return fmt.Sprintf(" [%d/%d tags]", tagSuccess, tagTotal)
	}
	return ""
}

func printSection(cmd *cobra.Command, title string, lines []statusLine) {
	hasContent := false
	for _, line := range lines {
		if len(line.items) > 0 {
			hasContent = true
			break
		}
	}

	if !hasContent {
		return
	}

	cmd.Printf("  %s:\n", title)
	for _, line := range lines {
		if len(line.items) == 0 {
			continue
		}
		content := strings.Join(line.items, ", ")
		if line.isQuiet && len(line.items) > 5 {
			content = fmt.Sprintf("%d items", len(line.items))
		}
		cmd.Printf("    %s %s: %s\n", line.symbol, line.label, content)
	}
}

func getPruneSymbol(dryRun bool) string {
	if dryRun {
		return "!"
	}
	return "✓"
}

func getPruneLabel(dryRun bool) string {
	if dryRun {
		return "to prune (dry-run)"
	}
	return "pruned"
}
