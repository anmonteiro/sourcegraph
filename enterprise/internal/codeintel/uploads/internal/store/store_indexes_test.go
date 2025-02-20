package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/google/go-cmp/cmp"
	"github.com/keegancsmith/sqlf"
	"github.com/sourcegraph/log/logtest"

	"github.com/sourcegraph/sourcegraph/cmd/frontend/globals"
	"github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/uploads/shared"
	uploadsshared "github.com/sourcegraph/sourcegraph/enterprise/internal/codeintel/uploads/shared"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/database/dbtest"
	"github.com/sourcegraph/sourcegraph/internal/executor"
	"github.com/sourcegraph/sourcegraph/internal/observation"
	"github.com/sourcegraph/sourcegraph/schema"
)

func TestDeleteIndexesWithoutRepository(t *testing.T) {
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	var indexes []uploadsshared.Index
	for i := 0; i < 25; i++ {
		for j := 0; j < 10+i; j++ {
			indexes = append(indexes, uploadsshared.Index{ID: len(indexes) + 1, RepositoryID: 50 + i})
		}
	}
	insertIndexes(t, db, indexes...)

	t1 := time.Unix(1587396557, 0).UTC()
	t2 := t1.Add(-DeletedRepositoryGracePeriod + time.Minute)
	t3 := t1.Add(-DeletedRepositoryGracePeriod - time.Minute)

	deletions := map[int]time.Time{
		52: t2, 54: t2, 56: t2, // deleted too recently
		61: t3, 63: t3, 65: t3, // deleted
	}

	for repositoryID, deletedAt := range deletions {
		query := sqlf.Sprintf(`UPDATE repo SET deleted_at=%s WHERE id=%s`, deletedAt, repositoryID)

		if _, err := db.QueryContext(context.Background(), query.Query(sqlf.PostgresBindVar), query.Args()...); err != nil {
			t.Fatalf("Failed to update repository: %s", err)
		}
	}

	_, count, err := store.DeleteIndexesWithoutRepository(context.Background(), t1)
	if err != nil {
		t.Fatalf("unexpected error deleting indexes: %s", err)
	}
	if expected := 21 + 23 + 25; count != expected {
		t.Fatalf("unexpected count. want=%d have=%d", expected, count)
	}
}

func TestRecentIndexesSummary(t *testing.T) {
	ctx := context.Background()
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	t0 := time.Unix(1587396557, 0).UTC()
	t1 := t0.Add(-time.Minute * 1)
	t2 := t0.Add(-time.Minute * 2)
	t3 := t0.Add(-time.Minute * 3)
	t4 := t0.Add(-time.Minute * 4)
	t5 := t0.Add(-time.Minute * 5)
	t6 := t0.Add(-time.Minute * 6)
	t7 := t0.Add(-time.Minute * 7)
	t8 := t0.Add(-time.Minute * 8)
	t9 := t0.Add(-time.Minute * 9)

	r1 := 1
	r2 := 2

	addDefaults := func(index uploadsshared.Index) uploadsshared.Index {
		index.Commit = makeCommit(index.ID)
		index.RepositoryID = 50
		index.RepositoryName = "n-50"
		index.DockerSteps = []uploadsshared.DockerStep{}
		index.IndexerArgs = []string{}
		index.LocalSteps = []string{}
		return index
	}

	indexes := []uploadsshared.Index{
		addDefaults(uploadsshared.Index{ID: 150, QueuedAt: t0, Root: "r1", Indexer: "i1", State: "queued", Rank: &r2}), // visible (group 1)
		addDefaults(uploadsshared.Index{ID: 151, QueuedAt: t1, Root: "r1", Indexer: "i1", State: "queued", Rank: &r1}), // visible (group 1)
		addDefaults(uploadsshared.Index{ID: 152, FinishedAt: &t2, Root: "r1", Indexer: "i1", State: "errored"}),        // visible (group 1)
		addDefaults(uploadsshared.Index{ID: 153, FinishedAt: &t3, Root: "r1", Indexer: "i2", State: "completed"}),      // visible (group 2)
		addDefaults(uploadsshared.Index{ID: 154, FinishedAt: &t4, Root: "r2", Indexer: "i1", State: "completed"}),      // visible (group 3)
		addDefaults(uploadsshared.Index{ID: 155, FinishedAt: &t5, Root: "r2", Indexer: "i1", State: "errored"}),        // shadowed
		addDefaults(uploadsshared.Index{ID: 156, FinishedAt: &t6, Root: "r2", Indexer: "i2", State: "completed"}),      // visible (group 4)
		addDefaults(uploadsshared.Index{ID: 157, FinishedAt: &t7, Root: "r2", Indexer: "i2", State: "errored"}),        // shadowed
		addDefaults(uploadsshared.Index{ID: 158, FinishedAt: &t8, Root: "r2", Indexer: "i2", State: "errored"}),        // shadowed
		addDefaults(uploadsshared.Index{ID: 159, FinishedAt: &t9, Root: "r2", Indexer: "i2", State: "errored"}),        // shadowed
	}
	insertIndexes(t, db, indexes...)

	summary, err := store.GetRecentIndexesSummary(ctx, 50)
	if err != nil {
		t.Fatalf("unexpected error querying recent index summary: %s", err)
	}

	expected := []uploadsshared.IndexesWithRepositoryNamespace{
		{Root: "r1", Indexer: "i1", Indexes: []uploadsshared.Index{indexes[0], indexes[1], indexes[2]}},
		{Root: "r1", Indexer: "i2", Indexes: []uploadsshared.Index{indexes[3]}},
		{Root: "r2", Indexer: "i1", Indexes: []uploadsshared.Index{indexes[4]}},
		{Root: "r2", Indexer: "i2", Indexes: []uploadsshared.Index{indexes[6]}},
	}
	if diff := cmp.Diff(expected, summary); diff != "" {
		t.Errorf("unexpected index summary (-want +got):\n%s", diff)
	}
}

func TestGetIndexes(t *testing.T) {
	ctx := context.Background()
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	t1 := time.Unix(1587396557, 0).UTC()
	t2 := t1.Add(-time.Minute * 1)
	t3 := t1.Add(-time.Minute * 2)
	t4 := t1.Add(-time.Minute * 3)
	t5 := t1.Add(-time.Minute * 4)
	t6 := t1.Add(-time.Minute * 5)
	t7 := t1.Add(-time.Minute * 6)
	t8 := t1.Add(-time.Minute * 7)
	t9 := t1.Add(-time.Minute * 8)
	t10 := t1.Add(-time.Minute * 9)
	failureMessage := "unlucky 333"

	indexID1, indexID2, indexID3, indexID4 := 1, 3, 5, 5 // note the duplication
	uploadID1, uploadID2, uploadID3, uploadID4 := 10, 11, 12, 13

	insertIndexes(t, db,
		uploadsshared.Index{ID: 1, Commit: makeCommit(3331), QueuedAt: t1, State: "queued", AssociatedUploadID: &uploadID1},
		uploadsshared.Index{ID: 2, QueuedAt: t2, State: "errored", FailureMessage: &failureMessage},
		uploadsshared.Index{ID: 3, Commit: makeCommit(3333), QueuedAt: t3, State: "queued", AssociatedUploadID: &uploadID1},
		uploadsshared.Index{ID: 4, QueuedAt: t4, State: "queued", RepositoryID: 51, RepositoryName: "foo bar x"},
		uploadsshared.Index{ID: 5, Commit: makeCommit(3333), QueuedAt: t5, State: "processing", AssociatedUploadID: &uploadID1},
		uploadsshared.Index{ID: 6, QueuedAt: t6, State: "processing", RepositoryID: 52, RepositoryName: "foo bar y"},
		uploadsshared.Index{ID: 7, QueuedAt: t7, Indexer: "lsif-typescript"},
		uploadsshared.Index{ID: 8, QueuedAt: t8, Indexer: "scip-ocaml"},
		uploadsshared.Index{ID: 9, QueuedAt: t9, State: "queued"},
		uploadsshared.Index{ID: 10, QueuedAt: t10},
	)
	insertUploads(t, db,
		shared.Upload{ID: uploadID1, AssociatedIndexID: &indexID1},
		shared.Upload{ID: uploadID2, AssociatedIndexID: &indexID2},
		shared.Upload{ID: uploadID3, AssociatedIndexID: &indexID3},
		shared.Upload{ID: uploadID4, AssociatedIndexID: &indexID4},
	)

	testCases := []struct {
		repositoryID  int
		state         string
		states        []string
		term          string
		indexerNames  []string
		withoutUpload bool
		expectedIDs   []int
	}{
		{expectedIDs: []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		{repositoryID: 50, expectedIDs: []int{1, 2, 3, 5, 7, 8, 9, 10}},
		{state: "completed", expectedIDs: []int{7, 8, 10}},
		{term: "003", expectedIDs: []int{1, 3, 5}},                                 // searches commits
		{term: "333", expectedIDs: []int{1, 2, 3, 5}},                              // searches commits and failure message
		{term: "QuEuEd", expectedIDs: []int{1, 3, 4, 9}},                           // searches text status
		{term: "bAr", expectedIDs: []int{4, 6}},                                    // search repo names
		{state: "failed", expectedIDs: []int{2}},                                   // treats errored/failed states equivalently
		{states: []string{"completed", "failed"}, expectedIDs: []int{2, 7, 8, 10}}, // searches multiple states
		{withoutUpload: true, expectedIDs: []int{2, 4, 6, 7, 8, 9, 10}},            // anti-join with upload records
		{indexerNames: []string{"typescript", "ocaml"}, expectedIDs: []int{7, 8}},  // searches indexer name (only)
	}

	for _, testCase := range testCases {
		for lo := 0; lo < len(testCase.expectedIDs); lo++ {
			hi := lo + 3
			if hi > len(testCase.expectedIDs) {
				hi = len(testCase.expectedIDs)
			}

			name := fmt.Sprintf(
				"repositoryID=%d state=%s states=%s term=%s without_upload=%v indexer_names=%v offset=%d",
				testCase.repositoryID,
				testCase.state,
				strings.Join(testCase.states, ","),
				testCase.term,
				testCase.withoutUpload,
				testCase.indexerNames,
				lo,
			)

			t.Run(name, func(t *testing.T) {
				indexes, totalCount, err := store.GetIndexes(ctx, shared.GetIndexesOptions{
					RepositoryID:  testCase.repositoryID,
					State:         testCase.state,
					States:        testCase.states,
					Term:          testCase.term,
					IndexerNames:  testCase.indexerNames,
					WithoutUpload: testCase.withoutUpload,
					Limit:         3,
					Offset:        lo,
				})
				if err != nil {
					t.Fatalf("unexpected error getting indexes for repo: %s", err)
				}
				if totalCount != len(testCase.expectedIDs) {
					t.Errorf("unexpected total count. want=%d have=%d", len(testCase.expectedIDs), totalCount)
				}

				var ids []int
				for _, index := range indexes {
					ids = append(ids, index.ID)
				}

				if diff := cmp.Diff(testCase.expectedIDs[lo:hi], ids); diff != "" {
					t.Errorf("unexpected index ids at offset %d (-want +got):\n%s", lo, diff)
				}
			})
		}
	}

	t.Run("enforce repository permissions", func(t *testing.T) {
		// Enable permissions user mapping forces checking repository permissions
		// against permissions tables in the database, which should effectively block
		// all access because permissions tables are empty.
		before := globals.PermissionsUserMapping()
		globals.SetPermissionsUserMapping(&schema.PermissionsUserMapping{Enabled: true})
		defer globals.SetPermissionsUserMapping(before)

		indexes, totalCount, err := store.GetIndexes(ctx,
			shared.GetIndexesOptions{
				Limit: 1,
			},
		)
		if err != nil {
			t.Fatal(err)
		}
		if len(indexes) > 0 || totalCount > 0 {
			t.Fatalf("Want no index but got %d indexes with totalCount %d", len(indexes), totalCount)
		}
	})
}

func TestGetIndexByID(t *testing.T) {
	ctx := context.Background()
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	// Index does not exist initially
	if _, exists, err := store.GetIndexByID(ctx, 1); err != nil {
		t.Fatalf("unexpected error getting index: %s", err)
	} else if exists {
		t.Fatal("unexpected record")
	}

	uploadID := 5
	queuedAt := time.Unix(1587396557, 0).UTC()
	startedAt := queuedAt.Add(time.Minute)
	expected := uploadsshared.Index{
		ID:             1,
		Commit:         makeCommit(1),
		QueuedAt:       queuedAt,
		State:          "processing",
		FailureMessage: nil,
		StartedAt:      &startedAt,
		FinishedAt:     nil,
		RepositoryID:   123,
		RepositoryName: "n-123",
		DockerSteps: []uploadsshared.DockerStep{
			{
				Image:    "cimg/node:12.16",
				Commands: []string{"yarn install --frozen-lockfile --no-progress"},
			},
		},
		LocalSteps:  []string{"echo hello"},
		Root:        "/foo/bar",
		Indexer:     "sourcegraph/scip-typescript:latest",
		IndexerArgs: []string{"index", "--yarn-workspaces"},
		Outfile:     "dump.lsif",
		ExecutionLogs: []executor.ExecutionLogEntry{
			{Command: []string{"op", "1"}, Out: "Indexing\nUploading\nDone with 1.\n"},
			{Command: []string{"op", "2"}, Out: "Indexing\nUploading\nDone with 2.\n"},
		},
		Rank:               nil,
		AssociatedUploadID: &uploadID,
	}

	insertIndexes(t, db, expected)
	insertUploads(t, db, shared.Upload{ID: uploadID, AssociatedIndexID: &expected.ID})

	if index, exists, err := store.GetIndexByID(ctx, 1); err != nil {
		t.Fatalf("unexpected error getting index: %s", err)
	} else if !exists {
		t.Fatal("expected record to exist")
	} else if diff := cmp.Diff(expected, index); diff != "" {
		t.Errorf("unexpected index (-want +got):\n%s", diff)
	}

	t.Run("enforce repository permissions", func(t *testing.T) {
		// Enable permissions user mapping forces checking repository permissions
		// against permissions tables in the database, which should effectively block
		// all access because permissions tables are empty.
		before := globals.PermissionsUserMapping()
		globals.SetPermissionsUserMapping(&schema.PermissionsUserMapping{Enabled: true})
		defer globals.SetPermissionsUserMapping(before)

		_, exists, err := store.GetIndexByID(ctx, 1)
		if err != nil {
			t.Fatal(err)
		}
		if exists {
			t.Fatalf("exists: want false but got %v", exists)
		}
	})
}

func TestGetIndexesByIDs(t *testing.T) {
	ctx := context.Background()
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	indexID1, indexID2, indexID3, indexID4 := 1, 3, 5, 5 // note the duplication
	uploadID1, uploadID2, uploadID3, uploadID4 := 10, 11, 12, 13

	insertIndexes(t, db,
		uploadsshared.Index{ID: 1, AssociatedUploadID: &uploadID1},
		uploadsshared.Index{ID: 2},
		uploadsshared.Index{ID: 3, AssociatedUploadID: &uploadID1},
		uploadsshared.Index{ID: 4},
		uploadsshared.Index{ID: 5, AssociatedUploadID: &uploadID1},
		uploadsshared.Index{ID: 6},
		uploadsshared.Index{ID: 7},
		uploadsshared.Index{ID: 8},
		uploadsshared.Index{ID: 9},
		uploadsshared.Index{ID: 10},
	)
	insertUploads(t, db,
		shared.Upload{ID: uploadID1, AssociatedIndexID: &indexID1},
		shared.Upload{ID: uploadID2, AssociatedIndexID: &indexID2},
		shared.Upload{ID: uploadID3, AssociatedIndexID: &indexID3},
		shared.Upload{ID: uploadID4, AssociatedIndexID: &indexID4},
	)

	t.Run("fetch", func(t *testing.T) {
		indexes, err := store.GetIndexesByIDs(ctx, 2, 4, 6, 8, 12)
		if err != nil {
			t.Fatalf("unexpected error getting indexes for repo: %s", err)
		}

		var ids []int
		for _, index := range indexes {
			ids = append(ids, index.ID)
		}
		sort.Ints(ids)

		if diff := cmp.Diff([]int{2, 4, 6, 8}, ids); diff != "" {
			t.Errorf("unexpected index ids (-want +got):\n%s", diff)
		}
	})

	t.Run("enforce repository permissions", func(t *testing.T) {
		// Enable permissions user mapping forces checking repository permissions
		// against permissions tables in the database, which should effectively block
		// all access because permissions tables are empty.
		before := globals.PermissionsUserMapping()
		globals.SetPermissionsUserMapping(&schema.PermissionsUserMapping{Enabled: true})
		defer globals.SetPermissionsUserMapping(before)

		indexes, err := store.GetIndexesByIDs(ctx, 1, 2, 3, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(indexes) > 0 {
			t.Fatalf("Want no index but got %d indexes", len(indexes))
		}
	})
}

func TestGetQueuedIndexRank(t *testing.T) {
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	t1 := time.Unix(1587396557, 0).UTC()
	t2 := t1.Add(+time.Minute * 6)
	t3 := t1.Add(+time.Minute * 3)
	t4 := t1.Add(+time.Minute * 1)
	t5 := t1.Add(+time.Minute * 4)
	t6 := t1.Add(+time.Minute * 2)
	t7 := t1.Add(+time.Minute * 5)

	insertIndexes(t, db,
		uploadsshared.Index{ID: 1, QueuedAt: t1, State: "queued"},
		uploadsshared.Index{ID: 2, QueuedAt: t2, State: "queued"},
		uploadsshared.Index{ID: 3, QueuedAt: t3, State: "queued"},
		uploadsshared.Index{ID: 4, QueuedAt: t4, State: "queued"},
		uploadsshared.Index{ID: 5, QueuedAt: t5, State: "queued"},
		uploadsshared.Index{ID: 6, QueuedAt: t6, State: "processing"},
		uploadsshared.Index{ID: 7, QueuedAt: t1, State: "queued", ProcessAfter: &t7},
	)

	if index, _, _ := store.GetIndexByID(context.Background(), 1); index.Rank == nil || *index.Rank != 1 {
		t.Errorf("unexpected rank. want=%d have=%s", 1, printableRank{index.Rank})
	}
	if index, _, _ := store.GetIndexByID(context.Background(), 2); index.Rank == nil || *index.Rank != 6 {
		t.Errorf("unexpected rank. want=%d have=%s", 5, printableRank{index.Rank})
	}
	if index, _, _ := store.GetIndexByID(context.Background(), 3); index.Rank == nil || *index.Rank != 3 {
		t.Errorf("unexpected rank. want=%d have=%s", 3, printableRank{index.Rank})
	}
	if index, _, _ := store.GetIndexByID(context.Background(), 4); index.Rank == nil || *index.Rank != 2 {
		t.Errorf("unexpected rank. want=%d have=%s", 2, printableRank{index.Rank})
	}
	if index, _, _ := store.GetIndexByID(context.Background(), 5); index.Rank == nil || *index.Rank != 4 {
		t.Errorf("unexpected rank. want=%d have=%s", 4, printableRank{index.Rank})
	}

	// Only considers queued indexes to determine rank
	if index, _, _ := store.GetIndexByID(context.Background(), 6); index.Rank != nil {
		t.Errorf("unexpected rank. want=%s have=%s", "nil", printableRank{index.Rank})
	}

	// Process after takes priority over upload time
	if upload, _, _ := store.GetIndexByID(context.Background(), 7); upload.Rank == nil || *upload.Rank != 5 {
		t.Errorf("unexpected rank. want=%d have=%s", 4, printableRank{upload.Rank})
	}
}

func TestDeleteIndexByID(t *testing.T) {
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	insertIndexes(t, db, uploadsshared.Index{ID: 1})

	if found, err := store.DeleteIndexByID(context.Background(), 1); err != nil {
		t.Fatalf("unexpected error deleting index: %s", err)
	} else if !found {
		t.Fatalf("expected record to exist")
	}

	// Index no longer exists
	if _, exists, err := store.GetIndexByID(context.Background(), 1); err != nil {
		t.Fatalf("unexpected error getting index: %s", err)
	} else if exists {
		t.Fatal("unexpected record")
	}
}

func TestDeleteIndexes(t *testing.T) {
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	insertIndexes(t, db, uploadsshared.Index{ID: 1, State: "completed"})
	insertIndexes(t, db, uploadsshared.Index{ID: 2, State: "errored"})

	if err := store.DeleteIndexes(context.Background(), shared.DeleteIndexesOptions{
		States:       []string{"errored"},
		Term:         "",
		RepositoryID: 0,
	}); err != nil {
		t.Fatalf("unexpected error deleting indexes: %s", err)
	}

	// Index no longer exists
	if _, exists, err := store.GetIndexByID(context.Background(), 2); err != nil {
		t.Fatalf("unexpected error getting index: %s", err)
	} else if exists {
		t.Fatal("unexpected record")
	}
}

func TestDeleteIndexesWithIndexerKey(t *testing.T) {
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	insertIndexes(t, db, uploadsshared.Index{ID: 1, Indexer: "sourcegraph/scip-go@sha256:123456"})
	insertIndexes(t, db, uploadsshared.Index{ID: 2, Indexer: "sourcegraph/scip-go"})
	insertIndexes(t, db, uploadsshared.Index{ID: 3, Indexer: "sourcegraph/scip-typescript"})
	insertIndexes(t, db, uploadsshared.Index{ID: 4, Indexer: "sourcegraph/scip-typescript"})

	if err := store.DeleteIndexes(context.Background(), shared.DeleteIndexesOptions{
		IndexerNames: []string{"scip-go"},
	}); err != nil {
		t.Fatalf("unexpected error deleting indexes: %s", err)
	}

	// Target indexes no longer exist
	for _, id := range []int{1, 2} {
		if _, exists, err := store.GetIndexByID(context.Background(), id); err != nil {
			t.Fatalf("unexpected error getting index: %s", err)
		} else if exists {
			t.Fatal("unexpected record")
		}
	}

	// Unmatched indexes remain
	for _, id := range []int{3, 4} {
		if _, exists, err := store.GetIndexByID(context.Background(), id); err != nil {
			t.Fatalf("unexpected error getting index: %s", err)
		} else if !exists {
			t.Fatal("expected record, got none")
		}
	}
}

func TestDeleteIndexByIDMissingRow(t *testing.T) {
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	if found, err := store.DeleteIndexByID(context.Background(), 1); err != nil {
		t.Fatalf("unexpected error deleting index: %s", err)
	} else if found {
		t.Fatalf("unexpected record")
	}
}

func TestReindexIndexes(t *testing.T) {
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	insertIndexes(t, db, uploadsshared.Index{ID: 1, State: "completed"})
	insertIndexes(t, db, uploadsshared.Index{ID: 2, State: "errored"})

	if err := store.ReindexIndexes(context.Background(), shared.ReindexIndexesOptions{
		States:       []string{"errored"},
		Term:         "",
		RepositoryID: 0,
	}); err != nil {
		t.Fatalf("unexpected error deleting indexes: %s", err)
	}

	// Index has been marked for reindexing
	if index, exists, err := store.GetIndexByID(context.Background(), 2); err != nil {
		t.Fatalf("unexpected error getting index: %s", err)
	} else if !exists {
		t.Fatal("index missing")
	} else if !index.ShouldReindex {
		t.Fatal("index not marked for reindexing")
	}
}

func TestReindexIndexByID(t *testing.T) {
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	insertIndexes(t, db, uploadsshared.Index{ID: 1, State: "completed"})
	insertIndexes(t, db, uploadsshared.Index{ID: 2, State: "errored"})

	if err := store.ReindexIndexByID(context.Background(), 2); err != nil {
		t.Fatalf("unexpected error deleting indexes: %s", err)
	}

	// Index has been marked for reindexing
	if index, exists, err := store.GetIndexByID(context.Background(), 2); err != nil {
		t.Fatalf("unexpected error getting index: %s", err)
	} else if !exists {
		t.Fatal("index missing")
	} else if !index.ShouldReindex {
		t.Fatal("index not marked for reindexing")
	}
}

func TestReindexIndexesWithIndexerKey(t *testing.T) {
	logger := logtest.Scoped(t)
	db := database.NewDB(logger, dbtest.NewDB(logger, t))
	store := New(&observation.TestContext, db)

	insertIndexes(t, db, uploadsshared.Index{ID: 1, Indexer: "sourcegraph/scip-go@sha256:123456"})
	insertIndexes(t, db, uploadsshared.Index{ID: 2, Indexer: "sourcegraph/scip-go"})
	insertIndexes(t, db, uploadsshared.Index{ID: 3, Indexer: "sourcegraph/scip-typescript"})
	insertIndexes(t, db, uploadsshared.Index{ID: 4, Indexer: "sourcegraph/scip-typescript"})

	if err := store.ReindexIndexes(context.Background(), shared.ReindexIndexesOptions{
		IndexerNames: []string{"scip-go"},
		Term:         "",
		RepositoryID: 0,
	}); err != nil {
		t.Fatalf("unexpected error deleting indexes: %s", err)
	}

	// Expected indexes marked for re-indexing
	for id, expected := range map[int]bool{
		1: true, 2: true,
		3: false, 4: false,
	} {
		if index, exists, err := store.GetIndexByID(context.Background(), id); err != nil {
			t.Fatalf("unexpected error getting index: %s", err)
		} else if !exists {
			t.Fatal("index missing")
		} else if index.ShouldReindex != expected {
			t.Fatalf("unexpected mark. want=%v have=%v", expected, index.ShouldReindex)
		}
	}
}

// insertIndexes populates the lsif_indexes table with the given index models.
func insertIndexes(t testing.TB, db database.DB, indexes ...uploadsshared.Index) {
	for _, index := range indexes {
		if index.Commit == "" {
			index.Commit = makeCommit(index.ID)
		}
		if index.State == "" {
			index.State = "completed"
		}
		if index.RepositoryID == 0 {
			index.RepositoryID = 50
		}
		if index.DockerSteps == nil {
			index.DockerSteps = []uploadsshared.DockerStep{}
		}
		if index.IndexerArgs == nil {
			index.IndexerArgs = []string{}
		}
		if index.LocalSteps == nil {
			index.LocalSteps = []string{}
		}

		// Ensure we have a repo for the inner join in select queries
		insertRepo(t, db, index.RepositoryID, index.RepositoryName, true)

		query := sqlf.Sprintf(`
			INSERT INTO lsif_indexes (
				id,
				commit,
				queued_at,
				state,
				failure_message,
				started_at,
				finished_at,
				process_after,
				num_resets,
				num_failures,
				repository_id,
				docker_steps,
				root,
				indexer,
				indexer_args,
				outfile,
				execution_logs,
				local_steps,
				should_reindex
			) VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s, %s)
		`,
			index.ID,
			index.Commit,
			index.QueuedAt,
			index.State,
			index.FailureMessage,
			index.StartedAt,
			index.FinishedAt,
			index.ProcessAfter,
			index.NumResets,
			index.NumFailures,
			index.RepositoryID,
			pq.Array(index.DockerSteps),
			index.Root,
			index.Indexer,
			pq.Array(index.IndexerArgs),
			index.Outfile,
			pq.Array(index.ExecutionLogs),
			pq.Array(index.LocalSteps),
			index.ShouldReindex,
		)

		if _, err := db.ExecContext(context.Background(), query.Query(sqlf.PostgresBindVar), query.Args()...); err != nil {
			t.Fatalf("unexpected error while inserting index: %s", err)
		}
	}
}
