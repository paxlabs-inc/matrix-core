package server

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"matrix/neo/internal/machinemailsettings"
	"matrix/neo/internal/tools"
)

var machineMailKeyPattern = regexp.MustCompile(`^mm_[A-Za-z0-9_-]{20,200}$`)

type machineMailStatus struct {
	Available         bool   `json:"available"`
	Configured        bool   `json:"configured"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type machineMailBridge struct {
	store *machinemailsettings.Store
	tools *tools.Manager
}

func newMachineMailBridge(store *machinemailsettings.Store, manager *tools.Manager) *machineMailBridge {
	return &machineMailBridge{store: store, tools: manager}
}

func (b *machineMailBridge) Status() machineMailStatus {
	if b == nil || b.store == nil || b.tools == nil {
		return machineMailStatus{UnavailableReason: "MachineMail is not available on this Neo daemon"}
	}
	status := machineMailStatus{Available: b.store.Secure(), Configured: b.store.View().Configured()}
	if !status.Available {
		status.UnavailableReason = "Neo's encrypted user vault is not available; the API key was not saved"
	}
	return status
}

func (b *machineMailBridge) Restore(ctx context.Context) error {
	if b == nil || b.store == nil || b.tools == nil {
		return errors.New("MachineMail is not available on this Neo daemon")
	}
	key := strings.TrimSpace(b.store.View().APIKey)
	if key == "" {
		return nil
	}
	restoreCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return b.tools.LoadMachineMail(restoreCtx, key)
}

func (b *machineMailBridge) Configure(ctx context.Context, apiKey string) (machineMailStatus, error) {
	if b == nil || b.store == nil || b.tools == nil {
		return machineMailStatus{}, errors.New("MachineMail is not available on this Neo daemon")
	}
	if !b.store.Secure() {
		return b.Status(), machinemailsettings.ErrEncryptionRequired
	}
	apiKey = strings.TrimSpace(apiKey)
	if !machineMailKeyPattern.MatchString(apiKey) {
		return b.Status(), errors.New("the MachineMail API key format is invalid")
	}
	configureCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := b.tools.ConfigureMachineMail(configureCtx, apiKey); err != nil {
		return b.Status(), err
	}
	previous := b.store.View()
	if err := b.store.Replace(apiKey); err != nil {
		if previous.Configured() {
			_ = b.tools.LoadMachineMail(context.Background(), previous.APIKey)
		} else {
			_ = b.tools.ClearMachineMail(context.Background())
		}
		return b.Status(), err
	}
	return b.Status(), nil
}

func (b *machineMailBridge) Disconnect(ctx context.Context) error {
	if b == nil || b.store == nil || b.tools == nil {
		return nil
	}
	previous := b.store.View()
	clearCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := b.tools.ClearMachineMail(clearCtx); err != nil {
		return err
	}
	if err := b.store.Clear(); err != nil {
		if previous.Configured() {
			_ = b.tools.LoadMachineMail(context.Background(), previous.APIKey)
		}
		return err
	}
	return nil
}
