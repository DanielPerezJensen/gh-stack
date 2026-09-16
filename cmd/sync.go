package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cli/go-gh/v2/pkg/prompter"
	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/modify"
	"github.com/github/gh-stack/internal/stack"
	"github.com/spf13/cobra"
)

type syncOptions struct {
	remote    string
	prune     bool
	worktrees bool
}

func SyncCmd(cfg *config.Config) *cobra.Command {
	opts := &syncOptions{}

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync the current stack with the remote",
		Long: `Fetch, rebase, push, and sync PR state for the current stack.

This command performs a safe synchronization:

  1. Fetches the latest changes from the remote
  2. Reconciles the stack on GitHub with your local stack: pulls down
     branches for any PRs added to the stack on GitHub, or prompts you to
     resolve a divergence in an interactive terminal
  3. Fast-forwards the trunk branch to match the remote
  4. Cascade-rebases stack branches onto their updated parents
  5. Pushes all branches atomically (using --force-with-lease --atomic)
  6. Syncs PR state from GitHub
  7. Links the stack's open PRs into a stack on GitHub (creating or updating
     the remote stack object) when two or more PRs exist

If PRs have been added to the stack on GitHub, their branches are pulled
down and appended to your local stack so it mirrors the remote. A clean
"remote is ahead" update happens automatically without prompting. If the
local and remote stacks have diverged, sync prompts (in an interactive
terminal) to use the remote as the source of truth, delete the stack on
GitHub and recreate it later with sync/submit, or cancel. Cancelling — or a
divergence in a non-interactive terminal — aborts the sync without pushing
branches or updating PRs.

If a rebase conflict is detected, all branches are restored to their
original state and you are advised to run "gh stack rebase" to resolve
conflicts interactively.

Sync never opens pull requests — use "gh stack submit" for that. It only
links PRs that already exist. The final message reflects what happened:
"Stack synced" means the stack object on GitHub now matches your local
stack, while "Branches synced" means the branches were rebased and pushed
but no remote stack object was created or updated (for example, when fewer
than two PRs exist yet).

Use --prune to delete local branches for merged PRs. Stack metadata is
preserved so that rebase and display logic continue to work correctly.
If you are on a branch that would be pruned, your checkout is moved to
the first active branch in the stack, or the trunk if all are merged.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSync(cfg, opts)
		},
	}

	cmd.Flags().StringVar(&opts.remote, "remote", "", "Remote to fetch from and push to (defaults to auto-detected remote)")
	cmd.Flags().BoolVar(&opts.prune, "prune", false, "Delete local branches for merged PRs")
	cmd.Flags().BoolVarP(&opts.worktrees, "worktrees", "w", false, "Sync checked-out branches in their linked worktrees")

	return cmd
}

func runSync(cfg *config.Config, opts *syncOptions) error {
	result, err := loadStack(cfg, "")
	if err != nil {
		return ErrNotInStack
	}
	gitDir := result.GitDir

	if err := modify.CheckStateGuard(gitDir); err != nil {
		cfg.Errorf("%s", err)
		return ErrModifyRecovery
	}

	sf := result.StackFile
	s := result.Stack
	currentBranch := result.CurrentBranch
	worktrees, invokingWorktree, err := syncWorktreeContext(opts.worktrees)
	if err != nil {
		cfg.Errorf("%s", err)
		return ErrSilent
	}
	if err := syncRebaseRecoveryGuard(); err != nil {
		cfg.Errorf("%s", err)
		return ErrSilent
	}

	// Resolve remote once for fetch and push
	remote, err := pickRemote(cfg, currentBranch, opts.remote)
	if err != nil {
		if !errors.Is(err, errInterrupt) {
			cfg.Errorf("%s", err)
		}
		return ErrSilent
	}

	// --- Step 1: Fetch ---
	// Enable git rerere so conflict resolutions are remembered.
	if err := ensureRerere(cfg); errors.Is(err, errInterrupt) {
		return ErrSilent
	}

	// Fetch trunk + active branches so tracking refs are current for
	// fast-forward detection (Step 2) and --force-with-lease (Step 4).
	normalizeStackTrunk(cfg, s, remote)
	if err := git.FetchBranches(remote, activeBranchNames(s)); err != nil {
		cfg.Errorf("failed to fetch stack branches from %s: %v", remote, err)
		return ErrSilent
	}
	if opts.worktrees {
		if err := preflightSyncWorktrees(s, worktrees, invokingWorktree); err != nil {
			cfg.Errorf("Cannot sync: %s", err)
			return ErrSilent
		}
	} else if err := requireSyncWorktrees(s, currentBranch, worktrees, invokingWorktree); err != nil {
		cfg.Errorf("%s", err)
		return ErrSilent
	}

	// --- Step 1b: Reconcile remote-ahead stack changes ---
	// Pull in branches for PRs that were added to the stack on GitHub, or
	// resolve a divergence, before rebasing and pushing so pulled branches
	// participate in the normal flow. Best-effort for stacks tracked on the
	// remote; a no-op otherwise.
	reconcileRes, err := reconcileRemoteStack(cfg, sf, s, currentBranch, gitDir, remote)
	if err != nil {
		if errors.Is(err, errInterrupt) {
			return ErrSilent
		}
		return err
	}
	if reconcileRes.stack != nil {
		s = reconcileRes.stack
	}
	if reconcileRes.stop {
		// The reconcile step resolved the situation and there is nothing more to
		// do (the user cancelled or deleted the remote stack, or a divergence was
		// detected non-interactively). The resolving path already reported the
		// outcome, so just exit successfully.
		return nil
	}
	// Reconciling "use remote as source of truth" may have moved us off a
	// branch that is no longer in the stack, so re-read the current branch.
	if cb, cbErr := git.CurrentBranch(); cbErr == nil {
		currentBranch = cb
	}
	if opts.worktrees {
		// Reconciliation can add stack branches, so refresh owners and validate the
		// complete participating set before the first local ref update.
		worktrees, invokingWorktree, err = syncWorktreeContext(true)
		if err != nil {
			cfg.Errorf("%s", err)
			return ErrSilent
		}
		if err := preflightSyncWorktrees(s, worktrees, invokingWorktree); err != nil {
			cfg.Errorf("Cannot sync: %s", err)
			return ErrSilent
		}
	}

	// --- Step 2: Resolve trunk ---
	trunk, err := resolveTrunkTargetInWorktree(cfg, s, remote, currentBranch, worktrees[s.Trunk.Branch])
	if err != nil {
		return err
	}

	// --- Step 2b: Fast-forward stack branches behind their remote tracking branch ---
	var updatedBranches []string
	if opts.worktrees {
		updatedBranches = fastForwardWorktreeBranches(cfg, s, remote, currentBranch, worktrees)
	} else {
		updatedBranches = fastForwardBranches(cfg, s, remote, currentBranch)
	}

	// --- Step 3: Cascade rebase ---
	needsRebase := trunk.Moved || len(updatedBranches) > 0 || stackNeedsRebase(s, trunk.Ref)
	rebased := false
	var rebasedBranches []string
	var originalRefs map[string]string
	if needsRebase {
		cfg.Printf("")
		cfg.Printf("Rebasing stack ...")

		// Sync PR state to detect merged PRs before rebasing.
		_ = syncStackPRs(cfg, s)

		originalRefs, err = resolveOriginalRefs(s)
		if err != nil {
			cfg.Warningf("Could not resolve branch SHAs — skipping rebase: %v", err)
		} else {
			result := cascadeRebase(cascadeRebaseOpts{
				Cfg:          cfg,
				Stack:        s,
				Branches:     s.Branches,
				StartAbsIdx:  0,
				OriginalRefs: originalRefs,
				TrunkRef:     trunk.Ref,
				Worktrees:    worktrees,
				WorktreeMode: opts.worktrees,
			})

			if result.Err != nil {
				cfg.Errorf("%v", result.Err)
				if result.Rebased {
					if opts.worktrees {
						restoreErrors := restoreSyncWorktreeRefs(originalRefs, worktrees, result.RebasedBranches)
						_ = git.CheckoutBranch(currentBranch)
						reportRestoreStatus(cfg, restoreErrors)
					} else {
						restoreRebaseRefs(cfg, currentBranch, originalRefs)
					}
				} else {
					_ = git.CheckoutBranch(currentBranch)
				}
				stack.SaveNonBlocking(gitDir, sf)
				return ErrSilent
			}

			if result.Conflicted {
				// Abort and restore everything — sync is non-interactive.
				var restoreErrors []string
				abortFailed := false
				if path := worktrees[result.ConflictBranch]; path != "" && git.IsRebaseInProgressAt(path) {
					if err := git.RebaseAbortAt(path); err != nil {
						restoreErrors = append(restoreErrors, fmt.Sprintf("abort rebase of %s in %s: %s", result.ConflictBranch, path, err))
						abortFailed = true
					}
				} else if git.IsRebaseInProgress() {
					if err := git.RebaseAbort(); err != nil {
						restoreErrors = append(restoreErrors, fmt.Sprintf("abort rebase of %s: %s", result.ConflictBranch, err))
						abortFailed = true
					}
				}
				if opts.worktrees {
					restoreErrors = append(restoreErrors, restoreSyncWorktreeRefs(originalRefs, worktrees, result.RebasedBranches)...)
				} else {
					restoreErrors = restoreBranches(originalRefs)
				}
				_ = git.CheckoutBranch(currentBranch)

				cfg.Errorf("Conflict detected rebasing %s onto %s%s", result.ConflictBranch, result.ConflictBase, worktreeSuffix(worktrees[result.ConflictBranch]))
				reportRestoreStatus(cfg, restoreErrors)
				if abortFailed {
					cfg.Printf("  Abort the rebase manually%s with `%s` before retrying.", worktreeSuffix(worktrees[result.ConflictBranch]), cfg.ColorCyan("git rebase --abort"))
				} else if opts.worktrees {
					cfg.Printf("  Run `%s` to resolve conflicts interactively.", cfg.ColorCyan("gh stack rebase --worktrees"))
					cfg.Printf("  Then run `%s` again.", cfg.ColorCyan("gh stack sync --worktrees"))
				} else {
					cfg.Printf("  Run `%s` to resolve conflicts interactively.", cfg.ColorCyan("gh stack rebase"))
					cfg.Printf("  Then run `%s` again.", cfg.ColorCyan("gh stack sync"))
				}

				// Persist refreshed PR state even on conflict, then bail out
				// before pushing or reporting success.
				stack.SaveNonBlocking(gitDir, sf)
				return ErrConflict
			}

			if result.Rebased {
				rebased = true
				rebasedBranches = result.RebasedBranches
			}
		}
		if !opts.worktrees || hasUnownedBranch(s.Branches, worktrees) {
			_ = git.CheckoutBranch(currentBranch)
		}
	}

	if unstacked := verifyStacked(s, trunk.Ref, 0, len(s.Branches)); len(unstacked) > 0 {
		_ = git.CheckoutBranch(currentBranch)
		reportUnstacked(cfg, trunk.Ref, unstacked)
		if rebased && originalRefs != nil {
			if opts.worktrees {
				restoreErrors := restoreSyncWorktreeRefs(originalRefs, worktrees, rebasedBranches)
				reportRestoreStatus(cfg, restoreErrors)
			} else {
				restoreRebaseRefs(cfg, currentBranch, originalRefs)
			}
		}
		stack.SaveNonBlocking(gitDir, sf)
		return ErrSilent
	}

	// --- Step 4: Push ---
	cfg.Printf("")
	branches := activeBranchNames(s)

	if mergedCount := len(s.MergedBranches()); mergedCount > 0 {
		cfg.Printf("Skipping %d merged %s", mergedCount, plural(mergedCount, "branch", "branches"))
	}
	if queuedCount := len(s.QueuedBranches()); queuedCount > 0 {
		cfg.Printf("Skipping %d queued %s", queuedCount, plural(queuedCount, "branch", "branches"))
	}

	if len(branches) == 0 {
		cfg.Printf("No active branches to push (all merged)")
	} else {
		// After rebase, force-with-lease is required (history rewritten).
		// Without rebase, try a normal push first.
		force := rebased
		cfg.Printf("Pushing %d %s to %s...", len(branches), plural(len(branches), "branch", "branches"), remote)
		if err := git.Push(remote, branches, force, true); err != nil {
			if !force {
				cfg.Warningf("Push failed — branches may need force push after rebase")
				cfg.Printf("  Run `%s` to push with --force-with-lease.",
					cfg.ColorCyan("gh stack push"))
			} else {
				cfg.Warningf("Push failed: %v", err)
				cfg.Printf("  Run `%s` to retry.", cfg.ColorCyan("gh stack push"))
			}
		} else {
			cfg.Successf("Pushed %d branches", len(branches))
		}
	}

	// --- Step 5: Sync PR state ---
	cfg.Printf("")
	cfg.Printf("Syncing PRs ...")
	_ = syncStackPRs(cfg, s)

	// Report PR status for each branch
	for _, b := range s.Branches {
		if b.IsMerged() {
			continue
		}
		if b.IsQueued() {
			cfg.Successf("PR %s (%s) — Queued", cfg.PRLink(b.PullRequest.Number, b.PullRequest.URL), b.Branch)
			continue
		}
		if b.PullRequest != nil {
			cfg.Successf("PR %s (%s) — Open", cfg.PRLink(b.PullRequest.Number, b.PullRequest.URL), b.Branch)
		} else {
			cfg.Warningf("%s has no PR", b.Branch)
		}
	}
	merged := s.MergedBranches()
	if len(merged) > 0 {
		names := make([]string, len(merged))
		for i, m := range merged {
			if m.PullRequest != nil {
				names[i] = fmt.Sprintf("#%d", m.PullRequest.Number)
			} else {
				names[i] = m.Branch
			}
		}
		cfg.Printf("Merged: %s", strings.Join(names, ", "))
	}

	// --- Step 5b: Reconcile the remote stack object ---
	// syncStackPRs above only refreshes local PR associations; it does not touch
	// the stack object on GitHub. When the branches have open PRs, link them into
	// a stack so the remote reflects the local stack. This never opens PRs — that
	// is still `gh stack submit`'s job. stackSynced records whether the remote
	// stack object actually reflects the local stack, which determines the final
	// summary message below.
	stackSynced := false
	if client, err := cfg.GitHubClient(); err == nil {
		stackSynced = syncStack(cfg, client, s)
	}

	// --- Step 6: Prune merged branches (optional) ---
	doPrune := opts.prune
	if !doPrune {
		// --prune was not provided. If interactive, prompt.
		merged := s.MergedBranches()
		var prunableCount int
		for _, b := range merged {
			if git.BranchExists(b.Branch) && (!opts.worktrees || worktrees[b.Branch] == "") {
				prunableCount++
			}
		}
		if prunableCount > 0 && cfg.IsInteractive() {
			prompt := fmt.Sprintf("Prune %d merged %s?",
				prunableCount, plural(prunableCount, "branch", "branches"))
			confirmed, err := confirmPrune(cfg, prompt, true)
			if err != nil {
				if isInterruptError(err) {
					printInterrupt(cfg)
					// Save state before exiting so PR sync isn't lost.
					_ = stack.Save(gitDir, sf)
					return ErrSilent
				}
				// On any other prompt error, skip pruning silently.
			} else {
				doPrune = confirmed
			}
		}
	}

	if doPrune {
		merged := s.MergedBranches()
		var prunable []string
		for _, b := range merged {
			if git.BranchExists(b.Branch) && (!opts.worktrees || worktrees[b.Branch] == "") {
				prunable = append(prunable, b.Branch)
			} else if opts.worktrees && worktrees[b.Branch] != "" {
				cfg.Printf("Skipping %s (checked out at %s)", b.Branch, worktrees[b.Branch])
			}
		}

		if len(prunable) > 0 {
			// If the current branch is being pruned, switch away first.
			needsSwitch := false
			for _, name := range prunable {
				if name == currentBranch {
					needsSwitch = true
					break
				}
			}
			if needsSwitch {
				switchTarget := trunk.Branch
				for _, b := range s.Branches {
					if !b.IsSkipped() {
						switchTarget = b.Branch
						break
					}
				}
				if err := git.CheckoutBranch(switchTarget); err != nil {
					cfg.Warningf("Failed to switch from %s to %s: %v", currentBranch, switchTarget, err)
				} else {
					currentBranch = switchTarget
				}
			}

			cfg.Printf("")
			pruned := 0
			for _, name := range prunable {
				if err := git.DeleteBranch(name, true); err != nil {
					cfg.Warningf("Failed to delete %s: %v", name, err)
				} else {
					cfg.Successf("Pruned %s (merged)", name)
					pruned++
				}
			}
			if pruned > 0 {
				cfg.Successf("Pruned %d merged %s", pruned, plural(pruned, "branch", "branches"))
			}
		} else if opts.prune {
			cfg.Printf("")
			cfg.Printf("No merged branches to prune")
		}

		// Clean up remote-tracking refs for all merged branches, even if
		// the local branch was already deleted. This prevents
		// `git checkout <name>` from resurrecting the branch.
		for _, b := range merged {
			if !opts.worktrees || worktrees[b.Branch] == "" {
				_ = git.DeleteTrackingRef(remote, b.Branch)
			}
		}
	}

	// --- Step 7: Update base SHAs and save ---
	updateBaseSHAs(s)

	if err := stack.Save(gitDir, sf); err != nil {
		return handleSaveError(cfg, err)
	}

	cfg.Printf("")
	if stackSynced {
		cfg.Successf("Stack synced")
	} else {
		// The branches were fetched, rebased, and pushed, but no stack object on
		// GitHub was created or updated (no PRs, fewer than two PRs, stacked PRs
		// unavailable, or a divergence). Report only what actually happened.
		cfg.Successf("Branches synced")
	}
	cfg.Printf("  Stacked on %s", trunk.Describe())
	return nil
}

// restoreBranches resets each branch to its original SHA, collecting any errors.
func restoreBranches(originalRefs map[string]string) []string {
	var errors []string
	for branch, sha := range originalRefs {
		if !git.BranchExists(branch) {
			continue
		}
		if currentSHA, err := git.RevParse(branch); err == nil && currentSHA == sha {
			continue
		}
		if err := git.CheckoutBranch(branch); err != nil {
			errors = append(errors, fmt.Sprintf("checkout %s: %s", branch, err))
			continue
		}
		if err := git.ResetHard(sha); err != nil {
			errors = append(errors, fmt.Sprintf("reset %s: %s", branch, err))
		}
	}
	return errors
}

func restoreRebaseRefs(cfg *config.Config, originalBranch string, originalRefs map[string]string) {
	restoreErrors := restoreBranches(originalRefs)
	_ = git.CheckoutBranch(originalBranch)
	reportRestoreStatus(cfg, restoreErrors)
}

// reportRestoreStatus prints whether branch restoration succeeded or partially failed.
func reportRestoreStatus(cfg *config.Config, restoreErrors []string) {
	if len(restoreErrors) > 0 {
		cfg.Warningf("Some branches could not be fully restored:")
		for _, e := range restoreErrors {
			cfg.Printf("  %s", e)
		}
	} else {
		cfg.Printf("  All branches restored to their original state.")
	}
}

// short returns the first 7 characters of a SHA.
func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// confirmPrune asks the user to confirm pruning via ConfirmFn or a terminal prompt.
func confirmPrune(cfg *config.Config, prompt string, defaultValue bool) (bool, error) {
	if cfg.ConfirmFn != nil {
		return cfg.ConfirmFn(prompt, defaultValue)
	}
	p := prompter.New(cfg.In, cfg.Out, cfg.Err)
	return p.Confirm(prompt, defaultValue)
}

func syncWorktreeContext(worktreeMode bool) (map[string]string, string, error) {
	if !worktreeMode {
		return nil, "", nil
	}
	if err := git.RequireRebaseNoUpdateRefs(); err != nil {
		return nil, "", err
	}
	worktrees, err := git.Worktrees()
	if err != nil {
		return nil, "", fmt.Errorf("listing worktrees: %w", err)
	}
	invokingWorktree, err := git.WorktreePath()
	if err != nil {
		return nil, "", fmt.Errorf("resolving current worktree: %w", err)
	}
	return worktrees, invokingWorktree, nil
}

func syncRebaseRecoveryGuard() error {
	if !git.UsingDefaultOps() {
		return nil
	}
	commonDir, err := git.CommonDir()
	if err != nil {
		return fmt.Errorf("checking rebase recovery state: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(commonDir, rebaseStateFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking rebase recovery state: %w", err)
	}
	var state rebaseState
	if err := json.Unmarshal(data, &state); err != nil {
		return fmt.Errorf("checking rebase recovery state: %w", err)
	}
	if state.WorktreeMode {
		return fmt.Errorf("a worktree rebase is awaiting recovery; run `gh stack rebase --continue` or `gh stack rebase --abort` first")
	}
	return nil
}

func preflightSyncWorktrees(s *stack.Stack, worktrees map[string]string, invokingWorktree string) error {
	branches := append([]stack.BranchRef{}, s.ActiveBranches()...)
	branches = append(branches, s.Trunk)
	return preflightWorktreeBranches(branches, worktrees, invokingWorktree)
}

func requireSyncWorktrees(s *stack.Stack, currentBranch string, _ map[string]string, invokingWorktree string) error {
	if !git.UsingDefaultOps() {
		return nil
	}
	discovered, err := git.Worktrees()
	if err != nil {
		return fmt.Errorf("listing worktrees: %w", err)
	}
	for _, branch := range append(s.ActiveBranches(), s.Trunk) {
		if path := discovered[branch.Branch]; path != "" && path != invokingWorktree && branch.Branch != currentBranch {
			return fmt.Errorf("branch %s is checked out at %s; re-run with `%s` to sync branches in their owning worktrees", branch.Branch, path, "gh stack sync --worktrees")
		}
	}
	return nil
}

func restoreSyncWorktreeRefs(originalRefs, worktrees map[string]string, branches []string) []string {
	var restoreErrors []string
	for _, branch := range branches {
		sha, ok := originalRefs[branch]
		if !ok {
			continue
		}
		if path := worktrees[branch]; path != "" {
			if err := git.ResetHardAt(path, sha); err != nil {
				restoreErrors = append(restoreErrors, fmt.Sprintf("reset %s in %s: %s", branch, path, err))
			}
			continue
		}
		if err := git.CheckoutBranch(branch); err != nil {
			restoreErrors = append(restoreErrors, fmt.Sprintf("checkout %s: %s", branch, err))
			continue
		}
		if err := git.ResetHard(sha); err != nil {
			restoreErrors = append(restoreErrors, fmt.Sprintf("reset %s: %s", branch, err))
		}
	}
	return restoreErrors
}
