package secrets

import (
	"regexp"
	"testing"
)

func TestMappingRecordsSecretValues(t *testing.T) {
	mapping := NewMapping()
	mapping.Set("TOKEN", "secret-value")
	mapping.Set("EMPTY", "")

	if !mapping.IsSecret("TOKEN") {
		t.Fatal("expected TOKEN to be recorded as a secret")
	}
	if mapping.IsSecret("EMPTY") {
		t.Fatal("an empty value must not be recorded")
	}
	if len(mapping.Values()) != 1 {
		t.Fatalf("expected 1 value, got %d", len(mapping.Values()))
	}
}

func TestMappingOrdersValuesLongestFirst(t *testing.T) {
	// The longest secret must be redacted first, otherwise a shorter
	// secret that is a substring of a longer one breaks the longer one
	// and the remainder leaks.
	mapping := NewMapping()
	mapping.Set("A", "abcd")
	mapping.Set("B", "abcdefgh")

	values := mapping.Values()
	if len(values) != 2 {
		t.Fatalf("expected 2 values, got %d", len(values))
	}
	if values[0] != "abcdefgh" {
		t.Fatalf("expected the longest value first, got %q", values[0])
	}
}

func TestRedactorMatchesTheLongestSecret(t *testing.T) {
	pattern := Redactor([]string{"abcd", "abcdefgh"})
	if pattern == "" {
		t.Fatal("expected a pattern")
	}
	expression := regexp.MustCompile(pattern)
	redacted := expression.ReplaceAllString("value abcdefgh value", "***")
	if redacted != "value *** value" {
		t.Fatalf("unexpected redaction: %q", redacted)
	}
}

func TestRedactorEscapesSpecialCharacters(t *testing.T) {
	// A secret containing a regular expression metacharacter must
	// still be matched literally.
	pattern := Redactor([]string{"a.b*c"})
	expression := regexp.MustCompile(pattern)
	if !expression.MatchString("a.b*c") {
		t.Fatal("expected the literal value to match")
	}
	if expression.MatchString("axbxc") {
		t.Fatal("expected the metacharacters to be escaped")
	}
}

func TestRedactorWithNoValues(t *testing.T) {
	if Redactor(nil) != "" {
		t.Fatal("expected an empty pattern")
	}
}

func TestMappingMerge(t *testing.T) {
	first := NewMapping()
	first.Set("A", "value-a")
	second := NewMapping()
	second.Set("B", "value-b")

	first.Merge(second)
	if !first.IsSecret("A") || !first.IsSecret("B") {
		t.Fatal("expected the merged secrets to be present")
	}
}
