package ocm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

type Transport struct {
	Root, OpenConnect, VendorHook string
	resolverDir                   string
	command                       func(context.Context, string, string, ...string) (string, error)
}

var errAuthentication = errors.New("VPN authentication failed")
var errRecovery = errors.New("network recovery failed")
var authError = regexp.MustCompile(`(?i)authentication failed|login failed|HTTP response HTTP/[^ ]+ 401`)
var connectionError = regexp.MustCompile(`(?i)Dead Peer Detection detected dead peer|Can't assign requested address|Reconnect failed|Failed to reconnect|Session terminated|SSL connection failure|Script .+ returned error`)
var established = regexp.MustCompile(`Configured as [0-9.]+|CSTP connected\.`)

type childProcess interface {
	Signal(os.Signal) error
	Kill() error
}

func finishSession(process childProcess, done <-chan error, exited bool, grace, force time.Duration, cleanup func() error) error {
	var signalErr error
	if !exited {
		select {
		case <-done:
			exited = true
		default:
		}
		if !exited {
			// The process handle belongs to the child we started; no PID is rediscovered.
			if err := process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
				signalErr = fmt.Errorf("cannot signal OpenConnect child: %w", err)
			} else {
				exited = waitForChild(done, grace)
			}
		}
		if !exited {
			if err := process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				signalErr = errors.Join(signalErr, fmt.Errorf("cannot kill OpenConnect child: %w", err))
			}
			exited = waitForChild(done, force)
		}
	}
	if !exited {
		return errors.Join(errRecovery, errors.New("OpenConnect child exit was not confirmed"), signalErr)
	}
	if err := cleanup(); err != nil {
		return errors.Join(signalErr, fmt.Errorf("%w: %v", errRecovery, err))
	}
	return signalErr
}

func waitForChild(done <-chan error, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

func (t Transport) existing(ctx context.Context) (bool, error) {
	output, err := t.networkCommand(ctx, "/usr/bin/pgrep", "-x", "openconnect")
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		return false, nil
	}
	return output != "", err
}

func (t Transport) cleanup(ctx context.Context) error {
	running, err := t.existing(ctx)
	if err != nil {
		return fmt.Errorf("cannot inspect OpenConnect processes: %w", err)
	}
	if running {
		return fmt.Errorf("another OpenConnect process is running")
	}
	return t.recoverNetwork(ctx)
}

func (t Transport) ready(ctx context.Context) tunnelLink {
	file, err := os.Open(filepath.Join(t.Root, "tunnel"))
	if err != nil {
		return tunnelLink{}
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return tunnelLink{}
	}
	var data [32]byte
	n, _ := file.Read(data[:])
	name := strings.TrimSpace(string(data[:n]))
	if !tunnelDevice.MatchString(name) {
		return tunnelLink{}
	}
	output, err := systemCommand(ctx, "", "/sbin/ifconfig", name)
	if err != nil || !strings.Contains(output, "\tinet ") {
		return tunnelLink{}
	}
	return tunnelLink{device: name, marker: info}
}

func (t Transport) Run(ctx context.Context, profile Profile, status func(state, health string)) (result error) {
	status("waiting-network", "")
	var network string
	for ctx.Err() == nil {
		network = uplink(ctx)
		if !pause(ctx, 5*time.Second) {
			return ctx.Err()
		}
		if network != "" && network == uplink(ctx) {
			break
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.cleanup(ctx); err != nil {
		return fmt.Errorf("%w: %v", errRecovery, err)
	}
	os.Remove(filepath.Join(t.Root, "tunnel"))
	status("connecting", "")
	path := filepath.Join(t.Root, "openconnect.log")
	if err := os.Rename(path, path+".previous"); err != nil && !os.IsNotExist(err) {
		return err
	}
	log, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(t.OpenConnect, profile.args...)
	cmd.Env = []string{systemPath, "LC_ALL=C", "HOME=/var/root",
		"OC_DNS_DOMAINS=" + strings.Join(profile.dns.domains, ","), "OC_DNS_SERVER=" + profile.dns.server}
	cmd.Stdin = strings.NewReader(profile.password + "\n")
	cmd.Stdout, cmd.Stderr = log, log
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("cannot launch OpenConnect: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	exited := false
	defer func() {
		finishErr := finishSession(cmd.Process, done, exited, 12*time.Second, 5*time.Second, func() error {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			return t.cleanup(cleanupCtx)
		})
		if finishErr != nil && !errors.Is(finishErr, errRecovery) {
			fmt.Fprintln(os.Stderr, "ocm:", finishErr)
		}
		result = errors.Join(result, finishErr)
	}()
	watch := newTunnelWatch(time.Now(), network, tunnelHealth{policy: profile.health, ping: t.ping})
	defer func() {
		watch.health.reset()
		if result := watch.health.pending.result; result != nil {
			<-result
		}
	}()
	reconnect := func(ctx context.Context) error {
		if err := t.restoreDNS(ctx); err != nil {
			return err
		}
		os.Remove(filepath.Join(t.Root, "tunnel"))
		if err := cmd.Process.Signal(syscall.SIGUSR2); err != nil {
			return fmt.Errorf("cannot request OpenConnect reconnect: %w", err)
		}
		return nil
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			exited = true
			if authError.MatchString(logTail(path)) {
				return errAuthentication
			}
			if err != nil {
				return fmt.Errorf("OpenConnect exited: %w", err)
			}
			return nil
		case <-tick.C:
			now := time.Now()
			log := logTail(path)
			if authError.MatchString(log) {
				return errAuthentication
			}
			if err := watch.sample(ctx, now, t.ready(ctx), log, uplink, reconnect, status); err != nil {
				return err
			}
		}
	}
}

func reconnectNeeded(previous, current string, awake, unhealthy bool) bool {
	return awake || previous != current || unhealthy
}

func logTail(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	data := make([]byte, min(info.Size(), 65536))
	n, _ := file.ReadAt(data, max(0, info.Size()-int64(len(data))))
	return string(data[:n])
}
