package ocm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestRecoverNetworkOnlyRevertsOwnedChanges(t *testing.T) {
	for _, external := range []bool{false, true} {
		name := "owned"
		if external {
			name = "changed externally"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			dns := "192.0.2.30"
			gateway := "192.0.2.1"
			if external {
				dns = "192.0.2.31"
				gateway = "192.0.2.2"
			}
			var changes []string
			transport := Transport{Root: root, command: func(_ context.Context, _, path string, args ...string) (string, error) {
				switch {
				case path == "/usr/sbin/networksetup" && slices.Equal(args, []string{"-getdnsservers", "Wi-Fi"}):
					return dns, nil
				case path == "/usr/sbin/netstat" && slices.Equal(args, []string{"-rn", "-f", "inet"}):
					return "203.0.113.1 " + gateway + " UGHS en0", nil
				default:
					changes = append(changes, path+" "+strings.Join(args, " "))
					return "", nil
				}
			}}
			for file, data := range map[string]any{
				"dns.json":   dnsRecord{"Wi-Fi", nil, "192.0.2.30"},
				"route.json": routeRecord{"203.0.113.1", "192.0.2.1"},
			} {
				if err := writeJSON(filepath.Join(root, file), data); err != nil {
					t.Fatal(err)
				}
			}
			if err := transport.recoverNetwork(context.Background()); err != nil {
				t.Fatal(err)
			}
			if external {
				if len(changes) != 0 {
					t.Fatalf("modified externally changed network state: %v", changes)
				}
			} else {
				want := []string{
					"/usr/sbin/networksetup -setdnsservers Wi-Fi empty",
					"/sbin/route -n delete -host 203.0.113.1 192.0.2.1",
				}
				if !slices.Equal(changes, want) {
					t.Fatalf("network changes = %v, want %v", changes, want)
				}
			}
			for _, file := range []string{"dns.json", "route.json"} {
				if _, err := os.Stat(filepath.Join(root, file)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("ownership record %s was retained: %v", file, err)
				}
			}
		})
	}
}

func TestRoamingWakeAndHealthRequestReconnect(t *testing.T) {
	for _, test := range []struct {
		name, previous, current string
		awake, unhealthy, want  bool
	}{
		{"steady uplink", "en0/a/g", "en0/a/g", false, false, false},
		{"new router", "en0/a/g", "en0/a/h", false, false, true},
		{"new interface", "en0/a/g", "en1/a/g", false, false, true},
		{"wake", "en0/a/g", "en0/a/g", true, false, true},
		{"unhealthy tunnel", "en0/a/g", "en0/a/g", false, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := reconnectNeeded(test.previous, test.current, test.awake, test.unhealthy); got != test.want {
				t.Fatalf("reconnect = %t, want %t", got, test.want)
			}
		})
	}
}
