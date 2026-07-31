package skills

import (
	"fmt"
	"sort"

	"matrix/workforce/internal/contracts"
)

// WorkforcePack returns the exact built-in contract set referenced by the
// canonical seven-department mandates.
func WorkforcePack() ([]Contract, error) {
	builders := []func() ([]Contract, error){
		DeveloperPack,
		ExecutiveResearchPack,
		MarketingLegalPack,
		OperationsPack,
	}
	byID := make(map[contracts.SkillID]Contract)
	for _, build := range builders {
		values, err := build()
		if err != nil {
			return nil, err
		}
		for _, value := range values {
			if existing, duplicate := byID[value.ID]; duplicate &&
				existing.Digest != value.Digest {
				return nil, fmt.Errorf(
					"skills: conflicting built-in contract %q", value.ID,
				)
			}
			byID[value.ID] = value
		}
	}
	result := make([]Contract, 0, len(byID))
	for _, value := range byID {
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].ID < result[right].ID
	})
	return result, nil
}
