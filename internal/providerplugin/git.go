package providerplugin

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"
)

const gitAcquisitionTimeout = 2 * time.Minute

func (m *Manager) installGit(ctx context.Context, request InstallRequest) (Snapshot, error) {
	remote, err := validateGitURL(request.Source)
	if err != nil {
		return Snapshot{}, err
	}
	acquisitionContext, cancel := context.WithTimeout(ctx, gitAcquisitionTimeout)
	defer cancel()
	checkout := filepath.Join(m.paths.Cache, ".git-"+uuid.NewString())
	defer os.RemoveAll(checkout)
	repository, err := git.PlainCloneContext(acquisitionContext, checkout, false, &git.CloneOptions{
		URL:               remote,
		NoCheckout:        true,
		RecurseSubmodules: git.NoRecurseSubmodules,
		Tags:              git.AllTags,
	})
	if err != nil {
		return Snapshot{}, errors.New("acquire HTTPS Git plugin source")
	}
	var revision plumbing.Revision = "HEAD"
	if request.Ref != "" {
		revision = plumbing.Revision(request.Ref)
	}
	hash, err := repository.ResolveRevision(revision)
	if err != nil {
		return Snapshot{}, errors.New("resolve requested Git plugin ref")
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return Snapshot{}, errors.New("open acquired Git plugin worktree")
	}
	if err := worktree.Checkout(&git.CheckoutOptions{Hash: *hash, Force: true}); err != nil {
		return Snapshot{}, errors.New("materialize requested Git plugin commit")
	}
	if err := rejectUnsupportedGitMetadata(checkout); err != nil {
		return Snapshot{}, err
	}
	if err := os.RemoveAll(filepath.Join(checkout, ".git")); err != nil {
		return Snapshot{}, errors.New("remove Git acquisition metadata")
	}
	local := request
	local.Source = checkout
	local.Ref = ""
	local.sourceKind = "git"
	local.sourceCommit = hash.String()
	return m.installDirectory(ctx, local, checkout)
}

func validateGitURL(raw string) (string, error) {
	value, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || value.Scheme != "https" || value.Hostname() == "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" {
		return "", errors.New("Git source must be an HTTPS URL without credentials, query, or fragment")
	}
	if value.Port() != "" && value.Port() != "443" {
		return "", errors.New("HTTPS Git source must use the default TLS port")
	}
	return value.String(), nil
}

func rejectUnsupportedGitMetadata(root string) error {
	modules := filepath.Join(root, ".gitmodules")
	if info, err := os.Lstat(modules); err == nil {
		if info.Mode().IsRegular() {
			return errors.New("Git plugin sources may not declare submodules")
		}
		return errors.New("Git plugin source contains an unsafe .gitmodules entry")
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect Git submodule declaration: %w", err)
	}
	attributes := filepath.Join(root, ".gitattributes")
	data, err := os.ReadFile(attributes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("inspect Git attributes")
	}
	if len(data) > 1<<20 {
		return errors.New("Git attributes exceed host limit")
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.ToLower(strings.TrimSpace(line))
		if line != "" && !strings.HasPrefix(line, "#") && (strings.Contains(line, "filter=lfs") || strings.Contains(line, "filter lfs")) {
			return errors.New("Git LFS plugin sources are unsupported")
		}
	}
	return nil
}
