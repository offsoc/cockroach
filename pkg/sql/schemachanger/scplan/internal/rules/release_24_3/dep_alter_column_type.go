// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package release_24_3

import (
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/rel"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
	. "github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/rules"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scplan/internal/scgraph"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/screl"
)

// This rule ensures that when changing the column type, that the old column
// type is dropped before the new type is added.
func init() {
	registerDepRule(
		"column type update is decomposed as a drop then add",
		scgraph.Precedence,
		"old-column-type", "new-column-type",
		func(from, to NodeVars) rel.Clauses {
			return rel.Clauses{
				from.Type((*scpb.ColumnType)(nil)),
				to.Type((*scpb.ColumnType)(nil)),
				JoinOnColumnID(from, to, "table-id", "col-id"),
				from.TargetStatus(scpb.ToAbsent),
				from.CurrentStatus(scpb.Status_PUBLIC),
				to.TargetStatus(scpb.ToPublic),
				to.CurrentStatus(scpb.Status_ABSENT),
			}
		},
	)

	registerDepRule(
		"column type is changed to public after doing validation of a transient check constraint",
		scgraph.SameStagePrecedence,
		"transient-check-constraint", "column-type",
		func(from, to NodeVars) rel.Clauses {
			colID := rel.Var("columnID")
			return rel.Clauses{
				from.Type((*scpb.CheckConstraint)(nil)),
				to.Type((*scpb.ColumnType)(nil)),
				JoinOnDescID(from, to, "table-id"),
				to.El.AttrEqVar(screl.ColumnID, colID),
				from.ReferencedColumnIDsContains(colID),
				from.TargetStatus(scpb.TransientAbsent),
				to.TargetStatus(scpb.ToPublic),
				from.CurrentStatus(scpb.Status_TRANSIENT_VALIDATED),
				to.CurrentStatus(scpb.Status_PUBLIC),
			}
		},
	)
}
