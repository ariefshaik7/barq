package controller

import "testing"

func TestMergeStatusPrefersTheMoreSevere(t *testing.T) {
	cases := []struct {
		a, b     Status
		expected Status
	}{
		{StatusSuccess, StatusFailure, StatusFailure},
		{StatusFailure, StatusSuccess, StatusFailure},
		{StatusRunning, StatusPending, StatusRunning},
		{StatusPending, StatusRunning, StatusRunning},
		{StatusError, StatusFailure, StatusError},
		{StatusFailure, StatusError, StatusError},
		{StatusKilled, StatusFailure, StatusKilled},
		{StatusCanceled, StatusCanceled, StatusCanceled},
		{StatusSuccess, StatusSkipped, StatusSuccess},
		{StatusCached, StatusSuccess, StatusCached},
	}
	for _, test := range cases {
		if got := MergeStatus(test.a, test.b); got != test.expected {
			t.Fatalf("MergeStatus(%q, %q) = %q, expected %q", test.a, test.b, got, test.expected)
		}
	}
}

func TestMergeStatusIsCommutative(t *testing.T) {
	statuses := []Status{
		StatusPending, StatusRunning, StatusSuccess, StatusFailure,
		StatusSkipped, StatusCanceled, StatusKilled, StatusError,
	}
	for _, a := range statuses {
		for _, b := range statuses {
			if MergeStatus(a, b) != MergeStatus(b, a) {
				t.Fatalf("MergeStatus is not commutative for %q and %q", a, b)
			}
		}
	}
}

func TestMergeStatusTreatsCancelAsKillWhenMerged(t *testing.T) {
	// A cancellation that is merged with another status means the
	// cancellation was not the final outcome of the work.
	if got := MergeStatus(StatusCanceled, StatusFailure); got != StatusKilled {
		t.Fatalf("expected killed, got %q", got)
	}
	if got := MergeStatus(StatusCanceled, StatusCanceled); got != StatusCanceled {
		t.Fatalf("expected canceled, got %q", got)
	}
}

func TestMergeStatusIsIdempotent(t *testing.T) {
	statuses := []Status{
		StatusPending, StatusRunning, StatusSuccess, StatusFailure,
		StatusSkipped, StatusCanceled, StatusKilled, StatusError,
	}
	for _, status := range statuses {
		if got := MergeStatus(status, status); got != status {
			t.Fatalf("MergeStatus(%q, %q) = %q", status, status, got)
		}
	}
}

func TestMergeStatuses(t *testing.T) {
	// One failure among many successes is a failure.
	result := MergeStatuses([]Status{StatusSuccess, StatusSuccess, StatusFailure, StatusSuccess})
	if result != StatusFailure {
		t.Fatalf("expected failure, got %q", result)
	}
	// An empty list is pending, not success.
	if got := MergeStatuses(nil); got != StatusPending {
		t.Fatalf("expected pending, got %q", got)
	}
}

func TestStatusIsTerminal(t *testing.T) {
	terminal := []Status{StatusSuccess, StatusFailure, StatusCanceled, StatusKilled, StatusError, StatusSkipped}
	for _, status := range terminal {
		if !status.IsTerminal() {
			t.Fatalf("%q should be terminal", status)
		}
	}
	active := []Status{StatusPending, StatusRunning, StatusCached, StatusSpotMigrating}
	for _, status := range active {
		if status.IsTerminal() {
			t.Fatalf("%q should not be terminal", status)
		}
	}
}

func TestStatusIsFailure(t *testing.T) {
	failures := []Status{StatusFailure, StatusError, StatusKilled}
	for _, status := range failures {
		if !status.IsFailure() {
			t.Fatalf("%q should be a failure", status)
		}
	}
	nonFailures := []Status{StatusSuccess, StatusPending, StatusRunning, StatusCached, StatusSkipped}
	for _, status := range nonFailures {
		if status.IsFailure() {
			t.Fatalf("%q should not be a failure", status)
		}
	}
}

func TestParseStatus(t *testing.T) {
	status, err := ParseStatus("success")
	if err != nil {
		t.Fatal(err)
	}
	if status != StatusSuccess {
		t.Fatalf("expected success, got %q", status)
	}
	if _, err := ParseStatus("nonsense"); err == nil {
		t.Fatal("expected an error for an unknown status")
	}
}
