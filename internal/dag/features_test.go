package dag

import (
	"testing"

	"barq/internal/yaml"
)

func TestCompilePluginSettingsBecomePrefixedVariables(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: scan
    plugin: acme/scanner:v1
    settings:
      severity: "HIGH"
      fail-on-vuln: "true"
`)
	step := compiled.Steps[0]
	if step.Env["PLUGIN_SEVERITY"] != "HIGH" {
		t.Fatalf("expected PLUGIN_SEVERITY, got %+v", step.Env)
	}
	if step.Env["PLUGIN_FAIL_ON_VULN"] != "true" {
		t.Fatalf("expected PLUGIN_FAIL_ON_VULN, got %+v", step.Env)
	}
}

func TestCompileEnvironmentIsInjectedWithoutAPrefix(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
    environment:
      GOPATH: /tmp/go
`)
	if compiled.Steps[0].Env["GOPATH"] != "/tmp/go" {
		t.Fatalf("expected GOPATH, got %+v", compiled.Steps[0].Env)
	}
}

func TestCompileServicesBecomeSidecars(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: test
    image: alpine
    services:
      - id: database
        image: postgres:16
        env:
          POSTGRES_PASSWORD: secret
        ports: ["5432"]
`)
	services := compiled.Steps[0].Services
	if len(services) != 1 {
		t.Fatalf("expected 1 service, got %d", len(services))
	}
	if services[0].ID != "database" || services[0].Image != "postgres:16" {
		t.Fatalf("unexpected service: %+v", services[0])
	}
	if services[0].Env["POSTGRES_PASSWORD"] != "secret" {
		t.Fatalf("expected the service environment, got %+v", services[0].Env)
	}
}

func TestCompileOnFailureFlag(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
  - id: report
    image: alpine
    depends_on: [build]
    on_failure: true
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
	if !report.OnFailure {
		t.Fatal("expected the on_failure flag to be set")
	}
	if report.OnSuccess {
		t.Fatal("the on_success flag must be off when on_failure is on")
	}
}

func TestCompileDefaultOnSuccess(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
`)
	if !compiled.Steps[0].OnSuccess {
		t.Fatal("expected the on_success flag to default to true")
	}
	if compiled.Steps[0].OnFailure {
		t.Fatal("the on_failure flag must default to false")
	}
}

func TestCompileFailureModeDefaultsToFail(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: build
    image: alpine
    failure: ignore
`)
	if compiled.Steps[0].Failure != "ignore" {
		t.Fatalf("expected the failure mode to be preserved, got %q", compiled.Steps[0].Failure)
	}
}

func TestCompileDetachedStep(t *testing.T) {
	compiled := compile(t, `
name: demo
steps:
  - id: notify
    image: alpine
    detached: true
`)
	if !compiled.Steps[0].Detached {
		t.Fatal("expected the detached flag to be set")
	}
}

func TestCompilePipelineSecretsReachEveryStep(t *testing.T) {
	compiled := compile(t, `
name: demo
secrets:
  - GLOBAL_TOKEN
steps:
  - id: build
    image: alpine
  - id: test
    image: alpine
    secrets:
      - EXTRA_TOKEN
`)
	for _, step := range compiled.Steps {
		found := false
		for _, secret := range step.Secrets {
			if secret == "GLOBAL_TOKEN" {
				found = true
			}
		}
		if !found {
			t.Fatalf("the pipeline secret is missing from %q: %+v", step.ID, step.Secrets)
		}
	}
}

func TestCompileUsesTheGitCommitAlias(t *testing.T) {
	spec, err := yaml.Parse([]byte(`
name: demo
git_commit: "abc123"
steps:
  - id: build
    image: alpine
`))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := Compile(spec, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if compiled.GitCommitTree != "abc123" {
		t.Fatalf("expected the git commit alias to be used, got %q", compiled.GitCommitTree)
	}
}
