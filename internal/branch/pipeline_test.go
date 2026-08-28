// SPDX-License-Identifier: AGPL-3.0-or-later

package branch

import (
	"strings"
	"testing"
)

// sqlIdent preserves the source identifier verbatim (case + punctuation), quoted,
// truncating only to Postgres's 63-byte limit — so a migration never renames the
// source schema.
func TestSQLIdent(t *testing.T) {
	cases := map[string]string{
		"createdAt":    `"createdAt"`,
		"userId":       `"userId"`,
		"leaseAiChats": `"leaseAiChats"`,
		"my.field":     `"my.field"`, // punctuation preserved
		`he"llo`:       `"he""llo"`,  // internal quote doubled
		"":             `"col"`,      // empty fallback
	}
	for in, want := range cases {
		if got := sqlIdent(in); got != want {
			t.Errorf("sqlIdent(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 70)
	if got := sqlIdent(long); got != `"`+strings.Repeat("a", 63)+`"` {
		t.Errorf("sqlIdent(70 chars) not truncated to 63: %q", got)
	}
}

// renderSQL resolves {{ ref() }} / {{ source() }} to schema-qualified identifiers.
func TestRenderSQL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"SELECT * FROM {{ source('buildings') }}", `SELECT * FROM raw."buildings"`},
		{"SELECT * FROM {{ ref('stg_orders') }}", `SELECT * FROM public."stg_orders"`},
		{`SELECT * FROM {{ source("orders") }} JOIN {{ ref('dim_users') }} u`, `SELECT * FROM raw."orders" JOIN public."dim_users" u`},
		{"{{ref('a')}}+{{source('b')}}", `public."a"+raw."b"`}, // tight whitespace
		// case is preserved exactly (schema fidelity), not folded to lowercase:
		{"SELECT * FROM {{ source('leaseAiChats') }}", `SELECT * FROM raw."leaseAiChats"`},
		{"no templates here", "no templates here"},
	}
	for _, c := range cases {
		if got := renderSQL(c.in); got != c.want {
			t.Errorf("renderSQL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// testQuery builds the correct "count of violating rows" SQL per assertion type.
func TestTestQuery(t *testing.T) {
	cases := []struct {
		name string
		test PipelineTest
		want string
		err  bool
	}{
		{"not_null", PipelineTest{Type: "not_null", Model: "stg", Column: "name"},
			`SELECT count(*) FROM public."stg" WHERE "name" IS NULL`, false},
		{"unique", PipelineTest{Type: "unique", Model: "users", Column: "email"},
			`SELECT count(*) FROM (SELECT "email" FROM public."users" GROUP BY "email" HAVING count(*)>1) x`, false},
		{"accepted_values", PipelineTest{Type: "accepted_values", Model: "orders", Column: "status", Values: []string{"new", "paid"}},
			`SELECT count(*) FROM public."orders" WHERE "status" NOT IN ('new','paid')`, false},
		{"row_count_min", PipelineTest{Type: "row_count_min", Model: "m", Min: 5},
			`SELECT CASE WHEN count(*)>=5 THEN 0 ELSE 1 END FROM public."m"`, false},
		{"custom", PipelineTest{Type: "custom", SQL: "SELECT 1 FROM {{ ref('m') }} WHERE x<0"},
			`SELECT count(*) FROM (SELECT 1 FROM public."m" WHERE x<0) x`, false},
		{"accepted_values empty", PipelineTest{Type: "accepted_values", Model: "m", Column: "c"}, "", true},
		{"unknown type", PipelineTest{Type: "bogus", Model: "m"}, "", true},
	}
	for _, c := range cases {
		got, err := testQuery(c.test)
		if c.err {
			if err == nil {
				t.Errorf("%s: expected an error", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", c.name, err)
		} else if got != c.want {
			t.Errorf("%s: testQuery = %q, want %q", c.name, got, c.want)
		}
	}
}

// jsonColumn maps a document key's observed JSON types to a Postgres column.
func TestJSONColumn(t *testing.T) {
	cases := []struct {
		name     string
		key      string
		kinds    map[string]bool
		wantType string
		wantExpr string
	}{
		{"id unwraps oid", "_id", map[string]bool{"object": true}, "text", `coalesce(doc->'_id'->>'$oid', doc->>'_id')`},
		{"string", "name", map[string]bool{"string": true}, "text", `doc->>'name'`},
		{"number", "age", map[string]bool{"number": true}, "numeric", `(doc->>'age')::numeric`},
		{"boolean", "ok", map[string]bool{"boolean": true}, "boolean", `(doc->>'ok')::boolean`},
		{"nested object", "addr", map[string]bool{"object": true}, "jsonb", `doc->'addr'`},
		{"array", "tags", map[string]bool{"array": true}, "jsonb", `doc->'tags'`},
		{"mixed → jsonb", "v", map[string]bool{"string": true, "number": true}, "jsonb", `doc->'v'`},
		{"all null → text", "x", map[string]bool{"null": true}, "text", `doc->>'x'`},
		{"number with nulls → numeric", "n", map[string]bool{"number": true, "null": true}, "numeric", `(doc->>'n')::numeric`},
	}
	for _, c := range cases {
		gotType, gotExpr := jsonColumn(c.key, c.kinds)
		if gotType != c.wantType || gotExpr != c.wantExpr {
			t.Errorf("%s: jsonColumn = (%q, %q), want (%q, %q)", c.name, gotType, gotExpr, c.wantType, c.wantExpr)
		}
	}
}
