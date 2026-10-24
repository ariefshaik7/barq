package yaml

import (
	"encoding/json"
	"fmt"
	"strings"
)

// validate checks a condition block for obvious mistakes.
func (w *When) validate() error {
	for _, event := range w.Event {
		switch event {
		case "push", "pull_request", "tag", "release", "manual", "cron", "deployment":
		default:
			return fmt.Errorf("unknown event %q in a when block", event)
		}
	}
	for _, status := range w.Status {
		switch status {
		case "success", "failure":
		default:
			return fmt.Errorf("unknown status %q in a when block", status)
		}
	}
	for dimension, values := range w.Matrix {
		if dimension == "" {
			return fmt.Errorf("a when block has an empty matrix dimension")
		}
		if len(values) == 0 {
			return fmt.Errorf("the matrix dimension %q in a when block has no values", dimension)
		}
	}
	return nil
}

// WhenList is a list of conditions.
//
// It accepts either a single mapping or a list of mappings, because a
// single condition is the common case and forcing the author to
// write a list for it is needless friction.
type WhenList []*When

// UnmarshalJSON implements json.Unmarshaler.
func (l *WhenList) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*l = nil
		return nil
	}
	if strings.HasPrefix(trimmed, "[") {
		var list []*When
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		*l = list
		return nil
	}
	var single When
	if err := json.Unmarshal(data, &single); err != nil {
		return err
	}
	*l = WhenList{&single}
	return nil
}

// Context carries the facts that a condition is evaluated against.
type Context struct {
	// Event is the trigger that started the run.
	Event string
	// Branch is the branch that was pushed.
	Branch string
	// Ref is the git reference.
	Ref string
	// Repository is the owner and name of the repository.
	Repository string
	// Instance is the forge instance.
	Instance string
	// Matrix is the matrix axis of the step being evaluated.
	Matrix map[string]string
	// Local reports whether the run is executing locally.
	Local bool
	// UpstreamStatus is the aggregate status of the upstream steps.
	UpstreamStatus string
}

// Match evaluates a set of conditions.
//
// The conditions within one block are combined with AND. Several
// blocks are combined with OR. An empty block matches everything.
//
// The status condition is deliberately asymmetric. A step with no
// status condition runs when the upstream steps succeeded, because
// success is the default expectation. A step that must run on failure
// has to say so explicitly.
func Match(conditions []*When, context Context) (bool, error) {
	if len(conditions) == 0 {
		return true, nil
	}
	for _, condition := range conditions {
		matched, err := condition.match(context)
		if err != nil {
			return false, err
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

// match evaluates a single condition block.
func (w *When) match(context Context) (bool, error) {
	if err := w.validate(); err != nil {
		return false, err
	}
	if len(w.Event) > 0 && !containsString(w.Event, context.Event) {
		return false, nil
	}
	if len(w.Branch) > 0 && !matchesGlob(w.Branch, context.Branch) {
		return false, nil
	}
	if len(w.Ref) > 0 && !matchesGlob(w.Ref, context.Ref) {
		return false, nil
	}
	if len(w.Repo) > 0 && !matchesGlob(w.Repo, context.Repository) {
		return false, nil
	}
	if len(w.Instance) > 0 && !containsString(w.Instance, context.Instance) {
		return false, nil
	}
	if w.Local != nil && *w.Local != context.Local {
		return false, nil
	}
	if len(w.Matrix) > 0 {
		for dimension, values := range w.Matrix {
			actual, ok := context.Matrix[dimension]
			if !ok {
				return false, nil
			}
			if !matchesGlob(values, actual) {
				return false, nil
			}
		}
	}
	if len(w.Status) > 0 {
		if !containsString(w.Status, context.UpstreamStatus) {
			return false, nil
		}
	}
	return true, nil
}

// containsString reports whether a slice contains a value.
func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// matchesGlob reports whether a value matches any of the patterns.
//
// A pattern may end with a star, which matches any suffix. This is
// the subset of glob syntax that is useful for branch and path
// conditions, and it avoids a dependency on a glob library.
func matchesGlob(patterns []string, value string) bool {
	for _, pattern := range patterns {
		if matchPattern(pattern, value) {
			return true
		}
	}
	return false
}

// matchPattern reports whether a value matches a single pattern.
func matchPattern(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(value, strings.TrimSuffix(pattern, "*"))
	}
	if strings.HasPrefix(pattern, "*") {
		return strings.HasSuffix(value, strings.TrimPrefix(pattern, "*"))
	}
	return pattern == value
}
