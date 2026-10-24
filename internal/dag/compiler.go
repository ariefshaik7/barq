// Package dag compiles a pipeline specification into an executable DAG.
//
// Compilation expands matrix dimensions into individual nodes, clones
// sharded steps, translates plugins into container images and environment
// variables, and groups independent nodes into concurrent execution waves
// using Kahn's topological sort.
package dag

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"barq/internal/yaml"
)

// BuiltinPlugins maps the barq:// plugin namespace to the command executed
// inside the plugin runtime image.
var BuiltinPlugins = map[string]string{
	"barq://slack":     "/usr/local/bin/barq-builtin slack",
	"barq://webhook":   "/usr/local/bin/barq-builtin webhook",
	"barq://git-clone": "/usr/local/bin/barq-builtin git-clone",
	"barq://artifact":  "/usr/local/bin/barq-builtin artifact",
	"barq://audit":     "/usr/local/bin/barq-builtin audit",
}

// Options configures compilation.
type Options struct {
	// PluginRuntimeImage is the image providing the barq:// builtins.
	PluginRuntimeImage string
	// Context carries the facts that the conditions are evaluated
	// against. It is nil for a manual run, in which case the branch
	// and event conditions are ignored.
	Context *yaml.Context
}

// Step is a single executable node in the compiled DAG.
type Step struct {
	ID             string            `json:"id"`
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	Commands       []string          `json:"commands,omitempty"`
	Env            map[string]string `json:"env"`
	DependsOn      []string          `json:"depends_on"`
	Matrix         map[string]string `json:"matrix,omitempty"`
	Wave           int               `json:"wave"`
	ShardIndex     int               `json:"shard_index"`
	ShardTotal     int               `json:"shard_total"`
	Secrets        []string          `json:"secrets,omitempty"`
	Cacheable      bool              `json:"cacheable"`
	NoWorkspace    bool              `json:"no_workspace"`
	WorkingDir     string            `json:"working_dir,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
	// Failure is the behaviour when the step fails: fail, ignore or
	// cancel.
	Failure string `json:"failure,omitempty"`
	// When holds the conditions that decided to include the step.
	When []*yaml.When `json:"when,omitempty"`
	// WaveSize is the number of steps in this step's wave. It lets
	// the UI render wave capacity without recomputing it.
	WaveSize int `json:"wave_size"`
	// Requests and Limits are the worker resource requirements.
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
	// ServiceAccount runs the worker as a specific service account.
	ServiceAccount string `json:"service_account,omitempty"`
	// NodeSelector constrains which nodes may run the worker.
	NodeSelector map[string]string `json:"node_selector,omitempty"`
	// Tolerations allow the worker to run on tainted nodes.
	Tolerations []string `json:"tolerations,omitempty"`
	// MaxParallel caps how many shards run at the same time.
	MaxParallel int `json:"max_parallel,omitempty"`
	// OnSuccess runs the step only when the upstream steps succeeded.
	OnSuccess bool `json:"on_success,omitempty"`
	// OnFailure runs the step only when an upstream step failed.
	OnFailure bool `json:"on_failure,omitempty"`
	// Detached runs the step in the background.
	Detached bool `json:"detached,omitempty"`
	// Services are the sidecar containers of the step.
	Services []Service `json:"services,omitempty"`
}

// Service is a sidecar container that runs alongside a step.
type Service struct {
	ID       string            `json:"id"`
	Image    string            `json:"image"`
	Commands []string          `json:"commands,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Ports    []string          `json:"ports,omitempty"`
}

// PostAction is a compiled post-build action.
type PostAction struct {
	Condition string `json:"condition"`
	Steps     []Step `json:"steps"`
}

// Compiled is the compiled DAG.
type Compiled struct {
	Name          string          `json:"name"`
	GitCommitTree string          `json:"git_commit_tree,omitempty"`
	Steps         []Step          `json:"steps"`
	Waves         [][]string      `json:"waves"`
	ByID          map[string]Step `json:"by_id"`
	PostActions   []PostAction    `json:"post_actions,omitempty"`
}

// variant is a matrix-expanded step before sharding.
type variant struct {
	id     string
	name   string
	values map[string]string
}

// Compile expands and sorts the given pipeline specification.
func Compile(spec yaml.PipelineSpec, options Options) (Compiled, error) {
	if spec.Name == "" {
		return Compiled{}, fmt.Errorf("pipeline name is required")
	}
	if len(spec.Steps) == 0 {
		return Compiled{}, fmt.Errorf("pipeline must declare at least one step")
	}
	compiled := Compiled{
		Name:          spec.Name,
		GitCommitTree: spec.Revision(),
		ByID:          make(map[string]Step),
	}

	// The pipeline level conditions decide whether the run happens at
	// all. When they do not match, the compilation produces an empty
	// plan and the run is reported as skipped.
	if len(spec.When) > 0 {
		context := yaml.Context{}
		if options.Context != nil {
			context = *options.Context
		}
		matched, err := yaml.Match(spec.When, context)
		if err != nil {
			return Compiled{}, err
		}
		if !matched {
			return compiled, nil
		}
	}

	// Expand every declared step into its matrix variants first so that
	// dependency edges can be rewritten to concrete node identifiers.
	variantsByStep := make(map[string][]variant, len(spec.Steps))
	for _, source := range spec.Steps {
		if !stepMatches(source, options.Context) {
			// A step whose conditions do not match is removed from
			// the plan entirely. The plan is therefore exactly the
			// work that needs to happen.
			continue
		}
		expanded, err := expandMatrix(source)
		if err != nil {
			return Compiled{}, err
		}
		variantsByStep[source.ID] = expanded
	}

	// The compiled step identifiers are the matrix variant identifiers
	// with a shard suffix when the step is sharded. The dependency map
	// must therefore be built from the final identifiers, otherwise a
	// dependency on a sharded step would never resolve.
	compiledIDsByStep := make(map[string][]string, len(spec.Steps))
	for _, source := range spec.Steps {
		if !stepMatches(source, options.Context) {
			continue
		}
		shards := source.Parallelism
		if shards <= 0 {
			shards = 1
		}
		identifiers := make([]string, 0)
		for _, expanded := range variantsByStep[source.ID] {
			for shard := 0; shard < shards; shard++ {
				id := expanded.id
				if shards > 1 {
					id = boundedID(fmt.Sprintf("%s-shard-%d", expanded.id, shard))
				}
				identifiers = append(identifiers, id)
			}
		}
		compiledIDsByStep[source.ID] = identifiers
	}

	used := make(map[string]struct{})
	for _, source := range spec.Steps {
		if !stepMatches(source, options.Context) {
			continue
		}
		for _, expanded := range variantsByStep[source.ID] {
			shards := source.Parallelism
			if shards <= 0 {
				shards = 1
			}
			dependencies := make([]string, 0)
			seen := make(map[string]struct{})
			for _, dependency := range source.DependsOn {
				for _, parentID := range compiledIDsByStep[dependency] {
					if _, exists := seen[parentID]; !exists {
						seen[parentID] = struct{}{}
						dependencies = append(dependencies, parentID)
					}
				}
			}
			sort.Strings(dependencies)

			for shard := 0; shard < shards; shard++ {
				id := expanded.id
				if shards > 1 {
					id = boundedID(fmt.Sprintf("%s-shard-%d", expanded.id, shard))
				}
				if _, exists := used[id]; exists {
					return Compiled{}, fmt.Errorf("compiled step id collision: %q", id)
				}
				used[id] = struct{}{}

				environment := make(map[string]string, len(spec.Env)+len(source.Env)+len(expanded.values)+4)
				for key, value := range spec.Env {
					environment[key] = value
				}
				for key, value := range source.Env {
					environment[key] = value
				}
				for key, value := range expanded.values {
					environment[key] = value
				}
				environment["BARQ_NODE_INDEX"] = fmt.Sprint(shard)
				environment["BARQ_NODE_TOTAL"] = fmt.Sprint(shards)

				// A pipeline level secret is available to every step
				// that does not already request it explicitly.
				secrets := append([]string(nil), spec.Secrets...)
				for _, name := range source.Secrets {
					if !containsString(secrets, name) {
						secrets = append(secrets, name)
					}
				}

				image, commands, cacheable, err := resolvePlugin(source, options)
				if err != nil {
					return Compiled{}, err
				}
				// The settings and the with block are the plugin
				// inputs. Both are injected as PLUGIN_ variables.
				for key, value := range source.Settings {
					environment[pluginVariable(key)] = value
				}
				for key, value := range source.With {
					environment[pluginVariable(key)] = value
				}
				// The environment block is injected as it is, without
				// a prefix.
				for key, value := range source.Environment {
					environment[key] = value
				}

				name := expanded.name
				if shards > 1 {
					name = fmt.Sprintf("%s [shard %d/%d]", name, shard+1, shards)
				}

				// The success and failure gates are mutually
				// exclusive. A step that declares on_failure
				// does not run on success, and the other way
				// around. When neither is declared, the step
				// runs on success.
				onSuccess := true
				onFailure := false
				if source.OnSuccess != nil {
					onSuccess = *source.OnSuccess
				}
				if source.OnFailure != nil {
					onFailure = *source.OnFailure
					if source.OnSuccess == nil {
						onSuccess = false
					}
				}
				compiled.Steps = append(compiled.Steps, Step{
					ID:             id,
					Name:           name,
					Image:          image,
					Commands:       append([]string(nil), commands...),
					Env:            environment,
					DependsOn:      dependencies,
					Matrix:         copyMap(expanded.values),
					ShardIndex:     shard,
					ShardTotal:     shards,
					Secrets:        secrets,
					Cacheable:      cacheable,
					NoWorkspace:    source.NoWorkspace,
					WorkingDir:     source.WorkingDir,
					TimeoutSeconds: source.TimeoutSeconds,
					Requests:       resourceMap(source.Resources, true),
					Limits:         resourceMap(source.Resources, false),
					ServiceAccount: source.ServiceAccount,
					NodeSelector:   copyMap(source.NodeSelector),
					Tolerations:    append([]string(nil), source.Tolerations...),
					MaxParallel:    source.MaxParallel,
					Failure:        failureMode(source.Failure),
					When:           source.When,
					OnSuccess:      onSuccess,
					OnFailure:      onFailure,
					Detached:       source.Detached,
					Services:       compileServices(append(append([]yaml.ServiceSpec(nil), spec.Services...), source.Services...)),
				})
			}
		}
	}

	waves, err := topologicalWaves(compiled.Steps)
	if err != nil {
		return Compiled{}, err
	}
	compiled.Waves = waves
	// Record the wave each step belongs to so that the controller can
	// group steps into concurrent execution waves.
	waveOf := make(map[string]int, len(compiled.Steps))
	for waveIndex, wave := range waves {
		for _, id := range wave {
			waveOf[id] = waveIndex
		}
	}
	for index := range compiled.Steps {
		compiled.Steps[index].Wave = waveOf[compiled.Steps[index].ID]
	}
	// Record the size of each wave so that the UI can show the
	// concurrency of a wave without recomputing it.
	for _, wave := range waves {
		for _, id := range wave {
			if step, ok := compiled.ByID[id]; ok {
				step.WaveSize = len(wave)
				compiled.ByID[id] = step
			}
		}
	}
	for _, step := range compiled.Steps {
		compiled.ByID[step.ID] = step
	}

	// Compile post-actions.
	for _, post := range spec.Post {
		compiledPost := PostAction{
			Condition: post.Condition,
			Steps:     make([]Step, 0, len(post.Steps)),
		}
		for _, source := range post.Steps {
			if !stepMatches(source, options.Context) {
				continue
			}
			expanded, err := expandMatrix(source)
			if err != nil {
				return Compiled{}, err
			}
			for _, exp := range expanded {
				shards := source.Parallelism
				if shards <= 0 {
					shards = 1
				}
				for shard := 0; shard < shards; shard++ {
					id := exp.id
					if shards > 1 {
						id = boundedID(fmt.Sprintf("%s-shard-%d", exp.id, shard))
					}
					if _, exists := used[id]; exists {
						return Compiled{}, fmt.Errorf("compiled step id collision: %q", id)
					}
					used[id] = struct{}{}

					environment := make(map[string]string, len(spec.Env)+len(source.Env)+len(exp.values)+4)
					for key, value := range spec.Env {
						environment[key] = value
					}
					for key, value := range source.Env {
						environment[key] = value
					}
					for key, value := range exp.values {
						environment[key] = value
					}
					environment["BARQ_NODE_INDEX"] = fmt.Sprint(shard)
					environment["BARQ_NODE_TOTAL"] = fmt.Sprint(shards)

					secrets := append([]string(nil), spec.Secrets...)
					for _, name := range source.Secrets {
						if !containsString(secrets, name) {
							secrets = append(secrets, name)
						}
					}

					image, commands, cacheable, err := resolvePlugin(source, options)
					if err != nil {
						return Compiled{}, err
					}
					for key, value := range source.Settings {
						environment[pluginVariable(key)] = value
					}
					for key, value := range source.With {
						environment[pluginVariable(key)] = value
					}
					for key, value := range source.Environment {
						environment[key] = value
					}

					name := exp.name
					if shards > 1 {
						name = fmt.Sprintf("%s [shard %d/%d]", name, shard+1, shards)
					}

					onSuccess := true
					onFailure := false
					if source.OnSuccess != nil {
						onSuccess = *source.OnSuccess
					}
					if source.OnFailure != nil {
						onFailure = *source.OnFailure
						if source.OnSuccess == nil {
							onSuccess = false
						}
					}
					compiledPost.Steps = append(compiledPost.Steps, Step{
						ID:             id,
						Name:           name,
						Image:          image,
						Commands:       append([]string(nil), commands...),
						Env:            environment,
						DependsOn:      nil, // Post-steps have no dependencies on main steps
						Matrix:         copyMap(exp.values),
						ShardIndex:     shard,
						ShardTotal:     shards,
						Secrets:        secrets,
						Cacheable:      cacheable,
						NoWorkspace:    source.NoWorkspace,
						WorkingDir:     source.WorkingDir,
						TimeoutSeconds: source.TimeoutSeconds,
						Requests:       resourceMap(source.Resources, true),
						Limits:         resourceMap(source.Resources, false),
						ServiceAccount: source.ServiceAccount,
						NodeSelector:   copyMap(source.NodeSelector),
						Tolerations:    append([]string(nil), source.Tolerations...),
						MaxParallel:    source.MaxParallel,
						Failure:        failureMode(source.Failure),
						When:           source.When,
						OnSuccess:      onSuccess,
						OnFailure:      onFailure,
						Detached:       source.Detached,
						Services:       compileServices(append(append([]yaml.ServiceSpec(nil), spec.Services...), source.Services...)),
					})
				}
			}
		}
		if len(compiledPost.Steps) > 0 {
			compiled.PostActions = append(compiled.PostActions, compiledPost)
		}
	}

	return compiled, nil
}

// stepMatches reports whether a step's conditions are satisfied.
//
// A step that depends on a step which was filtered out is itself
// filtered out, because the work it was waiting for will never
// happen.
func stepMatches(source yaml.StepSpec, context *yaml.Context) bool {
	if len(source.When) == 0 {
		return true
	}
	evaluation := yaml.Context{}
	if context != nil {
		evaluation = *context
	}
	matched, err := yaml.Match(source.When, evaluation)
	if err != nil {
		// A malformed condition removes the step rather than
		// failing the whole compilation. The alternative is that one
		// bad condition blocks every other step.
		return false
	}
	return matched
}

// resolvePlugin determines the image, command and cache policy for a step.
func resolvePlugin(source yaml.StepSpec, options Options) (string, []string, bool, error) {
	if source.Plugin == "" {
		return source.Image, append([]string(nil), source.Run...), true, nil
	}
	if command, builtin := BuiltinPlugins[source.Plugin]; builtin {
		image := options.PluginRuntimeImage
		if image == "" {
			return "", nil, false, fmt.Errorf("step %q uses builtin plugin %q but no plugin runtime image is configured", source.ID, source.Plugin)
		}
		return image, []string{command}, false, nil
	}
	if strings.HasPrefix(source.Plugin, "barq://") {
		return "", nil, false, fmt.Errorf("step %q references unknown builtin plugin %q", source.ID, source.Plugin)
	}
	// Custom OCI plugins must implement the documented plugin contract.
	// The runtime invokes /usr/local/bin/barq-plugin so the workspace
	// capture and checkpointing wrapper stays in control.
	return source.Plugin, []string{"/usr/local/bin/barq-plugin"}, false, nil
}

func expandMatrix(source yaml.StepSpec) ([]variant, error) {
	baseName := source.Name
	if baseName == "" {
		baseName = source.ID
	}
	if len(source.Matrix) == 0 {
		return []variant{{id: source.ID, name: baseName, values: map[string]string{}}}, nil
	}

	dimensions := make([]string, 0, len(source.Matrix))
	for dimension := range source.Matrix {
		dimensions = append(dimensions, dimension)
	}
	sort.Strings(dimensions)

	combinations := []map[string]string{{}}
	for _, dimension := range dimensions {
		var next []map[string]string
		for _, combination := range combinations {
			for _, value := range source.Matrix[dimension] {
				copied := make(map[string]string, len(combination)+1)
				for key, existing := range combination {
					copied[key] = existing
				}
				copied[dimension] = value
				next = append(next, copied)
			}
		}
		combinations = next
	}

	result := make([]variant, 0, len(combinations))
	for _, combination := range combinations {
		parts := make([]string, 0, len(combination))
		name := baseName
		for _, dimension := range dimensions {
			value := combination[dimension]
			parts = append(parts, sanitizeSegment(dimension)+"-"+sanitizeSegment(value))
			name += fmt.Sprintf(" [%s=%s]", dimension, value)
		}
		result = append(result, variant{
			id:     boundedID(source.ID + "-" + strings.Join(parts, "-")),
			name:   name,
			values: combination,
		})
	}
	return result, nil
}

// topologicalWaves groups nodes into concurrently executable waves.
func topologicalWaves(steps []Step) ([][]string, error) {
	indegree := make(map[string]int, len(steps))
	children := make(map[string][]string, len(steps))
	known := make(map[string]struct{}, len(steps))
	for _, step := range steps {
		known[step.ID] = struct{}{}
		indegree[step.ID] = len(step.DependsOn)
	}
	for _, step := range steps {
		for _, parent := range step.DependsOn {
			if _, ok := known[parent]; !ok {
				return nil, fmt.Errorf("step %q depends on unknown compiled step %q", step.ID, parent)
			}
			children[parent] = append(children[parent], step.ID)
		}
	}

	ready := make([]string, 0)
	for _, step := range steps {
		if indegree[step.ID] == 0 {
			ready = append(ready, step.ID)
		}
	}
	sort.Strings(ready)

	var waves [][]string
	processed := 0
	for len(ready) > 0 {
		wave := append([]string(nil), ready...)
		waves = append(waves, wave)
		processed += len(ready)

		next := make([]string, 0)
		for _, parent := range ready {
			for _, child := range children[parent] {
				indegree[child]--
				if indegree[child] == 0 {
					next = append(next, child)
				}
			}
		}
		sort.Strings(next)
		ready = next
	}
	if processed != len(steps) {
		remaining := make([]string, 0)
		for _, step := range steps {
			if indegree[step.ID] > 0 {
				remaining = append(remaining, step.ID)
			}
		}
		return nil, fmt.Errorf("dependency cycle detected among steps: %s", strings.Join(remaining, ", "))
	}
	return waves, nil
}

// compileServices converts the service declarations of a step.
func compileServices(services []yaml.ServiceSpec) []Service {
	if len(services) == 0 {
		return nil
	}
	result := make([]Service, 0, len(services))
	for index, service := range services {
		id := service.ID
		if id == "" {
			id = fmt.Sprintf("service-%d", index)
		}
		environment := make(map[string]string, len(service.Env))
		for key, value := range service.Env {
			environment[key] = value
		}
		result = append(result, Service{
			ID:       id,
			Image:    service.Image,
			Commands: append([]string(nil), service.Commands...),
			Env:      environment,
			Ports:    append([]string(nil), service.Ports...),
		})
	}
	return result
}

// failureMode normalises the failure behaviour of a step.
//
// The default is "fail", which is the safe default: a step that
// fails stops the pipeline unless the author says otherwise.
func failureMode(value string) string {
	if value == "" {
		return "fail"
	}
	return value
}

// pluginVariable converts a plugin input key into a PLUGIN_ environment
// variable name.
func pluginVariable(key string) string {
	var builder strings.Builder
	builder.WriteString("PLUGIN_")
	for _, char := range strings.ToUpper(key) {
		switch {
		case char >= 'A' && char <= 'Z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}
func sanitizeSegment(value string) string {
	var builder strings.Builder
	for _, char := range strings.ToLower(value) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9':
			builder.WriteRune(char)
		default:
			builder.WriteByte('-')
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "x"
	}
	return result
}

// boundedID keeps identifiers within the 63 character Kubernetes label
// limit while preserving uniqueness through a hash suffix.
func boundedID(value string) string {
	if len(value) <= 63 {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return strings.Trim(value[:54], "-") + "-" + hex.EncodeToString(sum[:])[:8]
}

// resourceMap extracts the requests or the limits from a resource
// block.
func resourceMap(resources *yaml.Resources, requests bool) map[string]string {
	if resources == nil {
		return nil
	}
	source := resources.Limits
	if requests {
		source = resources.Requests
	}
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func copyMap(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
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
