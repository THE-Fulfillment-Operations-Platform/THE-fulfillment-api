package docs

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// The spec is served to Swagger UI as-is: a YAML typo turns the API reference
// into a blank page, and nothing else in the build would catch it.
func TestOpenAPISpecIsValidYAML(t *testing.T) {
	// Both specs: the internal one, and the one sent to sellers' developers.
	for _, name := range []string{"openapi.yaml", "open_api.yaml"} {
		raw, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s is not valid YAML: %v", name, err)
		}
		if _, ok := doc["paths"]; !ok {
			t.Fatalf("%s has no paths section", name)
		}
	}
}
