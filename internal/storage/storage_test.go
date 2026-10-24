package storage

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecretMaskerRedactsExactMatch(t *testing.T) {
	masker := NewSecretMasker([]string{"super-secret-value"})
	output := string(masker.Mask([]byte("token=super-secret-value done")))
	if strings.Contains(output, "super-secret-value") {
		t.Fatalf("secret was not redacted: %q", output)
	}
	if !strings.Contains(output, Redaction) {
		t.Fatalf("expected the redaction placeholder: %q", output)
	}
}

func TestSecretMaskerIsBoundarySafe(t *testing.T) {
	// A secret split across two chunks must still be redacted. This is
	// the bug class that leaks credentials in production log streams.
	masker := NewSecretMasker([]string{"hunter2hunter2"})
	pending := []byte{}
	var output strings.Builder

	for _, chunk := range []string{"prefix hunter2", "hunter2 suffix"} {
		combined := append(append([]byte(nil), pending...), chunk...)
		safe, remainder := masker.consume(combined, false)
		output.Write(safe)
		pending = remainder
	}
	safe, _ := masker.consume(pending, true)
	output.Write(safe)

	if strings.Contains(output.String(), "hunter2hunter2") {
		t.Fatalf("split secret leaked: %q", output.String())
	}
}

func TestSecretMaskerIgnoresShortSecrets(t *testing.T) {
	masker := NewSecretMasker([]string{"ab"})
	if masker.HasSecrets() {
		t.Fatal("very short values must not be treated as secrets")
	}
	output := string(masker.Mask([]byte("ab cd ab")))
	if output != "ab cd ab" {
		t.Fatalf("expected unchanged output, got %q", output)
	}
}

func TestSecretMaskerDoesNotLeakPartialSecret(t *testing.T) {
	// A secret that is only partially present must be retained in the
	// pending buffer, never released into the output.
	masker := NewSecretMasker([]string{"abcdefghij"})
	safe, remainder := masker.consume([]byte("xx abcdefgh"), false)
	if strings.Contains(string(safe), "abcdefgh") {
		t.Fatalf("partial secret leaked into the safe prefix: %q", string(safe))
	}
	if !strings.Contains(string(remainder), "abcdefgh") {
		t.Fatalf("expected the partial secret to be retained: %q", string(remainder))
	}
}

func TestSecretMaskerRedactsCompleteSecretImmediately(t *testing.T) {
	masker := NewSecretMasker([]string{"abcdefghij"})
	safe, remainder := masker.consume([]byte("xx abcdefghij"), false)
	if strings.Contains(string(safe), "abcdefghij") {
		t.Fatalf("complete secret leaked: %q", string(safe))
	}
	if !strings.Contains(string(safe), Redaction) {
		t.Fatalf("expected redaction in the safe prefix: %q", string(safe))
	}
	if len(remainder) != 0 {
		t.Fatalf("expected an empty remainder, got %q", string(remainder))
	}
}

func TestTieredAppendAndArchive(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	tiered, err := NewTiered(store, "barq", filepath.Join(root, "hot"), NewSecretMasker(nil), TieredOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := tiered.AppendLog(ctx, "run-1", []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	if err := tiered.FlushLog(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	contents, err := tiered.ReadLog(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "hello\n" {
		t.Fatalf("unexpected log contents: %q", string(contents))
	}
	if err := tiered.ArchiveRun(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	archived, err := tiered.LoadArchivedLog(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if string(archived) != "hello\n" {
		t.Fatalf("unexpected archived log: %q", string(archived))
	}
}

func TestTieredLogBatching(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	// Use a small batch size and long flush period to test batching behavior.
	tiered, err := NewTiered(store, "barq", filepath.Join(root, "hot"), NewSecretMasker(nil), TieredOptions{
		LogBatchSize:   100, // 100 bytes
		LogFlushPeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Write small chunks that don't exceed batch size.
	if err := tiered.AppendLog(ctx, "run-batch", []byte("line1\n")); err != nil {
		t.Fatal(err)
	}
	if err := tiered.AppendLog(ctx, "run-batch", []byte("line2\n")); err != nil {
		t.Fatal(err)
	}

	// Without flush, data should be in batch buffer, not on disk yet.
	contents, err := tiered.ReadLog(ctx, "run-batch")
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) > 0 {
		t.Fatalf("expected no data on disk before flush, got %q", string(contents))
	}

	// Flush should write the batched data.
	if err := tiered.FlushLog(ctx, "run-batch"); err != nil {
		t.Fatal(err)
	}
	contents, err = tiered.ReadLog(ctx, "run-batch")
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "line1\nline2\n" {
		t.Fatalf("expected batched data after flush, got %q", string(contents))
	}

	// Archive should include all data.
	if err := tiered.ArchiveRun(ctx, "run-batch"); err != nil {
		t.Fatal(err)
	}
	archived, err := tiered.LoadArchivedLog(ctx, "run-batch")
	if err != nil {
		t.Fatal(err)
	}
	if string(archived) != "line1\nline2\n" {
		t.Fatalf("expected batched data in archive, got %q", string(archived))
	}
}

func TestTieredLogBatchingAutoFlush(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	// Small batch size so it triggers auto-flush on size.
	tiered, err := NewTiered(store, "barq", filepath.Join(root, "hot"), NewSecretMasker(nil), TieredOptions{
		LogBatchSize:   20, // Very small to trigger flush
		LogFlushPeriod: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Write enough to exceed batch size.
	if err := tiered.AppendLog(ctx, "run-auto", []byte("this line is longer than 20 bytes\n")); err != nil {
		t.Fatal(err)
	}

	// Should have auto-flushed because batch size was exceeded.
	contents, err := tiered.ReadLog(ctx, "run-auto")
	if err != nil {
		t.Fatal(err)
	}
	if len(contents) == 0 {
		t.Fatal("expected auto-flush on batch size exceeded")
	}
	if !strings.Contains(string(contents), "this line is longer than 20 bytes") {
		t.Fatalf("expected full line in output, got %q", string(contents))
	}
}

func TestTieredStateIsAtomicAndDurable(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	tiered, err := NewTiered(store, "barq", filepath.Join(root, "hot"), NewSecretMasker(nil), TieredOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	payload := map[string]any{"status": "RUNNING"}
	if err := tiered.SaveState(ctx, "run-2", payload); err != nil {
		t.Fatal(err)
	}
	var loaded map[string]any
	if err := tiered.LoadState(ctx, "run-2", &loaded); err != nil {
		t.Fatal(err)
	}
	if loaded["status"] != "RUNNING" {
		t.Fatalf("unexpected state: %+v", loaded)
	}
	// No temporary files may be left behind.
	entries, err := os.ReadDir(filepath.Join(root, "hot", "runs", "run-2"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".state-") {
			t.Fatalf("temporary file was not cleaned up: %s", entry.Name())
		}
	}
}

func TestSecretVaultEncryptsAndRoundTrips(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	vault, err := NewSecretVault(store, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	metadata, err := vault.Set(ctx, "GITHUB_TOKEN", "super-secret")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Version != 1 {
		t.Fatalf("expected version 1, got %d", metadata.Version)
	}
	value, _, err := vault.Value(ctx, "GITHUB_TOKEN")
	if err != nil {
		t.Fatal(err)
	}
	if value != "super-secret" {
		t.Fatalf("unexpected value: %q", value)
	}

	// The stored ciphertext must not contain the plaintext.
	raw, err := store.Get(ctx, secretStoreKey)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "super-secret") {
		t.Fatal("secret was stored in plaintext")
	}
}

func TestSecretVaultRejectsTamperedCiphertext(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	vault, err := NewSecretVault(store, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := vault.Set(ctx, "TOKEN", "value"); err != nil {
		t.Fatal(err)
	}

	raw, err := store.Get(ctx, secretStoreKey)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := store.Put(ctx, secretStoreKey, raw, "application/octet-stream"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := vault.Value(ctx, "TOKEN"); err == nil {
		t.Fatal("expected an authentication failure for tampered ciphertext")
	}
}

func TestSecretVaultListReturnsMetadataOnly(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	vault, err := NewSecretVault(store, key)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := vault.Set(ctx, "TOKEN", "value"); err != nil {
		t.Fatal(err)
	}
	metadata, err := vault.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 1 || metadata[0].Name != "TOKEN" {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}
	encoded, _ := json.Marshal(metadata)
	if strings.Contains(string(encoded), "value") {
		t.Fatalf("metadata leaked the secret value: %s", string(encoded))
	}
}

func TestLocalRelayRejectsUnauthenticatedRequests(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	handler := store.RelayHandler()

	request := httptest.NewRequest(http.MethodGet, "/_objects/runs/1/logs.txt", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}

func TestLocalRelayRejectsWrongSignature(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	handler := store.RelayHandler()
	request := httptest.NewRequest(http.MethodGet, "/_objects/runs/1/logs.txt?expires=99999999999&signature=deadbeef", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", recorder.Code)
	}
}

func TestLocalRelayRejectsExpiredCapability(t *testing.T) {
	root := t.TempDir()
	secret := []byte("0123456789abcdef")
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", secret)
	if err != nil {
		t.Fatal(err)
	}
	handler := store.RelayHandler()
	expires := store.now().Add(-time.Hour).Unix()
	signature := store.sign(http.MethodGet, "runs/1/logs.txt", expires)
	request := httptest.NewRequest(http.MethodGet, "/_objects/runs/1/logs.txt?expires="+itoa(expires)+"&signature="+signature, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for an expired capability, got %d", recorder.Code)
	}
}

func TestLocalRelayAcceptsValidCapability(t *testing.T) {
	root := t.TempDir()
	secret := []byte("0123456789abcdef")
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", secret)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.Put(ctx, "runs/1/logs.txt", []byte("payload"), "text/plain"); err != nil {
		t.Fatal(err)
	}
	expires := store.now().Add(time.Hour).Unix()
	signature := store.sign(http.MethodGet, "runs/1/logs.txt", expires)
	handler := store.RelayHandler()
	request := httptest.NewRequest(http.MethodGet, "/_objects/runs/1/logs.txt?expires="+itoa(expires)+"&signature="+signature, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", recorder.Code)
	}
	if recorder.Body.String() != "payload" {
		t.Fatalf("unexpected body: %q", recorder.Body.String())
	}
}

func TestLocalStoreRejectsPathTraversal(t *testing.T) {
	root := t.TempDir()
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", []byte("0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put(context.Background(), "../../etc/passwd", []byte("x"), "text/plain"); err == nil {
		t.Fatal("expected path traversal to be rejected")
	}
	if err := store.Put(context.Background(), "/absolute", []byte("x"), "text/plain"); err == nil {
		t.Fatal("expected absolute keys to be rejected")
	}
}

func TestLocalPresignIsMethodScoped(t *testing.T) {
	root := t.TempDir()
	secret := []byte("0123456789abcdef")
	store, err := NewLocal(filepath.Join(root, "objects"), "http://example.invalid", secret)
	if err != nil {
		t.Fatal(err)
	}
	expires := store.now().Add(time.Hour).Unix()
	getSignature := store.sign(http.MethodGet, "object", expires)
	putSignature := store.sign(http.MethodPut, "object", expires)
	if getSignature == putSignature {
		t.Fatal("signatures must be scoped to the HTTP method")
	}
}

// itoa is a tiny helper to avoid importing strconv in several places.
func itoa(value int64) string {
	return strings.TrimSpace(strings.Replace(strings.Replace(
		formatInt(value), " ", "", -1), "\n", "", -1))
}

func formatInt(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
