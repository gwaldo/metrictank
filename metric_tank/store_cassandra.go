package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gocql/gocql"
	"github.com/grafana/grafana/pkg/log"
	//"github.com/dgryski/go-tsz"
	"github.com/raintank/go-tsz"
)

// write aggregated data to cassandra.

const month_sec = 60 * 60 * 24 * 28

const keyspace_schema = `CREATE KEYSPACE IF NOT EXISTS raintank WITH replication = {'class': 'SimpleStrategy', 'replication_factor': 1}  AND durable_writes = true`
const table_schema = `CREATE TABLE IF NOT EXISTS raintank.metric (
    key ascii,
    ts int,
    data blob,
    PRIMARY KEY (key, ts)
) WITH COMPACT STORAGE
    AND CLUSTERING ORDER BY (ts DESC)
    AND compaction = {'class': 'org.apache.cassandra.db.compaction.DateTieredCompactionStrategy'}
    AND compression = {'sstable_compression': 'org.apache.cassandra.io.compress.LZ4Compressor'}
    AND read_repair_chance = 0.0
    AND dclocal_read_repair_chance = 0`

/*
https://godoc.org/github.com/gocql/gocql#Session
Session is the interface used by users to interact with the database.
It's safe for concurrent use by multiple goroutines and a typical usage scenario is to have one global session
object to interact with the whole Cassandra cluster.
*/
var cSession *gocql.Session
var CassandraWriteQueue chan *ChunkWriteRequest

type ChunkWriteRequest struct {
	key       string
	chunk     *Chunk
	ttl       uint32
	timestamp time.Time
}

func InitCassandra() error {
	cluster := gocql.NewCluster(strings.Split(*cassandraAddrs, ",")...)
	cluster.Consistency = gocql.One
	var err error
	tmpSession, err := cluster.CreateSession()
	if err != nil {
		return err
	}
	// ensure the keyspace and table exist.
	err = tmpSession.Query(keyspace_schema).Exec()
	if err != nil {
		return err
	}
	err = tmpSession.Query(table_schema).Exec()
	if err != nil {
		return err
	}
	tmpSession.Close()
	cluster.Keyspace = "raintank"
	cluster.NumConns = *cassandraWriteConcurrency
	cSession, err = cluster.CreateSession()

	CassandraWriteQueue = make(chan *ChunkWriteRequest)
	workerQueues := make([]chan *ChunkWriteRequest, *cassandraWriteConcurrency)
	for i := 0; i < *cassandraWriteConcurrency; i++ {
		workerQueues[i] = make(chan *ChunkWriteRequest, *cassandraWriteQueueSize)
		go processWriteQueue(workerQueues[i])
	}
	go func() {
		var sum int
		for cwr := range CassandraWriteQueue {
			sum = 0
			for _, char := range cwr.key {
				sum += int(char)
			}
			workerQueues[sum%*cassandraWriteConcurrency] <- cwr
		}
	}()

	return err
}

/* process writeQueue.
 */
func processWriteQueue(queue chan *ChunkWriteRequest) {
	for {
		select {
		case c := <-queue:
			log.Debug("starting to save %s:%d %v", c.key, c.chunk.T0, c.chunk)
			//log how long the chunk waited in the queue before we attempted to save to cassandra
			cassandraBlockDuration.Value(time.Now().Sub(c.timestamp))

			data := c.chunk.Series.Bytes()
			chunkSizeAtSave.Value(int64(len(data)))
			version := FormatStandardGoTsz
			buf := new(bytes.Buffer)
			err := binary.Write(buf, binary.LittleEndian, uint8(version))
			if err != nil {
				// TODO
			}
			_, err = buf.Write(data)
			if err != nil {
				// TODO
			}
			success := false
			attempts := 0
			for !success {
				err := InsertChunk(c.key, c.chunk.T0, buf.Bytes(), int(c.ttl))
				if err == nil {
					success = true
					c.chunk.Saved = true
					SendPersistMessage(c.key, c.chunk.T0)
					log.Debug("save complete. %s:%d %v", c.key, c.chunk.T0, c.chunk)
					chunkSaveOk.Inc(1)
				} else {
					if (attempts % 20) == 0 {
						log.Warn("failed to save chunk to cassandra after %d attempts. %v, %s", attempts+1, c.chunk, err)
					}
					chunkSaveFail.Inc(1)
					sleepTime := 100 * attempts
					if sleepTime > 2000 {
						sleepTime = 2000
					}
					time.Sleep(time.Duration(sleepTime) * time.Millisecond)
					attempts++
				}
			}
		}
	}
}

// Insert Chunks into Cassandra.
//
// key: is the metric_id
// ts: is the start of the aggregated time range.
// data: is the payload as bytes.
func InsertChunk(key string, t0 uint32, data []byte, ttl int) error {
	// for unit tests
	if cSession == nil {
		return nil
	}
	query := fmt.Sprintf("INSERT INTO metric (key, ts, data) values(?,?,?) USING TTL %d", ttl)
	row_key := fmt.Sprintf("%s_%d", key, t0/month_sec) // "month number" based on unix timestamp (rounded down)
	pre := time.Now()
	ret := cSession.Query(query, row_key, t0, data).Exec()
	cassandraPutDuration.Value(time.Now().Sub(pre))
	return ret
}

type outcome struct {
	month   uint32
	sortKey uint32
	i       *gocql.Iter
}
type asc []outcome

func (o asc) Len() int           { return len(o) }
func (o asc) Swap(i, j int)      { o[i], o[j] = o[j], o[i] }
func (o asc) Less(i, j int) bool { return o[i].sortKey < o[j].sortKey }

// Basic search of cassandra.
// start inclusive, end exclusive
func searchCassandra(key string, start, end uint32) ([]Iter, error) {
	iters := make([]Iter, 0)
	if start > end {
		return iters, fmt.Errorf("start must be before end.")
	}

	results := make(chan outcome)
	wg := &sync.WaitGroup{}

	query := func(month, sortKey uint32, q string, p ...interface{}) {
		wg.Add(1)
		go func() {
			results <- outcome{month, sortKey, cSession.Query(q, p...).Iter()}
			wg.Done()
		}()
	}

	start_month := start - (start % month_sec)       // starting row has to be at, or before, requested start
	end_month := (end - 1) - ((end - 1) % month_sec) // ending row has to be include the last point we might need

	pre := time.Now()

	// unfortunately in the database we only have the t0's of all chunks.
	// this means we can easily make sure to include the correct last chunk (just query for a t0 < end, the last chunk will contain the last needed data)
	// but it becomes hard to find which should be the first chunk to include. we can't just query for start <= t0 because than will miss some data at the beginning
	// we can't assume we know the chunkSpan so we can't just calculate the t0 >= start - <some-predefined-number> because chunkSpans may change over time.
	// we effectively need all chunks with a t0 > start, as well as the last chunk with a t0 <= start.
	// since we make sure that you can only use chunkSpans so that month_sec % chunkSpan == 0, we know that this previous chunk will always be in the same row
	// as the one that has start_month.

	row_key := fmt.Sprintf("%s_%d", key, start_month/month_sec)
	query(start_month, start_month, "SELECT ts, data FROM metric WHERE key=? AND ts <= ? Limit 1", row_key, start)

	if start_month == end_month {
		// we need a selection of the row between startTs and endTs
		row_key = fmt.Sprintf("%s_%d", key, start_month/month_sec)
		query(start_month, start_month+1, "SELECT ts, data FROM metric WHERE key = ? AND ts > ? AND ts < ? ORDER BY ts ASC", row_key, start, end)
	} else {
		// get row_keys for each row we need to query.
		for month := start_month; month <= end_month; month += month_sec {
			row_key = fmt.Sprintf("%s_%d", key, month/month_sec)
			if month == start_month {
				// we want from startTs to the end of the row.
				query(month, month+1, "SELECT ts, data FROM metric WHERE key = ? AND ts >= ? ORDER BY ts ASC", row_key, start+1)
			} else if month == end_month {
				// we want from start of the row till the endTs.
				query(month, month, "SELECT ts, data FROM metric WHERE key = ? AND ts <= ? ORDER BY ts ASC", row_key, end-1)
			} else {
				// we want all columns
				query(month, month, "SELECT ts, data FROM metric WHERE key = ? ORDER BY ts ASC", row_key)
			}
		}
	}
	outcomes := make([]outcome, 0)

	// wait for all queries to complete then close the results channel so that the following
	// for loop ends.
	go func() {
		wg.Wait()
		cassandraGetDuration.Value(time.Now().Sub(pre))
		close(results)
	}()

	for o := range results {
		outcomes = append(outcomes, o)
	}
	// we have all of the results, but they could have arrived in any order.
	sort.Sort(asc(outcomes))

	var b []byte
	var ts int
	for _, outcome := range outcomes {
		chunks := int64(0)
		for outcome.i.Scan(&ts, &b) {
			chunks += 1
			chunkSizeAtLoad.Value(int64(len(b)))
			if len(b) < 2 {
				err := errors.New("unpossibly small chunk in cassandra")
				log.Error(3, err.Error())
				return iters, err
			}
			startByte := 1
			if Format(b[0]) != FormatStandardGoTsz {
				// Production currently has a few weeks of data that does not have
				// a format byte.  In 1 month (20th Jan 2016), this can be removed.
				//err := errors.New("unrecognized chunk format in cassandra")
				//log.Error(3, err.Error())
				//return iters, err
				startByte = 0
			}
			iter, err := tsz.NewIterator(b[startByte:])
			if err != nil {
				log.Error(3, "failed to unpack cassandra payload. %s", err)
				return iters, err
			}
			iters = append(iters, NewIter(iter, "cassandra month=%d t0=%d", outcome.month, ts))
		}
		cassandraChunksPerRow.Value(chunks)
		err := outcome.i.Close()
		if err != nil {
			log.Error(3, "cassandra query error. %s", err)
		}
	}
	cassandraRowsPerResponse.Value(int64(len(outcomes)))
	log.Debug("searchCassandra(): %d outcomes (queries), %d total iters", len(outcomes), len(iters))
	return iters, nil
}