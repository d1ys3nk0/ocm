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
	"syscall"
	"time"

	"github.com/d1ys3nk0/ocm/internal/ocm"
)

var version = "dev"
var commit = "unknown"

const root = "/Library/OCM/state"
const hook = "/Library/OCM/bin/ocm-hook"

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintln(os.Stderr, "ocm:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 2 && args[1] == "--version" {
		fmt.Printf("ocm %s (%s)\n", version, commit)
		return nil
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
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if filepath.Base(args[0]) == "ocm-hook" {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return transport.Hook(ctx)
	}
	if len(args) == 2 && args[1] == "daemon" {
		return ocm.Serve(ctx, root, hook, transport)
	}
	request, err := parse(args[1:])
	if err != nil {
		return err
	}
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", filepath.Join(root, "control.sock"))
	if err != nil {
		return fmt.Errorf("cannot contact system OCM daemon: %w", err)
	}
	defer conn.Close()
	closeOnCancel := context.AfterFunc(ctx, func() { conn.Close() })
	defer closeOnCancel()
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return err
	}
	decoder := json.NewDecoder(conn)
	var status ocm.Status
	if err := decoder.Decode(&status); err != nil {
		return err
	}
	if status.Error != "" {
		return errors.New(status.Error)
	}
	if err := json.NewEncoder(os.Stdout).Encode(status); err != nil {
		return err
	}
	if request.Action != "connect" {
		return nil
	}
	if err := decoder.Decode(&status); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	if status.State == "failed" || status.State == "authentication-required" {
		return errors.New(status.State)
	}
	return nil
}

func parse(args []string) (ocm.Request, error) {
	if len(args) == 1 && (args[0] == "stop" || args[0] == "status") {
		return ocm.Request{Action: args[0]}, nil
	}
	if len(args) == 2 {
		switch args[0] {
		case "connect", "start", "toggle":
			return ocm.Request{Action: args[0], Profile: args[1]}, nil
		}
		switch args[1] {
		case "connect", "start", "toggle":
			return ocm.Request{Action: args[1], Profile: args[0]}, nil
		}
	}
	return ocm.Request{}, errors.New("usage: ocm {start|connect|toggle} PROFILE | ocm {status|stop|--version}")
}
