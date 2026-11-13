package ocm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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
	_, done, err := d.handle(Request{Action: "run", Profile: "office"})
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

func TestStartAndToggleRequireStoppingBeforeProfileChange(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{}, 2)
	d := &daemon{
		ctx: context.Background(), root: root, status: Status{State: "stopped"},
		load: func(string) (Profile, error) { return Profile{}, nil },
		run: func(ctx context.Context, _ Profile, state func(string)) error {
			state("connecting")
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		},
	}
	if _, _, err := d.handle(Request{Action: "start", Profile: "office"}); err != nil {
		t.Fatal(err)
	}
	wait(t, started)
	if _, _, err := d.handle(Request{Action: "start", Profile: "office"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
		t.Fatal("repeated start restarted the session")
	default:
	}
	for _, action := range []string{"start", "toggle"} {
		if _, _, err := d.handle(Request{Action: action, Profile: "other"}); err == nil {
			t.Fatalf("%s accepted a different profile", action)
		}
		if got := d.snapshot(); got.SavedProfile != "office" || got.Profile != "office" {
			t.Fatalf("%s changed the existing session: %+v", action, got)
		}
	}
	if _, _, err := d.handle(Request{Action: "stop"}); err != nil {
		t.Fatal(err)
	}
}

func TestProfileTuningAndPasswordSeparation(t *testing.T) {
	profile, err := parseProfile(bufio.NewScanner(strings.NewReader(
		"OC_HOST=vpn.example\nOC_USER=alice\nOC_PASS=synthetic=password\nOC_GROUP=staff\nOC_MTU=1400\nOC_COMPRESSION=none\nOC_DTLS_LOCAL_PORT=4443",
	)), "/opt/ocm/bin/ocm-hook")
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

func TestForegroundSocketAndExclusiveSession(t *testing.T) {
	for _, externalStop := range []bool{true, false} {
		t.Run(fmt.Sprintf("external-stop-%t", externalStop), func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "ocm-test-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(root) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			started := make(chan struct{})
			d := &daemon{
				ctx: ctx, root: root, foreground: true, status: Status{State: "stopped"},
				load: func(string) (Profile, error) { return Profile{}, nil },
				run: func(ctx context.Context, _ Profile, state func(string)) error {
					state("connected")
					close(started)
					<-ctx.Done()
					return ctx.Err()
				},
			}
			ready := make(chan error, 1)
			done := make(chan error, 1)
			go func() { done <- foregroundSession(ctx, d, "office", ready) }()
			if err := <-ready; err != nil {
				t.Fatal(err)
			}
			wait(t, started)
			other := &daemon{ctx: ctx, root: root, status: Status{State: "stopped"}, load: d.load, run: d.run}
			if _, _, err := other.handle(Request{Action: "start", Profile: "other"}); err == nil {
				t.Fatal("background session acquired an owned foreground session")
			}
			if saved, err := readIntent(root); err != nil || saved != "" {
				t.Fatalf("conflicting start changed desired state: %q, %v", saved, err)
			}
			if externalStop {
				conn, err := net.Dial("unix", filepath.Join(root, "foreground.sock"))
				if err != nil {
					t.Fatal(err)
				}
				if err := json.NewEncoder(conn).Encode(Request{Action: "stop"}); err != nil {
					t.Fatal(err)
				}
				var response map[string]any
				if err := json.NewDecoder(conn).Decode(&response); err != nil {
					t.Fatal(err)
				}
				conn.Close()
				if response["state"] != "stopped" || response["error"] != "" {
					t.Fatalf("stop response = %#v", response)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if externalStop && err != nil || !externalStop && !errors.Is(err, context.Canceled) {
					t.Fatalf("foreground result = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("foreground did not exit")
			}
			if saved, err := readIntent(root); err != nil || saved != "" {
				t.Fatalf("foreground persisted intent: %q, %v", saved, err)
			}
		})
	}
}

func TestFailedRecoveryReportedByStop(t *testing.T) {
	root := t.TempDir()
	started := make(chan struct{})
	d := &daemon{
		ctx: context.Background(), root: root, status: Status{State: "stopped"},
		load: func(string) (Profile, error) { return Profile{}, nil },
		run: func(ctx context.Context, _ Profile, _ func(string)) error {
			close(started)
			<-ctx.Done()
			return errors.Join(ctx.Err(), errRecovery)
		},
	}
	if _, _, err := d.handle(Request{Action: "start", Profile: "office"}); err != nil {
		t.Fatal(err)
	}
	wait(t, started)
	if _, _, err := d.handle(Request{Action: "stop"}); err == nil {
		t.Fatal("stop concealed a recovery failure")
	}
	if saved, err := readIntent(root); err != nil || saved != "" {
		t.Fatalf("stop did not clear intent: %q, %v", saved, err)
	}
	if got := d.snapshot(); got.State != "failed" || got.Error == "" {
		t.Fatalf("recovery failure status = %+v", got)
	}
}

func TestStopReportsRecoveryFailureDuringRetry(t *testing.T) {
	root := t.TempDir()
	attempt := make(chan struct{})
	d := &daemon{
		ctx: context.Background(), root: root, status: Status{State: "stopped"},
		load: func(string) (Profile, error) { return Profile{}, nil },
		run: func(context.Context, Profile, func(string)) error {
			close(attempt)
			return errRecovery
		},
	}
	if _, _, err := d.handle(Request{Action: "start", Profile: "office"}); err != nil {
		t.Fatal(err)
	}
	wait(t, attempt)
	deadline := time.Now().Add(time.Second)
	for d.snapshot().Error == "" && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, _, err := d.handle(Request{Action: "stop"}); err == nil {
		t.Fatal("stop concealed the failed recovery during retry")
	}
}

func TestStatusJSONDistinguishesTunnelFromConnectivity(t *testing.T) {
	data, err := json.Marshal(Status{State: "connected", Profile: "office", SavedProfile: "office"})
	if err != nil {
		t.Fatal(err)
	}
	var status struct {
		State          string            `json:"state"`
		Mode           string            `json:"mode"`
		Profile        string            `json:"profile"`
		DesiredProfile string            `json:"desired_profile"`
		Health         string            `json:"health"`
		Error          string            `json:"error"`
		Details        map[string]string `json:"details"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		t.Fatal(err)
	}
	if status.State != "running" || status.Mode != "background" || status.Profile != "office" || status.DesiredProfile != "office" || status.Health != "unknown" || status.Error != "" || status.Details["stage"] != "connected" {
		t.Fatalf("status JSON = %s", data)
	}
}

func TestSessionLockRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "unrelated")
	if err := os.WriteFile(target, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(root, "session.lock")); err != nil {
		t.Fatal(err)
	}
	if lock, err := lockSession(context.Background(), root); err == nil {
		lock.Close()
		t.Fatal("accepted a symlinked session lock")
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "keep" {
		t.Fatalf("unrelated file changed: %q, %v", got, err)
	}
}

func TestProfileNamesExcludeFlagLikeNames(t *testing.T) {
	for _, name := range []string{"office", "_office", "office-west", "A1"} {
		if !ValidProfileName(name) {
			t.Errorf("rejected valid profile %q", name)
		}
	}
	for _, name := range []string{"", "-office", "--help", "office.example", "../office"} {
		if ValidProfileName(name) {
			t.Errorf("accepted invalid profile %q", name)
		}
	}
}
