package repository

import "io"

type Storage interface {
	Unzip(zipFile, dst string) error
	Save(path string, r io.Reader) error
}

type IDGenerator interface {
	NewID() string
}
