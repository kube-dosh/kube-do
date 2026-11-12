package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCiliumValuesUseMirroredOperatorRepository(t *testing.T) {
	path := filepath.Join("..", "..", "ansible", "roles", "cilium", "templates", "values.yaml.j2")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "repository: {{ kube_do_operator_image.repository }}") {
		t.Fatal("Cilium operator repository must use the mirrored operator image metadata")
	}
	if strings.Contains(string(data), "/cilium/operator\n") {
		t.Fatal("Cilium operator repository must not bypass the manifest target")
	}
}
