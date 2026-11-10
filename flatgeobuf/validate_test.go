// Copyright 2023 The flatgeobuf (Go) Authors. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package flatgeobuf

import (
	"bytes"
	"errors"
	"io"
	"math/rand"
	"testing"

	"github.com/gogama/flatgeobuf/flatgeobuf/flat"
	"github.com/gogama/flatgeobuf/packedrtree"
	flatbuffers "github.com/google/flatbuffers/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildValidateFile assembles a complete FlatGeobuf file from a
// mockHeader, an optional packed R-Tree reference list and zero or
// more pre-built feature frame byte slices.
func buildValidateFile(t *testing.T, hdr *mockHeader, refs []packedrtree.Ref, featureBytes ...[]byte) []byte {
	t.Helper()
	var b bytes.Buffer

	_, err := b.Write(magic[:])
	require.NoError(t, err)
	_, err = b.Write(hdr.buildAsBytes())
	require.NoError(t, err)

	if len(refs) > 0 {
		require.NotNil(t, hdr.indexNodeSize)
		prt, err := packedrtree.New(refs, *hdr.indexNodeSize)
		require.NoError(t, err)
		_, err = prt.Marshal(&b)
		require.NoError(t, err)
	}

	for i := range featureBytes {
		_, err = b.Write(featureBytes[i])
		require.NoError(t, err)
	}

	return b.Bytes()
}

// mustMockFile locates and initializes one of the shared mockFile
// fixtures by name.
func mustMockFile(t *testing.T, name string) *mockFile {
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

// pointFeatureBytes builds a size-prefixed feature containing one
// point geometry of the requested geometry type and optional
// properties.
func pointFeatureBytes(t *testing.T, geometryType flat.GeometryType, props []byte) []byte {
	t.Helper()
	mf := mockFeature{
		geometry: &mockGeometry{
			xy:           []float64{1, 2},
			geometryType: geometryType,
		},
		properties: props,
	}
	return mf.buildAsBytes()
}

// hasIssueCode reports whether the report contains an issue with the
// given code.
func hasIssueCode(rep *ValidationReport, code IssueCode) bool {
	for i := range rep.Issues {
		if rep.Issues[i].Code == code {
			return true
		}
	}
	return false
}

// findIssue returns the first issue with the given code, failing the
// test if no such issue exists.
func findIssue(t *testing.T, rep *ValidationReport, code IssueCode) ValidationIssue {
	t.Helper()
	for i := range rep.Issues {
		if rep.Issues[i].Code == code {
			return rep.Issues[i]
		}
	}
	t.Fatalf("report has no issue with code %q; issues are: %+v", code, rep.Issues)
	return ValidationIssue{}
}

// assertReportInvariant checks the fundamental contract that a report
// is always returned and that errors and issues agree.
func assertReportInvariant(t *testing.T, rep *ValidationReport, err error) {
	t.Helper()
	require.NotNil(t, rep, "ValidateFile must always return a report")
	assert.Equal(t, len(rep.Issues) > 0, err != nil,
		"non-nil error must agree with non-empty issues; issues=%d err=%v", len(rep.Issues), err)
}

func TestValidateFile_Valid(t *testing.T) {
	t.Run("With Index", func(t *testing.T) {
		mf := mustMockFile(t, "four_points_in_quadrants")

		rep, err := ValidateFile(bytes.NewReader(mf.allBytes))

		require.NoError(t, err)
		assertReportInvariant(t, rep, err)
		assert.Empty(t, rep.Issues)
		assert.Equal(t, SpecVersion{Major: 3, Patch: 1}, rep.Version)
		assert.Equal(t, uint64(4), rep.DeclaredFeatures)
		assert.Equal(t, 4, rep.ActualFeatures)
		assert.True(t, rep.HasIndex)
		assert.Equal(t, uint16(2), rep.NodeSize)
		assert.Equal(t, ByteRange{Start: mf.indexOffset, End: mf.dataOffsets[0]}, rep.IndexBounds)
		assert.Equal(t, ByteRange{Start: mf.dataOffsets[0], End: int64(len(mf.allBytes))}, rep.DataBounds)
	})

	t.Run("No Index", func(t *testing.T) {
		mf := mustMockFile(t, "one_feature_no_index")

		rep, err := ValidateFile(bytes.NewReader(mf.allBytes))

		require.NoError(t, err)
		assertReportInvariant(t, rep, err)
		assert.Empty(t, rep.Issues)
		assert.Equal(t, uint64(1), rep.DeclaredFeatures)
		assert.Equal(t, 1, rep.ActualFeatures)
		assert.False(t, rep.HasIndex)
		assert.Equal(t, uint16(0), rep.NodeSize)
		assert.Equal(t, ByteRange{}, rep.IndexBounds)
		assert.Equal(t, ByteRange{Start: mf.indexOffset, End: int64(len(mf.allBytes))}, rep.DataBounds)
	})

	t.Run("Unknown Feature Count", func(t *testing.T) {
		mf := mustMockFile(t, "one_feature_no_index_unknown_count")

		rep, err := ValidateFile(bytes.NewReader(mf.allBytes))

		require.NoError(t, err)
		assertReportInvariant(t, rep, err)
		assert.Empty(t, rep.Issues)
		assert.Equal(t, uint64(0), rep.DeclaredFeatures)
		assert.Equal(t, 1, rep.ActualFeatures)
		assert.False(t, rep.HasIndex)
	})

	t.Run("Empty Dataset", func(t *testing.T) {
		mf := mustMockFile(t, "empty")

		rep, err := ValidateFile(bytes.NewReader(mf.allBytes))

		require.NoError(t, err)
		assertReportInvariant(t, rep, err)
		assert.Empty(t, rep.Issues)
		assert.Equal(t, 0, rep.ActualFeatures)
		assert.Equal(t, int64(len(mf.allBytes)), rep.DataBounds.End)
	})

	t.Run("Real Test Data", func(t *testing.T) {
		for _, filename := range testDataFileNames(t) {
			filename := filename
			t.Run(filename, func(t *testing.T) {
				// Skip files written under an unsupported spec major
				// version; those are expected to fail.
				version, err := Magic(newTestDataBytesReader(t, false, filename))
				require.NoError(t, err)
				if version.Major != MinSpecMajorVersion {
					t.Skipf("unsupported major version %d", version.Major)
				}

				for _, seeker := range []bool{true, false} {
					r := newTestDataBytesReader(t, seeker, filename)
					rep, err := ValidateFile(r)

					require.NotNil(t, rep)
					if !assert.NoError(t, err, "valid testdata file %q should validate cleanly: %+v", filename, rep.Issues) {
						for _, iss := range rep.Issues {
							t.Logf("  issue: %+v", iss)
						}
					}
					assert.Empty(t, rep.Issues)
					if rep.DeclaredFeatures > 0 {
						assert.Equal(t, rep.DeclaredFeatures, uint64(rep.ActualFeatures))
					}
				}
			})
		}
	})
}

func TestValidateFile_EmptyInput(t *testing.T) {
	t.Run("Zero Bytes", func(t *testing.T) {
		rep, err := ValidateFile(bytes.NewReader(nil))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.ErrorIs(t, err, io.EOF)
		require.Len(t, rep.Issues, 1)
		iss := rep.Issues[0]
		assert.Equal(t, IssueCodeEmptyInput, iss.Code)
		assert.Equal(t, ValidationStageMagic, iss.Stage)
		assert.Equal(t, noFeatureIndex, iss.FeatureIndex)
		assert.Equal(t, int64(0), iss.ByteOffset)
		assert.Equal(t, SpecVersion{}, rep.Version)
	})

	t.Run("Nil Reader Does Not Panic", func(t *testing.T) {
		rep, err := ValidateFile(nil)

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		require.Len(t, rep.Issues, 1)
		assert.Equal(t, IssueCodeReadError, rep.Issues[0].Code)
	})
}

func TestValidateFile_InvalidMagic(t *testing.T) {
	t.Run("Wrong Magic Bytes", func(t *testing.T) {
		b := []byte{1, 2, 3, 4, 5, 6, 7, 8}

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		require.Len(t, rep.Issues, 1)
		assert.Equal(t, IssueCodeInvalidMagic, rep.Issues[0].Code)
		assert.Equal(t, ValidationStageMagic, rep.Issues[0].Stage)
		assert.Equal(t, int64(0), rep.Issues[0].ByteOffset)
	})

	t.Run("Partial Magic", func(t *testing.T) {
		b := append(append([]byte{}, magic[:4]...), 0, 0, 0, 0)

		rep, err := ValidateFile(bytes.NewReader(b[:4]))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
		assert.True(t, hasIssueCode(rep, IssueCodeInvalidMagic))
	})

	t.Run("Unsupported Major Version", func(t *testing.T) {
		b := append([]byte{}, magic[:]...)
		b[3] = 0x02

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		require.True(t, hasIssueCode(rep, IssueCodeUnsupportedVersion))
		assert.Equal(t, SpecVersion{Major: 2, Patch: 1}, rep.Version)
	})
}

func TestValidateFile_HeaderIssues(t *testing.T) {
	t.Run("Truncated Header", func(t *testing.T) {
		mf := mustMockFile(t, "truncated_header")

		rep, err := ValidateFile(bytes.NewReader(mf.allBytes))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.True(t, hasIssueCode(rep, IssueCodeReadError) || hasIssueCode(rep, IssueCodeHeaderLengthInvalid))
		assert.Equal(t, ValidationStageHeader, rep.Issues[0].Stage)
	})

	t.Run("Node Size One", func(t *testing.T) {
		mh := mockHeader{
			featuresCount: 1,
			indexNodeSize: uint16Ptr(1),
		}
		b := buildValidateFile(t, &mh, nil)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		iss := findIssue(t, rep, IssueCodeNodeSizeInvalid)
		assert.Equal(t, ValidationStageHeader, iss.Stage)
		assert.Equal(t, uint16(1), rep.NodeSize)
	})

	t.Run("Feature Count Overflow", func(t *testing.T) {
		mf := mustMockFile(t, "index_size_overflow")

		rep, err := ValidateFile(bytes.NewReader(mf.allBytes))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		// On 64-bit platforms the count fits in int and the failure
		// surfaces when the index size overflows; on 32-bit platforms
		// the header count itself overflows first.
		assert.True(t,
			hasIssueCode(rep, IssueCodeFeatureCountOverflow) ||
				hasIssueCode(rep, IssueCodeIndexSizeOverflow))
	})
}

func TestValidateFile_TruncatedData(t *testing.T) {
	t.Run("Truncated Index", func(t *testing.T) {
		mf := mustMockFile(t, "truncated_index")

		rep, err := ValidateFile(bytes.NewReader(mf.allBytes))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		// The index section is completely absent, so the read fails
		// with plain EOF rather than unexpected EOF.
		assert.True(t, errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF))
		iss := findIssue(t, rep, IssueCodeIndexUnparseable)
		assert.Equal(t, ValidationStageIndex, iss.Stage)
		assert.Equal(t, mf.indexOffset, iss.ByteOffset)
		assert.False(t, rep.HasIndex)
	})

	t.Run("Truncated Feature Body", func(t *testing.T) {
		mf := mustMockFile(t, "one_feature_with_index")
		// Drop the final three bytes so the single feature frame is
		// cut off in the middle of its table body.
		b := mf.allBytes[:len(mf.allBytes)-3]

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
		iss := findIssue(t, rep, IssueCodeFeatureTruncated)
		assert.Equal(t, ValidationStageData, iss.Stage)
		assert.Equal(t, 0, iss.FeatureIndex)
		assert.Equal(t, mf.dataOffsets[0], iss.ByteOffset)
		assert.Equal(t, 0, rep.ActualFeatures)
		assert.True(t, hasIssueCode(rep, IssueCodeFeatureCountMismatch))
		assert.Equal(t, int64(len(b)), rep.DataBounds.End)
	})

	t.Run("Truncated Feature Length Prefix", func(t *testing.T) {
		mf := mustMockFile(t, "one_feature_no_index")
		// Leave only two of the four length-prefix bytes.
		dataStart := mf.indexOffset
		b := mf.allBytes[:dataStart+2]

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
		iss := findIssue(t, rep, IssueCodeFeatureTruncated)
		assert.Equal(t, 0, iss.FeatureIndex)
		assert.Equal(t, dataStart, iss.ByteOffset)
	})

	t.Run("Feature Length Too Small", func(t *testing.T) {
		mf := mustMockFile(t, "feature_length_too_small")

		rep, err := ValidateFile(bytes.NewReader(mf.allBytes))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		iss := findIssue(t, rep, IssueCodeFeatureFraming)
		assert.Equal(t, ValidationStageData, iss.Stage)
		assert.Equal(t, 0, iss.FeatureIndex)
	})

	t.Run("Corrupt Feature FlatBuffer", func(t *testing.T) {
		mf := mustMockFile(t, "feature_corrupt")

		rep, err := ValidateFile(bytes.NewReader(mf.allBytes))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.True(t, hasIssueCode(rep, IssueCodeFeatureFraming))
		// The frame itself was fully read, so the feature is still
		// counted.
		assert.Equal(t, 1, rep.ActualFeatures)
	})
}

func TestValidateFile_FeatureCountMismatch(t *testing.T) {
	t.Run("Fewer Features Than Declared", func(t *testing.T) {
		hdr := mockHeader{
			featuresCount: 2,
			indexNodeSize: uint16Ptr(0),
			geometryType:  flat.GeometryTypePoint,
		}
		feat := pointFeatureBytes(t, flat.GeometryTypePoint, nil)
		b := buildValidateFile(t, &hdr, nil, feat)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.Equal(t, uint64(2), rep.DeclaredFeatures)
		assert.Equal(t, 1, rep.ActualFeatures)
		iss := findIssue(t, rep, IssueCodeFeatureCountMismatch)
		assert.Equal(t, ValidationStageData, iss.Stage)
		assert.False(t, hasIssueCode(rep, IssueCodeTrailingBytes))
	})

	t.Run("More Features Than Declared Is Trailing Bytes", func(t *testing.T) {
		hdr := mockHeader{
			featuresCount: 1,
			indexNodeSize: uint16Ptr(0),
			geometryType:  flat.GeometryTypePoint,
		}
		feat1 := pointFeatureBytes(t, flat.GeometryTypePoint, nil)
		feat2 := pointFeatureBytes(t, flat.GeometryTypePoint, nil)
		b := buildValidateFile(t, &hdr, nil, feat1, feat2)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.Equal(t, 2, rep.ActualFeatures)
		assert.True(t, hasIssueCode(rep, IssueCodeFeatureCountMismatch))
		iss := findIssue(t, rep, IssueCodeTrailingBytes)
		assert.Equal(t, ValidationStageTrailer, iss.Stage)
		assert.Equal(t, 1, iss.FeatureIndex)
		dataStart := int64(magicLen) + int64(len(hdr.buildAsBytes()))
		assert.Equal(t, dataStart+int64(len(feat1)), iss.ByteOffset)
	})

	t.Run("Indexed Count And Reference Mismatch", func(t *testing.T) {
		hdr := mockHeader{
			featuresCount: 2,
			indexNodeSize: uint16Ptr(2),
			geometryType:  flat.GeometryTypePoint,
		}
		feat := pointFeatureBytes(t, flat.GeometryTypePoint, nil)
		// The second reference points at an offset where no feature
		// frame starts.
		refs := []packedrtree.Ref{
			{Box: packedrtree.Box{XMax: 1, YMax: 2}, Offset: 0},
			{Box: packedrtree.Box{XMax: 1, YMax: 2}, Offset: int64(len(feat)) + 100},
		}
		b := buildValidateFile(t, &hdr, refs, feat)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.True(t, rep.HasIndex)
		assert.True(t, hasIssueCode(rep, IssueCodeFeatureCountMismatch))
		assert.True(t, hasIssueCode(rep, IssueCodeIndexRefCountMismatch))
		iss := findIssue(t, rep, IssueCodeIndexRefInvalid)
		assert.Equal(t, ValidationStageIndex, iss.Stage)
		assert.Equal(t, 1, iss.FeatureIndex)
	})
}

func TestValidateFile_BadIndex(t *testing.T) {
	t.Run("Corrupt Leaf Offset", func(t *testing.T) {
		mf := mustMockFile(t, "one_feature_with_index")
		b := append([]byte(nil), mf.allBytes...)

		// The single index node is 40 bytes; its int64 Offset field
		// occupies the final 8 bytes. Corrupt its most significant
		// byte so the leaf points far outside the data section.
		indexEnd := mf.indexOffset + int64(len(mf.indexBytes))
		b[indexEnd-1] = 0x7F

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.True(t, rep.HasIndex)
		iss := findIssue(t, rep, IssueCodeIndexRefInvalid)
		assert.Equal(t, ValidationStageIndex, iss.Stage)
		assert.Equal(t, 0, iss.FeatureIndex)
		assert.Equal(t, 1, rep.ActualFeatures)
		// The feature itself is fine, so no data-stage issues should
		// accompany the bad index reference.
		for i := range rep.Issues {
			assert.NotEqual(t, ValidationStageData, rep.Issues[i].Stage)
		}
	})
}

func TestValidateFile_BadProperties(t *testing.T) {
	newHeader := func() *mockHeader {
		return &mockHeader{
			featuresCount: 1,
			indexNodeSize: uint16Ptr(0),
			geometryType:  flat.GeometryTypePoint,
			columns: []mockColumn{
				{name: "n", columnType: flat.ColumnTypeInt},
				{name: "s", columnType: flat.ColumnTypeString},
			},
		}
	}

	t.Run("Truncated Property Value", func(t *testing.T) {
		hdr := newHeader()
		// Column index 0 (Int) followed by only two of the four
		// required value bytes.
		badProps := []byte{0x00, 0x00, 0x2A, 0x00}
		feat := pointFeatureBytes(t, flat.GeometryTypePoint, badProps)
		b := buildValidateFile(t, hdr, nil, feat)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		iss := findIssue(t, rep, IssueCodePropertiesUnreadable)
		assert.Equal(t, ValidationStageData, iss.Stage)
		assert.Equal(t, 0, iss.FeatureIndex)
		assert.Equal(t, int64(magicLen)+int64(len(hdr.buildAsBytes())), iss.ByteOffset)
		assert.False(t, hasIssueCode(rep, IssueCodeFeatureTruncated))
		assert.Equal(t, 1, rep.ActualFeatures)
	})

	t.Run("Column Index Out Of Range", func(t *testing.T) {
		hdr := newHeader()
		badProps := []byte{0x05, 0x00}
		feat := pointFeatureBytes(t, flat.GeometryTypePoint, badProps)
		b := buildValidateFile(t, hdr, nil, feat)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.True(t, hasIssueCode(rep, IssueCodePropertiesUnreadable))
	})

	t.Run("String Length Runs Past Buffer", func(t *testing.T) {
		hdr := newHeader()
		badProps := []byte{0x01, 0x00}
		lengthBuf := make([]byte, flatbuffers.SizeUint32)
		flatbuffers.WriteUint32(lengthBuf, 1000)
		badProps = append(badProps, lengthBuf...)
		feat := pointFeatureBytes(t, flat.GeometryTypePoint, badProps)
		b := buildValidateFile(t, hdr, nil, feat)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.True(t, hasIssueCode(rep, IssueCodePropertiesUnreadable))
	})

	t.Run("Unknown Column Type", func(t *testing.T) {
		hdr := &mockHeader{
			featuresCount: 1,
			indexNodeSize: uint16Ptr(0),
			geometryType:  flat.GeometryTypePoint,
			columns: []mockColumn{
				{name: "weird", columnType: flat.ColumnType(99)},
			},
		}
		badProps := []byte{0x00, 0x00, 0x01}
		feat := pointFeatureBytes(t, flat.GeometryTypePoint, badProps)
		b := buildValidateFile(t, hdr, nil, feat)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.True(t, hasIssueCode(rep, IssueCodePropertiesUnreadable))
	})

	t.Run("Feature Level Schema Is Used", func(t *testing.T) {
		hdr := &mockHeader{
			featuresCount: 1,
			indexNodeSize: uint16Ptr(0),
			geometryType:  flat.GeometryTypePoint,
		}
		var pb bytes.Buffer
		pw := NewPropWriter(&pb)
		_, _ = pw.WriteUShort(0)
		_, _ = pw.WriteDouble(3.14)
		mf := mockFeature{
			geometry: &mockGeometry{
				xy:           []float64{1, 2},
				geometryType: flat.GeometryTypePoint,
			},
			properties: pb.Bytes(),
			columns: []mockColumn{
				{name: "d", columnType: flat.ColumnTypeDouble},
			},
		}
		b := buildValidateFile(t, hdr, nil, mf.buildAsBytes())

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.NoError(t, err)
		assert.Empty(t, rep.Issues)

		// The same buffer fails against a mismatched feature schema.
		mf.columns = []mockColumn{
			{name: "s", columnType: flat.ColumnTypeString},
		}
		bad := buildValidateFile(t, hdr, nil, mf.buildAsBytes())

		rep, err = ValidateFile(bytes.NewReader(bad))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		assert.True(t, hasIssueCode(rep, IssueCodePropertiesUnreadable))
	})

	t.Run("Lone Trailing Property Byte Is Tolerated", func(t *testing.T) {
		// Canonical FlatGeobuf data exists (e.g. the official
		// alldatatypes.fgb fixture) whose final Binary property is
		// followed by a single trailing zero byte. Reference readers
		// tolerate this, so validation must not flag it.
		hdr := newHeader()
		var pb bytes.Buffer
		pw := NewPropWriter(&pb)
		_, _ = pw.WriteUShort(0)
		_, _ = pw.WriteInt(7)
		props := append(pb.Bytes(), 0x00)
		feat := pointFeatureBytes(t, flat.GeometryTypePoint, props)
		b := buildValidateFile(t, hdr, nil, feat)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.NoError(t, err)
		assert.Empty(t, rep.Issues)
	})

	t.Run("Valid And Sparse Properties Are Accepted", func(t *testing.T) {
		hdr := newHeader()

		// Full set of properties.
		var full bytes.Buffer
		pw := NewPropWriter(&full)
		_, _ = pw.WriteUShort(0)
		_, _ = pw.WriteInt(42)
		_, _ = pw.WriteUShort(1)
		_, _ = pw.WriteString("ok")
		feat1 := pointFeatureBytes(t, flat.GeometryTypePoint, full.Bytes())

		// Sparse properties: only column 1 is present.
		var sparse bytes.Buffer
		pw = NewPropWriter(&sparse)
		_, _ = pw.WriteUShort(1)
		_, _ = pw.WriteString("sparse")
		feat2 := pointFeatureBytes(t, flat.GeometryTypePoint, sparse.Bytes())

		// No properties at all.
		feat3 := pointFeatureBytes(t, flat.GeometryTypePoint, nil)

		hdr.featuresCount = 3
		b := buildValidateFile(t, hdr, nil, feat1, feat2, feat3)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.NoError(t, err)
		assert.Empty(t, rep.Issues)
		assert.Equal(t, 3, rep.ActualFeatures)
	})
}

func TestValidateFile_GeometryTypeConflict(t *testing.T) {
	t.Run("Conflict Is Reported", func(t *testing.T) {
		hdr := mockHeader{
			featuresCount: 1,
			indexNodeSize: uint16Ptr(0),
			geometryType:  flat.GeometryTypePoint,
		}
		feat := pointFeatureBytes(t, flat.GeometryTypePolygon, nil)
		b := buildValidateFile(t, &hdr, nil, feat)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.Error(t, err)
		iss := findIssue(t, rep, IssueCodeGeometryTypeConflict)
		assert.Equal(t, ValidationStageData, iss.Stage)
		assert.Equal(t, 0, iss.FeatureIndex)
	})

	t.Run("Unknown Header Type Never Conflicts", func(t *testing.T) {
		hdr := mockHeader{
			featuresCount: 1,
			indexNodeSize: uint16Ptr(0),
		}
		feat := pointFeatureBytes(t, flat.GeometryTypePolygon, nil)
		b := buildValidateFile(t, &hdr, nil, feat)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.NoError(t, err)
		assert.Empty(t, rep.Issues)
	})

	t.Run("Geometryless Feature Is Allowed", func(t *testing.T) {
		hdr := mockHeader{
			featuresCount: 1,
			indexNodeSize: uint16Ptr(0),
			geometryType:  flat.GeometryTypePoint,
		}
		empty := mockFeature{}
		feat := empty.buildAsBytes()
		b := buildValidateFile(t, &hdr, nil, feat)

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.NoError(t, err)
		assert.Empty(t, rep.Issues)
	})
}

func TestValidateFile_DoesNotChangeFileReader(t *testing.T) {
	mf := mustMockFile(t, "four_points_in_quadrants")

	// Drive a normal FileReader through its header stage and snapshot
	// the internal state.
	r := NewFileReader(bytes.NewReader(append([]byte(nil), mf.allBytes...)))
	hdr, err := r.Header()
	require.NoError(t, err)
	require.NotNil(t, hdr)
	snapshot := struct {
		state         state
		err           error
		numFeatures   int
		nodeSize      uint16
		featureIndex  int
		featureOffset int64
		indexOffset   int64
		dataOffset    int64
	}{
		state:         r.state,
		err:           r.err,
		numFeatures:   r.numFeatures,
		nodeSize:      r.nodeSize,
		featureIndex:  r.featureIndex,
		featureOffset: r.featureOffset,
		indexOffset:   r.indexOffset,
		dataOffset:    r.dataOffset,
	}

	// Validate through an independent stream, both seekable and not.
	for _, seeker := range []bool{true, false} {
		vr := newTestDataBytesReaderFromBytes(mf.allBytes, seeker)
		rep, verr := ValidateFile(vr)
		require.NoError(t, verr)
		require.Empty(t, rep.Issues)
	}

	// The original FileReader must be completely untouched and remain
	// fully functional.
	assert.Equal(t, snapshot.state, r.state)
	assert.Equal(t, snapshot.err, r.err)
	assert.Equal(t, snapshot.numFeatures, r.numFeatures)
	assert.Equal(t, snapshot.nodeSize, r.nodeSize)
	assert.Equal(t, snapshot.featureIndex, r.featureIndex)
	assert.Equal(t, snapshot.featureOffset, r.featureOffset)
	assert.Equal(t, snapshot.indexOffset, r.indexOffset)
	assert.Equal(t, snapshot.dataOffset, r.dataOffset)

	features, err := r.DataRem()
	require.NoError(t, err)
	assert.Len(t, features, 4)
}

// newTestDataBytesReaderFromBytes wraps raw file bytes as either a
// seekable bytes.Reader or a non-seekable reader-only wrapper.
func newTestDataBytesReaderFromBytes(b []byte, seeker bool) io.Reader {
	br := bytes.NewReader(b)
	if seeker {
		return br
	}
	return &readerOnly{r: br}
}

func TestValidateFile_NoPanicOnGarbage(t *testing.T) {
	rnd := rand.New(rand.NewSource(20230526))
	for i := 0; i < 50; i++ {
		n := rnd.Intn(256)
		b := make([]byte, n)
		_, _ = rnd.Read(b)
		// Long enough inputs get a valid magic prefix so that the
		// later stages are exercised too.
		if n >= magicLen {
			copy(b, magic[:])
		}

		rep, err := ValidateFile(bytes.NewReader(b))

		assertReportInvariant(t, rep, err)
		require.NotNil(t, rep)
		if n == 0 {
			assert.True(t, hasIssueCode(rep, IssueCodeEmptyInput))
		} else if n >= magicLen {
			// A random body can never be a valid header or data
			// section, so something must have been flagged.
			require.NotEmpty(t, rep.Issues, "iteration %d: garbage input was reported valid", i)
		}
	}
}

func TestValidateFile_IssueFieldsAreStable(t *testing.T) {
	// A truncated feature gives us an issue with a concrete feature
	// index and byte offset to assert against.
	mf := mustMockFile(t, "one_feature_no_index")
	b := mf.allBytes[:mf.indexOffset+2]

	rep, err := ValidateFile(bytes.NewReader(b))

	require.Error(t, err)
	iss := findIssue(t, rep, IssueCodeFeatureTruncated)
	assert.NotEmpty(t, string(iss.Code))
	assert.NotEmpty(t, string(iss.Stage))
	assert.GreaterOrEqual(t, iss.FeatureIndex, 0)
	assert.GreaterOrEqual(t, iss.ByteOffset, int64(0))
	assert.NotEmpty(t, iss.Message)
}
