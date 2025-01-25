// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package opgen

import (
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scop"
	"github.com/cockroachdb/cockroach/pkg/sql/schemachanger/scpb"
)

func init() {
	opRegistry.register((*scpb.RowLevelSecurityEnabled)(nil),
		toPublic(
			scpb.Status_ABSENT,
			to(scpb.Status_PUBLIC,
				emit(func(this *scpb.RowLevelSecurityEnabled) *scop.ToggleRowLevelSecurityMode {
					return &scop.ToggleRowLevelSecurityMode{TableID: this.TableID, Enabled: true}
				}),
			),
		),
		toAbsent(
			scpb.Status_PUBLIC,
			to(scpb.Status_ABSENT,
				emit(func(this *scpb.RowLevelSecurityEnabled) *scop.ToggleRowLevelSecurityMode {
					return &scop.ToggleRowLevelSecurityMode{TableID: this.TableID, Enabled: false}
				}),
			),
		),
	)
}
