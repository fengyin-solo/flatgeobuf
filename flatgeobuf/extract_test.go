// Copyright 2023 The flatgeobuf (Go) Authors. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package flatgeobuf

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gogama/flatgeobuf/flatgeobuf/flat"
	"github.com/gogama/flatgeobuf/packedrtree"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

// initMockFile locates and initializes one of the package-level mock
// FlatGeobuf files by name.
func initMockFile(t *testing.T, name string) *mockFile {
	t.Helper()
	for i := range mockFiles {
		if mockFiles[i].name == name {
			mf := &mockFiles[i]
			mf.init(t)
			return mf
		}
	}
	t.Fatalf("no such mock FlatGeobuf file: %q", name)
	return nil
}

// buildSourceFile serializes a mock header and feature list into a
// complete FlatGeobuf byte slice, using the production FileWriter so
// the extraction tests exercise genuine interop with the writer.
func buildSourceFile(t *testing.T, mh mockHeader, feats []mockFeature) []byte {
	t.Helper()
	var b bytes.Buffer
	w := NewFileWriter(&b)
	_, err := w.Header(mh.buildAsTable())
	require.NoError(t, err)
	p := make([]flat.Feature, len(feats))
	for i := range feats {
		p[i] = *feats[i].buildAsTable()
	}
	if mh.indexNodeSize != nil && *mh.indexNodeSize > 0 {
		_, err = w.IndexData(p)
	} else {
		_, err = w.Data(p)
	}
	require.NoError(t, err)
	return b.Bytes()
}

// runExtract runs WriteExtract against an in-memory destination and
// requires success.
func runExtract(t *testing.T, src io.Reader, opts ExtractOptions) (ExtractResult, []byte) {
	t.Helper()
	var out bytes.Buffer
	res, err := WriteExtract(src, &out, opts)
	require.NoError(t, err)
	return res, out.Bytes()
}

// readBackHeader opens an extracted byte slice and reads its header.
func readBackHeader(t *testing.T, b []byte) (*FileReader, *flat.Header) {
	t.Helper()
	r := NewFileReader(bytes.NewReader(b))
	hdr, err := r.Header()
	require.NoError(t, err)
	return r, hdr
}

func pointFeature(x, y float64) mockFeature {
	return mockFeature{
		geometry: &mockGeometry{
			xy:           []float64{x, y},
			geometryType: flat.GeometryTypePoint,
		},
	}
}

func TestWriteExtract_Validation(t *testing.T) {
	mf := initMockFile(t, "four_points_in_quadrants")

	t.Run("Nil Reader Panics", func(t *testing.T) {
		assert.PanicsWithValue(t, "flatgeobuf: nil reader", func() {
			_, _ = WriteExtract(nil, &bytes.Buffer{}, ExtractOptions{})
		})
	})

	t.Run("Nil Writer Panics", func(t *testing.T) {
		assert.PanicsWithValue(t, "flatgeobuf: nil writer", func() {
			_, _ = WriteExtract(bytes.NewReader(mf.allBytes), nil, ExtractOptions{})
		})
	})

	t.Run("Negative MaxFeatures", func(t *testing.T) {
		res, err := WriteExtract(bytes.NewReader(mf.allBytes), &bytes.Buffer{}, ExtractOptions{MaxFeatures: -1})

		assert.EqualError(t, err, "flatgeobuf: MaxFeatures must be non-negative, got -1")
		assert.Equal(t, ExtractResult{}, res)
	})

	t.Run("Bad Source Magic", func(t *testing.T) {
		bad := initMockFile(t, "truncated_header")

		res, err := WriteExtract(bytes.NewReader(bad.allBytes), &bytes.Buffer{}, ExtractOptions{})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "failed to read source header")
		assert.Equal(t, int64(0), res.BytesWritten)
	})

	t.Run("Truncated Index", func(t *testing.T) {
		bad := initMockFile(t, "truncated_index")

		res, err := WriteExtract(bytes.NewReader(bad.allBytes), &bytes.Buffer{}, ExtractOptions{})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "truncated source data")
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
		assert.Equal(t, 0, res.Matched)
	})
}

func TestWriteExtract_BBox(t *testing.T) {
	mf := initMockFile(t, "four_points_in_quadrants")
	wantFeatures := mockFeaturesFromFlatBufferTable(mustReadAllFeatures(t, mf.allBytes))

	t.Run("Single Point", func(t *testing.T) {
		opts := ExtractOptions{BBox: &packedrtree.Box{XMin: -1.5, YMin: -1.5, XMax: -0.5, YMax: -0.5}}

		res, out := runExtract(t, bytes.NewReader(mf.allBytes), opts)

		assert.Equal(t, 4, res.Scanned)
		assert.Equal(t, 1, res.Matched)
		assert.False(t, res.Truncated)
		assert.Greater(t, res.BytesWritten, int64(magicLen))

		r, hdr := readBackHeader(t, out)
		assert.Equal(t, uint64(1), hdr.FeaturesCount())
		assert.Equal(t, uint16(2), hdr.IndexNodeSize())
		assert.Equal(t, []float64{-1, -1, -1, -1}, envelopeValues(t, hdr))

		index, err := r.Index()
		require.NoError(t, err)
		assert.Equal(t, 1, index.NumRefs())

		got, err := r.DataRem()
		require.NoError(t, err)
		assert.Equal(t, []mockFeature{wantFeatures[0]}, mockFeaturesFromFlatBufferTable(got))
	})

	t.Run("Half Space In File Order", func(t *testing.T) {
		opts := ExtractOptions{BBox: &packedrtree.Box{XMin: -2, YMin: -2, XMax: 0, YMax: 2}}

		res, out := runExtract(t, bytes.NewReader(mf.allBytes), opts)

		require.Equal(t, 2, res.Matched)
		assert.Equal(t, 4, res.Scanned)

		r, hdr := readBackHeader(t, out)
		assert.Equal(t, uint64(2), hdr.FeaturesCount())
		assert.Equal(t, []float64{-1, -1, -1, 1}, envelopeValues(t, hdr))

		// The rebuilt index must provide random access to both hits.
		require.NoError(t, r.Rewind())
		hits, err := r.IndexSearch(*opts.BBox)
		require.NoError(t, err)
		assert.Len(t, hits, 2)

		// IndexSearch leaves the reader at EOF, so verify the data
		// section sequentially through a fresh reader.
		r2, _ := readBackHeader(t, out)
		got, err := r2.DataRem()
		require.NoError(t, err)
		assert.Equal(t, []mockFeature{wantFeatures[0], wantFeatures[1]}, mockFeaturesFromFlatBufferTable(got))
	})

	t.Run("Boundary Touch Counts As Intersection", func(t *testing.T) {
		opts := ExtractOptions{BBox: &packedrtree.Box{XMin: -1, YMin: -1, XMax: -1, YMax: -1}}

		res, _ := runExtract(t, bytes.NewReader(mf.allBytes), opts)

		assert.Equal(t, 1, res.Matched)
	})

	t.Run("No Intersection", func(t *testing.T) {
		opts := ExtractOptions{BBox: &packedrtree.Box{XMin: 5, YMin: 5, XMax: 6, YMax: 6}}

		res, out := runExtract(t, bytes.NewReader(mf.allBytes), opts)

		assert.Equal(t, 4, res.Scanned)
		assert.Equal(t, 0, res.Matched)
		assert.False(t, res.Truncated)
		assertLegalEmptyFile(t, out, res)
	})

	t.Run("Not Seekable Source", func(t *testing.T) {
		opts := ExtractOptions{BBox: &packedrtree.Box{XMin: 0, YMin: -2, XMax: 2, YMax: 2}}

		res, out := runExtract(t, &readerOnly{r: bytes.NewReader(mf.allBytes)}, opts)

		assert.Equal(t, 2, res.Matched)
		r, hdr := readBackHeader(t, out)
		assert.Equal(t, uint64(2), hdr.FeaturesCount())
		got, err := r.DataRem()
		require.NoError(t, err)
		assert.Equal(t, []mockFeature{wantFeatures[2], wantFeatures[3]}, mockFeaturesFromFlatBufferTable(got))
	})

	t.Run("Feature Without Geometry Never Matches", func(t *testing.T) {
		src := buildSourceFile(t, mockHeader{
			featuresCount: 1,
			indexNodeSize: uint16Ptr(0),
		}, []mockFeature{{}})
		opts := ExtractOptions{BBox: &packedrtree.Box{XMin: -10, YMin: -10, XMax: 10, YMax: 10}}

		res, out := runExtract(t, bytes.NewReader(src), opts)

		assert.Equal(t, 1, res.Scanned)
		assert.Equal(t, 0, res.Matched)
		assertLegalEmptyFile(t, out, res)
	})
}

func TestWriteExtract_Predicate(t *testing.T) {
	mf := initMockFile(t, "four_points_in_quadrants")
	wantFeatures := mockFeaturesFromFlatBufferTable(mustReadAllFeatures(t, mf.allBytes))

	xGreaterThanZero := func(f *flat.Feature) (bool, error) {
		var g flat.Geometry
		if f.Geometry(&g) == nil {
			return false, nil
		}
		return g.Xy(0) > 0, nil
	}

	t.Run("Custom Predicate", func(t *testing.T) {
		res, out := runExtract(t, bytes.NewReader(mf.allBytes), ExtractOptions{Predicate: xGreaterThanZero})

		assert.Equal(t, 4, res.Scanned)
		assert.Equal(t, 2, res.Matched)

		r, _ := readBackHeader(t, out)
		got, err := r.DataRem()
		require.NoError(t, err)
		assert.Equal(t, []mockFeature{wantFeatures[2], wantFeatures[3]}, mockFeaturesFromFlatBufferTable(got))
	})

	t.Run("BBox And Predicate Combined", func(t *testing.T) {
		opts := ExtractOptions{
			BBox:      &packedrtree.Box{XMin: -2, YMin: -2, XMax: 2, YMax: 2},
			Predicate: xGreaterThanZero,
		}

		res, out := runExtract(t, bytes.NewReader(mf.allBytes), opts)

		assert.Equal(t, 4, res.Scanned)
		assert.Equal(t, 2, res.Matched)
		r, _ := readBackHeader(t, out)
		got, err := r.DataRem()
		require.NoError(t, err)
		assert.Equal(t, []mockFeature{wantFeatures[2], wantFeatures[3]}, mockFeaturesFromFlatBufferTable(got))
	})

	t.Run("Feature Without Geometry Passes Predicate Without BBox", func(t *testing.T) {
		src := buildSourceFile(t, mockHeader{
			featuresCount: 1,
			indexNodeSize: uint16Ptr(0),
		}, []mockFeature{{}})

		res, _ := runExtract(t, bytes.NewReader(src), ExtractOptions{
			Predicate: func(f *flat.Feature) (bool, error) { return true, nil },
		})

		assert.Equal(t, 1, res.Matched)
	})

	t.Run("Predicate Error Aborts Extraction", func(t *testing.T) {
		errSentinel := errors.New("predicate boom")
		var out bytes.Buffer

		res, err := WriteExtract(bytes.NewReader(mf.allBytes), &out, ExtractOptions{
			Predicate: func(f *flat.Feature) (bool, error) { return false, errSentinel },
		})

		require.ErrorIs(t, err, errSentinel)
		assert.Contains(t, err.Error(), "predicate failed on feature")
		assert.Equal(t, 1, res.Scanned)
		assert.Equal(t, 0, res.Matched)
		assert.Equal(t, 0, out.Len(), "nothing must be written before scanning finishes")
	})
}

func TestWriteExtract_NoMatch(t *testing.T) {
	t.Run("No Match Produces Legal Empty File", func(t *testing.T) {
		mf := initMockFile(t, "four_points_in_quadrants")

		res, out := runExtract(t, bytes.NewReader(mf.allBytes), ExtractOptions{
			BBox: &packedrtree.Box{XMin: 50, YMin: 50, XMax: 51, YMax: 51},
		})

		assert.Equal(t, 4, res.Scanned)
		assert.Equal(t, 0, res.Matched)
		assert.False(t, res.Truncated)
		assertLegalEmptyFile(t, out, res)
	})

	t.Run("Empty Source Produces Legal Empty File", func(t *testing.T) {
		mf := initMockFile(t, "empty")

		res, out := runExtract(t, bytes.NewReader(mf.allBytes), ExtractOptions{})

		assert.Equal(t, 0, res.Scanned)
		assert.Equal(t, 0, res.Matched)
		assert.False(t, res.Truncated)
		assertLegalEmptyFile(t, out, res)
	})
}

// assertLegalEmptyFile verifies that an extraction output is a
// well-formed FlatGeobuf file containing zero features and no index.
func assertLegalEmptyFile(t *testing.T, out []byte, res ExtractResult) {
	t.Helper()
	require.Equal(t, int64(len(out)), res.BytesWritten)
	require.Greater(t, len(out), magicLen)

	r, hdr := readBackHeader(t, out)
	assert.Equal(t, uint64(0), hdr.FeaturesCount())
	assert.Equal(t, uint16(0), hdr.IndexNodeSize())

	index, err := r.Index()
	assert.Nil(t, index)
	assert.Same(t, ErrNoIndex, err)

	data, err := r.DataRem()
	assert.NoError(t, err)
	assert.Empty(t, data)
}

func TestWriteExtract_MaxFeatures(t *testing.T) {
	mf := initMockFile(t, "four_points_in_quadrants")
	wantFeatures := mockFeaturesFromFlatBufferTable(mustReadAllFeatures(t, mf.allBytes))

	t.Run("Zero Means Unlimited", func(t *testing.T) {
		res, out := runExtract(t, bytes.NewReader(mf.allBytes), ExtractOptions{MaxFeatures: 0})

		assert.Equal(t, 4, res.Scanned)
		assert.Equal(t, 4, res.Matched)
		assert.False(t, res.Truncated)
		r, _ := readBackHeader(t, out)
		got, err := r.DataRem()
		require.NoError(t, err)
		assert.Len(t, got, 4)
	})

	t.Run("Positive Cap Truncates And Keeps File Order", func(t *testing.T) {
		res, out := runExtract(t, bytes.NewReader(mf.allBytes), ExtractOptions{MaxFeatures: 2})

		assert.True(t, res.Truncated)
		assert.Equal(t, 2, res.Matched)
		assert.Equal(t, 2, res.Scanned, "scanning stops once the cap is reached")

		r, hdr := readBackHeader(t, out)
		assert.Equal(t, uint64(2), hdr.FeaturesCount())
		index, err := r.Index()
		require.NoError(t, err)
		assert.Equal(t, 2, index.NumRefs())

		got, err := r.DataRem()
		require.NoError(t, err)
		assert.Equal(t, []mockFeature{wantFeatures[0], wantFeatures[1]}, mockFeaturesFromFlatBufferTable(got))
	})

	t.Run("Cap Reached At Last Feature Is Not Truncated", func(t *testing.T) {
		res, _ := runExtract(t, bytes.NewReader(mf.allBytes), ExtractOptions{MaxFeatures: 4})

		assert.False(t, res.Truncated)
		assert.Equal(t, 4, res.Matched)
		assert.Equal(t, 4, res.Scanned)
	})

	t.Run("Cap Above Match Count Is Not Truncated", func(t *testing.T) {
		res, _ := runExtract(t, bytes.NewReader(mf.allBytes), ExtractOptions{MaxFeatures: 100})

		assert.False(t, res.Truncated)
		assert.Equal(t, 4, res.Matched)
		assert.Equal(t, 4, res.Scanned)
	})

	t.Run("Cap With Selective Filter", func(t *testing.T) {
		res, out := runExtract(t, bytes.NewReader(mf.allBytes), ExtractOptions{
			MaxFeatures: 1,
			BBox:        &packedrtree.Box{XMin: -1.5, YMin: -1.5, XMax: -0.5, YMax: -0.5},
		})

		assert.True(t, res.Truncated, "unread features remain after the single hit")
		assert.Equal(t, 1, res.Matched)
		assert.Equal(t, 1, res.Scanned)
		_, hdr := readBackHeader(t, out)
		assert.Equal(t, uint64(1), hdr.FeaturesCount())
	})

	t.Run("Cap At Batch Boundary Probes For More Data", func(t *testing.T) {
		// Build a source spanning more than one scan batch so the cap
		// can be reached exactly at a batch boundary, exercising the
		// end-of-stream probe branch.
		feats := make([]mockFeature, 2*extractBatchSize+10)
		for i := range feats {
			feats[i] = pointFeature(float64(i), float64(i))
		}
		src := buildSourceFile(t, mockHeader{
			featuresCount: uint64(len(feats)),
			indexNodeSize: uint16Ptr(16),
		}, feats)

		t.Run("Probe Finds More Features", func(t *testing.T) {
			res, out := runExtract(t, bytes.NewReader(src), ExtractOptions{MaxFeatures: extractBatchSize})

			assert.True(t, res.Truncated)
			assert.Equal(t, extractBatchSize, res.Matched)
			assert.Equal(t, extractBatchSize, res.Scanned)
			_, hdr := readBackHeader(t, out)
			assert.Equal(t, uint64(extractBatchSize), hdr.FeaturesCount())
		})

		t.Run("Probe Finds End Of Stream", func(t *testing.T) {
			res, _ := runExtract(t, bytes.NewReader(src), ExtractOptions{MaxFeatures: len(feats)})

			assert.False(t, res.Truncated)
			assert.Equal(t, len(feats), res.Matched)
			assert.Equal(t, len(feats), res.Scanned)
		})
	})
}

func TestWriteExtract_NoIndexSource(t *testing.T) {
	t.Run("Known Count Without Index", func(t *testing.T) {
		mf := initMockFile(t, "one_feature_no_index")
		want := mockFeaturesFromFlatBufferTable(mustReadAllFeatures(t, mf.allBytes))

		res, out := runExtract(t, bytes.NewReader(mf.allBytes), ExtractOptions{})

		assert.Equal(t, 1, res.Scanned)
		assert.Equal(t, 1, res.Matched)
		r, hdr := readBackHeader(t, out)
		assert.Equal(t, uint64(1), hdr.FeaturesCount())
		assert.Equal(t, uint16(0), hdr.IndexNodeSize())
		index, err := r.Index()
		assert.Nil(t, index)
		assert.Same(t, ErrNoIndex, err)
		got, err := r.DataRem()
		require.NoError(t, err)
		assert.Equal(t, want, mockFeaturesFromFlatBufferTable(got))
	})

	t.Run("Unknown Count Becomes Exact Count", func(t *testing.T) {
		mf := initMockFile(t, "one_feature_no_index_unknown_count")

		res, out := runExtract(t, bytes.NewReader(mf.allBytes), ExtractOptions{})

		assert.Equal(t, 1, res.Scanned)
		assert.Equal(t, 1, res.Matched)
		r, hdr := readBackHeader(t, out)
		assert.Equal(t, uint64(1), hdr.FeaturesCount(), "output records the exact extracted count")
		assert.Equal(t, uint16(0), hdr.IndexNodeSize())
		got, err := r.DataRem()
		require.NoError(t, err)
		assert.Len(t, got, 1)
	})

	t.Run("Not Seekable Source Without Index", func(t *testing.T) {
		mf := initMockFile(t, "one_feature_no_index")

		res, out := runExtract(t, &readerOnly{r: bytes.NewReader(mf.allBytes)}, ExtractOptions{})

		assert.Equal(t, 1, res.Matched)
		r, hdr := readBackHeader(t, out)
		assert.Equal(t, uint16(0), hdr.IndexNodeSize())
		got, err := r.DataRem()
		require.NoError(t, err)
		assert.Len(t, got, 1)
	})
}

func TestWriteExtract_HeaderMetadata(t *testing.T) {
	columns := []mockColumn{
		{name: "c0", columnType: flat.ColumnTypeString, nullable: true},
		{name: "c1", columnType: flat.ColumnTypeInt, nullable: false},
	}
	mh := mockHeader{
		name:          stringPtr("extract-src"),
		envelope:      []float64{-100, -100, 100, 100}, // Deliberately stale.
		geometryType:  flat.GeometryTypePoint,
		hasZ:          true,
		hasM:          true,
		hasT:          false,
		hasTM:         true,
		columns:       columns,
		featuresCount: 2,
		indexNodeSize: uint16Ptr(2),
		crs: &mockCRS{
			org:         stringPtr("EPSG"),
			code:        4326,
			name:        stringPtr("WGS 84"),
			description: stringPtr("World Geodetic System 1984"),
			wkt:         stringPtr("GEOGCS[...]"),
			codeString:  stringPtr("4326"),
		},
		title:       stringPtr("Extract Title"),
		description: stringPtr("Extract Description"),
		metadata:    stringPtr(`{"purpose":"test"}`),
	}
	feats := []mockFeature{pointFeature(0, 0), pointFeature(2, 2)}
	src := buildSourceFile(t, mh, feats)

	// Read the source header to establish the expected metadata.
	srcReader := NewFileReader(bytes.NewReader(src))
	srcHeader, err := srcReader.Header()
	require.NoError(t, err)
	want := mockHeaderFromFlatBufferTable(srcHeader)

	res, out := runExtract(t, bytes.NewReader(src), ExtractOptions{})
	require.Equal(t, 2, res.Matched)

	r, hdr := readBackHeader(t, out)
	got := mockHeaderFromFlatBufferTable(hdr)

	assert.Equal(t, want.name, got.name)
	assert.Equal(t, want.title, got.title)
	assert.Equal(t, want.description, got.description)
	assert.Equal(t, want.metadata, got.metadata)
	assert.Equal(t, want.crs, got.crs)
	assert.Equal(t, want.columns, got.columns)
	assert.Equal(t, want.geometryType, got.geometryType)
	assert.Equal(t, want.hasZ, got.hasZ)
	assert.Equal(t, want.hasM, got.hasM)
	assert.Equal(t, want.hasT, got.hasT)
	assert.Equal(t, want.hasTM, got.hasTM)
	assert.Equal(t, uint64(2), hdr.FeaturesCount())
	assert.Equal(t, uint16(2), hdr.IndexNodeSize())

	// The envelope is recomputed from the selected features rather than
	// copied from the stale source envelope.
	assert.Equal(t, []float64{0, 0, 2, 2}, envelopeValues(t, hdr))

	index, err := r.Index()
	require.NoError(t, err)
	assert.Equal(t, 2, index.NumRefs())
	assert.Equal(t, uint16(2), index.NodeSize())

	gotData, err := r.DataRem()
	require.NoError(t, err)
	assert.Equal(t, feats, mockFeaturesFromFlatBufferTable(gotData))
}

func TestWriteExtract_PropertySchema(t *testing.T) {
	columns := []mockColumn{
		{name: "name", columnType: flat.ColumnTypeString},
		{name: "age", columnType: flat.ColumnTypeShort},
	}
	makeProps := func(name string, age int16) []byte {
		var b bytes.Buffer
		pw := NewPropWriter(&b)
		_, err := pw.WriteUShort(0)
		require.NoError(t, err)
		_, err = pw.WriteString(name)
		require.NoError(t, err)
		_, err = pw.WriteUShort(1)
		require.NoError(t, err)
		_, err = pw.WriteShort(age)
		require.NoError(t, err)
		return b.Bytes()
	}
	feats := []mockFeature{
		{geometry: pointFeature(-1, -1).geometry, properties: makeProps("alice", 30)},
		{geometry: pointFeature(1, 1).geometry, properties: makeProps("bob", 41)},
	}
	mh := mockHeader{
		columns:       columns,
		featuresCount: 2,
		indexNodeSize: uint16Ptr(2),
	}
	src := buildSourceFile(t, mh, feats)

	res, out := runExtract(t, bytes.NewReader(src), ExtractOptions{
		BBox: &packedrtree.Box{XMin: -2, YMin: -2, XMax: 0, YMax: 0},
	})
	require.Equal(t, 1, res.Matched)

	r, hdr := readBackHeader(t, out)

	// The full column schema must be copied, even though only one
	// feature survived extraction.
	require.Equal(t, len(columns), hdr.ColumnsLength())
	var col flat.Column
	require.True(t, hdr.Columns(&col, 0))
	assert.Equal(t, "name", string(col.Name()))
	assert.Equal(t, flat.ColumnTypeString, col.Type())
	require.True(t, hdr.Columns(&col, 1))
	assert.Equal(t, "age", string(col.Name()))
	assert.Equal(t, flat.ColumnTypeShort, col.Type())

	// The surviving feature's properties must still decode against the
	// copied schema.
	data, err := r.DataRem()
	require.NoError(t, err)
	require.Len(t, data, 1)
	assert.Equal(t, feats[0].properties, data[0].PropertiesBytes())
	vals, err := NewPropReader(bytes.NewReader(data[0].PropertiesBytes())).ReadSchema(hdr)
	require.NoError(t, err)
	require.Len(t, vals, 2)
	assert.Equal(t, "name", string(vals[0].Col.Name()))
	assert.Equal(t, "alice", vals[0].Value)
	assert.Equal(t, "age", string(vals[1].Col.Name()))
	assert.Equal(t, int16(30), vals[1].Value)
}

func TestWriteExtract_TruncatedInput(t *testing.T) {
	t.Run("Truncated Data Section Is An Error", func(t *testing.T) {
		for _, seekable := range []bool{true, false} {
			name := "Seekable"
			var src io.Reader
			mf := initMockFile(t, "truncated_data")
			if seekable {
				src = bytes.NewReader(mf.allBytes)
			} else {
				name = "Not Seekable"
				src = &readerOnly{r: bytes.NewReader(mf.allBytes)}
			}
			t.Run(name, func(t *testing.T) {
				res, err := WriteExtract(src, &bytes.Buffer{}, ExtractOptions{})

				require.Error(t, err)
				assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
				assert.Contains(t, err.Error(), "truncated source data")
				assert.Equal(t, 0, res.Matched)
			})
		}
	})

	t.Run("Mid Stream Read Error Is An Error", func(t *testing.T) {
		mf := initMockFile(t, "four_points_in_quadrants")
		errSentinel := errors.New("disk on fire")
		src := newMockDataBytesReader(t, mf, &mockDataReadError{
			pos: mf.dataOffsets[0] + 7,
			err: errSentinel,
		})

		res, err := WriteExtract(src, &bytes.Buffer{}, ExtractOptions{})

		require.ErrorIs(t, err, errSentinel)
		assert.NotEqual(t, 4, res.Matched)
	})
}

func TestWriteExtract_WriteFailure(t *testing.T) {
	errSentinel := errors.New("tape snapped")

	t.Run("Magic Write Failure", func(t *testing.T) {
		mf := initMockFile(t, "four_points_in_quadrants")
		m := newMockWriteCloser(t)
		m.On("Write", mock.Anything).Return(0, errSentinel).Once()

		res, err := WriteExtract(bytes.NewReader(mf.allBytes), m, ExtractOptions{})

		require.ErrorIs(t, err, errSentinel)
		assert.Equal(t, int64(0), res.BytesWritten)
		m.AssertExpectations(t)
	})

	t.Run("Index Write Failure Is Not Success", func(t *testing.T) {
		mf := initMockFile(t, "four_points_in_quadrants")
		m := newMockWriteCloser(t)
		m.On("Write", mock.MatchedBy(func(p []byte) bool {
			return bytes.Equal(p, magic[:])
		})).Return(magicLen, nil).Once()
		headerCall := m.On("Write", mock.Anything).Once()
		headerCall.RunFn = func(args mock.Arguments) {
			p := args[0].([]byte)
			headerCall.ReturnArguments = mock.Arguments{len(p), nil}
		}
		m.On("Write", mock.Anything).Return(0, errSentinel).Once()

		res, err := WriteExtract(bytes.NewReader(mf.allBytes), m, ExtractOptions{
			BBox: &packedrtree.Box{XMin: -2, YMin: -2, XMax: 2, YMax: 2},
		})

		require.ErrorIs(t, err, errSentinel)
		assert.Contains(t, err.Error(), "failed to write output index")
		assert.Greater(t, res.BytesWritten, int64(magicLen), "partial byte count is still reported")
		assert.Equal(t, 4, res.Matched, "scanning completed before writing failed")
		m.AssertExpectations(t)
	})

	t.Run("Data Write Failure Without Index", func(t *testing.T) {
		mf := initMockFile(t, "one_feature_no_index")
		m := newMockWriteCloser(t)
		m.On("Write", mock.MatchedBy(func(p []byte) bool {
			return bytes.Equal(p, magic[:])
		})).Return(magicLen, nil).Once()
		headerCall := m.On("Write", mock.Anything).Once()
		headerCall.RunFn = func(args mock.Arguments) {
			p := args[0].([]byte)
			headerCall.ReturnArguments = mock.Arguments{len(p), nil}
		}
		m.On("Write", mock.Anything).Return(0, errSentinel).Once()

		res, err := WriteExtract(bytes.NewReader(mf.allBytes), m, ExtractOptions{})

		require.ErrorIs(t, err, errSentinel)
		assert.Greater(t, res.BytesWritten, int64(magicLen))
		m.AssertExpectations(t)
	})
}

func TestWriteExtract_RealData(t *testing.T) {
	for _, seekable := range []bool{true, false} {
		name := "Seekable"
		if !seekable {
			name = "Not Seekable"
		}
		t.Run(name, func(t *testing.T) {
			// Populate the shared test-data cache, then take a private
			// copy so every sub-test re-opens an untouched source.
			_ = newTestDataBytesReader(t, true, "countries.fgb")
			srcBytes := append([]byte(nil), testDataByteMap["countries.fgb"]...)
			require.NotEmpty(t, srcBytes)

			newSource := func() io.Reader {
				if seekable {
					return bytes.NewReader(srcBytes)
				}
				return &readerOnly{r: bytes.NewReader(srcBytes)}
			}

			// Establish ground truth from the source file.
			srcReader := NewFileReader(newSource())
			srcHeader, err := srcReader.Header()
			require.NoError(t, err)
			srcFeatures, err := srcReader.DataRem()
			require.NoError(t, err)
			total := int(srcHeader.FeaturesCount())
			require.Len(t, srcFeatures, total)

			t.Run("Full Extraction Round Trip", func(t *testing.T) {
				res, out := runExtract(t, newSource(), ExtractOptions{})

				assert.Equal(t, total, res.Scanned)
				assert.Equal(t, total, res.Matched)
				assert.False(t, res.Truncated)
				assert.Equal(t, int64(len(out)), res.BytesWritten)

				r, hdr := readBackHeader(t, out)
				assert.Equal(t, srcHeader.FeaturesCount(), hdr.FeaturesCount())
				assert.Equal(t, srcHeader.IndexNodeSize(), hdr.IndexNodeSize())
				assert.Equal(t, string(srcHeader.Name()), string(hdr.Name()))
				assert.Equal(t, srcHeader.ColumnsLength(), hdr.ColumnsLength())
				var srcCRS, outCRS flat.Crs
				require.NotNil(t, srcHeader.Crs(&srcCRS))
				require.NotNil(t, hdr.Crs(&outCRS))
				assert.Equal(t, srcCRS.Code(), outCRS.Code())
				assert.Equal(t, string(srcCRS.Org()), string(outCRS.Org()))

				index, err := r.Index()
				require.NoError(t, err)
				assert.Equal(t, total, index.NumRefs())

				got, err := r.DataRem()
				require.NoError(t, err)
				assert.Equal(t, mockFeaturesFromFlatBufferTable(srcFeatures), mockFeaturesFromFlatBufferTable(got))
			})

			t.Run("BBox Extraction Is Independently Searchable", func(t *testing.T) {
				// Derive the query from a real source feature's own
				// geometry bounds, guaranteeing at least one true
				// feature-level (not merely index-level) hit.
				const anchor = 5
				query, ok, err := extractFeatureBox(&srcFeatures[anchor])
				require.NoError(t, err)
				require.True(t, ok)
				anchorProps := srcFeatures[anchor].PropertiesBytes()

				res, out := runExtract(t, newSource(), ExtractOptions{BBox: &query})

				require.Greater(t, res.Matched, 0)
				assert.Equal(t, total, res.Scanned)
				assert.False(t, res.Truncated)

				r, hdr := readBackHeader(t, out)
				assert.Equal(t, uint64(res.Matched), hdr.FeaturesCount())
				assert.Equal(t, srcHeader.IndexNodeSize(), hdr.IndexNodeSize())

				// Random access via the rebuilt index must find every
				// extracted feature. IndexSearch returns the matched
				// features themselves and leaves the reader at EOF.
				require.NoError(t, r.Rewind())
				hits, err := r.IndexSearch(query)
				require.NoError(t, err)
				assert.Len(t, hits, res.Matched)

				foundAnchor := false
				for i := range hits {
					b, ok, err := extractFeatureBox(&hits[i])
					require.NoError(t, err)
					require.True(t, ok)
					assert.True(t, boxesIntersect(&b, &query), "feature %d outside query box", i)
					if bytes.Equal(hits[i].PropertiesBytes(), anchorProps) {
						foundAnchor = true
					}
				}
				assert.True(t, foundAnchor, "the anchor source feature must survive extraction")

				// A fresh sequential read must yield the same count.
				r2, _ := readBackHeader(t, out)
				got, err := r2.DataRem()
				require.NoError(t, err)
				assert.Len(t, got, res.Matched)
			})
		})
	}
}

// mustReadAllFeatures reads the complete data section of an in-memory
// FlatGeobuf file, failing the test on error.
func mustReadAllFeatures(t *testing.T, src []byte) []flat.Feature {
	t.Helper()
	r := NewFileReader(bytes.NewReader(src))
	_, err := r.Header()
	require.NoError(t, err)
	fs, err := r.DataRem()
	require.NoError(t, err)
	return fs
}

// envelopeValues reads the header envelope as a float slice, returning
// nil when the header carries no envelope.
func envelopeValues(t *testing.T, hdr *flat.Header) []float64 {
	t.Helper()
	n := hdr.EnvelopeLength()
	if n == 0 {
		return nil
	}
	v := make([]float64, n)
	for i := range v {
		v[i] = hdr.Envelope(i)
	}
	return v
}
