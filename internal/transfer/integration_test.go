// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// These tests move data between real databases started with Testcontainers: go test -tags integration ./...
// The SQL Server and Oracle images are large; the first run takes a while to pull them.
package transfer

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"math"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcexec "github.com/testcontainers/testcontainers-go/exec"
	"github.com/testcontainers/testcontainers-go/modules/mssql"
	tcmysql "github.com/testcontainers/testcontainers-go/modules/mysql"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/ergulcu/data-transporter/internal/configs"
	"github.com/ergulcu/data-transporter/internal/drivers"
	"github.com/ergulcu/data-transporter/internal/secret"
)

// database is a started server, saved as a configuration.
type database struct {
	config configs.Config
	// create makes the person table; insert is a target query with named parameters.
	create, insert string
}

func ptr(s string) *string { return &s }

// The passwords of the test databases are new on every run. SQL Server demands upper case, lower case, a digit
// and a symbol; Oracle is given letters and digits only, so that the shell scripts of its image cope.
func testPassword() string      { return "Aa1!" + rand.Text() }
func testPasswordPlain() string { return "Pw" + rand.Text() }

func startPostgres(ctx context.Context) (database, testcontainers.Container, error) {
	pw := testPasswordPlain()
	c, err := tcpostgres.Run(ctx, "postgres:18-alpine", tcpostgres.WithDatabase("shop"),
		tcpostgres.WithUsername("app"), tcpostgres.WithPassword(pw), tcpostgres.BasicWaitStrategies())
	if err != nil {
		return database{}, c, err
	}
	host, port, err := address(ctx, c, "5432/tcp")
	return database{
		config: configs.Config{Code: "pg", Driver: "postgres", URL: fmt.Sprintf("postgres://%s:%d/shop?sslmode=disable", host, port),
			Username: "app", Password: ptr(pw)},
		create: `CREATE TABLE person (id INT PRIMARY KEY, full_name VARCHAR(100) NOT NULL, salary NUMERIC(12,2),
			born TIMESTAMP, photo BYTEA, note VARCHAR(100))`,
		insert: "INSERT INTO person VALUES (:id, :full_name, :salary, :born, :photo, :note)",
	}, c, err
}

func startMySQL(ctx context.Context) (database, testcontainers.Container, error) {
	pw := testPasswordPlain()
	c, err := tcmysql.Run(ctx, "mysql:8.4", tcmysql.WithDatabase("shop"), tcmysql.WithUsername("app"), tcmysql.WithPassword(pw))
	if err != nil {
		return database{}, c, err
	}
	host, port, err := address(ctx, c, "3306/tcp")
	return database{
		config: configs.Config{Code: "my", Driver: "mysql", URL: fmt.Sprintf("mysql://%s:%d/shop", host, port),
			Username: "app", Password: ptr(pw)},
		create: `CREATE TABLE person (id INT PRIMARY KEY, full_name VARCHAR(100) NOT NULL, salary DECIMAL(12,2),
			born DATETIME, photo VARBINARY(100), note VARCHAR(100))`,
		insert: "INSERT INTO person VALUES (:id, :full_name, :salary, :born, :photo, :note)",
	}, c, err
}

func startSQLServer(ctx context.Context) (database, testcontainers.Container, error) {
	pw := testPassword()
	c, err := mssql.Run(ctx, "mcr.microsoft.com/mssql/server:2022-latest", mssql.WithAcceptEULA(), mssql.WithPassword(pw))
	if err != nil {
		return database{}, c, err
	}
	host, port, err := address(ctx, c, "1433/tcp")
	return database{
		config: configs.Config{Code: "ms", Driver: "sqlserver", URL: fmt.Sprintf("sqlserver://%s:%d?database=master", host, port),
			Username: "sa", Password: ptr(pw)},
		create: `CREATE TABLE person (id INT PRIMARY KEY, full_name NVARCHAR(100) NOT NULL, salary DECIMAL(12,2),
			born DATETIME2, photo VARBINARY(100), note NVARCHAR(100))`,
		insert: "INSERT INTO person VALUES (:id, :full_name, :salary, :born, :photo, :note)",
	}, c, err
}

// oracleSetup opens the pluggable database and creates the test user in it. The image can do the latter
// (APP_USER), but on some machines it tries before the pluggable database is open (ORA-01109) and exits.
func oracleSetup(appPassword string) string {
	return fmt.Sprintf(`WHENEVER SQLERROR EXIT FAILURE
DECLARE
  open_mode VARCHAR2(20);
BEGIN
  SELECT open_mode INTO open_mode FROM v$pdbs WHERE name = 'FREEPDB1';
  IF open_mode <> 'READ WRITE' THEN
    EXECUTE IMMEDIATE 'ALTER PLUGGABLE DATABASE FREEPDB1 OPEN READ WRITE FORCE';
  END IF;
END;
/
ALTER SESSION SET CONTAINER = FREEPDB1;
CREATE USER app IDENTIFIED BY "%s" DEFAULT TABLESPACE USERS QUOTA UNLIMITED ON USERS;
GRANT CREATE SESSION, CREATE TABLE TO app;
EXIT
`, appPassword)
}

func startOracle(ctx context.Context) (database, testcontainers.Container, error) {
	pw := testPasswordPlain()
	c, err := testcontainers.Run(ctx, "gvenzl/oracle-free:23-slim-faststart",
		testcontainers.WithExposedPorts("1521/tcp"),
		testcontainers.WithEnv(map[string]string{"ORACLE_PASSWORD": testPasswordPlain()}),
		testcontainers.WithFiles(testcontainers.ContainerFile{
			Reader: strings.NewReader(oracleSetup(pw)), ContainerFilePath: "/tmp/setup.sql", FileMode: 0o644}),
		testcontainers.WithWaitStrategyAndDeadline(10*time.Minute, wait.ForLog("DATABASE IS READY TO USE!").WithStartupTimeout(10*time.Minute)))
	if err != nil {
		return database{}, c, err
	}
	code, out, err := c.Exec(ctx, []string{"bash", "-c", "sqlplus -s / as sysdba @/tmp/setup.sql"}, tcexec.Multiplexed())
	if err == nil && code != 0 {
		msg, _ := io.ReadAll(out)
		err = fmt.Errorf("oracle setup exited with %d: %s", code, msg)
	}
	if err != nil {
		return database{}, c, err
	}
	host, port, err := address(ctx, c, "1521/tcp")
	return database{
		config: configs.Config{Code: "ora", Driver: "oracle", URL: fmt.Sprintf("oracle://%s:%d/FREEPDB1", host, port),
			Username: "app", Password: ptr(pw)},
		create: `CREATE TABLE person (id NUMBER(10) PRIMARY KEY, full_name VARCHAR2(100 CHAR) NOT NULL, salary NUMBER(12,2),
			born TIMESTAMP, photo RAW(100), note VARCHAR2(100 CHAR))`,
		// Oracle reports the column names in upper case; they are matched ignoring case.
		insert: "INSERT INTO person VALUES (:id, :full_name, :salary, :born, :photo, :note)",
	}, c, err
}

func address(ctx context.Context, c testcontainers.Container, port string) (string, int, error) {
	host, err := c.Host(ctx)
	if err != nil {
		return "", 0, err
	}
	p, err := c.MappedPort(ctx, port)
	if err != nil {
		return "", 0, err
	}
	return host, int(p.Num()), nil
}

// start starts the databases at the same time.
func start(t *testing.T, starters ...func(context.Context) (database, testcontainers.Container, error)) []database {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	dbs := make([]database, len(starters))
	errs := make([]error, len(starters))
	var wg sync.WaitGroup
	for i, s := range starters {
		wg.Go(func() {
			var c testcontainers.Container
			dbs[i], c, errs[i] = s(ctx)
			if c != nil {
				t.Cleanup(func() { _ = testcontainers.TerminateContainer(c) })
			}
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	return dbs
}

func newTransporter(t *testing.T, dbs []database) (*Transporter, map[string]*sql.DB) {
	t.Helper()
	box, _ := secret.New("pw", "0011223344556677")
	store, err := configs.Open(ctx, filepath.Join(t.TempDir(), "config"), box, drivers.DefaultAllowed)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	tr := New(store, 4, slog.New(slog.DiscardHandler))
	t.Cleanup(tr.Close)
	pools := map[string]*sql.DB{}
	for _, d := range dbs {
		if _, err := store.Save(ctx, d.config); err != nil {
			t.Fatal(err)
		}
		check, err := tr.CheckConnection(ctx, d.config.Code)
		if err != nil || !check.Success {
			t.Fatalf("%s: %+v %v", d.config.Code, check, err)
		}
		t.Logf("%s: %s", d.config.Code, check.DatabaseProduct)
		db, _, err := tr.pool(ctx, d.config.Code)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx, d.create); err != nil {
			t.Fatalf("%s: %v", d.config.Code, err)
		}
		pools[d.config.Code] = db
	}
	return tr, pools
}

type person struct {
	id     int
	name   string
	salary float64
	born   time.Time
	photo  []byte
	note   sql.NullString
}

func people() []person {
	var ps []person
	for i := 1; i <= 120; i++ {
		p := person{id: i, name: fmt.Sprintf("Çağrı Işıklı Öztürk %d", i), salary: 1000 + float64(i)/4,
			born: time.Date(1980+i%30, time.Month(1+i%12), 1+i%28, i%24, i%60, 0, 0, time.UTC), photo: []byte{0xff, 0x00, byte(i)}}
		if i%3 == 0 {
			p.note = sql.NullString{String: "üç", Valid: true}
		}
		ps = append(ps, p)
	}
	return ps
}

// TestARoundTripThroughEveryDatabase moves the same rows PostgreSQL → MySQL → SQL Server → Oracle → PostgreSQL
// and checks that they come back unchanged: text with Turkish letters, decimals, timestamps, bytes and nulls.
func TestARoundTripThroughEveryDatabase(t *testing.T) {
	dbs := start(t, startPostgres, startMySQL, startSQLServer, startOracle)
	tr, pools := newTransporter(t, dbs)

	pg := pools["pg"]
	for _, p := range people() {
		var note any
		if p.note.Valid {
			note = p.note.String
		}
		if _, err := pg.ExecContext(ctx, "INSERT INTO person VALUES ($1, $2, $3, $4, $5, $6)",
			p.id, p.name, p.salary, p.born, p.photo, note); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pg.ExecContext(ctx, "CREATE TABLE person_back AS SELECT * FROM person WITH NO DATA"); err != nil {
		t.Fatal(err)
	}

	hops := []struct{ from, to, insert string }{
		{"pg", "my", dbs[1].insert},
		{"my", "ms", dbs[2].insert},
		{"ms", "ora", dbs[3].insert},
		{"ora", "pg", "INSERT INTO person_back VALUES (:id, :full_name, :salary, :born, :photo, :note)"},
	}
	for _, h := range hops {
		res, err := tr.Migrate(ctx, Request{SourceCode: h.from, TargetCode: h.to,
			SourceQuery: "SELECT id, full_name, salary, born, photo, note FROM person", TargetQuery: h.insert,
			BatchSize: 50, CommitMode: CommitBatch}, nil)
		if err != nil || !res.Success || res.TransferredRows != 120 || res.CommittedRows != 120 {
			t.Fatalf("%s -> %s: %+v %v", h.from, h.to, res, err)
		}
	}

	rows, err := pg.QueryContext(ctx, "SELECT id, full_name, salary, born, photo, note FROM person_back ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	want := people()
	i := 0
	for rows.Next() {
		var got person
		var salary string
		if err := rows.Scan(&got.id, &got.name, &salary, &got.born, &got.photo, &got.note); err != nil {
			t.Fatal(err)
		}
		got.salary, _ = strconv.ParseFloat(salary, 64)
		w := want[i]
		if got.id != w.id || got.name != w.name || math.Abs(got.salary-w.salary) > 0.001 || !got.born.Equal(w.born) ||
			!bytes.Equal(got.photo, w.photo) || got.note != w.note {
			t.Errorf("row %d:\n got %+v\nwant %+v", w.id, got, w)
		}
		i++
	}
	if i != len(want) {
		t.Errorf("%d rows came back, want %d", i, len(want))
	}
}

// TestCommitAllRollsBackOnARealDatabase checks the transaction handling against PostgreSQL itself.
func TestCommitAllRollsBackOnARealDatabase(t *testing.T) {
	dbs := start(t, startPostgres)
	tr, pools := newTransporter(t, dbs)
	pg := pools["pg"]
	if _, err := pg.ExecContext(ctx, `INSERT INTO person (id, full_name) SELECT n, 'p' || n FROM generate_series(1, 30) n;
		CREATE TABLE copy (id INT PRIMARY KEY, full_name VARCHAR(100)); INSERT INTO copy VALUES (25, 'taken')`); err != nil {
		t.Fatal(err)
	}
	r := Request{SourceCode: "pg", TargetCode: "pg", SourceQuery: "SELECT id, full_name FROM person ORDER BY id",
		TargetQuery: "INSERT INTO copy VALUES (:id, :full_name)", BatchSize: 10, CommitMode: CommitAll}
	res, _ := tr.Migrate(ctx, r, nil)
	var n int
	pg.QueryRowContext(ctx, "SELECT count(*) FROM copy").Scan(&n) //nolint:errcheck
	if res.Success || res.CommittedRows != 0 || n != 1 {
		t.Errorf("ALL: %+v, %d rows in copy", res, n)
	}

	r.CommitMode = CommitBatch
	res, _ = tr.Migrate(ctx, r, nil)
	pg.QueryRowContext(ctx, "SELECT count(*) FROM copy").Scan(&n) //nolint:errcheck
	if res.Success || res.CommittedRows != 20 || n != 21 {
		t.Errorf("BATCH: %+v, %d rows in copy", res, n)
	}
}
