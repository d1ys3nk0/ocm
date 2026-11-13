package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/d1ys3nk0/ocm/internal/ocm"
)

var version = "dev"
var commit = "unknown"

const root = "/var/lib/ocm"
const hook = "/opt/ocm/bin/ocm-hook"

const usage = `Usage: ocm COMMAND

Commands:
  install [--dry-run]  Install the system service and command
  uninstall            Remove the installation, preserving private data
  start NAME           Request a background session
  run NAME             Run a foreground session
  stop                 Stop the session and clear background intent
  toggle NAME          Start or stop the named session
  status [--json]      Show requested and observed state
  check NAME           Validate a saved profile
  profiles             List available profiles
  --help               Show this help
  --version            Show version information`

type usageError struct{}

func (usageError) Error() string { return usage }

type command struct {
	name, profile string
	json, dryRun  bool
}

func parse(args []string) (command, error) {
	if len(args) == 1 {
		switch args[0] {
		case "--help", "--version", "stop", "status", "profiles", "install", "uninstall", "daemon":
			return command{name: args[0]}, nil
		}
	}
	if len(args) == 2 {
		switch {
		case args[0] == "status" && args[1] == "--json":
			return command{name: "status", json: true}, nil
		case args[0] == "install" && args[1] == "--dry-run":
			return command{name: "install", dryRun: true}, nil
		case args[1] != "" && !strings.HasPrefix(args[1], "-"):
			switch args[0] {
			case "start", "run", "toggle", "check":
				if ocm.ValidProfileName(args[1]) {
					return command{name: args[0], profile: args[1]}, nil
				}
			}
		}
	}
	return command{}, usageError{}
}

type reply struct {
	State          string            `json:"state"`
	Mode           string            `json:"mode"`
	Profile        string            `json:"profile"`
	DesiredProfile string            `json:"desired_profile"`
	Health         string            `json:"health"`
	Error          string            `json:"error"`
	Details        map[string]string `json:"details"`
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	received := make(chan os.Signal, 1)
	go func() {
		select {
		case sig := <-signals:
			received <- sig
			cancel()
		case <-ctx.Done():
		}
	}()
	err := run(ctx, os.Args)
	select {
	case sig := <-received:
		if sig == os.Interrupt {
			os.Exit(130)
		}
		os.Exit(143)
	default:
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ocm:", err)
		var usageErr usageError
		if errors.As(err, &usageErr) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	isHook := filepath.Base(args[0]) == "ocm-hook"
	var cmd command
	if !isHook {
		var err error
		cmd, err = parse(args[1:])
		if err != nil {
			return err
		}
		switch cmd.name {
		case "--help":
			fmt.Println(usage)
			return nil
		case "--version":
			fmt.Printf("ocm %s (%s)\n", version, commit)
			return nil
		}
	}
	if os.Geteuid() != 0 {
		return errors.New("run OCM as root with sudo")
	}
	syscall.Umask(0077)
	prefix := "/usr/local"
	if runtime.GOARCH == "arm64" {
		prefix = "/opt/homebrew"
	}
	transport := ocm.Transport{Root: root, OpenConnect: prefix + "/bin/openconnect", VendorHook: prefix + "/etc/vpnc/vpnc-script"}
	if isHook {
		hookCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return transport.Hook(hookCtx)
	}
	switch cmd.name {
	case "daemon":
		return ocm.Serve(ctx, root, hook, transport)
	case "install":
		result, err := ocm.InstallResult(ctx, cmd.dryRun)
		if err != nil {
			return err
		}
		fmt.Println(result)
		return nil
	case "uninstall":
		if _, err := request(ctx, ocm.Request{Action: "stop"}); err != nil && !os.IsNotExist(err) {
			return err
		}
		return ocm.Uninstall(ctx)
	case "check":
		_, err := ocm.LoadProfile(root, cmd.profile, hook)
		if err == nil {
			fmt.Println("Profile is valid")
		}
		return err
	case "profiles":
		names, err := ocm.Profiles(root)
		if err != nil {
			return err
		}
		for _, name := range names {
			fmt.Println(name)
		}
		return nil
	case "run":
		control, err := ocm.LockControl(ctx, root)
		if err != nil {
			return err
		}
		ready := make(chan error, 1)
		done := make(chan error, 1)
		go func() { done <- ocm.Foreground(ctx, root, hook, cmd.profile, transport, ready) }()
		err = <-ready
		control.Close()
		if err != nil {
			return err
		}
		return <-done
	default:
		status, err := request(ctx, ocm.Request{Action: cmd.name, Profile: cmd.profile})
		if err != nil {
			return err
		}
		if cmd.json {
			return json.NewEncoder(os.Stdout).Encode(status)
		}
		fmt.Printf("State: %s\nMode: %s\nProfile: %s\nDesired profile: %s\nHealth: %s\n", status.State, status.Mode, status.Profile, status.DesiredProfile, status.Health)
		if status.Error != "" {
			fmt.Printf("Error: %s\n", status.Error)
		}
		return nil
	}
}

func request(ctx context.Context, req ocm.Request) (reply, error) {
	control, err := ocm.LockControl(ctx, root)
	if err != nil {
		return reply{}, err
	}
	defer control.Close()
	for _, socket := range []string{"foreground.sock", "control.sock"} {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", filepath.Join(root, socket))
		if err != nil {
			continue
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		if err := json.NewEncoder(conn).Encode(req); err != nil {
			return reply{}, err
		}
		var status reply
		if err := json.NewDecoder(conn).Decode(&status); err != nil {
			return reply{}, err
		}
		if req.Action != "status" && status.Error != "" {
			return reply{}, errors.New(status.Error)
		}
		return status, nil
	}
	if req.Action != "status" && req.Action != "stop" {
		return reply{}, errors.New("cannot contact system OCM service")
	}
	var status ocm.Status
	if req.Action == "status" {
		status, err = ocm.OfflineStatus(root)
	} else {
		status, err = ocm.StopOffline(ctx, root)
	}
	if err != nil {
		return reply{}, err
	}
	data, err := json.Marshal(status)
	if err != nil {
		return reply{}, err
	}
	var result reply
	err = json.Unmarshal(data, &result)
	return result, err
}
