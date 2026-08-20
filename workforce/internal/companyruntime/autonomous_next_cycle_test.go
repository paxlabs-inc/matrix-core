package companyruntime

import (
	"slices"
	"testing"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/portfolio"
)

func TestAutonomousCycleCoverage_MapsSupportedCadencesExactly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		kind         portfolio.CadenceKind
		departments  []contracts.DepartmentKind
		capabilities []string
	}{
		{
			name: "discovery",
			kind: portfolio.CadenceDiscovery,
			departments: []contracts.DepartmentKind{
				contracts.DepartmentExecutive,
				contracts.DepartmentResearch,
			},
			capabilities: []string{
				"decision.portfolio",
				"market.research",
				"opportunity.intake",
			},
		},
		{
			name: "learning",
			kind: portfolio.CadenceLearning,
			departments: []contracts.DepartmentKind{
				contracts.DepartmentAccounting,
				contracts.DepartmentExecutive,
				contracts.DepartmentResearch,
			},
			capabilities: []string{
				"decision.portfolio",
				"learning.review",
				"measurement.review",
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			departments, capabilities := autonomousCycleCoverage(test.kind)
			if !slices.Equal(departments, test.departments) {
				t.Fatalf("departments = %v, want %v", departments, test.departments)
			}
			if !slices.Equal(capabilities, test.capabilities) {
				t.Fatalf("capabilities = %v, want %v", capabilities, test.capabilities)
			}
		})
	}
}

func TestAutonomousCycleCoverage_RejectsUnsupportedCadence(t *testing.T) {
	t.Parallel()

	departments, capabilities := autonomousCycleCoverage(portfolio.CadencePortfolio)
	if len(departments) != 0 || len(capabilities) != 0 {
		t.Fatalf("unsupported cadence returned departments=%v capabilities=%v", departments, capabilities)
	}
}
