package storage

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Redaction is the placeholder written in place of a secret value.
const Redaction = "***REDACTED***"

// maxSecretLength bounds the rolling buffer used for masking.
const maxSecretLength = 4096

// SecretMasker redacts secret values from log output.
//
// The masker is boundary safe: it never releases a byte that could be the
// start of a secret, so a secret split across two log chunks is still
// redacted. The implementation is deliberately simple and literal; it
// matches byte sequences exactly and does not attempt to detect
// transformed or encoded secret material.
type SecretMasker struct {
	mu      sync.RWMutex
	secrets [][]byte
	pending []byte
}

// NewSecretMasker creates a masker for the given secret values.
func NewSecretMasker(values []string) *SecretMasker {
	masker := &SecretMasker{}
	seen := make(map[string]struct{}, len(values))
	ordered := make([]string, 0, len(values))
	for _, value := range values {
		if len(value) < 4 {
			// Very short values would redact far too much ordinary log
			// content, so they are ignored deliberately.
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		ordered = append(ordered, value)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, value := range ordered {
		masker.secrets = append(masker.secrets, []byte(value))
	}
	return masker
}

// HasSecrets reports whether the masker has any active secrets.
func (m *SecretMasker) HasSecrets() bool { return len(m.secrets) > 0 }

// Add registers a new secret value.
//
// The value is inserted so that the longest secret is always matched
// first. A shorter secret that is a substring of a longer one must
// never break the longer one, because the remainder would leak.
func (m *SecretMasker) Add(value string) {
	if len(value) < 4 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, existing := range m.secrets {
		if string(existing) == value {
			return
		}
	}
	m.secrets = append(m.secrets, []byte(value))
	sort.SliceStable(m.secrets, func(i, j int) bool {
		return len(m.secrets[i]) > len(m.secrets[j])
	})
}

// Mask redacts a complete buffer.
func (m *SecretMasker) Mask(data []byte) []byte {
	out, _ := m.consume(data, true)
	return out
}

// consume releases the safe prefix of pending and returns the remainder
// that must be retained for the next chunk.
func (m *SecretMasker) consume(pending []byte, final bool) ([]byte, []byte) {
	m.mu.RLock()
	secrets := m.secrets
	m.mu.RUnlock()
	if len(secrets) == 0 {
		return pending, nil
	}
	var output bytes.Buffer
	for {
		if len(pending) == 0 {
			return output.Bytes(), nil
		}
		if final {
			matched, length := earliestMatch(secrets, pending)
			if matched < 0 {
				output.Write(pending)
				return output.Bytes(), nil
			}
			output.Write(pending[:matched])
			output.WriteString(Redaction)
			pending = pending[matched+length:]
			continue
		}
		// Retain the longest possible partial secret so that a secret
		// spanning a chunk boundary is still matched.
		keep := keepLength(secrets)
		if keep > len(pending) {
			keep = len(pending)
		}
		cut := len(pending) - keep
		matched, length := earliestMatch(secrets, pending)
		if matched >= 0 && matched < cut {
			output.Write(pending[:matched])
			output.WriteString(Redaction)
			pending = pending[matched+length:]
			continue
		}
		output.Write(pending[:cut])
		return output.Bytes(), append([]byte(nil), pending[cut:]...)
	}
}

// keepLength returns the number of trailing bytes to retain.
func keepLength(secrets [][]byte) int {
	longest := 0
	for _, secret := range secrets {
		if len(secret) > longest {
			longest = len(secret)
		}
	}
	if longest > maxSecretLength {
		longest = maxSecretLength
	}
	return longest - 1
}

// earliestMatch returns the index and length of the first secret found.
func earliestMatch(secrets [][]byte, data []byte) (int, int) {
	best := -1
	bestLength := 0
	for _, secret := range secrets {
		index := bytes.Index(data, secret)
		if index < 0 {
			continue
		}
		if best < 0 || index < best || (index == best && len(secret) > bestLength) {
			best = index
			bestLength = len(secret)
		}
	}
	return best, bestLength
}

// Broker distributes log chunks to live subscribers.
type Broker struct {
	mu          sync.Mutex
	subscribers map[string]map[int]chan []byte
	nextID      int
}

// NewBroker creates an empty broker.
func NewBroker() *Broker {
	return &Broker{subscribers: make(map[string]map[int]chan []byte)}
}

// Subscribe registers a listener for a run and returns a cancel function.
func (b *Broker) Subscribe(runID string) (<-chan []byte, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subscribers[runID] == nil {
		b.subscribers[runID] = make(map[int]chan []byte)
	}
	id := b.nextID
	b.nextID++
	channel := make(chan []byte, 256)
	b.subscribers[runID][id] = channel
	return channel, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if set, ok := b.subscribers[runID]; ok {
			if existing, ok := set[id]; ok {
				delete(set, id)
				close(existing)
			}
			if len(set) == 0 {
				delete(b.subscribers, runID)
			}
		}
	}
}

// Publish delivers a chunk to every subscriber of a run.
func (b *Broker) Publish(runID string, chunk []byte) {
	if len(chunk) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, channel := range b.subscribers[runID] {
		copied := append([]byte(nil), chunk...)
		select {
		case channel <- copied:
		default:
			// A slow consumer must never block the writer. Drop the
			// oldest buffered chunk and keep the newest output.
			select {
			case <-channel:
			default:
			}
			select {
			case channel <- copied:
			default:
			}
		}
	}
}

// Tiered manages the hot local tier and the durable cold tier.
type Tiered struct {
	store  ObjectStore
	prefix string
	hotDir string
	masker *SecretMasker
	broker *Broker

	// Log batching configuration. When batchSize > 0, log writes are
	// buffered and flushed in batches to reduce syscall overhead and
	// object store round trips.
	logBatchSize   int
	logFlushPeriod time.Duration

	mu      sync.Mutex
	streams map[string]*logStream
}

type logStream struct {
	file       *os.File
	pending    []byte
	batchBuf   *bytes.Buffer
	batchBytes int
	lastFlush  time.Time
}

// TieredOptions configures the tiered storage.
type TieredOptions struct {
	// LogBatchSize is the maximum size of a log batch before flushing.
	// Zero disables batching (immediate write). Default: 1MiB.
	LogBatchSize int
	// LogFlushPeriod is the maximum time between flushes.
	// Default: 1 second.
	LogFlushPeriod time.Duration
}

// NewTiered creates the tiered storage facade.
func NewTiered(store ObjectStore, prefix, hotDir string, masker *SecretMasker, opts TieredOptions) (*Tiered, error) {
	if store == nil {
		return nil, fmt.Errorf("object store is required")
	}
	if hotDir == "" {
		return nil, fmt.Errorf("hot directory is required")
	}
	if err := os.MkdirAll(hotDir, 0o700); err != nil {
		return nil, fmt.Errorf("create hot directory: %w", err)
	}
	if masker == nil {
		masker = NewSecretMasker(nil)
	}
	if opts.LogBatchSize <= 0 {
		opts.LogBatchSize = 1024 * 1024 // 1 MiB default
	}
	if opts.LogFlushPeriod <= 0 {
		opts.LogFlushPeriod = time.Second
	}
	return &Tiered{
		store:          store,
		prefix:         strings.Trim(prefix, "/"),
		hotDir:         hotDir,
		masker:         masker,
		broker:         NewBroker(),
		streams:        make(map[string]*logStream),
		logBatchSize:   opts.LogBatchSize,
		logFlushPeriod: opts.LogFlushPeriod,
	}, nil
}

// Probe verifies that the hot tier is writable. It is used by the
// health monitor.
func (t *Tiered) Probe(ctx context.Context) error {
	probe := filepath.Join(t.hotDir, ".probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return err
	}
	return os.Remove(probe)
}

// AddSecret registers a value that must be redacted from logs.
//
// The masker is normally seeded from the controller configuration, but
// a secret that is injected into a worker at run time is only known
// then. Registering it here means the value is redacted from the
// moment it is injected, not from the moment the state is persisted.
func (t *Tiered) AddSecret(value string) {
	if len(value) < 4 {
		return
	}
	t.masker.Add(value)
}

// Broker exposes the log broker for the SSE endpoint.
func (t *Tiered) Broker() *Broker { return t.broker }

// Store exposes the underlying object store.
func (t *Tiered) Store() ObjectStore { return t.store }

// StateKey returns the durable key for a run state document.
func (t *Tiered) StateKey(runID string) string {
	return fmt.Sprintf("%s/runs/%s/state.json", t.prefix, runID)
}

// ActiveLogKey returns the durable key for the live log of a run.
func (t *Tiered) ActiveLogKey(runID string) string {
	return fmt.Sprintf("%s/runs/%s/logs.txt", t.prefix, runID)
}

// SaveState atomically persists a run state document to the hot tier and
// mirrors it to the durable object store.
func (t *Tiered) SaveState(ctx context.Context, runID string, state any) error {
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode run state: %w", err)
	}
	if err := t.writeAtomic(t.localStatePath(runID), encoded); err != nil {
		return err
	}
	return t.store.Put(ctx, t.StateKey(runID), encoded, "application/json")
}

// LoadState loads a run state document from the durable object store.
func (t *Tiered) LoadState(ctx context.Context, runID string, out any) error {
	data, err := t.store.Get(ctx, t.StateKey(runID))
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

// ListRuns returns the identifiers of every persisted run.
func (t *Tiered) ListRuns(ctx context.Context) ([]string, error) {
	keys, err := t.store.List(ctx, fmt.Sprintf("%s/runs/", t.prefix))
	if err != nil {
		return nil, err
	}
	seen := make(map[string]struct{})
	for _, key := range keys {
		if !strings.HasSuffix(key, "/state.json") {
			continue
		}
		parts := strings.Split(key, "/")
		if len(parts) < 2 {
			continue
		}
		seen[parts[len(parts)-2]] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for id := range seen {
		result = append(result, id)
	}
	sort.Strings(result)
	return result, nil
}

// AppendLog appends a log chunk, redacting secrets before the data
// reaches disk, the object store or any live subscriber.
//
// When batching is enabled (LogBatchSize > 0), writes are accumulated
// in an in-memory buffer and flushed when either the buffer reaches
// LogBatchSize or LogFlushPeriod elapses. This dramatically reduces
// syscall overhead and object store round trips under high log volume.
func (t *Tiered) AppendLog(ctx context.Context, runID string, chunk []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	stream, err := t.streamFor(runID)
	if err != nil {
		return err
	}

	combined := append(append([]byte(nil), stream.pending...), chunk...)
	safe, remainder := t.masker.consume(combined, false)
	stream.pending = remainder

	if len(safe) == 0 {
		return nil
	}

	// If batching is disabled, write immediately (legacy behavior).
	if t.logBatchSize <= 0 {
		if _, err := stream.file.Write(safe); err != nil {
			return fmt.Errorf("write hot log: %w", err)
		}
		t.broker.Publish(runID, safe)
		return nil
	}

	// Batching enabled: accumulate in batchBuf.
	stream.batchBuf.Write(safe)
	stream.batchBytes += len(safe)

	// Flush if batch is full or flush period elapsed.
	shouldFlush := stream.batchBytes >= t.logBatchSize ||
		time.Since(stream.lastFlush) >= t.logFlushPeriod

	if shouldFlush {
		return t.flushStreamLocked(stream, runID)
	}
	return nil
}

// flushStreamLocked writes the accumulated batch to disk and publishes
// to subscribers. Caller must hold t.mu.
func (t *Tiered) flushStreamLocked(stream *logStream, runID string) error {
	if stream.batchBuf.Len() == 0 {
		return nil
	}
	data := stream.batchBuf.Bytes()
	if _, err := stream.file.Write(data); err != nil {
		return fmt.Errorf("write hot log: %w", err)
	}
	t.broker.Publish(runID, data)
	stream.batchBuf.Reset()
	stream.batchBytes = 0
	stream.lastFlush = time.Now()
	return nil
}

// FlushLog releases the masked tail of a run log and flushes any
// pending batched data.
func (t *Tiered) FlushLog(ctx context.Context, runID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	stream, ok := t.streams[runID]
	if !ok {
		return nil
	}
	// Flush any masked pending data first.
	if len(stream.pending) > 0 {
		safe, _ := t.masker.consume(stream.pending, true)
		stream.pending = nil
		if len(safe) > 0 {
			stream.batchBuf.Write(safe)
			stream.batchBytes += len(safe)
		}
	}
	// Flush the batch buffer.
	return t.flushStreamLocked(stream, runID)
}

// ReadLog returns the current hot log contents of a run.
func (t *Tiered) ReadLog(ctx context.Context, runID string) ([]byte, error) {
	t.mu.Lock()
	stream, ok := t.streams[runID]
	t.mu.Unlock()
	if ok {
		if err := stream.file.Sync(); err != nil {
			return nil, err
		}
		return os.ReadFile(stream.file.Name())
	}
	return os.ReadFile(t.localLogPath(runID))
}

// ArchiveRun compresses the run log, uploads it to the durable tier and
// removes the local hot copy.
func (t *Tiered) ArchiveRun(ctx context.Context, runID string) error {
	if err := t.FlushLog(ctx, runID); err != nil {
		return err
	}
	logPath := t.localLogPath(runID)
	compressed, err := compressFile(logPath)
	if err != nil {
		return err
	}
	if err := t.store.Put(ctx, t.ActiveLogKey(runID)+".gz", compressed, "application/gzip"); err != nil {
		return err
	}
	t.mu.Lock()
	if stream, ok := t.streams[runID]; ok {
		stream.file.Close()
		delete(t.streams, runID)
	}
	t.mu.Unlock()
	_ = os.Remove(logPath)
	return nil
}

// LoadArchivedLog decompresses an archived run log.
func (t *Tiered) LoadArchivedLog(ctx context.Context, runID string) ([]byte, error) {
	compressed, err := t.store.Get(ctx, t.ActiveLogKey(runID)+".gz")
	if err != nil {
		return nil, err
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("open archived log: %w", err)
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// Subscribe registers a live log listener.
func (t *Tiered) Subscribe(runID string) (<-chan []byte, func()) {
	return t.broker.Subscribe(runID)
}

// streamFor returns the open log stream for a run, creating it lazily.
func (t *Tiered) streamFor(runID string) (*logStream, error) {
	if stream, ok := t.streams[runID]; ok {
		return stream, nil
	}
	path := t.localLogPath(runID)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open hot log: %w", err)
	}
	stream := &logStream{
		file:      file,
		batchBuf:  &bytes.Buffer{},
		lastFlush: time.Now(),
	}
	t.streams[runID] = stream
	return stream, nil
}

func (t *Tiered) localStatePath(runID string) string {
	return filepath.Join(t.hotDir, "runs", runID, "state.json")
}

func (t *Tiered) localLogPath(runID string) string {
	return filepath.Join(t.hotDir, "runs", runID, "logs.txt")
}

// writeAtomic writes a file through a temporary file and a rename.
func (t *Tiered) writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// compressFile gzip compresses a file to a temporary file and returns its
// path.
func compressFile(path string) ([]byte, error) {
	source, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer source.Close()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := io.Copy(writer, source); err != nil {
		writer.Close()
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
