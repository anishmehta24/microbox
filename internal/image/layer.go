package image

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// Whiteouts: a layer marks what it deletes from the layers below with
// special entries, since a tar can only add files.
const (
	whiteoutPrefix = ".wh."         // .wh.NAME deletes NAME
	opaqueMarker   = ".wh..wh..opq" // in a directory: hide everything below it
)

// unpack extracts one layer's tar into dir, as an overlayfs lower layer:
//
//   - .wh.NAME becomes what overlayfs reads as a deleted file, a character
//     device 0:0 called NAME.
//   - .wh..wh..opq marks its directory opaque with the trusted.overlay.opaque
//     xattr, so overlayfs doesn't show the lower layers' contents of it.
//
// Everything goes through an os.Root on dir, which refuses any path that
// would resolve outside it, also through symlinks the layer itself created.
// So an entry like ../../etc/passwd, or a file written through a symlink to
// /etc, fails the pull instead of reaching the host.
func unpack(r io.Reader, dir string) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	type dirTimes struct {
		name  string
		mtime time.Time
	}
	var dirs []dirTimes
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		name := path.Clean(strings.TrimPrefix(hdr.Name, "/"))
		if name == "." {
			continue // the layer's root, which is dir
		}
		parent, base := path.Split(name)
		if parent == "" {
			parent = "."
		}
		if err := root.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if base == opaqueMarker {
			if err := setOpaque(root, parent); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			continue
		}
		if deleted, ok := strings.CutPrefix(base, whiteoutPrefix); ok {
			if err := whiteout(root, parent, deleted); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
			continue
		}
		if err := unpackEntry(root, name, hdr, tr); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if hdr.Typeflag == tar.TypeDir {
			dirs = append(dirs, dirTimes{name, hdr.ModTime})
		}
	}
	// Last, since creating entries in a directory changes its mtime.
	for _, d := range dirs {
		if err := root.Chtimes(d.name, d.mtime, d.mtime); err != nil {
			return err
		}
	}
	return nil
}

func unpackEntry(root *os.Root, name string, hdr *tar.Header, r io.Reader) error {
	// Tar files can name a path twice; the later entry wins. A directory
	// replacing a directory keeps its contents.
	if fi, err := root.Lstat(name); err == nil && !(fi.IsDir() && hdr.Typeflag == tar.TypeDir) {
		if err := root.RemoveAll(name); err != nil {
			return err
		}
	}
	var err error
	switch hdr.Typeflag {
	case tar.TypeDir:
		if err = root.Mkdir(name, 0o700); errors.Is(err, fs.ErrExist) {
			err = nil
		}
	case tar.TypeReg:
		var f *os.File
		if f, err = root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	case tar.TypeSymlink:
		// The target is stored as it is, and only resolved inside the
		// container, where / is the image's root.
		if err := root.Symlink(hdr.Linkname, name); err != nil {
			return err
		}
		// Not chmod or chtimes, which would follow the link.
		return root.Lchown(name, hdr.Uid, hdr.Gid)
	case tar.TypeLink:
		// Shares the target's owner, mode and times.
		return root.Link(path.Clean(strings.TrimPrefix(hdr.Linkname, "/")), name)
	case tar.TypeChar:
		err = mknod(root, name, unix.S_IFCHR, hdr)
	case tar.TypeBlock:
		err = mknod(root, name, unix.S_IFBLK, hdr)
	case tar.TypeFifo:
		err = mknod(root, name, unix.S_IFIFO, hdr)
	default:
		return nil // e.g. a PAX global header
	}
	if err != nil {
		return err
	}
	// chown before chmod: chown clears the setuid and setgid bits.
	if err := root.Lchown(name, hdr.Uid, hdr.Gid); err != nil {
		return err
	}
	if err := root.Chmod(name, hdr.FileInfo().Mode()); err != nil {
		return err
	}
	if hdr.Typeflag != tar.TypeDir {
		return root.Chtimes(name, hdr.ModTime, hdr.ModTime)
	}
	return nil
}

// mknod creates a device node or FIFO. os.Root can't, so it uses mknodat on
// the parent directory, opened through the root.
func mknod(root *os.Root, name string, kind uint32, hdr *tar.Header) error {
	dev := unix.Mkdev(uint32(hdr.Devmajor), uint32(hdr.Devminor))
	return inDir(root, path.Dir(name), func(fd int) error {
		return unix.Mknodat(fd, path.Base(name), kind|0o600, int(dev))
	})
}

// whiteout records that the layer deletes parent/name, as overlayfs expects:
// a character device 0:0 with the deleted file's name.
func whiteout(root *os.Root, parent, name string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("bad whiteout name %q", name)
	}
	if err := root.RemoveAll(path.Join(parent, name)); err != nil {
		return err
	}
	return inDir(root, parent, func(fd int) error {
		return unix.Mknodat(fd, name, unix.S_IFCHR, 0)
	})
}

// setOpaque marks a directory opaque for overlayfs. Setting trusted.* xattrs
// needs CAP_SYS_ADMIN.
func setOpaque(root *os.Root, dir string) error {
	return inDir(root, dir, func(fd int) error {
		return unix.Fsetxattr(fd, "trusted.overlay.opaque", []byte("y"), 0)
	})
}

// inDir runs fn on a descriptor of dir, opened through the root.
func inDir(root *os.Root, dir string, fn func(fd int) error) error {
	d, err := root.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	if fi, err := d.Stat(); err != nil || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	return fn(int(d.Fd()))
}
