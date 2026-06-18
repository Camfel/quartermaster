// Package git monitors Git repositories for changes using go-git.
// No os/exec — all operations use the pure-Go go-git library.
package git

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Watcher monitors a Git repository for changes.
type Watcher struct {
	repoURL           string
	branch            string
	token             string
	sshKeyPath        string
	sshKnownHostsPath string
	pollInterval      time.Duration
	cooldown          time.Duration
	localPath         string
	onChanged         func(ctx context.Context, newHash string)

	lastTrigger time.Time

	// Cached go-git auth method, built once on Start.
	auth transport.AuthMethod
}

// NewWatcher creates a new Watcher instance.
func NewWatcher(
	repoURL, branch, token, sshKeyPath, sshKnownHostsPath, localPath string,
	interval, cooldown time.Duration,
	onChange func(ctx context.Context, newHash string),
) *Watcher {
	return &Watcher{
		repoURL:           repoURL,
		branch:            branch,
		token:             token,
		sshKeyPath:        sshKeyPath,
		sshKnownHostsPath: sshKnownHostsPath,
		localPath:         localPath,
		pollInterval:      interval,
		cooldown:          cooldown,
		onChanged:         onChange,
	}
}

func (w *Watcher) RepoURL() string { return w.repoURL }

// Start begins the polling loop.
func (w *Watcher) Start(ctx context.Context) error {
	log.Printf("Starting Git watcher for %s (branch: %s)", sanitiseURL(w.repoURL), w.branch)

	repo, err := w.ensureRepo(ctx)
	if err != nil {
		return fmt.Errorf("failed to ensure repository: %w", err)
	}

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(w.branch), true)
	if err != nil {
		return fmt.Errorf("failed to get branch reference: %w", err)
	}
	lastHash := ref.Hash().String()
	log.Printf("Initial hash: %s", lastHash)

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Git watcher stopping...")
			return ctx.Err()
		case <-ticker.C:
			newHash, err := w.poll(ctx, repo)
			if err != nil {
				log.Printf("Error polling repository: %v", err)
				continue
			}

			if newHash != "" && newHash != lastHash {
				log.Printf("New commit detected: %s (was %s)", newHash, lastHash)
				lastHash = newHash

				if err := w.checkout(repo); err != nil {
					log.Printf("Error updating working tree: %v", err)
					continue
				}

				if w.cooldown > 0 && time.Since(w.lastTrigger) < w.cooldown {
					log.Printf("Skipping trigger: cooldown (last: %v ago)", time.Since(w.lastTrigger))
					continue
				}

				if w.onChanged != nil {
					w.lastTrigger = time.Now()
					w.onChanged(ctx, newHash)
				}
			}
		}
	}
}

// ── clone ────────────────────────────────────────────────────────────────

func (w *Watcher) ensureRepo(ctx context.Context) (*git.Repository, error) {
	repo, err := git.PlainOpen(w.localPath)
	if err == git.ErrRepositoryNotExists {
		log.Printf("Repository not found at %s, cloning...", w.localPath)
		return w.clone(ctx)
	}
	if err != nil {
		return nil, err
	}
	// Existing repo — reset to match remote in case of stale files.
	if err := w.checkout(repo); err != nil {
		log.Printf("Warning: checkout on existing repo failed: %v", err)
	}
	return repo, nil
}

func (w *Watcher) clone(ctx context.Context) (*git.Repository, error) {
	opts := &git.CloneOptions{
		URL:           w.repoURL,
		ReferenceName: plumbing.NewBranchReferenceName(w.branch),
	}
	if a, err := w.authMethod(); err != nil {
		return nil, err
	} else {
		opts.Auth = a
	}
	return git.PlainCloneContext(ctx, w.localPath, false, opts)
}

// ── poll ─────────────────────────────────────────────────────────────────

func (w *Watcher) poll(ctx context.Context, repo *git.Repository) (string, error) {
	remote, err := repo.Remote("origin")
	if err != nil {
		return "", fmt.Errorf("remote 'origin': %w", err)
	}

	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", w.branch, w.branch)
	opts := &git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(refspec)},
	}
	if a, err := w.authMethod(); err != nil {
		return "", err
	} else {
		opts.Auth = a
	}

	if err := remote.FetchContext(ctx, opts); err != nil {
		if err == git.NoErrAlreadyUpToDate {
			ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", w.branch), true)
			if err != nil {
				return "", nil // no remote tracking ref yet
			}
			return ref.Hash().String(), nil
		}
		return "", fmt.Errorf("fetch: %w", err)
	}

	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", w.branch), true)
	if err != nil {
		return "", fmt.Errorf("remote tracking ref after fetch: %w", err)
	}
	newHash := ref.Hash().String()
	log.Printf("Fetched %s -> %s", w.branch, newHash)
	return newHash, nil
}

// ── checkout ─────────────────────────────────────────────────────────────

func (w *Watcher) checkout(repo *git.Repository) error {
	// Fetch the branch tip into the remote tracking ref.
	remote, err := repo.Remote("origin")
	if err != nil {
		return fmt.Errorf("remote 'origin': %w", err)
	}

	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", w.branch, w.branch)
	opts := &git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(refspec)},
	}
	if a, err := w.authMethod(); err != nil {
		return err
	} else {
		opts.Auth = a
	}

	if err := remote.Fetch(opts); err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("fetch: %w", err)
	}

	// Reset the worktree to the remote tracking ref.
	wt, err := repo.Worktree()
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}

	ref, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", w.branch), true)
	if err != nil {
		return fmt.Errorf("remote ref %s: %w", w.branch, err)
	}

	return wt.Reset(&git.ResetOptions{
		Commit: ref.Hash(),
		Mode:   git.HardReset,
	})
}

// ── auth ─────────────────────────────────────────────────────────────────

func (w *Watcher) authMethod() (transport.AuthMethod, error) {
	if w.sshKeyPath != "" {
		keyPath := expandPath(w.sshKeyPath)
		auth, err := gogitssh.NewPublicKeysFromFile("git", keyPath, "")
		if err != nil {
			return nil, fmt.Errorf("loading SSH key %s: %w", keyPath, err)
		}
		if w.sshKnownHostsPath != "" {
			cb, err := hostKeyCallback(expandPath(w.sshKnownHostsPath))
			if err != nil {
				if os.IsNotExist(err) {
					log.Printf("WARNING: known_hosts not found at %s — host-key verification disabled",
						expandPath(w.sshKnownHostsPath))
				} else {
					return nil, fmt.Errorf("known_hosts %s: %w", expandPath(w.sshKnownHostsPath), err)
				}
			} else {
				auth.HostKeyCallback = cb
			}
		}
		return auth, nil
	}

	if w.token != "" {
		return &http.BasicAuth{
			Username: "x-access-token",
			Password: w.token,
		}, nil
	}

	return nil, nil // public repo
}

// hostKeyCallback parses known_hosts for github.com keys.
func hostKeyCallback(path string) (gossh.HostKeyCallback, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys []gossh.PublicKey
	for len(data) > 0 {
		_, hosts, key, _, rest, err := gossh.ParseKnownHosts(data)
		if err != nil {
			break
		}
		data = rest
		for _, h := range hosts {
			if strings.Contains(h, "github.com") {
				keys = append(keys, key)
				break
			}
		}
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no github.com key found in %s", path)
	}
	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		fp := gossh.FingerprintSHA256(key)
		for _, trusted := range keys {
			if gossh.FingerprintSHA256(trusted) == fp {
				return nil
			}
		}
		return fmt.Errorf("knownhosts: key mismatch for %s", hostname)
	}, nil
}

// ── helpers ──────────────────────────────────────────────────────────────

func sanitiseURL(raw string) string {
	if idx := strings.Index(raw, "@"); idx != -1 {
		schemeEnd := strings.Index(raw, "://")
		if schemeEnd != -1 && idx > schemeEnd+3 {
			return raw[:schemeEnd+3] + "***" + raw[idx:]
		}
	}
	return raw
}

func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
