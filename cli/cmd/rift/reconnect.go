package main

import (
	"context"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fixed-labs/oss/cli/internal/client"
	"github.com/fixed-labs/oss/cli/internal/tunnel"
)

// sessionKeeper owns every post-connect Attach call and peer-endpoint mutation
// for one session, so the lease-refresh loop and the relay-reassign watcher can't
// race two different endpoints onto the live device. A single mutex serializes
// Attach→UpdatePeerEndpoint as one unit; Attach is idempotent for the same pubkey
// and always returns the pairing's CURRENT relay coords, so re-applying converges.
type sessionKeeper struct {
	c   *client.Client
	t   *tunnel.Tunnel
	id  string
	pub string
	mu  sync.Mutex
}

// reattach refreshes the attachment lease AND re-points the wireguard peer at
// whatever relay the pairing currently lives on (a drain/failover can move it).
// Safe to call from multiple loops — serialized and idempotent.
func (k *sessionKeeper) reattach(ctx context.Context) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	actx, cancel := ctxTimeout(ctx, 30*time.Second)
	defer cancel()
	bundle, err := k.c.Attach(actx, k.id, k.pub, "")
	if err != nil {
		return err
	}
	endpoint := net.JoinHostPort(bundle.RelayPublicEndpoint, strconv.Itoa(bundle.RelayPort))
	return k.t.UpdatePeerEndpoint(bundle.WorkspaceWgPubkey, endpoint)
}

// leaseLoop keeps the attachment lease alive on a half-lease cadence (and, via
// reattach, keeps the peer endpoint current). Replaces the old refreshAttachment:
// same lease contract, now funneled through the shared reattach so it can't race
// the relay watcher.
func (k *sessionKeeper) leaseLoop(ctx context.Context) {
	wait := attachRefreshInterval
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := k.reattach(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			slog.Warn("attachment lease refresh failed; will retry", "err", err)
			wait = attachRefreshRetry
		} else {
			wait = attachRefreshInterval
		}
	}
}

// watchRelay long-polls the workspace row; when the relay endpoint/id changes
// (drain / failover), it re-attaches so the live tunnel FOLLOWS the pairing to its
// new relay without dropping the session. The SSH session over the tunnel blips
// and the reconnect loop re-attaches the SAME persistent session (the box-side
// session survives the transport drop). (The lease loop's periodic reattach is the
// backstop if a move ever lands without changing the row's relay fields.)
func (k *sessionKeeper) watchRelay(ctx context.Context) {
	// Snapshot current relay coords, then long-poll for changes. If the snapshot
	// fails we don't seed a fake "" baseline (which would trip a spurious reattach
	// on the first real row); instead we adopt the first row we see as the
	// baseline and only reattach on a SUBSEQUENT change.
	gctx, cancel := ctxTimeout(ctx, 30*time.Second)
	ws, cursor, err := k.c.Get(gctx, k.id, "")
	cancel()
	var relayID, relayEP string
	haveBaseline := false
	if err == nil && ws.WorkspaceID != "" {
		relayID, relayEP = ws.RelayID, ws.RelayEndpoint
		haveBaseline = true
	}
	for {
		if ctx.Err() != nil {
			return
		}
		pctx, pcancel := ctxTimeout(ctx, 45*time.Second)
		next, ncursor, err := k.c.Get(pctx, k.id, cursor)
		pcancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		cursor = ncursor
		if next.WorkspaceID == "" {
			continue // long-poll timed out with no change (304); re-poll
		}
		if !haveBaseline {
			relayID, relayEP = next.RelayID, next.RelayEndpoint
			haveBaseline = true
			continue // first row seen → establish baseline, don't reattach
		}
		if next.RelayID != relayID || next.RelayEndpoint != relayEP {
			relayID, relayEP = next.RelayID, next.RelayEndpoint
			if err := k.reattach(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("relay reattach failed; will retry on next lease refresh", "err", err)
			}
		}
	}
}

// watchNetworkChanges rebinds the wireguard socket whenever the laptop's set of
// non-loopback IPs changes (sleep/wake, Wi-Fi switch, VPN up/down). Portable — it
// polls InterfaceAddrs rather than depending on netlink (Linux) or SCNetwork
// notifications (macOS), so reconnect-on-resume works the same everywhere. Cheap:
// a Rebind fires only on an actual address-set change, and the SSH session
// recovers (or the reconnect loop re-attaches) after it.
func watchNetworkChanges(ctx context.Context, t *tunnel.Tunnel) {
	last := localAddrSet()
	tk := time.NewTicker(3 * time.Second)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		if cur := localAddrSet(); cur != last {
			last = cur
			if err := t.Rebind(); err != nil && ctx.Err() == nil {
				slog.Warn("wireguard rebind failed", "err", err)
			}
		}
	}
}

// localAddrSet is a stable fingerprint of the laptop's non-loopback IPs.
func localAddrSet() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return ""
	}
	ips := make([]string, 0, len(addrs))
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
			ips = append(ips, ipn.IP.String())
		}
	}
	sort.Strings(ips)
	return strings.Join(ips, ",")
}
