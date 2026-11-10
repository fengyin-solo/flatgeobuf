// Copyright 2023 The flatgeobuf (Go) Authors. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package flatgeobuf

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/gogama/flatgeobuf/flatgeobuf/flat"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inspectProps builds a FlatGeobuf properties buffer using the
// production PropWriter.
func inspectProps(f func(pw *PropWriter)) []byte {
	var b bytes.Buffer
	pw := NewPropWriter(&b)
	f(pw)
	return b.Bytes()
}

// buildInspectFile serializes a header and features into a complete,
// index-free FlatGeobuf byte stream.
func buildInspectFile(t *testing.T, hdr mockHeader, feats []mockFeature) *bytes.Reader {
	t.Helper()
	if hdr.indexNodeSize == nil {
		hdr.indexNodeSize = uint16Ptr(0)
	}
	if hdr.featuresCount == 0 {
		hdr.featuresCount = uint64(len(feats))
	}
	var b bytes.Buffer
	_, err := b.Write(magic[:])
	require.NoError(t, err)
	_, err = b.Write(hdr.buildAsBytes())
	require.NoError(t, err)
	for i := range feats {
		_, err = b.Write(feats[i].buildAsBytes())
		require.NoError(t, err)
	}
	return bytes.NewReader(b.Bytes())
}

func pointFeature(t *testing.T, props []byte) mockFeature {
	return mockFeature{
		geometry: &mockGeometry{
			geometryType: flat.GeometryTypePoint,
			xy:           []float64{1, 2},
		},
		properties: props,
	}
}

func findProfile(t *testing.T, inv *FeatureInventory, name string) ColumnProfile {
	t.Helper()
	for _, c := range inv.Columns {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("column profile %q not found in inventory", name)
	return ColumnProfile{}
}

func TestInspectFeatures_InvalidOptionsAndInput(t *testing.T) {
	t.Run("Nil Reader", func(t *testing.T) {
		inv, err := InspectFeatures(nil, InspectOptions{})

		require.Error(t, err)
		require.NotNil(t, inv)
		assert.EqualError(t, err, "flatgeobuf: nil reader")
		assert.False(t, inv.Truncated)
	})

	t.Run("Empty Stream", func(t *testing.T) {
		// A structured, non-nil inventory must accompany the error so
		// callers never have to nil-check the result before inspecting
		// whatever partial state exists.
		inv, err := InspectFeatures(bytes.NewReader(nil), InspectOptions{})

		require.Error(t, err)
		require.NotNil(t, inv)
		assert.False(t, inv.Truncated)
		assert.Zero(t, inv.Features)
		assert.NotNil(t, inv.HeaderColumns)
		assert.NotNil(t, inv.Columns)
	})

	t.Run("Negative MaxFeatures", func(t *testing.T) {
		r := buildInspectFile(t, mockHeader{}, nil)

		inv, err := InspectFeatures(r, InspectOptions{MaxFeatures: -1})

		require.Error(t, err)
		require.NotNil(t, inv)
		assert.EqualError(t, err, "flatgeobuf: InspectOptions.MaxFeatures must not be negative (got -1)")
		assert.Zero(t, inv.Features, "invalid options must not consume the stream")
	})

	t.Run("Negative MaxValueBytes", func(t *testing.T) {
		r := buildInspectFile(t, mockHeader{}, nil)

		inv, err := InspectFeatures(r, InspectOptions{MaxValueBytes: -42})

		require.Error(t, err)
		require.NotNil(t, inv)
		assert.EqualError(t, err, "flatgeobuf: InspectOptions.MaxValueBytes must not be negative (got -42)")
		assert.Zero(t, inv.Features, "invalid options must not consume the stream")
	})
}

func TestInspectFeatures_UniformSchema(t *testing.T) {
	hdr := mockHeader{
		geometryType: flat.GeometryTypePoint,
		columns: []mockColumn{
			{name: "id", columnType: flat.ColumnTypeInt},
			{name: "label", columnType: flat.ColumnTypeString},
			{name: "active", columnType: flat.ColumnTypeBool},
		},
	}
	feats := []mockFeature{
		pointFeature(t, inspectProps(func(pw *PropWriter) {
			pw.WriteUShort(0)
			pw.WriteInt(42)
			pw.WriteUShort(1)
			pw.WriteString("foo")
			pw.WriteUShort(2)
			pw.WriteBool(true)
		})),
		pointFeature(t, inspectProps(func(pw *PropWriter) {
			pw.WriteUShort(0)
			pw.WriteInt(7)
			pw.WriteUShort(1)
			pw.WriteString("") // Empty value is still a present value.
		})),
		pointFeature(t, nil), // No properties at all.
	}
	r := buildInspectFile(t, hdr, feats)

	inv, err := InspectFeatures(r, InspectOptions{})

	require.NoError(t, err)
	require.NotNil(t, inv)
	assert.False(t, inv.Truncated)
	assert.Equal(t, flat.GeometryTypePoint, inv.HeaderGeometryType)
	require.Len(t, inv.HeaderColumns, 3)
	assert.Equal(t, HeaderColumn{Index: 0, Name: "id", Type: flat.ColumnTypeInt}, inv.HeaderColumns[0])
	assert.Equal(t, 3, inv.Features)
	assert.Zero(t, inv.EmptyGeometries)
	assert.Zero(t, inv.FeatureSchemas)
	assert.Equal(t, []GeometryTypeCount{{Type: flat.GeometryTypePoint, Count: 3}}, inv.GeometryTypes)
	// 4+7+1 + 4+4 = 20 encoded value bytes.
	assert.Equal(t, int64(20), inv.ValueBytes)

	// All slice fields are non-nil for deterministic serialization.
	assert.NotNil(t, inv.Columns)
	assert.NotNil(t, inv.ExtraColumns)
	assert.NotNil(t, inv.MissingColumns)
	assert.Empty(t, inv.ExtraColumns)

	// Profiles are sorted by canonical name.
	require.Len(t, inv.Columns, 3)
	assert.Equal(t, []string{"active", "id", "label"}, []string{
		inv.Columns[0].Name, inv.Columns[1].Name, inv.Columns[2].Name,
	})

	active := findProfile(t, inv, "active")
	assert.Equal(t, 1, active.Count)
	assert.Equal(t, 2, active.Missing)
	assert.Equal(t, []flat.ColumnType{flat.ColumnTypeBool}, active.ObservedTypes)
	assert.True(t, active.InHeader)
	assert.False(t, active.InFeatureSchema)

	id := findProfile(t, inv, "id")
	assert.Equal(t, 2, id.Count)
	assert.Equal(t, 1, id.Missing)
	assert.Equal(t, []flat.ColumnType{flat.ColumnTypeInt}, id.ObservedTypes)

	label := findProfile(t, inv, "label")
	assert.Equal(t, 2, label.Count, "empty string still counts as an observed value")
	assert.Equal(t, 1, label.Missing)

	// Every header column is missing at least once in this fixture.
	require.Len(t, inv.MissingColumns, 3)
	assert.Equal(t, []string{"active", "id", "label"}, []string{
		inv.MissingColumns[0].Name, inv.MissingColumns[1].Name, inv.MissingColumns[2].Name,
	})
}

func TestInspectFeatures_EmptyAndMismatchedGeometry(t *testing.T) {
	hdr := mockHeader{
		geometryType: flat.GeometryTypePolygon, // Deliberately mismatched.
	}
	feats := []mockFeature{
		{}, // No geometry table at all.
		{
			geometry: &mockGeometry{
				geometryType: flat.GeometryTypePoint,
				xy:           []float64{1, 2},
			},
		},
		{
			geometry: &mockGeometry{
				geometryType: flat.GeometryTypePolygon, // Table present, no coordinates.
			},
		},
	}
	r := buildInspectFile(t, hdr, feats)

	inv, err := InspectFeatures(r, InspectOptions{})

	require.NoError(t, err)
	assert.Equal(t, flat.GeometryTypePolygon, inv.HeaderGeometryType)
	assert.Equal(t, 3, inv.Features)
	assert.Equal(t, 2, inv.EmptyGeometries, "missing table and coordinate-less table are both empty")
	assert.Equal(t, []GeometryTypeCount{
		{Type: flat.GeometryTypePoint, Count: 1},
		{Type: flat.GeometryTypePolygon, Count: 1},
	}, inv.GeometryTypes, "geometry types must be sorted numerically")
}

func TestInspectFeatures_HeterogeneousSchema(t *testing.T) {
	hdr := mockHeader{
		columns: []mockColumn{
			{name: "id", columnType: flat.ColumnTypeInt},
			{name: "name", columnType: flat.ColumnTypeString},
		},
	}
	feats := []mockFeature{
		// Header schema, only one of two columns present.
		pointFeature(t, inspectProps(func(pw *PropWriter) {
			pw.WriteUShort(0)
			pw.WriteInt(1)
		})),
		// Overriding schema that drops "name" and adds "extra".
		{
			geometry: &mockGeometry{geometryType: flat.GeometryTypePoint, xy: []float64{1, 2}},
			columns: []mockColumn{
				{name: "id", columnType: flat.ColumnTypeInt},
				{name: "extra", columnType: flat.ColumnTypeBool},
			},
			properties: inspectProps(func(pw *PropWriter) {
				pw.WriteUShort(0)
				pw.WriteInt(2)
				pw.WriteUShort(1)
				pw.WriteBool(true)
			}),
		},
		// Another overriding schema; this feature omits "extra".
		{
			geometry: &mockGeometry{geometryType: flat.GeometryTypePoint, xy: []float64{1, 2}},
			columns: []mockColumn{
				{name: "id", columnType: flat.ColumnTypeInt},
				{name: "extra", columnType: flat.ColumnTypeBool},
			},
			properties: inspectProps(func(pw *PropWriter) {
				pw.WriteUShort(0)
				pw.WriteInt(3)
			}),
		},
	}
	r := buildInspectFile(t, hdr, feats)

	inv, err := InspectFeatures(r, InspectOptions{})

	require.NoError(t, err)
	assert.Equal(t, 3, inv.Features)
	assert.Equal(t, 2, inv.FeatureSchemas)
	// 4 + (4+1) + 4 = 13 value bytes.
	assert.Equal(t, int64(13), inv.ValueBytes)
	assert.Equal(t, []string{"extra", "id", "name"}, []string{
		inv.Columns[0].Name, inv.Columns[1].Name, inv.Columns[2].Name,
	})

	extra := findProfile(t, inv, "extra")
	assert.False(t, extra.InHeader)
	assert.True(t, extra.InFeatureSchema)
	assert.Equal(t, []int{1}, extra.Indexes)
	assert.Equal(t, 1, extra.Count)
	assert.Equal(t, 1, extra.Missing)
	assert.Equal(t, []flat.ColumnType{flat.ColumnTypeBool}, extra.ObservedTypes)
	require.Len(t, inv.ExtraColumns, 1)
	assert.Equal(t, "extra", inv.ExtraColumns[0].Name)

	id := findProfile(t, inv, "id")
	assert.True(t, id.InHeader)
	assert.True(t, id.InFeatureSchema)
	assert.Equal(t, 3, id.Count)
	assert.Zero(t, id.Missing)

	name := findProfile(t, inv, "name")
	assert.True(t, name.InHeader)
	assert.Zero(t, name.Count)
	// Missing in feature 1 (omitted value), then dropped wholesale by
	// the two overriding schemas.
	assert.Equal(t, 3, name.Missing)
	require.Len(t, inv.MissingColumns, 1)
	assert.Equal(t, "name", inv.MissingColumns[0].Name)
}

func TestInspectFeatures_TypeConflict(t *testing.T) {
	hdr := mockHeader{
		columns: []mockColumn{
			{name: "v", columnType: flat.ColumnTypeInt},
		},
	}
	feats := []mockFeature{
		pointFeature(t, inspectProps(func(pw *PropWriter) {
			pw.WriteUShort(0)
			pw.WriteInt(42)
		})),
		{
			geometry: &mockGeometry{geometryType: flat.GeometryTypePoint, xy: []float64{1, 2}},
			columns: []mockColumn{
				{name: "v", columnType: flat.ColumnTypeString},
			},
			properties: inspectProps(func(pw *PropWriter) {
				pw.WriteUShort(0)
				pw.WriteString("hi")
			}),
		},
	}
	r := buildInspectFile(t, hdr, feats)

	inv, err := InspectFeatures(r, InspectOptions{})

	require.NoError(t, err)
	assert.Equal(t, 1, inv.FeatureSchemas)
	v := findProfile(t, inv, "v")
	assert.Equal(t, 2, v.Count)
	assert.Equal(t, []flat.ColumnType{flat.ColumnTypeInt, flat.ColumnTypeString}, v.ObservedTypes,
		"conflicting observed types must be sorted and both retained")
	assert.Equal(t, []flat.ColumnType{flat.ColumnTypeInt, flat.ColumnTypeString}, v.DeclaredTypes)
	assert.Equal(t, []int{0}, v.Indexes)
	assert.True(t, v.InHeader)
	assert.True(t, v.InFeatureSchema)
	// 4 + (4+2) = 10 value bytes.
	assert.Equal(t, int64(10), inv.ValueBytes)
}

func TestInspectFeatures_DuplicateColumnNames(t *testing.T) {
	hdr := mockHeader{
		columns: []mockColumn{
			{name: "x", columnType: flat.ColumnTypeInt},
			{name: "a", columnType: flat.ColumnTypeString},
			{name: "x", columnType: flat.ColumnTypeDouble},
		},
	}
	feats := []mockFeature{
		pointFeature(t, inspectProps(func(pw *PropWriter) {
			pw.WriteUShort(0)
			pw.WriteInt(1)
			pw.WriteUShort(1)
			pw.WriteString("z")
			pw.WriteUShort(2)
			pw.WriteDouble(2.5)
		})),
	}
	r := buildInspectFile(t, hdr, feats)

	inv, err := InspectFeatures(r, InspectOptions{})

	require.NoError(t, err)
	require.Len(t, inv.Columns, 2, "duplicate names must merge into one profile")
	assert.Equal(t, "a", inv.Columns[0].Name)
	assert.Equal(t, "x", inv.Columns[1].Name)
	x := inv.Columns[1]
	assert.Equal(t, []int{0, 2}, x.Indexes, "indexes must be sorted and de-duplicated")
	assert.Equal(t, []flat.ColumnType{flat.ColumnTypeInt, flat.ColumnTypeDouble}, x.DeclaredTypes)
	assert.Equal(t, []flat.ColumnType{flat.ColumnTypeInt, flat.ColumnTypeDouble}, x.ObservedTypes)
	assert.Equal(t, 2, x.Count)
}

func TestInspectFeatures_MaxFeatures(t *testing.T) {
	feats := []mockFeature{
		pointFeature(t, nil), pointFeature(t, nil),
		pointFeature(t, nil), pointFeature(t, nil),
	}
	newReader := func() *bytes.Reader {
		return buildInspectFile(t, mockHeader{}, feats)
	}

	t.Run("Zero Means Full Scan", func(t *testing.T) {
		inv, err := InspectFeatures(newReader(), InspectOptions{MaxFeatures: 0})

		require.NoError(t, err)
		assert.Equal(t, 4, inv.Features)
		assert.False(t, inv.Truncated)
	})

	t.Run("Cap Below Total Truncates", func(t *testing.T) {
		inv, err := InspectFeatures(newReader(), InspectOptions{MaxFeatures: 2})

		require.NoError(t, err)
		assert.Equal(t, 2, inv.Features)
		assert.True(t, inv.Truncated)
	})

	t.Run("Cap Of One Truncates", func(t *testing.T) {
		inv, err := InspectFeatures(newReader(), InspectOptions{MaxFeatures: 1})

		require.NoError(t, err)
		assert.Equal(t, 1, inv.Features)
		assert.True(t, inv.Truncated)
	})

	t.Run("Cap Equal To Total Is Not Truncated", func(t *testing.T) {
		inv, err := InspectFeatures(newReader(), InspectOptions{MaxFeatures: 4})

		require.NoError(t, err)
		assert.Equal(t, 4, inv.Features)
		assert.False(t, inv.Truncated, "reaching EOF exactly at the cap is not truncation")
	})

	t.Run("Cap Above Total Is Not Truncated", func(t *testing.T) {
		inv, err := InspectFeatures(newReader(), InspectOptions{MaxFeatures: 100})

		require.NoError(t, err)
		assert.Equal(t, 4, inv.Features)
		assert.False(t, inv.Truncated)
	})
}

func TestInspectFeatures_MaxValueBytes(t *testing.T) {
	hdr := mockHeader{
		columns: []mockColumn{
			{name: "n", columnType: flat.ColumnTypeInt},
			{name: "s", columnType: flat.ColumnTypeString},
		},
	}
	feats := []mockFeature{
		pointFeature(t, inspectProps(func(pw *PropWriter) {
			pw.WriteUShort(0)
			pw.WriteInt(1) // 4 value bytes.
		})),
		pointFeature(t, inspectProps(func(pw *PropWriter) {
			pw.WriteUShort(0)
			pw.WriteInt(2) // 4 value bytes.
		})),
	}

	t.Run("Exact Budget Succeeds", func(t *testing.T) {
		r := buildInspectFile(t, hdr, feats)
		inv, err := InspectFeatures(r, InspectOptions{MaxValueBytes: 8})

		require.NoError(t, err)
		assert.Equal(t, int64(8), inv.ValueBytes)
		assert.Equal(t, 2, inv.Features)
	})

	t.Run("Zero Means Unlimited", func(t *testing.T) {
		r := buildInspectFile(t, hdr, feats)
		inv, err := InspectFeatures(r, InspectOptions{MaxValueBytes: 0})

		require.NoError(t, err)
		assert.Equal(t, int64(8), inv.ValueBytes)
	})

	t.Run("Breach Returns Sentinel Error And Partial Inventory", func(t *testing.T) {
		r := buildInspectFile(t, hdr, feats)
		inv, err := InspectFeatures(r, InspectOptions{MaxValueBytes: 4})

		require.Error(t, err)
		require.True(t, errors.Is(err, ErrValueLimit))
		require.NotNil(t, inv)
		assert.False(t, inv.Truncated, "budget breach is an error, not a truncation")
		assert.Equal(t, int64(4), inv.ValueBytes)
		assert.Equal(t, 2, inv.Features, "the failing feature was entered before its properties were read")
	})

	t.Run("First Feature Over Budget", func(t *testing.T) {
		r := buildInspectFile(t, hdr, feats)
		inv, err := InspectFeatures(r, InspectOptions{MaxValueBytes: 3})

		require.Error(t, err)
		require.True(t, errors.Is(err, ErrValueLimit))
		require.NotNil(t, inv)
		assert.Equal(t, int64(0), inv.ValueBytes, "no value bytes are charged past the budget")
	})

	t.Run("Variable Length Boundary", func(t *testing.T) {
		strHdr := mockHeader{
			columns: []mockColumn{{name: "s", columnType: flat.ColumnTypeString}},
		}
		strFeat := []mockFeature{
			pointFeature(t, inspectProps(func(pw *PropWriter) {
				pw.WriteUShort(0)
				pw.WriteString("foo") // 4-byte length prefix + 3 payload bytes.
			})),
		}

		// Budget covers the length prefix but not the payload.
		r := buildInspectFile(t, strHdr, strFeat)
		inv, err := InspectFeatures(r, InspectOptions{MaxValueBytes: 4})
		require.Error(t, err)
		require.True(t, errors.Is(err, ErrValueLimit))
		assert.Equal(t, int64(4), inv.ValueBytes)

		r = buildInspectFile(t, strHdr, strFeat)
		inv, err = InspectFeatures(r, InspectOptions{MaxValueBytes: 7})
		require.NoError(t, err)
		assert.Equal(t, int64(7), inv.ValueBytes)
	})
}

func TestInspectFeatures_MalformedData(t *testing.T) {
	t.Run("Truncated Data Section", func(t *testing.T) {
		// The header promises one feature but the data section is
		// entirely absent.
		mockDataRunTest(t, func(t *testing.T, mf *mockFile) {
			inv, err := InspectFeatures(bytes.NewReader(mf.allBytes), InspectOptions{})

			require.Error(t, err)
			require.NotNil(t, inv)
			assert.False(t, inv.Truncated)
			assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
			assert.ErrorContains(t, err, "header-declared features")
		}, "truncated_data")
	})

	t.Run("Truncated Index Section", func(t *testing.T) {
		mockDataRunTest(t, func(t *testing.T, mf *mockFile) {
			inv, err := InspectFeatures(bytes.NewReader(mf.allBytes), InspectOptions{})

			require.Error(t, err)
			require.NotNil(t, inv)
			assert.False(t, inv.Truncated)
		}, "truncated_index")
	})

	t.Run("Feature Truncated Mid Stream", func(t *testing.T) {
		// One complete feature followed by two stray bytes that cannot
		// form a complete feature length prefix.
		goodFeature := pointFeature(t, nil)
		good := goodFeature.buildAsBytes()
		hdr := mockHeader{featuresCount: 2, indexNodeSize: uint16Ptr(0)}
		var b bytes.Buffer
		_, err := b.Write(magic[:])
		require.NoError(t, err)
		_, err = b.Write(hdr.buildAsBytes())
		require.NoError(t, err)
		_, err = b.Write(good)
		require.NoError(t, err)
		_, err = b.Write([]byte{0x01, 0x02})
		require.NoError(t, err)

		inv, err := InspectFeatures(bytes.NewReader(b.Bytes()), InspectOptions{})

		require.Error(t, err)
		require.NotNil(t, inv)
		assert.False(t, inv.Truncated)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
		assert.Equal(t, 1, inv.Features, "the complete feature is still reflected in the partial inventory")
	})

	t.Run("Feature Length Too Small", func(t *testing.T) {
		mockDataRunTest(t, func(t *testing.T, mf *mockFile) {
			inv, err := InspectFeatures(bytes.NewReader(mf.allBytes), InspectOptions{})

			require.Error(t, err)
			require.NotNil(t, inv)
			assert.False(t, inv.Truncated)
		}, "feature_length_too_small")
	})

	t.Run("Corrupt Feature Does Not Panic", func(t *testing.T) {
		mockDataRunTest(t, func(t *testing.T, mf *mockFile) {
			inv, err := InspectFeatures(bytes.NewReader(mf.allBytes), InspectOptions{})

			require.Error(t, err)
			require.NotNil(t, inv)
			assert.False(t, inv.Truncated)
		}, "feature_corrupt")
	})

	t.Run("Property Column Index Out Of Range", func(t *testing.T) {
		hdr := mockHeader{
			columns: []mockColumn{{name: "a", columnType: flat.ColumnTypeInt}},
		}
		feats := []mockFeature{
			pointFeature(t, inspectProps(func(pw *PropWriter) {
				pw.WriteUShort(5)
				pw.WriteInt(1)
			})),
		}
		r := buildInspectFile(t, hdr, feats)

		inv, err := InspectFeatures(r, InspectOptions{})

		require.Error(t, err)
		require.NotNil(t, inv)
		assert.ErrorContains(t, err, "schema has only 1 columns")
		assert.ErrorContains(t, err, "column index 5")
	})

	t.Run("Truncated Property Value", func(t *testing.T) {
		hdr := mockHeader{
			columns: []mockColumn{{name: "s", columnType: flat.ColumnTypeString}},
		}
		feats := []mockFeature{
			pointFeature(t, inspectProps(func(pw *PropWriter) {
				pw.WriteUShort(0)
				pw.WriteUInt(10) // Promises ten payload bytes...
				pw.WriteUInt(0)  // ...but only four are present.
			})),
		}
		r := buildInspectFile(t, hdr, feats)

		inv, err := InspectFeatures(r, InspectOptions{})

		require.Error(t, err)
		require.NotNil(t, inv)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
	})

	t.Run("Unsupported Column Type Does Not Panic", func(t *testing.T) {
		hdr := mockHeader{
			columns: []mockColumn{{name: "x", columnType: flat.ColumnType(99)}},
		}
		feats := []mockFeature{
			pointFeature(t, inspectProps(func(pw *PropWriter) {
				pw.WriteUShort(0)
				pw.WriteUByte(1)
			})),
		}
		r := buildInspectFile(t, hdr, feats)

		inv, err := InspectFeatures(r, InspectOptions{})

		require.Error(t, err)
		require.NotNil(t, inv)
		assert.ErrorContains(t, err, "unsupported column type")
		assert.ErrorContains(t, err, "column index 0")
	})

	t.Run("Property On Zero Column Schema", func(t *testing.T) {
		feats := []mockFeature{
			pointFeature(t, inspectProps(func(pw *PropWriter) {
				pw.WriteUShort(0)
				pw.WriteUByte(1)
			})),
		}
		r := buildInspectFile(t, mockHeader{}, feats)

		inv, err := InspectFeatures(r, InspectOptions{})

		require.Error(t, err)
		require.NotNil(t, inv)
		assert.ErrorContains(t, err, "schema has only 0 columns")
	})
}

func TestInspectFeatures_Deterministic(t *testing.T) {
	hdr := mockHeader{
		geometryType: flat.GeometryTypePoint,
		columns: []mockColumn{
			{name: "zeta", columnType: flat.ColumnTypeInt},
			{name: "alpha", columnType: flat.ColumnTypeString},
			{name: "alpha", columnType: flat.ColumnTypeDouble},
			{name: "mid", columnType: flat.ColumnTypeBool},
		},
	}
	feats := []mockFeature{
		pointFeature(t, inspectProps(func(pw *PropWriter) {
			pw.WriteUShort(0)
			pw.WriteInt(1)
			pw.WriteUShort(1)
			pw.WriteString("s")
			pw.WriteUShort(2)
			pw.WriteDouble(2)
			pw.WriteUShort(3)
			pw.WriteBool(true)
		})),
		{
			geometry: &mockGeometry{geometryType: flat.GeometryTypeLineString, xy: []float64{0, 0, 1, 1}},
			columns: []mockColumn{
				{name: "zeta", columnType: flat.ColumnTypeInt},
				{name: "alpha", columnType: flat.ColumnTypeString},
				{name: "alpha", columnType: flat.ColumnTypeDouble},
				{name: "mid", columnType: flat.ColumnTypeBool},
			},
			properties: inspectProps(func(pw *PropWriter) {
				pw.WriteUShort(0)
				pw.WriteInt(2)
			}),
		},
	}

	var results []*FeatureInventory
	for i := 0; i < 3; i++ {
		r := buildInspectFile(t, hdr, feats)
		inv, err := InspectFeatures(r, InspectOptions{})
		require.NoError(t, err)
		results = append(results, inv)
	}

	for i := 1; i < len(results); i++ {
		assert.True(t, reflect.DeepEqual(results[0], results[i]), "repeated scans must yield identical inventories")
	}
	inv := results[0]
	assert.Equal(t, []string{"alpha", "mid", "zeta"}, []string{
		inv.Columns[0].Name, inv.Columns[1].Name, inv.Columns[2].Name,
	})
	assert.Equal(t, []flat.GeometryType{flat.GeometryTypePoint, flat.GeometryTypeLineString},
		[]flat.GeometryType{inv.GeometryTypes[0].Type, inv.GeometryTypes[1].Type})
	alpha := findProfile(t, inv, "alpha")
	assert.Equal(t, []int{1, 2}, alpha.Indexes)
	assert.Equal(t, []flat.ColumnType{flat.ColumnTypeDouble, flat.ColumnTypeString}, alpha.ObservedTypes)
	assert.Equal(t, 1, inv.FeatureSchemas)
}

func TestInspectFeatures_RealTestData(t *testing.T) {
	cases := []struct {
		name     string
		features int
	}{
		{"countries.fgb", 179},
		{"heterogeneous.fgb", 3},
		{"empty.fgb", 0},
		{"no_properties.fgb", 1},
		{"unknown_feature_count.fgb", 1},
		{"UScounties.fgb", 3221},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var first *FeatureInventory
			for i := 0; i < 2; i++ {
				r := newTestDataBytesReader(t, false, tc.name)
				inv, err := InspectFeatures(r, InspectOptions{})
				require.NoError(t, err)
				require.NotNil(t, inv)
				assert.Equal(t, tc.features, inv.Features)
				assert.False(t, inv.Truncated)
				assert.NotNil(t, inv.Columns)
				assert.NotNil(t, inv.GeometryTypes)
				if first == nil {
					first = inv
				} else {
					assert.True(t, reflect.DeepEqual(first, inv), "real data scans must be deterministic")
				}
			}

			// A capped scan of real data stops safely and is marked.
			if tc.features > 1 {
				r := newTestDataBytesReader(t, false, tc.name)
				inv, err := InspectFeatures(r, InspectOptions{MaxFeatures: 1})
				require.NoError(t, err)
				assert.Equal(t, 1, inv.Features)
				assert.True(t, inv.Truncated)
			}
		})
	}

	t.Run("Heterogeneous Geometry Types", func(t *testing.T) {
		r := newTestDataBytesReader(t, false, "heterogeneous.fgb")
		inv, err := InspectFeatures(r, InspectOptions{})
		require.NoError(t, err)
		assert.Equal(t, []GeometryTypeCount{
			{Type: flat.GeometryTypePoint, Count: 1},
			{Type: flat.GeometryTypeLineString, Count: 1},
			{Type: flat.GeometryTypeMultiPolygon, Count: 1},
		}, inv.GeometryTypes)
		assert.Zero(t, inv.FeatureSchemas)
	})

	t.Run("Empty File Keeps Header Schema", func(t *testing.T) {
		r := newTestDataBytesReader(t, false, "empty.fgb")
		inv, err := InspectFeatures(r, InspectOptions{})
		require.NoError(t, err)
		assert.Zero(t, inv.Features)
		require.Len(t, inv.HeaderColumns, 6)
		require.Len(t, inv.Columns, 6)
		for _, c := range inv.Columns {
			assert.Zero(t, c.Count)
			assert.True(t, c.InHeader)
		}
	})

	t.Run("Unknown Feature Count Scans To EOF", func(t *testing.T) {
		r := newTestDataBytesReader(t, false, "unknown_feature_count.fgb")
		inv, err := InspectFeatures(r, InspectOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, inv.Features)
		assert.Equal(t, int64(40), inv.ValueBytes)
	})
}
