package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/wardstone-project/wardstone/internal/admin"
	"github.com/wardstone-project/wardstone/internal/domain"
)

// GetResearchEvidence uses the evidence primary key and checks the exact attempt,
// not just the case root. No history scan, network I/O, or current catalog lookup
// is needed. Oversized historical payloads never cross the database boundary.
func (s *Store) GetResearchEvidence(ctx context.Context, id domain.InvestigationID, evidenceID domain.EvidenceID) (admin.ResearchEvidence, error) {
	var item domain.Evidence
	err := s.pool.QueryRow(ctx, `SELECT id, investigation_id, source, capability, kind, data, observed_at
		FROM evidence WHERE id = $1 AND investigation_id = $2 AND source = 'research'
		AND octet_length(data::text) <= $3`, evidenceID, id, admin.MaxResearchEvidenceData).Scan(
		&item.ID, &item.InvestigationID, &item.Source, &item.Capability, &item.Kind, &item.Data, &item.ObservedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return admin.ResearchEvidence{}, admin.ErrEvidenceNotFound
	}
	if err != nil {
		return admin.ResearchEvidence{}, err
	}
	return admin.ProjectResearchEvidence(item)
}
