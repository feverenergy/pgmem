package memfs

// pageSize is the size of one piece of a regular file's data. It matches
// the 64 KiB pieces Postgres's copy_file writes in CREATE DATABASE ...
// FILE_COPY, and is a multiple of its 8 KiB block, so block reads and
// writes never span two pages.
const pageSize = 64 << 10

// fileData is a regular file's contents, kept in pages rather than one
// slice. Growing a file appends pages and never moves the bytes already
// there. With one slice, a file written front to back was reallocated and
// copied each time it outgrew its capacity, which allocated about five
// times its size.
//
// Every page but the last holds exactly pageSize bytes. The last holds the
// rest, 1 to pageSize bytes. The zero value is an empty file.
type fileData struct {
	pages [][]byte
	size  int
}

// readAt copies the data from off into p and returns the byte count. It
// stops at the end of the file.
func (d *fileData) readAt(p []byte, off int) int {
	n := 0
	for n < len(p) && off < d.size {
		c := copy(p[n:], d.pages[off/pageSize][off%pageSize:])
		n += c
		off += c
	}
	return n
}

// writeAt copies p into the data at off. The file must already reach
// off+len(p).
func (d *fileData) writeAt(p []byte, off int) {
	for len(p) > 0 {
		c := copy(d.pages[off/pageSize][off%pageSize:], p)
		p = p[c:]
		off += c
	}
}

// resize sets the length to size. New bytes are zero. Resizing to 0
// releases all pages.
func (d *fileData) resize(size int) {
	switch {
	case size == 0:
		*d = fileData{}
		return
	case size < d.size:
		n := (size + pageSize - 1) / pageSize
		clear(d.pages[n:]) // let the dropped pages be collected
		d.pages = d.pages[:n]
		d.pages[n-1] = d.pages[n-1][:size-(n-1)*pageSize]
	case size > d.size:
		if n := len(d.pages); n > 0 {
			d.pages[n-1] = growPage(d.pages[n-1], min(size-(n-1)*pageSize, pageSize))
		}
		for have := len(d.pages) * pageSize; have < size; have += pageSize {
			d.pages = append(d.pages, make([]byte, min(size-have, pageSize)))
		}
	}
	d.size = size
}

// bytes returns a copy of the data in one slice.
func (d *fileData) bytes() []byte {
	b := make([]byte, d.size)
	d.readAt(b, 0)
	return b
}

// growPage extends the page b to n bytes, at most pageSize, and the new
// bytes are zero. Capacity past len may still hold data from before a
// truncate, so growPage clears it. When b must move, its capacity doubles
// up to pageSize, so a file appended in small pieces copies each byte only
// a few times.
func growPage(b []byte, n int) []byte {
	if n <= cap(b) {
		old := len(b)
		b = b[:n]
		clear(b[old:])
		return b
	}
	nb := make([]byte, n, min(max(n, 2*cap(b)), pageSize))
	copy(nb, b)
	return nb
}
