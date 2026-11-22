# Data Transporter

A service that copies data from one database to another: it runs a query on the source and writes the rows
to the target with a parameterized insert or update, in batches. Written in Go as one binary without a Java
runtime or JDBC drivers; it replaces an earlier Spring Boot version.

Supported databases: PostgreSQL, MySQL, MariaDB, SQL Server and Oracle (and SQLite when allowed). All drivers
are pure Go, so no client libraries are needed.

## Features

- Saved connection settings that survive a restart (in a SQLite file); their passwords are stored encrypted
  (AES-256-GCM, key derived with PBKDF2) and never returned by the API.
- A connection pool per saved database.
- Streaming reads: the source rows are written as they arrive; the result is never held in memory.
- Transactional writes: `commitMode: ALL` (default) writes everything or nothing; `BATCH` commits every
  `batchSize` rows, for transfers too large for one transaction.
- Named parameters (`:name`) in the target query, matched to the source columns ignoring case (Oracle reports
  upper case names), and turned into the placeholders of the target database.
- Synchronous (`/db/migrate`) or background (`/db/migrations`) runs with progress and cancellation.
- HTTP Basic authentication on every endpoint except the health check.

## Run

```bash
go run ./cmd/datatransporter            # http://localhost:8888
docker compose up --build               # with a PostgreSQL and a MySQL database to try it on
```

| Variable | |
|---|---|
| `DATATRANSPORTER_PASSWORD` | password of the API user; without it a random one is printed at startup |
| `DATATRANSPORTER_USER` | name of the API user, `admin` |
| `DATATRANSPORTER_CRYPTO_PASSWORD`, `DATATRANSPORTER_CRYPTO_SALT` | key for the saved passwords; the salt is hex, at least 16 characters; give both or neither. Without them a random key is generated on the first start and kept in `crypto.key` in the data directory (readable by the owner only; back it up with the data). Changing the key makes the saved passwords unreadable |
| `DATATRANSPORTER_DATA_DIR` | where the saved settings are kept, `./data` |
| `DATATRANSPORTER_ALLOWED_DRIVERS` | drivers a saved setting may use, `postgres,mysql,mariadb,sqlserver,oracle`; add `sqlite` to allow SQLite files on the server |
| `DATATRANSPORTER_POOL_SIZE` | connections per saved database, `4` |
| `DATATRANSPORTER_ADDR` | listen address, `:8888` |

## API

All requests need HTTP Basic credentials (`curl -u admin:...`).

### Save connection settings

`POST /db/config` creates (201) or replaces (200) the settings with that code. The URL has no credentials;
the user name and password have their own fields, so the password can be stored encrypted.

```json
{
  "code": "dev_mysql",
  "driver": "mysql",
  "url": "mysql://localhost:3306/source_db",
  "username": "user",
  "password": "password"
}
```

| Driver | URL |
|---|---|
| `postgres` | `postgres://host:5432/db?sslmode=disable` |
| `mysql`, `mariadb` | `mysql://host:3306/db` (driver parameters such as `?tls=true` are passed on) |
| `sqlserver` | `sqlserver://host:1433?database=db` |
| `oracle` | `oracle://host:1521/SERVICE` |
| `sqlite` | `file:/path/to/file.db` |

The answer, like every read, has no password:

```json
{"code": "dev_mysql", "driver": "mysql", "url": "mysql://localhost:3306/source_db", "username": "user",
 "passwordSet": true, "updatedAt": "2026-11-16T09:00:00Z"}
```

| | |
|---|---|
| `GET /db/config/{code}` | one setting (404 if unknown) |
| `GET /db/configs` | all settings, by code |
| `DELETE /db/config/{code}` | 204 |
| `POST /db/config/{code}/test` | opens a connection: `{"success": true, "message": "Connected", "databaseProduct": "PostgreSQL 18.1 ..."}` |

### Migrate

```json
{
  "sourceCode": "source_db",
  "targetCode": "target_db",
  "sourceQuery": "SELECT id, full_name FROM person",
  "targetQuery": "INSERT INTO people (person_id, name) VALUES (:id, :full_name)",
  "batchSize": 1000,
  "commitMode": "ALL"
}
```

`POST /db/migrate` waits for the end: 200 with the result, 422 when it failed, 404 for an unknown code.

```json
{"success": true, "message": "Migration completed", "transferredRows": 1500, "committedRows": 1500, "durationMillis": 5210}
```

`POST /db/migrations` answers 202 at once with a job; follow it with `GET /db/migrations/{id}`
(`RUNNING`, `SUCCEEDED`, `FAILED`, `CANCELLED`, plus `transferredRows` so far), list them with
`GET /db/migrations`, cancel with `DELETE /db/migrations/{id}`. Stopping the service cancels the running
jobs, which roll back what they have not committed.

Errors are [problem details](https://www.rfc-editor.org/rfc/rfc9457): 400 for invalid input (with every
invalid field in `errors`), 401 without credentials, 404 for unknown codes.

## Differences from the Java version

- DB2 is not supported: there is no pure Go driver for it (IBM's needs cgo and the DB2 CLI client).
- Settings take a driver name and a URL instead of a JDBC URL and a driver class.
- A URL with a password in it is refused, so that no password is stored unencrypted.
- The rows of a batch are written one statement at a time inside the transaction (database/sql has no
  JDBC-style batch); `batchSize` sets how often progress is reported and, with `BATCH`, how often to commit.
- Settings saved by the Java version are not read; save them again.

## Tests

```bash
go test ./...                        # on SQLite, no Docker needed
go test -tags integration ./...      # moves rows PostgreSQL → MySQL → SQL Server → Oracle → PostgreSQL (Docker)
```

The integration test checks that Turkish text, decimals, timestamps, bytes and nulls arrive unchanged
after passing through all four databases. On Docker Desktop for Windows set `TESTCONTAINERS_RYUK_DISABLED=true`
if the Testcontainers reaper fails to start.

## License

Copyright 2025 Mehmet ERGULCU. Licensed under the [Apache License 2.0](LICENSE).
