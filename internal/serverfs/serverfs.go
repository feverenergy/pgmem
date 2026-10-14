// Package serverfs lets the module's own commands reach a pgmem server's
// in-memory file system without adding it to pgmem's public API. Being
// internal, it can be imported only from within this module.
package serverfs

import "github.com/feverenergy/pgmem/internal/memfs"

// FS returns the file system of s, a *pgmem.Server. Package pgmem sets it
// in an init function, so it is non-nil in any program that imports pgmem.
var FS func(s any) *memfs.FS
