// Package secrets tracks which environment variables hold secret
// values.
//
// The pattern is the one that mature CI systems converge on: the
// controller does not merely inject a secret into a worker, it also
// records that it did. The record is what allows the log masker to
// redact exactly the injected values, without guessing from a global
// list of known secrets, and what allows the runtime to tell the
// difference between a secret the author wrote and a value that
// happens to look like one.
package secrets

import (
	"sort"
	"strings"
	"sync"
)

// Mapping records the environment variables that hold secrets.
//
// The keys are the environment variable names and the values are the
// secret values that were injected into them.
type Mapping struct {
	mu      sync.RWMutex
	entries map[string]string
	// order preserves the injection order so that the redaction is
	// deterministic.
	order []string
}

// NewMapping creates an empty mapping.
func NewMapping() *Mapping {
	return &Mapping{entries: make(map[string]string)}
}

// Set records that an environment variable holds a secret value.
//
// An empty value is ignored, because redacting the empty string would
// replace every character in every log line.
func (m *Mapping) Set(name, value string) {
	if name == "" || value == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[name]; !exists {
		m.order = append(m.order, name)
	}
	m.entries[name] = value
}

// Values returns the secret values, longest first.
//
// The order matters. A shorter secret that is a substring of a longer
// one must be redacted last, otherwise the longer one is broken by the
// shorter one and the remainder leaks.
func (m *Mapping) Values() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	values := make([]string, 0, len(m.entries))
	for _, value := range m.entries {
		values = append(values, value)
	}
	sort.SliceStable(values, func(i, j int) bool {
		return len(values[i]) > len(values[j])
	})
	return values
}

// Names returns the environment variable names that hold secrets.
func (m *Mapping) Names() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, len(m.order))
	copy(names, m.order)
	return names
}

// IsSecret reports whether an environment variable holds a secret.
func (m *Mapping) IsSecret(name string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	_, exists := m.entries[name]
	return exists
}

// Merge folds another mapping into this one.
func (m *Mapping) Merge(other *Mapping) {
	if other == nil {
		return
	}
	for _, name := range other.Names() {
		other.mu.RLock()
		value := other.entries[name]
		other.mu.RUnlock()
		m.Set(name, value)
	}
}

// Redactor renders a set of secret values as a replacement
// expression.
//
// The expression is a single regular expression with alternation. The
// alternatives are ordered longest first so that the longest secret
// wins, and the whole match is replaced in one pass, so a short secret
// can never break a long one.
func Redactor(values []string) string {
	if len(values) == 0 {
		return ""
	}
	ordered := append([]string(nil), values...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return len(ordered[i]) > len(ordered[j])
	})
	parts := make([]string, 0, len(ordered))
	for _, value := range ordered {
		if value == "" {
			continue
		}
		parts = append(parts, regexpQuote(value))
	}
	if len(parts) == 0 {
		return ""
	}
	return "(?:" + strings.Join(parts, "|") + ")"
}

// regexpQuote escapes a value for use in a regular expression.
func regexpQuote(value string) string {
	var builder strings.Builder
	for _, char := range value {
		switch char {
		case '.', '+', '*', '?', '(', ')', '[', ']', '{', '}', '^', '$', '|', '\\':
			builder.WriteByte('\\')
			builder.WriteRune(char)
		default:
			builder.WriteRune(char)
		}
	}
	return builder.String()
}
