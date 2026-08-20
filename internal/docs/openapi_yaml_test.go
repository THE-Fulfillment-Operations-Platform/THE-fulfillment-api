package docs

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// The spec is served to Swagger UI as-is: a YAML typo turns the API reference
// into a blank page, and nothing else in the build would catch it.
func TestOpenAPISpecIsValidYAML(t *testing.T) {
	raw, err := os.ReadFile("openapi.yaml")
	if err != nil {
		t.Fatalf("read spec: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	if _, ok := doc["paths"]; !ok {
		t.Fatal("spec has no paths section")
	}
}
