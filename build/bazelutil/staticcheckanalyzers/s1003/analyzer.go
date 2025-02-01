// Copyright 2021 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.
//
// Code generated by generate-staticcheck; DO NOT EDIT.
//
//go:build bazel

package s1003

import (
	util "github.com/cockroachdb/cockroach/pkg/testutils/lint/passes/staticcheck"
	"golang.org/x/tools/go/analysis"
	"honnef.co/go/tools/simple"
)

var Analyzer *analysis.Analyzer

func init() {
	for _, analyzer := range simple.Analyzers {
		if analyzer.Analyzer.Name == "S1003" {
			Analyzer = analyzer.Analyzer
			break
		}
	}
	util.MungeAnalyzer(Analyzer)
}
