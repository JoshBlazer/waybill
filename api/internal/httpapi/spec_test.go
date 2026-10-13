package httpapi_test

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// readSpecSecurity returns the operations (in oapi-codegen's PascalCase)
// that declare a non-empty `security` requirement in the OpenAPI file.
func readSpecSecurity(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var spec struct {
		Paths map[string]map[string]struct {
			OperationID string           `yaml:"operationId"`
			Security    []map[string]any `yaml:"security"`
		} `yaml:"paths"`
	}
	if err := yaml.Unmarshal(raw, &spec); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, methods := range spec.Paths {
		for _, op := range methods {
			if len(op.Security) > 0 && op.OperationID != "" {
				out[strings.ToUpper(op.OperationID[:1])+op.OperationID[1:]] = true
			}
		}
	}
	return out, nil
}
