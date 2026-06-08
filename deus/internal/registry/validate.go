package registry

import (
	"fmt"
	"net/http"
	"time"

	"github.com/paxlabs-inc/deus/pkg/manifest"
)

// ValidationResult is returned from manifest validation.
type ValidationResult struct {
	OK       bool     `json:"ok"`
	Warnings []string `json:"warnings"`
	Errors   []string `json:"errors,omitempty"`
}

// ValidateManifest runs schema + business validation.
func ValidateManifest(m *manifest.Manifest) ValidationResult {
	if err := manifest.Validate(m); err != nil {
		return ValidationResult{OK: false, Errors: []string{err.Error()}}
	}
	var warnings []string
	if m.Mode == "proxy" && m.Endpoint != nil && m.Endpoint.ProxyURL != "" {
		if err := probeURL(m.Endpoint.ProxyURL); err != nil {
			warnings = append(warnings, "proxy_url unreachable: "+err.Error())
		}
	}
	return ValidationResult{OK: true, Warnings: warnings}
}

func probeURL(raw string) error {
	client := &http.Client{Timeout: 3 * time.Second}
	req, err := http.NewRequest(http.MethodHead, raw, http.NoBody)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}
