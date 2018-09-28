// Copyright (c) 2018 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package sync

import (
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/uber-go/tally"
)

const (
	numGoroutinesGaugeSampleRate = 1000
)

type pooledWorkerPool struct {
	sync.Mutex
	numRoutinesAtomic     int64
	numRoutinesGauge      tally.Gauge
	growOnDemand          bool
	workChs               []chan Work
	numShards             int64
	killWorkerProbability float64
	nowFn                 NowFn
}

// NewPooledWorkerPool creates a new worker pool.
func NewPooledWorkerPool(size int, opts PooledWorkerPoolOptions) (PooledWorkerPool, error) {
	if size <= 0 {
		return nil, fmt.Errorf("pooled worker pool size too small: %d", size)
	}

	numShards := opts.NumShards()
	if int64(size) < numShards {
		numShards = int64(size)
	}

	workChs := make([]chan Work, numShards)
	for i := range workChs {
		workChs[i] = make(chan Work, int64(size)/numShards)
	}

	return &pooledWorkerPool{
		numRoutinesAtomic:     0,
		numRoutinesGauge:      opts.InstrumentOptions().MetricsScope().Gauge("num-routines"),
		growOnDemand:          opts.GrowOnDemand(),
		workChs:               workChs,
		numShards:             numShards,
		killWorkerProbability: opts.KillWorkerProbability(),
		nowFn: opts.NowFn(),
	}, nil
}

func (p *pooledWorkerPool) Init() {
	for _, workCh := range p.workChs {
		for i := 0; i < cap(workCh); i++ {
			p.spawnWorker(nil, workCh, true)
		}
	}
}

func (p *pooledWorkerPool) Go(work Work) {
	var (
		// Use time.Now() to avoid excessive synchronization
		currTime  = p.nowFn().UnixNano()
		workChIdx = currTime % p.numShards
		workCh    = p.workChs[workChIdx]
	)

	if currTime%numGoroutinesGaugeSampleRate == 0 {
		p.emitNumRoutines()
	}

	if !p.growOnDemand {
		workCh <- work
		return
	}

	select {
	case workCh <- work:
	default:
		// If the queue for the worker we were assigned to is full,
		// allocate a new goroutine to do the work and then
		// assign it to be a temporary additional worker for the queue.
		// This allows the worker pool to accommodate "bursts" of
		// traffic. Also, it reduces the need for operators to tune the size
		// of the pool for a given workload. If the pool is initially
		// sized too small, it will eventually grow to accommodate the
		// workload, and if the workload decreases the killWorkerProbability
		// will slowly shrink the pool back down to its original size because
		// workers created in this manner will not spawn their replacement
		// before killing themselves.
		p.spawnWorker(work, workCh, false)
	}
}

func (p *pooledWorkerPool) spawnWorker(
	initialWork Work, workCh chan Work, spawnReplacement bool) {
	go func() {
		p.incNumRoutines()
		if initialWork != nil {
			initialWork()
		}

		// RNG per worker to avoid synchronization.
		rng := rand.New(rand.NewSource(p.nowFn().UnixNano()))
		for f := range workCh {
			f()
			if rng.Float64() < p.killWorkerProbability {
				if spawnReplacement {
					p.spawnWorker(nil, workCh, true)
				}
				p.decNumRoutines()
				return
			}
		}
	}()
}

func (p *pooledWorkerPool) emitNumRoutines() {
	numRoutines := float64(p.getNumRoutines())
	p.numRoutinesGauge.Update(numRoutines)
}

func (p *pooledWorkerPool) incNumRoutines() {
	atomic.AddInt64(&p.numRoutinesAtomic, 1)
}

func (p *pooledWorkerPool) decNumRoutines() {
	atomic.AddInt64(&p.numRoutinesAtomic, -1)
}

func (p *pooledWorkerPool) getNumRoutines() int64 {
	return atomic.LoadInt64(&p.numRoutinesAtomic)
}