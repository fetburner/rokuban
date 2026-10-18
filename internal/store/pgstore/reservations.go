package pgstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/fetburner/rokuban/internal/db/sqlcgen"
	"github.com/fetburner/rokuban/internal/reservation"
	"github.com/fetburner/rokuban/internal/store"
)

// Reservations implements user reservation intent and reservation reads in PostgreSQL.
type Reservations struct {
	pool *pgxpool.Pool
}

// NewReservations creates a PostgreSQL Reservations store.
func NewReservations(pool *pgxpool.Pool) *Reservations {
	return &Reservations{pool: pool}
}

// PutIntent snapshots the program and writes its user intent in one transaction.
func (s *Reservations) PutIntent(ctx context.Context, site string, programID int64, action string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning intent write for program %d at site %s: %w", programID, site, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	if err := ensureProgramSnapshot(ctx, tx, q, site, programID); err != nil {
		return err
	}
	if _, err := q.UpsertProgramIntent(ctx, sqlcgen.UpsertProgramIntentParams{
		Site: site, ProgramID: programID, Action: action,
	}); err != nil {
		return fmt.Errorf("upserting intent for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing intent for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	return nil
}

// DeleteIntent removes a user's intent. Missing intents are already in the desired state.
func (s *Reservations) DeleteIntent(ctx context.Context, site string, programID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning intent delete for program %d at site %s: %w", programID, site, err)
	}
	defer rollback(tx)
	if _, err := sqlcgen.New(tx).DeleteProgramIntent(ctx, sqlcgen.DeleteProgramIntentParams{
		Site: site, ProgramID: programID,
	}); err != nil {
		return fmt.Errorf("deleting intent for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing intent delete for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	return nil
}

// PatchOverrides serializes same-program patches before reading existing values, then commits
// the merged result atomically. The program snapshot is the shared serialization row.
func (s *Reservations) PatchOverrides(ctx context.Context, site string, programID int64, patch store.OverridesPatch) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning overrides patch for program %d at site %s: %w", programID, site, err)
	}
	defer rollback(tx)
	q := sqlcgen.New(tx)

	// This must precede the old override read. The upsert locks the snapshot row for existing
	// projection rows, so another patch reads only after the first merged value commits.
	if err := ensureProgramSnapshot(ctx, tx, q, site, programID); err != nil {
		return err
	}

	existing, err := q.GetProgramOverrides(ctx, sqlcgen.GetProgramOverridesParams{Site: site, ProgramID: programID})
	var existingJSON json.RawMessage
	if err == nil {
		existingJSON = existing.Overrides
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("getting overrides for program %d at site %s: %w", programID, site, err)
	}
	merged, err := mergeOverridesPatch(existingJSON, patch)
	if err != nil {
		return fmt.Errorf("merging overrides for program %d at site %s: %w", programID, site, err)
	}

	var baseJSON json.RawMessage
	base, err := q.GetReservationBySiteAndProgramID(ctx, sqlcgen.GetReservationBySiteAndProgramIDParams{
		Site: site, ProgramID: programID,
	})
	if err == nil {
		baseJSON = base.Base
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("getting reservation base for program %d at site %s: %w", programID, site, err)
	}

	mergedJSON, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("marshalling merged overrides for program %d at site %s: %w", programID, site, err)
	}
	effective, err := reservation.EffectiveOptions(baseJSON, mergedJSON, nil)
	if err != nil {
		return fmt.Errorf("computing effective options for program %d at site %s: %w", programID, site, err)
	}
	if err := reservation.ValidateEffectiveKeepOriginal(effective); err != nil {
		return fmt.Errorf("validating effective options for program %d at site %s: %w", programID, site, err)
	}
	if err := persistOverrides(ctx, q, site, programID, mergedJSON); err != nil {
		return fmt.Errorf("persisting overrides for program %d at site %s: %w", programID, site, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing overrides for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	return nil
}

// DeleteOverrides removes all user option overrides for a program.
func (s *Reservations) DeleteOverrides(ctx context.Context, site string, programID int64) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("beginning overrides delete for program %d at site %s: %w", programID, site, err)
	}
	defer rollback(tx)
	if _, err := sqlcgen.New(tx).DeleteProgramOverrides(ctx, sqlcgen.DeleteProgramOverridesParams{
		Site: site, ProgramID: programID,
	}); err != nil {
		return fmt.Errorf("deleting overrides for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("committing overrides delete for program %d at site %s: %w", programID, site, mapDatabaseError(err))
	}
	return nil
}

// GetReservation returns one reservation and its program snapshot and user inputs.
func (s *Reservations) GetReservation(ctx context.Context, site string, programID int64) (store.ReservationDetails, error) {
	row, err := sqlcgen.New(s.pool).GetReservationFullBySiteAndProgramID(ctx, sqlcgen.GetReservationFullBySiteAndProgramIDParams{
		Site: site, ProgramID: programID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return store.ReservationDetails{}, fmt.Errorf("reservation for program %d at site %s: %w", programID, site, store.ErrNotFound)
		}
		return store.ReservationDetails{}, fmt.Errorf("getting reservation for program %d at site %s: %w", programID, site, err)
	}
	return reservationDetails(row.Reservation, row.ProgramSnapshot, row.Overrides, row.IntentAction, row.NeverRecorded, row.Series), nil
}

// ListReservations returns detailed reservations ordered by site and program start time.
func (s *Reservations) ListReservations(ctx context.Context) ([]store.ReservationDetails, error) {
	rows, err := sqlcgen.New(s.pool).ListReservationsFull(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing reservations: %w", err)
	}
	result := make([]store.ReservationDetails, 0, len(rows))
	for _, row := range rows {
		result = append(result, reservationDetails(row.Reservation, row.ProgramSnapshot, row.Overrides, row.IntentAction, row.NeverRecorded, row.Series))
	}
	return result, nil
}

// ListOverlaps returns reservation candidates overlapping the EPG program's broadcast window.
func (s *Reservations) ListOverlaps(ctx context.Context, site string, programID int64) ([]store.ReservationOverlap, error) {
	q := sqlcgen.New(s.pool)
	program, err := q.GetEpgProgram(ctx, sqlcgen.GetEpgProgramParams{Site: site, ProgramID: programID})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("program %d at site %s: %w", programID, site, store.ErrNotFound)
		}
		return nil, fmt.Errorf("getting broadcast window for program %d at site %s: %w", programID, site, err)
	}
	rows, err := q.ListOverlappingReservations(ctx, sqlcgen.ListOverlappingReservationsParams{
		Site: site, TargetProgramID: programID,
		WindowStart: program.EpgProgram.StartAt, WindowEnd: program.EpgProgram.EndAt,
	})
	if err != nil {
		return nil, fmt.Errorf("listing reservations overlapping program %d at site %s: %w", programID, site, err)
	}
	result := make([]store.ReservationOverlap, 0, len(rows))
	for _, row := range rows {
		result = append(result, store.ReservationOverlap{
			ProgramID: row.Reservation.ProgramID, Title: row.ProgramSnapshot.Title,
			StartAt: row.ProgramSnapshot.StartAt, DurationMs: row.ProgramSnapshot.DurationMs,
			Base: row.Reservation.Base, Overrides: row.Overrides, IntentAction: row.IntentAction,
		})
	}
	return result, nil
}

func ensureProgramSnapshot(ctx context.Context, tx pgx.Tx, q *sqlcgen.Queries, site string, programID int64) error {
	source, err := q.GetProgramSnapshotSource(ctx, sqlcgen.GetProgramSnapshotSourceParams{Site: site, ProgramID: programID})
	if err == nil {
		if err := q.UpsertProgramSnapshot(ctx, sqlcgen.UpsertProgramSnapshotParams{
			Site: site, ProgramID: programID, Title: source.Title, StartAt: source.StartAt,
			DurationMs: source.DurationMs, NetworkID: source.NetworkID, ServiceID: source.ServiceID,
			ChannelType: source.ChannelType, Channel: source.Channel, EventID: source.EventID, ServiceName: source.ServiceName,
		}); err != nil {
			return fmt.Errorf("upserting program snapshot for program %d at site %s: %w", programID, site, mapDatabaseError(err))
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("getting program snapshot source for program %d at site %s: %w", programID, site, err)
	}

	// Keep frozen snapshots editable after the EPG projection rolls forward, and serialize
	// those writes too. If GC wins the race, the missing row is reported instead of recreated.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT true FROM program_snapshots WHERE site = $1 AND program_id = $2 FOR UPDATE`, site, programID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("program %d at site %s: %w", programID, site, store.ErrNotFound)
		}
		return fmt.Errorf("locking frozen program snapshot %d at site %s: %w", programID, site, err)
	}
	return nil
}

func reservationDetails(r sqlcgen.Reservation, snapshot sqlcgen.ProgramSnapshot, overrides json.RawMessage, intentAction *string, neverRecorded bool, series *string) store.ReservationDetails {
	var similarity *float32
	if r.DedupSimilarity.Valid {
		value := r.DedupSimilarity.Float32
		similarity = &value
	}
	return store.ReservationDetails{
		Reservation: store.ReservationRecord{
			Site: r.Site, ProgramID: r.ProgramID, RuleID: r.RuleID, Base: r.Base,
			CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
			DedupMatchRecordingID: r.DedupMatchRecordingID, DedupSimilarity: similarity,
		},
		Snapshot: store.ProgramSnapshot{
			Title: snapshot.Title, StartAt: snapshot.StartAt, DurationMs: snapshot.DurationMs,
			ChannelType: snapshot.ChannelType, ServiceName: snapshot.ServiceName,
		},
		Overrides: overrides, IntentAction: intentAction, NeverRecorded: neverRecorded, Series: series,
	}
}

func mergeOverridesPatch(existing json.RawMessage, patch store.OverridesPatch) (reservation.Options, error) {
	var options reservation.Options
	if len(existing) > 0 {
		if err := json.Unmarshal(existing, &options); err != nil {
			return reservation.Options{}, fmt.Errorf("unmarshalling existing overrides: %w", err)
		}
	}
	if patch.Priority != nil {
		options.Priority = patch.Priority
	}
	if patch.ContentPath != nil {
		options.ContentPath = patch.ContentPath
	}
	if patch.FilenameTemplate != nil {
		options.FilenameTemplate = patch.FilenameTemplate
	}
	if patch.KeepOriginal != nil {
		options.KeepOriginal = patch.KeepOriginal
	}
	if patch.EncodeProfiles != nil {
		options.EncodeProfiles = patch.EncodeProfiles
	}
	for _, field := range patch.Reset {
		switch field {
		case store.OverridePriority:
			options.Priority = nil
		case store.OverrideContentPath:
			options.ContentPath = nil
		case store.OverrideFilenameTemplate:
			options.FilenameTemplate = nil
		case store.OverrideKeepOriginal:
			options.KeepOriginal = nil
		case store.OverrideEncodeProfiles:
			options.EncodeProfiles = nil
		default:
			return reservation.Options{}, fmt.Errorf("unknown override field %q", field)
		}
	}
	return options, nil
}

func persistOverrides(ctx context.Context, q *sqlcgen.Queries, site string, programID int64, finalJSON json.RawMessage) error {
	if isEmptyOverridesJSON(finalJSON) {
		if _, err := q.DeleteProgramOverrides(ctx, sqlcgen.DeleteProgramOverridesParams{Site: site, ProgramID: programID}); err != nil {
			return fmt.Errorf("deleting emptied overrides: %w", mapDatabaseError(err))
		}
		return nil
	}
	if _, err := q.UpsertProgramOverrides(ctx, sqlcgen.UpsertProgramOverridesParams{
		Site: site, ProgramID: programID, Overrides: finalJSON,
	}); err != nil {
		return fmt.Errorf("upserting overrides: %w", mapDatabaseError(err))
	}
	return nil
}

func isEmptyOverridesJSON(raw json.RawMessage) bool {
	var values map[string]any
	if json.Unmarshal(raw, &values) != nil {
		return false
	}
	return len(values) == 0
}

var _ store.Reservations = (*Reservations)(nil)
