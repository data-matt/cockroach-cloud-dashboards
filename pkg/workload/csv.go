// Copyright 2018 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package workload

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"unsafe"

	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/bufalloc"
	"github.com/cockroachdb/cockroach/pkg/util/encoding/csv"
	"github.com/cockroachdb/errors"
	"github.com/spf13/pflag"
)

const (
	rowStartParam = `row-start`
	rowEndParam   = `row-end`
)

// WriteCSVRows writes the specified table rows as a csv. If sizeBytesLimit is >
// 0, it will be used as an approximate upper bound for how much to write. The
// next rowStart is returned (so last row written + 1).
func WriteCSVRows(
	ctx context.Context, w io.Writer, table Table, rowStart, rowEnd int, sizeBytesLimit int64,
) (rowBatchIdx int, err error) {
	cb := coldata.NewMemBatchWithCapacity(nil /* typs */, 0 /* capacity */, coldata.StandardColumnFactory)
	var a bufalloc.ByteAllocator

	bytesWrittenW := &bytesWrittenWriter{w: w}
	csvW := csv.NewWriter(bytesWrittenW)
	var rowStrings []string
	for rowBatchIdx = rowStart; rowBatchIdx < rowEnd; rowBatchIdx++ {
		if sizeBytesLimit > 0 && bytesWrittenW.written > sizeBytesLimit {
			break
		}

		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		default:
		}
		a = a.Truncate()
		table.InitialRows.FillBatch(rowBatchIdx, cb, &a)
		if numCols := cb.Width(); cap(rowStrings) < numCols {
			rowStrings = make([]string, numCols)
		} else {
			rowStrings = rowStrings[:numCols]
		}
		for rowIdx, numRows := 0, cb.Length(); rowIdx < numRows; rowIdx++ {
			for colIdx, col := range cb.ColVecs() {
				rowStrings[colIdx] = colDatumToCSVString(col, rowIdx)
			}
			if err := csvW.Write(rowStrings); err != nil {
				return 0, err
			}
		}
	}
	csvW.Flush()
	return rowBatchIdx, csvW.Error()
}

func colDatumToCSVString(col *coldata.Vec, rowIdx int) string {
	if col.Nulls().NullAt(rowIdx) {
		return `NULL`
	}
	switch col.CanonicalTypeFamily() {
	case types.BoolFamily:
		return strconv.FormatBool(col.Bool()[rowIdx])
	case types.IntFamily:
		return strconv.FormatInt(col.Int64()[rowIdx], 10)
	case types.FloatFamily:
		return strconv.FormatFloat(col.Float64()[rowIdx], 'f', -1, 64)
	case types.BytesFamily:
		// See the HACK comment in ColBatchToRows.
		bytes := col.Bytes().Get(rowIdx)
		return *(*string)(unsafe.Pointer(&bytes))
	case types.TimestampTZFamily:
		return col.Timestamp()[rowIdx].Format(timestampOutputFormat)
	case types.DecimalFamily:
		return col.Decimal()[rowIdx].String()
	}
	panic(fmt.Sprintf(`unhandled type %s`, col.Type()))
}

// HandleCSV configures a Generator with url params and outputs the data for a
// single Table as a CSV (optionally limiting the rows via `row-start` and
// `row-end` params). It is intended for use in implementing a
// `net/http.Handler`.
func HandleCSV(w http.ResponseWriter, req *http.Request, prefix string, meta Meta) error {
	ctx := context.Background()
	if err := req.ParseForm(); err != nil {
		return err
	}

	gen := meta.New()
	if f, ok := gen.(Flagser); ok {
		var flags []string
		f.Flags().VisitAll(func(f *pflag.Flag) {
			if vals, ok := req.Form[f.Name]; ok {
				for _, val := range vals {
					flags = append(flags, fmt.Sprintf(`--%s=%s`, f.Name, val))
				}
			}
		})
		if err := f.Flags().Parse(flags); err != nil {
			return errors.Wrapf(err, `parsing parameters %s`, strings.Join(flags, ` `))
		}
	}

	tableName := strings.TrimPrefix(req.URL.Path, prefix)
	var table *Table
	for _, t := range gen.Tables() {
		if t.Name == tableName {
			table = &t
			break
		}
	}
	if table == nil {
		return errors.Errorf(`could not find table %s in generator %s`, tableName, meta.Name)
	}
	if table.InitialRows.FillBatch == nil {
		return errors.Errorf(`csv-server is not supported for workload %s`, meta.Name)
	}

	rowStart, rowEnd := 0, table.InitialRows.NumBatches
	if vals, ok := req.Form[rowStartParam]; ok && len(vals) > 0 {
		var err error
		rowStart, err = strconv.Atoi(vals[len(vals)-1])
		if err != nil {
			return errors.Wrapf(err, `parsing %s`, rowStartParam)
		}
	}
	if vals, ok := req.Form[rowEndParam]; ok && len(vals) > 0 {
		var err error
		rowEnd, err = strconv.Atoi(vals[len(vals)-1])
		if err != nil {
			return errors.Wrapf(err, `parsing %s`, rowEndParam)
		}
	}

	w.Header().Set(`Content-Type`, `text/csv`)
	_, err := WriteCSVRows(ctx, w, *table, rowStart, rowEnd, -1 /* sizeBytesLimit */)
	return err
}

type bytesWrittenWriter struct {
	w       io.Writer
	written int64
}

func (w *bytesWrittenWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	w.written += int64(n)
	return n, err
}

// CSVMux returns a mux over http handlers for csv data in all tables in the
// given generators.
func CSVMux(metas []Meta) *http.ServeMux {
	mux := http.NewServeMux()
	for _, meta := range metas {
		meta := meta
		prefix := fmt.Sprintf(`/csv/%s/`, meta.Name)
		mux.HandleFunc(prefix, func(w http.ResponseWriter, req *http.Request) {
			if err := HandleCSV(w, req, prefix, meta); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		})
	}
	return mux
}
