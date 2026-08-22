package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/db"
	"github.com/ollieInstantOffr/infra-lightwight-s3-server/internal/storage"
)

// Two kinds of garbage accumulate in normal operation, and neither announces
// itself. Overwritten and deleted objects leave blobs nothing references, and
// clients that begin a multipart upload and disappear leave their parts pinned
// forever. Without a sweeper the disk fills with data no object points at, and
// the first symptom is a full volume.

const (
	// maintenanceInterval is how often the sweeper runs. Reclaiming space is
	// not urgent; running rarely keeps the load negligible.
	maintenanceInterval = 15 * time.Minute

	// blobGrace is how long a blob must sit unreferenced before collection. A
	// freshly written blob is momentarily at zero references, between its bytes
	// landing and its object row committing, and sweeping that window would
	// delete a live object's data.
	blobGrace = time.Hour

	// abandonedUploadAge is how long an untouched multipart upload survives.
	// Generous, because a legitimate upload of a very large object over a slow
	// link can genuinely take hours.
	abandonedUploadAge = 24 * time.Hour

	// batchLimit bounds one pass so a large backlog is worked through steadily
	// rather than in a single long transaction storm.
	batchLimit = 500
)

// runMaintenance sweeps until ctx is cancelled.
func runMaintenance(ctx context.Context, pool *db.Pool, blobs *storage.Store, log *slog.Logger) {
	ticker := time.NewTicker(maintenanceInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepOnce(ctx, pool, blobs, log)
		}
	}
}

func sweepOnce(ctx context.Context, pool *db.Pool, blobs *storage.Store, log *slog.Logger) {
	// Uploads first: aborting one releases its part blobs, which the blob sweep
	// can then reclaim in the same pass rather than waiting for the next.
	reaped, err := db.ReapAbandonedUploads(ctx, pool, abandonedUploadAge, batchLimit)
	if err != nil {
		log.Warn("could not reap abandoned uploads", "error", err)
	} else if reaped > 0 {
		log.Info("reaped abandoned multipart uploads", "count", reaped)
	}

	reclaimed, err := db.SweepUnreferenced(ctx, pool, blobs, blobGrace, batchLimit, log)
	if err != nil {
		log.Warn("could not sweep unreferenced blobs", "error", err)
	} else if reclaimed > 0 {
		log.Info("reclaimed unreferenced blobs", "count", reclaimed)
	}
}
