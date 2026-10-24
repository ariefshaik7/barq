package yaml

import (
	"strings"
	"testing"
)

func TestParsePipelineFull(t *testing.T) {
	document := `
name: demo
git_commit_tree: "9f8e7d6c"
env:
  GOFLAGS: -mod=readonly
steps:
  - id: lint
    image: golang:1.22-alpine
    run:
      - go vet ./...
  - id: test
    image: golang:1.22-alpine
    depends_on: [lint]
    parallelism: 2
    matrix:
      GO_VERSION: ["1.22", "1.23"]
      ARCH: [amd64, arm64]
    run: go test ./...
  - id: notify
    plugin: barq://slack
    depends_on: [test]
    with:
      channel: "#ci-builds"
      webhook_url: "https://example.invalid/hook"
`
	spec, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if spec.Name != "demo" {
		t.Fatalf("expected name demo, got %q", spec.Name)
	}
	if spec.GitCommitTree != "9f8e7d6c" {
		t.Fatalf("expected git tree, got %q", spec.GitCommitTree)
	}
	if spec.Env["GOFLAGS"] != "-mod=readonly" {
		t.Fatalf("expected pipeline env, got %+v", spec.Env)
	}
	if len(spec.Steps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(spec.Steps))
	}
	if len(spec.Steps[0].Run) != 1 || spec.Steps[0].Run[0] != "go vet ./..." {
		t.Fatalf("unexpected run list: %+v", spec.Steps[0].Run)
	}
	if spec.Steps[1].Parallelism != 2 {
		t.Fatalf("expected parallelism 2, got %d", spec.Steps[1].Parallelism)
	}
	if len(spec.Steps[1].Matrix["GO_VERSION"]) != 2 {
		t.Fatalf("expected 2 go versions, got %+v", spec.Steps[1].Matrix)
	}
	if spec.Steps[1].Run[0] != "go test ./..." {
		t.Fatalf("expected scalar run to expand, got %+v", spec.Steps[1].Run)
	}
	if spec.Steps[2].Plugin != "barq://slack" {
		t.Fatalf("expected plugin, got %q", spec.Steps[2].Plugin)
	}
	if spec.Steps[2].With["channel"] != "#ci-builds" {
		t.Fatalf("expected quoted hash value, got %q", spec.Steps[2].With["channel"])
	}
}

func TestParseRejectsUnknownFields(t *testing.T) {
	document := `
name: demo
steps:
  - id: build
    image: alpine
    totally_unknown: yes
`
	if _, err := Parse([]byte(document)); err == nil {
		t.Fatal("expected an error for an unknown field")
	}
}

func TestParseRejectsTabs(t *testing.T) {
	document := "name: demo\nsteps:\n\t- id: build\n"
	if _, err := Parse([]byte(document)); err == nil {
		t.Fatal("expected an error for tab indentation")
	}
}

func TestParseRejectsAnchors(t *testing.T) {
	document := "name: &anchor demo\nsteps: []\n"
	if _, err := Parse([]byte(document)); err == nil {
		t.Fatal("expected an error for anchors")
	}
}

func TestParseRejectsDuplicateStepIDs(t *testing.T) {
	document := `
name: demo
steps:
  - id: build
    image: alpine
  - id: build
    image: alpine
`
	if _, err := Parse([]byte(document)); err == nil {
		t.Fatal("expected an error for duplicate step ids")
	}
}

func TestParseRejectsUnknownDependency(t *testing.T) {
	document := `
name: demo
steps:
  - id: build
    image: alpine
    depends_on: [missing]
`
	if _, err := Parse([]byte(document)); err == nil {
		t.Fatal("expected an error for an unknown dependency")
	}
}

func TestParseRejectsPluginWithRun(t *testing.T) {
	document := `
name: demo
steps:
  - id: build
    plugin: barq://slack
    run: echo hi
`
	if _, err := Parse([]byte(document)); err == nil {
		t.Fatal("expected an error when plugin and run are combined")
	}
}

func TestParseJSON(t *testing.T) {
	document := `{"name":"demo","steps":[{"id":"build","image":"alpine","run":"make"}]}`
	spec, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(spec.Steps) != 1 || spec.Steps[0].Run[0] != "make" {
		t.Fatalf("unexpected parse result: %+v", spec.Steps)
	}
}

func TestParseBlockScalar(t *testing.T) {
	document := `
name: demo
steps:
  - id: build
    image: alpine
    run: |
      set -e
      make build
`
	spec, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if !strings.Contains(spec.Steps[0].Run[0], "make build") {
		t.Fatalf("expected block scalar content, got %q", spec.Steps[0].Run[0])
	}
}

func TestParseCommentsAreStripped(t *testing.T) {
	document := `
name: demo # the pipeline name
steps:
  # a comment line
  - id: build
    image: alpine # the image
`
	spec, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if spec.Name != "demo" || spec.Steps[0].Image != "alpine" {
		t.Fatalf("unexpected parse result: %+v", spec)
	}
}

func TestParseNestedMaps(t *testing.T) {
	document := `
name: demo
steps:
  - id: build
    image: alpine
    env:
      A: "1"
      B: "2"
    matrix:
      GO: ["1.22"]
`
	spec, err := Parse([]byte(document))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if spec.Steps[0].Env["A"] != "1" || spec.Steps[0].Env["B"] != "2" {
		t.Fatalf("unexpected env: %+v", spec.Steps[0].Env)
	}
}

func FuzzParseNeverPanics(f *testing.F) {
	f.Add([]byte("name: demo\nsteps:\n  - id: a\n    image: b\n"))
	f.Add([]byte("{"))
	f.Add([]byte("steps: [\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = Parse(data)
	})
}
