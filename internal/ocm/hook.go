package ocm

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

var tunnelDevice = regexp.MustCompile(`^(utun|tun)[0-9]+$`)
var devicePattern = regexp.MustCompile(`(?m)^\s*interface:\s*(\S+)`)
var serviceHeader = regexp.MustCompile(`^\([0-9]+\) (.+)$`)
var serviceDevice = regexp.MustCompile(`Device: ([^),]+)`)

type dnsRecord struct {
	Service  string
	Previous []string
	Applied  string
}

type routeRecord struct {
	Destination, Gateway string
}

func (t Transport) networkCommand(ctx context.Context, path string, args ...string) (string, error) {
	if t.command != nil {
		return t.command(ctx, "", path, args...)
	}
	return systemCommand(ctx, "", path, args...)
}

func (t Transport) withNetworkLock(ctx context.Context, action func() error) error {
	file, err := os.OpenFile(filepath.Join(t.Root, "network.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return action()
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return err
		}
		if !pause(ctx, 100*time.Millisecond) {
			return ctx.Err()
		}
	}
}

func (t Transport) recoverNetwork(ctx context.Context) error {
	return t.withNetworkLock(ctx, func() error {
		return errors.Join(t.restoreDNSLocked(ctx), t.forgetRoute(ctx))
	})
}

func (t Transport) restoreDNS(ctx context.Context) error {
	return t.withNetworkLock(ctx, func() error { return t.restoreDNSLocked(ctx) })
}

func (t Transport) restoreDNSLocked(ctx context.Context) error {
	path := filepath.Join(t.Root, "dns.json")
	var record dnsRecord
	if err := readJSON(path, &record); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot read DNS ownership record: %w", err)
	}
	current, err := t.currentDNS(ctx, record.Service)
	if err != nil {
		return err
	}
	if len(current) == 1 && current[0] == record.Applied {
		if err := t.setDNS(ctx, record.Service, record.Previous); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func (t Transport) currentDNS(ctx context.Context, service string) ([]string, error) {
	if service == "" || strings.HasPrefix(service, "-") {
		return nil, fmt.Errorf("invalid physical network service")
	}
	text, err := t.networkCommand(ctx, "/usr/sbin/networksetup", "-getdnsservers", service)
	if err != nil {
		return nil, fmt.Errorf("cannot read network service DNS: %w", err)
	}
	if text == "There aren't any DNS Servers set on "+service+"." {
		return nil, nil
	}
	addresses := strings.Fields(text)
	for _, address := range addresses {
		if _, err := netip.ParseAddr(address); err != nil {
			return nil, fmt.Errorf("unexpected network service DNS output")
		}
	}
	return addresses, nil
}

func (t Transport) setDNS(ctx context.Context, service string, servers []string) error {
	for _, server := range servers {
		if _, err := netip.ParseAddr(server); err != nil {
			return fmt.Errorf("invalid saved DNS address")
		}
	}
	if len(servers) == 0 {
		servers = []string{"empty"}
	}
	_, err := t.networkCommand(ctx, "/usr/sbin/networksetup", append([]string{"-setdnsservers", service}, servers...)...)
	if err != nil {
		return fmt.Errorf("cannot set network service DNS: %w", err)
	}
	return nil
}

func (t Transport) physicalService(ctx context.Context, gateway string) (string, error) {
	physical := func(destination string) string {
		output, _ := t.networkCommand(ctx, "/sbin/route", "-n", "get", destination)
		device := capture(devicePattern, output)
		if strings.HasPrefix(device, "lo") || tunnelDevice.MatchString(device) {
			return ""
		}
		return device
	}
	device := physical("default")
	if device == "" && gateway != "" {
		device = physical(gateway)
	}
	services, err := t.networkCommand(ctx, "/usr/sbin/networksetup", "-listnetworkserviceorder")
	if err != nil {
		return "", err
	}
	var service string
	for _, line := range strings.Split(services, "\n") {
		if name := capture(serviceHeader, line); name != "" {
			service = name
		}
		if device != "" && capture(serviceDevice, line) == device && service != "" {
			return service, nil
		}
	}
	return "", fmt.Errorf("cannot identify physical network service")
}

func (t Transport) switchDNS(ctx context.Context, gateway string) error {
	var server string
	if err := readJSON(filepath.Join(t.Root, "dns-server.json"), &server); err != nil {
		return fmt.Errorf("cannot read DNS setting: %w", err)
	}
	if _, err := netip.ParseAddr(server); err != nil {
		return fmt.Errorf("invalid DNS setting")
	}
	service, err := t.physicalService(ctx, gateway)
	if err != nil {
		return err
	}
	if err := t.restoreDNSLocked(ctx); err != nil {
		return err
	}
	current, err := t.currentDNS(ctx, service)
	if err != nil {
		return err
	}
	if len(current) == 1 && current[0] == server {
		return nil
	}
	if err := writeJSON(filepath.Join(t.Root, "dns.json"), dnsRecord{service, current, server}); err != nil {
		return err
	}
	return t.setDNS(ctx, service, []string{server})
}

func findRoute(table, destination string) (routeRecord, error) {
	var result routeRecord
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || (fields[0] != destination && fields[0] != destination+"/32") ||
			!strings.Contains(fields[2], "H") || strings.Contains(fields[2], "W") {
			continue
		}
		if result != (routeRecord{}) {
			return routeRecord{}, fmt.Errorf("ambiguous explicit VPN gateway routes")
		}
		result = routeRecord{destination, fields[1]}
	}
	return result, nil
}

func (t Transport) gatewayRoute(ctx context.Context, destination string) (routeRecord, error) {
	output, err := t.networkCommand(ctx, "/usr/sbin/netstat", "-rn", "-f", "inet")
	if err != nil {
		return routeRecord{}, err
	}
	return findRoute(output, destination)
}

func (t Transport) forgetRoute(ctx context.Context) error {
	path := filepath.Join(t.Root, "route.json")
	var saved routeRecord
	if err := readJSON(path, &saved); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !ipv4(saved.Destination) || !ipv4(saved.Gateway) {
		return fmt.Errorf("invalid route ownership record")
	}
	current, err := t.gatewayRoute(ctx, saved.Destination)
	if err != nil {
		return err
	}
	if current == saved {
		if _, err := t.networkCommand(ctx, "/sbin/route", "-n", "delete", "-host", saved.Destination, saved.Gateway); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func (t Transport) Hook(ctx context.Context) error {
	if err := CheckRoot(t.Root); err != nil {
		return err
	}
	return t.withNetworkLock(ctx, func() error { return t.hook(ctx) })
}

func (t Transport) hook(ctx context.Context) error {
	reason, gateway := os.Getenv("reason"), os.Getenv("VPNGATEWAY")
	if gateway != "" {
		if _, err := netip.ParseAddr(gateway); err != nil {
			return fmt.Errorf("invalid VPN gateway")
		}
	}
	ipv4Gateway := ipv4(gateway)
	if reason == "attempt-reconnect" || reason == "disconnect" {
		os.Remove(filepath.Join(t.Root, "tunnel"))
		if !strings.Contains(gateway, ":") {
			if err := t.forgetRoute(ctx); err != nil {
				return err
			}
		}
	}
	if ipv4Gateway && (reason == "connect" || reason == "attempt-reconnect") {
		current, err := t.gatewayRoute(ctx, gateway)
		if err != nil {
			return err
		}
		if current != (routeRecord{}) {
			return fmt.Errorf("refusing to adopt an existing VPN gateway host route")
		}
	}
	env := []string{systemPath, "LC_ALL=C", "SKIP_DEFAULT_ROUTE=1"}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		switch key {
		case "INTERNAL_IP4_DNS", "INTERNAL_IP6_DNS", "CISCO_DEF_DOMAIN", "CISCO_SPLIT_DNS":
			continue
		case "VPNGATEWAY":
			if reason == "disconnect" && !strings.Contains(gateway, ":") {
				continue
			}
		default:
			if key != "reason" && key != "TUNDEV" && key != "VPNPID" && !strings.HasPrefix(key, "INTERNAL_IP") && !strings.HasPrefix(key, "CISCO_") {
				continue
			}
		}
		env = append(env, entry)
	}
	cmd := exec.CommandContext(ctx, t.VendorHook)
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("vendor VPN hook failed: %w", err)
	}
	switch reason {
	case "disconnect":
		return t.restoreDNSLocked(ctx)
	case "connect", "reconnect", "attempt-reconnect":
		if ipv4Gateway {
			current, err := t.gatewayRoute(ctx, gateway)
			if err != nil {
				return err
			}
			if current != (routeRecord{}) {
				var prior routeRecord
				priorErr := readJSON(filepath.Join(t.Root, "route.json"), &prior)
				if !ipv4(current.Gateway) || (reason == "reconnect" && (priorErr != nil || prior != current)) {
					return fmt.Errorf("cannot establish ownership of VPN gateway route")
				}
				if err := writeJSON(filepath.Join(t.Root, "route.json"), current); err != nil {
					return err
				}
			}
		}
		if reason != "attempt-reconnect" {
			device := os.Getenv("TUNDEV")
			if !tunnelDevice.MatchString(device) {
				return fmt.Errorf("invalid VPN tunnel device")
			}
			if err := t.switchDNS(ctx, gateway); err != nil {
				return err
			}
			return writeState(filepath.Join(t.Root, "tunnel"), []byte(device+"\n"))
		}
	}
	return nil
}
