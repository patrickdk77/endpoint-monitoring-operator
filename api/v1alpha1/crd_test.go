package v1alpha1

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"
)

func crdSchema(t *testing.T) map[string]any {
	t.Helper()
	data, err := os.ReadFile("../../config/crd/bases/monitoring.licious.app_endpointmonitors.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Schema struct {
					OpenAPIV3Schema map[string]any `json:"openAPIV3Schema"`
				} `json:"schema"`
			} `json:"versions"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 {
		t.Fatalf("%d versions in the CRD", len(crd.Spec.Versions))
	}
	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema
}

func TestCRDCheckIntervalBoundsMatchCode(t *testing.T) {
	root := crdSchema(t)
	required, _ := root["required"].([]any)
	if len(required) != 1 || required[0] != "spec" {
		t.Fatalf("spec must be required, got %v", root["required"])
	}
	spec := root["properties"].(map[string]any)["spec"].(map[string]any)
	interval := spec["properties"].(map[string]any)["checkInterval"].(map[string]any)
	if interval["minimum"] != float64(1) {
		t.Fatalf("checkInterval minimum %v, want 1", interval["minimum"])
	}
	if interval["maximum"] != float64(MaxCheckInterval) {
		t.Fatalf("checkInterval maximum %v, want MaxCheckInterval %d", interval["maximum"], MaxCheckInterval)
	}
}
