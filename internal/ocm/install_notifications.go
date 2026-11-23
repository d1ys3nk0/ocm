package ocm

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/d1ys3nk0/ocm/internal/desktopnotify"
)

func (i installation) notifierPath() string {
	return filepath.Join(i.packageDir, desktopnotify.BundleName)
}

func (i installation) notifierDirs() []string {
	root := i.notifierPath()
	return []string{root, filepath.Join(root, "Contents"), filepath.Join(root, "Contents", "MacOS"),
		filepath.Join(root, "Contents", "_CodeSignature")}
}

func (i installation) inspectNotifier(record installRecord) error {
	directories := i.notifierDirs()
	resources := desktopnotify.Resources()
	for _, dir := range directories {
		if err := i.secure(dir, true, 0755); err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if len(record.Notifier) == 0 {
			return fmt.Errorf("unrelated notification app: %s", dir)
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			known := slices.Contains(directories, path) || slices.ContainsFunc(resources, func(r desktopnotify.Resource) bool {
				return path == filepath.Join(i.notifierPath(), r.Name)
			})
			if !known {
				return fmt.Errorf("unrelated notification resource: %s", path)
			}
		}
	}
	for _, resource := range resources {
		path := filepath.Join(i.notifierPath(), resource.Name)
		if err := i.secure(path, false, resource.Mode); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !slices.Contains(record.Notifier[resource.Name], hashBytes(data)) {
			return fmt.Errorf("unrelated notification resource: %s", path)
		}
	}
	return nil
}

func (i installation) notifierMatches(payload map[string][]byte) bool {
	for _, resource := range desktopnotify.Resources() {
		data, err := os.ReadFile(filepath.Join(i.notifierPath(), resource.Name))
		if err != nil || !bytes.Equal(data, payload[resource.Name]) {
			return false
		}
	}
	return true
}

func (r *installRecord) rememberNotifier(payload map[string][]byte) {
	if r.Notifier == nil {
		r.Notifier = make(map[string][]string)
	}
	for _, resource := range desktopnotify.Resources() {
		hash := hashBytes(payload[resource.Name])
		if !slices.Contains(r.Notifier[resource.Name], hash) {
			r.Notifier[resource.Name] = append(r.Notifier[resource.Name], hash)
		}
	}
}

func (i installation) installNotifier(payload map[string][]byte) error {
	for _, directory := range i.notifierDirs() {
		if err := makeInstallDirectory(directory, 0755); err != nil {
			return err
		}
	}
	for _, resource := range desktopnotify.Resources() {
		if err := atomicInstall(filepath.Join(i.notifierPath(), resource.Name), payload[resource.Name], resource.Mode); err != nil {
			return err
		}
	}
	return nil
}

func (i installation) removeNotifier() error {
	for _, resource := range desktopnotify.Resources() {
		if err := os.Remove(filepath.Join(i.notifierPath(), resource.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, directory := range slices.Backward(i.notifierDirs()) {
		if err := os.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
