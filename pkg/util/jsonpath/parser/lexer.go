// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package parser

import (
	"strings"

	"github.com/cockroachdb/cockroach/pkg/sql/parser"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgcode"
	"github.com/cockroachdb/cockroach/pkg/sql/pgwire/pgerror"
	"github.com/cockroachdb/cockroach/pkg/util/jsonpath"
	"github.com/cockroachdb/errors"
)

type lexer struct {
	in string
	// tokens contains tokens generated by the scanner.
	tokens []jsonpathSymType

	// lastPos is the position into the tokens slice of the last
	// token returned by Lex().
	lastPos int

	expr *jsonpath.Jsonpath

	lastError error

	parser jsonpathParser
}

func (l *lexer) init(sql string, tokens []jsonpathSymType, p jsonpathParser) {
	l.in = sql
	l.tokens = tokens
	l.lastPos = -1
	l.expr = nil
	l.lastError = nil
	l.parser = p
}

// cleanup is used to avoid holding on to memory unnecessarily (for the cases
// where we reuse a scanner).
func (l *lexer) cleanup() {
	l.tokens = nil
	l.expr = nil
	l.lastError = nil
}

func (l *lexer) lastToken() jsonpathSymType {
	if l.lastPos < 0 {
		return jsonpathSymType{}
	}

	if l.lastPos >= len(l.tokens) {
		return jsonpathSymType{
			id:  0,
			pos: int32(len(l.in)),
			str: "EOF",
		}
	}
	return l.tokens[l.lastPos]
}

// Lex implements the jsonpathLexer interface.
func (l *lexer) Lex(lval *jsonpathSymType) int {
	l.lastPos++
	if l.lastPos >= len(l.tokens) {
		lval.id = 0
		lval.pos = int32(len(l.in))
		lval.str = "EOF"
		return 0
	}
	*lval = l.tokens[l.lastPos]
	return int(lval.id)
}

// Error implements the jsonpathLexer interface.
func (l *lexer) Error(s string) {
	s = strings.TrimPrefix(s, "syntax error: ") // we'll add it again below.
	err := pgerror.WithCandidateCode(errors.Newf("%s", s), pgcode.Syntax)
	lastTok := l.lastToken()
	l.lastError = parser.PopulateErrorDetails(lastTok.id, lastTok.str, lastTok.pos, err, l.in)
}

func (l *lexer) SetJsonpath(expr jsonpath.Jsonpath) {
	l.expr = &expr
}
