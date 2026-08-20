package financial

import (
	"context"
	"fmt"
	"time"

	"centra/workforce/internal/contracts"
)

type IncidentView struct {
	ID                string                   `json:"id"`
	OrganizationID    contracts.OrganizationID `json:"organization_id"`
	ReservationID     string                   `json:"reservation_id"`
	ConnectionID      string                   `json:"connection_id"`
	ConnectionVersion uint64                   `json:"connection_version"`
	Operation         string                   `json:"operation"`
	IdempotencyKey    string                   `json:"idempotency_key"`
	Kind              string                   `json:"kind"`
	SafeCode          string                   `json:"safe_code"`
	State             string                   `json:"state"`
	CreatedAt         time.Time                `json:"created_at"`
	ResolvedAt        *time.Time               `json:"resolved_at"`
}

type AccountingEntryView struct {
	ID             string                   `json:"id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	ObservationID  string                   `json:"observation_id"`
	ReservationID  string                   `json:"reservation_id"`
	InitiativeID   string                   `json:"initiative_id"`
	AccountID      string                   `json:"account_id"`
	Side           AccountingSide           `json:"side"`
	Currency       string                   `json:"currency"`
	Microunits     uint64                   `json:"microunits"`
	ValuationTime  time.Time                `json:"valuation_time"`
	MethodologyID  string                   `json:"methodology_id"`
	EvidenceHash   contracts.ContentHash    `json:"evidence_hash"`
	CreatedAt      time.Time                `json:"created_at"`
}

func (store *Store) ListOpenIncidents(
	ctx context.Context,
	organizationID contracts.OrganizationID,
) ([]IncidentView, error) {
	if token("organization id", string(organizationID)) != nil {
		return nil, fmt.Errorf("financial adapter: organization identity is invalid")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT incident_id,COALESCE(reservation_id,''),connection_id,
		       connection_version,operation,idempotency_key,kind,safe_code,state,
		       created_at,resolved_at
		FROM workforce_financial_incidents
		WHERE tenant_id=$1 AND organization_id=$2 AND state IN ('open','escalated')
		ORDER BY created_at,incident_id
	`, store.tenantID, organizationID)
	if err != nil {
		return nil, fmt.Errorf("financial adapter: list financial incidents: %w", err)
	}
	defer rows.Close()
	views := make([]IncidentView, 0)
	for rows.Next() {
		view := IncidentView{OrganizationID: organizationID}
		if err := rows.Scan(&view.ID, &view.ReservationID, &view.ConnectionID,
			&view.ConnectionVersion, &view.Operation, &view.IdempotencyKey,
			&view.Kind, &view.SafeCode, &view.State, &view.CreatedAt,
			&view.ResolvedAt); err != nil {
			return nil, fmt.Errorf("financial adapter: scan financial incident: %w", err)
		}
		view.CreatedAt = view.CreatedAt.UTC()
		if view.ResolvedAt != nil {
			value := view.ResolvedAt.UTC()
			view.ResolvedAt = &value
		}
		views = append(views, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("financial adapter: iterate financial incidents: %w", err)
	}
	return views, nil
}

func (store *Store) ListAccountingEntries(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	initiativeID string,
) ([]AccountingEntryView, error) {
	if token("organization id", string(organizationID)) != nil ||
		token("initiative id", initiativeID) != nil {
		return nil, fmt.Errorf("financial adapter: accounting scope is invalid")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT entry_id,observation_id,reservation_id,account_id,side,currency,
		       microunits,valuation_time,methodology_id,evidence_hash,created_at
		FROM workforce_financial_accounting_entries
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
		ORDER BY created_at,entry_id
	`, store.tenantID, organizationID, initiativeID)
	if err != nil {
		return nil, fmt.Errorf("financial adapter: list accounting entries: %w", err)
	}
	defer rows.Close()
	views := make([]AccountingEntryView, 0)
	for rows.Next() {
		view := AccountingEntryView{OrganizationID: organizationID, InitiativeID: initiativeID}
		if err := rows.Scan(&view.ID, &view.ObservationID, &view.ReservationID,
			&view.AccountID, &view.Side, &view.Currency, &view.Microunits,
			&view.ValuationTime, &view.MethodologyID, &view.EvidenceHash.Digest,
			&view.CreatedAt); err != nil {
			return nil, fmt.Errorf("financial adapter: scan accounting entry: %w", err)
		}
		view.EvidenceHash.Algorithm = "sha256"
		view.ValuationTime = view.ValuationTime.UTC()
		view.CreatedAt = view.CreatedAt.UTC()
		views = append(views, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("financial adapter: iterate accounting entries: %w", err)
	}
	return views, nil
}
