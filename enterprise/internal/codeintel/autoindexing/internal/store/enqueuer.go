package store

import (
	"context"

	"github.com/keegancsmith/sqlf"
	"github.com/lib/pq"
	"github.com/opentracing/opentracing-go/log"

	uploadsshared "github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/uploads/shared"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/database/basestore"
	"github.com/sourcegraph/sourcegraph/internal/database/dbutil"
	"github.com/sourcegraph/sourcegraph/internal/executor"
	"github.com/sourcegraph/sourcegraph/internal/observation"
)

// IsQueued returns true if there is an index or an upload for the repository and commit.
func (s *store) IsQueued(ctx context.Context, repositoryID int, commit string) (_ bool, err error) {
	ctx, _, endObservation := s.operations.isQueued.With(ctx, &err, observation.Args{LogFields: []log.Field{
		log.Int("repositoryID", repositoryID),
		log.String("commit", commit),
	}})
	defer endObservation(1, observation.Args{})

	isQueued, _, err := basestore.ScanFirstBool(s.db.Query(ctx, sqlf.Sprintf(
		isQueuedQuery,
		repositoryID, commit,
		repositoryID, commit,
	)))
	return isQueued, err
}

const isQueuedQuery = `
-- The query has two parts, 'A' UNION 'B', where 'A' is true if there's a manual and
-- reachable upload for a repo/commit pair. This signifies that the user has configured
-- manual indexing on a repo and we shouldn't clobber it with autoindexing. The other
-- query 'B' is true if there's an auto-index record already enqueued for this repo. This
-- signifies that we've already infered jobs for this repo/commit pair so we can skip it
-- (we should infer the same jobs).

-- We added a way to say "you might infer different jobs" for part 'B' by adding the
-- check on u.should_reindex. We're now adding a way to say "the indexer might result
-- in a different output_ for part A, allowing auto-indexing to clobber records that
-- have undergone some possibly lossy transformation (like LSIF -> SCIP conversion in-db).
SELECT
	EXISTS (
		SELECT 1
		FROM lsif_uploads u
		WHERE
			repository_id = %s AND
			commit = %s AND
			state NOT IN ('deleting', 'deleted') AND
			associated_index_id IS NULL AND
			NOT u.should_reindex
	)

	OR

	-- We want IsQueued to return true when there exists auto-indexing job records
	-- and none of them are marked for reindexing. If we have one or more rows and
	-- ALL of them are not marked for re-indexing, we'll block additional indexing
	-- attempts.
	(
		SELECT COALESCE(bool_and(NOT should_reindex), false)
		FROM (
			-- For each distinct (root, indexer) pair, use the most recently queued
			-- index as the authoritative attempt.
			SELECT DISTINCT ON (root, indexer) should_reindex
			FROM lsif_indexes
			WHERE repository_id = %s AND commit = %s
			ORDER BY root, indexer, queued_at DESC
		) _
	)
`

// IsQueuedRootIndexer returns true if there is an index or an upload for the given (repository, commit, root, indexer).
func (s *store) IsQueuedRootIndexer(ctx context.Context, repositoryID int, commit string, root string, indexer string) (_ bool, err error) {
	ctx, _, endObservation := s.operations.isQueued.With(ctx, &err, observation.Args{LogFields: []log.Field{
		log.Int("repositoryID", repositoryID),
		log.String("commit", commit),
		log.String("root", root),
		log.String("indexer", indexer),
	}})
	defer endObservation(1, observation.Args{})

	isQueued, _, err := basestore.ScanFirstBool(s.db.Query(ctx, sqlf.Sprintf(isQueuedRootIndexerQuery, repositoryID, commit, root, indexer)))
	return isQueued, err
}

const isQueuedRootIndexerQuery = `
SELECT NOT should_reindex
FROM lsif_indexes
WHERE
	repository_id  = %s AND
	commit         = %s AND
	root           = %s AND
	indexer        = %s
ORDER BY queued_at DESC
LIMIT 1
`

// InsertIndexes inserts a new index and returns the hydrated index models.
func (s *store) InsertIndexes(ctx context.Context, indexes []uploadsshared.Index) (_ []uploadsshared.Index, err error) {
	ctx, _, endObservation := s.operations.insertIndexes.With(ctx, &err, observation.Args{})
	defer func() {
		endObservation(1, observation.Args{LogFields: []log.Field{
			log.Int("numIndexes", len(indexes)),
		}})
	}()

	if len(indexes) == 0 {
		return nil, nil
	}

	values := make([]*sqlf.Query, 0, len(indexes))
	for _, index := range indexes {
		if index.DockerSteps == nil {
			index.DockerSteps = []uploadsshared.DockerStep{}
		}
		if index.IndexerArgs == nil {
			index.IndexerArgs = []string{}
		}
		if index.LocalSteps == nil {
			index.LocalSteps = []string{}
		}

		values = append(values, sqlf.Sprintf(
			"(%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)",
			index.State,
			index.Commit,
			index.RepositoryID,
			pq.Array(index.DockerSteps),
			pq.Array(index.LocalSteps),
			index.Root,
			index.Indexer,
			pq.Array(index.IndexerArgs),
			index.Outfile,
			pq.Array(index.ExecutionLogs),
			pq.Array(index.RequestedEnvVars),
		))
	}

	tx, err := s.transact(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = tx.db.Done(err) }()

	ids, err := basestore.ScanInts(tx.db.Query(ctx, sqlf.Sprintf(insertIndexQuery, sqlf.Join(values, ","))))
	if err != nil {
		return nil, err
	}

	s.operations.indexesInserted.Add(float64(len(ids)))

	return tx.getIndexesByIDs(ctx, ids...)
}

const insertIndexQuery = `
INSERT INTO lsif_indexes (
	state,
	commit,
	repository_id,
	docker_steps,
	local_steps,
	root,
	indexer,
	indexer_args,
	outfile,
	execution_logs,
	requested_envvars
) VALUES %s
RETURNING id
`

// getIndexesByIDs returns an index for each of the given identifiers. Not all given ids will necessarily
// have a corresponding element in the returned list.
func (s *store) getIndexesByIDs(ctx context.Context, ids ...int) (_ []uploadsshared.Index, err error) {
	if len(ids) == 0 {
		return nil, nil
	}

	authzConds, err := database.AuthzQueryConds(ctx, database.NewDBWith(s.logger, s.db))
	if err != nil {
		return nil, err
	}

	queries := make([]*sqlf.Query, 0, len(ids))
	for _, id := range ids {
		queries = append(queries, sqlf.Sprintf("%d", id))
	}

	return scanIndexes(s.db.Query(ctx, sqlf.Sprintf(getIndexesByIDsQuery, sqlf.Join(queries, ", "), authzConds)))
}

const indexAssociatedUploadIDQueryFragment = `
(
	SELECT MAX(id) FROM lsif_uploads WHERE associated_index_id = u.id
) AS associated_upload_id
`

const indexRankQueryFragment = `
SELECT
	r.id,
	ROW_NUMBER() OVER (ORDER BY COALESCE(r.process_after, r.queued_at), r.id) as rank
FROM lsif_indexes_with_repository_name r
WHERE r.state = 'queued'
`

const getIndexesByIDsQuery = `
SELECT
	u.id,
	u.commit,
	u.queued_at,
	u.state,
	u.failure_message,
	u.started_at,
	u.finished_at,
	u.process_after,
	u.num_resets,
	u.num_failures,
	u.repository_id,
	repo.name,
	u.docker_steps,
	u.root,
	u.indexer,
	u.indexer_args,
	u.outfile,
	u.execution_logs,
	s.rank,
	u.local_steps,
	` + indexAssociatedUploadIDQueryFragment + `,
	u.should_reindex,
	u.requested_envvars
FROM lsif_indexes u
LEFT JOIN (` + indexRankQueryFragment + `) s
ON u.id = s.id
JOIN repo ON repo.id = u.repository_id
WHERE repo.deleted_at IS NULL AND u.id IN (%s) AND %s
ORDER BY u.id
`

//
//

var scanIndexes = basestore.NewSliceScanner(scanIndex)

func scanIndex(s dbutil.Scanner) (index uploadsshared.Index, err error) {
	var executionLogs []executor.ExecutionLogEntry
	if err := s.Scan(
		&index.ID,
		&index.Commit,
		&index.QueuedAt,
		&index.State,
		&index.FailureMessage,
		&index.StartedAt,
		&index.FinishedAt,
		&index.ProcessAfter,
		&index.NumResets,
		&index.NumFailures,
		&index.RepositoryID,
		&index.RepositoryName,
		pq.Array(&index.DockerSteps),
		&index.Root,
		&index.Indexer,
		pq.Array(&index.IndexerArgs),
		&index.Outfile,
		pq.Array(&executionLogs),
		&index.Rank,
		pq.Array(&index.LocalSteps),
		&index.AssociatedUploadID,
		&index.ShouldReindex,
		pq.Array(&index.RequestedEnvVars),
	); err != nil {
		return index, err
	}

	index.ExecutionLogs = append(index.ExecutionLogs, executionLogs...)

	return index, nil
}
