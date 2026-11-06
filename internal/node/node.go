// Package node exposes a filesystem behind a single interface, so that a local
// disk and a machine across the tailnet are indistinguishable to callers.
package node

import (
	"context"
	"io"
	"io/fs"
	"path"
	"sort"
	"time"
)

// Entry is one item in a directory listing. It is deliberately small: only
// what a browser needs, and JSON-friendly for the web frontend.
type Entry struct {
	Name    string    `json:"name"`
	IsDir   bool      `json:"isDir"`
	Size    int64     `json:"size"`
	Mode    string    `json:"mode"`
	ModTime time.Time `json:"modTime"`
}

// Node is a browsable filesystem on some device.
//
// All paths are absolute POSIX paths. Every device knot talks to is Linux or
// Android/Termux, so forward slashes are safe everywhere and callers never need
// to know which OS they are addressing.
type Node interface {
	// Name is the short identifier used on the command line and in the UI.
	Name() string
	// Kind reports the transport, "local" or "ssh".
	Kind() string
	// Home is the directory a browser should open first.
	Home() string

	List(ctx context.Context, dir string) ([]Entry, error)
	Stat(ctx context.Context, p string) (Entry, error)

	Open(ctx context.Context, p string) (io.ReadCloser, error)
	Create(ctx context.Context, p string) (io.WriteCloser, error)

	Mkdir(ctx context.Context, p string) error
	MkdirAll(ctx context.Context, p string) error
	// Remove deletes a file, or a directory and everything under it.
	Remove(ctx context.Context, p string) error
	Rename(ctx context.Context, from, to string) error

	Close() error
}

// SortEntries orders a listing the way a file browser should: directories
// first, then case-insensitive by name.
func SortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return lessFold(a.Name, b.Name)
	})
}

func lessFold(a, b string) bool {
	la, lb := len(a), len(b)
	for i := 0; i < la && i < lb; i++ {
		ca, cb := lower(a[i]), lower(b[i])
		if ca != cb {
			return ca < cb
		}
	}
	return la < lb
}

func lower(c byte) byte {
	if c >= 'A' && c <= 'Z' {
		return c + ('a' - 'A')
	}
	return c
}

// Join is path.Join with the convenience that absolute second arguments are
// treated as relative to the first. Browser UIs hand us user-typed paths, and
// silently escaping the parent directory is never what was intended.
func Join(dir, name string) string {
	if name == "" || name == "." {
		return dir
	}
	for len(name) > 0 && name[0] == '/' {
		name = name[1:]
	}
	if name == "" {
		return "/"
	}
	return path.Join(dir, name)
}

// Parent returns the directory above p, clamped at root.
func Parent(p string) string {
	parent := path.Dir(p)
	if parent == "" || parent == "." {
		return "/"
	}
	return parent
}

// Base is the final element of p, with root reported as "/".
func Base(p string) string {
	b := path.Base(p)
	if b == "" || b == "." {
		return "/"
	}
	return b
}

func entryFromInfo(name string, info fs.FileInfo) Entry {
	return Entry{
		Name:    name,
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		Mode:    info.Mode().String(),
		ModTime: info.ModTime(),
	}
}
