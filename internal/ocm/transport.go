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

func (t Transport) ready(ctx context.Context) bool {
	data, err := os.ReadFile(filepath.Join(t.Root, "tunnel"))
	if err != nil {
		return false
	}
	name := strings.TrimSpace(string(data))
	if !tunnelDevice.MatchString(name) {
		return false
	}
	output, err := systemCommand(ctx, "", "/sbin/ifconfig", name)
	return err == nil && strings.Contains(output, "\tinet ")
}

func (t Transport) Run(ctx context.Context, profile Profile, status func(string)) (result error) {
	status("waiting-network")
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
	status("connecting")
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
	started := time.Now()
	lastSample := started
	lastNetwork := started
	reconnecting := time.Time{}
	connected := false
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
			ready := t.ready(ctx)
			if ready && !connected {
				connected = true
				status("connected")
			}
			if !connected && now.Sub(started) > 40*time.Second {
				return fmt.Errorf("OpenConnect tunnel did not become ready")
			}
			if !connected {
				continue
			}
			failed := connectionError.FindAllStringIndex(log, -1)
			ok := established.FindAllStringIndex(log, -1)
			unhealthy := !ready || (len(failed) > 0 && (len(ok) == 0 || failed[len(failed)-1][0] > ok[len(ok)-1][0]))
			if !reconnecting.IsZero() && now.Sub(reconnecting) > 12*time.Second {
				return fmt.Errorf("OpenConnect did not recover after uplink change")
			}
			awake := now.Unix()-lastSample.Unix() > 20
			lastSample = now
			if now.Sub(lastNetwork) < 5*time.Second && !awake && !unhealthy {
				continue
			}
			lastNetwork = now
			current := uplink(ctx)
			if current == "" {
				return fmt.Errorf("physical uplink unavailable")
			}
			if reconnecting.IsZero() && reconnectNeeded(network, current, awake, unhealthy) {
				network = current
				if err := t.restoreDNS(ctx); err != nil {
					return err
				}
				os.Remove(filepath.Join(t.Root, "tunnel"))
				if err := cmd.Process.Signal(syscall.SIGUSR2); err != nil {
					return fmt.Errorf("cannot request OpenConnect reconnect: %w", err)
				}
				reconnecting = now
				status("reconnecting")
			} else if !reconnecting.IsZero() && !unhealthy {
				reconnecting = time.Time{}
				status("connected")
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
