// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Package drivers knows the databases that data can be moved between: how to build a connection string
// from a URL and separate credentials, which placeholders they use, and how to ask for their version.
// All drivers are pure Go, so the program needs no client libraries.
package drivers

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"  // pgx
	_ "github.com/microsoft/go-mssqldb" // sqlserver
	_ "github.com/sijms/go-ora/v2"      // oracle
	_ "modernc.org/sqlite"              // sqlite

	"github.com/ergulcu/data-transporter/internal/named"
)

// Driver is one kind of database.
type Driver struct {
	Name string
	// Example is a URL in the form the driver expects.
	Example     string
	sqlName     string
	schemes     []string
	Placeholder named.Placeholder
	// Version asks the database for its product and version.
	Version string
	dsn     func(u *url.URL, raw, user, password string) (string, error)
}

var all = map[string]*Driver{
	"postgres": {Name: "postgres", Example: "postgres://localhost:5432/shop?sslmode=disable", sqlName: "pgx",
		schemes: []string{"postgres", "postgresql"}, Placeholder: named.Dollar, Version: "SELECT version()", dsn: withUser},
	"mysql": {Name: "mysql", Example: "mysql://localhost:3306/shop", sqlName: "mysql",
		schemes: []string{"mysql"}, Placeholder: named.Question, Version: "SELECT CONCAT('MySQL ', VERSION())", dsn: mysqlDSN},
	"mariadb": {Name: "mariadb", Example: "mariadb://localhost:3306/shop", sqlName: "mysql",
		schemes: []string{"mariadb", "mysql"}, Placeholder: named.Question, Version: "SELECT CONCAT('MariaDB ', VERSION())", dsn: mysqlDSN},
	"sqlserver": {Name: "sqlserver", Example: "sqlserver://localhost:1433?database=shop", sqlName: "sqlserver",
		schemes: []string{"sqlserver"}, Placeholder: named.AtP, Version: "SELECT @@VERSION", dsn: withUser},
	"oracle": {Name: "oracle", Example: "oracle://localhost:1521/FREEPDB1", sqlName: "oracle",
		schemes: []string{"oracle"}, Placeholder: named.Colon,
		Version: "SELECT banner FROM v$version WHERE banner LIKE 'Oracle%' AND ROWNUM = 1", dsn: withUser},
	// SQLite reads and writes files on the server, so it is not allowed unless configured.
	"sqlite": {Name: "sqlite", Example: "file:/var/data/shop.db", sqlName: "sqlite",
		schemes: []string{"file"}, Placeholder: named.Question, Version: "SELECT 'SQLite ' || sqlite_version()", dsn: sqliteDSN},
}

// DefaultAllowed are the drivers allowed when nothing else is configured.
var DefaultAllowed = []string{"postgres", "mysql", "mariadb", "sqlserver", "oracle"}

// Names lists every known driver.
func Names() []string {
	names := make([]string, 0, len(all))
	for n := range all {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Lookup finds a driver by name.
func Lookup(name string) (*Driver, bool) {
	d, ok := all[name]
	return d, ok
}

// CheckURL tells why a URL does not suit the driver, or returns nil.
func (d *Driver) CheckURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a URL: %w", err)
	}
	ok := false
	for _, s := range d.schemes {
		ok = ok || u.Scheme == s
	}
	if !ok {
		return fmt.Errorf("a %s URL looks like %s", d.Name, d.Example)
	}
	if u.User != nil {
		return errors.New("put the user name and password in their own fields, so that the password is stored encrypted")
	}
	if d.Name != "sqlite" && u.Host == "" {
		return fmt.Errorf("the URL has no host; a %s URL looks like %s", d.Name, d.Example)
	}
	return nil
}

// Open returns a connection pool. Nothing is connected until the pool is used.
func (d *Driver) Open(raw, user, password string) (*sql.DB, error) {
	if err := d.CheckURL(raw); err != nil {
		return nil, err
	}
	u, _ := url.Parse(raw)
	dsn, err := d.dsn(u, raw, user, password)
	if err != nil {
		return nil, err
	}
	return sql.Open(d.sqlName, dsn)
}

// withUser puts the credentials into the URL; PostgreSQL, SQL Server and Oracle read them from there.
func withUser(u *url.URL, _, user, password string) (string, error) {
	if user != "" {
		u.User = url.UserPassword(user, password)
	}
	if u.Scheme == "postgresql" {
		u.Scheme = "postgres"
	}
	return u.String(), nil
}

// mysqlDSN turns mysql://host:3306/db?params into the driver's user:password@tcp(host:3306)/db?params.
func mysqlDSN(u *url.URL, _, user, password string) (string, error) {
	host := u.Host
	if u.Port() == "" {
		host += ":3306"
	}
	cfg, err := mysql.ParseDSN("tcp(" + host + ")/" + strings.TrimPrefix(u.Path, "/") + "?" + u.RawQuery)
	if err != nil {
		return "", err
	}
	cfg.User, cfg.Passwd = user, password
	return cfg.FormatDSN(), nil
}

// sqliteDSN keeps the file URL and turns on foreign keys and a busy timeout.
func sqliteDSN(_ *url.URL, raw, _, _ string) (string, error) {
	sep := "?"
	if strings.Contains(raw, "?") {
		sep = "&"
	}
	return raw + sep + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", nil
}

// IsBinary tells whether a column of this database type holds bytes rather than text. Drivers hand text as
// []byte as well (MySQL does for most columns), and such values are written to the target as strings.
func IsBinary(databaseTypeName string) bool {
	t := strings.ToUpper(databaseTypeName)
	for _, b := range []string{"BLOB", "BINARY", "VARBINARY", "BYTEA", "RAW", "IMAGE", "BIT", "GEOMETRY"} {
		if strings.Contains(t, b) {
			return true
		}
	}
	return false
}
