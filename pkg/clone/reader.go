package clone

import (
	"context"
	"database/sql"
	"github.com/pkg/errors"
	"github.com/platinummonkey/go-concurrency-limits/core"
	"github.com/prometheus/client_golang/prometheus"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	"golang.org/x/sync/semaphore"
	"math/rand"
)

var (
	tablesTotalMetric = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "tables",
			Help: "How many total tables to do.",
		},
	)
	rowCountMetric = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "estimated_rows",
			Help: "How many total tables to do.",
		},
		[]string{"table"},
	)
	tablesDoneMetric = prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "tables_done",
			Help: "How many tables done.",
		},
	)
)

func init() {
	prometheus.MustRegister(tablesTotalMetric)
	prometheus.MustRegister(rowCountMetric)
	prometheus.MustRegister(tablesDoneMetric)
}

type Reader struct {
	config ReaderConfig
	table  *Table
	source *sql.DB
	target *sql.DB

	sourceRetry RetryOptions
	targetRetry RetryOptions
}

func (r *Reader) Diff(ctx context.Context, diffs chan Diff) error {
	return errors.WithStack(r.read(ctx, diffs, true))
}

func (r *Reader) Read(ctx context.Context, diffs chan Diff) error {
	// TODO this can be refactored to a separate method
	return errors.WithStack(r.read(ctx, diffs, false))
}

func (r *Reader) read(ctx context.Context, diffsCh chan Diff, diff bool) error {
	g, ctx := errgroup.WithContext(ctx)

	// Generate chunks of source table
	logger := log.WithContext(ctx).WithField("task", "chunking")
	logger = logger.WithField("table", r.table.Name)
	logger.Infof("'%s' chunking start", r.table.Name)
	chunks, err := generateTableChunks(ctx, r.table, r.source, r.sourceRetry)
	if err != nil {
		return errors.WithStack(err)
	}
	rand.Shuffle(len(chunks), func(i, j int) { chunks[i], chunks[j] = chunks[j], chunks[i] })
	logger.Infof("'%s' chunking done", r.table.Name)

	// Generate diffs from all chunks
	readerParallelism := semaphore.NewWeighted(r.config.ReaderParallelism)
	for _, c := range chunks {
		chunk := c
		err := readerParallelism.Acquire(ctx, 1)
		if err != nil {
			return errors.WithStack(err)
		}
		g.Go(func() (err error) {
			defer readerParallelism.Release(1)
			var diffs []Diff
			if diff {
				diffs, err = r.diffChunk(ctx, chunk)
			} else {
				diffs, err = r.readChunk(ctx, chunk)
			}

			if err != nil {
				if r.config.Consistent {
					return errors.WithStack(err)
				}

				log.WithField("table", chunk.Table.Name).
					WithError(err).
					WithContext(ctx).
					Warnf("failed to read chunk %s[%d - %d] after retries and backoff, "+
						"since this is a best effort clone we just give up: %+v",
						chunk.Table.Name, chunk.Start, chunk.End, err)
				return nil
			}

			if len(diffs) > 0 {
				chunksWithDiffs.WithLabelValues(chunk.Table.Name).Inc()
			}

			chunksProcessed.WithLabelValues(chunk.Table.Name).Inc()

			for _, diff := range diffs {
				select {
				case diffsCh <- diff:
				case <-ctx.Done():
					return ctx.Err()
				}
			}

			return nil
		})
	}

	err = g.Wait()
	if err != nil {
		return errors.WithStack(err)
	}

	return nil
}

func NewReader(
	config ReaderConfig,
	table *Table,
	source *sql.DB,
	sourceLimiter core.Limiter,
	target *sql.DB,
	targetLimiter core.Limiter,
) *Reader {
	return &Reader{
		config: config,
		table:  table,
		source: source,
		sourceRetry: RetryOptions{
			Limiter:       sourceLimiter,
			AcquireMetric: readLimiterDelay.WithLabelValues("source"),
			MaxRetries:    config.ReadRetries,
			Timeout:       config.ReadTimeout,
		},
		target: target,
		targetRetry: RetryOptions{
			Limiter:       targetLimiter,
			AcquireMetric: readLimiterDelay.WithLabelValues("target"),
			MaxRetries:    config.ReadRetries,
			Timeout:       config.ReadTimeout,
		},
	}
}
