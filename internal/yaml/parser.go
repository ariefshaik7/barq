// Package yaml implements the Barq pipeline YAML subset.
//
// The parser is intentionally strict: it supports exactly the constructs
// documented in docs/plugins.md and rejects everything else (anchors,
// aliases, tags, merge keys, multi-document streams) instead of silently
// misinterpreting them. It also supports JSON, because JSON is a subset of
// YAML and the API accepts both.
package yaml

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// StringList accepts either a single scalar or a list of scalars.
type StringList []string

// UnmarshalJSON implements json.Unmarshaler.
func (s *StringList) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = nil
		return nil
	}
	var single string
	if err := json.Unmarshal(trimmed, &single); err == nil {
		*s = StringList{single}
		return nil
	}
	var many []any
	if err := json.Unmarshal(trimmed, &many); err != nil {
		return fmt.Errorf("expected a string or a list of strings: %w", err)
	}
	result := make(StringList, 0, len(many))
	for _, item := range many {
		result = append(result, scalarToString(item))
	}
	*s = result
	return nil
}

// ScalarMap accepts a mapping whose values are coerced to strings.
type ScalarMap map[string]string

// UnmarshalJSON implements json.Unmarshaler.
func (m *ScalarMap) UnmarshalJSON(data []byte) error {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	result := make(ScalarMap, len(raw))
	for key, value := range raw {
		result[key] = scalarToString(value)
	}
	*m = result
	return nil
}

// MatrixSpec maps a dimension name to the list of values to expand.
type MatrixSpec map[string]StringList

// UnmarshalJSON implements json.Unmarshaler.
func (m *MatrixSpec) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	result := make(MatrixSpec, len(raw))
	for key, value := range raw {
		var list StringList
		if err := json.Unmarshal(value, &list); err != nil {
			return fmt.Errorf("matrix dimension %q must be a string or list of strings: %w", key, err)
		}
		result[key] = list
	}
	*m = result
	return nil
}

// PipelineSpec is the top level pipeline document.
// PipelineSpec is the top level pipeline document.
type PipelineSpec struct {
	Name string `json:"name"`
	// GitCommitTree identifies the source revision.
	GitCommitTree string `json:"git_commit_tree,omitempty"`
	// GitCommit is an alias for GitCommitTree. It is the shorter form
	// that most CI systems use.
	GitCommit string    `json:"git_commit,omitempty"`
	Env       ScalarMap `json:"env,omitempty"`
	When      WhenList  `json:"when,omitempty"`
	// Secrets are injected into every step that does not already
	// request them.
	Secrets StringList `json:"secrets,omitempty"`
	// Group is the concurrency group of the pipeline. Runs in the same
	// group share a concurrency budget, so a single repository cannot
	// consume the whole cluster.
	Group string `json:"group,omitempty"`
	// MaxConcurrent is the number of runs of this pipeline that may
	// execute at the same time. Zero means the global limit applies.
	MaxConcurrent int `json:"max_concurrent,omitempty"`
	// Services are containers that run alongside every step of the
	// pipeline, such as a database or a cache.
	Services []ServiceSpec `json:"services,omitempty"`
	// Clone is the step that fetches the source. It is injected
	// automatically when the pipeline does not declare one.
	Clone *StepSpec `json:"clone,omitempty"`
	// SkipClone disables the automatic clone step.
	SkipClone bool       `json:"skip_clone,omitempty"`
	Steps     []StepSpec `json:"steps"`
	// Post defines post-build actions that run after the main steps
	// complete. Conditions: always, success, failure, cleanup, changed.
	Post []PostAction `json:"post,omitempty"`
}

// PostAction defines a post-build action.
type PostAction struct {
	// Condition is when the action runs: always, success, failure, cleanup, changed.
	Condition string `json:"condition"`
	// Steps are the steps to execute.
	Steps []StepSpec `json:"steps"`
}

// Revision returns the source revision the pipeline was built from.
func (p PipelineSpec) Revision() string {
	if p.GitCommitTree != "" {
		return p.GitCommitTree
	}
	return p.GitCommit
}

// When is a set of conditions that decide whether a step runs.
//
// The conditions within a `when` block are combined with AND. A step
// may declare several `when` blocks, which are combined with OR. The
// semantics mirror the ones used by mature CI systems, so a pipeline
// written for them behaves the same way here.
type When struct {
	// Event restricts the trigger that may start the pipeline.
	Event []string `json:"event,omitempty"`
	// Branch restricts the branch that may start the pipeline.
	Branch []string `json:"branch,omitempty"`
	// Ref restricts the git reference.
	Ref []string `json:"ref,omitempty"`
	// Repo restricts the repository.
	Repo []string `json:"repo,omitempty"`
	// Status restricts the status of the upstream steps. The special
	// value "failure" matches a failed upstream step and the special
	// value "success" matches a successful one. A step with no status
	// condition runs when the upstream steps succeeded.
	Status []string `json:"status,omitempty"`
	// Instance restricts the repository instance, for a self hosted
	// forge.
	Instance []string `json:"instance,omitempty"`
	// Path restricts the changed files.
	Path []string `json:"path,omitempty"`
	// Matrix restricts the matrix axis value.
	Matrix map[string][]string `json:"matrix,omitempty"`
	// Local restricts the run to a local execution.
	Local *bool `json:"local,omitempty"`
}

// Resources describes the compute resources requested by a worker.
type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// StepSpec is a single pipeline step.
type StepSpec struct {
	ID    string `json:"id"`
	Name  string `json:"name,omitempty"`
	Image string `json:"image,omitempty"`
	// Run is the list of shell commands the step executes.
	Run StringList `json:"run,omitempty"`
	// Commands is an alias for Run. It is the name that most CI
	// systems use.
	Commands StringList `json:"commands,omitempty"`
	// Settings are the plugin inputs. They are injected as
	// PLUGIN_<UPPER_SNAKE_CASE> environment variables.
	Settings ScalarMap `json:"settings,omitempty"`
	// Environment are the plain environment variables of the step.
	Environment    ScalarMap  `json:"environment,omitempty"`
	Env            ScalarMap  `json:"env,omitempty"`
	DependsOn      StringList `json:"depends_on,omitempty"`
	Matrix         MatrixSpec `json:"matrix,omitempty"`
	Parallelism    int        `json:"parallelism,omitempty"`
	Plugin         string     `json:"plugin,omitempty"`
	With           ScalarMap  `json:"with,omitempty"`
	WorkingDir     string     `json:"working_dir,omitempty"`
	TimeoutSeconds int        `json:"timeout_seconds,omitempty"`
	Secrets        StringList `json:"secrets,omitempty"`
	Cache          *bool      `json:"cache,omitempty"`
	NoWorkspace    bool       `json:"no_workspace,omitempty"`
	// Resources requests and limits for the worker Pod.
	Resources *Resources `json:"resources,omitempty"`
	// ServiceAccount runs the worker as this service account.
	ServiceAccount string `json:"service_account,omitempty"`
	// NodeSelector constrains which nodes may run the worker.
	NodeSelector map[string]string `json:"node_selector,omitempty"`
	// Tolerations allow the worker to run on tainted nodes.
	Tolerations []string `json:"tolerations,omitempty"`
	// MaxParallel caps how many shards of this step may run at the
	// same time, independently of the shard count.
	MaxParallel int `json:"max_parallel,omitempty"`
	// When restricts the conditions under which the step runs.
	When WhenList `json:"when,omitempty"`
	// Failure is the behaviour when the step fails. It is one of
	// "fail", "ignore" or "cancel".
	Failure string `json:"failure,omitempty"`
	// DependsOnOptional lists the dependencies that may be missing.
	DependsOnOptional []string `json:"depends_on_optional,omitempty"`
	// OnSuccess runs the step only when the upstream steps succeeded.
	// OnFailure runs it only when an upstream step failed.
	OnSuccess *bool `json:"on_success,omitempty"`
	OnFailure *bool `json:"on_failure,omitempty"`
	// Detached runs the step in the background. The pipeline does not
	// wait for it.
	Detached bool `json:"detached,omitempty"`
	// Services are sidecar containers that run alongside the step.
	Services []ServiceSpec `json:"services,omitempty"`
}

// ServiceSpec is a sidecar container that runs alongside a step.
type ServiceSpec struct {
	ID        string     `json:"id"`
	Image     string     `json:"image"`
	Commands  StringList `json:"commands,omitempty"`
	Env       ScalarMap  `json:"env,omitempty"`
	Ports     []string   `json:"ports,omitempty"`
	Detached  bool       `json:"detached,omitempty"`
	Resources *Resources `json:"resources,omitempty"`
}

// Parse decodes a pipeline document from YAML or JSON.
func Parse(data []byte) (PipelineSpec, error) {
	var spec PipelineSpec
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return spec, fmt.Errorf("pipeline document is empty")
	}

	var document any
	if trimmed[0] == '{' {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		if err := decoder.Decode(&document); err != nil {
			return spec, fmt.Errorf("decode pipeline JSON: %w", err)
		}
	} else {
		lines, err := scanLines(data)
		if err != nil {
			return spec, err
		}
		if len(lines) == 0 {
			return spec, fmt.Errorf("pipeline document is empty")
		}
		document, _, err = parseBlock(lines, 0, lines[0].indent)
		if err != nil {
			return spec, err
		}
	}

	root, ok := document.(map[string]any)
	if !ok {
		return spec, fmt.Errorf("pipeline document must be a mapping")
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return spec, fmt.Errorf("normalize pipeline document: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return spec, fmt.Errorf("decode pipeline: %w", err)
	}
	if err := Validate(&spec); err != nil {
		return spec, err
	}
	return spec, nil
}

// Validate checks a pipeline for structural errors that the DAG compiler
// cannot detect on its own.
func Validate(spec *PipelineSpec) error {
	// Normalise the aliases, so that the rest of the system only ever
	// sees one spelling of each field.
	if spec.GitCommitTree == "" {
		spec.GitCommitTree = spec.GitCommit
	}
	for index := range spec.Steps {
		step := &spec.Steps[index]
		if len(step.Run) == 0 && len(step.Commands) > 0 {
			step.Run = step.Commands
		}
	}
	if spec.Name == "" {
		return fmt.Errorf("pipeline name is required")
	}
	if len(spec.Steps) == 0 {
		return fmt.Errorf("pipeline must declare at least one step")
	}
	known := make(map[string]struct{}, len(spec.Steps))
	for index := range spec.Steps {
		step := &spec.Steps[index]
		if step.ID == "" {
			return fmt.Errorf("step %d is missing an id", index)
		}
		if _, duplicate := known[step.ID]; duplicate {
			return fmt.Errorf("duplicate step id %q", step.ID)
		}
		known[step.ID] = struct{}{}
		if step.Image == "" && step.Plugin == "" {
			return fmt.Errorf("step %q must declare either image or plugin", step.ID)
		}
		if step.Image != "" && step.Plugin != "" {
			return fmt.Errorf("step %q must declare either image or plugin, not both", step.ID)
		}
		if step.Plugin != "" && len(step.Run) > 0 {
			return fmt.Errorf("step %q must not combine plugin with run", step.ID)
		}
		if step.Parallelism < 0 || step.Parallelism > 64 {
			return fmt.Errorf("step %q parallelism must be between 0 and 64", step.ID)
		}
		if step.TimeoutSeconds < 0 {
			return fmt.Errorf("step %q timeout_seconds must not be negative", step.ID)
		}
		if step.MaxParallel < 0 {
			return fmt.Errorf("step %q max_parallel must not be negative", step.ID)
		}
		if step.Parallelism > 0 && step.MaxParallel > 0 && step.MaxParallel > step.Parallelism {
			return fmt.Errorf("step %q max_parallel %d exceeds the shard count %d", step.ID, step.MaxParallel, step.Parallelism)
		}
		if step.Resources != nil {
			for name, value := range step.Resources.Requests {
				if err := validateQuantity(name, value); err != nil {
					return fmt.Errorf("step %q request %s: %w", step.ID, name, err)
				}
			}
			for name, value := range step.Resources.Limits {
				if err := validateQuantity(name, value); err != nil {
					return fmt.Errorf("step %q limit %s: %w", step.ID, name, err)
				}
			}
		}
		switch step.Failure {
		case "", "fail", "ignore", "cancel":
		default:
			return fmt.Errorf("step %q failure must be fail, ignore or cancel", step.ID)
		}
		for _, condition := range step.When {
			if err := condition.validate(); err != nil {
				return fmt.Errorf("step %q: %w", step.ID, err)
			}
		}
		for dimension, values := range step.Matrix {
			if dimension == "" {
				return fmt.Errorf("step %q has an empty matrix dimension", step.ID)
			}
			if len(values) == 0 {
				return fmt.Errorf("step %q matrix dimension %q has no values", step.ID, dimension)
			}
		}
	}
	for index := range spec.Steps {
		step := &spec.Steps[index]
		seen := make(map[string]struct{}, len(step.DependsOn))
		for _, dependency := range step.DependsOn {
			if dependency == step.ID {
				return fmt.Errorf("step %q depends on itself", step.ID)
			}
			if _, ok := known[dependency]; !ok {
				return fmt.Errorf("step %q depends on unknown step %q", step.ID, dependency)
			}
			if _, duplicate := seen[dependency]; duplicate {
				return fmt.Errorf("step %q lists dependency %q twice", step.ID, dependency)
			}
			seen[dependency] = struct{}{}
		}
	}
	return nil
}

func scalarToString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case nil:
		return ""
	default:
		return fmt.Sprint(typed)
	}
}

// sourceLine is a single significant line of the document.
type sourceLine struct {
	indent int
	text   string
	number int
}

func scanLines(data []byte) ([]sourceLine, error) {
	var result []sourceLine
	raw := strings.ReplaceAll(string(data), "\r\n", "\n")
	for number, text := range strings.Split(raw, "\n") {
		indent := 0
		for indent < len(text) && text[indent] == ' ' {
			indent++
		}
		if indent < len(text) && text[indent] == '\t' {
			return nil, fmt.Errorf("line %d: tabs may not be used for indentation", number+1)
		}
		body := stripComment(text[indent:])
		body = strings.TrimRight(body, " \t")
		if body == "" {
			continue
		}
		if body == "---" || body == "..." {
			continue
		}
		if strings.HasPrefix(body, "&") || strings.HasPrefix(body, "*") || strings.HasPrefix(body, "!") {
			return nil, fmt.Errorf("line %d: anchors, aliases and tags are not supported", number+1)
		}
		result = append(result, sourceLine{indent: indent, text: body, number: number + 1})
	}
	return result, nil
}

// stripComment removes a trailing YAML comment while respecting quotes.
func stripComment(text string) string {
	var quote byte
	escaped := false
	for index := 0; index < len(text); index++ {
		char := text[index]
		if quote == '\'' {
			if char == '\'' {
				if index+1 < len(text) && text[index+1] == '\'' {
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == '"' {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char == '#' && (index == 0 || text[index-1] == ' ' || text[index-1] == '\t') {
			return text[:index]
		}
	}
	return text
}

func isSequenceItem(text string) bool {
	return text == "-" || strings.HasPrefix(text, "- ")
}

func parseBlock(lines []sourceLine, index, indent int) (any, int, error) {
	if index >= len(lines) {
		return nil, index, nil
	}
	if isSequenceItem(lines[index].text) {
		return parseSequence(lines, index, indent)
	}
	return parseMapping(lines, index, indent)
}

func parseMapping(lines []sourceLine, index, indent int) (map[string]any, int, error) {
	result := make(map[string]any)
	for index < len(lines) && lines[index].indent == indent {
		line := lines[index]
		if isSequenceItem(line.text) {
			return nil, index, fmt.Errorf("line %d: unexpected sequence item inside a mapping", line.number)
		}
		key, rest, ok := splitKey(line.text)
		if !ok {
			return nil, index, fmt.Errorf("line %d: expected \"key: value\"", line.number)
		}
		if _, duplicate := result[key]; duplicate {
			return nil, index, fmt.Errorf("line %d: duplicate key %q", line.number, key)
		}
		index++
		value, next, err := parseValue(lines, index, indent, rest, line.number)
		if err != nil {
			return nil, index, err
		}
		result[key] = value
		index = next
	}
	return result, index, nil
}

func parseValue(lines []sourceLine, index, indent int, rest string, number int) (any, int, error) {
	rest = strings.TrimSpace(rest)
	switch {
	case rest == "":
		if index < len(lines) && lines[index].indent > indent {
			return parseBlock(lines, index, lines[index].indent)
		}
		if index < len(lines) && lines[index].indent == indent && isSequenceItem(lines[index].text) {
			return parseSequence(lines, index, indent)
		}
		return nil, index, nil
	case rest == "|" || rest == "|-" || rest == ">" || rest == ">-":
		return parseBlockScalar(lines, index, indent, rest)
	default:
		value, err := parseScalar(rest)
		if err != nil {
			return nil, index, fmt.Errorf("line %d: %w", number, err)
		}
		return value, index, nil
	}
}

func parseBlockScalar(lines []sourceLine, index, indent int, style string) (any, int, error) {
	var collected []string
	blockIndent := -1
	for index < len(lines) && lines[index].indent > indent {
		if blockIndent < 0 {
			blockIndent = lines[index].indent
		}
		padding := lines[index].indent - blockIndent
		if padding < 0 {
			padding = 0
		}
		collected = append(collected, strings.Repeat(" ", padding)+lines[index].text)
		index++
	}
	if style[0] == '>' {
		return strings.Join(collected, " "), index, nil
	}
	return strings.Join(collected, "\n"), index, nil
}

func parseSequence(lines []sourceLine, index, indent int) ([]any, int, error) {
	result := make([]any, 0)
	for index < len(lines) && lines[index].indent == indent && isSequenceItem(lines[index].text) {
		line := lines[index]
		content := strings.TrimPrefix(line.text, "-")
		trimmed := strings.TrimLeft(content, " ")
		if trimmed == "" {
			index++
			if index < len(lines) && lines[index].indent > indent {
				value, next, err := parseBlock(lines, index, lines[index].indent)
				if err != nil {
					return nil, index, err
				}
				result = append(result, value)
				index = next
				continue
			}
			result = append(result, nil)
			continue
		}
		if _, _, ok := splitKey(trimmed); ok {
			// Rewrite the item so the mapping parser sees a plain block
			// starting at the column of the item content.
			contentIndent := indent + 1 + (len(content) - len(trimmed))
			rewritten := make([]sourceLine, 0, len(lines)-index)
			rewritten = append(rewritten, sourceLine{indent: contentIndent, text: trimmed, number: line.number})
			rewritten = append(rewritten, lines[index+1:]...)
			value, consumed, err := parseBlock(rewritten, 0, contentIndent)
			if err != nil {
				return nil, index, err
			}
			result = append(result, value)
			index += consumed
			continue
		}
		value, err := parseScalar(trimmed)
		if err != nil {
			return nil, index, fmt.Errorf("line %d: %w", line.number, err)
		}
		result = append(result, value)
		index++
	}
	return result, index, nil
}

// splitKey splits "key: value" using YAML block-context rules: the key
// terminator is a colon followed by whitespace or end of line.
func splitKey(text string) (string, string, bool) {
	var quote byte
	escaped := false
	depth := 0
	for index := 0; index < len(text); index++ {
		char := text[index]
		if quote == '\'' {
			if char == '\'' {
				if index+1 < len(text) && text[index+1] == '\'' {
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == '"' {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		switch char {
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		case ':':
			if depth != 0 {
				continue
			}
			if index+1 < len(text) && text[index+1] != ' ' && text[index+1] != '\t' {
				continue
			}
			key := strings.TrimSpace(text[:index])
			if key == "" {
				return "", "", false
			}
			if unquoted, err := strconv.Unquote(key); err == nil {
				key = unquoted
			}
			return key, strings.TrimSpace(text[index+1:]), true
		}
	}
	return "", "", false
}

func parseScalar(raw string) (any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	if raw == "null" || raw == "~" {
		return nil, nil
	}
	if raw == "true" {
		return true, nil
	}
	if raw == "false" {
		return false, nil
	}
	if strings.HasPrefix(raw, "\"") {
		var value string
		if err := json.Unmarshal([]byte(raw), &value); err != nil {
			return nil, fmt.Errorf("invalid double quoted scalar: %w", err)
		}
		return value, nil
	}
	if strings.HasPrefix(raw, "'") {
		if !strings.HasSuffix(raw, "'") || len(raw) < 2 {
			return nil, fmt.Errorf("invalid single quoted scalar")
		}
		return strings.ReplaceAll(raw[1:len(raw)-1], "''", "'"), nil
	}
	if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
		parts, err := splitFlow(raw[1:len(raw)-1], ',')
		if err != nil {
			return nil, err
		}
		values := make([]any, 0, len(parts))
		for _, part := range parts {
			value, err := parseScalar(part)
			if err != nil {
				return nil, err
			}
			values = append(values, value)
		}
		return values, nil
	}
	if strings.HasPrefix(raw, "{") && strings.HasSuffix(raw, "}") {
		parts, err := splitFlow(raw[1:len(raw)-1], ',')
		if err != nil {
			return nil, err
		}
		values := make(map[string]any, len(parts))
		for _, part := range parts {
			key, value, ok := splitKey(part)
			if !ok {
				return nil, fmt.Errorf("invalid flow mapping entry %q", part)
			}
			decoded, err := parseScalar(value)
			if err != nil {
				return nil, err
			}
			values[key] = decoded
		}
		return values, nil
	}
	if integer, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return integer, nil
	}
	if number, err := strconv.ParseFloat(raw, 64); err == nil {
		return number, nil
	}
	return raw, nil
}

// splitFlow splits a flow collection body on a separator, honouring
// nesting and quoting.
func splitFlow(text string, separator byte) ([]string, error) {
	var result []string
	start := 0
	var quote byte
	depth := 0
	escaped := false
	for index := 0; index < len(text); index++ {
		char := text[index]
		if quote == '\'' {
			if char == '\'' {
				if index+1 < len(text) && text[index+1] == '\'' {
					index++
					continue
				}
				quote = 0
			}
			continue
		}
		if quote == '"' {
			if escaped {
				escaped = false
				continue
			}
			if char == '\\' {
				escaped = true
				continue
			}
			if char == '"' {
				quote = 0
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		switch char {
		case '[', '{':
			depth++
		case ']', '}':
			depth--
		case separator:
			if depth == 0 {
				result = append(result, strings.TrimSpace(text[start:index]))
				start = index + 1
			}
		}
	}
	if quote != 0 || depth != 0 {
		return nil, fmt.Errorf("unterminated flow collection")
	}
	result = append(result, strings.TrimSpace(text[start:]))
	return result, nil
}

// validateQuantity checks that a Kubernetes resource quantity is
// syntactically plausible. The Kubernetes API server performs the
// authoritative validation; this catches obvious mistakes early.
func validateQuantity(name, value string) error {
	if value == "" {
		return fmt.Errorf("the quantity is empty")
	}
	switch name {
	case "cpu", "memory", "ephemeral-storage":
	default:
		if !strings.Contains(name, "/") && !strings.HasPrefix(name, "requests.") {
			return fmt.Errorf("%q is not a recognised resource name", name)
		}
	}
	for index, char := range value {
		switch {
		case char >= '0' && char <= '9':
		case char == '.' || char == 'm' || char == 'k' || char == 'M' ||
			char == 'G' || char == 'T' || char == 'P' || char == 'E' ||
			char == 'i' || char == 'K':
		case char == '-' || char == '+':
			if index != 0 {
				return fmt.Errorf("%q is not a valid quantity", value)
			}
		default:
			return fmt.Errorf("%q is not a valid quantity", value)
		}
	}
	return nil
}

// Decode parses a YAML or JSON document into a Go value.
//
// Unknown fields are rejected. This is the same strictness that
// Parse applies to pipelines, exposed for other configuration formats.
func Decode(data []byte, out any) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("document is empty")
	}
	var document any
	if trimmed[0] == '{' {
		decoder := json.NewDecoder(bytes.NewReader(trimmed))
		decoder.UseNumber()
		if err := decoder.Decode(&document); err != nil {
			return err
		}
	} else {
		lines, err := scanLines(data)
		if err != nil {
			return err
		}
		if len(lines) == 0 {
			return fmt.Errorf("document is empty")
		}
		document, _, err = parseBlock(lines, 0, lines[0].indent)
		if err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	return decoder.Decode(out)
}

// SortedKeys returns the sorted key set of a scalar map. It exists so the
// compiler can produce deterministic fingerprints.
func SortedKeys(values ScalarMap) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
