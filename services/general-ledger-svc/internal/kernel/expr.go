package kernel

import (
	"math/big"
	"regexp"
	"strings"
)

// Rule amounts are written in a tiny declarative expression language over the event's facts: numbers, fact names,
// + - * / and parentheses. There is no function call, no loop and no way to run code, so a rule is data that can be
// parsed, checked and replayed (the standard prefers a declarative DSL over code for mappings and formulas).

type node interface {
	eval(env map[string]*big.Rat) (*big.Rat, error)
}

type numNode struct{ v *big.Rat }
type refNode struct{ name string }
type negNode struct{ x node }
type binNode struct {
	op   byte
	l, r node
}

func (n numNode) eval(map[string]*big.Rat) (*big.Rat, error) { return new(big.Rat).Set(n.v), nil }
func (n refNode) eval(env map[string]*big.Rat) (*big.Rat, error) {
	v, ok := env[n.name]
	if !ok {
		return nil, exc(FamRule, "FACT_MISSING", "the rule needs the fact %q and the event does not carry it", n.name).with("fact", n.name)
	}
	return new(big.Rat).Set(v), nil
}
func (n negNode) eval(env map[string]*big.Rat) (*big.Rat, error) {
	v, err := n.x.eval(env)
	if err != nil {
		return nil, err
	}
	return v.Neg(v), nil
}
func (n binNode) eval(env map[string]*big.Rat) (*big.Rat, error) {
	l, err := n.l.eval(env)
	if err != nil {
		return nil, err
	}
	r, err := n.r.eval(env)
	if err != nil {
		return nil, err
	}
	switch n.op {
	case '+':
		return l.Add(l, r), nil
	case '-':
		return l.Sub(l, r), nil
	case '*':
		return l.Mul(l, r), nil
	default:
		if r.Sign() == 0 {
			return nil, exc(FamData, "DIVISION_BY_ZERO", "a rule amount divides by zero")
		}
		return l.Quo(l, r), nil
	}
}

// Expr is a parsed amount expression.
type Expr struct {
	src  string
	root node
	refs []string
}

// Refs lists the fact names the expression reads, sorted by first use.
func (e *Expr) Refs() []string { return e.refs }

// String returns the source text.
func (e *Expr) String() string { return e.src }

var identRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}(\.[a-z][a-z0-9_]{0,62})?$`)

type parser struct {
	s    string
	pos  int
	refs []string
	seen map[string]bool
	dep  int
}

// ParseExpr parses an amount expression.
func ParseExpr(src string) (*Expr, error) {
	src = strings.TrimSpace(src)
	if src == "" || len(src) > 400 {
		return nil, exc(FamData, "EXPRESSION_INVALID", "an amount expression is 1-400 characters")
	}
	p := &parser{s: src, seen: map[string]bool{}}
	n, err := p.expr()
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.pos != len(p.s) {
		return nil, exc(FamData, "EXPRESSION_INVALID", "unexpected %q at position %d", p.s[p.pos:p.pos+1], p.pos)
	}
	return &Expr{src: src, root: n, refs: p.refs}, nil
}

func (p *parser) ws() {
	for p.pos < len(p.s) && p.s[p.pos] == ' ' {
		p.pos++
	}
}

func (p *parser) expr() (node, error) {
	l, err := p.term()
	if err != nil {
		return nil, err
	}
	for {
		p.ws()
		if p.pos < len(p.s) && (p.s[p.pos] == '+' || p.s[p.pos] == '-') {
			op := p.s[p.pos]
			p.pos++
			r, err := p.term()
			if err != nil {
				return nil, err
			}
			l = binNode{op, l, r}
			continue
		}
		return l, nil
	}
}

func (p *parser) term() (node, error) {
	l, err := p.factor()
	if err != nil {
		return nil, err
	}
	for {
		p.ws()
		if p.pos < len(p.s) && (p.s[p.pos] == '*' || p.s[p.pos] == '/') {
			op := p.s[p.pos]
			p.pos++
			r, err := p.factor()
			if err != nil {
				return nil, err
			}
			l = binNode{op, l, r}
			continue
		}
		return l, nil
	}
}

func (p *parser) factor() (node, error) {
	p.ws()
	if p.pos >= len(p.s) {
		return nil, exc(FamData, "EXPRESSION_INVALID", "the expression ends where a value was expected")
	}
	if p.dep++; p.dep > 40 {
		return nil, exc(FamData, "EXPRESSION_INVALID", "the expression is nested too deeply")
	}
	defer func() { p.dep-- }()
	c := p.s[p.pos]
	switch {
	case c == '-':
		p.pos++
		x, err := p.factor()
		if err != nil {
			return nil, err
		}
		return negNode{x}, nil
	case c == '(':
		p.pos++
		x, err := p.expr()
		if err != nil {
			return nil, err
		}
		p.ws()
		if p.pos >= len(p.s) || p.s[p.pos] != ')' {
			return nil, exc(FamData, "EXPRESSION_INVALID", "a parenthesis is not closed")
		}
		p.pos++
		return x, nil
	case c >= '0' && c <= '9':
		start := p.pos
		for p.pos < len(p.s) && (p.s[p.pos] >= '0' && p.s[p.pos] <= '9' || p.s[p.pos] == '.') {
			p.pos++
		}
		v, ok := ParseDecimal(p.s[start:p.pos])
		if !ok {
			return nil, exc(FamData, "EXPRESSION_INVALID", "%q is not a decimal number", p.s[start:p.pos])
		}
		return numNode{v}, nil
	case c >= 'a' && c <= 'z':
		start := p.pos
		for p.pos < len(p.s) && (p.s[p.pos] >= 'a' && p.s[p.pos] <= 'z' || p.s[p.pos] >= '0' && p.s[p.pos] <= '9' || p.s[p.pos] == '_' || p.s[p.pos] == '.') {
			p.pos++
		}
		name := p.s[start:p.pos]
		if !identRE.MatchString(name) {
			return nil, exc(FamData, "EXPRESSION_INVALID", "%q is not a fact name", name)
		}
		if !p.seen[name] {
			p.seen[name] = true
			p.refs = append(p.refs, name)
		}
		return refNode{name}, nil
	}
	return nil, exc(FamData, "EXPRESSION_INVALID", "unexpected %q at position %d", string(c), p.pos)
}

// Eval computes the expression exactly over the given facts.
func (e *Expr) Eval(env map[string]*big.Rat) (*big.Rat, error) { return e.root.eval(env) }
