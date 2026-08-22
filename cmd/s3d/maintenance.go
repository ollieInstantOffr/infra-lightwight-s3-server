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

	// auditRetention and metricRetention bound how long the console's history
	// is kept. Both tables grow without limit otherwise: one row per action and
	// one per hour respectively.
	auditRetention  = 365 * 24 * time.Hour
	metricRetention = 90 * 24 * time.Hour
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

	// Lifecycle expiry runs before the blob sweep so anything it deletes is
	// reclaimable in the same pass.
	expireByLifecycle(ctx, pool, log)

	reclaimed, err := db.SweepUnreferenced(ctx, pool, blobs, blobGrace, batchLimit, log)
	if err != nil {
		log.Warn("could not sweep unreferenced blobs", "error", err)
	} else if reclaimed > 0 {
		log.Info("reclaimed unreferenced blobs", "count", reclaimed)
	}

	if purged, err := db.PurgeAuditEvents(ctx, pool, auditRetention); err != nil {
		log.Warn("could not purge audit events", "error", err)
	} else if purged > 0 {
		log.Info("purged old audit events", "count", purged)
	}

	if err := db.PurgeMetrics(ctx, pool, metricRetention); err != nil {
		log.Warn("could not purge request metrics", "error", err)
	}
}

// expireByLifecycle deletes objects matching a bucket's expiry rules.
//
// Deletion goes through the ordinary path rather than a bulk SQL delete, so
// versioning, reference counting and blob release all behave exactly as they
// would for a person pressing delete. A rule that quietly bypassed those would
// leak blobs on every sweep.
func expireByLifecycle(ctx context.Context, pool *db.Pool, log *slog.Logger) {
	targets, err := db.BucketsWithLifecycle(ctx, pool)
	if err != nil {
		log.Warn("could not read lifecycle rules", "error", err)
		return
	}

	for _, target := range targets {
		settings, err := db.GetBucketSettings(ctx, pool, target.BucketID)
		if err != nil {
			log.Warn("could not read bucket settings", "bucket", target.BucketName, "error", err)
			continue
		}
		options := db.WriteOptions{Versioning: settings.Versioning, Actor: "lifecycle"}

		for _, rule := range target.Rules {
			if !rule.Enabled || rule.ExpireDays <= 0 {
				continue
			}
			keys, err := db.ExpiredObjectKeys(ctx, pool, target.BucketID, rule.Prefix, rule.ExpireDays, batchLimit)
			if err != nil {
				log.Warn("could not find expired objects",
					"bucket", target.BucketName, "rule", rule.ID, "error", err)
				continue
			}

			expired := 0
			for _, key := range keys {
				if _, err := db.DeleteObject(ctx, pool, target.BucketID, key, options); err != nil {
					log.Warn("could not expire object",
						"bucket", target.BucketName, "key", key, "error", err)
					continue
				}
				expired++
			}
			if expired > 0 {
				log.Info("expired objects by lifecycle rule",
					"bucket", target.BucketName, "rule", rule.ID,
					"prefix", rule.Prefix, "days", rule.ExpireDays, "count", expired)
			}
		}
	}
}
