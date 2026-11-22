// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package named

import (
	"slices"
	"testing"
)

func TestCompile(t *testing.T) {
	tests := []struct {
		query string
		ph    Placeholder
		want  string
		names []string
	}{
		{"INSERT INTO people (person_id, name) VALUES (:id, :full_name)", Dollar,
			"INSERT INTO people (person_id, name) VALUES ($1, $2)", []string{"id", "full_name"}},
		{"INSERT INTO t VALUES (:a, :b)", Question, "INSERT INTO t VALUES (?, ?)", []string{"a", "b"}},
		{"INSERT INTO t VALUES (:a, :b)", AtP, "INSERT INTO t VALUES (@p1, @p2)", []string{"a", "b"}},
		{"INSERT INTO t VALUES (:a, :b)", Colon, "INSERT INTO t VALUES (:1, :2)", []string{"a", "b"}},
		{"UPDATE t SET a = :a, b = :a WHERE id = :ID", Dollar, "UPDATE t SET a = $1, b = $2 WHERE id = $3",
			[]string{"a", "a", "ID"}},
		{"INSERT INTO t VALUES (:n::int, ':not', 'it''s :no', \"col:x\", `y:z`)", Dollar,
			"INSERT INTO t VALUES ($1::int, ':not', 'it''s :no', \"col:x\", `y:z`)", []string{"n"}},
		{"INSERT INTO t VALUES (:a) -- :comment\n/* :block */", Question,
			"INSERT INTO t VALUES (?) -- :comment\n/* :block */", []string{"a"}},
		{"SELECT 1 WHERE x = :1 OR y = :", Question, "SELECT 1 WHERE x = :1 OR y = :", nil},
	}
	for _, tt := range tests {
		got, names, err := Compile(tt.query, tt.ph)
		if err != nil || got != tt.want || !slices.Equal(names, tt.names) {
			t.Errorf("Compile(%q)\n got %q %v %v\nwant %q %v", tt.query, got, names, err, tt.want, tt.names)
		}
	}
}

func TestUnclosedQuotesAreReported(t *testing.T) {
	for _, q := range []string{"INSERT INTO t VALUES ('x, :a)", "INSERT /* :a"} {
		if _, _, err := Compile(q, Question); err == nil {
			t.Errorf("%q must fail", q)
		}
	}
}
