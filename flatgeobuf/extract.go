// Copyright 2023 The flatgeobuf (Go) Authors. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package flatgeobuf

import (
	"io"

	"github.com/gogama/flatgeobuf/flatgeobuf/flat"
	"github.com/gogama/flatgeobuf/packedrtree"
	flatbuffers "github.com/google/flatbuffers/go"
)

// extractBatchSize is the number of source features read from the
// input stream in a single FileReader.Data call during an extraction.
const extractBatchSize = 256

// ExtractOptions configures the behavior of WriteExtract.
//
// A feature is selected for extraction only if it satisfies every
// configured filter. When BBox and Predicate are both unset, every
// source feature is selected.
type ExtractOptions struct {
	// BBox, when non-nil, restricts extraction to features whose
	// geometry bounding box intersects the given 2D box. A nil value
	// means no spatial filtering is performed. Features that have no
	// geometry never match a spatial filter.
	BBox *packedrtree.Box

	// Predicate, when non-nil, is invoked once for every source
	// feature that passed the BBox filter (or for every source
	// feature when BBox is nil). The feature is selected only when
	// the predicate returns true. If the predicate returns an error,
	// the extraction is aborted and the error is reported, wrapped,
	// from WriteExtract. A nil value means no custom filtering is
	// performed.
	Predicate func(f *flat.Feature) (bool, error)

	// MaxFeatures caps the number of selected features written to the
	// output file. A zero value means there is no cap. A positive
	// value stops extraction as soon as the cap is reached; if the
	// source still contains unread features at that point, the
	// ExtractResult.Truncated flag is set to true. A negative value
	// is invalid and causes WriteExtract to return an error.
	MaxFeatures int
}

// ExtractResult reports the outcome of a WriteExtract call.
//
// When WriteExtract returns a non-nil error, the extraction did not
// complete successfully and the destination contains partial,
// unusable data which the caller must discard. The counter fields are
// still populated in that case, on a best-effort basis, to aid
// diagnostics, but the result must not be treated as a success.
type ExtractResult struct {
	// Scanned is the number of source features examined, in source
	// file order, before the extraction finished or aborted.
	Scanned int

	// Matched is the number of source features that satisfied all
	// configured filters and were (or were about to be) written to
	// the output file.
	Matched int

	// BytesWritten is the total number of bytes written to the
	// destination stream, including the magic number, header and,
	// when present, the index and data sections.
	BytesWritten int64

	// Truncated is true if and only if a positive MaxFeatures cap
	// was reached while the source still contained at least one
	// unread feature. It is always false when MaxFeatures is zero.
	Truncated bool
}

// WriteExtract reads a FlatGeobuf stream, selects a subset of its
// features, and writes the selection as a new, independently readable
// FlatGeobuf file supporting random access.
//
// Source features are scanned once, in stable source file order. Each
// feature is tested against the filters in opts and every selected
// feature is retained for the output. The output header copies the
// source's name, title, description, coordinate reference system,
// dimension flags (HasZ, HasM, HasT, HasTm), geometry type, metadata
// and column definitions, while its feature count and index are
// rebuilt from the selected features and its envelope is recomputed
// from their bounds. When the source header indicates an index, the
// rebuilt index uses the same index node size; when there are no
// selected features, the output is a legal empty file with no index.
//
// WriteExtract does not close either stream; ownership of r and w
// remains with the caller. On failure the destination stream may hold
// a partial file and the returned error is always non-nil, so a
// partial result can never be mistaken for a success.
//
// WriteExtract panics if r or w is nil, mirroring NewFileReader and
// NewFileWriter. It returns an error, without writing anything, if
// opts.MaxFeatures is negative.
func WriteExtract(r io.Reader, w io.Writer, opts ExtractOptions) (ExtractResult, error) {
	if r == nil {
		textPanic("nil reader")
	}
	if w == nil {
		textPanic("nil writer")
	}
	if opts.MaxFeatures < 0 {
		return ExtractResult{}, fmtErr("MaxFeatures must be non-negative, got %d", opts.MaxFeatures)
	}

	// Open the source and read its header.
	rd := NewFileReader(r)
	srcHeader, err := rd.Header()
	if err != nil {
		return ExtractResult{}, wrapErr("failed to read source header", err)
	}
	var srcNodeSize uint16
	var srcCount int
	if err = safeFlatBuffersInteraction(func() error {
		srcNodeSize = srcHeader.IndexNodeSize()
		srcCount = int(srcHeader.FeaturesCount())
		return nil
	}); err != nil {
		return ExtractResult{}, wrapErr("failed to read source header fields", err)
	}

	// Scan the data section once, in stable file order, retaining only
	// the selected features and accumulating their overall envelope.
	var res ExtractResult
	selected := make([]flat.Feature, 0)
	envelope := packedrtree.EmptyBox
	hasEnvelope := false
	capReached := false

	batch := make([]flat.Feature, extractBatchSize)
scanLoop:
	for {
		n, dataErr := rd.Data(batch)
		for i := 0; i < n; i++ {
			res.Scanned++
			f := &batch[i]

			keep, err := opts.featureMatches(f)
			if err != nil {
				return res, wrapErr("predicate failed on feature %d", err, res.Scanned-1)
			}
			if !keep {
				continue
			}

			selected = append(selected, *f)
			if b, ok, err := extractFeatureBox(f); err != nil {
				return res, wrapErr("failed to compute bounds of feature %d", err, res.Scanned-1)
			} else if ok {
				envelope.Expand(&b)
				hasEnvelope = true
			}

			// Stop as soon as the positive feature cap is reached. The
			// Truncated flag is decided from the read position rather
			// than guesswork: unexamined features already buffered in
			// the current batch prove truncation, an EOF on the current
			// batch proves completion, and otherwise one more feature is
			// read to probe the end of stream.
			if opts.MaxFeatures > 0 && len(selected) == opts.MaxFeatures {
				switch {
				case dataErr != nil && dataErr != io.EOF:
					return res, wrapErr("failed to read source feature %d", dataErr, res.Scanned)
				case i+1 < n:
					res.Truncated = true
				case dataErr == io.EOF:
					res.Truncated = false
				default:
					probe := make([]flat.Feature, 1)
					pn, perr := rd.Data(probe)
					if perr != nil && perr != io.EOF {
						return res, wrapErr("failed while probing for features after reaching MaxFeatures", perr)
					}
					res.Truncated = pn > 0
				}
				capReached = true
				break scanLoop
			}
		}
		if dataErr == io.EOF {
			break
		}
		if dataErr != nil {
			return res, wrapErr("failed to read source feature %d", dataErr, res.Scanned)
		}
	}

	// A seekable source whose header promised a known feature count may
	// surface a truncated data section as a plain EOF (the reader simply
	// reaches end of stream); detect that explicitly so a cut-short
	// input is never silently turned into a smaller valid file.
	if !capReached && srcCount > 0 && res.Scanned < srcCount {
		return res, wrapErr("truncated source data: scanned %d of %d header-indicated features",
			io.ErrUnexpectedEOF, res.Scanned, srcCount)
	}

	res.Matched = len(selected)

	// Rebuild the header from the source metadata and the actual
	// selection. An empty selection is normalized to a legal empty
	// file with feature count zero and no index section.
	outNodeSize := srcNodeSize
	var envelopeValues []float64
	if res.Matched == 0 {
		outNodeSize = 0
	} else if hasEnvelope {
		envelopeValues = []float64{envelope.XMin, envelope.YMin, envelope.XMax, envelope.YMax}
	}
	headerBytes, err := buildExtractHeader(srcHeader, uint64(res.Matched), outNodeSize, envelopeValues)
	if err != nil {
		return res, wrapErr("failed to build output header", err)
	}
	outHeader := flat.GetSizePrefixedRootAsHeader(headerBytes, 0)

	// Write the output file. Any failure leaves the destination with
	// partial data and is returned as a non-nil error.
	wr := NewFileWriter(w)
	n, err := wr.Header(outHeader)
	res.BytesWritten += int64(n)
	if err != nil {
		return res, wrapErr("failed to write output header", err)
	}
	if outNodeSize > 0 {
		// Build the index from references whose offsets and leaf order
		// both follow the stable data order, rather than Hilbert-sorting
		// them. A PackedRTree does not require Hilbert ordering for
		// correctness, whereas the streaming FileReader.IndexSearch path
		// resolves hits by skipping sequentially through the data
		// section, which requires leaf order to match the ascending
		// data offsets. Keeping them aligned makes the output file
		// independently and randomly accessible without reordering the
		// selected features away from source file order.
		refs := make([]packedrtree.Ref, len(selected))
		var offset int64
		if err = safeFlatBuffersInteraction(func() error {
			for i := range selected {
				size, err := tableSize(selected[i].Table())
				if err != nil {
					return wrapErr("feature %d", err, i)
				}
				refs[i].Offset = offset
				if err = featureBoundsInOrder(&refs[i].Box, &selected[i]); err != nil {
					return wrapErr("feature %d", err, i)
				}
				offset += int64(size) + flatbuffers.SizeUint32
			}
			return nil
		}); err != nil {
			return res, wrapErr("failed to build output index", err)
		}
		index, err := packedrtree.New(refs, outNodeSize)
		if err != nil {
			return res, wrapErr("failed to build output index", err)
		}
		n, err = wr.Index(index)
		res.BytesWritten += int64(n)
		if err != nil {
			return res, wrapErr("failed to write output index", err)
		}
		n, err = wr.Data(selected)
	} else {
		n, err = wr.Data(selected)
	}
	res.BytesWritten += int64(n)
	if err != nil {
		return res, wrapErr("failed to write extracted features", err)
	}

	return res, nil
}

// featureBoundsInOrder computes a feature's 2D bounds, including nested
// geometry parts, for index construction.
func featureBoundsInOrder(b *packedrtree.Box, f *flat.Feature) error {
	return safeFlatBuffersInteraction(func() error {
		var g flat.Geometry
		if f.Geometry(&g) == nil {
			return nil
		}
		*b = packedrtree.EmptyBox
		accumulateGeometryBox(b, &g)
		return nil
	})
}

// featureMatches reports whether a feature satisfies every filter
// configured in the receiving options.
func (opts ExtractOptions) featureMatches(f *flat.Feature) (bool, error) {
	if opts.BBox != nil {
		b, ok, err := extractFeatureBox(f)
		if err != nil {
			return false, err
		}
		if !ok || !boxesIntersect(&b, opts.BBox) {
			return false, nil
		}
	}
	if opts.Predicate != nil {
		ok, err := opts.Predicate(f)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
	}
	return true, nil
}

// extractFeatureBox computes the 2D bounding box of a feature's
// geometry, descending into nested geometry parts as required by
// multi-geometries such as MultiPolygon. The boolean result is false
// when the feature has no geometry at all. A geometry that carries no
// coordinates yields the infinite EmptyBox.
func extractFeatureBox(f *flat.Feature) (b packedrtree.Box, hasGeometry bool, err error) {
	err = safeFlatBuffersInteraction(func() error {
		var g flat.Geometry
		if f.Geometry(&g) == nil {
			return nil
		}
		hasGeometry = true
		b = packedrtree.EmptyBox
		accumulateGeometryBox(&b, &g)
		return nil
	})
	return
}

// accumulateGeometryBox expands b with every coordinate pair in g and
// all of its nested parts.
func accumulateGeometryBox(b *packedrtree.Box, g *flat.Geometry) {
	n := g.XyLength()
	for i := 0; i+1 < n; i += 2 {
		b.ExpandXY(g.Xy(i), g.Xy(i+1))
	}
	for i := 0; i < g.PartsLength(); i++ {
		var part flat.Geometry
		if g.Parts(&part, i) {
			accumulateGeometryBox(b, &part)
		}
	}
}

// boxesIntersect reports whether two 2D boxes have any point in
// common. Boundary contact counts as intersection, consistent with
// the packed R-Tree search semantics.
func boxesIntersect(a, b *packedrtree.Box) bool {
	if a.XMax < b.XMin {
		return false
	}
	if a.YMax < b.YMin {
		return false
	}
	if a.XMin > b.XMax {
		return false
	}
	if a.YMin > b.YMax {
		return false
	}
	return true
}

// buildExtractHeader serializes a new size-prefixed header FlatBuffer
// which carries the metadata copied from src together with the
// extracted feature count, effective index node size and recomputed
// envelope.
func buildExtractHeader(src *flat.Header, count uint64, nodeSize uint16, envelope []float64) (out []byte, err error) {
	bldr := flatbuffers.NewBuilder(flatbuffers.SizeUint32)
	err = safeFlatBuffersInteraction(func() error {
		// Create all string offsets before the tables that reference
		// them, as FlatBuffers requires.
		var nameOff, titleOff, descriptionOff, metadataOff flatbuffers.UOffsetT
		if b := src.Name(); b != nil {
			nameOff = bldr.CreateString(string(b))
		}
		if b := src.Title(); b != nil {
			titleOff = bldr.CreateString(string(b))
		}
		if b := src.Description(); b != nil {
			descriptionOff = bldr.CreateString(string(b))
		}
		if b := src.Metadata(); b != nil {
			metadataOff = bldr.CreateString(string(b))
		}

		// Copy the coordinate reference system, if present.
		var crsOff flatbuffers.UOffsetT
		var srcCRS flat.Crs
		if src.Crs(&srcCRS) != nil {
			crsOff = copyExtractCRS(bldr, &srcCRS)
		}

		// Copy the column definitions, i.e. the property schema.
		var columnsOff flatbuffers.UOffsetT
		if src.ColumnsLength() > 0 {
			columnsOff = copyExtractColumns(bldr, src)
		}

		// Rebuild the envelope from the selected features.
		var envelopeOff flatbuffers.UOffsetT
		if len(envelope) > 0 {
			flat.HeaderStartEnvelopeVector(bldr, len(envelope))
			for i := len(envelope) - 1; i >= 0; i-- {
				bldr.PrependFloat64(envelope[i])
			}
			envelopeOff = bldr.EndVector(len(envelope))
		}

		flat.HeaderStart(bldr)
		if nameOff != 0 {
			flat.HeaderAddName(bldr, nameOff)
		}
		if envelopeOff != 0 {
			flat.HeaderAddEnvelope(bldr, envelopeOff)
		}
		flat.HeaderAddGeometryType(bldr, src.GeometryType())
		flat.HeaderAddHasZ(bldr, src.HasZ())
		flat.HeaderAddHasM(bldr, src.HasM())
		flat.HeaderAddHasT(bldr, src.HasT())
		flat.HeaderAddHasTm(bldr, src.HasTm())
		if columnsOff != 0 {
			flat.HeaderAddColumns(bldr, columnsOff)
		}
		flat.HeaderAddFeaturesCount(bldr, count)
		flat.HeaderAddIndexNodeSize(bldr, nodeSize)
		if crsOff != 0 {
			flat.HeaderAddCrs(bldr, crsOff)
		}
		if titleOff != 0 {
			flat.HeaderAddTitle(bldr, titleOff)
		}
		if descriptionOff != 0 {
			flat.HeaderAddDescription(bldr, descriptionOff)
		}
		if metadataOff != 0 {
			flat.HeaderAddMetadata(bldr, metadataOff)
		}
		hdrOff := flat.HeaderEnd(bldr)
		flat.FinishSizePrefixedHeaderBuffer(bldr, hdrOff)
		out = bldr.FinishedBytes()
		return nil
	})
	return
}

// copyExtractCRS rebuilds a source CRS table inside the given builder.
func copyExtractCRS(bldr *flatbuffers.Builder, src *flat.Crs) flatbuffers.UOffsetT {
	// FlatBuffers requires all nested offsets to be created before the
	// table starts, while slot additions happen after the start.
	var orgOff, nameOff, descriptionOff, wktOff, codeStringOff flatbuffers.UOffsetT
	if b := src.Org(); b != nil {
		orgOff = bldr.CreateString(string(b))
	}
	if b := src.Name(); b != nil {
		nameOff = bldr.CreateString(string(b))
	}
	if b := src.Description(); b != nil {
		descriptionOff = bldr.CreateString(string(b))
	}
	if b := src.Wkt(); b != nil {
		wktOff = bldr.CreateString(string(b))
	}
	if b := src.CodeString(); b != nil {
		codeStringOff = bldr.CreateString(string(b))
	}
	code := src.Code()

	flat.CrsStart(bldr)
	if orgOff != 0 {
		flat.CrsAddOrg(bldr, orgOff)
	}
	if code != 0 {
		flat.CrsAddCode(bldr, code)
	}
	if nameOff != 0 {
		flat.CrsAddName(bldr, nameOff)
	}
	if descriptionOff != 0 {
		flat.CrsAddDescription(bldr, descriptionOff)
	}
	if wktOff != 0 {
		flat.CrsAddWkt(bldr, wktOff)
	}
	if codeStringOff != 0 {
		flat.CrsAddCodeString(bldr, codeStringOff)
	}
	return flat.CrsEnd(bldr)
}

// copyExtractColumns rebuilds the source header's column vector inside
// the given builder, preserving column order and every column field.
func copyExtractColumns(bldr *flatbuffers.Builder, src *flat.Header) flatbuffers.UOffsetT {
	n := src.ColumnsLength()
	offsets := make([]flatbuffers.UOffsetT, n)
	for i := 0; i < n; i++ {
		var col flat.Column
		src.Columns(&col, i)
		offsets[i] = copyExtractColumn(bldr, &col)
	}
	flat.HeaderStartColumnsVector(bldr, n)
	for i := n - 1; i >= 0; i-- {
		bldr.PrependUOffsetT(offsets[i])
	}
	return bldr.EndVector(n)
}

// copyExtractColumn rebuilds a single source column table inside the
// given builder.
func copyExtractColumn(bldr *flatbuffers.Builder, src *flat.Column) flatbuffers.UOffsetT {
	// Create all nested string offsets before starting the table.
	var nameOff, titleOff, descriptionOff, metadataOff flatbuffers.UOffsetT
	if b := src.Name(); b != nil {
		nameOff = bldr.CreateString(string(b))
	}
	if b := src.Title(); b != nil {
		titleOff = bldr.CreateString(string(b))
	}
	if b := src.Description(); b != nil {
		descriptionOff = bldr.CreateString(string(b))
	}
	if b := src.Metadata(); b != nil {
		metadataOff = bldr.CreateString(string(b))
	}
	colType := src.Type()
	width, precision, scale := src.Width(), src.Precision(), src.Scale()
	nullable, unique, primaryKey := src.Nullable(), src.Unique(), src.PrimaryKey()

	// Add slots only after the table has started.
	flat.ColumnStart(bldr)
	if nameOff != 0 {
		flat.ColumnAddName(bldr, nameOff)
	}
	flat.ColumnAddType(bldr, colType)
	if titleOff != 0 {
		flat.ColumnAddTitle(bldr, titleOff)
	}
	if descriptionOff != 0 {
		flat.ColumnAddDescription(bldr, descriptionOff)
	}
	flat.ColumnAddWidth(bldr, width)
	flat.ColumnAddPrecision(bldr, precision)
	flat.ColumnAddScale(bldr, scale)
	flat.ColumnAddNullable(bldr, nullable)
	flat.ColumnAddUnique(bldr, unique)
	flat.ColumnAddPrimaryKey(bldr, primaryKey)
	if metadataOff != 0 {
		flat.ColumnAddMetadata(bldr, metadataOff)
	}
	return flat.ColumnEnd(bldr)
}
