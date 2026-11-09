package ocm

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func wait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("session did not transition")
	}
}

func TestSavedProfileSurvivesDaemonRestartAndStopCancels(t *testing.T) {
	root := t.TempDir()
	newDaemon := func(started, cancelled chan struct{}) *daemon {
		saved, err := readIntent(root)
		if err != nil {
			t.Fatal(err)
		}
		d := &daemon{
			ctx: context.Background(), root: root, status: Status{State: "stopped", SavedProfile: saved},
			load: func(name string) (Profile, error) {
				if name != "office" {
					return Profile{}, errors.New("unexpected profile")
				}
				return Profile{}, nil
			},
			run: func(ctx context.Context, _ Profile, state func(string)) error {
				state("connecting")
				close(started)
				<-ctx.Done()
				close(cancelled)
				return ctx.Err()
			},
		}
		if saved != "" {
			d.launch(saved, true)
		}
		return d
	}
	started, cancelled := make(chan struct{}), make(chan struct{})
	first := newDaemon(started, cancelled)
	if _, _, err := first.handle(Request{Action: "start", Profile: "office"}); err != nil {
		t.Fatal(err)
	}
	wait(t, started)
	first.stopWorker()
	wait(t, cancelled)

	started, cancelled = make(chan struct{}), make(chan struct{})
	second := newDaemon(started, cancelled)
	wait(t, started)
	if got := second.snapshot(); got.SavedProfile != "office" || got.State != "connecting" {
		t.Fatalf("restored status = %+v", got)
	}
	if _, _, err := second.handle(Request{Action: "stop"}); err != nil {
		t.Fatal(err)
	}
	wait(t, cancelled)
	if saved, err := readIntent(root); err != nil || saved != "" {
		t.Fatalf("saved state after stop = %q, %v", saved, err)
	}
}

func TestForegroundDoesNotPersistAndAuthFailureStopsRetry(t *testing.T) {
	root := t.TempDir()
	d := &daemon{
		ctx: context.Background(), root: root, status: Status{State: "stopped"},
		load: func(string) (Profile, error) { return Profile{}, nil },
		run:  func(context.Context, Profile, func(string)) error { return errAuthentication },
	}
	_, done, err := d.handle(Request{Action: "connect", Profile: "office"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, done)
	if got := d.snapshot(); got.State != "authentication-required" || got.SavedProfile != "" {
		t.Fatalf("foreground status = %+v", got)
	}
	if _, err := os.Stat(filepath.Join(root, "desired.json")); !os.IsNotExist(err) {
		t.Fatalf("foreground wrote state: %v", err)
	}
	_, done, err = d.handle(Request{Action: "start", Profile: "office"})
	if err != nil {
		t.Fatal(err)
	}
	wait(t, done)
	if got := d.snapshot(); got.State != "authentication-required" || got.SavedProfile != "office" {
		t.Fatalf("persistent status = %+v", got)
	}
}

func TestProfileTuningAndPasswordSeparation(t *testing.T) {
	profile, err := parseProfile(bufio.NewScanner(strings.NewReader(
		"OC_HOST=vpn.example\nOC_USER=alice\nOC_PASS=synthetic=password\nOC_GROUP=staff\nOC_MTU=1400\nOC_COMPRESSION=none\nOC_DTLS_LOCAL_PORT=4443",
	)), "/Library/OCM/bin/ocm-hook")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(profile.args, " ")
	for _, flag := range []string{"--mtu=1400", "--no-deflate", "--dtls-local-port=4443", "--reconnect-timeout=300", "--force-dpd=30"} {
		if !strings.Contains(args, flag) {
			t.Errorf("missing %s", flag)
		}
	}
	if profile.password != "synthetic=password" || strings.Contains(args, profile.password) {
		t.Fatal("password did not stay separate from arguments")
	}
}

func TestRouteIgnoresClonesAndRejectsAmbiguity(t *testing.T) {
	const explicit = "203.0.113.1/32 192.0.2.1 UGHS en0"
	const clone = "203.0.113.1 192.0.2.3 UHW en1"
	want := routeRecord{"203.0.113.1", "192.0.2.1"}
	if got, err := findRoute(clone+"\n"+explicit, want.Destination); err != nil || got != want {
		t.Fatalf("route = %+v, %v", got, err)
	}
	if _, err := findRoute(explicit+"\n"+explicit, want.Destination); err == nil {
		t.Fatal("accepted ambiguous route")
	}
}
