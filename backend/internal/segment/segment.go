// Package segment defines the audience rule language that Claude is allowed to
// produce, validates it against a strict whitelist and compiles it into a
// parameterised SQL fragment. LLM output is never executed as SQL: only field
// names and operators from the tables below ever reach the query text, and every
// value travels as a bind parameter.
package segment

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
)

type FieldType int

const (
	String FieldType = iota
	Number
)

// FieldDef describes one queryable attribute. SQL is trusted, hand-written text.
type FieldDef struct {
	Name     string
	Type     FieldType
	SQL      string
	Nullable bool
	Doc      string
}

// Fields is the complete whitelist of attributes a rule may reference.
var Fields = map[string]FieldDef{
	"city":                     {Name: "city", Type: String, SQL: "u.city", Doc: "City of the customer (canonical name, e.g. İstanbul)"},
	"language":                 {Name: "language", Type: String, SQL: "u.language", Doc: "Preferred language code: tr or en"},
	"purchase_count":           {Name: "purchase_count", Type: Number, SQL: "u.purchase_count", Doc: "Lifetime number of orders"},
	"total_spent":              {Name: "total_spent", Type: Number, SQL: "u.total_spent", Doc: "Lifetime spend in TRY"},
	"cart_items":               {Name: "cart_items", Type: Number, SQL: "u.cart_items", Doc: "Items currently sitting in the cart"},
	"days_since_last_cart_add": {Name: "days_since_last_cart_add", Type: Number, Nullable: true, SQL: "(EXTRACT(EPOCH FROM (now() - u.last_cart_add_at)) / 86400.0)", Doc: "Days since the customer last added an item to the cart; null if never"},
	"days_since_last_purchase": {Name: "days_since_last_purchase", Type: Number, Nullable: true, SQL: "(EXTRACT(EPOCH FROM (now() - u.last_purchase_at)) / 86400.0)", Doc: "Days since the customer last purchased; null if never purchased"},
}

var stringOps = map[string]bool{"eq": true, "neq": true, "in": true, "not_in": true}
var numberOps = map[string]bool{"eq": true, "neq": true, "gt": true, "gte": true, "lt": true, "lte": true, "in": false}
var nullOps = map[string]bool{"is_null": true, "is_not_null": true}

const (
	MaxDepth    = 3
	MaxLeaves   = 20
	MaxChildren = 10
	MaxListLen  = 20
	MaxStrLen   = 64
	MaxNumber   = 1e9
)

// Node is either a group (Op + Children) or a leaf (Field + Operator + Value).
type Node struct {
	Op       string          `json:"op,omitempty"`
	Children []Node          `json:"children,omitempty"`
	Field    string          `json:"field,omitempty"`
	Operator string          `json:"operator,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
}

// Schema carries the dynamic parts of validation (values that live in the data).
type Schema struct {
	Cities    []string
	Languages []string
}

type ValidationError struct{ Problems []string }

func (e *ValidationError) Error() string {
	return "invalid segment rule: " + strings.Join(e.Problems, "; ")
}

// Parse decodes strictly (unknown keys are rejected), then validates and
// canonicalises the rule.
func Parse(raw []byte, sc Schema) (Node, error) {
	var n Node
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&n); err != nil {
		return Node{}, &ValidationError{Problems: []string{"rule is not valid JSON for the expected shape: " + err.Error()}}
	}
	if dec.More() {
		return Node{}, &ValidationError{Problems: []string{"trailing data after rule"}}
	}
	if err := Validate(&n, sc); err != nil {
		return Node{}, err
	}
	return n, nil
}

// Validate checks the tree and rewrites string values to their canonical spelling.
func Validate(n *Node, sc Schema) error {
	v := &validator{sc: sc}
	v.node(n, 1)
	if v.leaves == 0 {
		v.add("rule must contain at least one condition")
	}
	if v.leaves > MaxLeaves {
		v.add("too many conditions (%d > %d)", v.leaves, MaxLeaves)
	}
	if len(v.problems) > 0 {
		return &ValidationError{Problems: v.problems}
	}
	return nil
}

type validator struct {
	sc       Schema
	leaves   int
	problems []string
}

func (v *validator) add(format string, a ...any) {
	if len(v.problems) < 10 {
		v.problems = append(v.problems, fmt.Sprintf(format, a...))
	}
}

func (v *validator) node(n *Node, depth int) {
	if depth > MaxDepth {
		v.add("rule is nested deeper than %d levels", MaxDepth)
		return
	}
	isGroup := n.Op != "" || len(n.Children) > 0
	isLeaf := n.Field != "" || n.Operator != "" || len(n.Value) > 0
	switch {
	case isGroup && isLeaf:
		v.add("a node cannot be both a group (op/children) and a condition (field/operator/value)")
	case isGroup:
		if n.Op != "and" && n.Op != "or" {
			v.add("group op must be \"and\" or \"or\", got %q", n.Op)
		}
		if len(n.Children) == 0 || len(n.Children) > MaxChildren {
			v.add("a group needs between 1 and %d children", MaxChildren)
		}
		for i := range n.Children {
			v.node(&n.Children[i], depth+1)
		}
	case isLeaf:
		v.leaves++
		v.leaf(n)
	default:
		v.add("empty node")
	}
}

func (v *validator) leaf(n *Node) {
	def, ok := Fields[n.Field]
	if !ok {
		v.add("unknown field %q (allowed: %s)", n.Field, strings.Join(fieldNames(), ", "))
		return
	}
	if nullOps[n.Operator] {
		if !def.Nullable {
			v.add("operator %q is not allowed on non-nullable field %q", n.Operator, n.Field)
		}
		if len(n.Value) > 0 && string(n.Value) != "null" {
			v.add("operator %q takes no value", n.Operator)
		}
		n.Value = nil
		return
	}
	switch def.Type {
	case String:
		if !stringOps[n.Operator] {
			v.add("operator %q is not allowed on string field %q (allowed: eq, neq, in, not_in)", n.Operator, n.Field)
			return
		}
		v.stringValue(n, def)
	case Number:
		if !numberOps[n.Operator] {
			v.add("operator %q is not allowed on numeric field %q (allowed: eq, neq, gt, gte, lt, lte, is_null, is_not_null)", n.Operator, n.Field)
			return
		}
		v.numberValue(n)
	}
}

func (v *validator) stringValue(n *Node, def FieldDef) {
	list := n.Operator == "in" || n.Operator == "not_in"
	var vals []string
	if list {
		if err := json.Unmarshal(n.Value, &vals); err != nil || len(vals) == 0 || len(vals) > MaxListLen {
			v.add("operator %q on %q needs a non-empty list of up to %d strings", n.Operator, n.Field, MaxListLen)
			return
		}
	} else {
		var s string
		if err := json.Unmarshal(n.Value, &s); err != nil {
			v.add("operator %q on %q needs a single string value", n.Operator, n.Field)
			return
		}
		vals = []string{s}
	}
	for i, s := range vals {
		if s == "" || len([]rune(s)) > MaxStrLen {
			v.add("value for %q must be 1-%d characters", n.Field, MaxStrLen)
			return
		}
		canon, ok := v.canonical(def.Name, s)
		if !ok {
			v.add("%q is not a known value for %q (known: %s)", s, def.Name, strings.Join(v.known(def.Name), ", "))
			return
		}
		vals[i] = canon
	}
	if list {
		n.Value, _ = json.Marshal(vals)
	} else {
		n.Value, _ = json.Marshal(vals[0])
	}
}

func (v *validator) known(field string) []string {
	if field == "city" {
		return v.sc.Cities
	}
	return v.sc.Languages
}

func (v *validator) canonical(field, s string) (string, bool) {
	for _, k := range v.known(field) {
		if fold(k) == fold(s) {
			return k, true
		}
	}
	return "", false
}

// fold lowercases using Turkish rules so "ISTANBUL", "istanbul" and "İstanbul" match.
func fold(s string) string {
	return strings.ToLowerSpecial(unicode.TurkishCase, strings.TrimSpace(s))
}

func (v *validator) numberValue(n *Node) {
	var f float64
	if err := json.Unmarshal(n.Value, &f); err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > MaxNumber {
		v.add("operator %q on %q needs a finite number", n.Operator, n.Field)
	}
}

func fieldNames() []string {
	names := make([]string, 0, len(Fields))
	for k := range Fields {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// FieldDocs is used when building the prompt for Claude.
func FieldDocs() string {
	var b strings.Builder
	for _, name := range fieldNames() {
		d := Fields[name]
		typ := "string"
		if d.Type == Number {
			typ = "number"
		}
		fmt.Fprintf(&b, "- %s (%s%s): %s\n", d.Name, typ, map[bool]string{true: ", nullable", false: ""}[d.Nullable], d.Doc)
	}
	return b.String()
}

// Compile turns a *validated* rule into a SQL boolean expression over alias "u".
// Placeholders are numbered from startArg; the returned args line up with them.
func Compile(n Node, startArg int) (string, []any, error) {
	c := &compiler{next: startArg}
	sql, err := c.node(n)
	if err != nil {
		return "", nil, err
	}
	return sql, c.args, nil
}

type compiler struct {
	next int
	args []any
}

func (c *compiler) bind(v any) string {
	c.args = append(c.args, v)
	p := fmt.Sprintf("$%d", c.next)
	c.next++
	return p
}

func (c *compiler) node(n Node) (string, error) {
	if n.Op != "" {
		if n.Op != "and" && n.Op != "or" {
			return "", errors.New("invalid group operator")
		}
		parts := make([]string, 0, len(n.Children))
		for _, ch := range n.Children {
			s, err := c.node(ch)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		if len(parts) == 0 {
			return "", errors.New("empty group")
		}
		return "(" + strings.Join(parts, " "+strings.ToUpper(n.Op)+" ") + ")", nil
	}
	def, ok := Fields[n.Field]
	if !ok {
		return "", fmt.Errorf("unknown field %q", n.Field)
	}
	switch n.Operator {
	case "is_null":
		return "(" + def.SQL + " IS NULL)", nil
	case "is_not_null":
		return "(" + def.SQL + " IS NOT NULL)", nil
	}
	if def.Type == String {
		return c.stringLeaf(def, n)
	}
	return c.numberLeaf(def, n)
}

func (c *compiler) stringLeaf(def FieldDef, n Node) (string, error) {
	switch n.Operator {
	case "eq", "neq":
		var s string
		if err := json.Unmarshal(n.Value, &s); err != nil {
			return "", err
		}
		op := map[string]string{"eq": "=", "neq": "<>"}[n.Operator]
		return fmt.Sprintf("(%s %s %s::text)", def.SQL, op, c.bind(s)), nil
	case "in", "not_in":
		var vals []string
		if err := json.Unmarshal(n.Value, &vals); err != nil {
			return "", err
		}
		not := ""
		if n.Operator == "not_in" {
			not = "NOT "
		}
		return fmt.Sprintf("(%s%s = ANY(%s::text[]))", not, def.SQL, c.bind(vals)), nil
	}
	return "", fmt.Errorf("operator %q not supported for strings", n.Operator)
}

func (c *compiler) numberLeaf(def FieldDef, n Node) (string, error) {
	ops := map[string]string{"eq": "=", "neq": "<>", "gt": ">", "gte": ">=", "lt": "<", "lte": "<="}
	op, ok := ops[n.Operator]
	if !ok {
		return "", fmt.Errorf("operator %q not supported for numbers", n.Operator)
	}
	var f float64
	if err := json.Unmarshal(n.Value, &f); err != nil {
		return "", err
	}
	return fmt.Sprintf("(CAST(%s AS double precision) %s %s::double precision)", def.SQL, op, c.bind(f)), nil
}
