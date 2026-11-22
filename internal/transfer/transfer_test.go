// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package transfer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ergulcu/data-transporter/internal/configs"
	"github.com/ergulcu/data-transporter/internal/drivers"
	"github.com/ergulcu/data-transporter/internal/secret"
)

var ctx = context.Background()

type env struct {
	t              *testing.T
	transporter    *Transporter
	source, target *sql.DB
}

// newEnv saves two SQLite databases as "source" and "target"; the source has 25 people.
func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	box, _ := secret.New("pw", "0011223344556677")
	store, err := configs.Open(ctx, filepath.Join(dir, "config"), box, append(drivers.DefaultAllowed, "sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	tr := New(store, 4, slog.New(slog.DiscardHandler))
	t.Cleanup(tr.Close)

	e := &env{t: t, transporter: tr}
	for _, code := range []string{"source", "target"} {
		url := "file:" + filepath.ToSlash(filepath.Join(dir, code+".db"))
		if _, err := store.Save(ctx, configs.Config{Code: code, Driver: "sqlite", URL: url}); err != nil {
			t.Fatal(err)
		}
		db, _, err := tr.pool(ctx, code)
		if err != nil {
			t.Fatal(err)
		}
		if code == "source" {
			e.source = db
		} else {
			e.target = db
		}
	}
	e.exec(e.source, `CREATE TABLE person (id INTEGER PRIMARY KEY, full_name TEXT, salary NUMERIC, photo BLOB, note TEXT)`)
	for i := 1; i <= 25; i++ {
		e.exec(e.source, "INSERT INTO person VALUES (?, ?, ?, ?, ?)", i, fmt.Sprintf("Kişi %d", i), 1000.5+float64(i),
			[]byte{0xff, byte(i)}, nil)
	}
	e.exec(e.target, `CREATE TABLE people (person_id INTEGER PRIMARY KEY, name TEXT NOT NULL, salary NUMERIC, photo BLOB, note TEXT)`)
	return e
}

func (e *env) exec(db *sql.DB, query string, args ...any) {
	e.t.Helper()
	if _, err := db.ExecContext(ctx, query, args...); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) count() int {
	var n int
	e.target.QueryRow("SELECT count(*) FROM people").Scan(&n) //nolint:errcheck
	return n
}

func request(sourceQuery string, batch int, mode CommitMode) Request {
	return Request{SourceCode: "source", TargetCode: "target", SourceQuery: sourceQuery,
		TargetQuery: "INSERT INTO people (person_id, name, salary, photo, note) VALUES (:ID, :Full_Name, :salary, :photo, :note)",
		BatchSize:   batch, CommitMode: mode}
}

func TestMigrate(t *testing.T) {
	e := newEnv(t)
	var reported []int64
	res, err := e.transporter.Migrate(ctx, request("SELECT id, full_name, salary, photo, note FROM person", 10, CommitAll),
		func(rows int64) { reported = append(reported, rows) })
	if err != nil || !res.Success || res.TransferredRows != 25 || res.CommittedRows != 25 || res.Message != "Migration completed" {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if fmt.Sprint(reported) != "[10 20 25]" {
		t.Errorf("progress = %v", reported)
	}
	var name string
	var photo []byte
	var note sql.NullString
	e.target.QueryRow("SELECT name, photo, note FROM people WHERE person_id = 7").Scan(&name, &photo, &note) //nolint:errcheck
	if name != "Kişi 7" || len(photo) != 2 || photo[0] != 0xff || note.Valid {
		t.Errorf("row 7 = %q %x %v", name, photo, note)
	}
}

func TestCommitAllRollsBackEverything(t *testing.T) {
	e := newEnv(t)
	e.exec(e.target, "INSERT INTO people (person_id, name) VALUES (13, 'already there')")
	res, err := e.transporter.Migrate(ctx, request("SELECT * FROM person ORDER BY id", 5, CommitAll), nil)
	if err != nil || res.Success || res.TransferredRows != 12 || res.CommittedRows != 0 {
		t.Fatalf("result = %+v, %v", res, err)
	}
	if !strings.Contains(res.Message, "row 13") || !strings.Contains(res.Message, "UNIQUE") {
		t.Errorf("message = %q", res.Message)
	}
	if e.count() != 1 {
		t.Errorf("target has %d rows, want only the one that was there", e.count())
	}
}

func TestCommitBatchKeepsTheWrittenBatches(t *testing.T) {
	e := newEnv(t)
	e.exec(e.target, "INSERT INTO people (person_id, name) VALUES (13, 'already there')")
	res, _ := e.transporter.Migrate(ctx, request("SELECT * FROM person ORDER BY id", 5, CommitBatch), nil)
	if res.Success || res.TransferredRows != 12 || res.CommittedRows != 10 {
		t.Fatalf("result = %+v", res)
	}
	if e.count() != 11 {
		t.Errorf("target has %d rows, want 10 + 1", e.count())
	}
}

func TestAMissingColumnStopsBeforeWriting(t *testing.T) {
	e := newEnv(t)
	res, _ := e.transporter.Migrate(ctx, request("SELECT id, full_name FROM person", 5, CommitAll), nil)
	if res.Success || !strings.Contains(res.Message, "the source query has no column named 'salary'") || res.TransferredRows != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestBadQueriesAreReported(t *testing.T) {
	e := newEnv(t)
	res, _ := e.transporter.Migrate(ctx, request("SELECT * FROM nowhere", 5, CommitAll), nil)
	if res.Success || !strings.Contains(res.Message, "source query") {
		t.Errorf("result = %+v", res)
	}
	r := request("SELECT * FROM person", 5, CommitAll)
	r.TargetQuery = "INSERT INTO nowhere VALUES (:id)"
	res, _ = e.transporter.Migrate(ctx, r, nil)
	if res.Success || !strings.Contains(res.Message, "target query") {
		t.Errorf("result = %+v", res)
	}
}

func TestCancel(t *testing.T) {
	e := newEnv(t)
	cancelCtx, cancel := context.WithCancel(ctx)
	res, err := e.transporter.Migrate(cancelCtx, request("SELECT * FROM person ORDER BY id", 5, CommitAll), func(rows int64) {
		if rows >= 10 {
			cancel()
		}
	})
	if err != nil || res.Success || res.Message != "Migration was cancelled" || res.CommittedRows != 0 {
		t.Errorf("result = %+v, %v", res, err)
	}
	if e.count() != 0 {
		t.Errorf("target has %d rows after the cancel", e.count())
	}
}

func TestUnknownCodes(t *testing.T) {
	e := newEnv(t)
	r := request("SELECT 1", 5, CommitAll)
	r.TargetCode = "missing"
	var notFound *configs.NotFoundError
	if _, err := e.transporter.Migrate(ctx, r, nil); !errors.As(err, &notFound) || notFound.Code != "missing" {
		t.Errorf("err = %v", err)
	}
	if _, err := e.transporter.CheckConnection(ctx, "missing"); !errors.As(err, &notFound) {
		t.Errorf("check: %v", err)
	}
}

func TestCheckConnection(t *testing.T) {
	e := newEnv(t)
	c, err := e.transporter.CheckConnection(ctx, "source")
	if err != nil || !c.Success || !strings.HasPrefix(c.DatabaseProduct, "SQLite 3.") {
		t.Errorf("check = %+v, %v", c, err)
	}
}

func TestAPoolIsClosedWhenItsSettingsChange(t *testing.T) {
	e := newEnv(t)
	e.transporter.Evict("source")
	if err := e.source.Ping(); err == nil {
		t.Error("the old pool must be closed")
	}
	if _, _, err := e.transporter.pool(ctx, "source"); err != nil {
		t.Errorf("a new pool must be opened: %v", err)
	}
}

func TestValidate(t *testing.T) {
	r := Request{SourceCode: "a", TargetCode: "b", SourceQuery: "SELECT 1", TargetQuery: "INSERT"}
	if errs := r.Validate(); len(errs) != 0 || r.BatchSize != DefaultBatchSize || r.CommitMode != CommitAll {
		t.Errorf("defaults: %+v %v", r, errs)
	}
	bad := Request{SourceCode: " ", BatchSize: 100_001, CommitMode: "SOMETIMES"}
	errs := bad.Validate()
	for _, f := range []string{"sourceCode", "targetCode", "sourceQuery", "targetQuery", "batchSize", "commitMode"} {
		if errs[f] == "" {
			t.Errorf("%s must be reported: %v", f, errs)
		}
	}
	if errs := (&Request{BatchSize: -1}).Validate(); errs["batchSize"] == "" {
		t.Error("a negative batch size must be reported")
	}
}
