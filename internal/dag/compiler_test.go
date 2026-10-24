package dag

import (
	"strings"
	"testing"

	"barq/internal/yaml"
)

func compile(t *testing.T, document string) Compiled {
	t.Helper()
	return compileWithContext(t, document, &yaml.Context{
		Event:      "push",
		Branch:     "main",
		Repository: "acme/widgets",
	})
}

func compileWithContext(t *testing.T, document string, context *yaml.Context) Compiled {
	t.Helper()
	spec, err := yaml.Parse([]byte(document))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	compiled, err := Compile(spec, Options{
		PluginRuntimeImage: "barq/plugin-runtime:1.0.0",
		Context:            context,
	})
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	return compiled
}

func TestCompileDetectsCycles(t *testing.T) {
	spec, err := yaml.Parse([]byte(`
name: demo
steps:
  - id: a
    image: alpine
    depends_on: [b]
  - id: b
    image: alpine
    depends_on: [a]
`))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if _, err := Compile(spec, Options{}); err == nil {
		t.Fatal("expected a cycle detection error")
	}
}

func TestCompileGroupsIndependentStepsIntoWaves(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: a
    image: alpine
  - id: b
    image: alpine
  - id: c
    image: alpine
    depends_on: [a, b]
`)
	if len(compiled.Waves) != 2 {
		t.Fatalf("expected 2 waves, got %d: %+v", len(compiled.Waves), compiled.Waves)
	}
	if len(compiled.Waves[0]) != 2 {
		t.Fatalf("expected 2 steps in wave 0, got %+v", compiled.Waves[0])
	}
	if len(compiled.Waves[1]) != 1 {
		t.Fatalf("expected 1 step in wave 1, got %+v", compiled.Waves[1])
	}
}

func TestCompileExpandsMatrix(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
    matrix:
      GO: ["1.22", "1.23"]
      ARCH: [amd64, arm64]
`)
	if len(compiled.Steps) != 4 {
		t.Fatalf("expected 4 matrix combinations, got %d", len(compiled.Steps))
	}
	for _, step := range compiled.Steps {
		if step.Env["GO"] == "" || step.Env["ARCH"] == "" {
			t.Fatalf("expected matrix values in the environment: %+v", step.Env)
		}
	}
}

func TestCompileClonesShards(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: test
    image: alpine
    parallelism: 3
`)
	if len(compiled.Steps) != 3 {
		t.Fatalf("expected 3 shards, got %d", len(compiled.Steps))
	}
	for _, step := range compiled.Steps {
		if step.ShardTotal != 3 {
			t.Fatalf("expected shard total 3, got %d", step.ShardTotal)
		}
		if step.Env["BARQ_NODE_INDEX"] == "" || step.Env["BARQ_NODE_TOTAL"] != "3" {
			t.Fatalf("expected shard environment, got %+v", step.Env)
		}
	}
}

func TestCompileFanInExpandsToAllMatrixVariants(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
    matrix:
      GO: ["1.22", "1.23"]
  - id: test
    image: alpine
    depends_on: [build]
`)
	var test *Step
	for index := range compiled.Steps {
		if compiled.Steps[index].ID == "test" {
			test = &compiled.Steps[index]
		}
	}
	if test == nil {
		t.Fatal("expected a test step")
	}
	if len(test.DependsOn) != 2 {
		t.Fatalf("expected 2 upstream variants, got %+v", test.DependsOn)
	}
}

func TestCompileTranslatesBuiltinPlugin(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: notify
    plugin: barq://slack
    with:
      channel: "#ci"
      webhook_url: "https://example.invalid"
`)
	step := compiled.Steps[0]
	if step.Image != "barq/plugin-runtime:1.0.0" {
		t.Fatalf("expected the plugin runtime image, got %q", step.Image)
	}
	if len(step.Commands) != 1 || step.Commands[0] != "/usr/local/bin/barq-builtin slack" {
		t.Fatalf("expected the builtin entrypoint, got %+v", step.Commands)
	}
	if step.Env["PLUGIN_CHANNEL"] != "#ci" {
		t.Fatalf("expected PLUGIN_CHANNEL, got %+v", step.Env)
	}
	if step.Env["PLUGIN_WEBHOOK_URL"] != "https://example.invalid" {
		t.Fatalf("expected PLUGIN_WEBHOOK_URL, got %+v", step.Env)
	}
	if step.Cacheable {
		t.Fatal("builtin plugins must not be cacheable")
	}
}

func TestCompileCustomPluginUsesContractEntrypoint(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: scan
    plugin: acme/trivy-plugin:v1
    with:
      severity: HIGH
`)
	step := compiled.Steps[0]
	if step.Image != "acme/trivy-plugin:v1" {
		t.Fatalf("expected the plugin image, got %q", step.Image)
	}
	if len(step.Commands) != 1 || step.Commands[0] != "/usr/local/bin/barq-plugin" {
		t.Fatalf("expected the plugin contract entrypoint, got %+v", step.Commands)
	}
	if step.Env["PLUGIN_SEVERITY"] != "HIGH" {
		t.Fatalf("expected PLUGIN_SEVERITY, got %+v", step.Env)
	}
}

func TestCompileRejectsUnknownBuiltin(t *testing.T) {
	spec, err := yaml.Parse([]byte(`
name: demo
steps:
  - id: x
    plugin: barq://does-not-exist
`))
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if _, err := Compile(spec, Options{PluginRuntimeImage: "img"}); err == nil {
		t.Fatal("expected an error for an unknown builtin plugin")
	}
}

func TestCompileFanInAcrossShardedSteps(t *testing.T) {
	// A dependency on a sharded step must expand to every shard. The
	// shard suffix is part of the compiled identifier, so the
	// dependency map must be built from the final identifiers.
	compiled := compile(t, `
name: demo
steps:
  - id: test
    image: alpine
    parallelism: 3
  - id: report
    image: alpine
    depends_on: [test]
`)
	var report *Step
	for index := range compiled.Steps {
		if compiled.Steps[index].ID == "report" {
			report = &compiled.Steps[index]
		}
	}
	if report == nil {
		t.Fatal("expected a report step")
	}
	if len(report.DependsOn) != 3 {
		t.Fatalf("expected 3 upstream shards, got %+v", report.DependsOn)
	}
	for _, dependency := range report.DependsOn {
		if !strings.Contains(dependency, "test-shard-") {
			t.Fatalf("expected a shard identifier, got %q", dependency)
		}
		if _, ok := compiled.ByID[dependency]; !ok {
			t.Fatalf("the dependency %q does not exist in the compiled pipeline", dependency)
		}
	}
}

func TestCompileDependencyOnShardedMatrixStep(t *testing.T) {
	// A matrix step that is also sharded produces matrix and shard
	// suffixes. Both must be reflected in the dependency identifiers.
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
    matrix:
      GO: ["1.22", "1.23"]
    parallelism: 2
  - id: test
    image: alpine
    depends_on: [build]
`)
	var test *Step
	for index := range compiled.Steps {
		if compiled.Steps[index].ID == "test" {
			test = &compiled.Steps[index]
		}
	}
	if test == nil {
		t.Fatal("expected a test step")
	}
	if len(test.DependsOn) != 4 {
		t.Fatalf("expected 4 upstream nodes, got %+v", test.DependsOn)
	}
	for _, dependency := range test.DependsOn {
		if _, ok := compiled.ByID[dependency]; !ok {
			t.Fatalf("the dependency %q does not exist in the compiled pipeline", dependency)
		}
	}
}

func TestCompileRemovesStepsWhoseConditionsDoNotMatch(t *testing.T) {
	// A step whose conditions do not match is removed from the plan
	// entirely, rather than being kept and branched on at runtime.
	compiled := compileWithContext(t, `
name: demo
steps:
  - id: always
    image: alpine
  - id: only-on-release
    image: alpine
    when:
      branch: ["release/*"]
`, &yaml.Context{Event: "push", Branch: "main", Repository: "acme/widgets"})
	if len(compiled.Steps) != 1 {
		t.Fatalf("expected the unmatched step to be removed, got %d steps", len(compiled.Steps))
	}
	if compiled.Steps[0].ID != "always" {
		t.Fatalf("expected the matching step to remain, got %q", compiled.Steps[0].ID)
	}
}

func TestCompileKeepsStepsWhoseConditionsMatch(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
  - id: notify
    image: alpine
    depends_on: [build]
    when:
      branch: ["main"]
`)
	if len(compiled.Steps) != 2 {
		t.Fatalf("expected both steps to remain, got %d", len(compiled.Steps))
	}
}

func TestCompileSkipsTheWholePipelineWhenTheConditionDoesNotMatch(t *testing.T) {
	spec, err := yaml.Parse([]byte(`
name: demo
when:
  branch: ["main"]
steps:
  - id: build
    image: alpine
`))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(spec, Options{Context: &yaml.Context{Branch: "feature"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(compiled.Steps) != 0 {
		t.Fatalf("expected an empty plan, got %d steps", len(compiled.Steps))
	}
}

func TestCompileDefaultFailureModeIsFail(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
`)
	if compiled.Steps[0].Failure != "fail" {
		t.Fatalf("expected the default failure mode to be fail, got %q", compiled.Steps[0].Failure)
	}
}

func TestCompileBoundsIdentifiers(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: a
    image: alpine
    matrix:
      LONG: ["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]
`)
	if len(compiled.Steps[0].ID) > 63 {
		t.Fatalf("identifier %q exceeds the label limit", compiled.Steps[0].ID)
	}
}

func TestCompilePostActions(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
post:
  - condition: always
    steps:
      - id: notify
        image: alpine
  - condition: failure
    steps:
      - id: report-failure
        image: alpine
`)
	if len(compiled.PostActions) != 2 {
		t.Fatalf("expected 2 post actions, got %d", len(compiled.PostActions))
	}
	if compiled.PostActions[0].Condition != "always" {
		t.Fatalf("expected condition 'always', got %q", compiled.PostActions[0].Condition)
	}
	if len(compiled.PostActions[0].Steps) != 1 {
		t.Fatalf("expected 1 step in 'always' post action, got %d", len(compiled.PostActions[0].Steps))
	}
	if compiled.PostActions[0].Steps[0].ID != "notify" {
		t.Fatalf("expected step 'notify', got %q", compiled.PostActions[0].Steps[0].ID)
	}
	if compiled.PostActions[1].Condition != "failure" {
		t.Fatalf("expected condition 'failure', got %q", compiled.PostActions[1].Condition)
	}
	if compiled.PostActions[1].Steps[0].ID != "report-failure" {
		t.Fatalf("expected step 'report-failure', got %q", compiled.PostActions[1].Steps[0].ID)
	}
}

func TestCompilePostActionsWithMatrix(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
post:
  - condition: always
    steps:
      - id: test
        image: alpine
        matrix:
          GO: ["1.22", "1.23"]
`)
	if len(compiled.PostActions) != 1 {
		t.Fatalf("expected 1 post action, got %d", len(compiled.PostActions))
	}
	if len(compiled.PostActions[0].Steps) != 2 {
		t.Fatalf("expected 2 matrix variants, got %d", len(compiled.PostActions[0].Steps))
	}
}
