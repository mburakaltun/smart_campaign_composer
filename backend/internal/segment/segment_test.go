package segment

import (
	"encoding/json"
	"strings"
	"testing"
)

var testSchema = Schema{
	Cities:    []string{"İstanbul", "Ankara", "İzmir"},
	Languages: []string{"tr", "en"},
}

const abandonedCart = `{
  "op": "and",
  "children": [
    {"field": "city", "operator": "eq", "value": "istanbul"},
    {"field": "days_since_last_cart_add", "operator": "lte", "value": 30},
    {"op": "or", "children": [
      {"field": "days_since_last_purchase", "operator": "is_null"},
      {"field": "days_since_last_purchase", "operator": "gt", "value": 30}
    ]}
  ]
}`

func TestParseCanonicalisesCityAndCompiles(t *testing.T) {
	n, err := Parse([]byte(abandonedCart), testSchema)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(n.Children[0].Value); got != `"İstanbul"` {
		t.Fatalf("city not canonicalised: %s", got)
	}
	sql, args, err := Compile(n, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(args) != 3 {
		t.Fatalf("want 3 bind args, got %d (%v)", len(args), args)
	}
	for _, want := range []string{"$3", "$4", "$5", "IS NULL", " OR ", " AND "} {
		if !strings.Contains(sql, want) {
			t.Errorf("sql %q missing %q", sql, want)
		}
	}
	if strings.Contains(sql, "İstanbul") || strings.Contains(sql, "30") {
		t.Errorf("values must be bind parameters, found literal in %q", sql)
	}
}

func TestRejectsUnknownFieldsAndInjection(t *testing.T) {
	cases := map[string]string{
		"unknown field":     `{"field":"password","operator":"eq","value":"x"}`,
		"sql in field":      `{"field":"city; DROP TABLE users;--","operator":"eq","value":"Ankara"}`,
		"sql in value":      `{"field":"city","operator":"eq","value":"Ankara' OR '1'='1"}`,
		"unknown operator":  `{"field":"city","operator":"like","value":"%"}`,
		"number op on text": `{"field":"city","operator":"gt","value":"A"}`,
		"string for number": `{"field":"purchase_count","operator":"gt","value":"3"}`,
		"is_null non-null":  `{"field":"city","operator":"is_null"}`,
		"unknown key":       `{"field":"city","operator":"eq","value":"Ankara","sql":"1=1"}`,
		"group + leaf":      `{"op":"and","children":[{"field":"city","operator":"eq","value":"Ankara"}],"field":"city"}`,
		"bad group op":      `{"op":"xor","children":[{"field":"city","operator":"eq","value":"Ankara"}]}`,
		"empty":             `{}`,
		"not json":          `select * from users`,
		"huge number":       `{"field":"total_spent","operator":"gt","value":1e30}`,
		"unknown language":  `{"field":"language","operator":"eq","value":"xx"}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(raw), testSchema); err == nil {
				t.Fatalf("expected rejection of %s", raw)
			}
		})
	}
}

func TestDepthAndSizeLimits(t *testing.T) {
	leaf := `{"field":"purchase_count","operator":"gte","value":1}`
	deep := leaf
	for i := 0; i < 4; i++ {
		deep = `{"op":"and","children":[` + deep + `]}`
	}
	if _, err := Parse([]byte(deep), testSchema); err == nil {
		t.Error("expected depth limit error")
	}
	many := make([]string, MaxChildren+1)
	for i := range many {
		many[i] = leaf
	}
	if _, err := Parse([]byte(`{"op":"or","children":[`+strings.Join(many, ",")+`]}`), testSchema); err == nil {
		t.Error("expected too-many-children error")
	}
}

func TestInOperatorAndNullableOnly(t *testing.T) {
	n, err := Parse([]byte(`{"field":"city","operator":"in","value":["ankara","İZMİR"]}`), testSchema)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	_ = json.Unmarshal(n.Value, &got)
	if len(got) != 2 || got[0] != "Ankara" || got[1] != "İzmir" {
		t.Fatalf("canonicalisation failed: %v", got)
	}
	sql, args, err := Compile(n, 1)
	if err != nil || !strings.Contains(sql, "ANY($1::text[])") || len(args) != 1 {
		t.Fatalf("bad compile: %q %v %v", sql, args, err)
	}
}

func TestValidationErrorListsProblemsForRepair(t *testing.T) {
	_, err := Parse([]byte(`{"field":"city","operator":"eq","value":"Gotham"}`), testSchema)
	ve, ok := err.(*ValidationError)
	if !ok || len(ve.Problems) == 0 || !strings.Contains(ve.Problems[0], "Gotham") {
		t.Fatalf("want ValidationError mentioning bad value, got %v", err)
	}
}
