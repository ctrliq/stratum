// policy.go holds the license policy (the allowlist and the exceptions) and
// evaluates SPDX license expressions against it.
package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// allowlist is every license code stratum ships may carry.
var allowlist = []string{"MIT", "BSD-2-Clause", "BSD-3-Clause", "Apache-2.0", "ISC", "0BSD", "Zlib", "BSL-1.0"}

// exception lets one package carry licenses the allowlist does not.
type exception struct {
	licenses []string // the SPDX ids it allows
	decision string   // the human's recorded decision
}

// exceptions is keyed by Go import path or npm package name. Only a human
// decision adds an entry. Each entry covers the one package it names, so
// another package of the same module is still checked in full.
var exceptions = map[string]exception{
	// go-digest's code is Apache-2.0; its LICENSE.docs puts the
	// documentation under CC-BY-SA-4.0. Human decision, 2026-10-08.
	"github.com/opencontainers/go-digest": {licenses: []string{"CC-BY-SA-4.0"}, decision: "Exception: docs license"},
}

// judge checks the license expression e, the license of package name, and
// describes it with source, which says where e came from.
func judge(name string, e *expr, source string) verdict {
	allowed := idSet(allowlist)
	ok, denied := e.eval(allowed)
	if ok {
		return verdict{}
	}
	if exc, found := exceptions[name]; found {
		excepted := idSet(exc.licenses)
		for id := range allowed {
			excepted[id] = true
		}
		if ok, _ := e.eval(excepted); ok {
			var covered []string
			for _, id := range denied {
				if excepted[canonical(id)] {
					covered = append(covered, id)
				}
			}
			return verdict{levelNote, fmt.Sprintf("license allowed only by exception %q: %s (%s)", exc.decision, strings.Join(covered, ", "), source)}
		}
		_, denied = e.eval(excepted)
	}
	return verdict{levelFail, fmt.Sprintf("license not allowed: %s (%s)", strings.Join(denied, ", "), source)}
}

// canonical returns the form license ids are compared in: SPDX ids match
// case-insensitively, and a trailing "+" (this version or any later one)
// includes the version itself.
func canonical(id string) string {
	return strings.ToUpper(strings.TrimSuffix(id, "+"))
}

func idSet(ids []string) map[string]bool {
	set := make(map[string]bool, len(ids))
	for _, id := range ids {
		set[canonical(id)] = true
	}
	return set
}

// noLicense reports whether an SPDX expression is one of the special values
// that state no license (NONE) or no knowledge of one (NOASSERTION).
func noLicense(s string) bool {
	return strings.EqualFold(s, "NONE") || strings.EqualFold(s, "NOASSERTION")
}

// Expression operators.
const (
	opAnd = "AND"
	opOr  = "OR"
)

// expr is a parsed SPDX license expression: a license id, or an AND or OR of
// sub-expressions. "id WITH exception" parses as its license id, because a
// license exception only adds permissions.
type expr struct {
	op   string // "", opAnd or opOr
	id   string // the license id when op is ""
	args []*expr
}

// and returns the conjunction of exprs.
func and(exprs ...*expr) *expr {
	if len(exprs) == 1 {
		return exprs[0]
	}
	return &expr{op: opAnd, args: exprs}
}

// eval reports whether e is allowed when exactly the ids in allowed (in
// canonical form) are, and if not, which ids keep it from being allowed.
func (e *expr) eval(allowed map[string]bool) (bool, []string) {
	switch e.op {
	case "":
		if allowed[canonical(e.id)] {
			return true, nil
		}
		return false, []string{e.id}
	case opOr:
		var denied []string
		for _, arg := range e.args {
			ok, d := arg.eval(allowed)
			if ok {
				return true, nil
			}
			denied = appendNew(denied, d...)
		}
		return false, denied
	default: // opAnd
		var denied []string
		for _, arg := range e.args {
			if ok, d := arg.eval(allowed); !ok {
				denied = appendNew(denied, d...)
			}
		}
		return len(denied) == 0, denied
	}
}

// appendNew appends each id not already in ids.
func appendNew(ids []string, more ...string) []string {
	for _, id := range more {
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids
}

// additionRef matches a custom license addition, which may follow WITH as an
// exception id does.
var additionRef = regexp.MustCompile(`^(?:DocumentRef-[A-Za-z0-9.-]+:)?AdditionRef-[A-Za-z0-9.-]+$`)

// exceptionID reports whether tok names a license exception: an SPDX
// exception id or an AdditionRef. A license id such as MPL-2.0 is not one, so
// "MIT WITH MPL-2.0" does not parse rather than passing as MIT.
func exceptionID(tok string) bool {
	return spdxExceptionIDs[strings.ToLower(tok)] || additionRef.MatchString(tok)
}

// licenseID matches an SPDX license id or LicenseRef, with an optional "+".
var licenseID = regexp.MustCompile(`^(?:DocumentRef-[A-Za-z0-9.-]+:)?[A-Za-z0-9.-]+\+?$`)

// parseExpr parses an SPDX license expression. AND binds tighter than OR, and
// operators match in any case, as npm package metadata writes them.
func parseExpr(s string) (*expr, error) {
	p := &exprParser{toks: tokenize(s)}
	if len(p.toks) == 0 {
		return nil, errors.New("empty license expression")
	}
	e, err := p.or()
	if err != nil {
		return nil, err
	}
	if p.pos < len(p.toks) {
		return nil, fmt.Errorf("unexpected %q in license expression", p.toks[p.pos])
	}
	return e, nil
}

func tokenize(s string) []string {
	var toks []string
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			toks = append(toks, word.String())
			word.Reset()
		}
	}
	for _, r := range s {
		switch {
		case r == '(' || r == ')':
			flush()
			toks = append(toks, string(r))
		case unicode.IsSpace(r):
			flush()
		default:
			word.WriteRune(r)
		}
	}
	flush()
	return toks
}

type exprParser struct {
	toks []string
	pos  int
}

func (p *exprParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *exprParser) next() string {
	tok := p.peek()
	if tok != "" {
		p.pos++
	}
	return tok
}

// or parses and-expressions joined by OR.
func (p *exprParser) or() (*expr, error) {
	return p.joined(opOr, p.and)
}

// and parses WITH-expressions joined by AND.
func (p *exprParser) and() (*expr, error) {
	return p.joined(opAnd, p.with)
}

func (p *exprParser) joined(op string, operand func() (*expr, error)) (*expr, error) {
	first, err := operand()
	if err != nil {
		return nil, err
	}
	args := []*expr{first}
	for strings.EqualFold(p.peek(), op) {
		p.next()
		arg, err := operand()
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
	}
	if len(args) == 1 {
		return first, nil
	}
	return &expr{op: op, args: args}, nil
}

// with parses a license, optionally followed by WITH and an exception id.
func (p *exprParser) with() (*expr, error) {
	e, err := p.atom()
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(p.peek(), "WITH") {
		p.next()
		if exc := p.next(); !exceptionID(exc) {
			return nil, fmt.Errorf("WITH needs an SPDX license exception id, got %q", exc)
		}
		if e.op != "" {
			return nil, errors.New("WITH must follow a license id")
		}
	}
	return e, nil
}

// atom parses a license id or a parenthesised expression.
func (p *exprParser) atom() (*expr, error) {
	tok := p.next()
	switch {
	case tok == "":
		return nil, errors.New("license expression ends early")
	case tok == "(":
		e, err := p.or()
		if err != nil {
			return nil, err
		}
		if p.next() != ")" {
			return nil, errors.New("license expression has an unclosed parenthesis")
		}
		return e, nil
	// NONE and NOASSERTION may stand alone, which callers check for, but are
	// no license an expression can combine: "MIT OR NOASSERTION" is unreadable.
	case isOperator(tok) || noLicense(tok) || !licenseID.MatchString(tok):
		return nil, fmt.Errorf("unexpected %q in license expression", tok)
	}
	return &expr{id: tok}, nil
}

func isOperator(tok string) bool {
	for _, op := range []string{opAnd, opOr, "WITH"} {
		if strings.EqualFold(tok, op) {
			return true
		}
	}
	return false
}
