package charts

import (
	"os"
	"sigs.k8s.io/yaml"
	"strings"
	"testing"
)

func TestFleetPermissionCoverageEnabledInCI(t *testing.T) {
	data, err := os.ReadFile("../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run string
				Env map[string]string
			}
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	staged, covered := false, false
	for _, step := range workflow.Jobs["test"].Steps {
		if strings.Contains(step.Run, "fleet-stage.sh") {
			staged = true
		}
		if strings.Contains(step.Run, "go test ./...") && staged && step.Env["AEP_FLEET_CHART_TEST_DIR"] != "" {
			covered = true
		}
	}
	if !covered {
		t.Fatal("CI skips rendered fleet permission coverage")
	}
}
