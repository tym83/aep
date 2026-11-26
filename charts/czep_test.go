package charts

import (
	"os"
	"strings"
	"testing"
)

func TestMachineProposalUsesGenericApprovalIntegration(t *testing.T) {
	data, err := os.ReadFile("../docs/upstream/node-onboarding-czep.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "AEP") || strings.Contains(text, "Owner decision") || !strings.Contains(text, "an external approval system") {
		t.Fatal("proposal is coupled to a specific approval product")
	}
}
