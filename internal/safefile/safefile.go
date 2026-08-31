// Package safefile limits application-managed file access to a trusted root.
package safefile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// OpenFile opens candidate only when it is contained by rootDirectory. The
// returned path is absolute and can be retained for later operations on the
// already-authorized application file.
func OpenFile(rootDirectory, candidate string, flag int, permission os.FileMode) (*os.File, string, error) {
	rootPath, relativePath, absolutePath, err := resolve(rootDirectory, candidate)
	if err != nil {
		return nil, "", err
	}
	if err := os.MkdirAll(rootPath, 0o700); err != nil {
		return nil, "", fmt.Errorf("create managed file root: %w", err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, "", fmt.Errorf("open managed file root: %w", err)
	}
	defer root.Close()
	if directory := filepath.Dir(relativePath); directory != "." {
		if err := root.MkdirAll(directory, 0o700); err != nil {
			return nil, "", fmt.Errorf("create managed file directory: %w", err)
		}
	}
	file, err := root.OpenFile(relativePath, flag, permission)
	if err != nil {
		return nil, "", err
	}
	return file, absolutePath, nil
}

// ReadFile reads candidate only when it is contained by rootDirectory.
func ReadFile(rootDirectory, candidate string) ([]byte, error) {
	rootPath, relativePath, _, err := resolve(rootDirectory, candidate)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open managed file root: %w", err)
	}
	defer root.Close()
	return root.ReadFile(relativePath)
}

func resolve(rootDirectory, candidate string) (string, string, string, error) {
	rootDirectory = strings.TrimSpace(rootDirectory)
	candidate = strings.TrimSpace(candidate)
	if rootDirectory == "" {
		return "", "", "", errors.New("managed file root cannot be empty")
	}
	if candidate == "" {
		return "", "", "", errors.New("managed file path cannot be empty")
	}
	rootPath, err := filepath.Abs(filepath.Clean(rootDirectory))
	if err != nil {
		return "", "", "", fmt.Errorf("resolve managed file root: %w", err)
	}
	absolutePath, err := filepath.Abs(filepath.Clean(candidate))
	if err != nil {
		return "", "", "", fmt.Errorf("resolve managed file path: %w", err)
	}
	relativePath, err := filepath.Rel(rootPath, absolutePath)
	if err != nil {
		return "", "", "", fmt.Errorf("compare managed file path: %w", err)
	}
	if relativePath == "." || relativePath == ".." || filepath.IsAbs(relativePath) || strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return "", "", "", fmt.Errorf("managed file path %q must be inside %q", candidate, rootDirectory)
	}
	return rootPath, relativePath, absolutePath, nil
}
