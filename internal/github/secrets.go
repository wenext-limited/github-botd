package github

import (
	"io"
	"os"
)

// readSecretFile rejects symlinks, devices, shared file modes, changed file
// identities and oversized files. Error messages deliberately omit paths/data.
func readSecretFile(path string, limit int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0077 != 0 || !secretOwnedByCurrentUser(before) {
		return nil, failure("credential file must be a private owned regular file")
	}
	f, err := openSecretFile(path)
	if err != nil {
		return nil, failure("cannot open credential file")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || !secretOwnedByCurrentUser(info) || !os.SameFile(before, info) {
		return nil, failure("credential file changed or has unsafe permissions")
	}
	if info.Size() > limit {
		return nil, failure("credential file exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(data)) > limit {
		return nil, failure("cannot read bounded credential file")
	}
	return data, nil
}
