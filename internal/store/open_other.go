//go:build !unix

package store

import (
	"errors"
	bolt "go.etcd.io/bbolt"
)

// The daemon currently supports Unix hosts. Fail closed where its private-state
// directory and no-follow descriptor checks are unavailable.
func openDatabase(string) (*bolt.DB, func() error, error) {
	return nil, nil, errors.New("durable store requires a supported Unix host")
}
