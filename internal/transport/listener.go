package transport

import (
	"errors"
	"fmt"
	"github.com/wenext-limited/github-botd/internal/config"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ListenUnix never unlinks regular files or active sockets. The enclosing directory
// must be private, preventing another OS user from swapping the path during setup.
func ListenUnix(path string) (net.Listener, error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("IPC socket parent must be an owner-only directory")
	}
	if s, ok := info.Sys().(*syscall.Stat_t); ok && int(s.Uid) != os.Getuid() {
		return nil, errors.New("IPC socket directory must belong to current user")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, errors.New("refusing to replace non-socket IPC path")
		}
		conn, dialErr := net.DialTimeout("unix", path, 200*time.Millisecond)
		if dialErr == nil {
			conn.Close()
			return nil, errors.New("IPC socket already active")
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
			return nil, fmt.Errorf("cannot verify stale IPC socket: %w", dialErr)
		}
		if err = os.Remove(path); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}
func ListenLoopback(bind string) (net.Listener, error) {
	if err := config.Loopback(bind); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		return nil, err
	}
	a, ok := ln.Addr().(*net.TCPAddr)
	if !ok || !a.IP.IsLoopback() {
		ln.Close()
		return nil, errors.New("actual IPC listener is not loopback")
	}
	return ln, nil
}
