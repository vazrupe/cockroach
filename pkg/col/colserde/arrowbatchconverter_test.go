// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package colserde

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/apache/arrow/go/arrow/array"
	"github.com/cockroachdb/cockroach/pkg/col/coldata"
	"github.com/cockroachdb/cockroach/pkg/col/coltypes"
	"github.com/cockroachdb/cockroach/pkg/sql/exec"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
	"github.com/stretchr/testify/require"
)

func randomBatch() ([]coltypes.T, coldata.Batch) {
	const maxTyps = 16
	rng, _ := randutil.NewPseudoRand()

	availableTyps := make([]coltypes.T, 0, len(coltypes.AllTypes))
	for _, typ := range coltypes.AllTypes {
		// TODO(asubiotto): We do not support decimal conversion yet.
		if typ == coltypes.Decimal {
			continue
		}
		availableTyps = append(availableTyps, typ)
	}
	typs := make([]coltypes.T, rng.Intn(maxTyps)+1)
	for i := range typs {
		typs[i] = availableTyps[rng.Intn(len(availableTyps))]
	}

	b := exec.RandomBatch(rng, typs, rng.Intn(coldata.BatchSize)+1, rng.Float64())
	return typs, b
}

func copyBatch(original coldata.Batch) coldata.Batch {
	typs := make([]coltypes.T, original.Width())
	for i, vec := range original.ColVecs() {
		typs[i] = vec.Type()
	}
	b := coldata.NewMemBatchWithSize(typs, int(original.Length()))
	b.SetLength(original.Length())
	for colIdx, col := range original.ColVecs() {
		b.ColVec(colIdx).Copy(coldata.CopyArgs{
			ColType:   typs[colIdx],
			Src:       col,
			SrcEndIdx: uint64(original.Length()),
		})
	}
	return b
}

func assertEqualBatches(t *testing.T, expected, actual coldata.Batch) {
	t.Helper()

	if actual.Selection() != nil {
		t.Fatal("violated invariant that batches have no selection vectors")
	}
	require.Equal(t, expected.Length(), actual.Length())
	require.Equal(t, expected.Width(), actual.Width())
	for colIdx := 0; colIdx < expected.Width(); colIdx++ {
		// Verify equality of ColVecs (this includes nulls). Since the coldata.Vec
		// backing array is always of coldata.BatchSize due to the scratch batch
		// that the converter keeps around, the coldata.Vec needs to be sliced to
		// the first length elements to match on length, otherwise the check will
		// fail.
		expectedVec := expected.ColVec(colIdx)
		actualVec := actual.ColVec(colIdx)
		require.Equal(
			t,
			expectedVec.Slice(expectedVec.Type(), 0, uint64(expected.Length())),
			actualVec.Slice(actualVec.Type(), 0, uint64(actual.Length())),
		)
	}
}

func TestArrowBatchConverterRejectsUnsupportedTypes(t *testing.T) {
	defer leaktest.AfterTest(t)()

	typs := []coltypes.T{coltypes.Decimal}
	_, err := NewArrowBatchConverter(typs)
	require.Error(t, err)
}

func TestArrowBatchConverterRandom(t *testing.T) {
	defer leaktest.AfterTest(t)()

	typs, b := randomBatch()
	c, err := NewArrowBatchConverter(typs)
	require.NoError(t, err)

	// Make a copy of the original batch because the converter modifies and casts
	// data without copying for performance reasons.
	expected := copyBatch(b)

	arrowData, err := c.BatchToArrow(b)
	require.NoError(t, err)
	actual := coldata.NewMemBatchWithSize(nil, 0)
	require.NoError(t, c.ArrowToBatch(arrowData, actual))

	assertEqualBatches(t, expected, actual)
}

func TestRecordBatchRoundtripThroughBytes(t *testing.T) {
	defer leaktest.AfterTest(t)()

	typs, b := randomBatch()
	c, err := NewArrowBatchConverter(typs)
	require.NoError(t, err)
	r, err := NewRecordBatchSerializer(typs)
	require.NoError(t, err)

	// Make a copy of the original batch because the converter modifies and casts
	// data without copying for performance reasons.
	expected := copyBatch(b)

	var buf bytes.Buffer
	arrowDataIn, err := c.BatchToArrow(b)
	require.NoError(t, err)
	_, _, err = r.Serialize(&buf, arrowDataIn)
	require.NoError(t, err)

	var arrowDataOut []*array.Data
	require.NoError(t, r.Deserialize(&arrowDataOut, buf.Bytes()))
	actual := coldata.NewMemBatchWithSize(nil, 0)
	require.NoError(t, c.ArrowToBatch(arrowDataOut, actual))

	assertEqualBatches(t, expected, actual)
}

func BenchmarkArrowBatchConverter(b *testing.B) {
	// fixedLen specifies how many bytes we should fit variable length data types
	// to in order to reduce benchmark noise.
	const fixedLen = 64

	rng, _ := randutil.NewPseudoRand()

	typs := []coltypes.T{coltypes.Bool, coltypes.Bytes, coltypes.Int64}
	// numBytes corresponds 1:1 to typs and specifies how many bytes we are
	// converting on one iteration of the benchmark for the corresponding type in
	// typs.
	numBytes := []int64{coldata.BatchSize, fixedLen * coldata.BatchSize, 8 * coldata.BatchSize}
	// Run a benchmark on every type we care about.
	for typIdx, typ := range typs {
		batch := exec.RandomBatch(rng, []coltypes.T{typ}, coldata.BatchSize, 0 /* nullProbability */)
		if batch.Width() != 1 {
			b.Fatalf("unexpected batch width: %d", batch.Width())
		}
		if typ == coltypes.Bytes {
			// This type has variable length elements, fit all of them to be fixedLen
			// bytes long.
			bytes := batch.ColVec(0).Bytes()
			for i := 0; i < bytes.Len(); i++ {
				diff := len(bytes.Get(i)) - fixedLen
				if diff < 0 {
					bytes.Set(i, append(bytes.Get(i), make([]byte, -diff)...))
				} else if diff > 0 {
					bytes.Set(i, bytes.Get(i)[:fixedLen])
				}
			}
		}
		c, err := NewArrowBatchConverter([]coltypes.T{typ})
		require.NoError(b, err)
		nullFractions := []float64{0, 0.25, 0.5}
		setNullFraction := func(batch coldata.Batch, nullFraction float64) {
			vec := batch.ColVec(0)
			vec.Nulls().UnsetNulls()
			numNulls := uint16(int(nullFraction * float64(batch.Length())))
			// Set the first numNulls elements to null.
			for i := uint16(0); i < batch.Length() && i < numNulls; i++ {
				vec.Nulls().SetNull(i)
			}
		}
		for _, nullFraction := range nullFractions {
			setNullFraction(batch, nullFraction)
			testPrefix := fmt.Sprintf("%s/nullFraction=%0.2f", typ.String(), nullFraction)
			var data []*array.Data
			b.Run(testPrefix+"/BatchToArrow", func(b *testing.B) {
				b.SetBytes(numBytes[typIdx])
				for i := 0; i < b.N; i++ {
					data, _ = c.BatchToArrow(batch)
					if len(data) != 1 {
						b.Fatal("expected arrow batch of length 1")
					}
					if data[0].Len() != coldata.BatchSize {
						b.Fatal("unexpected number of elements")
					}
				}
			})
		}
		for _, nullFraction := range nullFractions {
			setNullFraction(batch, nullFraction)
			data, err := c.BatchToArrow(batch)
			require.NoError(b, err)
			testPrefix := fmt.Sprintf("%s/nullFraction=%0.2f", typ.String(), nullFraction)
			result := coldata.NewMemBatch(typs)
			b.Run(testPrefix+"/ArrowToBatch", func(b *testing.B) {
				b.SetBytes(numBytes[typIdx])
				for i := 0; i < b.N; i++ {
					// Using require.NoError here causes large enough allocations to
					// affect the result.
					if err := c.ArrowToBatch(data, result); err != nil {
						b.Fatal(err)
					}
					if result.Width() != 1 {
						b.Fatal("expected one column")
					}
					if result.Length() != coldata.BatchSize {
						b.Fatal("unexpected number of elements")
					}
				}
			})
		}
	}
}
