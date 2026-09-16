package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/github/gh-stack/internal/config"
	"github.com/github/gh-stack/internal/git"
	"github.com/github/gh-stack/internal/modify"
	"github.com/github/gh-stack/internal/stack"
	"github.com/spf13/cobra"
)

type rebaseOptions struct {
	branch                    string
	downstack                 bool
	upstack                   bool
	cont                      bool
	abort                     bool
	noTrunk                   bool
	worktrees                 bool
	remote                    string
	committerDateIsAuthorDate bool
}

type rebaseState struct {
	CurrentBranchIndex        int               `json:"currentBranchIndex"`
	ConflictBranch            string            `json:"conflictBranch"`
	RemainingBranches         []string          `json:"remainingBranches"`
	OriginalBranch            string            `json:"originalBranch"`
	OriginalRefs              map[string]string `json:"originalRefs"`
	UseOnto                   bool              `json:"useOnto,omitempty"`
	OntoOldBase               string            `json:"ontoOldBase,omitempty"`
	CommitterDateIsAuthorDate bool              `json:"committerDateIsAuthorDate,omitempty"`
	NoTrunk                   bool              `json:"noTrunk,omitempty"`
	TrunkRef                  string            `json:"trunkRef,omitempty"`
	TrunkSHA                  string            `json:"trunkSha,omitempty"`
	StartIndex                int               `json:"startIndex,omitempty"`
	EndIndex                  int               `json:"endIndex,omitempty"`
	Branches                  []string          `json:"branches,omitempty"`
	WorktreeMode              bool              `json:"worktreeMode,omitempty"`
	Worktrees                 map[string]string `json:"worktrees,omitempty"`
	InvokingWorktree          string            `json:"invokingWorktree,omitempty"`
}

const rebaseStateFile = "gh-stack-rebase-state"

func RebaseCmd(cfg *config.Config) *cobra.Command {
	opts := &rebaseOptions{}

	cmd := &cobra.Command{
		Use:   "rebase [branch]",
		Short: "Rebase a stack of branches",
		Long: `Pull from remote and do a cascading rebase across the stack.

Ensures that each branch in the stack has the tip of the previous
layer in its commit history, rebasing if necessary.

Use --no-trunk to skip fetching and rebasing with the trunk branch.
Only the inter-branch rebases are performed (branch 2 onto branch 1,
branch 3 onto branch 2, etc.).`,
		Example: `  # Rebase the entire stack
  $ gh stack rebase

  # Only rebase from trunk to the current branch
  $ gh stack rebase --downstack

  # Only rebase from current branch to the top
  $ gh stack rebase --upstack

  # Rebase stack branches without pulling from or rebasing with trunk
  $ gh stack rebase --no-trunk

  # Continue after resolving conflicts
  $ gh stack rebase --continue

  # Abort and restore all branches
  $ gh stack rebase --abort`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				opts.branch = args[0]
			}
			return runRebase(cfg, opts)
		},
	}

	cmd.Flags().BoolVar(&opts.downstack, "downstack", false, "Only rebase branches from trunk to current branch")
	cmd.Flags().BoolVar(&opts.upstack, "upstack", false, "Only rebase branches from current branch to top")
	cmd.Flags().BoolVar(&opts.noTrunk, "no-trunk", false, "Skip trunk — only rebase stack branches onto each other")
	cmd.Flags().BoolVarP(&opts.worktrees, "worktrees", "w", false, "Rebase checked-out branches in their linked worktrees")
	cmd.Flags().BoolVar(&opts.cont, "continue", false, "Continue rebase after resolving conflicts")
	cmd.Flags().BoolVar(&opts.abort, "abort", false, "Abort rebase and restore all branches")
	cmd.Flags().StringVar(&opts.remote, "remote", "", "Remote to fetch from (defaults to auto-detected remote)")
	cmd.Flags().BoolVar(&opts.committerDateIsAuthorDate, "committer-date-is-author-date", false, "Set the committer date to the author date during rebase")
	cmd.Flags().BoolVar(&opts.committerDateIsAuthorDate, "preserve-dates", false, "Alias for --committer-date-is-author-date")

	return cmd
}

func runRebase(cfg *config.Config, opts *rebaseOptions) error {
	gitDir, err := git.GitDir()
	if err != nil {
		cfg.Errorf("not a git repository")
		return ErrNotInStack
	}

	if opts.cont {
		return continueRebase(cfg, gitDir)
	}

	if opts.abort {
		return abortRebase(cfg, gitDir)
	}

	if err := modify.CheckStateGuard(gitDir); err != nil {
		cfg.Errorf("%s", err)
		return ErrModifyRecovery
	}

	result, err := loadStack(cfg, opts.branch)
	if err != nil {
		return ErrNotInStack
	}
	sf := result.StackFile
	s := result.Stack
	currentBranch := result.CurrentBranch
	gitDir = result.GitDir

	currentIdx := s.IndexOf(currentBranch)
	if currentIdx < 0 {
		currentIdx = 0
	}

	if opts.upstack && currentIdx >= 0 && s.Branches[currentIdx].IsMerged() {
		cfg.Warningf("Current branch %q has already been merged", currentBranch)
	}

	startIdx := 0
	endIdx := len(s.Branches)
	if opts.downstack {
		endIdx = currentIdx + 1
	}
	if opts.upstack {
		startIdx = currentIdx
	}
	if opts.noTrunk && startIdx < 1 {
		startIdx = 1
	}
	branchesToRebase := s.Branches[startIdx:endIdx]
	if len(branchesToRebase) == 0 {
		cfg.Printf("No branches to rebase")
		return nil
	}

	ownedWorktrees := make(map[string]string)
	invokingWorktree := ""
	if opts.worktrees {
		if err := git.RequireRebaseNoUpdateRefs(); err != nil {
			cfg.Errorf("%s", err)
			return ErrSilent
		}
		worktrees, err := git.Worktrees()
		if err != nil {
			return fmt.Errorf("listing worktrees: %w", err)
		}
		invokingWorktree, err = git.WorktreePath()
		if err != nil {
			return fmt.Errorf("resolving current worktree: %w", err)
		}
		for _, br := range branchesToRebase {
			ownedWorktrees[br.Branch] = worktrees[br.Branch]
		}

		// Fast-forwarding can move any active branch, including trunk. Check all
		// affected worktrees before moving a checked-out ref.
		preflightBranches := append([]stack.BranchRef{}, branchesToRebase...)
		if !opts.noTrunk {
			ownedWorktrees[s.Trunk.Branch] = worktrees[s.Trunk.Branch]
			preflightBranches = append(preflightBranches, s.ActiveBranches()...)
			for _, br := range s.ActiveBranches() {
				ownedWorktrees[br.Branch] = worktrees[br.Branch]
			}
			preflightBranches = append(preflightBranches, s.Trunk)
		}
		if err := preflightWorktreeBranches(preflightBranches, worktrees, invokingWorktree); err != nil {
			cfg.Errorf("Cannot rebase: %s", err)
			return ErrSilent
		}
	}

	// Enable git rerere so conflict resolutions are remembered.
	if err := ensureRerere(cfg); errors.Is(err, errInterrupt) {
		return ErrSilent
	}

	var trunk trunkTarget
	var remote string
	if !opts.noTrunk {
		remote, err = pickRemote(cfg, currentBranch, opts.remote)
		if err != nil {
			if !errors.Is(err, errInterrupt) {
				cfg.Errorf("%s", err)
			}
			return ErrSilent
		}
		trunk, err = resolveTrunkTargetInWorktree(cfg, s, remote, currentBranch, ownedWorktrees[s.Trunk.Branch])
		if err != nil {
			return err
		}
		if err := git.FetchBranches(remote, activeBranchNames(s)); err != nil {
			cfg.Errorf("failed to fetch stack branches from %s: %v", remote, err)
			return ErrSilent
		}
		if !opts.worktrees {
			fastForwardBranches(cfg, s, remote, currentBranch)
		}
	}

	if !opts.worktrees && git.UsingDefaultOps() {
		worktrees, err := git.Worktrees()
		if err != nil {
			return fmt.Errorf("listing worktrees: %w", err)
		}
		for _, br := range branchesToRebase {
			if path := worktrees[br.Branch]; path != "" && br.Branch != currentBranch {
				cfg.Errorf("Branch %s is checked out at %s", br.Branch, path)
				cfg.Printf("Re-run with `%s` to rebase branches in their owning worktrees.", cfg.ColorCyan("gh stack rebase --worktrees"))
				return ErrSilent
			}
		}
	}
	if opts.worktrees && !opts.noTrunk {
		fastForwardWorktreeBranches(cfg, s, remote, currentBranch, ownedWorktrees)
	}

	cfg.Printf("Stack detected: %s", s.DisplayChain())

	cfg.Printf("Rebasing branches in order, starting from %s to %s",
		branchesToRebase[0].Branch, branchesToRebase[len(branchesToRebase)-1].Branch)

	// Sync PR state before rebase so we can detect merged PRs.
	_ = syncStackPRs(cfg, s)

	originalRefs, err := resolveOriginalRefs(s)
	if err != nil {
		return fmt.Errorf("resolving branch refs: %w", err)
	}

	// Get --onto state from a merged branch immediately below the rebase range.
	// Ensures that when --upstack excludes merged branches, we still check the
	// immediate predecessor and use --onto if needed.
	needsOnto := false
	var ontoOldBase string
	if startIdx > 0 {
		prev := s.Branches[startIdx-1]
		if prev.IsMerged() {
			if sha, ok := originalRefs[prev.Branch]; ok {
				needsOnto = true
				ontoOldBase = sha
			}
		}
	}

	rebaseResult := cascadeRebase(cascadeRebaseOpts{
		Cfg:                       cfg,
		Stack:                     s,
		Branches:                  branchesToRebase,
		StartAbsIdx:               startIdx,
		OriginalRefs:              originalRefs,
		NeedsOnto:                 needsOnto,
		OntoOldBase:               ontoOldBase,
		CommitterDateIsAuthorDate: opts.committerDateIsAuthorDate,
		TrunkRef:                  trunk.Ref,
		Worktrees:                 ownedWorktrees,
		WorktreeMode:              opts.worktrees,
	})

	if rebaseResult.Err != nil {
		cfg.Errorf("%v", rebaseResult.Err)
		if rebaseResult.Rebased {
			if opts.worktrees {
				restoreRebaseRefsAt(cfg, originalRefs, ownedWorktrees, rebaseResult.RebasedBranches)
			} else {
				restoreRebaseRefs(cfg, currentBranch, originalRefs)
			}
		} else {
			_ = git.CheckoutBranch(currentBranch)
		}
		return ErrSilent
	}

	if rebaseResult.Conflicted {
		cfg.Warningf("Rebasing %s onto %s — conflict", rebaseResult.ConflictBranch, rebaseResult.ConflictBase)

		state := &rebaseState{
			CurrentBranchIndex:        rebaseResult.ConflictIdx,
			ConflictBranch:            rebaseResult.ConflictBranch,
			RemainingBranches:         rebaseResult.Remaining,
			OriginalBranch:            currentBranch,
			OriginalRefs:              originalRefs,
			UseOnto:                   rebaseResult.NeedsOnto,
			OntoOldBase:               rebaseResult.OntoOldBase,
			CommitterDateIsAuthorDate: opts.committerDateIsAuthorDate,
			NoTrunk:                   opts.noTrunk,
			TrunkRef:                  trunk.Ref,
			TrunkSHA:                  trunk.SHA,
			StartIndex:                startIdx,
			EndIndex:                  endIdx,
			Branches:                  branchNames(branchesToRebase),
			WorktreeMode:              opts.worktrees,
			Worktrees:                 ownedWorktrees,
			InvokingWorktree:          invokingWorktree,
		}
		if err := saveRebaseState(rebaseStateDir(gitDir, opts.worktrees), state); err != nil {
			cfg.Warningf("failed to save rebase state: %s", err)
		}

		printConflictDetailsAt(cfg, rebaseResult.ConflictBase, state.operationWorktreeFor(rebaseResult.ConflictBranch))
		cfg.Printf("")

		cfg.Printf("Resolve conflicts on %s%s, then run `%s`",
			rebaseResult.ConflictBranch, worktreeSuffix(state.operationWorktreeFor(rebaseResult.ConflictBranch)), cfg.ColorCyan("gh stack rebase --continue"))
		cfg.Printf("Or abort this operation with `%s`",
			cfg.ColorCyan("gh stack rebase --abort"))
		return ErrConflict
	}

	if !opts.worktrees || hasUnownedBranch(branchesToRebase, ownedWorktrees) {
		_ = git.CheckoutBranch(currentBranch)
	}

	if unstacked := verifyStacked(s, trunk.Ref, startIdx, endIdx); len(unstacked) > 0 {
		reportUnstacked(cfg, trunk.Ref, unstacked)
		if rebaseResult.Rebased {
			if opts.worktrees {
				restoreRebaseRefsAt(cfg, originalRefs, ownedWorktrees, rebaseResult.RebasedBranches)
			} else {
				restoreRebaseRefs(cfg, currentBranch, originalRefs)
			}
		}
		return ErrSilent
	}

	updateBaseSHAs(s)

	_ = syncStackPRs(cfg, s)

	stack.SaveNonBlocking(gitDir, sf)

	merged := s.MergedBranches()
	if len(merged) > 0 {
		names := make([]string, len(merged))
		for i, m := range merged {
			names[i] = m.Branch
		}
		cfg.Printf("Skipped %d merged %s: %s", len(merged), plural(len(merged), "branch", "branches"), strings.Join(names, ", "))
	}

	rangeDesc := "All branches in stack"
	if opts.downstack {
		rangeDesc = fmt.Sprintf("All downstack branches up to %s", currentBranch)
	} else if opts.upstack {
		rangeDesc = fmt.Sprintf("All upstack branches from %s", currentBranch)
	}

	if opts.noTrunk {
		cfg.Printf("%s rebased locally (without trunk)", rangeDesc)
	} else {
		cfg.Printf("%s rebased locally with %s", rangeDesc, trunk.Describe())
	}
	cfg.Printf("To push up your changes, run `%s`",
		cfg.ColorCyan("gh stack push"))

	return nil
}

func rebaseStateDir(gitDir string, worktrees bool) string {
	if !worktrees {
		return gitDir
	}
	commonDir, err := git.CommonDir()
	if err != nil {
		return gitDir
	}
	return commonDir
}

func continueRebase(cfg *config.Config, gitDir string) error {
	state, err := loadRebaseState(gitDir)
	if err != nil {
		commonDir, commonErr := git.CommonDir()
		if commonErr == nil && commonDir != gitDir {
			state, err = loadRebaseState(commonDir)
			if err == nil && state.WorktreeMode {
				gitDir = commonDir
			} else if err == nil {
				err = os.ErrNotExist
			}
		}
	}
	if err != nil {
		cfg.Errorf("no rebase in progress")
		return ErrSilent
	}

	sf, err := stack.Load(gitDir)
	if err != nil {
		cfg.Errorf("failed to load stack state: %s", err)
		return ErrNotInStack
	}

	// Use the saved original branch to find the stack, since git may be in
	// a detached HEAD state during an active rebase.
	s, err := resolveStack(sf, state.OriginalBranch, cfg)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("no stack found for branch %s", state.OriginalBranch)
	}
	trunkRef := state.TrunkRef
	if trunkRef == "" {
		trunkRef = s.Trunk.Branch
	}
	trunkBase := state.TrunkSHA
	if trunkBase == "" {
		trunkBase = trunkRef
	}

	// Refresh PR state before selecting the base and cascading the remaining
	// branches. The queued flag is transient (not persisted), so it was lost
	// when the stack was reloaded from disk above. Without this, a queued
	// branch in the remaining cascade would be treated as active and its
	// frozen merge-queue branch would be rebased. Mirrors the syncStackPRs
	// call in runRebase before its cascade.
	_ = syncStackPRs(cfg, s)

	// The branch that had the conflict is stored in state; fall back to
	// looking it up by index for backwards compatibility with older state files.
	conflictBranch := state.ConflictBranch
	if conflictBranch == "" && state.CurrentBranchIndex >= 0 && state.CurrentBranchIndex < len(s.Branches) {
		conflictBranch = s.Branches[state.CurrentBranchIndex].Branch
	}

	cfg.Printf("Continuing rebase of stack, resuming from %s to %s",
		conflictBranch, s.Branches[len(s.Branches)-1].Branch)

	if state.WorktreeMode {
		if err := preflightWorktrees(state, state.RemainingBranches); err != nil {
			return err
		}
	}

	if path := state.operationWorktreeFor(conflictBranch); path != "" && git.IsRebaseInProgressAt(path) {
		rebaseOpts := git.RebaseOpts{CommitterDateIsAuthorDate: state.CommitterDateIsAuthorDate, NoUpdateRefs: true}
		if err := git.RebaseContinueAt(path, rebaseOpts); err != nil {
			return fmt.Errorf("rebase continue failed in worktree at %s — resolve remaining conflicts and try again: %w", path, err)
		}
	} else if git.IsRebaseInProgress() {
		rebaseOpts := git.RebaseOpts{CommitterDateIsAuthorDate: state.CommitterDateIsAuthorDate}
		if err := git.RebaseContinue(rebaseOpts); err != nil {
			return fmt.Errorf("rebase continue failed — resolve remaining conflicts and try again: %w", err)
		}
	}

	var baseBranch string
	if state.UseOnto {
		// The --onto path targets the first non-merged ancestor, or trunk.
		baseBranch = trunkRef
		for j := state.CurrentBranchIndex - 1; j >= 0; j-- {
			if !s.Branches[j].IsMerged() {
				baseBranch = s.Branches[j].Branch
				break
			}
		}
	} else if state.CurrentBranchIndex > 0 {
		baseBranch = s.Branches[state.CurrentBranchIndex-1].Branch
	} else {
		baseBranch = trunkRef
	}
	cfg.Successf("Rebased %s onto %s", conflictBranch, baseBranch)

	// Rebase remaining branches using the shared cascade helper.
	if len(state.RemainingBranches) > 0 {
		// Validate all remaining branches still exist in the stack,
		// are in contiguous ascending order, and build the BranchRef slice.
		remainingRefs := make([]stack.BranchRef, 0, len(state.RemainingBranches))
		startAbsIdx := -1
		for i, name := range state.RemainingBranches {
			idx := s.IndexOf(name)
			if idx < 0 {
				return fmt.Errorf("branch %q from saved rebase state is no longer in the stack — the stack may have been modified since the rebase started; consider aborting with --abort", name)
			}
			if startAbsIdx < 0 {
				startAbsIdx = idx
			} else if idx != startAbsIdx+i {
				return fmt.Errorf("branch %q is at stack index %d, expected %d — the stack may have been reordered since the rebase started; consider aborting with --abort", name, idx, startAbsIdx+i)
			}
			remainingRefs = append(remainingRefs, s.Branches[idx])
		}

		result := cascadeRebase(cascadeRebaseOpts{
			Cfg:                       cfg,
			Stack:                     s,
			Branches:                  remainingRefs,
			StartAbsIdx:               startAbsIdx,
			OriginalRefs:              state.OriginalRefs,
			NeedsOnto:                 state.UseOnto,
			OntoOldBase:               state.OntoOldBase,
			CommitterDateIsAuthorDate: state.CommitterDateIsAuthorDate,
			TrunkRef:                  trunkBase,
			Worktrees:                 state.Worktrees,
			WorktreeMode:              state.WorktreeMode,
		})

		if result.Err != nil {
			cfg.Errorf("%v", result.Err)
			if state.WorktreeMode {
				state.RemainingBranches = state.RemainingBranches[len(result.RebasedBranches):]
				if err := saveRebaseState(gitDir, state); err != nil {
					cfg.Warningf("failed to save rebase state: %s", err)
				}
				return ErrSilent
			} else {
				restoreRebaseRefs(cfg, state.OriginalBranch, state.OriginalRefs)
				clearRebaseState(gitDir)
			}
			return ErrSilent
		}

		if result.Conflicted {
			cfg.Warningf("Rebasing %s onto %s — conflict", result.ConflictBranch, result.ConflictBase)

			state.CurrentBranchIndex = result.ConflictIdx
			state.ConflictBranch = result.ConflictBranch
			state.RemainingBranches = result.Remaining
			state.UseOnto = result.NeedsOnto
			state.OntoOldBase = result.OntoOldBase
			if err := saveRebaseState(gitDir, state); err != nil {
				cfg.Warningf("failed to save rebase state: %s", err)
			}

			printConflictDetailsAt(cfg, result.ConflictBase, state.operationWorktreeFor(result.ConflictBranch))
			cfg.Printf("")
			cfg.Printf("Resolve conflicts on %s%s, then run `%s`",
				result.ConflictBranch, worktreeSuffix(state.operationWorktreeFor(result.ConflictBranch)), cfg.ColorCyan("gh stack rebase --continue"))
			cfg.Printf("Or abort this operation with `%s`",
				cfg.ColorCyan("gh stack rebase --abort"))
			return ErrConflict
		}
	}

	if !state.WorktreeMode || hasUnownedBranchNames(state.branches(), state.Worktrees) {
		_ = git.CheckoutBranch(state.OriginalBranch)
	}

	verifyStart, verifyEnd := state.StartIndex, state.EndIndex
	if verifyEnd <= verifyStart {
		verifyStart, verifyEnd = 0, len(s.Branches)
		if state.NoTrunk {
			verifyStart = 1
		}
	}
	if unstacked := verifyStacked(s, trunkBase, verifyStart, verifyEnd); len(unstacked) > 0 {
		reportUnstacked(cfg, trunkRef, unstacked)
		if state.WorktreeMode {
			restoreRebaseRefsAt(cfg, state.OriginalRefs, state.Worktrees, state.branches())
		} else {
			restoreRebaseRefs(cfg, state.OriginalBranch, state.OriginalRefs)
		}
		clearRebaseState(gitDir)
		return ErrSilent
	}

	clearRebaseState(gitDir)
	updateBaseSHAs(s)

	_ = syncStackPRs(cfg, s)

	stack.SaveNonBlocking(gitDir, sf)

	if state.NoTrunk {
		cfg.Printf("All branches in stack rebased locally (without trunk)")
	} else if state.TrunkSHA != "" {
		cfg.Printf("All branches in stack rebased locally with %s (%s)", trunkRef, short(state.TrunkSHA))
	} else {
		cfg.Printf("All branches in stack rebased locally with %s", trunkRef)
	}
	cfg.Printf("To push up your changes and open/update the stack of PRs, run `%s`",
		cfg.ColorCyan("gh stack submit"))

	return nil
}

func abortRebase(cfg *config.Config, gitDir string) error {
	state, err := loadRebaseState(gitDir)
	if err != nil {
		commonDir, commonErr := git.CommonDir()
		if commonErr == nil && commonDir != gitDir {
			state, err = loadRebaseState(commonDir)
			if err == nil && state.WorktreeMode {
				gitDir = commonDir
			} else if err == nil {
				err = os.ErrNotExist
			}
		}
	}
	if err != nil {
		cfg.Errorf("no rebase in progress")
		return ErrSilent
	}

	if path := state.operationWorktreeFor(state.ConflictBranch); path != "" && git.IsRebaseInProgressAt(path) {
		_ = git.RebaseAbortAt(path)
	} else if git.IsRebaseInProgress() {
		_ = git.RebaseAbort()
	}

	var restoreErrors []string
	branches := state.branches()
	for _, branch := range branches {
		sha := state.OriginalRefs[branch]
		if path := state.Worktrees[branch]; path != "" {
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

	if len(restoreErrors) > 0 {
		cfg.Warningf("Rebase aborted but some branches could not be fully restored:")
		for _, e := range restoreErrors {
			cfg.Printf("  %s", e)
		}
		return ErrSilent
	}

	if !state.WorktreeMode {
		_ = git.CheckoutBranch(state.OriginalBranch)
	}
	clearRebaseState(gitDir)

	cfg.Successf("Rebase aborted and branches restored")
	return nil
}

func (s *rebaseState) branches() []string {
	if len(s.Branches) > 0 {
		return s.Branches
	}
	branches := make([]string, 0, len(s.OriginalRefs))
	for branch := range s.OriginalRefs {
		branches = append(branches, branch)
	}
	return branches
}

func (s *rebaseState) worktreeFor(branch string) string {
	return s.Worktrees[branch]
}

// operationWorktreeFor locates an active rebase. Unowned branches still
// rebase through checkout in the invoking worktree, but are never reset there.
func (s *rebaseState) operationWorktreeFor(branch string) string {
	if path := s.worktreeFor(branch); path != "" {
		return path
	}
	return s.InvokingWorktree
}

func preflightWorktreeBranches(branches []stack.BranchRef, worktrees map[string]string, invokingWorktree string) error {
	paths := make(map[string]struct{})
	for _, branch := range branches {
		if path := worktrees[branch.Branch]; path != "" {
			paths[path] = struct{}{}
		} else {
			paths[invokingWorktree] = struct{}{}
		}
	}
	for path := range paths {
		busy, err := git.WorktreeBusy(path)
		if err != nil {
			return fmt.Errorf("checking worktree at %s: %w", path, err)
		}
		if busy != "" {
			return fmt.Errorf("worktree at %s %s", path, busy)
		}
	}
	return nil
}

func preflightWorktrees(state *rebaseState, branches []string) error {
	refs := make([]stack.BranchRef, len(branches))
	for i, branch := range branches {
		refs[i].Branch = branch
	}
	return preflightWorktreeBranches(refs, state.Worktrees, state.InvokingWorktree)
}

func branchNames(branches []stack.BranchRef) []string {
	names := make([]string, len(branches))
	for i, branch := range branches {
		names[i] = branch.Branch
	}
	return names
}

func hasUnownedBranch(branches []stack.BranchRef, worktrees map[string]string) bool {
	for _, branch := range branches {
		if worktrees[branch.Branch] == "" {
			return true
		}
	}
	return false
}

func hasUnownedBranchNames(branches []string, worktrees map[string]string) bool {
	for _, branch := range branches {
		if worktrees[branch] == "" {
			return true
		}
	}
	return false
}

func saveRebaseState(gitDir string, state *rebaseState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("error serializing rebase state: %w", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, rebaseStateFile), data, 0644); err != nil {
		return fmt.Errorf("error writing rebase state: %w", err)
	}
	return nil
}

func loadRebaseState(gitDir string) (*rebaseState, error) {
	data, err := os.ReadFile(filepath.Join(gitDir, rebaseStateFile))
	if err != nil {
		return nil, err
	}
	var state rebaseState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func clearRebaseState(gitDir string) {
	_ = os.Remove(filepath.Join(gitDir, rebaseStateFile))
}

func printConflictDetails(cfg *config.Config, branch string) {
	printConflictDetailsWithContinue(cfg, branch, "gh stack rebase --continue")
}

func printConflictDetailsAt(cfg *config.Config, branch, worktree string) {
	if worktree == "" {
		printConflictDetails(cfg, branch)
		return
	}
	files, err := git.ConflictedFilesAt(worktree)
	if err == nil && len(files) > 0 {
		cfg.Printf("")
		cfg.Printf("%s", cfg.ColorBold("Conflicted files:"))
		for _, file := range files {
			info, err := git.FindConflictMarkersAt(worktree, file)
			if err != nil || len(info.Sections) == 0 {
				cfg.Printf("  %s %s", cfg.ColorWarning("C"), file)
				continue
			}
			for _, sec := range info.Sections {
				cfg.Printf("  %s %s (lines %d–%d)", cfg.ColorWarning("C"), file, sec.StartLine, sec.EndLine)
			}
		}
	}
}

func worktreeSuffix(path string) string {
	if path == "" {
		return ""
	}
	return fmt.Sprintf(" in worktree %s", path)
}

func printConflictDetailsWithContinue(cfg *config.Config, branch string, continueCmd string) {
	files, err := git.ConflictedFiles()
	if err == nil && len(files) > 0 {
		cfg.Printf("")
		cfg.Printf("%s", cfg.ColorBold("Conflicted files:"))
		for _, f := range files {
			info, err := git.FindConflictMarkers(f)
			if err != nil || len(info.Sections) == 0 {
				cfg.Printf("  %s %s", cfg.ColorWarning("C"), f)
				continue
			}
			for _, sec := range info.Sections {
				cfg.Printf("  %s %s (lines %d–%d)",
					cfg.ColorWarning("C"), f, sec.StartLine, sec.EndLine)
			}
		}
	}

	cfg.Printf("")
	cfg.Printf("%s", cfg.ColorBold("To resolve:"))
	cfg.Printf("  1. Open each conflicted file and look for conflict markers:")
	cfg.Printf("     %s  (incoming changes from %s)", cfg.ColorCyan("<<<<<<< HEAD"), branch)
	cfg.Printf("     %s", cfg.ColorCyan("======="))
	cfg.Printf("     %s  (changes being rebased)", cfg.ColorCyan(">>>>>>>"))
	cfg.Printf("  2. Edit the file to keep the desired changes and remove the markers")
	cfg.Printf("  3. Stage resolved files: `%s`", cfg.ColorCyan("git add <file>"))
	cfg.Printf("  4. Continue:  `%s`", cfg.ColorCyan(continueCmd))
}
