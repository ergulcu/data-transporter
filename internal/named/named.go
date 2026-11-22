// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Package named turns a query with named parameters (INSERT INTO t (a) VALUES (:a)) into the positional
// placeholders of a database ($1, ?, @p1 or :1), as Spring's NamedParameterJdbcTemplate did.
package named

import (
	"fmt"
	"strings"
)

// Placeholder returns the n-th placeholder, counting from 1.
type Placeholder func(n int) string

var (
	Dollar   Placeholder = func(n int) string { return fmt.Sprintf("$%d", n) }  // PostgreSQL
	Question Placeholder = func(int) string { return "?" }                      // MySQL, MariaDB, SQLite
	AtP      Placeholder = func(n int) string { return fmt.Sprintf("@p%d", n) } // SQL Server
	Colon    Placeholder = func(n int) string { return fmt.Sprintf(":%d", n) }  // Oracle
)

// Compile returns the query with placeholders and the parameter name of each one, in order. A name used twice
// gets two placeholders. Text in quotes and comments is left alone, and so are PostgreSQL casts (::int).
func Compile(query string, ph Placeholder) (string, []string, error) {
	var out strings.Builder
	var names []string
	for i := 0; i < len(query); {
		c := query[i]
		switch {
		case c == '\'' || c == '"' || c == '`':
			end := closing(query, i+1, c)
			if end < 0 {
				return "", nil, fmt.Errorf("unclosed %c in the target query", c)
			}
			out.WriteString(query[i : end+1])
			i = end + 1
		case c == '-' && strings.HasPrefix(query[i:], "--"):
			end := strings.IndexByte(query[i:], '\n')
			if end < 0 {
				end = len(query) - i
			}
			out.WriteString(query[i : i+end])
			i += end
		case c == '/' && strings.HasPrefix(query[i:], "/*"):
			end := strings.Index(query[i+2:], "*/")
			if end < 0 {
				return "", nil, fmt.Errorf("unclosed /* comment in the target query")
			}
			out.WriteString(query[i : i+2+end+2])
			i += 2 + end + 2
		case c == ':' && strings.HasPrefix(query[i:], "::"):
			out.WriteString("::")
			i += 2
		case c == ':' && i+1 < len(query) && isNameStart(query[i+1]):
			j := i + 1
			for j < len(query) && isNamePart(query[j]) {
				j++
			}
			names = append(names, query[i+1:j])
			out.WriteString(ph(len(names)))
			i = j
		default:
			out.WriteByte(c)
			i++
		}
	}
	return out.String(), names, nil
}

// closing finds the quote that ends a quoted part. A quote written twice is part of the text, not its end.
func closing(s string, from int, quote byte) int {
	for i := from; i < len(s); i++ {
		if s[i] == quote {
			if i+1 < len(s) && s[i+1] == quote {
				i++
				continue
			}
			return i
		}
	}
	return -1
}

func isNameStart(c byte) bool {
	return c == '_' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func isNamePart(c byte) bool {
	return isNameStart(c) || '0' <= c && c <= '9'
}
