package ocm

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/d1ys3nk0/ocm/internal/desktopnotify"
)

func (i installation) appPath() string {
	return filepath.Join(i.applicationsDir, desktopnotify.BundleName)
}

func (i installation) legacyAppPath() string {
	return filepath.Join(i.packageDir, desktopnotify.BundleName)
}

func (i installation) hasLegacyApp() bool {
	_, err := os.Lstat(i.legacyAppPath())
	return err == nil
}

func (i installation) inspectApps(record installRecord) error {
	info, err := os.Lstat(i.applicationsDir)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != i.uid || !info.IsDir() || info.Mode().Perm()&0002 != 0 {
		return fmt.Errorf("unsafe applications directory: %s", i.applicationsDir)
	}
	if err := i.inspectApp(i.appPath(), record.Application); err != nil {
		return err
	}
	return i.inspectApp(i.legacyAppPath(), record.Notifier)
}

func appBundleDirs(root string) []string {
	return []string{root, filepath.Join(root, "Contents"), filepath.Join(root, "Contents", "MacOS"),
		filepath.Join(root, "Contents", "_CodeSignature")}
}

func (i installation) inspectApp(root string, hashes map[string][]string) error {
	directories := appBundleDirs(root)
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
		if len(hashes) == 0 {
			return fmt.Errorf("unrelated notification app: %s", dir)
		}
		for _, entry := range entries {
			path := filepath.Join(dir, entry.Name())
			known := slices.Contains(directories, path) || slices.ContainsFunc(resources, func(r desktopnotify.Resource) bool {
				return path == filepath.Join(root, r.Name)
			})
			if !known {
				return fmt.Errorf("unrelated notification resource: %s", path)
			}
		}
	}
	for _, resource := range resources {
		path := filepath.Join(root, resource.Name)
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
		if !slices.Contains(hashes[resource.Name], hashBytes(data)) {
			return fmt.Errorf("unrelated notification resource: %s", path)
		}
	}
	return nil
}

func (i installation) appMatches(payload map[string][]byte) bool {
	for _, resource := range desktopnotify.Resources() {
		data, err := os.ReadFile(filepath.Join(i.appPath(), resource.Name))
		if err != nil || !bytes.Equal(data, payload[resource.Name]) {
			return false
		}
	}
	return true
}

func (r *installRecord) rememberApp(payload map[string][]byte) {
	if r.Application == nil {
		r.Application = make(map[string][]string)
	}
	for _, resource := range desktopnotify.Resources() {
		hash := hashBytes(payload[resource.Name])
		if !slices.Contains(r.Application[resource.Name], hash) {
			r.Application[resource.Name] = append(r.Application[resource.Name], hash)
		}
	}
}

func installApp(root string, payload map[string][]byte) error {
	for _, directory := range appBundleDirs(root) {
		if err := makeInstallDirectory(directory, 0755); err != nil {
			return err
		}
	}
	for _, resource := range desktopnotify.Resources() {
		if err := atomicInstall(filepath.Join(root, resource.Name), payload[resource.Name], resource.Mode); err != nil {
			return err
		}
	}
	return nil
}

func removeApp(root string) error {
	for _, resource := range desktopnotify.Resources() {
		if err := os.Remove(filepath.Join(root, resource.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	for _, directory := range slices.Backward(appBundleDirs(root)) {
		if err := os.Remove(directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
