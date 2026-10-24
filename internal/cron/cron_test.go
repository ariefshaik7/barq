package cron

import (
	"testing"
	"time"
)

func TestParseValidExpressions(t *testing.T) {
	cases := []string{
		"* * * * *",
		"0 3 * * *",
		"*/15 * * * *",
		"0 0 1 1 *",
		"30 2 * * 1-5",
		"0,30 * * * *",
	}
	for _, expression := range cases {
		if _, err := Parse(expression); err != nil {
			t.Fatalf("the expression %q was rejected: %v", expression, err)
		}
	}
}

func TestParseRejectsInvalidExpressions(t *testing.T) {
	cases := []string{
		"",
		"* * * *",
		"* * * * * *",
		"60 * * * *",
		"* 24 * * *",
		"* * 0 * *",
		"* * * 13 *",
		"* * * * 7",
		"a * * * *",
		"5-1 * * * *",
	}
	for _, expression := range cases {
		if _, err := Parse(expression); err == nil {
			t.Fatalf("the expression %q was accepted", expression)
		}
	}
}

func TestScheduleMatches(t *testing.T) {
	schedule, err := Parse("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	// The 3rd of January at 03:00 UTC.
	if !schedule.Match(time.Date(2026, 1, 3, 3, 0, 0, 0, time.UTC)) {
		t.Fatal("expected the schedule to match")
	}
	if schedule.Match(time.Date(2026, 1, 3, 4, 0, 0, 0, time.UTC)) {
		t.Fatal("expected the schedule not to match")
	}
}

func TestScheduleMatchesEveryFifteenMinutes(t *testing.T) {
	schedule, err := Parse("*/15 * * * *")
	if err != nil {
		t.Fatal(err)
	}
	for _, minute := range []int{0, 15, 30, 45} {
		if !schedule.Match(time.Date(2026, 1, 3, 4, minute, 0, 0, time.UTC)) {
			t.Fatalf("expected minute %d to match", minute)
		}
	}
	if schedule.Match(time.Date(2026, 1, 3, 4, 7, 0, 0, time.UTC)) {
		t.Fatal("expected minute 7 not to match")
	}
}

func TestScheduleNext(t *testing.T) {
	schedule, err := Parse("0 3 * * *")
	if err != nil {
		t.Fatal(err)
	}
	after := time.Date(2026, 1, 3, 4, 0, 0, 0, time.UTC)
	next, ok := schedule.Next(after)
	if !ok {
		t.Fatal("expected a next occurrence")
	}
	if next.Hour() != 3 || next.Minute() != 0 {
		t.Fatalf("expected the next occurrence at 03:00, got %s", next)
	}
	if !next.After(after) {
		t.Fatal("the next occurrence must be after the reference time")
	}
}

func TestStoreLifecycle(t *testing.T) {
	store := NewStore()
	if err := store.Put(Entry{
		Name:     "nightly",
		Pipeline: "release",
		Schedule: "0 3 * * *",
		Branch:   "main",
		Enabled:  true,
	}); err != nil {
		t.Fatal(err)
	}
	entry, ok := store.Get("nightly")
	if !ok {
		t.Fatal("expected the entry")
	}
	if entry.NextRun.IsZero() {
		t.Fatal("expected the next run to be calculated")
	}
	if len(store.List()) != 1 {
		t.Fatal("expected one entry")
	}
	store.Delete("nightly")
	if _, ok := store.Get("nightly"); ok {
		t.Fatal("expected the entry to be deleted")
	}
}

func TestStoreDueFiresOncePerMinute(t *testing.T) {
	store := NewStore()
	if err := store.Put(Entry{
		Name:     "every-minute",
		Pipeline: "smoke",
		Schedule: "* * * * *",
		Enabled:  true,
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 3, 4, 5, 0, 0, time.UTC)
	due := store.Due(now)
	if len(due) != 1 {
		t.Fatalf("expected 1 due entry, got %d", len(due))
	}
	// The same minute must not fire again.
	due = store.Due(now.Add(30 * time.Second))
	if len(due) != 0 {
		t.Fatalf("expected the entry not to fire twice, got %d", len(due))
	}
	// The next minute fires.
	due = store.Due(now.Add(time.Minute))
	if len(due) != 1 {
		t.Fatalf("expected the entry to fire again, got %d", len(due))
	}
}

func TestStoreRejectsAnInvalidSchedule(t *testing.T) {
	store := NewStore()
	if err := store.Put(Entry{Name: "bad", Schedule: "not a schedule"}); err == nil {
		t.Fatal("expected an error for an invalid schedule")
	}
}
