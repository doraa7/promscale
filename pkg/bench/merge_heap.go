package bench

import (
	"container/heap"
	"fmt"
	"io"
	"sync"

	"github.com/prometheus/prometheus/pkg/labels"
	"github.com/prometheus/prometheus/tsdb"
	"github.com/prometheus/prometheus/tsdb/chunks"
	"github.com/prometheus/prometheus/tsdb/record"
)

type point struct {
	series_id uint64
	ts        int64
	val       float64
	err       error
}

type Worker struct {
	p    *point
	next chan *point
	sth  *SeriesTimeHeap
	wq   *BufferingIteratorWorkQueue
	wg   sync.WaitGroup
}

func NewWorker() *Worker {
	wq := NewBufferingIteratorWorkQueue()
	ch := make(chan *point, 1000)
	sth := NewSeriesTimeHeap()
	return &Worker{nil, ch, sth, wq, sync.WaitGroup{}}
}

func (w *Worker) Close() error {
	w.wg.Wait()
	return w.wq.Close()
}

func (w *Worker) AddSeries(seriesID uint64, chks []chunks.Meta, chunkr tsdb.ChunkReader) {
	chksCopy := make([]chunks.Meta, len(chks))
	copy(chksCopy, chks)

	it := NewBufferingIterator(seriesID, chunkr, chksCopy, w.wq)
	w.sth.Add(seriesID, it)
}

func (w *Worker) Run() error {
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.sth.Run(w.next)
	}()
	nextPoint, ok := <-w.next
	if nextPoint.err != nil {
		return nextPoint.err
	}
	if !ok {
		panic("unexpected closed channel")
	}
	w.p = nextPoint
	return nil
}

type MergeHeap []*Worker

func NewMergeHeap(dm *DataModifier, block *tsdb.Block, qmi *qmInfo, seriesIndex int, concurrency int) (MergeHeap, []io.Closer, error) {
	fmt.Println("Starting to load series")
	seriesId := uint64(0)
	closers := []io.Closer{}

	ir, err := block.Index()
	if err != nil {
		return nil, closers, err
	}

	p, err := ir.Postings("", "") // The special all key.
	if err != nil {
		return nil, closers, err
	}
	closers = append(closers, ir)

	chunkr, err := block.Chunks()
	if err != nil {
		return nil, closers, err
	}
	closers = append(closers, chunkr)

	chks := []chunks.Meta{}

	workers := make(MergeHeap, concurrency)
	for i := range workers {
		wi := NewWorker()
		closers = append(closers, wi)
		workers[i] = wi
	}
	countSeries := 0

	for p.Next() {
		lbls := labels.Labels{}

		if err = ir.Series(p.At(), &lbls, &chks); err != nil {
			return nil, closers, err
		}

		workers[countSeries%len(workers)].AddSeries(seriesId, chks, chunkr)
		countSeries++

		dm.VisitSeries(seriesId, lbls, func(rec record.RefSeries) {
			seriesId++
			qmi.qm.StoreSeries([]record.RefSeries{rec}, seriesIndex)
		})
	}
	fmt.Println("Done loading series, # series", seriesId)

	if err := p.Err(); err != nil {
		return nil, closers, err
	}

	for i := range workers {
		if err := workers[i].Run(); err != nil {
			return nil, closers, err
		}
	}

	heap.Init(&workers)
	return workers, closers, nil
}

func (mh MergeHeap) Len() int { return len(mh) }

func (mh MergeHeap) Less(i, j int) bool {
	tsi := mh[i].p.ts
	tsj := mh[j].p.ts
	if tsi == tsj {
		return mh[i].p.series_id < mh[j].p.series_id
	}
	return tsi < tsj
}

func (mh MergeHeap) Swap(i, j int) {
	mh[i], mh[j] = mh[j], mh[i]
}

func (mh *MergeHeap) Push(x interface{}) {
	item := x.(*Worker)
	*mh = append(*mh, item)
}

func (mh *MergeHeap) Pop() interface{} {
	old := *mh
	n := len(old)
	item := old[n-1]
	old[n-1] = nil // avoid memory leak
	*mh = old[0 : n-1]
	return item
}

func (mh *MergeHeap) Visit(dm *DataModifier, visitor func([]record.RefSample, int64) error) error {
	for mh.Len() > 0 {
		item := (*mh)[0]

		dm.VisitSamples(item.p.series_id, item.p.ts, item.p.val, visitor)
		nextPoint, ok := <-item.next
		if nextPoint.err != nil {
			return nextPoint.err
		}

		if ok {
			item.p = nextPoint
			heap.Fix(mh, 0)
		} else {
			item2 := heap.Pop(mh)
			if item != item2 {
				panic("items not equal")
			}
		}
	}
	return nil
}

func (mh *MergeHeap) Debug() {
	for _, w := range *mh {
		w.sth.Debug()
	}
}
