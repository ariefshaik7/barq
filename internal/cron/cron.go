// Package cron implements the scheduled pipeline feature.
//
// A cron entry names a pipeline, a schedule and a branch. The
// scheduler fires the entry when the schedule is due and the branch
// has a new commit.
//
// The scheduler is a single goroutine with a one minute tick, which is
// the same resolution that the schedule expressions support. It is
// deliberately simple: a scheduled pipeline is a convenience, not a
// distributed scheduler.
package cron

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Schedule is a cron expression.
//
// The supported syntax is the standard five field form:
//
//	minute hour day-of-month month day-of-week
//
// A field may be a number, a range, a list or a star. The day-of-week
// field accepts 0 to 6, with 0 being Sunday.
type Schedule struct {
	Minute     []int
	Hour       []int
	DayOfMonth []int
	Month      []int
	DayOfWeek  []int
}

// Parse parses a cron expression.
func Parse(expression string) (Schedule, error) {
	fields := strings.Fields(expression)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("a cron expression must have five fields, got %d", len(fields))
	}
	minute, err := parseField(fields[0], 0, 59)
	if err != nil {
		return Schedule{}, fmt.Errorf("the minute field is invalid: %w", err)
	}
	hour, err := parseField(fields[1], 0, 23)
	if err != nil {
		return Schedule{}, fmt.Errorf("the hour field is invalid: %w", err)
	}
	dayOfMonth, err := parseField(fields[2], 1, 31)
	if err != nil {
		return Schedule{}, fmt.Errorf("the day of month field is invalid: %w", err)
	}
	month, err := parseField(fields[3], 1, 12)
	if err != nil {
		return Schedule{}, fmt.Errorf("the month field is invalid: %w", err)
	}
	dayOfWeek, err := parseField(fields[4], 0, 6)
	if err != nil {
		return Schedule{}, fmt.Errorf("the day of week field is invalid: %w", err)
	}
	return Schedule{
		Minute:     minute,
		Hour:       hour,
		DayOfMonth: dayOfMonth,
		Month:      month,
		DayOfWeek:  dayOfWeek,
	}, nil
}

// parseField parses one field of a cron expression.
func parseField(field string, minimum, maximum int) ([]int, error) {
	if field == "" {
		return nil, fmt.Errorf("the field is empty")
	}
	seen := make(map[int]struct{})
	for _, part := range strings.Split(field, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("the field has an empty entry")
		}
		switch {
		case part == "*":
			for value := minimum; value <= maximum; value++ {
				seen[value] = struct{}{}
			}
		case strings.HasPrefix(part, "*/"):
			// The step syntax, for example "*\/15".
			step, err := strconv.Atoi(strings.TrimPrefix(part, "*/"))
			if err != nil || step <= 0 {
				return nil, fmt.Errorf("the step %q is invalid", part)
			}
			for value := minimum; value <= maximum; value += step {
				seen[value] = struct{}{}
			}
		case strings.Contains(part, "-"):
			bounds := strings.SplitN(part, "-", 2)
			low, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
			if err != nil {
				return nil, err
			}
			high, err := strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err != nil {
				return nil, err
			}
			if low < minimum || high > maximum || low > high {
				return nil, fmt.Errorf("the range %q is out of bounds", part)
			}
			for value := low; value <= high; value++ {
				seen[value] = struct{}{}
			}
		default:
			value, err := strconv.Atoi(part)
			if err != nil {
				return nil, err
			}
			if value < minimum || value > maximum {
				return nil, fmt.Errorf("the value %d is out of bounds", value)
			}
			seen[value] = struct{}{}
		}
	}
	result := make([]int, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	// The result is sorted so that the match is deterministic.
	for index := 1; index < len(result); index++ {
		for inner := index; inner > 0 && result[inner] < result[inner-1]; inner-- {
			result[inner], result[inner-1] = result[inner-1], result[inner]
		}
	}
	return result, nil
}

// Match reports whether the schedule fires at the given time.
func (s Schedule) Match(when time.Time) bool {
	when = when.UTC()
	if !contains(s.Minute, when.Minute()) {
		return false
	}
	if !contains(s.Hour, when.Hour()) {
		return false
	}
	if !contains(s.Month, int(when.Month())) {
		return false
	}
	// The day of month and the day of week are combined with OR when
	// both are restricted, which is the behaviour of the standard cron
	// implementations.
	dayOfMonthRestricted := len(s.DayOfMonth) != 31
	dayOfWeekRestricted := len(s.DayOfWeek) != 7
	switch {
	case dayOfMonthRestricted && dayOfWeekRestricted:
		if !contains(s.DayOfMonth, when.Day()) && !contains(s.DayOfWeek, int(when.Weekday())) {
			return false
		}
	case dayOfMonthRestricted:
		if !contains(s.DayOfMonth, when.Day()) {
			return false
		}
	case dayOfWeekRestricted:
		if !contains(s.DayOfWeek, int(when.Weekday())) {
			return false
		}
	}
	return true
}

// Next returns the next time the schedule fires after the given time.
//
// The search is bounded to four years, which is the same bound that the
// standard cron implementations use.
func (s Schedule) Next(after time.Time) (time.Time, bool) {
	moment := after.UTC().Truncate(time.Minute).Add(time.Minute)
	limit := after.UTC().AddDate(4, 0, 0)
	for moment.Before(limit) {
		if s.Match(moment) {
			return moment, true
		}
		moment = moment.Add(time.Minute)
	}
	return time.Time{}, false
}

// contains reports whether a value is in a sorted set.
func contains(values []int, value int) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

// Entry is a scheduled pipeline.
type Entry struct {
	// Name identifies the entry.
	Name string
	// Pipeline is the name of the pipeline to run.
	Pipeline string
	// Schedule is the cron expression.
	Schedule string
	// Branch is the branch to run.
	Branch string
	// Repository is the repository the pipeline lives in.
	Repository string
	// LastRun is the last time the entry fired.
	LastRun time.Time
	// NextRun is the next time the entry will fire.
	NextRun time.Time
	// Enabled turns the entry on or off.
	Enabled bool
}

// Store holds the scheduled entries.
type Store struct {
	mu      sync.RWMutex
	entries map[string]*Entry
}

// NewStore creates an empty store.
func NewStore() *Store {
	return &Store{entries: make(map[string]*Entry)}
}

// Put adds or replaces an entry.
func (s *Store) Put(entry Entry) error {
	schedule, err := Parse(entry.Schedule)
	if err != nil {
		return err
	}
	if entry.Name == "" {
		return fmt.Errorf("the entry requires a name")
	}
	copied := entry
	copied.NextRun, _ = schedule.Next(time.Now())
	s.mu.Lock()
	s.entries[entry.Name] = &copied
	s.mu.Unlock()
	return nil
}

// Get returns an entry.
func (s *Store) Get(name string) (*Entry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[name]
	if !ok {
		return nil, false
	}
	copied := *entry
	return &copied, true
}

// Delete removes an entry.
func (s *Store) Delete(name string) {
	s.mu.Lock()
	delete(s.entries, name)
	s.mu.Unlock()
}

// List returns every entry.
func (s *Store) List() []Entry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]Entry, 0, len(s.entries))
	for _, entry := range s.entries {
		result = append(result, *entry)
	}
	return result
}

// Due returns the entries that should fire at the given time.
func (s *Store) Due(now time.Time) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []Entry
	for _, entry := range s.entries {
		if !entry.Enabled {
			continue
		}
		schedule, err := Parse(entry.Schedule)
		if err != nil {
			continue
		}
		if !schedule.Match(now) {
			continue
		}
		// An entry fires at most once per minute.
		if entry.LastRun.After(now.Add(-time.Minute)) {
			continue
		}
		entry.LastRun = now
		entry.NextRun, _ = schedule.Next(now)
		due = append(due, *entry)
	}
	return due
}
