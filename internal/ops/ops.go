// Package ops implements transfers between nodes.
package ops

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"

	"github.com/HimanshuSardana/knot/internal/node"
)

// Progress reports bytes copied so far out of a precomputed total. total is 0
// when it could not be determined.
type Progress func(copied, total int64)

// Copy transfers from src/from to dst/to. Directories are copied recursively.
//
// Bytes always stream through the machine running knot. That costs bandwidth on
// the host but means no device needs inbound access from any other device, and
// it works for the phone over DERP without any extra setup.
func Copy(ctx context.Context, src node.Node, from string, dst node.Node, to string, prog Progress) error {
	if err := guardSameNode(src, dst, from, to); err != nil {
		return err
	}

	total, _ := Total(ctx, src, from)
	var copied atomic.Int64
	report := func(n int64) {
		if prog != nil {
			prog(copied.Add(n), total)
		}
	}

	if err := copyTree(ctx, src, from, dst, to, report); err != nil {
		return err
	}
	if prog != nil {
		prog(copied.Load(), total)
	}
	return nil
}

// Move copies and then deletes the source. Within one node it is a rename, so
// no bytes cross the network.
func Move(ctx context.Context, src node.Node, from string, dst node.Node, to string, prog Progress) error {
	if src.Name() == dst.Name() {
		return src.Rename(ctx, from, to)
	}
	if err := Copy(ctx, src, from, dst, to, prog); err != nil {
		return err
	}
	return src.Remove(ctx, from)
}

func copyTree(ctx context.Context, src node.Node, from string, dst node.Node, to string, report func(int64)) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	info, err := src.Stat(ctx, from)
	if err != nil {
		return fmt.Errorf("%s: %w", from, err)
	}

	if info.IsDir {
		if err := dst.MkdirAll(ctx, to); err != nil {
			return fmt.Errorf("mkdir %s: %w", to, err)
		}
		entries, err := src.List(ctx, from)
		if err != nil {
			return fmt.Errorf("list %s: %w", from, err)
		}
		for _, e := range entries {
			if err := copyTree(ctx, src, node.Join(from, e.Name), dst, node.Join(to, e.Name), report); err != nil {
				return err
			}
		}
		return nil
	}

	r, err := src.Open(ctx, from)
	if err != nil {
		return fmt.Errorf("open %s: %w", from, err)
	}
	defer r.Close()

	w, err := dst.Create(ctx, to)
	if err != nil {
		return fmt.Errorf("create %s: %w", to, err)
	}

	buf := make([]byte, 256*1024)
	for {
		if err := ctx.Err(); err != nil {
			w.Close()
			return err
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				w.Close()
				return fmt.Errorf("write %s: %w", to, werr)
			}
			report(int64(n))
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			w.Close()
			return fmt.Errorf("read %s: %w", from, rerr)
		}
	}

	if err := w.Close(); err != nil {
		return fmt.Errorf("close %s: %w", to, err)
	}
	return nil
}

// Total sums the byte size of everything under p.
func Total(ctx context.Context, n node.Node, p string) (int64, error) {
	info, err := n.Stat(ctx, p)
	if err != nil {
		return 0, err
	}
	if !info.IsDir {
		return info.Size, nil
	}

	var sum int64
	entries, err := n.List(ctx, p)
	if err != nil {
		return 0, err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return sum, err
		}
		sub, err := Total(ctx, n, node.Join(p, e.Name))
		if err != nil {
			continue
		}
		sum += sub
	}
	return sum, nil
}

// guardSameNode rejects copying a directory into itself, which would otherwise
// recurse until the disk fills.
func guardSameNode(src, dst node.Node, from, to string) error {
	if src.Name() != dst.Name() {
		return nil
	}
	if from == to {
		return errors.New("source and destination are the same path")
	}
	prefix := strings.TrimSuffix(from, "/") + "/"
	if strings.HasPrefix(to, prefix) {
		return errors.New("cannot copy a directory into itself")
	}
	return nil
}
