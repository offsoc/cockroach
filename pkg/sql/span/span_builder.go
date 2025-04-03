// Copyright 2019 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package span

import (
	"context"
	"sort"

	"github.com/cockroachdb/cockroach/pkg/keys"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catalogkeys"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/catenumpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/descpb"
	"github.com/cockroachdb/cockroach/pkg/sql/catalog/fetchpb"
	"github.com/cockroachdb/cockroach/pkg/sql/inverted"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/constraint"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc/keyside"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/encoding"
	"github.com/cockroachdb/errors"
)

// Builder is a single struct for generating key spans from Constraints, Datums,
// encDatums, and InvertedSpans.
type Builder struct {
	evalCtx *eval.Context
	codec   keys.SQLCodec

	keyAndPrefixCols []fetchpb.IndexFetchSpec_KeyColumn

	// KeyPrefix is the prefix of keys generated by the builder.
	KeyPrefix []byte
	alloc     tree.DatumAlloc
}

// Init initializes a Builder with a table and index. It does not set up the
// Builder to create external spans, even if the table uses external row data.
func (s *Builder) Init(
	evalCtx *eval.Context, codec keys.SQLCodec, table catalog.TableDescriptor, index catalog.Index,
) {
	if ext := table.ExternalRowData(); ext != nil {
		panic(errors.AssertionFailedf("%s uses external row data", table.GetName()))
	}
	s.evalCtx = evalCtx
	s.codec = codec
	s.keyAndPrefixCols = table.IndexFetchSpecKeyAndSuffixColumns(index)
	s.KeyPrefix = rowenc.MakeIndexKeyPrefix(codec, table.GetID(), index.GetID())
}

// InitAllowingExternalRowData initializes a Builder with a table and index. If
// the table uses external row data, the Builder will create external spans.
func (s *Builder) InitAllowingExternalRowData(
	evalCtx *eval.Context, codec keys.SQLCodec, table catalog.TableDescriptor, index catalog.Index,
) {
	s.evalCtx = evalCtx
	s.keyAndPrefixCols = table.IndexFetchSpecKeyAndSuffixColumns(index)
	if ext := table.ExternalRowData(); ext != nil {
		s.codec = keys.MakeSQLCodec(ext.TenantID)
		s.KeyPrefix = rowenc.MakeIndexKeyPrefix(s.codec, ext.TableID, index.GetID())
	} else {
		s.codec = codec
		s.KeyPrefix = rowenc.MakeIndexKeyPrefix(codec, table.GetID(), index.GetID())
	}
}

// InitWithFetchSpec creates a Builder using IndexFetchSpec. If the spec
// specifies external row data, the Builder will create external spans.
func (s *Builder) InitWithFetchSpec(
	evalCtx *eval.Context, codec keys.SQLCodec, spec *fetchpb.IndexFetchSpec,
) {
	s.evalCtx = evalCtx
	s.keyAndPrefixCols = spec.KeyAndSuffixColumns
	if ext := spec.External; ext != nil {
		s.codec = keys.MakeSQLCodec(ext.TenantID)
		s.KeyPrefix = rowenc.MakeIndexKeyPrefix(s.codec, ext.TableID, spec.IndexID)
	} else {
		s.codec = codec
		s.KeyPrefix = rowenc.MakeIndexKeyPrefix(codec, spec.TableID, spec.IndexID)
	}
}

// SpanFromEncDatums encodes a span with len(values) constraint columns from the
// index prefixed with the index key prefix that includes the table and index
// ID. SpanFromEncDatums assumes that the EncDatums in values are in the order
// of the index columns. It also returns whether the input values contain a null
// value or not, which can be used as input for CanSplitSpanIntoFamilySpans.
func (s *Builder) SpanFromEncDatums(
	values rowenc.EncDatumRow,
) (_ roachpb.Span, containsNull bool, _ error) {
	return rowenc.MakeSpanFromEncDatums(values, s.keyAndPrefixCols, &s.alloc, s.KeyPrefix)
}

// SpanFromEncDatumsWithRange encodes a range span. The inequality is assumed to
// be the end of the span and the start/end keys are generated by putting them
// in the values row at the prefixLen position. Only one of start or end
// need be non-nil, omitted one causing an open ended range span to be
// generated. Since the exec code knows nothing about index column sorting
// direction we assume ascending if they are descending we deal with that here.
func (s *Builder) SpanFromEncDatumsWithRange(
	ctx context.Context,
	values rowenc.EncDatumRow,
	prefixLen int,
	startBound, endBound *rowenc.EncDatum,
	startInclusive, endInclusive bool,
	rangeColTyp *types.T,
) (_ roachpb.Span, containsNull, filterRow bool, err error) {
	isDesc := s.keyAndPrefixCols[prefixLen].Direction == catenumpb.IndexColumn_DESC
	if isDesc {
		startBound, endBound = endBound, startBound
		startInclusive, endInclusive = endInclusive, startInclusive
	}

	if startBound != nil {
		if err = startBound.EnsureDecoded(rangeColTyp, &s.alloc); err != nil {
			return roachpb.Span{}, false, false, err
		}
		if startBound.IsNull() {
			// Inequalities are null-rejecting.
			return roachpb.Span{}, true, true, nil
		}
		if !startInclusive {
			if (isDesc && startBound.Datum.IsMin(ctx, s.evalCtx)) ||
				(!isDesc && startBound.Datum.IsMax(ctx, s.evalCtx)) {
				// There are no values that satisfy the start bound.
				return roachpb.Span{}, false, true, nil
			}
		}
	}
	if endBound != nil {
		if err = endBound.EnsureDecoded(rangeColTyp, &s.alloc); err != nil {
			return roachpb.Span{}, false, false, err
		}
		if endBound.IsNull() {
			// Inequalities are null-rejecting.
			return roachpb.Span{}, true, true, nil
		}
	}

	makeKeyFromRow := func(r rowenc.EncDatumRow) (_ roachpb.Key, containsNull bool, _ error) {
		return rowenc.MakeKeyFromEncDatums(r, s.keyAndPrefixCols, &s.alloc, s.KeyPrefix)
	}

	var startKey, endKey roachpb.Key
	var startContainsNull, endContainsNull bool
	if startBound != nil {
		startDatum := startBound.Datum
		if !startInclusive && isDesc {
			// We need the datum that sorts immediately before startDatum in order
			// to make the start boundary inclusive. The optimizer should have
			// filtered cases where this is not possible. If the index column is ASC,
			// we can directly increment the key below instead of the datum.
			var ok bool
			startDatum, ok = startDatum.Prev(ctx, s.evalCtx)
			if !ok {
				return roachpb.Span{}, false, false, errors.AssertionFailedf(
					"couldn't get a Prev value for %s", startBound.Datum,
				)
			}
		}
		values[prefixLen] = rowenc.EncDatum{Datum: startDatum}
		startKey, startContainsNull, err = makeKeyFromRow(values[:prefixLen+1])
		if !startInclusive && !isDesc {
			// The start key of a span is always inclusive, so we need to increment to
			// the key that sorts immediately after this one in order to make it
			// exclusive.
			startKey = startKey.PrefixEnd()
		}
	} else {
		startKey, startContainsNull, err = makeKeyFromRow(values[:prefixLen])
		// If we have an ascending index make sure not to include NULLs.
		if !isDesc {
			startKey = encoding.EncodeNotNullAscending(startKey)
		}
	}

	if err != nil {
		return roachpb.Span{}, false, false, err
	}

	if endBound != nil {
		endDatum := endBound.Datum
		values[prefixLen] = rowenc.EncDatum{Datum: endDatum}
		endKey, endContainsNull, err = makeKeyFromRow(values[:prefixLen+1])
		if endInclusive {
			endKey = endKey.PrefixEnd()
		}
	} else {
		endKey, endContainsNull, err = makeKeyFromRow(values[:prefixLen])
		// If we have a descending index make sure not to include NULLs.
		if isDesc {
			endKey = encoding.EncodeNotNullDescending(endKey)
		} else {
			endKey = endKey.PrefixEnd()
		}
	}

	if err != nil {
		return roachpb.Span{}, false, false, err
	}

	if startKey.Compare(endKey) >= 0 {
		// It is possible that the inequality bounds filter out the input row.
		return roachpb.Span{}, false, true, nil
	}

	span := roachpb.Span{Key: startKey, EndKey: endKey}
	return span, startContainsNull || endContainsNull, false, nil
}

// SpanFromDatumRow generates an index span with prefixLen constraint columns from the index.
// SpanFromDatumRow assumes that values is a valid table row for the Builder's table.
// It also returns whether the input values contain a null value or not, which
// can be used as input for CanSplitSpanIntoFamilySpans.
func (s *Builder) SpanFromDatumRow(
	values tree.Datums, prefixLen int, colMap catalog.TableColMap,
) (_ roachpb.Span, containsNull bool, _ error) {
	return rowenc.EncodePartialIndexSpan(s.keyAndPrefixCols[:prefixLen], colMap, values, s.KeyPrefix)
}

// SpanToPointSpan converts a span into a span that represents a point lookup on a
// specific family. It is up to the caller to ensure that this is a safe operation,
// by calling CanSplitSpanIntoFamilySpans before using it.
func (s *Builder) SpanToPointSpan(span roachpb.Span, family descpb.FamilyID) roachpb.Span {
	key := keys.MakeFamilyKey(span.Key, uint32(family))
	return roachpb.Span{Key: key, EndKey: roachpb.Key(key).PrefixEnd()}
}

// Functions for optimizer related span generation are below.

// SpansFromConstraint generates spans from an optimizer constraint.
// A span.Splitter can be used to generate more specific family spans.
//
// TODO (rohany): In future work, there should be a single API to generate spans
//
//	from constraints, datums and encdatums.
func (s *Builder) SpansFromConstraint(
	c *constraint.Constraint, splitter Splitter,
) (roachpb.Spans, error) {
	var spans roachpb.Spans
	var err error
	if c == nil || c.IsUnconstrained() {
		// Encode a full span.
		spans, err = s.appendSpansFromConstraintSpan(spans, &constraint.UnconstrainedSpan, splitter)
		if err != nil {
			return nil, err
		}
		return spans, nil
	}

	spans = make(roachpb.Spans, 0, c.Spans.Count())
	for i := 0; i < c.Spans.Count(); i++ {
		spans, err = s.appendSpansFromConstraintSpan(spans, c.Spans.Get(i), splitter)
		if err != nil {
			return nil, err
		}
	}
	return spans, nil
}

// SpansFromConstraintSpan generates spans from optimizer
// spans. A span.Splitter can be used to generate more specific
// family spans from constraints, datums and encdatums.
func (s *Builder) SpansFromConstraintSpan(
	cs *constraint.Spans, splitter Splitter,
) (roachpb.Spans, error) {
	var spans roachpb.Spans
	var err error
	spans = make(roachpb.Spans, 0, cs.Count())
	for i := 0; i < cs.Count(); i++ {
		spans, err = s.appendSpansFromConstraintSpan(spans, cs.Get(i), splitter)
		if err != nil {
			return nil, err
		}
	}
	return spans, nil
}

// UnconstrainedSpans returns the full span corresponding to the Builder's
// table and index.
func (s *Builder) UnconstrainedSpans() (roachpb.Spans, error) {
	return s.SpansFromConstraint(nil, NoopSplitter())
}

// appendSpansFromConstraintSpan converts a constraint.Span to one or more
// roachpb.Spans and appends them to the provided spans. It appends multiple
// spans in the case that multiple, non-adjacent column families should be
// scanned.
func (s *Builder) appendSpansFromConstraintSpan(
	appendTo roachpb.Spans, cs *constraint.Span, splitter Splitter,
) (roachpb.Spans, error) {
	var span roachpb.Span
	var err error
	var containsNull bool
	// Encode each logical part of the start key.
	span.Key, containsNull, err = s.encodeConstraintKey(cs.StartKey(), true /* includePrefix */)
	if err != nil {
		return nil, err
	}
	if cs.StartBoundary() == constraint.IncludeBoundary {
		if cs.StartKey().IsEmpty() {
			span.Key = append(span.Key, s.KeyPrefix...)
		}
	} else {
		// We need to exclude the value this logical part refers to.
		span.Key = span.Key.PrefixEnd()
	}
	// Encode each logical part of the end key.
	span.EndKey, _, err = s.encodeConstraintKey(cs.EndKey(), true /* includePrefix */)
	if err != nil {
		return nil, err
	}
	if cs.EndKey().IsEmpty() {
		span.EndKey = append(span.EndKey, s.KeyPrefix...)
	}

	// Optimization: for single row lookups on a table with one or more column
	// families, only scan the relevant column families, and use GetRequests
	// instead of ScanRequests when doing the column family fetches.
	if splitter.CanSplitSpanIntoFamilySpans(cs.StartKey().Length(), containsNull) && span.Key.Equal(span.EndKey) {
		return rowenc.SplitRowKeyIntoFamilySpans(appendTo, span.Key, splitter.neededFamilies), nil
	}

	// We need to advance the end key if it is inclusive.
	if cs.EndBoundary() == constraint.IncludeBoundary {
		span.EndKey = span.EndKey.PrefixEnd()
	}

	return append(appendTo, span), nil
}

// encodeConstraintKey encodes each logical part of a constraint.Key into a
// roachpb.Key.
//
// includePrefix is true if the KeyPrefix bytes should be included in the
// returned key.
func (s *Builder) encodeConstraintKey(
	ck constraint.Key, includePrefix bool,
) (key roachpb.Key, containsNull bool, _ error) {
	if ck.IsEmpty() {
		return key, containsNull, nil
	}
	if includePrefix {
		key = append(key, s.KeyPrefix...)
	}
	for i := 0; i < ck.Length(); i++ {
		val := ck.Value(i)
		if val == tree.DNull {
			containsNull = true
		}

		dir, err := catalogkeys.IndexColumnEncodingDirection(
			s.keyAndPrefixCols[i].Direction,
		)
		if err != nil {
			return nil, false, err
		}

		key, err = keyside.Encode(key, val, dir)
		if err != nil {
			return nil, false, err
		}
	}
	return key, containsNull, nil
}

// InvertedSpans represent inverted index spans that can be encoded into
// key spans.
type InvertedSpans interface {
	// Len returns the number of spans represented.
	Len() int

	// Start returns the start bytes of the ith span.
	Start(i int) []byte

	// End returns the end bytes of the ith span.
	End(i int) []byte
}

var _ InvertedSpans = inverted.Spans{}
var _ InvertedSpans = inverted.SpanExpressionProtoSpans{}

// SpansFromInvertedSpans constructs spans to scan an inverted index.
//
// If the index is a single-column inverted index, c should be nil.
//
// If the index is a multi-column inverted index, c should constrain the
// non-inverted prefix columns of the index. Each span in c must have a single
// key. The resulting roachpb.Spans are created by performing a cross product of
// keys in c and the invertedSpan keys.
//
// scratch can be an optional roachpb.Spans slice that will be reused to
// populate the result.
func (s *Builder) SpansFromInvertedSpans(
	ctx context.Context, invertedSpans InvertedSpans, c *constraint.Constraint, scratch roachpb.Spans,
) (roachpb.Spans, error) {
	if invertedSpans == nil {
		return nil, errors.AssertionFailedf("invertedSpans cannot be nil")
	}

	var scratchRows []rowenc.EncDatumRow
	if c != nil {
		// For each span in c, create a scratchRow that starts with the span's
		// keys. The last slot in each scratchRow is reserved for encoding the
		// inverted span key.
		scratchRows = make([]rowenc.EncDatumRow, c.Spans.Count())
		for i, n := 0, c.Spans.Count(); i < n; i++ {
			span := c.Spans.Get(i)

			// The spans must have the same start and end key.
			if !span.HasSingleKey(ctx, s.evalCtx) {
				return nil, errors.AssertionFailedf("constraint span %s does not have a single key", span)
			}

			keyLength := span.StartKey().Length()
			scratchRows[i] = make(rowenc.EncDatumRow, keyLength+1)
			for j := 0; j < keyLength; j++ {
				val := span.StartKey().Value(j)
				scratchRows[i][j] = rowenc.DatumToEncDatum(val.ResolvedType(), val)
			}
		}
	} else {
		// If c is nil, then the spans must constrain a single-column inverted
		// index. In this case, only 1 scratchRow of length 1 is needed to
		// encode the inverted spans.
		scratchRows = make([]rowenc.EncDatumRow, 1)
		scratchRows[0] = make(rowenc.EncDatumRow, 1)
	}

	scratch = scratch[:0]
	for i := range scratchRows {
		for j, n := 0, invertedSpans.Len(); j < n; j++ {
			var indexSpan roachpb.Span
			var err error
			if indexSpan.Key, err = s.generateInvertedSpanKey(invertedSpans.Start(j), scratchRows[i]); err != nil {
				return nil, err
			}
			if indexSpan.EndKey, err = s.generateInvertedSpanKey(invertedSpans.End(j), scratchRows[i]); err != nil {
				return nil, err
			}
			scratch = append(scratch, indexSpan)
		}
	}
	sort.Sort(scratch)
	return scratch, nil
}

// generateInvertedSpanKey returns a key that encodes enc and scratchRow. The
// last slot in scratchRow is overwritten in order to encode enc. If the length
// of scratchRow is greater than one, the EncDatums that precede the last slot
// are encoded as prefix keys of enc.
func (s *Builder) generateInvertedSpanKey(
	enc []byte, scratchRow rowenc.EncDatumRow,
) (roachpb.Key, error) {
	keyLen := len(scratchRow) - 1
	scratchRow = scratchRow[:keyLen]
	if len(enc) > 0 {
		// The encoded inverted value will be passed through unchanged.
		encDatum := rowenc.EncDatumFromEncoded(catenumpb.DatumEncoding_ASCENDING_KEY, enc)
		scratchRow = append(scratchRow, encDatum)
		keyLen++
	}
	// Else, this is the case of scanning all the inverted keys under the
	// prefix of scratchRow (including the case where there is no prefix when
	// the inverted column is the first column). Note, the inverted span in
	// that case will be [nil, RKeyMax), and the caller calls this method with
	// both nil and RKeyMax. The first call will fall through here, and
	// generate a span, of which we will only use Span.Key. Span.EndKey is
	// generated by the caller in the second call, with RKeyMax.

	span, _, err := s.SpanFromEncDatums(scratchRow[:keyLen])
	return span.Key, err
}

// KeysFromVectorPrefixConstraint extracts the encoded prefix keys from a
// vector search operator's prefix constraint. It validates that each span in
// the constraint has a single key.
func (s *Builder) KeysFromVectorPrefixConstraint(
	ctx context.Context, prefixConstraint *constraint.Constraint,
) ([]roachpb.Key, error) {
	if prefixConstraint == nil || prefixConstraint.Spans.Count() == 0 {
		// No prefix.
		return nil, nil
	}
	prefixKeys := make([]roachpb.Key, prefixConstraint.Spans.Count())
	for i, n := 0, prefixConstraint.Spans.Count(); i < n; i++ {
		span := prefixConstraint.Spans.Get(i)

		// A vector index with prefix columns is organized as a forest of index
		// trees, one for each unique prefix. This structure does not support
		// scanning across multiple trees at once, so the prefix spans must have the
		// same start and end key.
		if !span.HasSingleKey(ctx, s.evalCtx) {
			return nil, errors.AssertionFailedf("constraint span %s does not have a single key", span)
		}
		// Do not include the /Table/Index prefix bytes - we only want the portion
		// of the prefix that corresponds to the prefix columns.
		var err error
		prefixKeys[i], _, err = s.encodeConstraintKey(span.StartKey(), false /* includePrefix */)
		if err != nil {
			return nil, err
		}
	}
	return prefixKeys, nil
}
