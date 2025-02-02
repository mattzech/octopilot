package repository

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/sirupsen/logrus"
)

func cloneGitRepository(ctx context.Context, repo Repository, localPath string, options GitHubOptions) (*git.Repository, error) {
	url := fmt.Sprintf("https://github.worldpay.com/%s.git", repo.FullName())

	branch := "HEAD"
	if b, ok := repo.Params["branch"]; ok && strings.TrimSpace(b) != "" {
		branch = fmt.Sprintf("refs/heads/%s", b)
	}
	referenceName := plumbing.ReferenceName(branch)
	logrus.WithFields(logrus.Fields{
		"git-url":       url,
		"git-reference": referenceName.String(),
		"local-path":    localPath,
	}).Trace("Cloning git repository")

	_, token, err := githubClient(ctx, options)
	if err != nil {
		return nil, fmt.Errorf("failed to create github client: %w", err)
	}

	gitRepo, err := git.PlainCloneContext(ctx, localPath, false, &git.CloneOptions{
		ReferenceName: referenceName,
		URL:           url,
		Auth: &http.BasicAuth{
			Username: "x-access-token", // For GitHub Apps, the username must be `x-access-token`. For Personal Tokens, it doesn't matter.
			Password: token,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to clone git repository from %s to %s: %w", url, localPath, err)
	}

	logrus.WithFields(logrus.Fields{
		"git-url":       url,
		"git-reference": referenceName.String(),
		"local-path":    localPath,
	}).Debug("Git repository cloned")

	return gitRepo, nil
}

type switchBranchOptions struct {
	BranchName   string
	CreateBranch bool
}

func switchBranch(_ context.Context, gitRepo *git.Repository, opts switchBranchOptions) error {
	workTree, err := gitRepo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to open worktree: %w", err)
	}

	var (
		rootPath      = workTree.Filesystem.Root()
		repoName      = filepath.Base(rootPath)
		branchRefName = plumbing.NewBranchReferenceName(opts.BranchName)
	)

	if !opts.CreateBranch {
		// for an existing branch, we need to create a local reference to the remote branch
		remoteBranchRefName := plumbing.NewRemoteReferenceName("origin", opts.BranchName)
		remoteBranchRef, err := gitRepo.Reference(remoteBranchRefName, true)
		if err != nil {
			return fmt.Errorf("failed to get the reference for %s: %w", remoteBranchRef, err)
		}

		branchRef := plumbing.NewHashReference(branchRefName, remoteBranchRef.Hash())
		err = gitRepo.Storer.SetReference(branchRef)
		if err != nil {
			return fmt.Errorf("failed to store the reference for branch %s: %w", opts.BranchName, err)
		}
	}

	if err := workTree.Checkout(&git.CheckoutOptions{
		Branch: branchRefName,
		Create: opts.CreateBranch,
	}); err != nil {
		return fmt.Errorf("failed to checkout the branch %s: %w", opts.BranchName, err)
	}

	logrus.WithFields(logrus.Fields{
		"repository-name": repoName,
		"branch":          opts.BranchName,
	}).Debug("Switched Git branch")
	return nil
}

func commitChanges(_ context.Context, gitRepo *git.Repository, options UpdateOptions) (bool, error) {
	workTree, err := gitRepo.Worktree()
	if err != nil {
		return false, fmt.Errorf("failed to open worktree: %w", err)
	}

	rootPath := workTree.Filesystem.Root()
	repoName := filepath.Base(rootPath)

	status, err := workTree.Status()
	if err != nil {
		return false, fmt.Errorf("failed to get the worktree status: %w", err)
	}
	if status.IsClean() {
		return false, nil
	}
	logrus.WithFields(logrus.Fields{
		"repository-name": repoName,
		"status":          status.String(),
	}).Debug("Git status")

	for _, pattern := range options.Git.StagePatterns {
		err = workTree.AddGlob(pattern)
		if err != nil {
			return false, fmt.Errorf("failed to stage files using pattern %s: %w", pattern, err)
		}
	}

	now := time.Now()
	commitMsg := new(strings.Builder)
	commitMsg.WriteString(options.Git.CommitTitle)
	if len(options.Git.CommitBody) > 0 {
		commitMsg.WriteString("\n\n")
		commitMsg.WriteString(options.Git.CommitBody)
	}
	if len(options.Git.CommitFooter) > 0 {
		commitMsg.WriteString("\n\n-- \n")
		commitMsg.WriteString(options.Git.CommitFooter)
	}

	commit, err := workTree.Commit(commitMsg.String(),
		&git.CommitOptions{
			All: options.Git.StageAllChanged,
			Author: &object.Signature{
				Name:  options.Git.AuthorName,
				Email: options.Git.AuthorEmail,
				When:  now,
			},
			Committer: &object.Signature{
				Name:  options.Git.CommitterName,
				Email: options.Git.CommitterEmail,
				When:  now,
			},
		},
	)
	if err != nil {
		return false, fmt.Errorf("failed to commit: %w", err)
	}
	logrus.WithFields(logrus.Fields{
		"repository-name": repoName,
		"commit":          commit.String(),
	}).Debug("Git commit")

	return true, nil
}

type pushOptions struct {
	GitHubOpts GitHubOptions
	BranchName string
	ForcePush  bool
}

func pushChanges(ctx context.Context, gitRepo *git.Repository, opts pushOptions) error {
	workTree, err := gitRepo.Worktree()
	if err != nil {
		return fmt.Errorf("failed to open worktree: %w", err)
	}

	rootPath := workTree.Filesystem.Root()
	repoName := filepath.Base(rootPath)

	refSpec := fmt.Sprintf("refs/heads/%[1]s:refs/heads/%[1]s", opts.BranchName)
	if opts.ForcePush {
		// https://git-scm.com/book/en/v2/Git-Internals-The-Refspec
		// The + tells Git to update the reference even if it isn’t a fast-forward.
		refSpec = fmt.Sprintf("+%s", refSpec)
	}

	_, token, err := githubClient(ctx, opts.GitHubOpts)
	if err != nil {
		return fmt.Errorf("failed to create github client: %w", err)
	}

	logrus.WithFields(logrus.Fields{
		"repository-name": repoName,
		"branch":          opts.BranchName,
		"force":           opts.ForcePush,
	}).Trace("Pushing git changes")
	err = gitRepo.PushContext(ctx, &git.PushOptions{
		RefSpecs: []config.RefSpec{
			config.RefSpec(refSpec),
		},
		Auth: &http.BasicAuth{
			Username: "x-access-token", // For GitHub Apps, the username must be `x-access-token`. For Personal Tokens, it doesn't matter.
			Password: token,
		},
	})
	if err != nil {
		return fmt.Errorf("failed to push branch %s to %s: %w", opts.BranchName, repoName, err)
	}

	logrus.WithFields(logrus.Fields{
		"repository-name": repoName,
		"branch":          opts.BranchName,
	}).Debug("Git changes pushed")
	return nil
}
