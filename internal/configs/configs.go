// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Package configs keeps the saved connection settings in a SQLite file, so they survive a restart.
// Passwords are stored encrypted and never handed out except to open connections.
package configs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ergulcu/data-transporter/internal/drivers"
	"github.com/ergulcu/data-transporter/internal/secret"
)

// Config is what the API accepts. The password is only ever accepted, never returned (see View).
type Config struct {
	Code     string  `json:"code"`
	Driver   string  `json:"driver"`
	URL      string  `json:"url"`
	Username string  `json:"username"`
	Password *string `json:"password"`
}

// View is what the API returns about saved settings: everything except the password.
type View struct {
	Code        string    `json:"code"`
	Driver      string    `json:"driver"`
	URL         string    `json:"url"`
	Username    string    `json:"username"`
	PasswordSet bool      `json:"passwordSet"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// NotFoundError is returned for an unknown code.
type NotFoundError struct{ Code string }

func (e *NotFoundError) Error() string {
	return fmt.Sprintf("No database configuration with code '%s'", e.Code)
}

// InvalidError lists what is wrong with a Config, field by field.
type InvalidError struct{ Fields map[string]string }

func (e *InvalidError) Error() string {
	parts := make([]string, 0, len(e.Fields))
	for _, f := range slices.Sorted(maps.Keys(e.Fields)) {
		parts = append(parts, f+": "+e.Fields[f])
	}
	return strings.Join(parts, "; ")
}

// Store is safe for concurrent use.
type Store struct {
	db      *sql.DB
	box     *secret.Box
	allowed []string
	now     func() time.Time
	// OnChange is called with the code of settings that were replaced or deleted, to close their pool.
	OnChange func(code string)
}

// Open opens or creates dir/datatransporter.db.
func Open(ctx context.Context, dir string, box *secret.Box, allowed []string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "datatransporter.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)")
	if err != nil {
		return nil, err
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS db_config (
		code               TEXT NOT NULL PRIMARY KEY,
		driver             TEXT NOT NULL,
		url                TEXT NOT NULL,
		username           TEXT NOT NULL DEFAULT '',
		password_encrypted TEXT,
		updated_at         TEXT NOT NULL)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return &Store{db: db, box: box, allowed: allowed, now: time.Now, OnChange: func(string) {}}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

var codePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// Validate checks a Config before it is saved.
func (s *Store) Validate(c Config) error {
	errs := map[string]string{}
	switch {
	case strings.TrimSpace(c.Code) == "":
		errs["code"] = "must not be blank"
	case utf8.RuneCountInString(c.Code) > 100:
		errs["code"] = "must be at most 100 characters"
	case !codePattern.MatchString(c.Code):
		errs["code"] = "may contain letters, digits, '_', '.' and '-'"
	}
	d, known := drivers.Lookup(c.Driver)
	switch {
	case !known || !slices.Contains(s.allowed, c.Driver):
		errs["driver"] = "must be one of " + strings.Join(s.allowed, ", ")
	case strings.TrimSpace(c.URL) == "":
		errs["url"] = "must not be blank; e.g. " + d.Example
	case len(c.URL) > 2000:
		errs["url"] = "must be at most 2000 characters"
	default:
		if err := d.CheckURL(c.URL); err != nil {
			errs["url"] = err.Error()
		}
	}
	if utf8.RuneCountInString(c.Username) > 200 {
		errs["username"] = "must be at most 200 characters"
	}
	if c.Password != nil && utf8.RuneCountInString(*c.Password) > 1000 {
		errs["password"] = "must be at most 1000 characters"
	}
	if len(errs) > 0 {
		return &InvalidError{Fields: errs}
	}
	return nil
}

// Save creates or replaces the settings with the same code. It returns true when they are new.
func (s *Store) Save(ctx context.Context, c Config) (created bool, err error) {
	if err := s.Validate(c); err != nil {
		return false, err
	}
	var encrypted *string
	if c.Password != nil {
		e := s.box.Encrypt(*c.Password)
		encrypted = &e
	}
	now := s.now().UTC().Format(time.RFC3339Nano)
	var existed bool
	if err := s.db.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM db_config WHERE code = ?)", c.Code).Scan(&existed); err != nil {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO db_config (code, driver, url, username, password_encrypted, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (code) DO UPDATE SET driver = excluded.driver, url = excluded.url, username = excluded.username,
			password_encrypted = excluded.password_encrypted, updated_at = excluded.updated_at`,
		c.Code, c.Driver, c.URL, c.Username, encrypted, now)
	if err != nil {
		return false, err
	}
	s.OnChange(c.Code)
	return !existed, nil
}

// Find returns the settings without the password.
func (s *Store) Find(ctx context.Context, code string) (View, error) {
	v, err := scanView(s.db.QueryRowContext(ctx, "SELECT "+viewColumns+" FROM db_config WHERE code = ?", code))
	if errors.Is(err, sql.ErrNoRows) {
		return v, &NotFoundError{Code: code}
	}
	return v, err
}

// List returns all settings ordered by code.
func (s *Store) List(ctx context.Context) ([]View, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+viewColumns+" FROM db_config ORDER BY code")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	views := []View{}
	for rows.Next() {
		v, err := scanView(rows)
		if err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, rows.Err()
}

// Delete removes the settings; an unknown code is a *NotFoundError.
func (s *Store) Delete(ctx context.Context, code string) error {
	res, err := s.db.ExecContext(ctx, "DELETE FROM db_config WHERE code = ?", code)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return &NotFoundError{Code: code}
	}
	s.OnChange(code)
	return nil
}

// Load returns the full settings with the decrypted password, for opening connections.
func (s *Store) Load(ctx context.Context, code string) (Config, error) {
	var c Config
	var encrypted sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT code, driver, url, username, password_encrypted FROM db_config WHERE code = ?", code).
		Scan(&c.Code, &c.Driver, &c.URL, &c.Username, &encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return c, &NotFoundError{Code: code}
	}
	if err != nil {
		return c, err
	}
	if encrypted.Valid {
		plain, err := s.box.Decrypt(encrypted.String)
		if err != nil {
			return c, err
		}
		c.Password = &plain
	}
	return c, nil
}

const viewColumns = "code, driver, url, username, password_encrypted IS NOT NULL, updated_at"

type scanner interface{ Scan(dest ...any) error }

func scanView(row scanner) (View, error) {
	var v View
	var updated string
	if err := row.Scan(&v.Code, &v.Driver, &v.URL, &v.Username, &v.PasswordSet, &updated); err != nil {
		return v, err
	}
	v.UpdatedAt, _ = time.Parse(time.RFC3339Nano, updated)
	return v, nil
}
