package setup

import (
	"fmt"
	"strings"

	"github.com/skyhuang233/workflow/internal/setupcontract"
)

func Project(plan setupcontract.Plan, digest string) string {
	var output strings.Builder
	fmt.Fprintf(&output, "Setup Plan %s\nKind: %s\nTarget: %s\nDigest (SHA-256): %s\n\nAuthorized effects:\n", plan.PlanID, plan.Kind, targetName(plan), digest)
	for _, effect := range plan.Effects {
		fmt.Fprintf(&output, "- %s: %s %s (%s)\n", effect.ID, effect.Action, effect.Subject, effect.Kind)
	}
	output.WriteString("\nExpected results:\n")
	for _, result := range plan.ExpectedResults {
		fmt.Fprintf(&output, "- %s: %s = %s\n", result.ID, result.Subject, result.Expected)
	}
	return output.String()
}
