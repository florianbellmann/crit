package server

import (
	"testing"

	"github.com/tomasz-tomczyk/crit/internal/forge"
)

func TestADOAwareForgeRoutesPRFlag(t *testing.T) {
	old := DetectForgeKindFn
	t.Cleanup(func() { DetectForgeKindFn = old })
	adoURL := "https://dev.azure.com/org/proj/_git/repo/pullrequest/7"
	cases := []struct {
		name, forgeFlag, spec string
		checkout              forge.Kind
		want                  string
	}{
		{"bare number in ADO checkout", "github", "7", forge.AzureDevOps, "azuredevops"},
		{"bare number in GitHub checkout", "github", "7", forge.GitHub, "github"},
		{"ADO URL anywhere", "github", adoURL, forge.GitHub, "azuredevops"},
		{"GitHub URL in ADO checkout", "github", "https://github.com/a/b/pull/7", forge.AzureDevOps, "github"},
		{"explicit --mr untouched", "gitlab", "7", forge.AzureDevOps, "gitlab"},
		{"explicit --ado", "azuredevops", "7", forge.GitHub, "azuredevops"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			DetectForgeKindFn = func() forge.Kind { return c.checkout }
			if got := adoAwareForge(c.forgeFlag, c.spec); got != c.want {
				t.Errorf("adoAwareForge(%q, %q) = %q, want %q", c.forgeFlag, c.spec, got, c.want)
			}
		})
	}
}
