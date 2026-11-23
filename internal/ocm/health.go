package ocm

import (
	"context"
	"fmt"
	"math"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

type pingPolicy struct {
	target            netip.Addr
	timeout, interval time.Duration
}

func parsePingPolicy(values map[string]string) (pingPolicy, error) {
	target, configured := values["OC_HEALTH_PING"]
	_, hasTimeout := values["OC_HEALTH_TIMEOUT"]
	_, hasInterval := values["OC_HEALTH_INTERVAL"]
	if !configured && !hasTimeout && !hasInterval {
		return pingPolicy{}, nil
	}
	ip, err := netip.ParseAddr(target)
	if err != nil || !ip.Is4() {
		return pingPolicy{}, fmt.Errorf("OC_HEALTH_PING must be an IPv4 address")
	}
	policy := pingPolicy{target: ip, timeout: 3 * time.Second, interval: 30 * time.Second}
	for _, setting := range []struct {
		key string
		dst *time.Duration
	}{
		{"OC_HEALTH_TIMEOUT", &policy.timeout},
		{"OC_HEALTH_INTERVAL", &policy.interval},
	} {
		if value, ok := values[setting.key]; ok {
			seconds, hasUnit := strings.CutSuffix(value, "s")
			number, err := strconv.ParseUint(seconds, 10, 32)
			if !hasUnit || err != nil || number == 0 {
				return pingPolicy{}, fmt.Errorf("%s must be a positive whole number of seconds (for example 3s)", setting.key)
			}
			*setting.dst = time.Duration(number) * time.Second
		}
	}
	if policy.timeout.Milliseconds() > math.MaxInt32 {
		return pingPolicy{}, fmt.Errorf("OC_HEALTH_TIMEOUT exceeds the macOS ping limit")
	}
	if policy.timeout >= policy.interval {
		return pingPolicy{}, fmt.Errorf("OC_HEALTH_TIMEOUT must be shorter than OC_HEALTH_INTERVAL")
	}
	return policy, nil
}

func (t Transport) ping(ctx context.Context, device string, policy pingPolicy) bool {
	command := t.command
	if command == nil {
		command = runSystemCommand
	}
	wait := strconv.FormatInt(policy.timeout.Milliseconds(), 10)
	_, err := command(ctx, "", "/sbin/ping", "-n", "-b", device, "-c", "1", "-W", wait, policy.target.String())
	return err == nil && ctx.Err() == nil
}

type tunnelLink struct {
	device string
	marker os.FileInfo
}

type pendingPing struct {
	result <-chan bool
	cancel context.CancelFunc
}

type tunnelHealth struct {
	policy   pingPolicy
	ping     func(context.Context, string, pingPolicy) bool
	link     tunnelLink
	pending  pendingPing
	next     time.Time
	value    string
	failures int
}

func (h *tunnelHealth) reset() {
	if h.pending.cancel != nil {
		h.pending.cancel()
		h.pending.cancel = nil
	}
	h.link = tunnelLink{}
	h.next = time.Time{}
	h.value = ""
	h.failures = 0
}

func (h *tunnelHealth) observe(ctx context.Context, link tunnelLink, now time.Time) (replaced bool) {
	if h.link.device != link.device || !os.SameFile(h.link.marker, link.marker) {
		replaced = h.link.device != ""
		h.reset()
		h.link = link
	}
	if h.pending.result != nil {
		select {
		case ok := <-h.pending.result:
			if h.pending.cancel != nil {
				h.pending.cancel()
				if ok {
					h.value, h.failures = "healthy", 0
				} else {
					h.value = "unhealthy"
					h.failures++
				}
			}
			h.pending = pendingPing{}
		default:
			return replaced
		}
	}
	if h.failures < 2 && !now.Before(h.next) && ctx.Err() == nil {
		probeCtx, cancel := context.WithTimeout(ctx, h.policy.timeout)
		result := make(chan bool, 1)
		h.pending = pendingPing{result: result, cancel: cancel}
		h.next = now.Add(h.policy.interval)
		go func() { result <- h.ping(probeCtx, link.device, h.policy) }()
	}
	return replaced
}
