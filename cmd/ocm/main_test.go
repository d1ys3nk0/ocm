package main

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandLineEntrypoint(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "ocm")
	build := exec.Command("go", "build", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, output)
	}
	for _, test := range []struct {
		args []string
		code int
		want string
	}{
		{[]string{"--help"}, 0, "run NAME"},
		{[]string{"--version"}, 0, "ocm dev"},
		{[]string{"connect", "office"}, 2, "Usage:"},
		{[]string{"office", "start"}, 2, "Usage:"},
		{[]string{"status", "--unknown"}, 2, "Usage:"},
		{[]string{"start", "../office"}, 2, "Usage:"},
	} {
		cmd := exec.Command(binary, test.args...)
		output, err := cmd.CombinedOutput()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatalf("%v: %v", test.args, err)
			}
			code = exit.ExitCode()
		}
		if code != test.code || !strings.Contains(string(output), test.want) {
			t.Errorf("%v: exit %d output %q", test.args, code, output)
		}
	}
}

func TestProfileNameParsing(t *testing.T) {
	for _, name := range []string{"_office", "office-west"} {
		if _, err := parse([]string{"start", name}); err != nil {
			t.Errorf("start %q: %v", name, err)
		}
	}
	for _, name := range []string{"-office", "office.example"} {
		if _, err := parse([]string{"start", name}); err == nil {
			t.Errorf("accepted start %q", name)
		}
	}
}
