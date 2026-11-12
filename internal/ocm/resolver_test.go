package ocm

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestProfileSplitDNS(t *testing.T) {
	for _, test := range []struct {
		name, domains, server, advertised string
		wantDomains, wantServers          []string
		parseError, missingDNS            bool
	}{
		{
			name: "explicit server wins", domains: " Corp.Example.,private.example,corp.example", server: "192.0.2.53",
			advertised: "192.0.2.55", wantDomains: []string{"corp.example", "private.example"}, wantServers: []string{"192.0.2.53"},
		},
		{
			name: "VPN DNS", domains: "corp.example", advertised: "192.0.2.53 bad 2001:db8::53 192.0.2.53",
			wantDomains: []string{"corp.example"}, wantServers: []string{"192.0.2.53", "2001:db8::53"},
		},
		{name: "VPN DNS missing", domains: "corp.example", advertised: "bad 0.0.0.0", wantDomains: []string{"corp.example"}, missingDNS: true},
		{name: "wildcard", domains: "*.corp.example", parseError: true},
		{name: "empty entry", domains: "corp.example,", parseError: true},
		{name: "multiple explicit servers", domains: "corp.example", server: "192.0.2.53,192.0.2.54", parseError: true},
		{name: "unsafe path", domains: "../corp.example", parseError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			config := "OC_HOST=vpn.example\nOC_USER=user\nOC_PASS=password\nOC_DNS_DOMAINS=" + test.domains + "\nOC_DNS_SERVER=" + test.server
			profile, err := parseProfile(bufio.NewScanner(strings.NewReader(config)), "/hook")
			if test.parseError {
				if err == nil {
					t.Fatal("accepted an invalid DNS profile")
				}
				return
			}
			if err != nil || !slices.Equal(profile.dns.domains, test.wantDomains) {
				t.Fatalf("profile domains = %v, error = %v", profile.dns.domains, err)
			}
			servers, err := profile.dns.nameservers(test.advertised)
			if (err != nil) != test.missingDNS || !slices.Equal(servers, test.wantServers) {
				t.Fatalf("servers = %v, error = %v", servers, err)
			}
		})
	}
}

func TestHookSplitDNSLifecycle(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "resolver")
	vendor := filepath.Join(root, "vpnc-script")
	script := "#!/bin/sh\n" +
		"[ -z \"$INTERNAL_IP4_DNS$INTERNAL_IP6_DNS$CISCO_DEF_DOMAIN$CISCO_SPLIT_DNS$OC_DNS_DOMAINS$OC_DNS_SERVER\" ]\n"
	if err := os.WriteFile(vendor, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	transport := Transport{Root: root, resolverDir: dir, VendorHook: vendor}
	var routes []string
	device := "utun3"
	transport.command = func(_ context.Context, _, path string, args ...string) (string, error) {
		if path != "/sbin/route" {
			t.Fatalf("unexpected network command %s %v", path, args)
		}
		routes = append(routes, strings.Join(args, " "))
		return "interface: " + device, nil
	}
	t.Setenv("VPNGATEWAY", "")
	t.Setenv("TUNDEV", "utun3")
	t.Setenv("INTERNAL_IP4_DNS", "192.0.2.53")
	t.Setenv("INTERNAL_IP6_DNS", "2001:db8::53")
	t.Setenv("CISCO_DEF_DOMAIN", "vendor.example")
	t.Setenv("CISCO_SPLIT_DNS", "vendor.example")
	t.Setenv("OC_DNS_SERVER", "")
	t.Setenv("OC_DNS_DOMAINS", "")
	t.Setenv("reason", "connect")
	ctx := context.Background()
	if err := transport.hook(ctx); err != nil || len(routes) != 0 {
		t.Fatalf("profile without DNS suffixes touched DNS: %v %v", routes, err)
	}
	assertAbsent(t, dir)
	t.Setenv("OC_DNS_DOMAINS", "corp.example,private.example")
	device = "en0"
	if err := transport.hook(ctx); err == nil {
		t.Fatal("accepted a DNS server outside the tunnel")
	}
	assertAbsent(t, dir)
	assertAbsent(t, filepath.Join(root, "tunnel"))

	device = "utun3"
	routes = nil
	if err := transport.hook(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(routes, []string{"-n get -inet 192.0.2.53", "-n get -inet6 2001:db8::53"}) {
		t.Fatalf("route checks = %v", routes)
	}
	corp := filepath.Join(dir, "corp.example")
	private := filepath.Join(dir, "private.example")
	data, err := os.ReadFile(corp)
	if err != nil || !strings.HasSuffix(string(data), "nameserver 192.0.2.53\nnameserver 2001:db8::53\n") {
		t.Fatalf("resolver = %q, error = %v", data, err)
	}

	t.Setenv("reason", "attempt-reconnect")
	if err := transport.hook(ctx); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, corp)
	assertAbsent(t, private)
	assertAbsent(t, transport.resolverJournal())
	t.Setenv("reason", "reconnect")
	t.Setenv("OC_DNS_SERVER", "192.0.2.54")
	if err := transport.hook(ctx); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(corp)
	if err != nil || !strings.HasSuffix(string(data), "nameserver 192.0.2.54\n") {
		t.Fatalf("reconnected resolver = %q, error = %v", data, err)
	}
	if err := os.WriteFile(private, []byte("nameserver 192.0.2.99\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("reason", "disconnect")
	if err := transport.hook(ctx); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, corp)
	assertAbsent(t, transport.resolverJournal())
	t.Setenv("reason", "connect")
	if err := transport.hook(ctx); err == nil {
		t.Fatal("overwrote an existing resolver")
	}
	assertAbsent(t, corp)
	data, err = os.ReadFile(private)
	if err != nil || string(data) != "nameserver 192.0.2.99\n" {
		t.Fatalf("external resolver = %q, error = %v", data, err)
	}
}

func TestRecoverPartialResolverInstall(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "resolver")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	transport := Transport{Root: root, resolverDir: dir}
	content := "# OCM test-token\nnameserver 192.0.2.53\n"
	if err := writeJSON(transport.resolverJournal(), resolverRecord{[]string{"corp.example", "private.example"}, content}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "corp.example")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	if err := transport.recoverNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertAbsent(t, path)
	assertAbsent(t, transport.resolverJournal())
}

func assertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent, got %v", path, err)
	}
}
