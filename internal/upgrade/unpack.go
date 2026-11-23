package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Unpack limits (item 6.3). MaxStream bounds the whole decompressed
// stream, headers (PAX, long names) included.
const (
	MaxUnpacked = 256 << 20
	MaxStream   = MaxUnpacked + 1<<20
	maxEntries  = 1000
)

// limits are Unpack's bounds; tests use small ones.
type limits struct {
	total   int64
	stream  int64
	entries int
}

var defaultLimits = limits{total: MaxUnpacked, stream: MaxStream, entries: maxEntries}

var errStream = errors.New("decompressed stream too large")

// streamCap fails a read past n bytes of the decompressed stream. Exactly
// n bytes followed by EOF is fine.
type streamCap struct {
	r io.Reader
	n int64
}

func (c *streamCap) Read(p []byte) (int, error) {
	if c.n <= 0 {
		var one [1]byte
		if n, _ := c.r.Read(one[:]); n > 0 {
			return 0, errStream
		}
		return 0, io.EOF
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	n, err := c.r.Read(p)
	c.n -= int64(n)
	return n, err
}

// Unpack extracts a gzip tarball into dest, which is replaced. Only regular
// files and directories are allowed, all under the single top directory
// top/, with no absolute path, no ".." and no links; the file sizes may
// add up to MaxUnpacked. dest and every directory are 0700; a file is 0700
// when its header has an execute bit and 0600 otherwise. On error the
// partial tree is removed.
func Unpack(tarball []byte, dest, top string) error {
	return unpack(tarball, dest, top, defaultLimits)
}

func unpack(tarball []byte, dest, top string, lim limits) (err error) {
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if err := os.Mkdir(dest, 0o700); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(dest)
		}
	}()
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return errors.New("not gzip")
	}
	tr := tar.NewReader(&streamCap{r: gz, n: lim.stream})
	var total int64
	n := 0
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if errors.Is(err, errStream) {
			return errStream
		}
		if err != nil {
			return errors.New("bad tar")
		}
		n++
		if n > lim.entries {
			return errors.New("too many entries")
		}
		rel, err := entryPath(hdr.Name, top)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return fmt.Errorf("%s: cannot create", rel)
			}
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > lim.total-total {
				return errors.New("bundle too large")
			}
			total += hdr.Size
			mode := os.FileMode(0o600)
			if hdr.Mode&0o111 != 0 {
				mode = 0o700
			}
			if err := writeEntry(target, tr, hdr.Size, mode); err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
		default:
			return fmt.Errorf("%s: not a regular file or directory", rel)
		}
	}
	fi, err := os.Lstat(filepath.Join(dest, top))
	if err != nil || !fi.IsDir() {
		return errors.New("top directory missing")
	}
	return nil
}

// entryPath checks one tar name and returns it as a clean relative path.
// The name must be top, top/ or top/<safe relative path>.
func entryPath(name, top string) (string, error) {
	clean := strings.TrimSuffix(name, "/")
	if clean == top {
		return top, nil
	}
	if strings.HasPrefix(name, "/") || !strings.HasPrefix(clean, top+"/") {
		return "", fmt.Errorf("%q: outside %s/", name, top)
	}
	if !safeRel(clean) {
		return "", fmt.Errorf("%q: unsafe path", name)
	}
	return clean, nil
}

// writeEntry creates path (which must not exist) and copies exactly size
// bytes. A missing parent is created 0700; the tree holds no links, so
// MkdirAll cannot leave dest.
func writeEntry(path string, r io.Reader, size int64, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errors.New("cannot create parent")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return errors.New("cannot create")
	}
	n, err := io.Copy(f, io.LimitReader(r, size))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil || n != size {
		return errors.New("short write")
	}
	return os.Chmod(path, mode)
}
