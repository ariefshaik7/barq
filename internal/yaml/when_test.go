package yaml

import "testing"

func TestMatchEmptyConditions(t *testing.T) {
	// An empty condition list matches everything.
	matched, err := Match(nil, Context{Event: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected an empty condition list to match")
	}
}

func TestMatchEvent(t *testing.T) {
	conditions := []*When{{Event: []string{"push", "tag"}}}
	matched, err := Match(conditions, Context{Event: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected the push event to match")
	}
	matched, err = Match(conditions, Context{Event: "pull_request"})
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("expected the pull_request event not to match")
	}
}

func TestMatchBranchGlob(t *testing.T) {
	conditions := []*When{{Branch: []string{"main", "release/*"}}}
	for _, branch := range []string{"main", "release/1.0", "release/2.0"} {
		matched, err := Match(conditions, Context{Branch: branch})
		if err != nil {
			t.Fatal(err)
		}
		if !matched {
			t.Fatalf("expected %q to match", branch)
		}
	}
	matched, err := Match(conditions, Context{Branch: "feature/x"})
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("expected feature/x not to match")
	}
}

func TestMatchStatus(t *testing.T) {
	conditions := []*When{{Status: []string{"failure"}}}
	matched, err := Match(conditions, Context{UpstreamStatus: "failure"})
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected the failure status to match")
	}
	matched, err = Match(conditions, Context{UpstreamStatus: "success"})
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("expected the success status not to match")
	}
}

func TestMatchMatrix(t *testing.T) {
	conditions := []*When{{Matrix: map[string][]string{"GO": {"1.22", "1.23"}}}}
	matched, err := Match(conditions, Context{Matrix: map[string]string{"GO": "1.23"}})
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected the matrix value to match")
	}
	matched, err = Match(conditions, Context{Matrix: map[string]string{"GO": "1.21"}})
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("expected the matrix value not to match")
	}
}

func TestMatchMultipleBlocksIsOr(t *testing.T) {
	conditions := []*When{
		{Branch: []string{"main"}},
		{Branch: []string{"release/*"}},
	}
	for _, branch := range []string{"main", "release/1.0"} {
		matched, err := Match(conditions, Context{Branch: branch})
		if err != nil {
			t.Fatal(err)
		}
		if !matched {
			t.Fatalf("expected %q to match one of the blocks", branch)
		}
	}
}

func TestMatchWithinABlockIsAnd(t *testing.T) {
	conditions := []*When{{
		Event:  []string{"push"},
		Branch: []string{"main"},
	}}
	matched, err := Match(conditions, Context{Event: "push", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected the conditions to match")
	}
	// Both conditions must hold.
	matched, err = Match(conditions, Context{Event: "push", Branch: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("expected the conditions not to match")
	}
}

func TestMatchRejectsAnUnknownEvent(t *testing.T) {
	conditions := []*When{{Event: []string{"not-an-event"}}}
	if _, err := Match(conditions, Context{Event: "push"}); err == nil {
		t.Fatal("expected an error for an unknown event")
	}
}

func TestMatchRejectsAnUnknownStatus(t *testing.T) {
	conditions := []*When{{Status: []string{"maybe"}}}
	if _, err := Match(conditions, Context{}); err == nil {
		t.Fatal("expected an error for an unknown status")
	}
}

func TestMatchLocal(t *testing.T) {
	truth := true
	conditions := []*When{{Local: &truth}}
	matched, err := Match(conditions, Context{Local: true})
	if err != nil {
		t.Fatal(err)
	}
	if !matched {
		t.Fatal("expected the local condition to match")
	}
	matched, err = Match(conditions, Context{Local: false})
	if err != nil {
		t.Fatal(err)
	}
	if matched {
		t.Fatal("expected the local condition not to match")
	}
}
