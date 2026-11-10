// Copyright 2023 The flatgeobuf (Go) Authors. All rights reserved.
// Use of this source code is governed by an MIT-style
// license that can be found in the LICENSE file.

package flatgeobuf

import (
	"bytes"
	"io"
	"sort"

	"github.com/gogama/flatgeobuf/flatgeobuf/flat"
)

// ErrValueLimit is returned by InspectFeatures when inspecting the
// property values of a FlatGeobuf data stream would cause the
// InspectOptions.MaxValueBytes budget to be exceeded.
//
// When ErrValueLimit is returned, the accompanying FeatureInventory is
// partial: the scan stopped as soon as the budget was breached and the
// inventory must not be treated as a complete description of the input
// stream.
var ErrValueLimit = textErr("property value byte limit exceeded")

// InspectOptions configures the behavior of InspectFeatures.
//
// The zero value is valid and requests an unbounded, full scan.
type InspectOptions struct {
	// MaxFeatures limits the number of FlatGeobuf features that
	// InspectFeatures will inspect.
	//
	// A zero value requests a complete scan of every feature in the
	// data section. A positive value requests that the scan stop after
	// that many features have been inspected; when the input stream
	// contains at least one further feature, the returned
	// FeatureInventory has its Truncated field set to true. A negative
	// value is invalid and causes InspectFeatures to fail without
	// reading the input.
	MaxFeatures int

	// MaxValueBytes limits the cumulative number of property value
	// bytes that InspectFeatures will process while scanning feature
	// properties.
	//
	// The budget counts the encoded bytes of property values only: the
	// fixed-width encoding of scalar values, and the uint32 length
	// prefix plus payload of string, date/time, JSON and binary values.
	// The two-byte column index that prefixes each property is not a
	// value byte and is not counted.
	//
	// A zero value removes the limit. A positive value causes
	// InspectFeatures to stop and return ErrValueLimit as soon as
	// processing the next property value would breach the budget. A
	// negative value is invalid and causes InspectFeatures to fail
	// without reading the input.
	MaxValueBytes int64
}

// HeaderColumn describes a single property column declared by a
// FlatGeobuf file header, in the order in which the header declares
// it.
type HeaderColumn struct {
	// Index is the zero-based position of the column within the header
	// schema.
	Index int
	// Name is the column name exactly as encoded in the header.
	Name string
	// Type is the column type declared by the header.
	Type flat.ColumnType
}

// GeometryTypeCount pairs an observed flat.GeometryType with the
// number of inspected features whose geometry uses it.
type GeometryTypeCount struct {
	// Type is the geometry type read from a feature geometry table.
	Type flat.GeometryType
	// Count is the number of inspected features using Type.
	Count int
}

// ColumnProfile is the aggregated, cross-feature inventory of a single
// property column, identified by its canonical name.
//
// Columns that share a name across the header and one or more
// per-feature schemas are stably merged into a single profile. Every
// schema index at which the name has been declared, and every column
// type declared or observed for the name, is retained so that type
// conflicts and duplicate column names remain visible.
type ColumnProfile struct {
	// Name is the canonical column name. It is the exact column name
	// as encoded in the schema tables; merging is case-sensitive and
	// the empty name is permitted.
	Name string
	// Indexes contains, in strictly ascending order and without
	// duplicates, every schema column index at which Name was declared
	// across the header and the inspected per-feature schemas.
	Indexes []int
	// DeclaredTypes contains, in strictly ascending order and without
	// duplicates, every column type declared for Name in the header or
	// a per-feature schema, whether or not a value was ever observed.
	DeclaredTypes []flat.ColumnType
	// ObservedTypes contains, in strictly ascending order and without
	// duplicates, every column type under which an actual property
	// value for Name was observed. More than one entry indicates a type
	// conflict across schemas.
	ObservedTypes []flat.ColumnType
	// Count is the number of actual property values observed for Name.
	Count int
	// Missing is the number of inspected features in which Name was
	// expected but no property value for it was present. A header
	// column is expected by every feature; a column introduced by a
	// per-feature schema is expected by the features that use that
	// schema. When a per-feature schema drops a header column, the
	// dropped column counts as missing for the overriding feature.
	Missing int
	// InHeader reports whether Name is declared by the file header.
	InHeader bool
	// InFeatureSchema reports whether Name is declared by at least one
	// per-feature schema.
	InFeatureSchema bool
}

// FeatureInventory is the reusable result of an InspectFeatures scan.
//
// Every slice field is non-nil and deterministically ordered, so
// scanning the same input bytes with the same InspectOptions always
// yields an identical FeatureInventory.
type FeatureInventory struct {
	// HeaderGeometryType is the geometry type declared by the file
	// header, or flat.GeometryTypeUnknown when the header does not
	// declare a geometry type.
	HeaderGeometryType flat.GeometryType
	// HeaderColumns is the property schema declared by the file header,
	// in header order.
	HeaderColumns []HeaderColumn
	// Features is the number of features actually inspected. When
	// Truncated is true, Features equals the InspectOptions.MaxFeatures
	// limit and further features may remain in the stream.
	Features int
	// EmptyGeometries is the number of inspected features that have no
	// geometry table, or whose geometry table carries no coordinates.
	EmptyGeometries int
	// FeatureSchemas is the number of inspected features that carry
	// their own per-feature column schema instead of relying on the
	// header schema.
	FeatureSchemas int
	// GeometryTypes counts inspected features that carry a geometry
	// table, grouped by the table's geometry type. The slice is sorted
	// ascending by geometry type. Features counted by
	// EmptyGeometries because they lack a geometry table are not
	// represented here.
	GeometryTypes []GeometryTypeCount
	// Columns contains one profile per distinct column name observed
	// or declared during the scan, including header-only columns. The
	// slice is sorted ascending by canonical name.
	Columns []ColumnProfile
	// ExtraColumns contains the profiles whose name is not declared by
	// the header schema, i.e. columns introduced solely by per-feature
	// schema overrides. The slice is sorted ascending by canonical name
	// and references the same profile values held in Columns.
	ExtraColumns []ColumnProfile
	// MissingColumns contains the header-declared profiles whose
	// Missing count is greater than zero. The slice is sorted
	// ascending by canonical name and references the same profile
	// values held in Columns.
	MissingColumns []ColumnProfile
	// ValueBytes is the cumulative number of encoded property value
	// bytes processed during the scan, counted as described on
	// InspectOptions.MaxValueBytes.
	ValueBytes int64
	// Truncated is true when the scan stopped deliberately because the
	// InspectOptions.MaxFeatures limit was reached while at least one
	// more feature remained available. It is never set as a result of a
	// malformed or truncated input stream; those conditions are
	// reported as errors.
	Truncated bool
}

// inspectBatchSize is the maximum number of features read from the
// FileReader in a single Data call during an inspection.
const inspectBatchSize = 64

// InspectFeatures performs a single sequential scan of the FlatGeobuf
// stream r and returns a reusable FeatureInventory describing the
// geometry types and property columns actually present in the data.
//
// The stream must be positioned at the start of the FlatGeobuf file,
// i.e. right before the magic number. InspectFeatures creates its own
// FileReader internally and never closes r; the caller retains
// ownership of the stream. Property values are counted and typed but
// never retained, so the memory footprint of the scan is bounded by
// the number of distinct columns rather than the volume of data.
//
// InspectFeatures always returns a non-nil FeatureInventory, even when
// the returned error is non-nil. In the error case the inventory only
// describes the portion of the stream scanned before the failure and
// must not be treated as complete. Malformed data, truncated streams
// and unsupported property types are reported as errors instead of
// panicking, and a MaxValueBytes breach is reported with ErrValueLimit.
// Only a clean stop at the MaxFeatures limit is reported through the
// inventory's Truncated field together with a nil error.
func InspectFeatures(r io.Reader, opts InspectOptions) (*FeatureInventory, error) {
	inv := newFeatureInventory()

	if opts.MaxFeatures < 0 {
		return inv, fmtErr("InspectOptions.MaxFeatures must not be negative (got %d)", opts.MaxFeatures)
	}
	if opts.MaxValueBytes < 0 {
		return inv, fmtErr("InspectOptions.MaxValueBytes must not be negative (got %d)", opts.MaxValueBytes)
	}
	if r == nil {
		return inv, textErr("nil reader")
	}

	a := newInspectAggregator(inv, opts.MaxValueBytes)

	// Read and inventory the header.
	fr := NewFileReader(r)
	hdr, err := fr.Header()
	if err != nil {
		if hdr != nil {
			_ = safeFlatBuffersInteraction(func() error {
				a.recordHeader(hdr)
				return nil
			})
		}
		return a.finalize(), err
	}
	if err = safeFlatBuffersInteraction(func() error {
		a.recordHeader(hdr)
		return nil
	}); err != nil {
		return a.finalize(), wrapErr("failed to read header schema", err)
	}

	// Sequentially scan the data section, honoring the feature limit.
	for {
		remaining := -1
		if opts.MaxFeatures > 0 {
			remaining = opts.MaxFeatures - inv.Features
			if remaining == 0 {
				// The feature limit has been reached. Probe for one
				// more feature so that Truncated reflects whether the
				// scan actually cut anything off.
				var probe [1]flat.Feature
				n, err := fr.Data(probe[:])
				if err == io.EOF {
					break // Clean end of stream: nothing was truncated.
				}
				if err != nil {
					return a.finalize(), err
				}
				if n > 0 {
					inv.Truncated = true
				}
				break
			}
		}

		batchSize := inspectBatchSize
		if remaining >= 0 && remaining < batchSize {
			batchSize = remaining
		}
		buf := make([]flat.Feature, batchSize)
		n, err := fr.Data(buf)
		for i := 0; i < n; i++ {
			if err = a.inspectFeature(hdr, &buf[i]); err != nil {
				return a.finalize(), err
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return a.finalize(), err
		}
	}

	// A header that declares a positive feature count makes an early
	// end-of-stream detectable: reaching EOF with fewer features means
	// the data section is truncated and the partial inventory must not
	// be reported as complete. A zero header count means the count is
	// unknown and scanning to EOF is correct.
	if !inv.Truncated {
		var declared uint64
		if err = safeFlatBuffersInteraction(func() error {
			declared = hdr.FeaturesCount()
			return nil
		}); err != nil {
			return a.finalize(), wrapErr("failed to read header feature count", err)
		}
		if declared > 0 && uint64(inv.Features) < declared {
			return a.finalize(), wrapErr("data section ended after %d of %d header-declared features",
				io.ErrUnexpectedEOF, inv.Features, declared)
		}
	}

	return a.finalize(), nil
}

func newFeatureInventory() *FeatureInventory {
	return &FeatureInventory{
		HeaderColumns:  []HeaderColumn{},
		GeometryTypes:  []GeometryTypeCount{},
		Columns:        []ColumnProfile{},
		ExtraColumns:   []ColumnProfile{},
		MissingColumns: []ColumnProfile{},
	}
}

// columnAgg is the mutable accumulator behind a ColumnProfile.
type columnAgg struct {
	name            string
	indexes         map[int]struct{}
	declaredTypes   map[flat.ColumnType]struct{}
	observedTypes   map[flat.ColumnType]struct{}
	count           int
	missing         int
	inHeader        bool
	inFeatureSchema bool
}

// inspectAggregator owns the mutable scan state. All maps are converted
// to sorted slices by finalize, so map iteration order never leaks
// into the result.
type inspectAggregator struct {
	inv           *FeatureInventory
	maxValueBytes int64
	header        Schema
	columns       map[string]*columnAgg
	geometryTypes map[flat.GeometryType]int
}

func newInspectAggregator(inv *FeatureInventory, maxValueBytes int64) *inspectAggregator {
	return &inspectAggregator{
		inv:           inv,
		maxValueBytes: maxValueBytes,
		columns:       make(map[string]*columnAgg),
		geometryTypes: make(map[flat.GeometryType]int),
	}
}

func (a *inspectAggregator) upsertColumn(name string) *columnAgg {
	c, ok := a.columns[name]
	if !ok {
		c = &columnAgg{
			name:          name,
			indexes:       make(map[int]struct{}),
			declaredTypes: make(map[flat.ColumnType]struct{}),
			observedTypes: make(map[flat.ColumnType]struct{}),
		}
		a.columns[name] = c
	}
	return c
}

// recordHeader captures the header-declared geometry type and columns,
// pre-populating column profiles so that never-observed header columns
// remain visible. Must be called inside a safeFlatBuffersInteraction.
func (a *inspectAggregator) recordHeader(hdr *flat.Header) {
	a.inv.HeaderGeometryType = hdr.GeometryType()
	a.header = hdr
	n := hdr.ColumnsLength()
	a.inv.HeaderColumns = make([]HeaderColumn, 0, n)
	for j := 0; j < n; j++ {
		var col flat.Column
		if !hdr.Columns(&col, j) {
			continue
		}
		name := string(col.Name())
		colType := col.Type()
		a.inv.HeaderColumns = append(a.inv.HeaderColumns, HeaderColumn{
			Index: j,
			Name:  name,
			Type:  colType,
		})
		c := a.upsertColumn(name)
		c.inHeader = true
		c.indexes[j] = struct{}{}
		c.declaredTypes[colType] = struct{}{}
	}
}

// recordFeatureSchema captures the per-feature schema columns of an
// overriding feature. Must be called inside a
// safeFlatBuffersInteraction.
func (a *inspectAggregator) recordFeatureSchema(f *flat.Feature) {
	n := f.ColumnsLength()
	for j := 0; j < n; j++ {
		var col flat.Column
		if !f.Columns(&col, j) {
			continue
		}
		name := string(col.Name())
		colType := col.Type()
		c := a.upsertColumn(name)
		c.inFeatureSchema = true
		c.indexes[j] = struct{}{}
		c.declaredTypes[colType] = struct{}{}
	}
}

// inspectFeature folds one feature table into the inventory. All
// FlatBuffer access happens inside safeFlatBuffersInteraction so a
// corrupt feature table yields an error rather than a panic.
func (a *inspectAggregator) inspectFeature(hdr *flat.Header, f *flat.Feature) error {
	a.inv.Features++
	return safeFlatBuffersInteraction(func() error {
		// Geometry accounting.
		var g flat.Geometry
		if f.Geometry(&g) == nil {
			a.inv.EmptyGeometries++
		} else {
			a.geometryTypes[g.Type()]++
			if geometryCoordinateCount(&g) == 0 {
				a.inv.EmptyGeometries++
			}
		}

		// Select the effective schema: a non-empty per-feature schema
		// overrides the header schema entirely.
		var schema Schema
		if f.ColumnsLength() > 0 {
			schema = f
			a.inv.FeatureSchemas++
			a.recordFeatureSchema(f)
		} else {
			schema = hdr
		}

		// Walk the properties buffer without retaining any values.
		present, err := a.scanProperties(f.PropertiesBytes(), schema)
		if err != nil {
			return err
		}

		// Account for columns the effective schema declares but the
		// feature did not include a value for, and for header columns
		// dropped by an overriding schema.
		return a.accountMissing(schema, f.ColumnsLength() > 0, present)
	})
}

// scanProperties walks one feature's properties buffer, updating column
// counts, observed types and the value-byte budget. It returns the set
// of schema column indexes for which a value was present.
func (a *inspectAggregator) scanProperties(props []byte, schema Schema) (map[int]bool, error) {
	present := make(map[int]bool)
	if len(props) == 0 {
		return present, nil
	}

	var schemaLen int
	if err := safeFlatBuffersInteraction(func() error {
		schemaLen = schema.ColumnsLength()
		return nil
	}); err != nil {
		return nil, wrapErr("failed to read schema column count", err)
	}

	br := bytes.NewReader(props)
	pr := NewPropReader(br)
	for br.Len() > 0 {
		// Read the column index prefix.
		idx, err := pr.ReadUShort()
		if err != nil {
			return nil, wrapErr("failed to read property column index", err)
		}
		j := int(idx)
		if j >= schemaLen {
			return nil, fmtErr("schema has only %d columns, but property references column index %d", schemaLen, j)
		}

		// Resolve the column metadata from the effective schema.
		var col flat.Column
		if err = safeFlatBuffersInteraction(func() error {
			if !schema.Columns(&col, j) {
				return textErr("column not found")
			}
			return nil
		}); err != nil {
			return nil, wrapErr("failed to fetch column %d", err, j)
		}
		name := string(col.Name())
		colType := col.Type()
		c := a.upsertColumn(name)
		c.indexes[j] = struct{}{}
		c.declaredTypes[colType] = struct{}{}

		// Skip and budget the encoded value.
		if err = a.skipValue(br, pr, j, colType); err != nil {
			return nil, err
		}

		c.count++
		c.observedTypes[colType] = struct{}{}
		present[j] = true
	}
	return present, nil
}

// skipValue advances past one property value, charging its encoded
// size against the value-byte budget before the bytes are consumed.
func (a *inspectAggregator) skipValue(br *bytes.Reader, pr *PropReader, colIndex int, colType flat.ColumnType) error {
	switch colType {
	case flat.ColumnTypeByte, flat.ColumnTypeUByte, flat.ColumnTypeBool:
		if err := a.charge(1, colIndex); err != nil {
			return err
		}
		return skipBytes(br, 1)
	case flat.ColumnTypeShort, flat.ColumnTypeUShort:
		if err := a.charge(2, colIndex); err != nil {
			return err
		}
		return skipBytes(br, 2)
	case flat.ColumnTypeInt, flat.ColumnTypeUInt, flat.ColumnTypeFloat:
		if err := a.charge(4, colIndex); err != nil {
			return err
		}
		return skipBytes(br, 4)
	case flat.ColumnTypeLong, flat.ColumnTypeULong, flat.ColumnTypeDouble:
		if err := a.charge(8, colIndex); err != nil {
			return err
		}
		return skipBytes(br, 8)
	case flat.ColumnTypeString, flat.ColumnTypeDateTime, flat.ColumnTypeJson, flat.ColumnTypeBinary:
		// The uint32 length prefix is part of the encoded value.
		if err := a.charge(4, colIndex); err != nil {
			return err
		}
		n, err := pr.ReadUInt()
		if err != nil {
			return wrapErr("failed to read property value length (column index %d)", err, colIndex)
		}
		if int64(n) > int64(br.Len()) {
			return wrapErr("property value of %d bytes exceeds remaining %d property bytes (column index %d)",
				io.ErrUnexpectedEOF, n, br.Len(), colIndex)
		}
		if err = a.charge(int64(n), colIndex); err != nil {
			return err
		}
		return skipBytes(br, int64(n))
	default:
		return fmtErr("unsupported column type %s (%d) at column index %d", colType, byte(colType), colIndex)
	}
}

// charge records n processed value bytes and rejects the scan before
// the budget is breached.
func (a *inspectAggregator) charge(n int64, colIndex int) error {
	if a.maxValueBytes > 0 && a.inv.ValueBytes+n > a.maxValueBytes {
		return wrapErr("inspecting property at column index %d would exceed MaxValueBytes budget of %d (already processed %d bytes)",
			ErrValueLimit, colIndex, a.maxValueBytes, a.inv.ValueBytes)
	}
	a.inv.ValueBytes += n
	return nil
}

// accountMissing increments missing counters for schema-declared
// columns that the feature omitted, and for header columns dropped by
// an overriding per-feature schema.
func (a *inspectAggregator) accountMissing(schema Schema, override bool, present map[int]bool) error {
	var n int
	if err := safeFlatBuffersInteraction(func() error {
		n = schema.ColumnsLength()
		return nil
	}); err != nil {
		return wrapErr("failed to read schema column count", err)
	}

	effectiveNames := make(map[string]bool, n)
	for j := 0; j < n; j++ {
		var col flat.Column
		if err := safeFlatBuffersInteraction(func() error {
			if !schema.Columns(&col, j) {
				return textErr("column not found")
			}
			return nil
		}); err != nil {
			return wrapErr("failed to fetch column %d", err, j)
		}
		name := string(col.Name())
		effectiveNames[name] = true
		if !present[j] {
			a.upsertColumn(name).missing++
		}
	}

	if override {
		// A per-feature schema replaces the header schema wholesale, so
		// any header name it omits is unavailable to this feature.
		for _, hc := range a.inv.HeaderColumns {
			if !effectiveNames[hc.Name] {
				a.upsertColumn(hc.Name).missing++
			}
		}
	}
	return nil
}

// geometryCoordinateCount returns the total number of coordinate
// values carried by a geometry, recursively including its parts.
func geometryCoordinateCount(g *flat.Geometry) int {
	n := g.XyLength()
	for i := 0; i < g.PartsLength(); i++ {
		var part flat.Geometry
		if g.Parts(&part, i) {
			n += geometryCoordinateCount(&part)
		}
	}
	return n
}

// skipBytes discards exactly n bytes from br, turning a short buffer
// into io.ErrUnexpectedEOF.
func skipBytes(br *bytes.Reader, n int64) error {
	if int64(br.Len()) < n {
		return io.ErrUnexpectedEOF
	}
	_, err := br.Seek(n, io.SeekCurrent)
	return err
}

// finalize converts all mutable accumulator maps into deterministically
// ordered slices. It is safe to call on a partially populated
// aggregator when the scan ends in an error.
func (a *inspectAggregator) finalize() *FeatureInventory {
	inv := a.inv

	// Geometry types in ascending numeric order.
	geomTypes := make([]GeometryTypeCount, 0, len(a.geometryTypes))
	for t, count := range a.geometryTypes {
		geomTypes = append(geomTypes, GeometryTypeCount{Type: t, Count: count})
	}
	sort.Slice(geomTypes, func(i, j int) bool {
		return geomTypes[i].Type < geomTypes[j].Type
	})
	inv.GeometryTypes = geomTypes

	// Column profiles in ascending canonical-name order.
	names := make([]string, 0, len(a.columns))
	for name := range a.columns {
		names = append(names, name)
	}
	sort.Strings(names)

	columns := make([]ColumnProfile, 0, len(names))
	extra := make([]ColumnProfile, 0)
	missing := make([]ColumnProfile, 0)
	for _, name := range names {
		ca := a.columns[name]
		profile := ColumnProfile{
			Name:            name,
			Indexes:         sortedIntSet(ca.indexes),
			DeclaredTypes:   sortedColumnTypeSet(ca.declaredTypes),
			ObservedTypes:   sortedColumnTypeSet(ca.observedTypes),
			Count:           ca.count,
			Missing:         ca.missing,
			InHeader:        ca.inHeader,
			InFeatureSchema: ca.inFeatureSchema,
		}
		columns = append(columns, profile)
		if !ca.inHeader {
			extra = append(extra, profile)
		}
		if ca.inHeader && ca.missing > 0 {
			missing = append(missing, profile)
		}
	}
	inv.Columns = columns
	inv.ExtraColumns = extra
	inv.MissingColumns = missing

	return inv
}

func sortedIntSet(set map[int]struct{}) []int {
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

func sortedColumnTypeSet(set map[flat.ColumnType]struct{}) []flat.ColumnType {
	out := make([]flat.ColumnType, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i] < out[j]
	})
	return out
}
