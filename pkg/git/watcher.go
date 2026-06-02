package git

import (
	"context"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	gossh "golang.org/x/crypto/ssh"
)

// Watcher monitors a Git repository for changes.
type Watcher struct {
	repoURL           string
	branch            string
	token             string // HTTPS PAT (optional)
	sshKeyPath        string // SSH private key path (optional, preferred)
	sshKnownHostsPath string // known_hosts for host-key verification
	pollInterval      time.Duration
	cooldown          time.Duration
	localPath         string
	onChanged         func(ctx context.Context, newHash string)

	lastTrigger time.Time
}

// NewWatcher creates a new Watcher instance.
//
// Authentication (in order of precedence):
//  1. sshKeyPath — SSH deploy key (recommended for private repos)
//  2. token       — HTTPS Personal Access Token
//  3. (none)      — public repo, no auth needed
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

// SetOnChanged allows updating the change callback after creation.
func (w *Watcher) SetOnChanged(fn func(ctx context.Context, newHash string)) {
	w.onChanged = fn
}

// RepoURL returns the repository URL this watcher monitors.
func (w *Watcher) RepoURL() string { return w.repoURL }

// Start begins the polling loop.
func (w *Watcher) Start(ctx context.Context) error {
	log.Printf("Starting Git watcher for %s (branch: %s)", sanitiseURL(w.repoURL), w.branch)

	var lastHash string

	repo, err := w.ensureRepo(ctx)
	if err != nil {
		return fmt.Errorf("failed to ensure repository: %w", err)
	}

	ref, err := repo.Reference(plumbing.NewBranchReferenceName(w.branch), true)
	if err != nil {
		return fmt.Errorf("failed to get branch reference: %w", err)
	}
	lastHash = ref.Hash().String()
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

				// Update the local working tree so the reconciler sees the
				// latest stack files.
				if err := w.checkout(ctx); err != nil {
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

// ── clone (go-git) ──────────────────────────────────────────────────────

func (w *Watcher) ensureRepo(ctx context.Context) (*git.Repository, error) {
	repo, err := git.PlainOpen(w.localPath)
	if err == git.ErrRepositoryNotExists {
		log.Printf("Repository not found at %s, cloning...", w.localPath)
		opts := &git.CloneOptions{
			URL:           w.repoURL,
			ReferenceName: plumbing.NewBranchReferenceName(w.branch),
			Progress:      nil,
		}

		if w.sshKeyPath != "" {
			auth, sshErr := w.sshAuth()
			if sshErr != nil {
				return nil, fmt.Errorf("SSH auth setup failed: %w", sshErr)
			}
			opts.Auth = auth
			log.Printf("Cloning via SSH (key: %s)", w.sshKeyPath)
		} else if w.token != "" {
			opts.Auth = &http.BasicAuth{
				Username: "x-access-token",
				Password: w.token,
			}
			log.Printf("Cloning via HTTPS (PAT)")
		}

		return git.PlainClone(w.localPath, false, opts)
	}
	if err != nil {
		return nil, err
	}

	// Existing repo — force-checkout to ensure working tree matches remote
	// in case the previous daemon run left stale files.
	if err := w.checkout(ctx); err != nil {
		log.Printf("Warning: checkout on existing repo failed: %v", err)
	}

	return repo, nil
}

// ── poll (git CLI) ──────────────────────────────────────────────────────

func (w *Watcher) poll(ctx context.Context, repo *git.Repository) (string, error) {
	log.Printf("Polling repo: %s", sanitiseURL(w.repoURL))

	args := []string{"-C", w.localPath, "fetch", "origin", w.branch}
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	switch {
	case w.sshKeyPath != "":
		// SSH: set GIT_SSH_COMMAND with key and known_hosts.
		sshCmd := fmt.Sprintf(
			"ssh -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s -i %s",
			shellQuote(w.sshKnownHostsPath), shellQuote(w.sshKeyPath),
		)
		env = append(env, "GIT_SSH_COMMAND="+sshCmd)

	case w.token != "" && strings.HasPrefix(w.repoURL, "https://"):
		// HTTPS PAT: inject Bearer header via one-shot git config.
		args = append([]string{
			"-c", "http.extraHeader=Authorization: Bearer " + w.token,
		}, args...)
		env = append(env, "GIT_CONFIG_COUNT=0")
	}

	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env

	if _, err := cmd.CombinedOutput(); err != nil {
		log.Printf("Git fetch error: %v", err)
		return "", fmt.Errorf("fetch failed: %w", err)
	}

	remoteRefName := plumbing.NewRemoteReferenceName("origin", w.branch)
	ref, err := repo.Reference(remoteRefName, true)
	if err != nil {
		return "", fmt.Errorf("failed to get remote branch reference after fetch: %w", err)
	}

	newHash := ref.Hash().String()
	log.Printf("New hash from poll (remote): %s", newHash)
	return newHash, nil
}

// ── SSH auth helpers ────────────────────────────────────────────────────

// sshAuth loads the private key and returns a go-git SSH auth method
// with host-key verification from the configured known_hosts file.
func (w *Watcher) sshAuth() (*gogitssh.PublicKeys, error) {
	keyPath := expandPath(w.sshKeyPath)
	auth, err := gogitssh.NewPublicKeysFromFile("git", keyPath, "")
	if err != nil {
		return nil, fmt.Errorf("loading SSH key %s: %w", keyPath, err)
	}

	if w.sshKnownHostsPath != "" {
		khPath := expandPath(w.sshKnownHostsPath)
		cb, err := hostKeyCallback(khPath)
		if err != nil {
			if os.IsNotExist(err) {
				log.Printf("WARNING: known_hosts file not found at %s — "+
					"host-key verification disabled. Run: ssh-keyscan github.com >> %s",
					khPath, khPath)
			} else {
				return nil, fmt.Errorf("loading known_hosts %s: %w", khPath, err)
			}
		} else {
			auth.HostKeyCallback = cb
		}
	}

	return auth, nil
}

// hostKeyCallback reads an OpenSSH known_hosts file and returns a
// HostKeyCallback that validates server keys.  Uses x/crypto/ssh/knownhosts
// directly because go-git's NewKnownHostsCallback has host-matching issues
// in v5.19.1.
func hostKeyCallback(path string) (gossh.HostKeyCallback, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var githubKeys []gossh.PublicKey
	for len(data) > 0 {
		_, hosts, key, _, rest, err := gossh.ParseKnownHosts(data)
		if err != nil {
			break
		}
		data = rest
		for _, h := range hosts {
			host := h
			if strings.Contains(host, "github.com") {
				githubKeys = append(githubKeys, key)
				break
			}
		}
	}

	if len(githubKeys) == 0 {
		return nil, fmt.Errorf("no github.com key found in %s", path)
	}

	return func(hostname string, remote net.Addr, key gossh.PublicKey) error {
		fingerprint := gossh.FingerprintSHA256(key)
		for _, trusted := range githubKeys {
			if gossh.FingerprintSHA256(trusted) == fingerprint {
				return nil
			}
		}
		return fmt.Errorf("knownhosts: key mismatch for %s (fingerprint %s)",
			hostname, fingerprint)
	}, nil
}

// ── helpers ──────────────────────────────────────────────────────────────

// checkout fetches and force-updates the local working tree to match the
// remote branch.  Uses FETCH_HEAD directly to avoid relying on the remote
// tracking ref being updated by the poll.
func (w *Watcher) checkout(ctx context.Context) error {
	// Fetch directly from the remote URL into FETCH_HEAD.
	fetchURL := w.repoURL
	if w.sshKeyPath != "" {
		// Use the CLI-appropriate fetch URL with auth.
	} else if w.token != "" && strings.HasPrefix(w.repoURL, "https://") {
		fetchURL = strings.Replace(w.repoURL, "https://", "https://x-access-token:"+w.token+"@", 1)
	}

	fetchArgs := []string{"-C", w.localPath, "fetch", fetchURL, w.branch}
	if w.sshKeyPath != "" {
		sshCmd := fmt.Sprintf("ssh -o StrictHostKeyChecking=yes -o UserKnownHostsFile=%s -i %s",
			shellQuote(w.sshKnownHostsPath), shellQuote(w.sshKeyPath))
		cmd := exec.CommandContext(ctx, "git", fetchArgs...)
		cmd.Env = append(os.Environ(), "GIT_SSH_COMMAND="+sshCmd, "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("fetch: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	} else {
		cmd := exec.CommandContext(ctx, "git", fetchArgs...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("fetch: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	// Reset the working tree to FETCH_HEAD.
	resetCmd := exec.CommandContext(ctx, "git",
		"-C", w.localPath,
		"reset", "--hard", "FETCH_HEAD",
	)
	if out, err := resetCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reset: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sanitiseURL(raw string) string {
	if idx := strings.Index(raw, "@"); idx != -1 {
		schemeEnd := strings.Index(raw, "://")
		if schemeEnd != -1 && idx > schemeEnd+3 {
			return raw[:schemeEnd+3] + "***" + raw[idx:]
		}
	}
	return raw
}

// shellQuote wraps a path in single quotes for safe use in shell commands.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

// expandPath expands ~ to the user's home directory.
func expandPath(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}
