// Copyright 2025 Mehmet ERGULCU
// SPDX-License-Identifier: Apache-2.0

// Command datatransporter serves the API that copies data from one database to another.
//
//	DATATRANSPORTER_ADDR             listen address (:8888)
//	DATATRANSPORTER_USER             API user (admin)
//	DATATRANSPORTER_PASSWORD         API password; without it a random one is printed at startup
//	DATATRANSPORTER_CRYPTO_PASSWORD  key for the saved passwords; without it (and the salt) a random key is
//	                                 generated on the first start and kept in <data dir>/crypto.key
//	DATATRANSPORTER_CRYPTO_SALT      hex salt for that key, at least 16 characters; give both or neither
//	DATATRANSPORTER_DATA_DIR         where the saved settings are kept (./data)
//	DATATRANSPORTER_ALLOWED_DRIVERS  comma separated (postgres,mysql,mariadb,sqlserver,oracle)
//	DATATRANSPORTER_POOL_SIZE        connections per saved database (4)
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ergulcu/data-transporter/internal/api"
	"github.com/ergulcu/data-transporter/internal/configs"
	"github.com/ergulcu/data-transporter/internal/drivers"
	"github.com/ergulcu/data-transporter/internal/jobs"
	"github.com/ergulcu/data-transporter/internal/secret"
	"github.com/ergulcu/data-transporter/internal/transfer"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(ctx, log, os.Getenv); err != nil {
		log.Error("datatransporter stopped", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, log *slog.Logger, getenv func(string) string) error {
	dataDir := or(getenv("DATATRANSPORTER_DATA_DIR"), "data")
	cryptoPassword, cryptoSalt, generated, err := secret.LoadOrCreateKey(dataDir,
		getenv("DATATRANSPORTER_CRYPTO_PASSWORD"), getenv("DATATRANSPORTER_CRYPTO_SALT"))
	if err != nil {
		return err
	}
	if generated {
		log.Info("generated a random key for the saved database passwords; keep this file with the data",
			"file", dataDir+"/"+secret.KeyFile)
	}
	box, err := secret.New(cryptoPassword, cryptoSalt)
	if err != nil {
		return err
	}

	allowed := drivers.DefaultAllowed
	if list := getenv("DATATRANSPORTER_ALLOWED_DRIVERS"); list != "" {
		allowed = nil
		for _, name := range strings.Split(list, ",") {
			name = strings.TrimSpace(name)
			if _, ok := drivers.Lookup(name); !ok {
				return fmt.Errorf("DATATRANSPORTER_ALLOWED_DRIVERS: unknown driver %q; known are %s", name,
					strings.Join(drivers.Names(), ", "))
			}
			allowed = append(allowed, name)
		}
	}
	poolSize, err := strconv.Atoi(or(getenv("DATATRANSPORTER_POOL_SIZE"), "4"))
	if err != nil || poolSize < 1 {
		return fmt.Errorf("DATATRANSPORTER_POOL_SIZE must be a positive number")
	}

	creds := api.Credentials{User: or(getenv("DATATRANSPORTER_USER"), "admin"), Password: getenv("DATATRANSPORTER_PASSWORD")}
	if creds.Password == "" {
		creds.Password = rand.Text()
		log.Warn("no DATATRANSPORTER_PASSWORD; using a generated password for this run", "user", creds.User,
			"password", creds.Password)
	}

	store, err := configs.Open(ctx, dataDir, box, allowed)
	if err != nil {
		return err
	}
	defer store.Close()
	transporter := transfer.New(store, poolSize, log)
	defer transporter.Close()
	jobCtx, stopJobs := context.WithCancel(context.Background())
	background := jobs.New(jobCtx, transporter)

	srv := &http.Server{
		Addr:              or(getenv("DATATRANSPORTER_ADDR"), ":8888"),
		Handler:           api.New(store, transporter, background, creds, log),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	log.Info("listening", "addr", srv.Addr, "drivers", strings.Join(allowed, ","))

	select {
	case err = <-errc:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if e := srv.Shutdown(shutdown); e != nil && !errors.Is(e, http.ErrServerClosed) {
			err = e
		}
	}
	// Running migrations roll back what they have not committed.
	stopJobs()
	background.Wait()
	return err
}

func or(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
