package scylla_cdc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gocql/gocql"
	"golang.org/x/sync/errgroup"
)

type ReaderConfig struct {
	// An active gocql session to the cluster.
	Session *gocql.Session

	// Consistency to use when querying CDC log
	Consistency gocql.Consistency

	// The name of the cdc log table name. Must have _scylla_cdc_log suffix.
	// Can be prefixed with keyspace name.
	LogTableName string

	// Determines by how much to offset time lower bound of the query when reading from CDC log.
	// For more information about this option, see README.
	LowerBoundReadOffset time.Duration

	// When polling from CDC log, only rows with timestamps lower than Now - UpperBoundReadOffset are requested.
	// For more information about this option, see README.
	UpperBoundReadOffset time.Duration

	// An object which tracks cluster metadata such as current tokens and node count.
	// While this object is optional, it is highly recommended that this field points to a valid ClusterStateTracker.
	// For usage, refer to the example application.
	ClusterStateTracker *ClusterStateTracker

	// A callback which processes information fetched from the CDC log.
	ChangeConsumer ChangeConsumer
}

func (rc *ReaderConfig) Copy() *ReaderConfig {
	newRC := &ReaderConfig{}
	*newRC = *rc
	return newRC
}

var (
	ErrNotALogTable = errors.New("the table is not a CDC log table")
)

const (
	cdcTableSuffix string = "_scylla_cdc_log"
)

type Reader struct {
	config     *ReaderConfig
	genFetcher *generationFetcher
	stoppedCh  chan struct{}
}

// Creates a new CDC reader.
func NewReader(config *ReaderConfig) (*Reader, error) {
	if !strings.HasSuffix(config.LogTableName, cdcTableSuffix) {
		return nil, ErrNotALogTable
	}

	genFetcher, err := newGenerationFetcher(
		config.Session,
		config.ClusterStateTracker,
		time.Now().Add(-24*time.Hour), // TODO: We should start from a provided timestamp
	)
	if err != nil {
		return nil, err
	}

	reader := &Reader{
		config:     config.Copy(),
		genFetcher: genFetcher,
		stoppedCh:  make(chan struct{}),
	}
	return reader, nil
}

// Run runs the CDC reader. This call is blocking and returns after an error occurs, or the reader
// is stopped gracefully.
func (r *Reader) Run(ctx context.Context) error {
	// TODO: Return a "snapshot" or something

	runErrG, runCtx := errgroup.WithContext(ctx)

	runErrG.Go(func() error {
		select {
		case <-runCtx.Done():
			return runCtx.Err()
		case <-r.stoppedCh:
		}
		r.genFetcher.stop()
		fmt.Println("Shutdown triggered")
		return nil
	})
	runErrG.Go(func() error {
		defer fmt.Println("Fetcher stopped")
		return r.genFetcher.run(runCtx)
	})
	runErrG.Go(func() error {
		gen, err := r.genFetcher.get(runCtx)
		if gen == nil {
			return err
		}

		for {
			// Start batch readers for this generation
			split, err := r.splitStreams(gen.streams)
			if err != nil {
				return err
			}

			genErrG, genCtx := errgroup.WithContext(runCtx)

			fmt.Printf("Opening generation %s\n", gen.startTime)

			readers := make([]*streamBatchReader, 0, len(split))
			for _, group := range split {
				readers = append(readers, newStreamBatchReader(
					r.config.Session,
					group,
					r.config.LogTableName,
					r.config.ChangeConsumer,
					gocql.MinTimeUUID(gen.startTime), // TODO: Change to a configured timestamp
				))
			}

			for i := range readers {
				reader := readers[i]
				// TODO: slightly sleep before creating each of them
				// in order to make them more distributed in time
				genErrG.Go(func() error {
					// TODO: Do something sensible with returned timeuuid
					_, err := reader.run(genCtx)
					// fmt.Println("Reader stopped")
					return err
				})
			}

			fmt.Println("Running all readers complete")

			var nextGen *generation
			genErrG.Go(func() error {
				defer fmt.Println("Stopping generation waiter")
				var err error
				nextGen, err = r.genFetcher.get(genCtx)
				if err != nil {
					return err
				}
				for _, reader := range readers {
					if nextGen == nil {
						reader.stopNow()
					} else {
						reader.close(gocql.MaxTimeUUID(nextGen.startTime))
					}
				}
				return nil
			})

			if err := genErrG.Wait(); err != nil {
				return err
			}
			if nextGen == nil {
				break
			}
			gen = nextGen
		}

		return nil
	})

	return runErrG.Wait()
}

// Stop gracefully stops the CDC reader. It does not wait until the reader shuts down.
func (r *Reader) Stop() {
	close(r.stoppedCh)
}

func (r *Reader) splitStreams(streams []stream) (split [][]stream, err error) {
	if r.config.ClusterStateTracker == nil {
		// This method of polling is quite bad for performance, but we cannot do better without the tracker
		// TODO: Maybe forbid, or at least warn?
		for _, s := range streams {
			split = append(split, []stream{s})
		}
		return
	}

	tokens := r.config.ClusterStateTracker.GetTokens()
	vnodesToStreams := make(map[int64][]stream, 0)

	for _, stream := range streams {
		var streamInt int64
		if err := binary.Read(bytes.NewReader(stream), binary.BigEndian, &streamInt); err != nil {
			return nil, err
		}
		idx := sort.Search(len(tokens), func(i int) bool {
			return !(tokens[i] < streamInt)
		})
		if idx >= len(tokens) {
			idx = 0
		}
		chosenTok := tokens[idx]
		vnodesToStreams[chosenTok] = append(vnodesToStreams[chosenTok], stream)
	}

	groups := make([][]stream, 0, len(vnodesToStreams))
	for _, streams := range vnodesToStreams {
		groups = append(groups, streams)
	}
	return groups, nil
}

// func (r *Reader) processStreamGroup(ctx context.Context, streams []stream) error {
// 	if len(streams) == 0 {
// 		return nil
// 	}

// 	lastTimestamp := gocql.MinTimeUUID(time.Now())

// 	streamsWildcards := "?" + strings.Repeat(", ?", len(streams)-1)
// 	queryString := fmt.Sprintf(
// 		"SELECT * FROM %s WHERE \"cdc$stream_id\" IN (%s) AND \"cdc$time\" > ? AND \"cdc$time\" < ?",
// 		r.config.LogTableName,
// 		streamsWildcards,
// 	)
// 	query := r.config.Session.Query(queryString).Consistency(r.config.Consistency)

// 	for {
// 		if atomic.LoadInt32(&r.stopped) != 0 {
// 			return nil
// 		}
// 		if err := ctx.Err(); err != nil {
// 			return err
// 		}

// 		readRangeStartTimestamp := lastTimestamp
// 		if r.config.LowerBoundReadOffset != 0 {
// 			readRangeStartTimestamp = gocql.MinTimeUUID(lastTimestamp.Time().Add(-r.config.LowerBoundReadOffset))
// 		}

// 		readRangeEndTimestamp := gocql.MaxTimeUUID(time.Now().Add(-r.config.UpperBoundReadOffset))

// 		bindArgs := make([]interface{}, 0, len(streams)+2)
// 		for _, stream := range streams {
// 			bindArgs = append(bindArgs, stream)
// 		}
// 		bindArgs = append(bindArgs, readRangeStartTimestamp, readRangeEndTimestamp)

// 		iter := query.Bind(bindArgs...).Iter()

// 		for {
// 			timestamp := &gocql.UUID{}
// 			data := map[string]interface{}{
// 				"cdc$time": timestamp,
// 			}
// 			if !iter.MapScan(data) {
// 				break
// 			}

// 			r.config.ChangeConsumer.Consume(Change{data: data})

// 			if bytes.Compare(lastTimestamp[:], timestamp[:]) < 0 {
// 				lastTimestamp = *timestamp
// 			}
// 		}

// 		if err := iter.Close(); err != nil {
// 			return err
// 		}
// 	}
// }
