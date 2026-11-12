package ocm

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

type dnsPolicy struct {
	domains []string
	server  string
}

func validDNSDomain(domain string) bool {
	if len(domain) > 253 {
		return false
	}
	if _, err := netip.ParseAddr(domain); err == nil {
		return false
	}
	for label := range strings.SplitSeq(domain, ".") {
		if !dnsLabel.MatchString(label) {
			return false
		}
	}
	return true
}

func dnsAddress(value string) (string, bool) {
	addr, err := netip.ParseAddr(value)
	if err != nil || addr.Zone() != "" {
		return "", false
	}
	addr = addr.Unmap()
	if addr.IsUnspecified() || addr.IsMulticast() {
		return "", false
	}
	return addr.String(), true
}

func parseDNSPolicy(domains, server string) (dnsPolicy, error) {
	var policy dnsPolicy
	if domains != "" {
		for item := range strings.SplitSeq(domains, ",") {
			domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(item)), ".")
			if !validDNSDomain(domain) {
				return dnsPolicy{}, fmt.Errorf("invalid suffix in OC_DNS_DOMAINS")
			}
			if !slices.Contains(policy.domains, domain) {
				policy.domains = append(policy.domains, domain)
			}
		}
	}
	if server != "" {
		var ok bool
		policy.server, ok = dnsAddress(strings.TrimSpace(server))
		if !ok {
			return dnsPolicy{}, fmt.Errorf("OC_DNS_SERVER must be a single DNS server IP address")
		}
	}
	return policy, nil
}

func (p dnsPolicy) nameservers(advertised string) ([]string, error) {
	if p.server != "" {
		return []string{p.server}, nil
	}
	var servers []string
	for _, value := range strings.Fields(advertised) {
		if addr, ok := dnsAddress(value); ok && !slices.Contains(servers, addr) {
			servers = append(servers, addr)
		}
	}
	if len(servers) == 0 {
		return nil, fmt.Errorf("VPN did not advertise a usable DNS server for OC_DNS_DOMAINS")
	}
	return servers, nil
}

type resolverRecord struct {
	Domains []string
	Content string
}

func (t Transport) resolverPath() string {
	if t.resolverDir != "" {
		return t.resolverDir
	}
	return "/etc/resolver"
}

func (t Transport) resolverJournal() string {
	return filepath.Join(t.Root, "resolvers.json")
}

func (t Transport) removeResolvers() error {
	var record resolverRecord
	if err := readJSON(t.resolverJournal(), &record); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("cannot read split DNS ownership record: %w", err)
	}
	if !strings.HasPrefix(record.Content, "# OCM ") {
		return fmt.Errorf("invalid split DNS ownership record")
	}
	for _, domain := range record.Domains {
		if !validDNSDomain(domain) {
			return fmt.Errorf("invalid domain in split DNS ownership record")
		}
	}
	dir := t.resolverPath()
	if info, err := os.Lstat(dir); os.IsNotExist(err) || (err == nil && !info.IsDir()) {
		return os.Remove(t.resolverJournal())
	} else if err != nil {
		return err
	}
	for _, domain := range record.Domains {
		path := filepath.Join(dir, domain)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) || (err == nil && !info.Mode().IsRegular()) {
			continue
		}
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		current, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if string(data) == record.Content && os.SameFile(info, current) {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return os.Remove(t.resolverJournal())
}

func (t Transport) installResolvers(ctx context.Context, device string, policy dnsPolicy, advertised string) (result error) {
	if len(policy.domains) == 0 {
		return nil
	}
	servers, err := policy.nameservers(advertised)
	if err != nil {
		return err
	}
	for _, server := range servers {
		family := "-inet"
		if strings.Contains(server, ":") {
			family = "-inet6"
		}
		output, err := t.networkCommand(ctx, "/sbin/route", "-n", "get", family, server)
		if err != nil || capture(devicePattern, output) != device {
			return fmt.Errorf("VPN DNS server %s is not routed through %s", server, device)
		}
	}

	dir := t.resolverPath()
	if err := os.Mkdir(dir, 0755); err == nil {
		// OCM's private umask would otherwise hide resolver files from macOS.
		if err := os.Chmod(dir, 0755); err != nil {
			return err
		}
	} else if !os.IsExist(err) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("split DNS resolver path is not a directory: %s", dir)
	}
	for _, domain := range policy.domains {
		if _, err := os.Lstat(filepath.Join(dir, domain)); !os.IsNotExist(err) {
			if err != nil {
				return err
			}
			return fmt.Errorf("resolver already exists for %s", domain)
		}
	}

	record := resolverRecord{Domains: policy.domains, Content: "# OCM " + rand.Text() + "\n"}
	for _, server := range servers {
		record.Content += "nameserver " + server + "\n"
	}
	if err := writeJSON(t.resolverJournal(), record); err != nil {
		return err
	}
	defer func() {
		if result != nil {
			result = errors.Join(result, t.removeResolvers())
		}
	}()
	for _, domain := range policy.domains {
		if err := publishResolver(filepath.Join(dir, domain), record.Content); err != nil {
			return fmt.Errorf("cannot install split DNS resolver for %s: %w", domain, err)
		}
	}
	return nil
}

func publishResolver(path, content string) error {
	// Stage outside the resolver directory and publish each file without replacing
	// any entry that another network tool creates after the conflict check.
	staged, err := os.CreateTemp(filepath.Dir(filepath.Dir(path)), ".ocm-resolver-")
	if err != nil {
		return err
	}
	defer os.Remove(staged.Name())
	_, writeErr := staged.WriteString(content)
	if err := errors.Join(writeErr, staged.Chmod(0644), staged.Sync(), staged.Close()); err != nil {
		return err
	}

	return os.Link(staged.Name(), path)
}
