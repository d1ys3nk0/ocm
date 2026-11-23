package ocm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestPingProfile(t *testing.T) {
	for _, tc := range []struct {
		name, config, invalid string
		timeout, interval     time.Duration
	}{
		{name: "disabled"},
		{"defaults", "OC_HEALTH_PING=192.168.0.2\n", "", 3 * time.Second, 30 * time.Second},
		{"overrides", "OC_HEALTH_PING=192.168.0.2\nOC_HEALTH_TIMEOUT=12s\nOC_HEALTH_INTERVAL=20s\n", "", 12 * time.Second, 20 * time.Second},
		{name: "DNS name", config: "OC_HEALTH_PING=example.com\n", invalid: "OC_HEALTH_PING"},
		{name: "IPv6", config: "OC_HEALTH_PING=::1\n", invalid: "OC_HEALTH_PING"},
		{name: "empty target", config: "OC_HEALTH_PING=\n", invalid: "OC_HEALTH_PING"},
		{name: "missing target", config: "OC_HEALTH_TIMEOUT=3s\n", invalid: "OC_HEALTH_PING"},
		{name: "fraction", config: "OC_HEALTH_PING=192.168.0.2\nOC_HEALTH_TIMEOUT=0.5s\n", invalid: "OC_HEALTH_TIMEOUT"},
		{name: "zero", config: "OC_HEALTH_PING=192.168.0.2\nOC_HEALTH_INTERVAL=0s\n", invalid: "OC_HEALTH_INTERVAL"},
		{name: "no unit", config: "OC_HEALTH_PING=192.168.0.2\nOC_HEALTH_INTERVAL=30\n", invalid: "OC_HEALTH_INTERVAL"},
		{name: "long timeout", config: "OC_HEALTH_PING=192.168.0.2\nOC_HEALTH_TIMEOUT=2147484s\nOC_HEALTH_INTERVAL=2147485s\n", invalid: "OC_HEALTH_TIMEOUT"},
		{name: "overlap", config: "OC_HEALTH_PING=192.168.0.2\nOC_HEALTH_INTERVAL=3s\n", invalid: "OC_HEALTH_TIMEOUT"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile, err := parseProfile(bufio.NewScanner(strings.NewReader("OC_HOST=example.com\nOC_USER=test\nOC_PASS=fixture\n"+tc.config)), "/hook")
			if tc.invalid != "" {
				if err == nil || !strings.Contains(err.Error(), tc.invalid) {
					t.Fatalf("error = %v, want %s validation", err, tc.invalid)
				}
				return
			}
			if err != nil || profile.health.timeout != tc.timeout || profile.health.interval != tc.interval || profile.health.target.IsValid() != (tc.interval != 0) {
				t.Fatalf("ping policy = %+v, error = %v", profile.health, err)
			}
			for _, arg := range profile.args {
				if strings.Contains(arg, "192.168.0.2") {
					t.Fatal("OCM health configuration passed to OpenConnect")
				}
			}
		})
	}
}

func testTunnelLink(t *testing.T, device string) tunnelLink {
	t.Helper()
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return tunnelLink{device: device, marker: info}
}

func TestPingDeadlineAndTunnelReplacement(t *testing.T) {
	old, replacement := testTunnelLink(t, "utun8"), testTunnelLink(t, "utun8")
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls int
		transport := Transport{command: func(ctx context.Context, input, path string, args ...string) (string, error) {
			want := []string{"-n", "-b", "utun8", "-c", "1", "-W", "12000", "192.168.0.2"}
			if input != "" || path != "/sbin/ping" || !slices.Equal(args, want) {
				t.Errorf("ping command = %q %q, stdin = %q", path, args, input)
			}
			if deadline, ok := ctx.Deadline(); !ok || deadline.Sub(time.Now()) != 12*time.Second {
				t.Errorf("probe deadline = %v, set = %t", deadline, ok)
			}
			calls++
			<-ctx.Done()
			// Simulate a reply racing cancellation.
			return "", nil
		}}
		h := tunnelHealth{policy: pingPolicy{netip.MustParseAddr("192.168.0.2"), 12 * time.Second, 30 * time.Second}, ping: transport.ping}
		h.observe(ctx, old, time.Now())
		synctest.Wait()
		if !h.observe(ctx, replacement, time.Now()) {
			t.Fatal("did not recognize replacement on the same utun")
		}
		synctest.Wait()
		if calls != 1 {
			t.Fatalf("overlapping probes: %d calls", calls)
		}
		h.observe(ctx, replacement, time.Now())
		synctest.Wait()
		if calls != 2 || h.value != "" || h.failures != 0 {
			t.Fatalf("replacement inherited stale result: calls=%d health=%q failures=%d", calls, h.value, h.failures)
		}
		time.Sleep(12 * time.Second)
		synctest.Wait()
		h.observe(ctx, replacement, time.Now())
		if calls != 2 || h.value != "unhealthy" || h.failures != 1 {
			t.Fatalf("timed-out result = %q, %d failures, %d calls", h.value, h.failures, calls)
		}
		time.Sleep(18 * time.Second)
		h.observe(ctx, replacement, time.Now())
		synctest.Wait()
		cancel()
		synctest.Wait()
		if calls != 3 || <-h.pending.result {
			t.Fatal("stopped probe reported success")
		}
	})
}

func TestPingWatchThroughSessionActions(t *testing.T) {
	for _, action := range []string{"run", "start"} {
		t.Run(action, func(t *testing.T) {
			root := t.TempDir()
			old, replacement := testTunnelLink(t, "utun8"), testTunnelLink(t, "utun9")
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				policy := pingPolicy{netip.MustParseAddr("192.168.0.2"), time.Second, 3 * time.Second}
				replies := []bool{true, false, true, false, false, true}
				probes := 0
				reconnects := 0
				var alerts []string
				d := &daemon{
					ctx: ctx, root: root, foreground: action == "run",
					load:   func(string) (Profile, error) { return Profile{health: policy}, nil },
					notify: func(body string) { alerts = append(alerts, body) },
					run: func(ctx context.Context, profile Profile, status func(string, string)) error {
						status("connecting", "")
						w := newTunnelWatch(time.Now(), "local", tunnelHealth{policy: profile.health, ping: func(_ context.Context, device string, _ pingPolicy) bool {
							probes++
							if probes > len(replies) {
								t.Errorf("unexpected probe %d", probes)
								return false
							}
							want := old.device
							if probes == 6 {
								want = replacement.device
							}
							if device != want {
								t.Errorf("ping %d uses %s, want %s", probes, device, want)
							}
							return replies[probes-1]
						}})
						defer w.health.reset()
						link := old
						for second := 0; second <= 14; second++ {
							if err := w.sample(ctx, time.Now(), link, "", func(context.Context) string { return "local" }, func(context.Context) error {
								reconnects++
								link = replacement
								return nil
							}, status); err != nil {
								return err
							}
							if second == 4 && (w.stage != "connected" || w.reportedHealth != "unhealthy" || reconnects != 0) {
								t.Errorf("after first failure: stage=%q health=%q reconnects=%d", w.stage, w.reportedHealth, reconnects)
							}
							if second == 13 && (w.stage != "reconnecting" || w.reportedHealth != "unhealthy" || reconnects != 1) {
								t.Errorf("after second consecutive failure: stage=%q health=%q reconnects=%d", w.stage, w.reportedHealth, reconnects)
							}
							time.Sleep(time.Second)
						}
						// Process the replacement's reply after starting its probe.
						if err := w.sample(ctx, time.Now(), link, "", func(context.Context) string { return "local" }, func(context.Context) error {
							return errors.New("unexpected additional reconnect")
						}, status); err != nil {
							return err
						}
						<-ctx.Done()
						return ctx.Err()
					},
				}
				if _, _, err := d.handle(Request{Action: action, Profile: "office"}); err != nil {
					t.Fatal(err)
				}
				time.Sleep(16 * time.Second)
				synctest.Wait()
				data, err := json.Marshal(d.snapshot())
				if err != nil {
					t.Fatal(err)
				}
				var st struct{ State, Health string }
				if err := json.Unmarshal(data, &st); err != nil {
					t.Fatal(err)
				}
				if st.State != "running" || st.Health != "healthy" || probes != 6 || reconnects != 1 {
					t.Errorf("after replacement: status=%s probes=%d reconnects=%d", data, probes, reconnects)
				}
				if _, _, err := d.handle(Request{Action: "stop"}); err != nil {
					t.Fatal(err)
				}
				want := []string{"Connected · office", "Connection lost · office", "Connection recovered · office", "Disconnected · office"}
				if !slices.Equal(alerts, want) {
					t.Errorf("alerts = %q, want %q", alerts, want)
				}
			})
		})
	}
}

func TestTunnelWithoutPingReportsReadinessAsBefore(t *testing.T) {
	link := testTunnelLink(t, "utun8")
	now := time.Now()
	w := newTunnelWatch(now, "local", tunnelHealth{ping: func(context.Context, string, pingPolicy) bool {
		t.Error("probe ran without a configured target")
		return false
	}})
	var states []string
	err := w.sample(context.Background(), now, link, "", func(context.Context) string {
		return "local"
	}, func(context.Context) error {
		return errors.New("unexpected reconnect")
	}, func(stage, health string) {
		states = append(states, stage)
		if health != "" {
			t.Errorf("health = %q for an unchecked tunnel", health)
		}
	})
	if err != nil || !slices.Equal(states, []string{"connected"}) {
		t.Fatalf("states = %q, error = %v", states, err)
	}
}

func TestPingFailedReplacementReturnsWorkerError(t *testing.T) {
	old, replacement := testTunnelLink(t, "utun8"), testTunnelLink(t, "utun9")
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		policy := pingPolicy{netip.MustParseAddr("192.168.0.2"), time.Second, 3 * time.Second}
		probes := 0
		w := newTunnelWatch(time.Now(), "local", tunnelHealth{policy: policy, ping: func(context.Context, string, pingPolicy) bool {
			probes++
			return probes == 1
		}})
		var states []string
		var reconnects int
		link := old
		for second := 0; second <= 12; second++ {
			err := w.sample(ctx, time.Now(), link, "", func(context.Context) string { return "local" }, func(context.Context) error {
				reconnects++
				link = replacement
				return nil
			}, func(stage, _ string) {
				states = append(states, stage)
			})
			if err != nil {
				want := []string{"connected", "connected", "reconnecting"}
				if err.Error() != "VPN health check failed after reconnect" || reconnects != 1 || probes != 5 || !slices.Equal(states, want) {
					t.Fatalf("error=%v, reconnects=%d, probes=%d, stages=%q", err, reconnects, probes, states)
				}
				return
			}
			time.Sleep(time.Second)
		}
		t.Fatal("failed replacement stayed active")
	})
}
