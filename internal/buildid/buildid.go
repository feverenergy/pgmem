// Package buildid gives the module's own packages, such as pgtest, the ID
// of the Postgres build that pgmem.Start runs, without adding it to pgmem's
// public API. Being internal, it can be imported only from within this
// module.
package buildid

import "io/fs"

// ID returns the build ID of the pgmem.Engine with the given Version and
// FS. Both empty mean the default engine. The ID is the lowercase hex
// sha256 over the engine's Version, postgres.wasm, share.tar and
// cluster.tar, in that order, each preceded by its length as 8 big-endian
// bytes. The files are the bytes Start would use.
//
// When $PGMEM_BUILD_DIR is set, the files come from that directory for
// every engine, and the Version hashed is empty. Otherwise ID fails as
// Start would for the engine, such as for an Engine with only one field
// set.
//
// ID computes an ID once per process for each Version, or for each value
// of $PGMEM_BUILD_DIR, and keeps it. It does not keep an error, so the
// next call tries again. Package pgmem sets ID in an init function, so it
// is non-nil in any program that imports pgmem.
var ID func(version string, fsys fs.FS) (string, error)
