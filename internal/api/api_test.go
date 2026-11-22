// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ergulcu/data-transporter/internal/configs"
	"github.com/ergulcu/data-transporter/internal/drivers"
	"github.com/ergulcu/data-transporter/internal/jobs"
	"github.com/ergulcu/data-transporter/internal/secret"
	"github.com/ergulcu/data-transporter/internal/transfer"

	_ "modernc.org/sqlite"
)

type env struct {
	t   *testing.T
	srv *httptest.Server
	dir string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	box, _ := secret.New("pw", "0011223344556677")
	store, err := configs.Open(context.Background(), filepath.Join(dir, "config"), box, append(drivers.DefaultAllowed, "sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	tr := transfer.New(store, 4, log)
	ctx, cancel := context.WithCancel(context.Background())
	j := jobs.New(ctx, tr)
	srv := httptest.NewServer(New(store, tr, j, Credentials{User: "admin", Password: "secret"}, log))
	t.Cleanup(func() {
		srv.Close()
		cancel()
		j.Wait()
		tr.Close()
		store.Close()
	})
	return &env{t: t, srv: srv, dir: dir}
}

func (e *env) call(method, path, body string) (*http.Response, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	req.SetBasicAuth("admin", "secret")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// sqliteConfig saves a SQLite database file under the code and returns it opened.
func (e *env) sqliteConfig(code string) *sql.DB {
	e.t.Helper()
	path := filepath.ToSlash(filepath.Join(e.dir, code+".db"))
	if resp, body := e.call("POST", "/db/config", fmt.Sprintf(`{"code": %q, "driver": "sqlite", "url": "file:%s"}`, code, path)); resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("save %s: %d %s", code, resp.StatusCode, body)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { db.Close() })
	return db
}

func TestCredentialsAreRequired(t *testing.T) {
	e := newEnv(t)
	for _, auth := range []func(*http.Request){
		func(*http.Request) {},
		func(r *http.Request) { r.SetBasicAuth("admin", "wrong") },
		func(r *http.Request) { r.SetBasicAuth("root", "secret") },
	} {
		req, _ := http.NewRequest("GET", e.srv.URL+"/db/configs", nil)
		auth(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || !strings.HasPrefix(resp.Header.Get("WWW-Authenticate"), "Basic") {
			t.Errorf("status %d, %v", resp.StatusCode, resp.Header)
		}
	}
	resp, err := http.Get(e.srv.URL + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("the health check needs no credentials: %v %v", resp, err)
	}
}

func TestConfigs(t *testing.T) {
	e := newEnv(t)
	cfg := `{"code": "dev_pg", "driver": "postgres", "url": "postgres://db1:5432/shop", "username": "app", "password": "s3cret"}`
	resp, body := e.call("POST", "/db/config", cfg)
	if resp.StatusCode != http.StatusCreated || resp.Header.Get("Location") != "/db/config/dev_pg" {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(body, "s3cret") || !strings.Contains(body, `"passwordSet":true`) {
		t.Errorf("the password must never be returned: %s", body)
	}
	if resp, _ := e.call("POST", "/db/config", cfg); resp.StatusCode != http.StatusOK {
		t.Errorf("replace: %d", resp.StatusCode)
	}
	_, body = e.call("GET", "/db/config/dev_pg", "")
	if !strings.Contains(body, `"username":"app"`) || strings.Contains(body, "s3cret") {
		t.Errorf("get: %s", body)
	}
	_, body = e.call("GET", "/db/configs", "")
	var all map[string]configs.View
	if err := json.Unmarshal([]byte(body), &all); err != nil || all["dev_pg"].Driver != "postgres" {
		t.Errorf("list: %s", body)
	}
	if resp, _ := e.call("DELETE", "/db/config/dev_pg", ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("delete: %d", resp.StatusCode)
	}
	resp, body = e.call("GET", "/db/config/dev_pg", "")
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "No database configuration with code 'dev_pg'") {
		t.Errorf("after delete: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.call("DELETE", "/db/config/dev_pg", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("second delete: %d", resp.StatusCode)
	}
}

func TestInvalidConfigs(t *testing.T) {
	e := newEnv(t)
	resp, body := e.call("POST", "/db/config", `{"code": "a b", "driver": "db2", "url": "jdbc:db2://x"}`)
	var p Problem
	json.Unmarshal([]byte(body), &p) //nolint:errcheck
	if resp.StatusCode != http.StatusBadRequest || len(p.Errors) != 2 || p.Errors[0].Field != "code" || p.Errors[1].Field != "driver" {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	if resp, _ := e.call("POST", "/db/config", `{"code": `); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("broken JSON: %d", resp.StatusCode)
	}
}

func TestMigrate(t *testing.T) {
	e := newEnv(t)
	src, dst := e.sqliteConfig("src"), e.sqliteConfig("dst")
	src.Exec("CREATE TABLE person (id INTEGER, full_name TEXT)")               //nolint:errcheck
	src.Exec("INSERT INTO person VALUES (1, 'Ali'), (2, 'Veli'), (3, 'Ayşe')") //nolint:errcheck
	dst.Exec("CREATE TABLE people (person_id INTEGER PRIMARY KEY, name TEXT)") //nolint:errcheck

	migration := `{"sourceCode": "src", "targetCode": "dst", "sourceQuery": "SELECT id, full_name FROM person",
		"targetQuery": "INSERT INTO people (person_id, name) VALUES (:id, :full_name)", "batchSize": 2}`
	resp, body := e.call("POST", "/db/migrate", migration)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"transferredRows":3`) {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	// Again: the ids are taken, so it fails and nothing more is written.
	resp, body = e.call("POST", "/db/migrate", migration)
	if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, `"success":false`) {
		t.Errorf("%d %s", resp.StatusCode, body)
	}

	resp, body = e.call("POST", "/db/migrate", `{"sourceCode": "src", "targetCode": "nowhere", "sourceQuery": "x", "targetQuery": "y"}`)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, "nowhere") {
		t.Errorf("unknown target: %d %s", resp.StatusCode, body)
	}
	resp, body = e.call("POST", "/db/migrate", `{"sourceCode": "src", "batchSize": 0, "commitMode": "NEVER"}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, `"field":"commitMode"`) || !strings.Contains(body, `"field":"targetCode"`) {
		t.Errorf("invalid: %d %s", resp.StatusCode, body)
	}
}

func TestBackgroundMigration(t *testing.T) {
	e := newEnv(t)
	src, dst := e.sqliteConfig("src"), e.sqliteConfig("dst")
	src.Exec("CREATE TABLE n (i INTEGER)")                   //nolint:errcheck
	src.Exec("INSERT INTO n VALUES (1), (2), (3), (4), (5)") //nolint:errcheck
	dst.Exec("CREATE TABLE m (i INTEGER)")                   //nolint:errcheck

	resp, body := e.call("POST", "/db/migrations", `{"sourceCode": "src", "targetCode": "dst",
		"sourceQuery": "SELECT i FROM n", "targetQuery": "INSERT INTO m VALUES (:i)"}`)
	var job jobs.View
	json.Unmarshal([]byte(body), &job) //nolint:errcheck
	if resp.StatusCode != http.StatusAccepted || resp.Header.Get("Location") != "/db/migrations/"+job.ID || job.ID == "" {
		t.Fatalf("%d %v %s", resp.StatusCode, resp.Header, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for job.Status == jobs.Running && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		_, body = e.call("GET", "/db/migrations/"+job.ID, "")
		json.Unmarshal([]byte(body), &job) //nolint:errcheck
	}
	if job.Status != jobs.Succeeded || job.TransferredRows != 5 || job.Result == nil {
		t.Errorf("job = %s", body)
	}
	_, body = e.call("GET", "/db/migrations", "")
	if !strings.Contains(body, job.ID) {
		t.Errorf("list: %s", body)
	}
	if resp, _ := e.call("DELETE", "/db/migrations/"+job.ID, ""); resp.StatusCode != http.StatusAccepted {
		t.Errorf("cancel: %d", resp.StatusCode)
	}
	if resp, _ := e.call("GET", "/db/migrations/nope", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown job: %d", resp.StatusCode)
	}
	if resp, _ := e.call("DELETE", "/db/migrations/nope", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("cancel unknown job: %d", resp.StatusCode)
	}
	resp, _ = e.call("POST", "/db/migrations", `{"sourceCode": "src", "targetCode": "nowhere", "sourceQuery": "x", "targetQuery": "y"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown codes are reported at once: %d", resp.StatusCode)
	}
}

func TestConnectionCheck(t *testing.T) {
	e := newEnv(t)
	e.sqliteConfig("src")
	_, body := e.call("POST", "/db/config/src/test", "")
	if !strings.Contains(body, `"success":true`) || !strings.Contains(body, "SQLite 3.") {
		t.Errorf("check: %s", body)
	}
	e.call("POST", "/db/config", `{"code": "down", "driver": "postgres", "url": "postgres://127.0.0.1:1/x?connect_timeout=2"}`)
	resp, body := e.call("POST", "/db/config/down/test", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"success":false`) {
		t.Errorf("unreachable: %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.call("POST", "/db/config/nope/test", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown: %d", resp.StatusCode)
	}
}
