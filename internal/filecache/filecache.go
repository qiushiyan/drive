// Package filecache keeps what a file read as for as long as the file stands
// as it was read. A watch looks every few seconds, for hours, at a transcript
// that grows only while its session writes and a log that grows only when
// something happens, and each look read and parsed both whole.
//
// It holds no state of its own: the file is the only one, and what is kept is
// that file's reading, dropped the moment the file differs in anything a
// write, a replacement or a change of permissions shows — which file it is,
// its size, its modification time or its mode.
package filecache

import (
	"os"
	"sync"
)

// Cache is the readings of files as one kind of value, by path.
type Cache[T any] struct {
	// Clone copies a reading so no two readers share what one may change.
	Clone func(T) T
	m     sync.Map // path, to reading
}

type reading[T any] struct {
	of    os.FileInfo
	value T
}

// Read is read's answer for path: the one kept, when the file stands as it
// did then, else a new one, which is kept only when it was read whole from a
// file that did not change under the read. A file that cannot be examined, or
// a read that fails, is read's own every time.
func (c *Cache[T]) Read(path string, read func() (T, error)) (T, error) {
	before, err := os.Stat(path)
	if err != nil {
		return read()
	}
	if kept, ok := c.m.Load(path); ok {
		if kept := kept.(reading[T]); same(kept.of, before) {
			return c.Clone(kept.value), nil
		}
	}
	v, err := read()
	if err != nil {
		return v, err
	}
	if after, err := os.Stat(path); err == nil && same(before, after) {
		c.m.Store(path, reading[T]{of: before, value: c.Clone(v)})
	}
	return v, nil
}

// same says b is the file a was, unwritten since: a chmod changes neither
// size nor time, and what a file reads as may hang on it.
func same(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && a.Mode() == b.Mode()
}
