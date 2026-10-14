package pgmem_test

import (
	"context"
	"fmt"
	"log"

	"github.com/feverenergy/pgmem"
	"github.com/jackc/pgx/v5"
)

// Start a server and query it with pgx. Every connection goes through
// Server.Dial, so the server opens no port and no socket file.
func ExampleStart() {
	ctx := context.Background()
	srv, err := pgmem.Start(ctx, pgmem.Options{})
	if err != nil {
		log.Fatal(err)
	}
	defer srv.Close()

	cfg, err := pgx.ParseConfig("host=/tmp user=postgres sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	cfg.DialFunc = srv.Dial
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close(ctx)

	var version string
	if err := conn.QueryRow(ctx, "SHOW server_version").Scan(&version); err != nil {
		log.Fatal(err)
	}
	fmt.Println(version)
}
