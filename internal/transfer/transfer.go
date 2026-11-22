// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Package transfer streams the rows of a query on one database into a parameterized insert or update on another.
//
//   - The source rows are read as they come; the whole result is never held in memory.
//   - The target is written in a transaction. With CommitAll a failure rolls back every row; with CommitBatch
//     each full batch is committed as it is written, for transfers too large for one transaction.
//   - The named parameters of the target query are matched to the source columns ignoring case, because
//     Oracle reports column names in upper case and PostgreSQL in lower case.
package transfer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/ergulcu/data-transporter/internal/configs"
	"github.com/ergulcu/data-transporter/internal/drivers"
	"github.com/ergulcu/data-transporter/internal/named"
)

// CommitMode decides when the target commits.
type CommitMode string

const (
	// CommitAll writes all rows in one transaction: either every row arrives or none does.
	CommitAll CommitMode = "ALL"
	// CommitBatch commits each batch on its own; a failure keeps the batches written before it.
	CommitBatch CommitMode = "BATCH"
)

// DefaultBatchSize is used when a request has none.
const DefaultBatchSize = 1000

// Request describes one migration.
type Request struct {
	SourceCode  string `json:"sourceCode"`
	TargetCode  string `json:"targetCode"`
	SourceQuery string `json:"sourceQuery"`
	// TargetQuery has named parameters, e.g. INSERT INTO t (a, b) VALUES (:a, :b).
	TargetQuery string     `json:"targetQuery"`
	BatchSize   int        `json:"batchSize"`
	CommitMode  CommitMode `json:"commitMode"`
}

// Validate fills the defaults and lists what is wrong.
func (r *Request) Validate() map[string]string {
	errs := map[string]string{}
	for field, value := range map[string]string{"sourceCode": r.SourceCode, "targetCode": r.TargetCode,
		"sourceQuery": r.SourceQuery, "targetQuery": r.TargetQuery} {
		if strings.TrimSpace(value) == "" {
			errs[field] = "must not be blank"
		}
	}
	if r.BatchSize == 0 {
		r.BatchSize = DefaultBatchSize
	}
	if r.BatchSize < 1 || r.BatchSize > 100_000 {
		errs["batchSize"] = "must be between 1 and 100000"
	}
	if r.CommitMode == "" {
		r.CommitMode = CommitAll
	}
	if r.CommitMode != CommitAll && r.CommitMode != CommitBatch {
		errs["commitMode"] = "must be ALL or BATCH"
	}
	return errs
}

// Result of a migration.
type Result struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	// TransferredRows were written to the target; with CommitAll a failed run leaves none of them.
	TransferredRows int64 `json:"transferredRows"`
	// CommittedRows stay in the target after the run.
	CommittedRows  int64 `json:"committedRows"`
	DurationMillis int64 `json:"durationMillis"`
}

// Check is the answer to a connection test.
type Check struct {
	Success         bool   `json:"success"`
	Message         string `json:"message"`
	DatabaseProduct string `json:"databaseProduct,omitempty"`
}

// ErrCancelled is the message of a cancelled migration.
var ErrCancelled = errors.New("Migration was cancelled") //nolint:staticcheck // shown to users as is

// Transporter keeps one connection pool per saved configuration, created on first use. A pool is closed
// when its settings change or are deleted, and all of them on Close.
type Transporter struct {
	configs  *configs.Store
	log      *slog.Logger
	maxConns int

	mu    sync.Mutex
	pools map[string]*sql.DB
}

func New(c *configs.Store, maxConns int, log *slog.Logger) *Transporter {
	t := &Transporter{configs: c, log: log, maxConns: maxConns, pools: map[string]*sql.DB{}}
	c.OnChange = t.Evict
	return t
}

// Evict closes the pool of the code.
func (t *Transporter) Evict(code string) {
	t.mu.Lock()
	db := t.pools[code]
	delete(t.pools, code)
	t.mu.Unlock()
	if db != nil {
		db.Close()
	}
}

// Close closes every pool.
func (t *Transporter) Close() {
	t.mu.Lock()
	codes := make([]string, 0, len(t.pools))
	for code := range t.pools {
		codes = append(codes, code)
	}
	t.mu.Unlock()
	for _, code := range codes {
		t.Evict(code)
	}
}

// pool returns the pool and driver of a saved configuration; an unknown code is a *configs.NotFoundError.
func (t *Transporter) pool(ctx context.Context, code string) (*sql.DB, *drivers.Driver, error) {
	c, err := t.configs.Load(ctx, code)
	if err != nil {
		return nil, nil, err
	}
	d, ok := drivers.Lookup(c.Driver)
	if !ok {
		return nil, nil, fmt.Errorf("unknown driver %q", c.Driver)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if db, ok := t.pools[code]; ok {
		return db, d, nil
	}
	password := ""
	if c.Password != nil {
		password = *c.Password
	}
	db, err := d.Open(c.URL, c.Username, password)
	if err != nil {
		return nil, nil, err
	}
	db.SetMaxOpenConns(t.maxConns)
	db.SetMaxIdleConns(t.maxConns)
	db.SetConnMaxIdleTime(5 * time.Minute)
	t.pools[code] = db
	return db, d, nil
}

// CheckConnection opens a connection and asks for the database version.
func (t *Transporter) CheckConnection(ctx context.Context, code string) (Check, error) {
	db, d, err := t.pool(ctx, code)
	if err != nil {
		var notFound *configs.NotFoundError
		if errors.As(err, &notFound) {
			return Check{}, err
		}
		return Check{Success: false, Message: err.Error()}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return Check{Success: false, Message: err.Error()}, nil
	}
	var version string
	if err := db.QueryRowContext(ctx, d.Version).Scan(&version); err != nil {
		version = d.Name // e.g. no right to read v$version on Oracle
	}
	version, _, _ = strings.Cut(strings.TrimSpace(version), "\n")
	return Check{Success: true, Message: "Connected", DatabaseProduct: version}, nil
}

// Migrate runs the request; progress, which may be nil, is told the number of rows written after every batch.
// Unknown codes are returned as a *configs.NotFoundError; everything else that goes wrong is in the Result.
func (t *Transporter) Migrate(ctx context.Context, req Request, progress func(rows int64)) (Result, error) {
	if progress == nil {
		progress = func(int64) {}
	}
	start := time.Now()
	source, _, err := t.pool(ctx, req.SourceCode)
	if err != nil {
		return t.failure(err, start, 0, 0)
	}
	target, targetDriver, err := t.pool(ctx, req.TargetCode)
	if err != nil {
		return t.failure(err, start, 0, 0)
	}
	query, params, err := named.Compile(req.TargetQuery, targetDriver.Placeholder)
	if err != nil {
		return t.failure(err, start, 0, 0)
	}

	c := &copier{req: req, target: target, query: query, params: params, progress: progress}
	err = c.run(ctx, source)
	if err != nil {
		t.log.Warn("migration failed", "source", req.SourceCode, "target", req.TargetCode, "rows", c.written, "err", err)
		return t.failure(err, start, c.written, c.committed)
	}
	millis := time.Since(start).Milliseconds()
	t.log.Info("migration completed", "source", req.SourceCode, "target", req.TargetCode, "rows", c.written, "ms", millis)
	return Result{Success: true, Message: "Migration completed", TransferredRows: c.written, CommittedRows: c.committed,
		DurationMillis: millis}, nil
}

func (t *Transporter) failure(err error, start time.Time, written, committed int64) (Result, error) {
	var notFound *configs.NotFoundError
	if errors.As(err, &notFound) {
		return Result{}, err
	}
	message := "Migration failed: " + err.Error()
	if errors.Is(err, context.Canceled) || errors.Is(err, ErrCancelled) {
		message = ErrCancelled.Error()
	}
	return Result{Success: false, Message: message, TransferredRows: written, CommittedRows: committed,
		DurationMillis: time.Since(start).Milliseconds()}, nil
}

type copier struct {
	req      Request
	target   *sql.DB
	query    string
	params   []string
	progress func(int64)

	tx                 *sql.Tx
	stmt               *sql.Stmt
	written, committed int64
}

func (c *copier) run(ctx context.Context, source *sql.DB) (err error) {
	rows, err := source.QueryContext(ctx, c.req.SourceQuery)
	if err != nil {
		return fmt.Errorf("source query: %w", err)
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return err
	}

	// Find the source column of every parameter before anything is written.
	columns := map[string]int{}
	for i, ct := range types {
		columns[strings.ToLower(ct.Name())] = i
	}
	index := make([]int, len(c.params))
	for i, p := range c.params {
		col, ok := columns[strings.ToLower(p)]
		if !ok {
			return fmt.Errorf("the source query has no column named '%s'", p)
		}
		index[i] = col
	}
	binary := make([]bool, len(types))
	for i, ct := range types {
		binary[i] = drivers.IsBinary(ct.DatabaseTypeName())
	}

	if err := c.begin(ctx); err != nil {
		return err
	}
	defer func() {
		if err != nil && c.tx != nil {
			c.tx.Rollback() //nolint:errcheck
		}
	}()

	values := make([]any, len(types))
	pointers := make([]any, len(types))
	for i := range values {
		pointers[i] = &values[i]
	}
	args := make([]any, len(c.params))
	inBatch := 0
	for rows.Next() {
		if ctx.Err() != nil {
			return ErrCancelled
		}
		if err := rows.Scan(pointers...); err != nil {
			return err
		}
		for i, col := range index {
			v := values[col]
			// Drivers hand text as []byte too; the target would take it for binary data.
			if b, ok := v.([]byte); ok && !binary[col] {
				v = string(b)
			}
			args[i] = v
		}
		if _, err := c.stmt.ExecContext(ctx, args...); err != nil {
			return fmt.Errorf("row %d: %w", c.written+1, err)
		}
		c.written++
		inBatch++
		if inBatch == c.req.BatchSize {
			inBatch = 0
			if c.req.CommitMode == CommitBatch {
				if err := c.commit(); err != nil {
					return err
				}
				if err := c.begin(ctx); err != nil {
					return err
				}
			}
			c.progress(c.written)
		}
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return ErrCancelled
		}
		return fmt.Errorf("source query: %w", err)
	}
	if err := c.commit(); err != nil {
		return err
	}
	c.progress(c.written)
	return nil
}

func (c *copier) begin(ctx context.Context) error {
	tx, err := c.target.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, c.query)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return fmt.Errorf("target query: %w", err)
	}
	c.tx, c.stmt = tx, stmt
	return nil
}

func (c *copier) commit() error {
	c.stmt.Close()
	err := c.tx.Commit()
	c.tx = nil
	if err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	c.committed = c.written
	return nil
}
