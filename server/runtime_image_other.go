//go:build !linux

package main

import (
	"errors"
	"os"
)

func admitProtectedImage() (*protectedImage, error) {
	return nil, errors.New("protected-image requires Linux; choose explicit sealed-copy for development")
}
func imageFileEntry(_ *os.File) (imageEntry, error) {
	return imageEntry{}, errors.New("protected-image requires Linux file identity")
}
