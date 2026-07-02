package tunnel

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strings"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// overlayMTU caps the userspace tunnel below the underlay so encapsulated
// frames fit through the relay. The box reaches its relay over Fly's 6PN
// (eth0 MTU 1420) and WireGuard over IPv6 adds 80 bytes, so 1420-byte inner
// packets (wireguard-go's device.DefaultMTU) become 1500-byte frames the box's
// eth0 cannot egress — large frames (e.g. the SSH KEXINIT) drop and the SSH
// connection reaches ESTAB then hangs in KEX. 1280 (the IPv6 minimum link MTU)
// clears the 1420-80=1340 ceiling. Must match the agent's overlayMTU.
const overlayMTU = 1280

// Tunnel is a live userspace WireGuard session: a wireguard-go device on an
// in-process gVisor netstack (no TUN, no root). Dial reaches the workspace
// over the overlay entirely within this process; BridgeSSH exposes a TCP port
// on the box as a localhost listener so the external `ssh` binary can use it;
// ListenOverlayTCP accepts box→laptop connections on the laptop's overlay IP
// (the secret-broker handler channel).
type Tunnel struct {
	dev    *device.Device
	tnet   *netstack.Net
	wgAddr netip.Addr // the laptop's overlay /128 — the only address on the netstack
}

// transientNetErrPatterns are the substrings wireguard-go emits, at error level
// and in a tight loop, while the laptop has no network path — i.e. across a
// sleep/wake or a Wi-Fi switch the kernel returns ENETUNREACH/ENETDOWN/EHOSTUNREACH
// on every keepalive and handshake-retransmit send ("peer(…) - Failed to send
// data packets: write udp …: sendmmsg: network is unreachable"). The bind is a
// wildcard UDP socket that recovers on its own once routing returns (on Linux
// wireguard-go's own route listener clears the stale source address), so these
// lines are pure noise. Up() routes the wg logger to slog (the diag logfile), so
// they no longer trample the terminal — but at this frequency they'd still bury
// the real signal in the log, so we drop them and forward every other error
// (handshake failures, config errors, etc. still surface in the log).
var transientNetErrPatterns = []string{
	"network is unreachable",
	"network is down",
	"no route to host",
	"host is unreachable",
}

// quietTransientNetErrs wraps a Printf-style error logger, dropping the transient
// no-network send/receive failures above and forwarding everything else verbatim.
func quietTransientNetErrs(errorf func(string, ...any)) func(string, ...any) {
	return func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		for _, p := range transientNetErrPatterns {
			if strings.Contains(msg, p) {
				return
			}
		}
		errorf(format, args...)
	}
}

// Up brings up the laptop end for one pairing: a netstack with the laptop's
// overlay /128, a wireguard-go device configured from params, and the link
// raised. The handshake completes when the workspace's wg0 has this laptop as
// an authorized peer (the api recorded it on Attach).
func Up(ctx context.Context, params Params, laptopWgIP string) (*Tunnel, error) {
	addr, err := netip.ParseAddr(laptopWgIP)
	if err != nil {
		return nil, fmt.Errorf("laptop wg ip %q: %w", laptopWgIP, err)
	}
	// netstack with the laptop's overlay address; no DNS (we dial by IP).
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{addr}, nil, overlayMTU)
	if err != nil {
		return nil, err
	}
	// Route wireguard-go's logger to slog (→ the diag logfile) instead of its
	// default stdout writer, which would trample the connect compositor's screen.
	lg := device.NewLogger(device.LogLevelError, "devbox-wg ")
	lg.Errorf = quietTransientNetErrs(func(format string, args ...any) {
		slog.Warn("wireguard", "msg", strings.TrimSpace(fmt.Sprintf(format, args...)))
	})
	dev := device.NewDevice(tun, conn.NewDefaultBind(), lg)
	uapi, err := params.UAPIConfig()
	if err != nil {
		dev.Close()
		return nil, err
	}
	if err := dev.IpcSet(uapi); err != nil {
		dev.Close()
		return nil, fmt.Errorf("configure wg device: %w", err)
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, err
	}
	return &Tunnel{dev: dev, tnet: tnet, wgAddr: addr}, nil
}

// DialContext dials a TCP address ON THE OVERLAY through the netstack.
func (t *Tunnel) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.tnet.DialContext(ctx, network, addr)
}

// ListenOverlayTCP listens for inbound TCP on the laptop's overlay (WireGuard)
// IP at the given port, returning a net.Listener that accepts box→laptop
// connections over the tunnel. This is the secret-broker handler channel: the
// box dials
// [laptop-overlay-ip]:port directly over the existing tunnel (the same return
// path the shell uses). The netstack admits only the box's authorized /128, so
// WireGuard cryptokey routing is the trust boundary — no other host can
// reach this listener. The listener lives until it is Closed (the handler closes
// it on session end).
func (t *Tunnel) ListenOverlayTCP(port int) (net.Listener, error) {
	return t.tnet.ListenTCP(&net.TCPAddr{IP: t.wgAddr.AsSlice(), Port: port})
}

// BridgeSSH listens on a loopback port and forwards each accepted connection
// to wgIP:port over the tunnel, returning the local "127.0.0.1:N" address for
// the ssh binary to dial. The listener lives until the tunnel closes.
func (t *Tunnel) BridgeSSH(ctx context.Context, wgIP string, port int) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	target := net.JoinHostPort(wgIP, fmt.Sprintf("%d", port))
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	go func() {
		for {
			local, err := ln.Accept()
			if err != nil {
				return
			}
			go t.proxy(ctx, local, target)
		}
	}()
	return ln.Addr().String(), nil
}

func (t *Tunnel) proxy(ctx context.Context, local net.Conn, target string) {
	defer local.Close()
	remote, err := t.tnet.DialContext(ctx, "tcp", target)
	if err != nil {
		return
	}
	defer remote.Close()
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(remote, local); done <- struct{}{} }()
	go func() { _, _ = io.Copy(local, remote); done <- struct{}{} }()
	<-done
}

// Rebind re-opens the underlying UDP socket (wireguard-go BindUpdate): called
// after a laptop network change (sleep/wake, Wi-Fi switch) to drop the stale
// socket and pick a fresh source path immediately, rather than waiting out
// keepalive/handshake timeouts. (On Linux wireguard-go also clears stale source
// addresses via its own route listener; this is the belt-and-suspenders trigger,
// and the only recovery nudge on platforms without that listener.)
func (t *Tunnel) Rebind() error {
	return t.dev.BindUpdate()
}

// UpdatePeerEndpoint re-points the workspace peer at a new relay endpoint
// ("host:port") on the LIVE device via UAPI update_only — used when a drain /
// failover moves the pairing to a different relay, so the session follows without
// a teardown. The Noise session is untouched; wireguard-go re-handshakes through
// the new endpoint transparently.
func (t *Tunnel) UpdatePeerEndpoint(workspacePubkeyB64, endpoint string) error {
	pubHex, err := b64ToHex(workspacePubkeyB64)
	if err != nil {
		return fmt.Errorf("workspace pubkey: %w", err)
	}
	return t.dev.IpcSet(fmt.Sprintf("public_key=%s\nupdate_only=true\nendpoint=%s\n", pubHex, endpoint))
}

func (t *Tunnel) Close() error {
	if t.dev != nil {
		t.dev.Close()
	}
	return nil
}
