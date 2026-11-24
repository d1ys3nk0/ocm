package ocm

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"time"
)

var errGatewayDNS = errors.New("gateway DNS lookup failed")
var serviceIPv4Key = regexp.MustCompile(`State:/Network/Service/([A-Fa-f0-9-]+)/IPv4`)
var serviceInterface = regexp.MustCompile(`(?m)^\s*InterfaceName\s*:\s*(\S+)`)
var dnsServerList = regexp.MustCompile(`(?s)ServerAddresses\s*:\s*<array>\s*\{([^}]*)\}`)
var dnsServerEntry = regexp.MustCompile(`(?m)^\s*\d+\s*:\s*(\S+)`)

func (p Profile) gatewayNeedsUplinkDNS() bool {
	host := strings.TrimSuffix(strings.ToLower(p.hostname), ".")
	if !validDNSDomain(host) {
		return false
	}
	for _, suffix := range p.dns.domains {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func (t Transport) gatewayCommand(ctx context.Context, input, path string, args ...string) (string, error) {
	if t.command != nil {
		return t.command(ctx, input, path, args...)
	}
	return systemCommand(ctx, input, path, args...)
}

func (t Transport) uplinkDNSServers(ctx context.Context, device string) []string {
	keys, _ := t.gatewayCommand(ctx, "list State:/Network/Service/.*/IPv4\n", "/usr/sbin/scutil")
	for _, match := range serviceIPv4Key.FindAllStringSubmatch(keys, -1) {
		ipv4, _ := t.gatewayCommand(ctx, "show "+match[0]+"\n", "/usr/sbin/scutil")
		if capture(serviceInterface, ipv4) != device {
			continue
		}
		for _, scope := range []string{"State", "Setup"} {
			key := scope + ":/Network/Service/" + match[1] + "/DNS"
			settings, _ := t.gatewayCommand(ctx, "show "+key+"\n", "/usr/sbin/scutil")
			if servers := capture(dnsServerList, settings); servers != "" {
				var addresses []string
				for _, entry := range dnsServerEntry.FindAllStringSubmatch(servers, -1) {
					addresses = append(addresses, entry[1])
				}
				if addresses := usableDNSServers(addresses); len(addresses) > 0 {
					return addresses
				}
			}
		}
	}
	dhcp, _ := t.gatewayCommand(ctx, "", "/usr/sbin/ipconfig", "getoption", device, "domain_name_server")
	return usableDNSServers(strings.Fields(dhcp))
}

func usableDNSServers(values []string) []string {
	var servers []string
	for _, value := range values {
		if address, ok := dnsAddress(value); ok && !slices.Contains(servers, address) {
			servers = append(servers, address)
		}
	}
	return servers
}

func (t Transport) lookupGatewayIPv4(ctx context.Context, hostname, server string) (netip.Addr, error) {
	answer, err := t.gatewayCommand(ctx, "", "/usr/bin/dig", "@"+server, strings.TrimSuffix(hostname, ".")+".", "A",
		"+nosearch", "+tries=1", "+noall", "+answer")
	if ctx.Err() != nil {
		return netip.Addr{}, ctx.Err()
	}
	if err != nil {
		return netip.Addr{}, fmt.Errorf("DNS query failed: %w", err)
	}
	for line := range strings.SplitSeq(answer, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 5 || fields[2] != "IN" || fields[3] != "A" {
			continue
		}
		address, err := netip.ParseAddr(fields[4])
		if err == nil && address.Is4() && !address.IsUnspecified() && !address.IsMulticast() {
			return address, nil
		}
	}
	return netip.Addr{}, errors.New("no usable IPv4 address")
}

func (t Transport) gatewayArgs(ctx context.Context, profile Profile, device string) ([]string, error) {
	if !profile.gatewayNeedsUplinkDNS() {
		return profile.args, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	servers := t.uplinkDNSServers(ctx, device)
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w for %s: %w", errGatewayDNS, profile.hostname, ctx.Err())
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("%w for %s: no DNS servers for %s", errGatewayDNS, profile.hostname, device)
	}
	deadline, _ := ctx.Deadline()
	var attempts []error
	for index, server := range servers {
		if ctx.Err() != nil {
			attempts = append(attempts, ctx.Err())
			break
		}
		timeout := time.Until(deadline) / time.Duration(len(servers)-index)
		attempt, done := context.WithTimeout(ctx, timeout)
		address, err := t.lookupGatewayIPv4(attempt, profile.hostname, server)
		done()
		if err == nil {
			return slices.Insert(slices.Clip(profile.args), len(profile.args)-2, "--resolve="+profile.hostname+":"+address.String()), nil
		}
		attempts = append(attempts, fmt.Errorf("server %s: %w", server, err))
	}
	return nil, fmt.Errorf("%w for %s on %s: %w", errGatewayDNS, profile.hostname, device, errors.Join(attempts...))
}
