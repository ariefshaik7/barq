package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

// secretStoreKey is the durable key of the encrypted secret database.
const secretStoreKey = "config/secrets.v1"

// SecretMetadata describes a registered secret without exposing its value.
type SecretMetadata struct {
	Name      string    `json:"name"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	// Fingerprint is a truncated digest of the value. It allows cache
	// invalidation and change detection without revealing the secret.
	Fingerprint string `json:"fingerprint"`
}

// secretEntry is the decrypted representation of one secret.
type secretEntry struct {
	Metadata SecretMetadata `json:"metadata"`
	Value    string         `json:"value"`
}

// secretDatabase is the plaintext form of the secret store.
type secretDatabase struct {
	Version int                    `json:"version"`
	Secrets map[string]secretEntry `json:"secrets"`
}

// SecretVault stores secrets encrypted with an authenticated cipher.
//
// The master key is supplied from outside the secret database, so a
// compromise of the object store alone does not reveal secret values.
type SecretVault struct {
	store ObjectStore
	aead  cipher.AEAD
	mu    sync.Mutex
}

// NewSecretVault creates a vault using the supplied 32 byte master key.
func NewSecretVault(store ObjectStore, masterKey []byte) (*SecretVault, error) {
	if store == nil {
		return nil, fmt.Errorf("object store is required")
	}
	if len(masterKey) != 32 {
		return nil, fmt.Errorf("secret vault requires a 32 byte master key")
	}
	block, err := aes.NewCipher(masterKey)
	if err != nil {
		return nil, fmt.Errorf("create AES cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("create GCM: %w", err)
	}
	return &SecretVault{store: store, aead: aead}, nil
}

// Set registers or replaces a secret value.
func (v *SecretVault) Set(ctx context.Context, name, value string) (SecretMetadata, error) {
	if name == "" {
		return SecretMetadata{}, fmt.Errorf("secret name is required")
	}
	if value == "" {
		return SecretMetadata{}, fmt.Errorf("secret value must not be empty")
	}
	if !validSecretName(name) {
		return SecretMetadata{}, fmt.Errorf("secret name %q must match [A-Za-z_][A-Za-z0-9_]*", name)
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	database, err := v.load(ctx)
	if err != nil {
		return SecretMetadata{}, err
	}
	now := time.Now().UTC()
	version := 1
	if existing, ok := database.Secrets[name]; ok {
		version = existing.Metadata.Version + 1
	}
	entry := secretEntry{
		Metadata: SecretMetadata{
			Name:        name,
			Version:     version,
			CreatedAt:   now,
			UpdatedAt:   now,
			Fingerprint: fingerprint(value),
		},
		Value: value,
	}
	database.Secrets[name] = entry
	if err := v.save(ctx, database); err != nil {
		return SecretMetadata{}, err
	}
	return entry.Metadata, nil
}

// Delete removes a secret.
func (v *SecretVault) Delete(ctx context.Context, name string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	database, err := v.load(ctx)
	if err != nil {
		return err
	}
	if _, ok := database.Secrets[name]; !ok {
		return fmt.Errorf("secret %q not found", name)
	}
	delete(database.Secrets, name)
	return v.save(ctx, database)
}

// List returns the metadata of every registered secret.
func (v *SecretVault) List(ctx context.Context) ([]SecretMetadata, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	database, err := v.load(ctx)
	if err != nil {
		return nil, err
	}
	result := make([]SecretMetadata, 0, len(database.Secrets))
	for _, entry := range database.Secrets {
		result = append(result, entry.Metadata)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result, nil
}

// Value returns the plaintext value of a secret. It is used only to
// inject values into worker environments.
func (v *SecretVault) Value(ctx context.Context, name string) (string, SecretMetadata, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	database, err := v.load(ctx)
	if err != nil {
		return "", SecretMetadata{}, err
	}
	entry, ok := database.Secrets[name]
	if !ok {
		return "", SecretMetadata{}, fmt.Errorf("secret %q not found", name)
	}
	return entry.Value, entry.Metadata, nil
}

// Versions returns a stable digest of the secret set. It is used in
// cache fingerprints so that rotating a secret invalidates cached steps.
func (v *SecretVault) Versions(ctx context.Context, names []string) (string, error) {
	if len(names) == 0 {
		return "", nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	database, err := v.load(ctx)
	if err != nil {
		return "", err
	}
	ordered := append([]string(nil), names...)
	sort.Strings(ordered)
	hasher := sha256.New()
	for _, name := range ordered {
		entry, ok := database.Secrets[name]
		if !ok {
			return "", fmt.Errorf("secret %q not found", name)
		}
		fmt.Fprintf(hasher, "%s=%d:%s\n", name, entry.Metadata.Version, entry.Metadata.Fingerprint)
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

// load reads and decrypts the secret database.
func (v *SecretVault) load(ctx context.Context) (*secretDatabase, error) {
	raw, err := v.store.Get(ctx, secretStoreKey)
	if errors.Is(err, ErrNotFound) {
		return &secretDatabase{Version: 1, Secrets: map[string]secretEntry{}}, nil
	}
	if err != nil {
		return nil, err
	}
	if len(raw) < v.aead.NonceSize() {
		return nil, fmt.Errorf("secret store is truncated")
	}
	nonce := raw[:v.aead.NonceSize()]
	ciphertext := raw[v.aead.NonceSize():]
	plaintext, err := v.aead.Open(nil, nonce, ciphertext, []byte(secretStoreKey))
	if err != nil {
		return nil, fmt.Errorf("decrypt secret store: %w", err)
	}
	database := &secretDatabase{Secrets: map[string]secretEntry{}}
	if err := json.Unmarshal(plaintext, database); err != nil {
		return nil, fmt.Errorf("decode secret store: %w", err)
	}
	if database.Secrets == nil {
		database.Secrets = map[string]secretEntry{}
	}
	return database, nil
}

// save encrypts and writes the secret database.
func (v *SecretVault) save(ctx context.Context, database *secretDatabase) error {
	plaintext, err := json.Marshal(database)
	if err != nil {
		return err
	}
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return err
	}
	ciphertext := v.aead.Seal(nil, nonce, plaintext, []byte(secretStoreKey))
	return v.store.Put(ctx, secretStoreKey, append(nonce, ciphertext...), "application/octet-stream")
}

// fingerprint returns a short, non-reversible identifier for a value.
func fingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// validSecretName reports whether a name is a safe environment variable.
func validSecretName(name string) bool {
	if name == "" {
		return false
	}
	for index, char := range name {
		switch {
		case char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z', char == '_':
		case char >= '0' && char <= '9' && index > 0:
		default:
			return false
		}
	}
	return true
}
