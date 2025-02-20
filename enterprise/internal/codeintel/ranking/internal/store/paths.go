package store

import (
	"context"
	"time"

	"github.com/keegancsmith/sqlf"
	"github.com/lib/pq"
	otlog "github.com/opentracing/opentracing-go/log"

	"github.com/sourcegraph/sourcegraph/internal/database/basestore"
	"github.com/sourcegraph/sourcegraph/internal/database/batch"
	"github.com/sourcegraph/sourcegraph/internal/observation"
)

func (s *store) InsertInitialPathRanks(ctx context.Context, uploadID int, documentPaths chan string, batchSize int, graphKey string) (err error) {
	ctx, _, endObservation := s.operations.insertInitialPathRanks.With(
		ctx,
		&err,
		observation.Args{LogFields: []otlog.Field{
			otlog.String("graphKey", graphKey),
		}},
	)
	defer endObservation(1, observation.Args{})

	tx, err := s.db.Transact(ctx)
	if err != nil {
		return err
	}
	defer func() { err = tx.Done(err) }()

	inserter := func(inserter *batch.Inserter) error {
		for paths := range batchChannel(documentPaths, batchSize) {
			if err := inserter.Insert(ctx, pq.Array(paths)); err != nil {
				return err
			}
		}

		return nil
	}

	if err := tx.Exec(ctx, sqlf.Sprintf(createInitialPathTemporaryTableQuery)); err != nil {
		return err
	}

	if err := batch.WithInserter(
		ctx,
		tx.Handle(),
		"t_codeintel_initial_path_ranks",
		batch.MaxNumPostgresParameters,
		[]string{"document_paths"},
		inserter,
	); err != nil {
		return err
	}

	if err = tx.Exec(ctx, sqlf.Sprintf(insertInitialPathRankCountsQuery, uploadID, graphKey)); err != nil {
		return err
	}

	return nil
}

const createInitialPathTemporaryTableQuery = `
CREATE TEMPORARY TABLE IF NOT EXISTS t_codeintel_initial_path_ranks (
	document_paths text[] NOT NULL
)
ON COMMIT DROP
`

const insertInitialPathRankCountsQuery = `
INSERT INTO codeintel_initial_path_ranks (upload_id, document_paths, graph_key)
SELECT %s, document_paths, %s FROM t_codeintel_initial_path_ranks
`

func (s *store) VacuumAbandonedInitialPathCounts(ctx context.Context, graphKey string, batchSize int) (_ int, err error) {
	ctx, _, endObservation := s.operations.vacuumAbandonedInitialPathCounts.With(ctx, &err, observation.Args{LogFields: []otlog.Field{}})
	defer endObservation(1, observation.Args{})

	count, _, err := basestore.ScanFirstInt(s.db.Query(ctx, sqlf.Sprintf(vacuumAbandonedInitialPathCountsQuery, graphKey, graphKey, batchSize)))
	return count, err
}

const vacuumAbandonedInitialPathCountsQuery = `
WITH
locked_initial_paths AS (
	SELECT id
	FROM codeintel_initial_path_ranks
	WHERE (graph_key < %s OR graph_key > %s)
	ORDER BY graph_key, id
	FOR UPDATE SKIP LOCKED
	LIMIT %s
),
deleted_initial_paths AS (
	DELETE FROM codeintel_initial_path_ranks
	WHERE id IN (SELECT id FROM locked_initial_paths)
	RETURNING 1
)
SELECT COUNT(*) FROM deleted_initial_paths
`

func (s *store) VacuumStaleInitialPaths(ctx context.Context, graphKey string) (
	numPathRecordsScanned int,
	numStalePathRecordsDeleted int,
	err error,
) {
	ctx, _, endObservation := s.operations.vacuumStaleInitialPaths.With(ctx, &err, observation.Args{LogFields: []otlog.Field{}})
	defer endObservation(1, observation.Args{})

	rows, err := s.db.Query(ctx, sqlf.Sprintf(
		vacuumStalePathsQuery,
		graphKey, int(threshold/time.Hour), vacuumBatchSize,
	))
	if err != nil {
		return 0, 0, err
	}
	defer func() { err = basestore.CloseRows(rows, err) }()

	for rows.Next() {
		if err := rows.Scan(
			&numPathRecordsScanned,
			&numStalePathRecordsDeleted,
		); err != nil {
			return 0, 0, err
		}
	}

	return numPathRecordsScanned, numStalePathRecordsDeleted, nil
}

const vacuumStalePathsQuery = `
WITH
locked_initial_path_ranks AS (
	SELECT
		ipr.id,
		u.repository_id,
		ipr.upload_id
	FROM codeintel_initial_path_ranks ipr
	JOIN lsif_uploads u ON u.id = ipr.upload_id
	WHERE
		ipr.graph_key = %s AND
		(ipr.last_scanned_at IS NULL OR NOW() - ipr.last_scanned_at >= %s * '1 hour'::interval)
	ORDER BY ipr.last_scanned_at ASC NULLS FIRST, ipr.id
	FOR UPDATE SKIP LOCKED
	LIMIT %s
),
candidates AS (
	SELECT
		lipr.id,
		uvt.is_default_branch IS TRUE AS safe
	FROM locked_initial_path_ranks lipr
	LEFT JOIN lsif_uploads_visible_at_tip uvt ON uvt.repository_id = lipr.repository_id AND uvt.upload_id = lipr.upload_id
),
updated_initial_path_ranks AS (
	UPDATE codeintel_initial_path_ranks
	SET last_scanned_at = NOW()
	WHERE id IN (SELECT c.id FROM candidates c WHERE c.safe)
),
deleted_initial_path_ranks AS (
	DELETE FROM codeintel_initial_path_ranks
	WHERE id IN (SELECT c.id FROM candidates c WHERE NOT c.safe)
	RETURNING 1
)
SELECT
	(SELECT COUNT(*) FROM candidates),
	(SELECT COUNT(*) FROM deleted_initial_path_ranks)
`
