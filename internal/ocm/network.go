package ocm

import (
	"context"
	"net/netip"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const systemPath = "PATH=/usr/bin:/bin:/usr/sbin:/sbin"

func systemCommand(ctx context.Context, input, path string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Env = []string{systemPath, "LC_ALL=C"}
	cmd.Stdin = strings.NewReader(input)
	data, err := cmd.Output()
	return strings.TrimSpace(string(data)), err
}

func ipv4(value string) bool {
	ip, err := netip.ParseAddr(value)
	return err == nil && ip.Is4()
}

var hardwarePort = regexp.MustCompile(`(?m)^Hardware Port: (.+)\nDevice: (\S+)`)
var primaryInterface = regexp.MustCompile(`(?m)^\s*PrimaryInterface\s*:\s*(\S+)`)
var primaryRouter = regexp.MustCompile(`(?m)^\s*Router\s*:\s*(\S+)`)

func capture(pattern *regexp.Regexp, text string) string {
	match := pattern.FindStringSubmatch(text)
	if match == nil {
		return ""
	}
	return match[1]
}

func uplink(ctx context.Context) string {
	ports, err := systemCommand(ctx, "", "/usr/sbin/networksetup", "-listallhardwareports")
	if err != nil {
		return ""
	}
	primary, _ := systemCommand(ctx, "show State:/Network/Global/IPv4\n", "/usr/sbin/scutil")
	device := capture(primaryInterface, primary)
	var ethernet, wifi string
	for _, port := range hardwarePort.FindAllStringSubmatch(ports, -1) {
		if port[1] != "Wi-Fi" && !strings.Contains(port[1], "Ethernet") {
			continue
		}
		address, _ := systemCommand(ctx, "", "/usr/sbin/ipconfig", "getifaddr", port[2])
		if !ipv4(address) || strings.HasPrefix(address, "169.254.") {
			continue
		}
		router, _ := systemCommand(ctx, "", "/usr/sbin/ipconfig", "getoption", port[2], "router")
		if router == "" && port[2] == device {
			router = capture(primaryRouter, primary)
		}
		if !ipv4(router) {
			continue
		}
		identity := strings.Join([]string{port[2], address, router}, "/")
		switch {
		case port[2] == device:
			return identity
		case port[1] == "Wi-Fi" && wifi == "":
			wifi = identity
		case port[1] != "Wi-Fi" && ethernet == "":
			ethernet = identity
		}
	}
	if ethernet != "" {
		return ethernet
	}
	return wifi
}
