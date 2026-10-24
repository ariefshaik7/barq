package cache

import (
	"context"
	"path/filepath"
	"testing"

	"barq/internal/storage"
)

func newStore(t *testing.T) *storage.Local {
	t.Helper()
	store, err := storage.NewLocal(filepath.Join(t.TempDir(), "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestFingerprintIsDeterministic(t *testing.T) {
	input := FingerprintInput{
		Image:          "alpine:3.20",
		Commands:       []string{"make build"},
		Env:            map[string]string{"B": "2", "A": "1"},
		GitCommitTree:  "abc123",
		UpstreamHashes: []string{"z", "a"},
	}
	first := Fingerprint(input)
	second := Fingerprint(input)
	if first != second {
		t.Fatal("fingerprint must be deterministic")
	}
	if len(first) != 64 {
		t.Fatalf("expected a hex sha256 digest, got %q", first)
	}
}

func TestFingerprintIgnoresEnvironmentOrder(t *testing.T) {
	first := Fingerprint(FingerprintInput{
		Image: "alpine",
		Env:   map[string]string{"A": "1", "B": "2"},
	})
	second := Fingerprint(FingerprintInput{
		Image: "alpine",
		Env:   map[string]string{"B": "2", "A": "1"},
	})
	if first != second {
		t.Fatal("environment ordering must not change the fingerprint")
	}
}

func TestFingerprintChangesWithEveryInput(t *testing.T) {
	base := FingerprintInput{
		Image:          "alpine",
		Commands:       []string{"make"},
		Env:            map[string]string{"A": "1"},
		GitCommitTree:  "abc",
		UpstreamHashes: []string{"one"},
		SecretVersions: "s1",
	}
	reference := Fingerprint(base)

	mutations := []FingerprintInput{
		{Image: "alpine2", Commands: base.Commands, Env: base.Env, GitCommitTree: base.GitCommitTree, UpstreamHashes: base.UpstreamHashes, SecretVersions: base.SecretVersions},
		{Image: base.Image, Commands: []string{"make other"}, Env: base.Env, GitCommitTree: base.GitCommitTree, UpstreamHashes: base.UpstreamHashes, SecretVersions: base.SecretVersions},
		{Image: base.Image, Commands: base.Commands, Env: map[string]string{"A": "2"}, GitCommitTree: base.GitCommitTree, UpstreamHashes: base.UpstreamHashes, SecretVersions: base.SecretVersions},
		{Image: base.Image, Commands: base.Commands, Env: base.Env, GitCommitTree: "different", UpstreamHashes: base.UpstreamHashes, SecretVersions: base.SecretVersions},
		{Image: base.Image, Commands: base.Commands, Env: base.Env, GitCommitTree: base.GitCommitTree, UpstreamHashes: []string{"two"}, SecretVersions: base.SecretVersions},
		{Image: base.Image, Commands: base.Commands, Env: base.Env, GitCommitTree: base.GitCommitTree, UpstreamHashes: base.UpstreamHashes, SecretVersions: "s2"},
	}
	for index, mutation := range mutations {
		if Fingerprint(mutation) == reference {
			t.Fatalf("mutation %d did not change the fingerprint", index)
		}
	}
}

func TestLookupMissesForUnknownFingerprint(t *testing.T) {
	manager := NewManager(newStore(t), "barq")
	record, hit, err := manager.Lookup(context.Background(), "does-not-exist")
	if err != nil {
		t.Fatal(err)
	}
	if hit || record != nil {
		t.Fatal("expected a cache miss")
	}
}

func TestStoreAndLookupRoundTrip(t *testing.T) {
	store := newStore(t)
	manager := NewManager(store, "barq")
	ctx := context.Background()

	workspaceKey := "barq/runs/run-1/workspaces/build-a0.tar.gz"
	if err := store.Put(ctx, workspaceKey, []byte("tarball"), "application/gzip"); err != nil {
		t.Fatal(err)
	}

	fingerprint := Fingerprint(FingerprintInput{Image: "alpine"})
	if err := manager.Store(ctx, Record{
		Fingerprint:  fingerprint,
		StepID:       "build",
		WorkspaceKey: workspaceKey,
	}); err != nil {
		t.Fatal(err)
	}
	record, hit, err := manager.Lookup(ctx, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !hit {
		t.Fatal("expected a cache hit")
	}
	if record.StepID != "build" {
		t.Fatalf("unexpected record: %+v", record)
	}
}

func TestLookupMissesWhenWorkspaceIsMissing(t *testing.T) {
	// A cache entry whose workspace was garbage collected must be a
	// miss, otherwise downstream steps would receive a dangling
	// reference and fail confusingly.
	manager := NewManager(newStore(t), "barq")
	ctx := context.Background()
	fingerprint := Fingerprint(FingerprintInput{Image: "alpine"})
	if err := manager.Store(ctx, Record{
		Fingerprint:  fingerprint,
		WorkspaceKey: "barq/runs/missing.tar.gz",
	}); err != nil {
		t.Fatal(err)
	}
	record, hit, err := manager.Lookup(ctx, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if hit || record != nil {
		t.Fatal("expected a cache miss when the workspace is gone")
	}
}
