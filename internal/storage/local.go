// Package storage provides the durable object plane used by Barq.
//
// The controller never talks to object storage directly; it uses the
// ObjectStore interface so that production deployments can use a durable
// S3-compatible backend while local development can use an embedded,
// explicitly non-durable store.
package storage

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("object not found")

// ObjectStore is the durable object plane contract.
type ObjectStore interface {
	// Backend returns the backend name for diagnostics.
	Backend() string
	// Put stores an object.
	Put(ctx context.Context, key string, data []byte, contentType string) error
	// PutFile stores a file without loading it fully into memory.
	PutFile(ctx context.Context, key, path, contentType string) error
	// Get retrieves an object.
	Get(ctx context.Context, key string) ([]byte, error)
	// GetTo streams an object to a writer.
	GetTo(ctx context.Context, key string, w io.Writer) error
	// Delete removes an object. Deleting a missing object is not an error.
	Delete(ctx context.Context, key string) error
	// List returns the keys with the given prefix.
	List(ctx context.Context, prefix string) ([]string, error)
	// Exists reports whether an object is present.
	Exists(ctx context.Context, key string) (bool, error)
	// Presign returns a time limited, method and object scoped URL.
	Presign(ctx context.Context, method, key string, ttl time.Duration) (string, error)
}

// RelayProvider is implemented by object stores that can also serve
// objects directly over HTTP for workers.
type RelayProvider interface {
	// RelayHandler serves the /_objects/ endpoint.
	RelayHandler() http.Handler
}

// Local is a simple filesystem backed object store.
//
// It is intended for local development only. It is leader local, it is
// NOT durable across controller replacement, and it is NOT S3
// compatible. Production deployments must use an S3 backend.
type Local struct {
	root      string
	publicURL string
	secret    []byte
	now       func() time.Time
}

// NewLocal creates a filesystem backed object store.
func NewLocal(root, publicURL string, secret []byte) (*Local, error) {
	if root == "" {
		return nil, fmt.Errorf("local object store root is required")
	}
	if len(secret) == 0 {
		return nil, fmt.Errorf("local object store signing secret is required")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create local object root: %w", err)
	}
	return &Local{
		root:      root,
		publicURL: strings.TrimRight(publicURL, "/"),
		secret:    secret,
		now:       time.Now,
	}, nil
}

// Backend implements ObjectStore.
func (l *Local) Backend() string { return "local" }

// Put implements ObjectStore.
func (l *Local) Put(ctx context.Context, key string, data []byte, contentType string) error {
	path, err := l.pathFor(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".upload-*")
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

// PutFile implements ObjectStore.
func (l *Local) PutFile(ctx context.Context, key, path, contentType string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return l.Put(ctx, key, data, contentType)
}

// Get implements ObjectStore.
func (l *Local) Get(ctx context.Context, key string) ([]byte, error) {
	path, err := l.pathFor(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return data, err
}

// GetTo implements ObjectStore.
func (l *Local) GetTo(ctx context.Context, key string, w io.Writer) error {
	path, err := l.pathFor(key)
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = io.Copy(w, file)
	return err
}

// Delete implements ObjectStore.
func (l *Local) Delete(ctx context.Context, key string) error {
	path, err := l.pathFor(key)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// List implements ObjectStore.
func (l *Local) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	err := filepath.WalkDir(l.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(l.root, path)
		if err != nil {
			return err
		}
		key := filepath.ToSlash(relative)
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(keys)
	return keys, nil
}

// Exists implements ObjectStore.
func (l *Local) Exists(ctx context.Context, key string) (bool, error) {
	path, err := l.pathFor(key)
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Presign implements ObjectStore.
func (l *Local) Presign(ctx context.Context, method, key string, ttl time.Duration) (string, error) {
	if l.publicURL == "" {
		return "", fmt.Errorf("local object store requires a public URL to presign URLs")
	}
	expires := l.now().Add(ttl).Unix()
	signature := l.sign(method, key, expires)
	return fmt.Sprintf("%s/_objects/%s?expires=%d&signature=%s",
		l.publicURL, escapeObjectKey(key), expires, signature), nil
}

// RelayHandler implements RelayProvider.
//
// The handler is mounted on "/_objects/{key...}" so the object key is
// available through the request path value.
func (l *Local) RelayHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.PathValue("key")
		if key == "" {
			key = strings.TrimPrefix(r.URL.Path, "/_objects/")
		}
		key, err := url.PathUnescape(key)
		if err != nil {
			http.Error(w, "invalid key", http.StatusBadRequest)
			return
		}
		if _, err := sanitizeKey(key); err != nil {
			http.Error(w, "invalid key", http.StatusBadRequest)
			return
		}
		expiresRaw := r.URL.Query().Get("expires")
		signature := r.URL.Query().Get("signature")
		expires, err := strconv.ParseInt(expiresRaw, 10, 64)
		if err != nil {
			http.Error(w, "missing expiry", http.StatusUnauthorized)
			return
		}
		if l.now().Unix() > expires {
			http.Error(w, "expired capability", http.StatusUnauthorized)
			return
		}
		expected := l.sign(r.Method, key, expires)
		if subtle.ConstantTimeCompare([]byte(expected), []byte(signature)) != 1 {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			data, err := l.Get(r.Context(), key)
			if errors.Is(err, ErrNotFound) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			if err != nil {
				http.Error(w, "read failed", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(data)
		case http.MethodPut:
			body, err := io.ReadAll(io.LimitReader(r.Body, maxRelayObjectBytes))
			if err != nil {
				http.Error(w, "read failed", http.StatusBadRequest)
				return
			}
			if err := l.Put(r.Context(), key, body, "application/octet-stream"); err != nil {
				http.Error(w, "write failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

// sign computes the HMAC capability for an object operation.
func (l *Local) sign(method, key string, expires int64) string {
	mac := hmac.New(sha256.New, l.secret)
	fmt.Fprintf(mac, "%s\n%s\n%d", method, key, expires)
	return hex.EncodeToString(mac.Sum(nil))
}

// pathFor maps an object key to a filesystem path, rejecting traversal.
func (l *Local) pathFor(key string) (string, error) {
	cleaned, err := sanitizeKey(key)
	if err != nil {
		return "", err
	}
	return filepath.Join(l.root, filepath.FromSlash(cleaned)), nil
}

// sanitizeKey validates an object key and rejects traversal attempts.
func sanitizeKey(key string) (string, error) {
	if key == "" {
		return "", fmt.Errorf("object key must not be empty")
	}
	if strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("object key must not be absolute")
	}
	if strings.Contains(key, "..") {
		return "", fmt.Errorf("object key must not contain path traversal segments")
	}
	if strings.ContainsAny(key, "\x00") {
		return "", fmt.Errorf("object key must not contain null bytes")
	}
	return strings.TrimPrefix(key, "./"), nil
}

// escapeObjectKey escapes a key for use in a URL path.
func escapeObjectKey(key string) string {
	parts := strings.Split(key, "/")
	for index, part := range parts {
		parts[index] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

const maxRelayObjectBytes = 2 << 30
