package ocm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Request struct {
	Action  string `json:"action"`
	Profile string `json:"profile,omitempty"`
}

type Status struct {
	State        string `json:"state"`
	Profile      string `json:"profile,omitempty"`
	SavedProfile string `json:"saved_profile,omitempty"`
	Error        string `json:"error,omitempty"`
}

type runner func(context.Context, Profile, func(string)) error

type daemon struct {
	ctx        context.Context
	root, hook string
	run        runner
	load       func(string) (Profile, error)
	control    sync.Mutex
	mu         sync.Mutex
	status     Status
	cancel     context.CancelFunc
	done       chan struct{}
}

func (d *daemon) loadProfile(name string) (Profile, error) {
	if d.load != nil {
		return d.load(name)
	}
	return LoadProfile(d.root, name, d.hook)
}

func (d *daemon) snapshot() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

func (d *daemon) setState(state string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status.State = state
}

func (d *daemon) stopWorker() {
	if d.cancel != nil {
		d.cancel()
		<-d.done
		d.cancel = nil
	}
	d.mu.Lock()
	d.status.Profile = ""
	d.status.State = "stopped"
	d.mu.Unlock()
}

func (d *daemon) launch(name string, persistent bool) <-chan struct{} {
	ctx, cancel := context.WithCancel(d.ctx)
	done := make(chan struct{})
	d.cancel, d.done = cancel, done
	d.mu.Lock()
	d.status.Profile = name
	d.status.State = "waiting-network"
	d.mu.Unlock()
	go func() {
		defer close(done)
		delay := 5 * time.Second
		for ctx.Err() == nil {
			profile, err := d.loadProfile(name)
			if err != nil {
				d.setState("profile-unavailable")
				if !persistent || !pause(ctx, delay) {
					return
				}
				delay = min(delay*2, 5*time.Minute)
				continue
			}
			err = d.run(ctx, profile, func(state string) {
				if state == "connected" {
					delay = 5 * time.Second
				}
				d.setState(state)
			})
			if ctx.Err() != nil {
				d.setState("stopped")
				return
			}
			if errors.Is(err, errAuthentication) {
				d.setState("authentication-required")
				return
			}
			if err == nil {
				d.setState("stopped")
			} else {
				d.setState("failed")
				fmt.Fprintln(os.Stderr, "ocm:", err)
			}
			if !persistent || !pause(ctx, delay) {
				return
			}
			delay = min(delay*2, 5*time.Minute)
		}
	}()
	return done
}

func (d *daemon) handle(req Request) (Status, <-chan struct{}, error) {
	if req.Action == "status" {
		return d.snapshot(), nil, nil
	}
	d.control.Lock()
	defer d.control.Unlock()
	if d.ctx.Err() != nil {
		return Status{}, nil, d.ctx.Err()
	}
	current := d.snapshot()
	switch req.Action {
	case "toggle":
		if !profileName.MatchString(req.Profile) {
			return Status{}, nil, fmt.Errorf("invalid profile name")
		}
		if current.SavedProfile != "" || (current.Profile != "" && current.State != "stopped" && current.State != "failed" && current.State != "authentication-required") {
			req.Action = "stop"
		} else {
			req.Action = "start"
		}
	case "start", "stop", "connect":
	default:
		return Status{}, nil, fmt.Errorf("unsupported command")
	}
	if req.Action != "stop" {
		if _, err := d.loadProfile(req.Profile); err != nil {
			return Status{}, nil, err
		}
	}
	if req.Action == "connect" && (current.SavedProfile != "" || (current.Profile != "" && current.State != "stopped" && current.State != "failed" && current.State != "authentication-required")) {
		return Status{}, nil, fmt.Errorf("stop the existing session before foreground connect")
	}
	if req.Action == "start" && current.SavedProfile == req.Profile && current.State == "connected" {
		return current, nil, nil
	}
	if req.Action != "connect" {
		saved := ""
		if req.Action == "start" {
			saved = req.Profile
		}
		if err := writeJSON(filepath.Join(d.root, "desired.json"), intent{Profile: saved}); err != nil {
			return Status{}, nil, fmt.Errorf("cannot save connection state")
		}
		d.mu.Lock()
		d.status.SavedProfile = saved
		d.mu.Unlock()
	}
	d.stopWorker()
	if req.Action == "stop" {
		return d.snapshot(), nil, nil
	}
	done := d.launch(req.Profile, req.Action == "start")
	return d.snapshot(), done, nil
}

func (d *daemon) serve(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var req Request
	if err := json.NewDecoder(io.LimitReader(conn, 4096)).Decode(&req); err != nil {
		return
	}
	conn.SetReadDeadline(time.Time{})
	status, done, err := d.handle(req)
	if err != nil {
		status.Error = err.Error()
	}
	encoder := json.NewEncoder(conn)
	if err := encoder.Encode(status); req.Action != "connect" || done == nil {
		return
	} else if err == nil {
		closed := make(chan struct{})
		go func() {
			var buf [1]byte
			conn.Read(buf[:])
			close(closed)
		}()
		select {
		case <-done:
			encoder.Encode(d.snapshot())
			return
		case <-closed:
		}
	}
	d.control.Lock()
	defer d.control.Unlock()
	if done == d.done {
		d.stopWorker()
	}
}

func Serve(ctx context.Context, root, hook string, transport Transport) error {
	if err := CheckRoot(root); err != nil {
		return err
	}
	if err := CheckRoot(filepath.Join(root, "profiles")); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(root, "daemon.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("another OCM daemon owns this state: %w", err)
	}
	saved, err := readIntent(root)
	if err != nil {
		return err
	}
	socket := filepath.Join(root, "control.sock")
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		return err
	}
	d := daemon{ctx: ctx, root: root, hook: hook, run: transport.Run, status: Status{State: "stopped", SavedProfile: saved}}
	if saved != "" {
		d.launch(saved, true)
	} else if err := transport.cleanup(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ocm: could not clean stale network state:", err)
	}
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			d.control.Lock()
			d.stopWorker()
			d.control.Unlock()
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.serve(conn)
	}
}

func pause(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
