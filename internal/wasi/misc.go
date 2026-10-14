package wasi

import (
	"context"
	"crypto/rand"
	"io"
	"runtime"
	"time"

	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"

	"github.com/feverenergy/pgmem/internal/wasiabi"
)

// start is the zero of the monotonic clock, shared by every process.
var start = time.Now()

// argsGet implements args_get(argv, argv_buf).
func argsGet(c *call) wasiabi.Errno { return c.putStrings(c.p.Args) }

// argsSizesGet implements args_sizes_get(*argc, *argv_buf_size).
func argsSizesGet(c *call) wasiabi.Errno { return c.putSizes(c.p.Args) }

// environGet implements environ_get(environ, environ_buf).
func environGet(c *call) wasiabi.Errno { return c.putStrings(c.p.Env) }

// environSizesGet implements environ_sizes_get(*count, *buf_size).
func environSizesGet(c *call) wasiabi.Errno { return c.putSizes(c.p.Env) }

// putStrings stores list as NUL-terminated strings in the buffer of
// argument 1 and their addresses in the pointer array of argument 0.
func (c *call) putStrings(list []string) wasiabi.Errno {
	ptrs, buf := c.u32(0), c.u32(1)
	for _, s := range list {
		if err := c.putU32(ptrs, buf); err != wasiabi.ESUCCESS {
			return err
		}
		if err := c.put(buf, append([]byte(s), 0)); err != wasiabi.ESUCCESS {
			return err
		}
		ptrs += 4
		buf += uint32(len(s)) + 1
	}
	return wasiabi.ESUCCESS
}

// putSizes stores the count of list at argument 0 and the bytes its
// NUL-terminated strings need at argument 1.
func (c *call) putSizes(list []string) wasiabi.Errno {
	size := 0
	for _, s := range list {
		size += len(s) + 1
	}
	if err := c.putU32(c.u32(0), uint32(len(list))); err != wasiabi.ESUCCESS {
		return err
	}
	return c.putU32(c.u32(1), uint32(size))
}

// clockResGet implements clock_res_get(id, *resolution). Every clock
// claims 1ns.
func clockResGet(c *call) wasiabi.Errno { return c.putU64(c.u32(1), 1) }

// clockTimeGet implements clock_time_get(id, precision, *time). The
// realtime clock is Proc.Realtime when that is set. Clocks other than
// realtime answer like the monotonic clock.
func clockTimeGet(c *call) wasiabi.Errno {
	var t int64
	switch {
	case c.u32(0) != wasiabi.ClockRealtime:
		t = int64(time.Since(start))
	case c.p.Realtime != nil:
		t = c.p.Realtime()
	default:
		t = time.Now().UnixNano()
	}
	return c.putU64(c.u32(2), uint64(t))
}

// procRaise implements proc_raise(sig), which preview1 deprecates. It
// answers ENOSYS.
func procRaise(*call) wasiabi.Errno { return wasiabi.ENOSYS }

// randomGet implements random_get(buf, buf_len). It reads from
// Proc.Random, or from crypto/rand if that is nil.
func randomGet(c *call) wasiabi.Errno {
	buf, err := c.bytes(c.u32(0), c.u32(1))
	if err != wasiabi.ESUCCESS {
		return err
	}
	if c.p.Random == nil {
		rand.Read(buf) // never fails, and crashes the program instead
		return wasiabi.ESUCCESS
	}
	if _, err := io.ReadFull(c.p.Random, buf); err != nil {
		return wasiabi.EIO
	}
	return wasiabi.ESUCCESS
}

// schedYield implements sched_yield().
func schedYield(*call) wasiabi.Errno {
	runtime.Gosched()
	return wasiabi.ESUCCESS
}

// procExit implements proc_exit(code). It closes the module and unwinds
// the call with the exit code, which reaches the caller of the guest as a
// *sys.ExitError.
func procExit(ctx context.Context, mod api.Module, stack []uint64) {
	code := uint32(stack[0])
	_ = mod.CloseWithExitCode(ctx, code)
	panic(sys.NewExitError(code))
}
