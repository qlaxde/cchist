package main

import (
	"container/heap"
	"math"
)

const (
	bm25K1 = 1.5
	bm25B  = 0.75
)

// BM25 is a simple in-memory inverted index. It is rebuilt from the corpus on
// every CLI invocation (fast enough for tens of thousands of documents) and
// therefore does not need its own persistence format.
type BM25 struct {
	// postings[term] -> slice of (docID, tf) pairs, packed to halve the
	// allocation count relative to a struct slice.
	postings map[string][]posting
	docLens  []uint32
	avgDocL  float64
}

type posting struct {
	DocID uint32
	TF    uint32
}

func buildBM25(docs [][]string) *BM25 {
	idx := &BM25{
		postings: make(map[string][]posting, 1<<15),
		docLens:  make([]uint32, len(docs)),
	}
	var totalLen uint64
	for i, tokens := range docs {
		idx.docLens[i] = uint32(len(tokens))
		totalLen += uint64(len(tokens))
		// Count term frequencies in the current doc.
		tf := make(map[string]uint32, len(tokens))
		for _, t := range tokens {
			tf[t]++
		}
		for term, c := range tf {
			idx.postings[term] = append(idx.postings[term], posting{DocID: uint32(i), TF: c})
		}
	}
	if len(docs) > 0 {
		idx.avgDocL = float64(totalLen) / float64(len(docs))
	}
	return idx
}

type scoredDoc struct {
	DocID uint32
	Score float64
}

// search returns the top-k docs, sorted by descending score. Zero-score docs
// are filtered out.
func (b *BM25) search(query []string, k int) []scoredDoc {
	if len(query) == 0 || len(b.docLens) == 0 {
		return nil
	}
	n := float64(len(b.docLens))
	scores := make(map[uint32]float64, 64)
	for _, term := range query {
		plist, ok := b.postings[term]
		if !ok {
			continue
		}
		df := float64(len(plist))
		idf := math.Log((n-df+0.5)/(df+0.5) + 1)
		for _, p := range plist {
			tf := float64(p.TF)
			docLen := float64(b.docLens[p.DocID])
			norm := tf * (bm25K1 + 1) /
				(tf + bm25K1*(1-bm25B+bm25B*docLen/b.avgDocL))
			scores[p.DocID] += idf * norm
		}
	}
	return topK(scores, k)
}

// topK extracts the k highest-scoring docs using a min-heap of size k. This is
// O(n log k) which beats sorting the full result slice for typical k << n.
func topK(scores map[uint32]float64, k int) []scoredDoc {
	if k <= 0 {
		return nil
	}
	h := &minHeap{}
	heap.Init(h)
	for id, s := range scores {
		if s <= 0 {
			continue
		}
		if h.Len() < k {
			heap.Push(h, scoredDoc{DocID: id, Score: s})
			continue
		}
		if s > (*h)[0].Score {
			(*h)[0] = scoredDoc{DocID: id, Score: s}
			heap.Fix(h, 0)
		}
	}
	out := make([]scoredDoc, h.Len())
	for i := len(out) - 1; i >= 0; i-- {
		out[i] = heap.Pop(h).(scoredDoc)
	}
	return out
}

type minHeap []scoredDoc

func (h minHeap) Len() int            { return len(h) }
func (h minHeap) Less(i, j int) bool  { return h[i].Score < h[j].Score }
func (h minHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x any)         { *h = append(*h, x.(scoredDoc)) }
func (h *minHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
