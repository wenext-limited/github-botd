//go:build !unix

package github

import "os"

// Non-Unix systems still enforce the common regular-file/mode/identity checks.
func secretOwnedByCurrentUser(os.FileInfo) bool    { return true }
func openSecretFile(path string) (*os.File, error) { return os.Open(path) }
