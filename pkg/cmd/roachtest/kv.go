// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.

package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/cockroachdb/cockroach/pkg/server"
	"github.com/cockroachdb/cockroach/pkg/ts/tspb"
	"github.com/cockroachdb/cockroach/pkg/util/httputil"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
)

func registerKV(r *registry) {
	runKV := func(ctx context.Context, t *test, c *cluster, percent int, encryption option) {
		nodes := c.nodes - 1
		c.Put(ctx, cockroach, "./cockroach", c.Range(1, nodes))
		c.Put(ctx, workload, "./workload", c.Node(nodes+1))
		c.Start(ctx, t, c.Range(1, nodes), encryption)

		t.Status("running workload")
		m := newMonitor(ctx, c, c.Range(1, nodes))
		m.Go(func(ctx context.Context) error {
			concurrency := ifLocal("", " --concurrency="+fmt.Sprint(nodes*64))
			duration := " --duration=" + ifLocal("10s", "10m")
			cmd := fmt.Sprintf(
				"./workload run kv --init --read-percent=%d --splits=1000 --histograms=logs/stats.json"+
					concurrency+duration+
					" {pgurl:1-%d}",
				percent, nodes)
			c.Run(ctx, c.Node(nodes+1), cmd)
			return nil
		})
		m.Wait()
	}

	for _, p := range []int{0, 95} {
		p := p
		for _, n := range []int{1, 3} {
			for _, e := range []bool{false, true} {
				e := e
				minVersion := "v2.0.0"
				if e {
					minVersion = "v2.1.0"
				}
				r.Add(testSpec{
					Name:       fmt.Sprintf("kv%d/encrypt=%t/nodes=%d", p, e, n),
					MinVersion: minVersion,
					Cluster:    makeClusterSpec(n+1, cpu(8)),
					Run: func(ctx context.Context, t *test, c *cluster) {
						runKV(ctx, t, c, p, startArgs(fmt.Sprintf("--encrypt=%t", e)))
					},
				})
			}
		}
	}
}

func registerKVQuiescenceDead(r *registry) {
	r.Add(testSpec{
		Name:       "kv/quiescence/nodes=3",
		Cluster:    makeClusterSpec(4),
		MinVersion: "v2.1.0",
		Run: func(ctx context.Context, t *test, c *cluster) {
			nodes := c.nodes - 1
			c.Put(ctx, cockroach, "./cockroach", c.Range(1, nodes))
			c.Put(ctx, workload, "./workload", c.Node(nodes+1))
			c.Start(ctx, t, c.Range(1, nodes))

			run := func(cmd string, lastDown bool) {
				n := nodes
				if lastDown {
					n--
				}
				m := newMonitor(ctx, c, c.Range(1, n))
				m.Go(func(ctx context.Context) error {
					t.WorkerStatus(cmd)
					defer t.WorkerStatus()
					return c.RunE(ctx, c.Node(nodes+1), cmd)
				})
				m.Wait()
			}

			db := c.Conn(ctx, 1)
			defer db.Close()

			waitForFullReplication(t, db)

			qps := func(f func()) float64 {

				numInserts := func() float64 {
					var v float64
					if err := db.QueryRowContext(
						ctx, `SELECT value FROM crdb_internal.node_metrics WHERE name = 'sql.insert.count'`,
					).Scan(&v); err != nil {
						t.Fatal(err)
					}
					return v
				}

				tBegin := timeutil.Now()
				before := numInserts()
				f()
				after := numInserts()
				return (after - before) / timeutil.Since(tBegin).Seconds()
			}

			const kv = "./workload run kv --duration=10m --read-percent=0"

			// Initialize the database with ~10k ranges so that the absence of
			// quiescence hits hard once a node goes down.
			run("./workload run kv --init --max-ops=1 --splits 10000 --concurrency 100 {pgurl:1}", false)
			run(kv+" --seed 0 {pgurl:1}", true) // warm-up
			// Measure qps with all nodes up (i.e. with quiescence).
			qpsAllUp := qps(func() {
				run(kv+" --seed 1 {pgurl:1}", true)
			})
			// Gracefully shut down third node (doesn't matter whether it's graceful or not).
			c.Run(ctx, c.Node(nodes), "./cockroach quit --insecure --host=:{pgport:3}")
			c.Stop(ctx, c.Node(nodes))
			// Measure qps with node down (i.e. without quiescence).
			qpsOneDown := qps(func() {
				// Use a different seed to make sure it's not just stepping into the
				// other earlier kv invocation's footsteps.
				run(kv+" --seed 2 {pgurl:1}", true)
			})

			if minFrac, actFrac := 0.8, qpsOneDown/qpsAllUp; actFrac < minFrac {
				t.Fatalf(
					"QPS dropped from %.2f to %.2f (factor of %.2f, min allowed %.2f)",
					qpsAllUp, qpsOneDown, actFrac, minFrac,
				)
			}
			t.l.Printf("QPS went from %.2f to %2.f with one node down\n", qpsAllUp, qpsOneDown)
		},
	})
}

func registerKVGracefulDraining(r *registry) {
	r.Add(testSpec{
		Name:    "kv/gracefuldraining/nodes=3",
		Cluster: makeClusterSpec(4),
		Run: func(ctx context.Context, t *test, c *cluster) {
			nodes := c.nodes - 1
			c.Put(ctx, cockroach, "./cockroach", c.Range(1, nodes))
			c.Put(ctx, workload, "./workload", c.Node(nodes+1))
			c.Start(ctx, t, c.Range(1, nodes))

			db := c.Conn(ctx, 1)
			defer db.Close()

			waitForFullReplication(t, db)

			// Initialize the database with a lot of ranges so that there are
			// definitely a large number of leases on the node that we shut down
			// before it starts draining.
			splitCmd := "./workload run kv --init --max-ops=1 --splits 100 {pgurl:1}"
			c.Run(ctx, c.Node(nodes+1), splitCmd)

			m := newMonitor(ctx, c, c.Range(1, nodes))

			// Run kv for 5 minutes, during which we can gracefully kill nodes and
			// determine whether doing so affects the cluster-wide qps.
			const expectedQPS = 1000
			m.Go(func(ctx context.Context) error {
				cmd := fmt.Sprintf(
					"./workload run kv --duration=5m --read-percent=0 --tolerate-errors --max-rate=%d {pgurl:1-%d}",
					expectedQPS, nodes-1)
				t.WorkerStatus(cmd)
				defer t.WorkerStatus()
				return c.RunE(ctx, c.Node(nodes+1), cmd)
			})

			m.Go(func(ctx context.Context) error {
				// Gracefully shut down the third node, let the cluster run for a
				// while, then restart it. Then repeat for good measure.
				for i := 0; i < 2; i++ {
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(1 * time.Minute):
					}
					c.Run(ctx, c.Node(nodes), "./cockroach quit --insecure --host=:{pgport:3}")
					c.Stop(ctx, c.Node(nodes))
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(1 * time.Minute):
					}
					c.Start(ctx, t, c.Node(nodes))
				}
				return nil
			})

			// Let the test run for nearly the entire duration of the kv command.
			runDuration := 4*time.Minute + 30*time.Second
			time.Sleep(runDuration)

			// Check that the QPS has been at the expected max rate for the entire
			// test duration, even as one of the nodes was being stopped and started.
			adminURLs := c.ExternalAdminUIAddr(ctx, c.Node(1))
			url := "http://" + adminURLs[0] + "/ts/query"
			now := timeutil.Now()
			request := tspb.TimeSeriesQueryRequest{
				StartNanos: now.Add(-runDuration).UnixNano(),
				EndNanos:   now.UnixNano(),
				// Check the performance in each timeseries sample interval.
				SampleNanos: server.DefaultMetricsSampleInterval.Nanoseconds(),
				Queries: []tspb.Query{
					{
						Name:             "cr.node.sql.query.count",
						Downsampler:      tspb.TimeSeriesQueryAggregator_AVG.Enum(),
						SourceAggregator: tspb.TimeSeriesQueryAggregator_SUM.Enum(),
						Derivative:       tspb.TimeSeriesQueryDerivative_NON_NEGATIVE_DERIVATIVE.Enum(),
					},
				},
			}
			var response tspb.TimeSeriesQueryResponse
			if err := httputil.PostJSON(http.Client{}, url, &request, &response); err != nil {
				t.Fatal(err)
			}
			if len(response.Results[0].Datapoints) <= 1 {
				t.Fatalf("not enough datapoints in timeseries query response: %+v", response)
			}
			datapoints := response.Results[0].Datapoints

			// Because we're specifying a --max-rate well less than what cockroach
			// should be capable of, draining one of the three nodes should have no
			// effect on performance at all, meaning that a fairly aggressive
			// threshold here should be ok.
			minQPS := expectedQPS * 0.9

			// Examine every data point except the first one, because at that time
			// splits may still have been happening or the cluster may still have
			// been initializing.
			for i := 1; i < len(datapoints); i++ {
				if qps := datapoints[i].Value; qps < minQPS {
					t.Fatalf(
						"QPS of %.2f at time %v is below minimum allowable QPS of %.2f; entire timeseries: %+v",
						qps, timeutil.Unix(0, datapoints[i].TimestampNanos), minQPS, datapoints)
				}
			}

			m.Wait()
		},
	})
}

func registerKVSplits(r *registry) {
	for _, item := range []struct {
		quiesce bool
		splits  int
		timeout time.Duration
	}{
		// NB: with 500000 splits, this test sometimes fails since it's pushing
		// far past the number of replicas per node we support, at least if the
		// ranges start to unquiesce (which can set off a cascade due to resource
		// exhaustion).
		{true, 300000, 2 * time.Hour},
		// This version of the test prevents range quiescence to trigger the
		// badness described above more reliably for when we wish to improve
		// the performance.
		{false, 100000, 2 * time.Hour},
	} {
		item := item // for use in closure below
		r.Add(testSpec{
			Name:    fmt.Sprintf("kv/splits/nodes=3/quiesce=%t", item.quiesce),
			Timeout: item.timeout,
			Cluster: makeClusterSpec(4),
			Run: func(ctx context.Context, t *test, c *cluster) {
				nodes := c.nodes - 1
				c.Put(ctx, cockroach, "./cockroach", c.Range(1, nodes))
				c.Put(ctx, workload, "./workload", c.Node(nodes+1))
				c.Start(ctx, t, c.Range(1, nodes),
					startArgs(
						"--env=COCKROACH_MEMPROF_INTERVAL=1m",
						"--env=COCKROACH_DISABLE_QUIESCENCE="+strconv.FormatBool(!item.quiesce),
						"--args=--cache=256MiB",
					))

				t.Status("running workload")
				m := newMonitor(ctx, c, c.Range(1, nodes))
				m.Go(func(ctx context.Context) error {
					concurrency := ifLocal("", " --concurrency="+fmt.Sprint(nodes*64))
					splits := " --splits=" + ifLocal("2000", fmt.Sprint(item.splits))
					cmd := fmt.Sprintf(
						"./workload run kv --init --max-ops=1"+
							concurrency+splits+
							" {pgurl:1-%d}",
						nodes)
					c.Run(ctx, c.Node(nodes+1), cmd)
					return nil
				})
				m.Wait()
			},
		})
	}
}

func registerKVScalability(r *registry) {
	runScalability := func(ctx context.Context, t *test, c *cluster, percent int) {
		nodes := c.nodes - 1

		c.Put(ctx, cockroach, "./cockroach", c.Range(1, nodes))
		c.Put(ctx, workload, "./workload", c.Node(nodes+1))

		const maxPerNodeConcurrency = 64
		for i := nodes; i <= nodes*maxPerNodeConcurrency; i += nodes {
			c.Wipe(ctx, c.Range(1, nodes))
			c.Start(ctx, t, c.Range(1, nodes))

			t.Status("running workload")
			m := newMonitor(ctx, c, c.Range(1, nodes))
			m.Go(func(ctx context.Context) error {
				cmd := fmt.Sprintf("./workload run kv --init --read-percent=%d "+
					"--splits=1000 --duration=1m "+fmt.Sprintf("--concurrency=%d", i)+
					" {pgurl:1-%d}",
					percent, nodes)

				l, err := t.l.ChildLogger(fmt.Sprint(i))
				if err != nil {
					t.Fatal(err)
				}
				defer l.close()

				return c.RunL(ctx, l, c.Node(nodes+1), cmd)
			})
			m.Wait()
		}
	}

	// TODO(peter): work in progress adaption of `roachprod test kv{0,95}`.
	if false {
		for _, p := range []int{0, 95} {
			p := p
			r.Add(testSpec{
				Name:    fmt.Sprintf("kv%d/scale/nodes=6", p),
				Cluster: makeClusterSpec(7, cpu(8)),
				Run: func(ctx context.Context, t *test, c *cluster) {
					runScalability(ctx, t, c, p)
				},
			})
		}
	}
}
