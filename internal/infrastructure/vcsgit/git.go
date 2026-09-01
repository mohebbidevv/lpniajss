package vcsgit

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// GitSource implements repository.GitSource by shelling out to the system
// git binary — same style as hostexec shelling to npm/node, no extra Go
// module dependency for something the OS already provides.
type GitSource struct{}

func NewGitSource() *GitSource {
	return &GitSource{}
}

// Sync clones repoURL into destDir if it isn't a git checkout yet, or
// fetches+hard-resets to the latest commit on ref if it already is. A hard
// reset (rather than a merge/rebase) is deliberate — destDir is a deploy
// target, not a dev working copy, so it should always match the remote
// exactly with no possibility of a conflict.
func (g *GitSource) Sync(ctx context.Context, repoURL, ref, destDir string) error {
	if _, err := os.Stat(filepath.Join(destDir, ".git")); err == nil {
		return g.pull(ctx, ref, destDir)
	}
	return g.clone(ctx, repoURL, ref, destDir)
}

func (g *GitSource) clone(ctx context.Context, repoURL, ref, destDir string) error {
	args := []string{"clone", "--depth", "1"}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, repoURL, destDir)
	return runGit(ctx, "", args...)
}

func (g *GitSource) pull(ctx context.Context, ref, destDir string) error {
	fetchRef := ref
	if fetchRef == "" {
		fetchRef = "HEAD"
	}
	if err := runGit(ctx, destDir, "fetch", "--depth", "1", "origin", fetchRef); err != nil {
		return err
	}
	return runGit(ctx, destDir, "reset", "--hard", "FETCH_HEAD")
}

func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w — output: %s", strings.Join(args, " "), err, buf.String())
	}
	return nil
}
