package node

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// Local is a Node backed by the filesystem this process is running on.
type Local struct {
	name string
	home string
}

// NewLocal returns a Node for the machine knot is running on.
func NewLocal(name string) (*Local, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Local{name: name, home: home}, nil
}

func (l *Local) Name() string { return l.name }
func (l *Local) Kind() string { return "local" }
func (l *Local) Home() string { return l.home }
func (l *Local) Close() error { return nil }

func (l *Local) List(ctx context.Context, dir string) ([]Entry, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(des))
	for _, de := range des {
		info, err := de.Info()
		if err != nil {
			// A file can vanish between ReadDir and Info; skip it rather than
			// failing the whole listing.
			continue
		}
		entries = append(entries, entryFromInfo(de.Name(), info))
	}
	SortEntries(entries)
	return entries, nil
}

func (l *Local) Stat(ctx context.Context, p string) (Entry, error) {
	info, err := os.Stat(p)
	if err != nil {
		return Entry{}, err
	}
	return entryFromInfo(Base(p), info), nil
}

func (l *Local) Open(ctx context.Context, p string) (io.ReadCloser, error) {
	return os.Open(p)
}

func (l *Local) Create(ctx context.Context, p string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return nil, err
	}
	return os.Create(p)
}

func (l *Local) Mkdir(ctx context.Context, p string) error {
	return os.Mkdir(p, 0o755)
}

func (l *Local) MkdirAll(ctx context.Context, p string) error {
	return os.MkdirAll(p, 0o755)
}

func (l *Local) Remove(ctx context.Context, p string) error {
	if p == "/" || p == l.home {
		return errors.New("refusing to remove " + p)
	}
	return os.RemoveAll(p)
}

func (l *Local) Rename(ctx context.Context, from, to string) error {
	return os.Rename(from, to)
}
