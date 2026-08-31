// Package pbac implements policy-based access control for the CVFF API with
// an embedded OPA rego evaluator (github.com/open-policy-agent/opa/rego) — a
// library, not a sidecar. Policy files load from a configured directory at
// startup; an unreadable directory, an unparseable policy or zero loaded
// policies refuses the boot. Every evaluation is deny-by-default: an
// undefined, false or erroneous policy result denies the request.
package pbac

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-policy-agent/opa/rego"
)

// policyQuery is the single authorization decision every policy pack must
// define: a boolean `allow` in package cvff.
const policyQuery = "data.cvff.allow"

// Principal is the verified identity a policy evaluates.
type Principal struct {
	Subject   string   `json:"subject"`
	Roles     []string `json:"roles"`
	Clearance string   `json:"clearance,omitempty"`
	TenantID  string   `json:"tenant_id,omitempty"`
}

// Input is the authorization evaluation input for one request. Assignments
// carries the proposed CVFF role bindings on role-assignment requests so the
// policy can enforce four-party segregation of duties.
type Input struct {
	Principal      Principal         `json:"principal"`
	TenantID       string            `json:"tenant_id"`
	Resource       string            `json:"resource"`
	Action         string            `json:"action"`
	Classification string            `json:"classification"`
	Assignments    map[string]string `json:"assignments,omitempty"`
}

// Enforcer evaluates the loaded policy pack. It is safe for concurrent use.
type Enforcer struct {
	query rego.PreparedEvalQuery
}

// LoadEnforcer reads every .rego file in dir and compiles the policy pack.
// It fails closed: an unreadable directory, an unreadable file, zero policy
// files or a pack that does not compile are all startup errors.
func LoadEnforcer(dir string) (*Enforcer, error) {
	if dir == "" || strings.TrimSpace(dir) != dir {
		return nil, errors.New("policy directory is required and must be canonical")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read policy directory: %w", err)
	}
	modules := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".rego") {
			continue
		}
		content, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read policy %s: %w", entry.Name(), err)
		}
		modules[entry.Name()] = string(content)
	}
	if len(modules) == 0 {
		return nil, fmt.Errorf("policy directory %s contains no .rego policies; refusing to start without authorization policy", dir)
	}
	return NewEnforcer(modules)
}

// NewEnforcer compiles the given policy modules (name -> rego source) and
// prepares the authorization query. A pack that does not compile is a hard
// error: a service without evaluable policy must never boot.
func NewEnforcer(modules map[string]string) (*Enforcer, error) {
	if len(modules) == 0 {
		return nil, errors.New("at least one policy module is required")
	}
	names := make([]string, 0, len(modules))
	for name := range modules {
		names = append(names, name)
	}
	sort.Strings(names)
	options := []func(*rego.Rego){rego.Query(policyQuery)}
	for _, name := range names {
		options = append(options, rego.Module(name, modules[name]))
	}
	prepared, err := rego.New(options...).PrepareForEval(context.Background())
	if err != nil {
		return nil, fmt.Errorf("compile policy pack: %w", err)
	}
	return &Enforcer{query: prepared}, nil
}

// Allow evaluates the policy for one request. Deny-by-default: evaluation
// errors, undefined results and non-boolean results all deny.
func (enforcer *Enforcer) Allow(ctx context.Context, input Input) bool {
	results, err := enforcer.query.Eval(ctx, rego.EvalInput(input))
	if err != nil || len(results) == 0 || len(results[0].Expressions) == 0 {
		return false
	}
	allow, ok := results[0].Expressions[0].Value.(bool)
	return ok && allow
}
