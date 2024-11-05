// Copyright 2023 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

// Code generated by "stringer"; DO NOT EDIT.

package screl

import "strconv"

func _() {
	// An "invalid array index" compiler error signifies that the constant values have changed.
	// Re-run the stringer command to generate them again.
	var x [1]struct{}
	_ = x[DescID-1]
	_ = x[IndexID-2]
	_ = x[ColumnFamilyID-3]
	_ = x[ColumnID-4]
	_ = x[ConstraintID-5]
	_ = x[Name-6]
	_ = x[ReferencedDescID-7]
	_ = x[Comment-8]
	_ = x[TemporaryIndexID-9]
	_ = x[SourceIndexID-10]
	_ = x[RecreateSourceIndexID-11]
	_ = x[SeqNum-12]
	_ = x[TriggerID-13]
	_ = x[TargetStatus-14]
	_ = x[CurrentStatus-15]
	_ = x[Element-16]
	_ = x[Target-17]
	_ = x[ReferencedTypeIDs-18]
	_ = x[ReferencedSequenceIDs-19]
	_ = x[ReferencedFunctionIDs-20]
	_ = x[ReferencedColumnIDs-21]
	_ = x[Expr-22]
	_ = x[TypeName-23]
	_ = x[PartitionName-24]
	_ = x[Usage-25]
	_ = x[AttrMax-25]
}

func (i Attr) String() string {
	switch i {
	case DescID:
		return "DescID"
	case IndexID:
		return "IndexID"
	case ColumnFamilyID:
		return "ColumnFamilyID"
	case ColumnID:
		return "ColumnID"
	case ConstraintID:
		return "ConstraintID"
	case Name:
		return "Name"
	case ReferencedDescID:
		return "ReferencedDescID"
	case Comment:
		return "Comment"
	case TemporaryIndexID:
		return "TemporaryIndexID"
	case SourceIndexID:
		return "SourceIndexID"
	case RecreateSourceIndexID:
		return "RecreateSourceIndexID"
	case SeqNum:
		return "SeqNum"
	case TriggerID:
		return "TriggerID"
	case TargetStatus:
		return "TargetStatus"
	case CurrentStatus:
		return "CurrentStatus"
	case Element:
		return "Element"
	case Target:
		return "Target"
	case ReferencedTypeIDs:
		return "ReferencedTypeIDs"
	case ReferencedSequenceIDs:
		return "ReferencedSequenceIDs"
	case ReferencedFunctionIDs:
		return "ReferencedFunctionIDs"
	case ReferencedColumnIDs:
		return "ReferencedColumnIDs"
	case Expr:
		return "Expr"
	case TypeName:
		return "TypeName"
	case PartitionName:
		return "PartitionName"
	case Usage:
		return "Usage"
	default:
		return "Attr(" + strconv.FormatInt(int64(i), 10) + ")"
	}
}
