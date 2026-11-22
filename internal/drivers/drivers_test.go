// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

package drivers

import (
	"net/url"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
)

func dsn(t *testing.T, driver, raw, user, password string) string {
	t.Helper()
	d, _ := Lookup(driver)
	if err := d.CheckURL(raw); err != nil {
		t.Fatalf("%s %s: %v", driver, raw, err)
	}
	u, _ := url.Parse(raw)
	s, err := d.dsn(u, raw, user, password)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestConnectionStrings(t *testing.T) {
	tests := []struct{ driver, url, user, password, want string }{
		{"postgres", "postgres://db1:5432/shop?sslmode=disable", "app", "p@ss/word", "postgres://app:p%40ss%2Fword@db1:5432/shop?sslmode=disable"},
		{"postgres", "postgresql://db1/shop", "", "", "postgres://db1/shop"},
		{"sqlserver", "sqlserver://db2:1433?database=Shop", "sa", "Secret1!", "sqlserver://sa:Secret1%21@db2:1433?database=Shop"},
		{"oracle", "oracle://db3:1521/FREEPDB1", "app", "pw", "oracle://app:pw@db3:1521/FREEPDB1"},
		{"sqlite", "file:/tmp/a.db", "", "", "file:/tmp/a.db?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"},
		{"sqlite", "file:/tmp/a.db?mode=ro", "", "", "file:/tmp/a.db?mode=ro&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"},
	}
	for _, tt := range tests {
		if got := dsn(t, tt.driver, tt.url, tt.user, tt.password); got != tt.want {
			t.Errorf("%s %s:\n got %s\nwant %s", tt.driver, tt.url, got, tt.want)
		}
	}
}

func TestMySQLConnectionString(t *testing.T) {
	got := dsn(t, "mysql", "mysql://db4/shop?parseTime=true", "app", "p@ss:w/rd")
	cfg, err := mysql.ParseDSN(got)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.User != "app" || cfg.Passwd != "p@ss:w/rd" || cfg.Addr != "db4:3306" || cfg.DBName != "shop" || !cfg.ParseTime {
		t.Errorf("dsn %s gives %+v", got, cfg)
	}
	if got := dsn(t, "mariadb", "mariadb://db5:3307/x", "u", ""); !strings.Contains(got, "tcp(db5:3307)/x") {
		t.Errorf("mariadb dsn = %s", got)
	}
}

func TestBadURLs(t *testing.T) {
	tests := []struct{ driver, url, want string }{
		{"postgres", "jdbc:postgresql://db1/shop", "looks like postgres://"},
		{"postgres", "postgres://app:secret@db1/shop", "own fields"},
		{"mysql", "mysql:///shop", "no host"},
		{"oracle", "sqlserver://db3", "looks like oracle://"},
		{"sqlite", "/tmp/a.db", "looks like file:"},
	}
	for _, tt := range tests {
		d, _ := Lookup(tt.driver)
		if err := d.CheckURL(tt.url); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s %s: %v", tt.driver, tt.url, err)
		}
	}
}

func TestNamesAndBinaryTypes(t *testing.T) {
	if got := strings.Join(Names(), ","); got != "mariadb,mysql,oracle,postgres,sqlite,sqlserver" {
		t.Errorf("Names() = %s", got)
	}
	for typ, want := range map[string]bool{"BYTEA": true, "varbinary": true, "LONGBLOB": true, "RAW": true,
		"VARCHAR": false, "TEXT": false, "NUMERIC": false, "INT8": false, "DATETIME": false} {
		if IsBinary(typ) != want {
			t.Errorf("IsBinary(%s) = %v", typ, !want)
		}
	}
}
