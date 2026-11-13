package ocm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeChild struct {
	termErr, killErr error
	onTerm, onKill   func()
	terms, kills     int
}

func (p *fakeChild) Signal(os.Signal) error {
	p.terms++
	if p.onTerm != nil {
		p.onTerm()
	}
	return p.termErr
}

func (p *fakeChild) Kill() error {
	p.kills++
	if p.onKill != nil {
		p.onKill()
	}
	return p.killErr
}

func TestUnconfirmedChildExitKeepsRecoveryState(t *testing.T) {
	done := make(chan error)
	child := &fakeChild{termErr: errors.New("term denied"), killErr: errors.New("kill denied")}
	cleaned := false
	result := make(chan error, 1)
	go func() {
		result <- finishSession(child, done, false, 5*time.Millisecond, 5*time.Millisecond, func() error {
			cleaned = true
			return nil
		})
	}()
	select {
	case err := <-result:
		if !errors.Is(err, errRecovery) || !strings.Contains(err.Error(), "term denied") || !strings.Contains(err.Error(), "kill denied") {
			t.Fatalf("unconfirmed exit error = %v", err)
		}
		if cleaned {
			t.Fatal("network recovery ran without confirmed child exit")
		}
	case <-time.After(time.Second):
		close(done)
		<-result
		t.Fatal("shutdown did not finish within its deadline")
	}
}

func TestConfirmedChildExitAllowsNetworkRecovery(t *testing.T) {
	for _, forced := range []bool{false, true} {
		done := make(chan error)
		child := &fakeChild{}
		if forced {
			child.onKill = func() { close(done) }
		} else {
			child.onTerm = func() { close(done) }
		}
		cleaned := false
		err := finishSession(child, done, false, time.Millisecond, 5*time.Millisecond, func() error {
			cleaned = true
			return nil
		})
		if err != nil || !cleaned || child.terms != 1 || (child.kills == 1) != forced {
			t.Fatalf("forced=%v: err=%v cleaned=%v term=%d kill=%d", forced, err, cleaned, child.terms, child.kills)
		}
	}
}

func TestSignalErrorIsReportedAfterConfirmedExit(t *testing.T) {
	done := make(chan error)
	child := &fakeChild{killErr: errors.New("kill denied")}
	child.onKill = func() { close(done) }
	cleaned := false
	err := finishSession(child, done, false, time.Millisecond, 5*time.Millisecond, func() error {
		cleaned = true
		return nil
	})
	if err == nil || errors.Is(err, errRecovery) || !strings.Contains(err.Error(), "kill denied") || !cleaned {
		t.Fatalf("err=%v cleaned=%v", err, cleaned)
	}
}

func TestCleanupFailureRetainsRecoveryError(t *testing.T) {
	child := &fakeChild{}
	err := finishSession(child, nil, true, 0, 0, func() error { return errors.New("resolver recovery failed") })
	if !errors.Is(err, errRecovery) || !strings.Contains(err.Error(), "resolver recovery failed") || child.terms != 0 || child.kills != 0 {
		t.Fatalf("err=%v term=%d kill=%d", err, child.terms, child.kills)
	}
}

func TestRecoveryRefusesWhileOpenConnectStillExists(t *testing.T) {
	root := t.TempDir()
	journal := filepath.Join(root, "dns.json")
	original := []byte(`{"Service":"Wi-Fi","Applied":"192.0.2.1"}`)
	if err := os.WriteFile(journal, original, 0600); err != nil {
		t.Fatal(err)
	}
	transport := Transport{Root: root, command: func(_ context.Context, _, path string, args ...string) (string, error) {
		if path != "/usr/bin/pgrep" {
			t.Fatalf("unexpected network recovery command: %s", path)
		}
		return "1234", nil
	}}
	if err := transport.cleanup(context.Background()); err == nil {
		t.Fatal("recovery proceeded while OpenConnect was running")
	}
	if data, err := os.ReadFile(journal); err != nil || string(data) != string(original) {
		t.Fatalf("recovery journal changed: %q, %v", data, err)
	}
}
