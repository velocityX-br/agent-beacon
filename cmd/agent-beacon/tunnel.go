package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"time"

	"github.com/mdp/qrterminal/v3"
)

// trycloudflareURL matches the public URL cloudflared prints for a quick
// tunnel, e.g. https://random-words-here.trycloudflare.com.
var trycloudflareURL = regexp.MustCompile(`https://[a-z0-9-]+\.trycloudflare\.com`)

// startTunnel launches `cloudflared tunnel --url http://127.0.0.1:<port>` as a
// child process tied to ctx (so it is killed automatically on server shutdown),
// scans its stderr for the assigned *.trycloudflare.com URL, and — on the first
// match — logs a banner and renders a scannable QR code plus the URL to stdout
// so a phone can open it immediately.
//
// protocol selects the edge transport passed to cloudflared via --protocol.
// The default "http2" avoids QUIC (outbound UDP/7844), which corporate networks
// and VPNs frequently block — a blocked QUIC dial leaves the tunnel with no
// registered edge connection, so visitors get Cloudflare error 1033/530.
// "http2" registers over TCP/443 and works everywhere DNS+HTTPS work. Pass
// "auto" to restore cloudflared's own QUIC-first default. An empty string is
// treated as no override (cloudflared's default).
func startTunnel(ctx context.Context, bin, port, protocol string, log *slog.Logger) (*exec.Cmd, error) {
	target := fmt.Sprintf("http://127.0.0.1:%s", port)
	args := []string{"tunnel", "--url", target}
	if protocol != "" && protocol != "auto" {
		args = append(args, "--protocol", protocol)
	}
	cmd := exec.CommandContext(ctx, bin, args...)

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("tunnel: capturing cloudflared output: %w", err)
	}

	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("tunnel: %q not found on PATH — install cloudflared or pass --tunnel-cmd (see docs/remote-access.md)", bin)
		}
		return nil, fmt.Errorf("tunnel: starting cloudflared: %w", err)
	}

	log.Info("cloudflared started; waiting for public URL", "target", target)

	// Scan stderr for the public URL. cloudflared keeps logging there for the
	// life of the tunnel, so keep draining after the first match to avoid the
	// pipe buffer filling and blocking the child.
	found := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		reported := false
		for scanner.Scan() {
			line := scanner.Text()
			if !reported {
				if m := trycloudflareURL.FindString(line); m != "" {
					reported = true
					select {
					case found <- m:
					default:
					}
				}
			}
		}
	}()

	go func() {
		select {
		case url := <-found:
			log.Info("public tunnel ready", "url", url)
			fmt.Fprintf(os.Stdout, "\n  agent-beacon is reachable at:\n\n    %s\n\n  Scan to open on your phone:\n\n", url)
			qrterminal.GenerateHalfBlock(url, qrterminal.L, os.Stdout)
			fmt.Fprintf(os.Stdout, "\n")
		case <-time.After(20 * time.Second):
			log.Warn("no trycloudflare URL seen within 20s; the tunnel may still be establishing — check cloudflared output")
		case <-ctx.Done():
		}
	}()

	return cmd, nil
}
