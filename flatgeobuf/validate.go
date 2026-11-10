// Copyright 2023 The flatgeobuf (Go) Authors. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package flatgeobuf

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/gogama/flatgeobuf/flatgeobuf/flat"
	"github.com/gogama/flatgeobuf/packedrtree"
	flatbuffers "github.com/google/flatbuffers/go"
)

// ValidationStage identifies the stage of FlatGeobuf file validation
// at which a ValidationIssue was detected.
type ValidationStage string

const (
	// ValidationStageMagic is the validation stage that reads and
	// verifies the 8-byte FlatGeobuf magic number.
	ValidationStageMagic ValidationStage = "magic"
	// ValidationStageHeader is the validation stage that reads and
	// verifies the size-prefixed FlatBuffer header table.
	ValidationStageHeader ValidationStage = "header"
	// ValidationStageIndex is the validation stage that reads and
	// verifies the optional packed Hilbert R-Tree spatial index.
	ValidationStageIndex ValidationStage = "index"
	// ValidationStageData is the validation stage that reads and
	// verifies the feature frames in the data section.
	ValidationStageData ValidationStage = "data"
	// ValidationStageTrailer is the validation stage that inspects
	// anything following the last expected feature frame.
	ValidationStageTrailer ValidationStage = "trailer"
)

// IssueCode is a stable, machine-readable identifier for the kind of
// problem recorded in a ValidationIssue.
type IssueCode string

const (
	// IssueCodeEmptyInput indicates that the input stream contained
	// no bytes at all, not even a magic number.
	IssueCodeEmptyInput IssueCode = "empty_input"
	// IssueCodeReadError indicates that an underlying read failed for
	// a reason not covered by a more specific issue code.
	IssueCodeReadError IssueCode = "read_error"
	// IssueCodeInvalidMagic indicates that the magic number is missing
	// or does not match the FlatGeobuf magic number.
	IssueCodeInvalidMagic IssueCode = "invalid_magic"
	// IssueCodeUnsupportedVersion indicates that the magic number
	// declares a FlatGeobuf specification major version that this
	// package cannot validate.
	IssueCodeUnsupportedVersion IssueCode = "unsupported_spec_version"
	// IssueCodeHeaderLengthInvalid indicates that the header's
	// 4-byte length prefix is zero, too small for a FlatBuffer, or
	// exceeds the package's defensive header size limit.
	IssueCodeHeaderLengthInvalid IssueCode = "header_length_invalid"
	// IssueCodeHeaderCorrupt indicates that the header bytes cannot be
	// parsed as a size-prefixed FlatBuffer Header table.
	IssueCodeHeaderCorrupt IssueCode = "header_corrupt"
	// IssueCodeFeatureCountOverflow indicates that the feature count
	// declared in the header overflows the platform's int type.
	IssueCodeFeatureCountOverflow IssueCode = "feature_count_overflow"
	// IssueCodeNodeSizeInvalid indicates that the header declares an
	// index node size that is not permitted (currently only node size
	// 1 is invalid; node size 0 legitimately means no index).
	IssueCodeNodeSizeInvalid IssueCode = "node_size_invalid"
	// IssueCodeIndexSizeOverflow indicates that the size of the
	// declared spatial index cannot be computed without integer
	// overflow.
	IssueCodeIndexSizeOverflow IssueCode = "index_size_overflow"
	// IssueCodeIndexUnparseable indicates that the header declares an
	// index but the index section cannot be read or parsed.
	IssueCodeIndexUnparseable IssueCode = "index_unparseable"
	// IssueCodeIndexRefCountMismatch indicates that the number of
	// feature references in the spatial index differs from the number
	// of features found in the data section.
	IssueCodeIndexRefCountMismatch IssueCode = "index_ref_count_mismatch"
	// IssueCodeIndexRefInvalid indicates that a spatial index leaf
	// references a data-section byte offset that is not the start of
	// any feature frame.
	IssueCodeIndexRefInvalid IssueCode = "index_ref_invalid"
	// IssueCodeFeatureTruncated indicates that a feature frame's
	// length prefix or body could not be fully read because the input
	// ended prematurely.
	IssueCodeFeatureTruncated IssueCode = "feature_truncated"
	// IssueCodeFeatureFraming indicates that a feature frame's length
	// prefix or FlatBuffer table bytes are structurally invalid.
	IssueCodeFeatureFraming IssueCode = "feature_framing"
	// IssueCodePropertiesUnreadable indicates that a feature's
	// property buffer cannot be decoded according to the applicable
	// column schema.
	IssueCodePropertiesUnreadable IssueCode = "properties_unreadable"
	// IssueCodeGeometryTypeConflict indicates that a feature's
	// geometry type clearly conflicts with the geometry type declared
	// in the header.
	IssueCodeGeometryTypeConflict IssueCode = "geometry_type_conflict"
	// IssueCodeFeatureCountMismatch indicates that the number of
	// feature frames in the data section differs from the feature
	// count declared in the header.
	IssueCodeFeatureCountMismatch IssueCode = "feature_count_mismatch"
	// IssueCodeTrailingBytes indicates that bytes remain after all
	// features declared by the header have been consumed.
	IssueCodeTrailingBytes IssueCode = "trailing_bytes"
	// IssueCodeInternalPanic is a defensive issue recorded when
	// validation unexpectedly panics. ValidateFile never propagates a
	// panic; it converts the panic into this issue and an error.
	IssueCodeInternalPanic IssueCode = "internal_panic"
)

const (
	// noFeatureIndex is the ValidationIssue.FeatureIndex value used
	// when an issue is not associated with a specific feature.
	noFeatureIndex = -1
	// noByteOffset is the ValidationIssue.ByteOffset value used when
	// an issue has no meaningful byte offset.
	noByteOffset = int64(-1)
	// validationFeatureMaxLen is a defensive limit on the declared
	// length of a single feature frame. It exists to prevent a
	// corrupted length prefix from triggering a gigantic allocation.
	validationFeatureMaxLen = 1 << 30
)

// ValidationIssue describes a single structural or semantic problem
// found while validating a FlatGeobuf file.
type ValidationIssue struct {
	// Code is the stable identifier of the kind of problem.
	Code IssueCode
	// Stage is the validation stage at which the problem was detected.
	Stage ValidationStage
	// FeatureIndex is the zero-based index of the offending feature
	// within the data section, or -1 if the issue is not associated
	// with a specific feature.
	FeatureIndex int
	// ByteOffset is the absolute byte offset within the validated
	// file at which the problem was detected, or -1 if the issue has
	// no meaningful byte offset.
	ByteOffset int64
	// Message is a human-readable description of the problem.
	Message string
}

// ByteRange is the half-open range [Start, End) of absolute byte
// offsets occupied by a section of a FlatGeobuf file.
type ByteRange struct {
	// Start is the byte offset of the first byte of the section.
	Start int64
	// End is the byte offset one past the last byte of the section.
	End int64
}

// ValidationReport is the structured result of a ValidateFile call.
//
// A report is always returned, even when validation fails early. In
// that case fields that could not be determined retain their zero
// values and the Issues slice describes what went wrong.
type ValidationReport struct {
	// Version is the FlatGeobuf specification version read from the
	// magic number. It retains its zero value if the magic number
	// could not be read.
	Version SpecVersion
	// DeclaredFeatures is the feature count recorded in the header.
	// A zero value means the header does not declare a feature count.
	DeclaredFeatures uint64
	// ActualFeatures is the number of complete feature frames
	// observed in the data section, including frames whose contents
	// are themselves invalid.
	ActualFeatures int
	// HasIndex reports whether a spatial index section was declared
	// and successfully located in the file.
	HasIndex bool
	// NodeSize is the index node size recorded in the header. A zero
	// value means the file has no index.
	NodeSize uint16
	// IndexBounds is the byte range occupied by the spatial index.
	// Both fields are zero when the file has no index or the index
	// bounds could not be determined.
	IndexBounds ByteRange
	// DataBounds is the byte range occupied by the data section. End
	// reflects how far validation actually read and may be smaller
	// than the true section end when the input was truncated.
	DataBounds ByteRange
	// Issues lists every problem detected during validation, in the
	// order it was detected. A valid file has no issues.
	Issues []ValidationIssue
}

// ValidateFile performs a single-pass, non-destructive validation of
// the FlatGeobuf file readable from r. The reader must be positioned
// at the beginning of the file, immediately before the magic number.
//
// Validation proceeds from the start of the file and checks the magic
// number, the header, the optional spatial index, feature count and
// feature framing, property buffers against the column schema,
// geometry types against the header, and any trailing bytes.
//
// ValidateFile never panics and never mutates any FileReader: it only
// reads from the stream it is given. When any read error or format
// problem is detected, the returned error is non-nil while the
// returned report still retains every field that could be determined
// before validation stopped. A corrupt file is never reported as
// valid: a non-nil error is returned whenever the report contains any
// issues.
func ValidateFile(r io.Reader) (report *ValidationReport, err error) {
	v := &validator{
		report: &ValidationReport{},
	}

	// Defensive top-level guard so that ValidateFile can never panic,
	// even if a third-party FlatBuffer or packed R-Tree interaction
	// misbehaves on corrupted input.
	defer func() {
		if rec := recover(); rec != nil {
			stage := v.stage
			if stage == "" {
				stage = ValidationStageMagic
			}
			v.issue(IssueCodeInternalPanic, stage, noFeatureIndex, v.pos(),
				"validation aborted by panic: %v", rec)
			v.errs = append(v.errs, fmtErr("validation panic: %v", rec))
			report = v.report
			err = v.finalErr()
		}
	}()

	if r == nil {
		v.issue(IssueCodeReadError, ValidationStageMagic, noFeatureIndex, 0, "nil reader")
		return v.report, v.finalErr()
	}
	v.cr = &countingReader{r: r}

	// Stage 1: magic number.
	if !v.validateMagic() {
		return v.report, v.finalErr()
	}

	// Stage 2: header table.
	headerEnd, ok := v.validateHeader()
	if !ok {
		return v.report, v.finalErr()
	}

	// Stage 3: optional spatial index.
	if !v.validateIndex(headerEnd) {
		return v.report, v.finalErr()
	}

	// Stage 4: data section feature frames, plus stage 5 trailer and
	// cross-section consistency checks.
	v.validateData()

	return v.report, v.finalErr()
}

// validator carries the mutable state of one ValidateFile invocation.
type validator struct {
	// cr is the position-counting reader wrapped around the caller's
	// stream.
	cr *countingReader
	// report is the accumulating validation report.
	report *ValidationReport
	// hdr is the parsed header table, nil until the header stage
	// succeeds.
	hdr *flat.Header
	// index is the parsed spatial index, nil when the file has no
	// index or the index could not be parsed.
	index *packedrtree.PackedRTree
	// featureOffsets holds the data-section-relative start offset of
	// every complete feature frame observed, in encounter order.
	featureOffsets []int64
	// stage is the validation stage currently executing. It is used
	// to place a defensive panic issue at the correct stage.
	stage ValidationStage
	// errs holds the causal errors (typically I/O errors) that
	// accompany the recorded issues.
	errs []error
}

// pos returns the current absolute read position.
func (v *validator) pos() int64 {
	if v.cr == nil {
		return 0
	}
	return v.cr.pos
}

// issue records a validation issue and returns the recorded value.
func (v *validator) issue(code IssueCode, stage ValidationStage, featureIndex int, byteOffset int64, format string, a ...any) ValidationIssue {
	i := ValidationIssue{
		Code:         code,
		Stage:        stage,
		FeatureIndex: featureIndex,
		ByteOffset:   byteOffset,
		Message:      fmt.Sprintf(format, a...),
	}
	v.report.Issues = append(v.report.Issues, i)
	return i
}

// fail records a validation issue together with its causal error.
func (v *validator) fail(code IssueCode, stage ValidationStage, featureIndex int, byteOffset int64, cause error, format string, a ...any) {
	v.issue(code, stage, featureIndex, byteOffset, format, a...)
	if cause != nil {
		v.errs = append(v.errs, cause)
	}
}

// finalErr builds the error returned from ValidateFile. It joins all
// causal errors and, when any issue was recorded, adds a summary
// error so that a report containing issues always yields a non-nil
// error.
func (v *validator) finalErr() error {
	if len(v.report.Issues) > 0 {
		v.errs = append(v.errs, fmtErr("validation failed with %d issue(s)", len(v.report.Issues)))
	}
	return errors.Join(v.errs...)
}

// validateMagic reads and verifies the 8-byte magic number. It returns
// false when validation cannot continue.
func (v *validator) validateMagic() bool {
	v.stage = ValidationStageMagic
	b := make([]byte, magicLen)
	n, err := io.ReadFull(v.cr, b)
	if err != nil {
		switch {
		case errors.Is(err, io.EOF) && n == 0:
			v.fail(IssueCodeEmptyInput, ValidationStageMagic, noFeatureIndex, 0, err,
				"input is empty: no magic number")
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
			v.fail(IssueCodeInvalidMagic, ValidationStageMagic, noFeatureIndex, 0, err,
				"input ended after %d of %d magic-number bytes", n, magicLen)
		default:
			v.fail(IssueCodeReadError, ValidationStageMagic, noFeatureIndex, int64(n), err,
				"failed to read magic number")
		}
		return false
	}

	if b[0] != magic[0] || b[1] != magic[1] || b[2] != magic[2] ||
		b[4] != magic[4] || b[5] != magic[5] || b[6] != magic[6] {
		v.issue(IssueCodeInvalidMagic, ValidationStageMagic, noFeatureIndex, 0,
			"invalid magic number % x, want % x", b, magic[:])
		return false
	}

	version := SpecVersion{Major: b[3], Patch: b[7]}
	v.report.Version = version
	if version.Major < MinSpecMajorVersion || version.Major > MaxSpecMajorVersion {
		v.issue(IssueCodeUnsupportedVersion, ValidationStageMagic, noFeatureIndex, 0,
			"magic number declares unsupported spec major version %d; supported range is [%d, %d]",
			version.Major, MinSpecMajorVersion, MaxSpecMajorVersion)
		return false
	}

	return true
}

// validateHeader reads and verifies the size-prefixed header table.
// It returns the absolute byte offset immediately following the
// header and true on success, otherwise zero and false.
func (v *validator) validateHeader() (int64, bool) {
	v.stage = ValidationStageHeader
	headerStart := v.pos()

	// Read the 4-byte little-endian header length.
	lengthBuf := make([]byte, flatbuffers.SizeUint32)
	if _, err := io.ReadFull(v.cr, lengthBuf); err != nil {
		v.fail(IssueCodeReadError, ValidationStageHeader, noFeatureIndex, headerStart, err,
			"failed to read header length prefix")
		return 0, false
	}
	headerLen := flatbuffers.GetUint32(lengthBuf)
	if headerLen < flatbuffers.SizeUOffsetT {
		v.issue(IssueCodeHeaderLengthInvalid, ValidationStageHeader, noFeatureIndex, headerStart,
			"header length %d is smaller than minimum FlatBuffer uoffset_t size %d",
			headerLen, flatbuffers.SizeUOffsetT)
		return 0, false
	}
	if headerLen > headerMaxLen {
		v.issue(IssueCodeHeaderLengthInvalid, ValidationStageHeader, noFeatureIndex, headerStart,
			"header length %d exceeds limit of %d bytes", headerLen, headerMaxLen)
		return 0, false
	}

	// Read the header table bytes, preserving the size prefix so the
	// buffer can be parsed as a size-prefixed root table.
	tbl := make([]byte, flatbuffers.SizeUint32+headerLen)
	copy(tbl, lengthBuf)
	if _, err := io.ReadFull(v.cr, tbl[flatbuffers.SizeUint32:]); err != nil {
		v.fail(IssueCodeHeaderCorrupt, ValidationStageHeader, noFeatureIndex, headerStart, err,
			"failed to read header table of declared length %d", headerLen)
		return 0, false
	}

	var hdr *flat.Header
	if err := safeFlatBuffersInteraction(func() error {
		hdr = flat.GetSizePrefixedRootAsHeader(tbl, 0)
		// Touch every field the validator depends on so that any
		// out-of-bounds access is trapped here rather than later.
		_ = hdr.FeaturesCount()
		_ = hdr.IndexNodeSize()
		_ = hdr.GeometryType()
		_ = hdr.ColumnsLength()
		return nil
	}); err != nil {
		v.fail(IssueCodeHeaderCorrupt, ValidationStageHeader, noFeatureIndex, headerStart, err,
			"header bytes are not a valid FlatBuffer Header table")
		return 0, false
	}
	v.hdr = hdr

	numFeatures := hdr.FeaturesCount()
	nodeSize := hdr.IndexNodeSize()
	v.report.DeclaredFeatures = numFeatures
	v.report.NodeSize = nodeSize

	if numFeatures > math.MaxInt {
		v.issue(IssueCodeFeatureCountOverflow, ValidationStageHeader, noFeatureIndex, headerStart,
			"header feature count %d overflows limit of %d features", numFeatures, math.MaxInt)
		return 0, false
	}
	// Node size 0 is legitimate: it means there is no index. Node
	// size 1 is never legal.
	if nodeSize == 1 {
		v.issue(IssueCodeNodeSizeInvalid, ValidationStageHeader, noFeatureIndex, headerStart,
			"header index node size 1 is not allowed")
		return 0, false
	}

	return v.pos(), true
}

// validateIndex reads and verifies the optional spatial index. It
// records the index and data section bounds in the report and returns
// false when validation cannot continue.
func (v *validator) validateIndex(headerEnd int64) bool {
	v.stage = ValidationStageIndex
	numFeatures := int(v.report.DeclaredFeatures)
	nodeSize := v.report.NodeSize

	// No index when the header says so, or when the feature count is
	// unknown (zero), in which case an index cannot exist either.
	if nodeSize == 0 || numFeatures == 0 {
		v.report.DataBounds = ByteRange{Start: headerEnd}
		return true
	}

	indexSize, err := packedrtree.Size(numFeatures, nodeSize)
	if err != nil {
		v.fail(IssueCodeIndexSizeOverflow, ValidationStageIndex, noFeatureIndex, headerEnd, err,
			"failed to compute spatial index size for %d features and node size %d",
			numFeatures, nodeSize)
		return false
	}

	buf := make([]byte, indexSize)
	if _, err = io.ReadFull(v.cr, buf); err != nil {
		v.report.IndexBounds = ByteRange{Start: headerEnd, End: v.pos()}
		v.fail(IssueCodeIndexUnparseable, ValidationStageIndex, noFeatureIndex, headerEnd, err,
			"header declares an index of %d bytes but the index section cannot be fully read", indexSize)
		return false
	}

	var prt *packedrtree.PackedRTree
	if err = safeFlatBuffersInteraction(func() error {
		var e error
		prt, e = packedrtree.Unmarshal(bytes.NewReader(buf), numFeatures, nodeSize)
		return e
	}); err != nil {
		v.report.IndexBounds = ByteRange{Start: headerEnd, End: headerEnd + int64(indexSize)}
		v.fail(IssueCodeIndexUnparseable, ValidationStageIndex, noFeatureIndex, headerEnd, err,
			"index section cannot be parsed as a packed Hilbert R-Tree")
		return false
	}

	v.index = prt
	v.report.HasIndex = true
	v.report.IndexBounds = ByteRange{Start: headerEnd, End: headerEnd + int64(indexSize)}
	v.report.DataBounds = ByteRange{Start: v.pos()}
	return true
}

// validateData scans every feature frame in the data section and then
// runs the trailer and cross-section consistency checks.
func (v *validator) validateData() {
	v.stage = ValidationStageData
	dataStart := v.report.DataBounds.Start
	var trailingStart int64 = noByteOffset
	fatal := false

	for featureIndex := 0; ; featureIndex++ {
		frameStart := v.pos()

		// Read the 4-byte little-endian feature length. A clean EOF
		// here means the data section ended normally.
		lengthBuf := make([]byte, flatbuffers.SizeUint32)
		n, err := io.ReadFull(v.cr, lengthBuf)
		if errors.Is(err, io.EOF) && n == 0 {
			break
		}
		if err != nil {
			v.fail(IssueCodeFeatureTruncated, ValidationStageData, featureIndex, frameStart, err,
				"feature[%d] length prefix is truncated at byte offset %d", featureIndex, frameStart)
			fatal = true
			break
		}
		featureLen := flatbuffers.GetUint32(lengthBuf)
		if featureLen < flatbuffers.SizeUOffsetT {
			v.issue(IssueCodeFeatureFraming, ValidationStageData, featureIndex, frameStart,
				"feature[%d] declared length %d is smaller than minimum FlatBuffer uoffset_t size %d",
				featureIndex, featureLen, flatbuffers.SizeUOffsetT)
			fatal = true
			break
		}
		if featureLen > validationFeatureMaxLen {
			v.issue(IssueCodeFeatureFraming, ValidationStageData, featureIndex, frameStart,
				"feature[%d] declared length %d exceeds validation limit of %d bytes",
				featureIndex, featureLen, validationFeatureMaxLen)
			fatal = true
			break
		}

		// Read the feature table body.
		tbl := make([]byte, flatbuffers.SizeUint32+featureLen)
		copy(tbl, lengthBuf)
		if _, err = io.ReadFull(v.cr, tbl[flatbuffers.SizeUint32:]); err != nil {
			v.fail(IssueCodeFeatureTruncated, ValidationStageData, featureIndex, frameStart, err,
				"feature[%d] body is truncated: declared length %d at byte offset %d",
				featureIndex, featureLen, frameStart)
			fatal = true
			break
		}

		// A frame starting after the declared count is trailing data.
		if v.report.DeclaredFeatures > 0 &&
			uint64(featureIndex) >= v.report.DeclaredFeatures && trailingStart == noByteOffset {
			trailingStart = frameStart
		}

		// The frame itself is complete, so it counts as an observed
		// feature even if its contents turn out to be invalid.
		v.featureOffsets = append(v.featureOffsets, frameStart-dataStart)
		v.validateFeatureFrame(featureIndex, tbl, frameStart)
		v.report.ActualFeatures++
	}

	v.report.DataBounds.End = v.pos()

	// Index reference verification requires the complete data
	// section, so it is skipped when a fatal truncation occurred.
	if !fatal {
		v.validateIndexRefs(dataStart)
	}
	v.validateCounts(trailingStart)
}

// validateFeatureFrame inspects one completely read feature frame for
// FlatBuffer structure, geometry type consistency and property buffer
// decodability.
func (v *validator) validateFeatureFrame(featureIndex int, tbl []byte, frameStart int64) {
	var f *flat.Feature
	if err := safeFlatBuffersInteraction(func() error {
		f = flat.GetSizePrefixedRootAsFeature(tbl, 0)
		// Force resolution of the root table so that structural
		// corruption is trapped here.
		_ = f.PropertiesLength()
		_ = f.ColumnsLength()
		return nil
	}); err != nil {
		v.fail(IssueCodeFeatureFraming, ValidationStageData, featureIndex, frameStart, err,
			"feature[%d] bytes are not a valid FlatBuffer Feature table", featureIndex)
		return
	}

	v.validateGeometryType(featureIndex, f, frameStart)
	v.validateFeatureProperties(featureIndex, f, frameStart)
}

// validateGeometryType checks that a feature's geometry type does not
// clearly conflict with the geometry type declared in the header.
func (v *validator) validateGeometryType(featureIndex int, f *flat.Feature, frameStart int64) {
	if err := safeFlatBuffersInteraction(func() error {
		headerType := v.hdr.GeometryType()
		// An Unknown header type makes no promise about feature
		// geometry types, so there cannot be a conflict.
		if headerType == flat.GeometryTypeUnknown {
			return nil
		}
		g := f.Geometry(nil)
		// A feature without a geometry (for example a
		// properties-only feature) is always allowed.
		if g == nil {
			return nil
		}
		featureType := g.Type()
		// An Unknown feature geometry type makes no concrete claim.
		if featureType == flat.GeometryTypeUnknown || featureType == headerType {
			return nil
		}
		v.issue(IssueCodeGeometryTypeConflict, ValidationStageData, featureIndex, frameStart,
			"feature[%d] geometry type %s conflicts with header geometry type %s",
			featureIndex, featureType, headerType)
		return nil
	}); err != nil {
		v.fail(IssueCodeFeatureFraming, ValidationStageData, featureIndex, frameStart, err,
			"feature[%d] geometry cannot be read", featureIndex)
	}
}

// validateFeatureProperties checks that the feature's property buffer
// can be decoded according to the applicable column schema.
func (v *validator) validateFeatureProperties(featureIndex int, f *flat.Feature, frameStart int64) {
	var props []byte
	if err := safeFlatBuffersInteraction(func() error {
		props = f.PropertiesBytes()
		return nil
	}); err != nil {
		v.fail(IssueCodePropertiesUnreadable, ValidationStageData, featureIndex, frameStart, err,
			"feature[%d] property vector cannot be read", featureIndex)
		return
	}
	if len(props) == 0 {
		return
	}

	// A feature may carry its own column schema; otherwise the header
	// schema applies.
	var schema Schema = v.hdr
	featureSchema := f.ColumnsLength() > 0
	if featureSchema {
		schema = f
	}

	if err := validatePropertyBuffer(props, schema); err != nil {
		v.fail(IssueCodePropertiesUnreadable, ValidationStageData, featureIndex, frameStart, err,
			"feature[%d] property buffer cannot be decoded according to the %s column schema",
			featureIndex, map[bool]string{true: "feature", false: "header"}[featureSchema])
	}
}

// validatePropertyBuffer decodes a FlatGeobuf property buffer against
// the given column schema. Properties are sparse (key-indexed), so the
// buffer is consumed pair by pair until exhausted rather than assuming
// one value per schema column.
//
// A single trailing byte is tolerated rather than reported. This
// matches the canonical FlatGeobuf readers, whose property loops run
// while at least two bytes remain: real-world files exist whose final
// Binary property is followed by a lone trailing byte. Any other
// truncation, such as a column key without its value or a value that
// runs past the buffer, remains an error.
func validatePropertyBuffer(props []byte, schema Schema) error {
	br := bytes.NewReader(props)
	pr := NewPropReader(br)

	numColumns := 0
	if err := safeFlatBuffersInteraction(func() error {
		numColumns = schema.ColumnsLength()
		return nil
	}); err != nil {
		return fmt.Errorf("failed to read schema column count: %w", err)
	}

	for br.Len() >= flatbuffers.SizeUint16 {
		colIndex, err := pr.ReadUShort()
		if err != nil {
			return fmt.Errorf("failed to read property column index: %w", err)
		}
		if int(colIndex) >= numColumns {
			return fmt.Errorf("property column index %d is out of range for schema with %d column(s)", colIndex, numColumns)
		}

		var col flat.Column
		if err = safeFlatBuffersInteraction(func() error {
			if !schema.Columns(&col, int(colIndex)) {
				return errors.New("column not found in schema")
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to resolve column %d: %w", colIndex, err)
		}

		if err = readPropertyValue(br, pr, col.Type(), colIndex); err != nil {
			return err
		}
	}

	return nil
}

// readPropertyValue consumes one property value of the given column
// type from the property buffer, discarding the value.
func readPropertyValue(br *bytes.Reader, pr *PropReader, colType flat.ColumnType, colIndex uint16) error {
	switch colType {
	case flat.ColumnTypeByte:
		_, err := pr.ReadByte()
		return err
	case flat.ColumnTypeUByte:
		_, err := pr.ReadUByte()
		return err
	case flat.ColumnTypeBool:
		_, err := pr.ReadBool()
		return err
	case flat.ColumnTypeShort:
		_, err := pr.ReadShort()
		return err
	case flat.ColumnTypeUShort:
		_, err := pr.ReadUShort()
		return err
	case flat.ColumnTypeInt:
		_, err := pr.ReadInt()
		return err
	case flat.ColumnTypeUInt:
		_, err := pr.ReadUInt()
		return err
	case flat.ColumnTypeLong:
		_, err := pr.ReadLong()
		return err
	case flat.ColumnTypeULong:
		_, err := pr.ReadULong()
		return err
	case flat.ColumnTypeFloat:
		_, err := pr.ReadFloat()
		return err
	case flat.ColumnTypeDouble:
		_, err := pr.ReadDouble()
		return err
	case flat.ColumnTypeString, flat.ColumnTypeDateTime, flat.ColumnTypeJson, flat.ColumnTypeBinary:
		// Read the 4-byte value length manually so that a corrupted
		// length cannot trigger a gigantic allocation. The value can
		// never be longer than the bytes remaining in the buffer.
		lengthBuf := make([]byte, flatbuffers.SizeUint32)
		if _, err := io.ReadFull(br, lengthBuf); err != nil {
			return fmt.Errorf("failed to read length of column %d value: %w", colIndex, err)
		}
		n := flatbuffers.GetUint32(lengthBuf)
		if uint64(n) > uint64(br.Len()) {
			return fmt.Errorf("column %d value length %d exceeds remaining property buffer of %d byte(s)",
				colIndex, n, br.Len())
		}
		if _, err := io.CopyN(io.Discard, br, int64(n)); err != nil {
			return fmt.Errorf("failed to read column %d value body: %w", colIndex, err)
		}
		return nil
	default:
		return fmt.Errorf("column %d has unknown column type %d", colIndex, colType)
	}
}

// validateIndexRefs verifies that every spatial index leaf points at
// the start of an actual feature frame and that the index reference
// count matches the observed feature count.
func (v *validator) validateIndexRefs(dataStart int64) {
	if v.index == nil {
		return
	}
	v.stage = ValidationStageIndex

	// A query box spanning the whole numeric plane matches every
	// finite bounding box in the index.
	var results packedrtree.Results
	if err := safeFlatBuffersInteraction(func() error {
		results = v.index.Search(packedrtree.Box{
			XMin: math.Inf(-1),
			YMin: math.Inf(-1),
			XMax: math.Inf(1),
			YMax: math.Inf(1),
		})
		return nil
	}); err != nil {
		v.fail(IssueCodeIndexUnparseable, ValidationStageIndex, noFeatureIndex, v.report.IndexBounds.Start, err,
			"spatial index cannot be searched")
		return
	}

	if len(results) != v.report.ActualFeatures {
		v.issue(IssueCodeIndexRefCountMismatch, ValidationStageIndex, noFeatureIndex, v.report.IndexBounds.Start,
			"spatial index contains %d feature reference(s) but data section contains %d feature(s)",
			len(results), v.report.ActualFeatures)
	}

	knownOffsets := make(map[int64]bool, len(v.featureOffsets))
	for _, off := range v.featureOffsets {
		knownOffsets[off] = true
	}
	for _, res := range results {
		if res.Offset < 0 || !knownOffsets[res.Offset] {
			v.issue(IssueCodeIndexRefInvalid, ValidationStageIndex, res.RefIndex, dataStart+res.Offset,
				"index leaf %d references data-section offset %d, which is not the start of any feature frame",
				res.RefIndex, res.Offset)
		}
	}
}

// validateCounts performs the feature count and trailing-bytes checks
// that only become known after the whole data section has been
// scanned.
func (v *validator) validateCounts(trailingStart int64) {
	v.stage = ValidationStageTrailer
	if v.report.DeclaredFeatures > 0 &&
		v.report.DeclaredFeatures != uint64(v.report.ActualFeatures) {
		v.issue(IssueCodeFeatureCountMismatch, ValidationStageData, noFeatureIndex, v.report.DataBounds.Start,
			"header declares %d feature(s) but data section contains %d feature frame(s)",
			v.report.DeclaredFeatures, v.report.ActualFeatures)
	}
	if trailingStart != noByteOffset {
		v.issue(IssueCodeTrailingBytes, ValidationStageTrailer, int(v.report.DeclaredFeatures), trailingStart,
			"unexpected bytes remain after the %d feature(s) declared by the header",
			v.report.DeclaredFeatures)
	}
}

// countingReader wraps an io.Reader and tracks the cumulative number
// of bytes read, giving every validation stage absolute byte offsets
// within the file.
type countingReader struct {
	r   io.Reader
	pos int64
}

// Read implements io.Reader.
func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.pos += int64(n)
	return n, err
}
