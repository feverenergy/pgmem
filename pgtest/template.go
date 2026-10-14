package pgtest

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/feverenergy/pgmem"
)

// buildTimeout bounds a template build: the server's start, Migrate, the
// support schema and the shutdown.
const buildTimeout = 10 * time.Minute

// templateLayoutVersion is part of the cache file name. Bump it when
// buildTemplate changes what goes into the image.
const templateLayoutVersion = "2"

// buildTemplate builds the image for tpl. It starts a server on a fresh
// cluster, creates database tmpl, runs tpl.Migrate in it through a pool of
// up to 4 connections, installs the support schema and shuts the server
// down. The shutdown produces the image. installSupport must be the last
// thing to touch tmpl. DDL after it would mark the template dirty, and
// then no copy of it could be recycled.
//
// The build has its own 10-minute timeout and ignores any caller's
// context, because other processes may be waiting on the lock for it.
// buildTemplate closes the server on every path, including a panic or
// runtime.Goexit (t.Fatal) in Migrate.
func buildTemplate(tpl Template) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()

	srv, err := pgmem.Start(ctx, pgmem.Options{Engine: tpl.Engine})
	if err != nil {
		return nil, fmt.Errorf("pgtest: start: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			srv.Close()
		}
	}()

	if err := execIn(ctx, srv, "postgres", "CREATE DATABASE tmpl"); err != nil {
		return nil, fmt.Errorf("pgtest: create database tmpl: %w", err)
	}
	if tpl.Migrate != nil {
		if err := migrate(ctx, srv, tpl.Migrate); err != nil {
			return nil, err
		}
	}

	conn, err := pgx.ConnectConfig(ctx, connConfig(srv, "tmpl"))
	if err != nil {
		return nil, fmt.Errorf("pgtest: install support schema: %w", err)
	}
	err = installSupport(ctx, conn)
	conn.Close(ctx)
	if err != nil {
		return nil, err // installSupport's errors start with "pgtest: "
	}

	img, err := srv.Shutdown(ctx)
	if err != nil {
		return nil, fmt.Errorf("pgtest: shutdown: %w", err)
	}
	ok = true
	return img, nil
}

// migrate runs fn in database tmpl on srv, through a pool of up to 4
// connections. If fn leaves a connection acquired (rows not closed), it
// returns an error and abandons the pool. pgxpool's Close would wait for
// the connection forever, while this process holds the cache lock.
// Closing the server then ends the connections.
func migrate(ctx context.Context, srv *pgmem.Server, fn func(context.Context, *pgxpool.Pool) error) (err error) {
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig(srv, "tmpl", 4))
	if err != nil {
		return fmt.Errorf("pgtest: migrate: %w", err)
	}
	defer func() {
		// Also on a panic or Goexit in fn.
		if n := pool.Stat().AcquiredConns(); n > 0 {
			leak := fmt.Errorf("pgtest: migrate: Migrate left %d connection(s) acquired (rows not closed?)", n)
			if err != nil {
				leak = fmt.Errorf("%w; it also returned: %w", leak, err)
			}
			err = leak
			return
		}
		pool.Close()
	}()
	if err := fn(ctx, pool); err != nil {
		return fmt.Errorf("pgtest: migrate: %w", err)
	}
	return nil
}

// execIn runs sql in database db on srv, on a connection of its own.
func execIn(ctx context.Context, srv *pgmem.Server, db, sql string) error {
	conn, err := pgx.ConnectConfig(ctx, connConfig(srv, db))
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, sql)
	return err
}
