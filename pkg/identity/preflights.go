package identity

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// PreflightCandidate is one upgrade preflight record a domain asks the
// control-plane database to keep (C-88). Signature identifies the request
// content: submitting the same request again returns the newest row unchanged
// instead of storing a second record.
type PreflightCandidate struct {
	// Signature identifies the request content; required.
	Signature string
	// Payload is the record body the domain encoded; required.
	Payload []byte
	// ObservedAt is when the preflight observed the cluster; it is the
	// observation time the read path reports (C-13).
	ObservedAt time.Time
}

// PreflightRow is one stored upgrade preflight record (C-88).
type PreflightRow struct {
	// ID is the record identity the API serves.
	ID string
	// Sequence orders the rows; the newest record carries the highest value.
	Sequence int64
	// Signature identifies the request content.
	Signature string
	// Payload is the record body the domain encoded.
	Payload []byte
	// ObservedAt is when the preflight observed the cluster.
	ObservedAt time.Time
	// IsSuperseded marks a record a later preflight replaced; it stays readable.
	IsSuperseded bool
}

// AppendPreflight stores one preflight record and reports whether this call
// created it (C-88). A submission whose signature equals the newest record's is
// the same request and keeps that record: the database is not given a second
// row and the stored result is returned untouched. A different request
// supersedes every older record in the same transaction and becomes the newest
// one, so the read path always has exactly one current record.
func (s *Store) AppendPreflight(ctx context.Context, candidate PreflightCandidate) (PreflightRow, bool, error) {
	if candidate.Signature == "" || len(candidate.Payload) == 0 {
		return PreflightRow{}, false, fmt.Errorf("identity: a preflight needs a signature and a payload")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PreflightRow{}, false, fmt.Errorf("identity: begin the preflight record: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	newest, found, err := newestPreflight(ctx, tx)
	if err != nil {
		return PreflightRow{}, false, err
	}
	if found && newest.Signature == candidate.Signature {
		if err := tx.Commit(); err != nil {
			return PreflightRow{}, false, fmt.Errorf("identity: read the preflight record back: %w", err)
		}
		newest.IsSuperseded = false
		return newest, false, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE upgrade_preflights SET superseded = 1 WHERE superseded = 0`); err != nil {
		return PreflightRow{}, false, fmt.Errorf("identity: supersede the preflight records: %w", err)
	}
	var last sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(sequence) FROM upgrade_preflights`).Scan(&last); err != nil {
		return PreflightRow{}, false, fmt.Errorf("identity: read the preflight sequence: %w", err)
	}
	sequence := int64(1)
	if last.Valid {
		sequence = last.Int64 + 1
	}
	row := PreflightRow{
		ID:         "preflight-" + strconv.FormatInt(sequence, 10),
		Sequence:   sequence,
		Signature:  candidate.Signature,
		Payload:    candidate.Payload,
		ObservedAt: candidate.ObservedAt.UTC(),
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO upgrade_preflights (id, sequence, signature, payload, observed_at, superseded)
		VALUES (?, ?, ?, ?, ?, 0)`,
		row.ID, row.Sequence, row.Signature, string(row.Payload), toMillis(row.ObservedAt)); err != nil {
		return PreflightRow{}, false, fmt.Errorf("identity: store the preflight record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PreflightRow{}, false, fmt.Errorf("identity: commit the preflight record: %w", err)
	}
	return row, true, nil
}

// Preflights lists every stored record, newest first. Superseded records are
// included: C-88 keeps them readable and never deletes them.
func (s *Store) Preflights(ctx context.Context) ([]PreflightRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, sequence, signature, payload, observed_at, superseded
		FROM upgrade_preflights ORDER BY sequence DESC`)
	if err != nil {
		return nil, fmt.Errorf("identity: read the preflight records: %w", err)
	}
	defer rows.Close()
	records := make([]PreflightRow, 0, 8)
	for rows.Next() {
		row, err := scanPreflight(rows)
		if err != nil {
			return nil, err
		}
		records = append(records, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("identity: read the preflight records: %w", err)
	}
	return records, nil
}

// AmendPreflight re-encodes one record's payload inside a transaction and
// reports whether the record exists (C-88). The upgrade write merges each
// node's result into the same record through it, so two nodes never split one
// upgrade across two records.
func (s *Store) AmendPreflight(ctx context.Context, id string, amend func(payload []byte) ([]byte, error)) (PreflightRow, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PreflightRow{}, false, fmt.Errorf("identity: begin the preflight amendment: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	row, found, err := preflight(ctx, tx, id)
	if err != nil {
		return PreflightRow{}, false, err
	}
	if !found {
		if err := tx.Commit(); err != nil {
			return PreflightRow{}, false, fmt.Errorf("identity: read the preflight record: %w", err)
		}
		return PreflightRow{}, false, nil
	}
	updated, err := amend(row.Payload)
	if err != nil {
		return PreflightRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE upgrade_preflights SET payload = ? WHERE id = ?`, string(updated), id); err != nil {
		return PreflightRow{}, false, fmt.Errorf("identity: amend the preflight record: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PreflightRow{}, false, fmt.Errorf("identity: commit the preflight amendment: %w", err)
	}
	row.Payload = updated
	return row, true, nil
}

// newestPreflight reads the current record, the one no later preflight has
// superseded.
func newestPreflight(ctx context.Context, tx *sql.Tx) (PreflightRow, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, sequence, signature, payload, observed_at, superseded
		FROM upgrade_preflights ORDER BY sequence DESC LIMIT 1`)
	record, err := scanPreflight(row)
	if err == sql.ErrNoRows {
		return PreflightRow{}, false, nil
	}
	if err != nil {
		return PreflightRow{}, false, err
	}
	return record, true, nil
}

// preflight reads one record by its identity.
func preflight(ctx context.Context, tx *sql.Tx, id string) (PreflightRow, bool, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, sequence, signature, payload, observed_at, superseded
		FROM upgrade_preflights WHERE id = ?`, id)
	record, err := scanPreflight(row)
	if err == sql.ErrNoRows {
		return PreflightRow{}, false, nil
	}
	if err != nil {
		return PreflightRow{}, false, err
	}
	return record, true, nil
}

// scanner is what scans one preflight row: a *sql.Row and *sql.Rows both
// satisfy it.
type scanner interface {
	Scan(dest ...any) error
}

// scanPreflight reads one stored row.
func scanPreflight(row scanner) (PreflightRow, error) {
	var (
		record     PreflightRow
		payload    string
		observedAt int64
		superseded int
	)
	if err := row.Scan(&record.ID, &record.Sequence, &record.Signature, &payload, &observedAt, &superseded); err != nil {
		if err == sql.ErrNoRows {
			return PreflightRow{}, err
		}
		return PreflightRow{}, fmt.Errorf("identity: scan the preflight record: %w", err)
	}
	record.Payload = []byte(payload)
	record.ObservedAt = fromMillis(observedAt)
	record.IsSuperseded = superseded != 0
	return record, nil
}
