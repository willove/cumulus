package adapt

// Parquet support.
//
// HF/ModelScope exports are parquet, and this suite's corpus sources include
// several of them (mmarco retrieval/reranking, baidu_baike). A pure-Go reader
// (github.com/parquet-go/parquet-go) covers them.
//
// The important design point: columns are DISCOVERED from the file's schema and
// mapped through the same autodetect policy as JSON. A fixed id/text struct
// works for mmarco but silently produces empty documents for baidu_baike, whose
// columns are title/content — so nothing may be assumed about the schema.

import (
	"context"
	"fmt"
	"os"

	"github.com/parquet-go/parquet-go"
)

// parquetMagic is the leading and trailing 4 bytes of every parquet file.
const parquetMagic = "PAR1"

// looksLikeParquet reports whether a sniffed prefix is a parquet file.
func looksLikeParquet(prefix []byte) bool {
	return len(prefix) >= 4 && string(prefix[:4]) == parquetMagic
}

// StreamParquetFile adapts one parquet file. It needs a real path (the reader
// wants io.ReaderAt + size), which is why it is separate from Stream.
func StreamParquetFile(ctx context.Context, path string, f Fields, emit func(Doc) error) error {
	fh, err := os.Open(path)
	if err != nil {
		return err
	}
	defer fh.Close()
	st, err := fh.Stat()
	if err != nil {
		return err
	}
	pf, err := parquet.OpenFile(fh, st.Size())
	if err != nil {
		return fmt.Errorf("adapt: parquet %s: %w", path, err)
	}
	cols := pf.Schema().Fields()
	if len(cols) == 0 {
		return fmt.Errorf("adapt: parquet %s: no columns", path)
	}
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name()
	}
	const batch = 64
	rows := make([]parquet.Row, batch)
	emitted := 0
	for _, rg := range pf.RowGroups() {
		rr := rg.Rows()
		for {
			if err := ctx.Err(); err != nil {
				_ = rr.Close()
				return err
			}
			n, rerr := rr.ReadRows(rows)
			for i := 0; i < n; i++ {
				rec := make(map[string]any, len(names))
				for j, name := range names {
					if j < len(rows[i]) {
						rec[name] = rows[i][j].String()
					} else {
						rec[name] = ""
					}
				}
				if d, ok := docFrom(rec, f); ok {
					if err := emit(d); err != nil {
						_ = rr.Close()
						return err
					}
					emitted++
				}
			}
			if rerr != nil || n == 0 {
				break
			}
		}
		_ = rr.Close()
	}
	if emitted == 0 {
		return fmt.Errorf("adapt: parquet %s: no usable records (columns %v)", path, names)
	}
	return nil
}
