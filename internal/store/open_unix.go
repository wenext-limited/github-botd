//go:build unix

package store

import (
	"fmt"
	bolt "go.etcd.io/bbolt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"time"
)

// Opening relative to an owned private directory descriptor prevents a rename
// or symlink swap from redirecting the database open. Existing permissions are
// never silently changed. Ancestor symlinks (e.g. macOS /var) are allowed; the
// actual state directory and database leaf must not be symlinks.
func openDatabase(dir string) (*bolt.DB, func() error, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	dir = filepath.Clean(dir)
	fd, err := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open private state directory: %w", err)
	}
	directory := os.NewFile(uintptr(fd), dir)
	success := false
	defer func() {
		if !success {
			directory.Close()
		}
	}()
	var info unix.Stat_t
	if err = unix.Fstat(fd, &info); err != nil {
		return nil, nil, err
	}
	if info.Uid != uint32(os.Geteuid()) || info.Mode&0077 != 0 {
		return nil, nil, fmt.Errorf("state directory must be owned by this user with mode 0700 or stricter")
	}
	open := func(path string, flags int, mode os.FileMode) (*os.File, error) {
		if path != filepath.Join(dir, "github-botd.db") || flags&os.O_TRUNC != 0 {
			return nil, fmt.Errorf("unexpected database open target or flags")
		}
		child, err := unix.Openat(fd, "github-botd.db", flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
		if err != nil {
			return nil, err
		}
		var stat unix.Stat_t
		if err = unix.Fstat(child, &stat); err != nil {
			unix.Close(child)
			return nil, err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0077 != 0 || stat.Nlink != 1 {
			unix.Close(child)
			return nil, fmt.Errorf("database must be a private, user-owned regular file without hard links")
		}
		return os.NewFile(uintptr(child), filepath.Join(dir, "github-botd.db")), nil
	}
	db, err := bolt.Open(filepath.Join(dir, "github-botd.db"), 0600, &bolt.Options{Timeout: time.Second, OpenFile: open})
	if err != nil {
		return nil, nil, err
	}
	success = true
	return db, directory.Close, nil
}
