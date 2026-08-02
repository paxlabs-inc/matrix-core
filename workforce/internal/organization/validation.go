package organization

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

func validateID(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("organization: %s is required", name)
	}
	if len(value) > 128 {
		return fmt.Errorf("organization: %s exceeds 128 bytes", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '-' || character == '_' || character == '.' ||
			character == ':' || character == '/' {
			continue
		}
		return fmt.Errorf("organization: %s contains an invalid character", name)
	}
	return nil
}

func validateText(name, value string, maximum int) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maximum {
		return fmt.Errorf("organization: %s must contain 1 to %d bytes", name, maximum)
	}
	return nil
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func validateOptionalExpiry(effectiveAt time.Time, expiresAt *time.Time) error {
	if !validUTC(effectiveAt) {
		return fmt.Errorf("organization: effective_at must be a non-zero UTC timestamp")
	}
	if expiresAt != nil && (!validUTC(*expiresAt) || !expiresAt.After(effectiveAt)) {
		return fmt.Errorf("organization: expires_at must be UTC and after effective_at")
	}
	return nil
}

func validateSortedUnique(name string, values []string, minimum, maximum int) error {
	if len(values) < minimum || len(values) > maximum {
		return fmt.Errorf("organization: %s must contain %d to %d entries", name, minimum, maximum)
	}
	if !slices.IsSorted(values) {
		return fmt.Errorf("organization: %s must be sorted", name)
	}
	for index, value := range values {
		if err := validateID(name, value); err != nil {
			return err
		}
		if index > 0 && values[index-1] == value {
			return fmt.Errorf("organization: %s contains duplicate %q", name, value)
		}
	}
	return nil
}

func containsString(values []string, target string) bool {
	_, found := slices.BinarySearch(values, target)
	return found
}
