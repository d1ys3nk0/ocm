package ocm

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

func ownedByRoot(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func CheckRoot(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ownedByRoot(info) {
		return fmt.Errorf("state directory must be root-owned with mode 0700")
	}
	return nil
}

func writeState(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".ocm-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}

func writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeState(path, append(data, '\n'))
}

type intent struct {
	Profile string `json:"profile"`
}

func readIntent(root string) (string, error) {
	var state intent
	err := readJSON(filepath.Join(root, "desired.json"), &state)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil || (state.Profile != "" && !profileName.MatchString(state.Profile)) {
		return "", fmt.Errorf("invalid saved connection state")
	}
	return state.Profile, nil
}
