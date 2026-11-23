package desktopnotify

import (
	"archive/zip"
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"io/fs"
)

//go:embed all:assets
var assets embed.FS

const BundleName = "OCM.app"
const ApplicationsDir = "/Applications"
const bundlePath = ApplicationsDir + "/" + BundleName

type Resource struct {
	Name string
	Mode fs.FileMode
}

func Resources() []Resource {
	return []Resource{
		{Name: "Contents/Info.plist", Mode: 0644},
		{Name: "Contents/MacOS/notifier", Mode: 0755},
		{Name: "Contents/_CodeSignature/CodeResources", Mode: 0644},
	}
}

func Bundle() (map[string][]byte, error) {
	data, err := assets.ReadFile("assets/notifier.zip")
	if err != nil {
		return nil, errors.New("native notification app missing from this build; use a release binary or run scripts/build-app.sh before building")
	}
	return readBundle(data)
}

func readBundle(data []byte) (map[string][]byte, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("invalid native notification app: %w", err)
	}
	resources := Resources()
	if len(archive.File) != len(resources) {
		return nil, errors.New("unexpected contents of native notification app")
	}
	payload := make(map[string][]byte, len(resources))
	for _, resource := range resources {
		file, err := archive.Open(resource.Name)
		if err != nil {
			return nil, fmt.Errorf("missing notification resource: %w", err)
		}
		const maxResourceSize = 4 << 20
		data, readErr := io.ReadAll(io.LimitReader(file, maxResourceSize+1))
		if err := errors.Join(readErr, file.Close()); err != nil {
			return nil, err
		}
		if len(data) == 0 || len(data) > maxResourceSize {
			return nil, errors.New("invalid size of native notification resource")
		}
		payload[resource.Name] = data
	}
	return payload, nil
}
