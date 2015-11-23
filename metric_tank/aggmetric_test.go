package main

import (
	"fmt"
	"github.com/grafana/grafana/pkg/metric/helper"
	"testing"
)

type point struct {
	ts  uint32
	val float64
}

func (p point) String() string {
	return fmt.Sprintf("point{%0.f at %d}", p.val, p.ts)
}

type Checker struct {
	t      *testing.T
	agg    *AggMetric
	points []point
}

func NewChecker(t *testing.T, agg *AggMetric) *Checker {
	return &Checker{t, agg, make([]point, 0)}
}

// always add points in ascending order, never same ts!
func (c *Checker) Add(ts uint32, val float64) {
	c.agg.Add(ts, val)
	c.points = append(c.points, point{ts, val})
}

// from to is the range that gets requested from AggMetric
// first/last is what we use as data range to compare to (both inclusive)
// these may be different because AggMetric returns broader rangers (due to packed format),
func (c *Checker) Verify(from, to, first, last uint32) {
	_, iters := c.agg.Get(from, to)
	// we don't do checking or fancy logic, it is assumed that the caller made sure first and last are ts of actual points
	var pi int // index of first point we want
	var pj int // index of last point we want
	for pi = 0; c.points[pi].ts != first; pi++ {
	}
	for pj = pi; c.points[pj].ts != last; pj++ {
	}
	c.t.Logf("verifying AggMetric.Get(%d,%d) =?= %d <= ts <= %d", from, to, first, last)
	index := pi - 1
	for _, iter := range iters {
		for iter.Next() {
			index++
			tt, vv := iter.Values()
			//c.t.Logf("got (%v,%v).. should be (%v,%v)", tt, vv, c.points[index].ts, c.points[index].val)
			if index > pj {
				c.t.Fatalf("Values()=(%v,%v), want end of stream\n", tt, vv)
			}
			if c.points[index].ts != tt || c.points[index].val != vv {
				c.t.Fatalf("Values()=(%v,%v), want (%v,%v)\n", tt, vv, c.points[index].ts, c.points[index].val)
			}
		}
	}
	if index != pj {
		c.t.Fatalf("not all values returned. missing %v", c.points[index:pj+1])
	}
}

func TestAggMetric(t *testing.T) {
	stats, _ := helper.New(false, "", "standard", "metrics_tank", "")
	initMetrics(stats)
	c := NewChecker(t, NewAggMetric("foo", 100, 5))

	// basic case, single range
	c.Add(101, 101)
	c.Verify(100, 200, 101, 101)
	c.Add(105, 105)
	c.Verify(100, 199, 101, 105)
	c.Add(115, 115)
	c.Add(125, 125)
	c.Add(135, 135)
	c.Verify(100, 199, 101, 135)

	// add new ranges, aligned and unaligned
	c.Add(200, 200)
	c.Add(315, 315)
	c.Verify(100, 399, 101, 315)

	// get subranges
	c.Verify(120, 299, 101, 200)
	c.Verify(220, 299, 200, 200)
	c.Verify(312, 330, 315, 315)

	// border dancing. good for testing inclusivity and exclusivity
	c.Verify(100, 199, 101, 135)
	c.Verify(100, 200, 101, 135)
	c.Verify(100, 201, 101, 200)
	c.Verify(198, 199, 101, 135)
	c.Verify(199, 200, 101, 135)
	c.Verify(200, 201, 200, 200)
	c.Verify(201, 202, 200, 200)
	c.Verify(299, 300, 200, 200)
	c.Verify(300, 301, 315, 315)

	// skipping
	c.Add(510, 510)
	c.Add(512, 512)
	c.Verify(100, 599, 101, 512)

	// basic wraparound
	c.Add(610, 610)
	c.Add(612, 612)
	c.Add(710, 710)
	c.Add(712, 712)
	// TODO would be nice to test that it panics when requesting old range. something with recover?
	//c.Verify(100, 799, 101, 512)

	// largest range we have so far
	c.Verify(300, 799, 315, 712)
	// a smaller range
	c.Verify(502, 799, 510, 712)

	// the circular buffer had these ranges:
	// 100 200 300 skipped 500
	// then we made it:
	// 600 700 300 skipped 500
	// now we want to do another wrap around with skip (must have cleared old data)
	// let's jump to 1200. the accessible range should then be 800-1200
	// clea 1200 clea clea clea
	// we can't (and shouldn't, due to abstraction) test the clearing itself
	// but we just check we only get this point
	c.Add(1299, 1299)
	// TODO: implement skips and enable this
	//	c.Verify(800, 1299, 1299, 1299)
}

// basic expected RAM usage for 1 iteration (= 1 days)
// 1000 metrics * (3600 * 24 / 10 ) points per metric * 1.3 B/point = 11 MB
// 1000 metrics * 5 agg metrics per metric * (3600 * 24 / 300) points per aggmetric * 1.3B/point = 1.9 MB
// total -> 13 MB
// go test -run=XX -bench=Bench -benchmem -v -memprofile mem.out
// go tool pprof -inuse_space metrics_tank.test mem.out -> shows 25 MB in use

// TODO update once we clean old data, then we should look at numChunks
func BenchmarkAggMetrics1000Metrics1Day(b *testing.B) {
	// we will store 10s metrics in 5 chunks of 2 hours
	// aggragate them in 5min buckets, stored in 1 chunk of 24hours
	chunkSpan := uint32(2 * 3600)
	numChunks := uint32(5)
	aggSpan := uint32(300)
	aggChunkSpan := uint32(24 * 3600)
	numAggChunks := uint32(1)
	chunkMaxStale := uint32(3600)
	metricMaxStale := uint32(21600)

	keys := make([]string, 1000)
	for i := 0; i < 1000; i++ {
		keys[i] = fmt.Sprintf("hello.this.is.a.test.key.%d", i)
	}

	metrics := NewAggMetrics(chunkSpan, numChunks, aggSpan, aggChunkSpan, numAggChunks, chunkMaxStale, metricMaxStale)
	maxT := 3600 * 24 * uint32(b.N) // b.N in days
	for t := uint32(1); t < maxT; t += 10 {
		for metricI := 0; metricI < 1000; metricI++ {
			m := metrics.GetOrCreate(keys[metricI])
			m.Add(t, float64(t))
		}
	}
}