package ocm

import (
	"context"
	"errors"
	"time"
)

type tunnelWatch struct {
	started, lastSample, lastNetwork time.Time
	reconnecting                     time.Time
	network                          string
	readyOnce, verifyingRecovery     bool
	health                           tunnelHealth
	stage, reportedHealth            string
}

func newTunnelWatch(now time.Time, network string, health tunnelHealth) tunnelWatch {
	return tunnelWatch{started: now, lastSample: now, lastNetwork: now, network: network, health: health, stage: "connecting"}
}

func (w *tunnelWatch) publish(state, health string, status func(string, string)) {
	if state != w.stage || health != w.reportedHealth {
		w.stage, w.reportedHealth = state, health
		status(state, health)
	}
}

func (w *tunnelWatch) sample(ctx context.Context, now time.Time, link tunnelLink, log string,
	uplink func(context.Context) string, reconnect func(context.Context) error, status func(string, string),
) error {
	checking := w.health.policy.target.IsValid()
	if link.device != "" && !w.readyOnce {
		w.readyOnce = true
		if !checking {
			w.publish("connected", "", status)
		}
	}
	if !w.readyOnce {
		if now.Sub(w.started) > 40*time.Second {
			return errors.New("OpenConnect tunnel did not become ready")
		}
		return nil
	}

	failed := connectionError.FindAllStringIndex(log, -1)
	ok := established.FindAllStringIndex(log, -1)
	unhealthy := link.device == "" || (len(failed) > 0 && (len(ok) == 0 || failed[len(failed)-1][0] > ok[len(ok)-1][0]))
	if checking && unhealthy {
		w.health.reset()
	}
	if !w.reconnecting.IsZero() && now.Sub(w.reconnecting) > 12*time.Second {
		return errors.New("OpenConnect did not recover after reconnect")
	}
	if checking && !unhealthy && w.reconnecting.IsZero() && w.health.observe(ctx, link, now) {
		// The OpenConnect hook can replace the tunnel without a sampled outage.
		w.verifyingRecovery = w.stage == "connected" || w.verifyingRecovery
	}
	pingFailed := w.health.failures >= 2
	if pingFailed && w.verifyingRecovery {
		return errors.New("VPN health check failed after reconnect")
	}

	awake := now.Unix()-w.lastSample.Unix() > 20
	w.lastSample = now
	sampleNetwork := now.Sub(w.lastNetwork) >= 5*time.Second || awake || unhealthy || pingFailed
	if sampleNetwork {
		w.lastNetwork = now
		current := uplink(ctx)
		if current == "" {
			return errors.New("physical uplink unavailable")
		}
		if w.reconnecting.IsZero() && reconnectNeeded(w.network, current, awake, unhealthy || pingFailed) {
			w.network = current
			w.health.reset()
			if err := reconnect(ctx); err != nil {
				return err
			}
			w.reconnecting = now
			w.verifyingRecovery = checking
			health := ""
			if pingFailed {
				health = "unhealthy"
			}
			w.publish("reconnecting", health, status)
			return nil
		}
	}
	if unhealthy {
		return nil
	}
	if !w.reconnecting.IsZero() {
		if !checking && !sampleNetwork {
			return nil
		}
		w.reconnecting = time.Time{}
		if !checking {
			w.publish("connected", "", status)
			return nil
		}
		w.health.observe(ctx, link, now)
	}
	if !checking {
		return nil
	}
	switch {
	case w.health.value == "healthy":
		w.verifyingRecovery = false
		w.publish("connected", "healthy", status)
	case w.verifyingRecovery:
		health := w.health.value
		if health == "" {
			health = w.reportedHealth
		}
		w.publish("reconnecting", health, status)
	case w.stage == "connected":
		w.publish("connected", w.health.value, status)
	default:
		w.publish("connecting", w.health.value, status)
	}
	return nil
}
