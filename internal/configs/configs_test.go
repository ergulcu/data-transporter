// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package configs

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/ergulcu/data-transporter/internal/drivers"
	"github.com/ergulcu/data-transporter/internal/secret"
)

var ctx = context.Background()

var box, _ = secret.New(rand.Text(), "00112233445566778899aabb")

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(ctx, dir, box, drivers.DefaultAllowed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ptr(s string) *string { return &s }

var pg = Config{Code: "source_db", Driver: "postgres", URL: "postgres://db1:5432/shop", Username: "app", Password: ptr("s3cret")}

func TestSaveFindLoad(t *testing.T) {
	s := open(t, t.TempDir())
	var changed []string
	s.OnChange = func(code string) { changed = append(changed, code) }

	created, err := s.Save(ctx, pg)
	if err != nil || !created {
		t.Fatalf("created %v, %v", created, err)
	}
	v, err := s.Find(ctx, "source_db")
	if err != nil {
		t.Fatal(err)
	}
	if v.Driver != "postgres" || v.URL != pg.URL || v.Username != "app" || !v.PasswordSet || v.UpdatedAt.IsZero() {
		t.Errorf("view = %+v", v)
	}
	c, err := s.Load(ctx, "source_db")
	if err != nil || c.Password == nil || *c.Password != "s3cret" {
		t.Errorf("load = %+v, %v", c, err)
	}

	// The password is not in the file in clear text.
	var stored string
	s.db.QueryRow("SELECT password_encrypted FROM db_config").Scan(&stored) //nolint:errcheck
	if stored == "" || strings.Contains(stored, "s3cret") {
		t.Errorf("stored = %q", stored)
	}

	replaced := pg
	replaced.Password = nil
	if created, err := s.Save(ctx, replaced); err != nil || created {
		t.Errorf("replace: created %v, %v", created, err)
	}
	if v, _ := s.Find(ctx, "source_db"); v.PasswordSet {
		t.Error("the new settings have no password")
	}
	if len(changed) != 2 {
		t.Errorf("OnChange calls: %v", changed)
	}
}

func TestSettingsSurviveARestart(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(ctx, dir, box, drivers.DefaultAllowed)
	if err != nil {
		t.Fatal(err)
	}
	s.Save(ctx, pg) //nolint:errcheck
	s.Close()

	if c, err := open(t, dir).Load(ctx, "source_db"); err != nil || *c.Password != "s3cret" {
		t.Errorf("after reopening: %+v, %v", c, err)
	}

	other, _ := secret.New(rand.Text(), "00112233445566778899aabb")
	s2, _ := Open(ctx, dir, other, drivers.DefaultAllowed)
	defer s2.Close()
	if _, err := s2.Load(ctx, "source_db"); !errors.Is(err, secret.ErrWrongKey) {
		t.Errorf("another key: %v", err)
	}
}

func TestListAndDelete(t *testing.T) {
	s := open(t, t.TempDir())
	if list, _ := s.List(ctx); list == nil || len(list) != 0 {
		t.Errorf("an empty list must not be nil: %#v", list)
	}
	b := pg
	b.Code = "b"
	a := pg
	a.Code = "a"
	s.Save(ctx, b) //nolint:errcheck
	s.Save(ctx, a) //nolint:errcheck
	list, _ := s.List(ctx)
	if len(list) != 2 || list[0].Code != "a" {
		t.Errorf("list = %+v", list)
	}
	if err := s.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	var notFound *NotFoundError
	if err := s.Delete(ctx, "a"); !errors.As(err, &notFound) {
		t.Errorf("second delete: %v", err)
	}
	if _, err := s.Find(ctx, "a"); !errors.As(err, &notFound) || err.Error() != "No database configuration with code 'a'" {
		t.Errorf("find: %v", err)
	}
	if _, err := s.Load(ctx, "a"); !errors.As(err, &notFound) {
		t.Errorf("load: %v", err)
	}
}

func TestValidation(t *testing.T) {
	s := open(t, t.TempDir())
	tests := []struct {
		config Config
		field  string
		want   string
	}{
		{Config{Code: "", Driver: "postgres", URL: pg.URL}, "code", "must not be blank"},
		{Config{Code: "has space", Driver: "postgres", URL: pg.URL}, "code", "may contain letters"},
		{Config{Code: strings.Repeat("a", 101), Driver: "postgres", URL: pg.URL}, "code", "at most 100"},
		{Config{Code: "x", Driver: "db2", URL: pg.URL}, "driver", "must be one of postgres, mysql, mariadb, sqlserver, oracle"},
		{Config{Code: "x", Driver: "sqlite", URL: "file:/etc/passwd"}, "driver", "must be one of"},
		{Config{Code: "x", Driver: "postgres", URL: " "}, "url", "e.g. postgres://"},
		{Config{Code: "x", Driver: "postgres", URL: "jdbc:postgresql://db1/shop"}, "url", "looks like"},
		{Config{Code: "x", Driver: "mysql", URL: "mysql://root:pw@db/shop"}, "url", "own fields"},
		{Config{Code: "x", Driver: "postgres", URL: pg.URL, Password: ptr(strings.Repeat("p", 1001))}, "password", "at most 1000"},
	}
	for _, tt := range tests {
		_, err := s.Save(ctx, tt.config)
		var invalid *InvalidError
		if !errors.As(err, &invalid) || !strings.Contains(invalid.Fields[tt.field], tt.want) {
			t.Errorf("%+v: %v", tt.config, err)
		}
	}
}
