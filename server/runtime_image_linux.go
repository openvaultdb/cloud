//go:build linux

package main

import (
	"errors"
	"io"
	"log"
	"os"
	"strings"
	"syscall"
)

func imageEntryFromInfo(info os.FileInfo) (imageEntry, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return imageEntry{}, errors.New("linux file identity is unavailable")
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

func recordImageDiagnostics(status, mounts []byte, statusErr, mountsErr error) {
	log.Printf("protected-image kernel observations: status_available=%t mountinfo_available=%t", statusErr == nil, mountsErr == nil)
	if statusErr == nil {
		for _, line := range strings.Split(string(status), "\n") {
			if strings.HasPrefix(line, "Uid:") || strings.HasPrefix(line, "Gid:") || strings.HasPrefix(line, "Groups:") || strings.HasPrefix(line, "Cap") || strings.HasPrefix(line, "NoNewPrivs:") {
				log.Printf("protected-image %s", line)
			}
		}
	}
	if mountsErr == nil {
		log.Printf("protected-image mountinfo: %d bytes inspected", len(mounts))
	}
}
