package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/nanzhi84/Rushes/go/internal/storage"
)

type ObjectRef struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
	Path string `json:"path"`
}

type ObjectStore struct {
	paths storage.Paths
}

func NewObjectStore(paths storage.Paths) ObjectStore {
	return ObjectStore{paths: paths}
}

func (store ObjectStore) PutFile(ctx context.Context, source string) (ObjectRef, error) {
	file, err := os.Open(source)
	if err != nil {
		return ObjectRef{}, err
	}
	defer func() { _ = file.Close() }()
	return store.Put(ctx, file)
}

func (store ObjectStore) PutBytes(ctx context.Context, data []byte) (ObjectRef, error) {
	return store.Put(ctx, bytes.NewReader(data))
}

func (store ObjectStore) Put(ctx context.Context, reader io.Reader) (ObjectRef, error) {
	if err := store.paths.Initialize(); err != nil {
		return ObjectRef{}, err
	}
	temporary, err := os.CreateTemp(store.paths.Temporary, "object-*")
	if err != nil {
		return ObjectRef{}, err
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		_ = temporary.Close()
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()

	hasher := sha256.New()
	size, err := copyWithContext(ctx, io.MultiWriter(temporary, hasher), reader)
	if err != nil {
		return ObjectRef{}, err
	}
	if err := temporary.Sync(); err != nil {
		return ObjectRef{}, err
	}
	if err := temporary.Close(); err != nil {
		return ObjectRef{}, err
	}
	hash := hex.EncodeToString(hasher.Sum(nil))
	destination, err := store.paths.ObjectPath(hash)
	if err != nil {
		return ObjectRef{}, err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return ObjectRef{}, err
	}
	if _, err := os.Stat(destination); err == nil {
		return ObjectRef{Hash: hash, Size: size, Path: destination}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return ObjectRef{}, err
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		return ObjectRef{}, err
	}
	keepTemporary = false
	return ObjectRef{Hash: hash, Size: size, Path: destination}, nil
}

func copyWithContext(ctx context.Context, writer io.Writer, reader io.Reader) (int64, error) {
	buffer := make([]byte, 1024*1024)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		read, readErr := reader.Read(buffer)
		if read > 0 {
			written, writeErr := writer.Write(buffer[:read])
			total += int64(written)
			if writeErr != nil {
				return total, writeErr
			}
			if written != read {
				return total, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return total, nil
		}
		if readErr != nil {
			return total, readErr
		}
	}
}
