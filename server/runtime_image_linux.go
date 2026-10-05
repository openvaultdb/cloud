//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"syscall"
)

func imageEntryFromInfo(info os.FileInfo) (imageEntry, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return imageEntry{}, errors.New("Linux file identity is unavailable")
	}
	return imageEntry{mode: info.Mode(), uid: stat.Uid, links: stat.Nlink, device: uint64(stat.Dev), inode: stat.Ino, size: info.Size()}, nil
}
func imageFileEntry(file *os.File) (imageEntry, error) {
	info, err := file.Stat()
	if err != nil {
		return imageEntry{}, err
	}
	return imageEntryFromInfo(info)
}
func admitProtectedImage() (*protectedImage, error) {
	if os.Getuid() != 65532 || os.Geteuid() != 65532 || os.Getgid() != 65532 || os.Getegid() != 65532 {
		return nil, errors.New("protected-image requires actual UID/GID 65532")
	}
	status, statusErr := os.ReadFile("/proc/self/status")
	mounts, mountsErr := os.ReadFile("/proc/self/mountinfo")
	recordImageDiagnostics(status, mounts, statusErr, mountsErr)
	if statusErr == nil {
		if err := checkImageStatus(status); err != nil {
			return nil, err
		}
	}
	if mountsErr == nil {
		if err := checkImageMounts(mounts); err != nil {
			return nil, err
		}
	}
	inspect := func(path string) (imageEntry, error) {
		info, err := os.Lstat(path)
		if err != nil {
			return imageEntry{}, err
		}
		return imageEntryFromInfo(info)
	}
	names := func(path string) ([]string, error) {
		directory, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		names, readErr := directory.Readdirnames(maxImageEntries + 1)
		closeErr := directory.Close()
		if len(names) > maxImageEntries {
			return nil, errors.New("protected image directory exceeds entry bound")
		}
		// Readdirnames returns EOF only when fewer than n names remain.
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return nil, errors.Join(readErr, closeErr)
		}
		return names, closeErr
	}
	return inspectProtectedTree(protectedFixtureRoot, inspect, names)
}
