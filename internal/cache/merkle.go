// Package cache implements the content addressed Merkle step cache.
//
// A cache hit skips Pod creation entirely, but it does not skip the
// outputs: the cache record stores the workspace object key produced by
// the original run, so downstream steps still receive the artifacts and
// the .barq.env outputs of the cached step.
package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"barq/internal/storage"
)

// FingerprintInput captures everything that can change a step result.
type FingerprintInput struct {
	Image string
	// Commands are the shell commands executed by the step.
	Commands []string
	// Env is the resolved environment, excluding secret values.
	Env map[string]string
	// GitCommitTree identifies the source revision.
	GitCommitTree string
	// UpstreamHashes are the fingerprints of the parent steps.
	UpstreamHashes []string
	// SecretVersions is a digest of the secret versions in use.
	SecretVersions string
}

// Record is the durable cache entry for a step.
type Record struct {
	Fingerprint string `json:"fingerprint"`
	StepID      string `json:"step_id"`
	// WorkspaceKey points at the workspace tarball produced by the
	// original run. Downstream steps download it to restore outputs.
	WorkspaceKey string    `json:"workspace_key,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	DurationMS   int64     `json:"duration_ms"`
}

// Manager reads and writes cache records in an object store.
type Manager struct {
	store  storage.ObjectStore
	prefix string
}

// NewManager creates a cache manager.
func NewManager(store storage.ObjectStore, prefix string) *Manager {
	return &Manager{store: store, prefix: strings.Trim(prefix, "/")}
}

// Fingerprint computes the deterministic SHA-256 fingerprint of a step.
//
// Every field is length prefixed before hashing so that no two different
// inputs can produce the same byte stream.
func Fingerprint(input FingerprintInput) string {
	hasher := sha256.New()
	write := func(value string) {
		fmt.Fprintf(hasher, "%d:", len(value))
		hasher.Write([]byte(value))
		hasher.Write([]byte{0})
	}
	write("barq-cache-v1")
	write(input.Image)
	write(input.GitCommitTree)
	write(input.SecretVersions)
	for _, command := range input.Commands {
		write(command)
	}
	keys := make([]string, 0, len(input.Env))
	for key := range input.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		write(key)
		write(input.Env[key])
	}
	upstream := append([]string(nil), input.UpstreamHashes...)
	sort.Strings(upstream)
	for _, hash := range upstream {
		write(hash)
	}
	return hex.EncodeToString(hasher.Sum(nil))
}

// Key returns the durable object key of a cache record.
func (m *Manager) Key(fingerprint string) string {
	return fmt.Sprintf("%s/merkle-cache/%s.json", m.prefix, fingerprint)
}

// Lookup returns the cache record for a fingerprint, if one is usable.
//
// A record is only usable when its workspace object still exists. A cache
// entry whose workspace was garbage collected is treated as a miss so
// that downstream steps never receive a dangling reference.
func (m *Manager) Lookup(ctx context.Context, fingerprint string) (*Record, bool, error) {
	data, err := m.store.Get(ctx, m.Key(fingerprint))
	if errors.Is(err, storage.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		// A corrupt record must not break the build. Treat it as a miss
		// and let the step re-execute.
		return nil, false, nil
	}
	if record.Fingerprint != fingerprint {
		return nil, false, nil
	}
	if record.WorkspaceKey != "" {
		exists, err := m.store.Exists(ctx, record.WorkspaceKey)
		if err != nil {
			return nil, false, err
		}
		if !exists {
			return nil, false, nil
		}
	}
	return &record, true, nil
}

// Store writes a cache record for a successful step.
func (m *Manager) Store(ctx context.Context, record Record) error {
	if record.Fingerprint == "" {
		return fmt.Errorf("cache record requires a fingerprint")
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return m.store.Put(ctx, m.Key(record.Fingerprint), encoded, "application/json")
}

// Duration renders a duration in milliseconds for the UI.
func Duration(d time.Duration) int64 {
	return d.Milliseconds()
}

// FormatIndex renders a shard index for display.
func FormatIndex(index int) string {
	return strconv.Itoa(index)
}
