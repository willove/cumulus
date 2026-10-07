package main

import (
	gocontext "context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/willove/cumulus/internal/corpus"
	"github.com/willove/cumulus/internal/query"
	"github.com/willove/cumulus/internal/retrieval"
	storecumulite "github.com/willove/cumulus/internal/store"
)

func main() {
	st, err := storecumulite.Open(filepath.Join(os.Getenv("HOME"), ".cumulus/next/data"), false)
	if err != nil {
		panic(err)
	}
	idsX, _ := st.ListIDs(gocontext.Background(), "documents", 0)
	fmt.Println("store ids:", len(idsX))
	docs, err := corpus.Load(gocontext.Background(), st)
	if err != nil {
		panic(err)
	}
	fmt.Println("corpus docs:", len(docs))
	idx := retrieval.Build(docs)
	q := "专利期限多少年"
	a := query.Analyze(q, idx, idx.N)
	fmt.Printf("analysis primary: %v oov: %v\n", a.Primary, a.OOV)
	words := make([]string, 0)
	for w := range a.Primary {
		words = append(words, w)
	}
	pool := idx.SearchWith(q, 12, 240, nil)
	fmt.Println("BM25 pool (top 12):")
	for i, h := range pool {
		fmt.Printf("  %2d. %-30s bm25=%.1f\n", i+1, h.Title[:min(30, len(h.Title))], h.Score)
	}
	ids := make([]string, 0, len(pool))
	for _, h := range pool {
		ids = append(ids, h.DocID)
	}
	scores := idx.PriorRank(words, ids, 0)
	bm := map[string]float64{}
	for _, h := range pool {
		bm[h.DocID] = h.Score
	}
	fmt.Println("融合后（prior × bm25）:")
	for _, s := range scores[:min(8, len(scores))] {
		fmt.Printf("  prior=%.3f bm25=%5.1f fused=%7.1f  %s\n", s.Score, bm[s.DocID], s.Score*bm[s.DocID], idx.TitleOf(s.DocID)[:min(30, len(idx.TitleOf(s.DocID)))])
	}
	// 专利法在不在池里？
	found := false
	for _, h := range pool {
		if h.Title == "中华人民共和国专利法" {
			found = true
		}
	}
	if !found {
		// 全库 BM25 排名
		all := idx.SearchWith(q, 1537, 240, nil)
		for i, h := range all {
			if h.Title == "中华人民共和国专利法" {
				fmt.Printf("专利法在全库 BM25 排第 %d（score %.1f）——池(k*3=12)外，boost 够不着\n", i+1, h.Score)
				break
			}
		}
	} else {
		fmt.Println("专利法在池内")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
