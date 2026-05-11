//go:build !linux && !darwin

package main

func lockFile(path string) (func(), error) {
	f, err := openLockFile(path)
	if err != nil {
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}
