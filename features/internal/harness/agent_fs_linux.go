//go:build linux

package harness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
)

var secureTemporarySequence atomic.Uint64

func secureMkdirAll(root, name string, mode os.FileMode) error {
	fd, final, err := secureParent(root, name, true, uint32(mode.Perm()))
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Mkdirat(fd, final, uint32(mode.Perm())); err != nil && !errors.Is(err, syscall.EEXIST) {
		return err
	}
	child, err := syscall.Openat(fd, final, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	return syscall.Close(child)
}

func secureWrite(root, name string, data []byte, mode os.FileMode) error {
	fd, final, err := secureParent(root, name, true, 0o700)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	writeMode := mode.Perm()
	existingFD, err := syscall.Openat(fd, final, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err == nil {
		var existing syscall.Stat_t
		if err := syscall.Fstat(existingFD, &existing); err != nil {
			syscall.Close(existingFD)
			return err
		}
		syscall.Close(existingFD)
		if existing.Mode&syscall.S_IFMT != syscall.S_IFREG {
			return fmt.Errorf("edit target %q is not a regular file", name)
		}
		writeMode = os.FileMode(existing.Mode).Perm()
	} else if !errors.Is(err, syscall.ENOENT) {
		return err
	}
	return secureAtomicWriteAt(root, name, fd, final, data, writeMode)
}

func secureRemove(root, name string) error {
	fd, final, err := secureParent(root, name, false, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return syscall.Unlinkat(fd, final)
}

func secureAtomicWrite(root, name string, data []byte, mode os.FileMode) error {
	fd, final, err := secureParent(root, name, true, 0o700)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	return secureAtomicWriteAt(root, name, fd, final, data, mode.Perm())
}

func secureAtomicWriteAt(root, name string, fd int, final string, data []byte, mode os.FileMode) error {
	if err := verifySecureParent(root, name, fd); err != nil {
		return err
	}
	temporary := ".togi-agent-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatUint(secureTemporarySequence.Add(1), 10)
	fileFD, err := syscall.Openat(fd, temporary, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return err
	}
	if err := syscall.Fchmod(fileFD, uint32(mode.Perm())); err != nil {
		syscall.Close(fileFD)
		_ = syscall.Unlinkat(fd, temporary)
		return err
	}
	file := os.NewFile(uintptr(fileFD), temporary)
	cleanup := func() { _ = syscall.Unlinkat(fd, temporary) }
	if err := writeAll(file, data); err != nil {
		file.Close()
		cleanup()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		cleanup()
		return err
	}
	if err := file.Close(); err != nil {
		cleanup()
		return err
	}
	if err := verifySecureParent(root, name, fd); err != nil {
		cleanup()
		return err
	}
	if err := syscall.Renameat(fd, temporary, fd, final); err != nil {
		cleanup()
		return err
	}
	return syscall.Fsync(fd)
}

func verifySecureParent(root, name string, expectedFD int) error {
	actualFD, _, err := secureParent(root, name, false, 0)
	if err != nil {
		return fmt.Errorf("verify confined parent: %w", err)
	}
	defer syscall.Close(actualFD)
	var expected, actual syscall.Stat_t
	if err := syscall.Fstat(expectedFD, &expected); err != nil {
		return err
	}
	if err := syscall.Fstat(actualFD, &actual); err != nil {
		return err
	}
	if expected.Dev != actual.Dev || expected.Ino != actual.Ino {
		return errors.New("workspace parent changed during fixture mutation")
	}
	return nil
}

// withWorkspaceMutation serializes controlled helpers. The fake harness treats
// itself as the sole workspace mutator; an uncooperative same-user process that
// races directory renames is outside this test-fixture threat model. Retained
// descriptors and identity checks still reject changes detected before rename.
func withWorkspaceMutation(root string, mutate func() error) error {
	fd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("workspace already has an active fixture mutator")
		}
		return err
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	return mutate()
}

func secureParent(root, name string, create bool, directoryMode uint32) (int, string, error) {
	clean := filepath.ToSlash(filepath.Clean(name))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || filepath.IsAbs(name) {
		return -1, "", fmt.Errorf("invalid confined path %q", name)
	}
	parts := strings.Split(clean, "/")
	fd, err := syscall.Open(root, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		next, openErr := syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if errors.Is(openErr, syscall.ENOENT) && create {
			if mkdirErr := syscall.Mkdirat(fd, part, directoryMode); mkdirErr != nil && !errors.Is(mkdirErr, syscall.EEXIST) {
				syscall.Close(fd)
				return -1, "", mkdirErr
			}
			next, openErr = syscall.Openat(fd, part, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		}
		if openErr != nil {
			syscall.Close(fd)
			return -1, "", openErr
		}
		syscall.Close(fd)
		fd = next
	}
	return fd, parts[len(parts)-1], nil
}

func writeAll(file *os.File, data []byte) error {
	for len(data) > 0 {
		n, err := file.Write(data)
		if err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}
