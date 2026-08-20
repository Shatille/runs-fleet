// Command mirror-proxy serves the local Docker Hub mirror on runs-fleet
// runners (see pkg/mirrorproxy), configured by ECR_PULL_THROUGH_ENDPOINT.
// Host-local only: the mirror is a per-host convenience for dockerd and
// BuildKit, never a network service.
//
// It binds more than loopback because its two clients sit in different network
// namespaces. dockerd runs in the host's, so loopback reaches it. But buildx's
// docker-container driver runs buildkitd in a bridge-network container, whose
// 127.0.0.1 is its own — so BuildKit reaches the mirror only via the docker
// bridge gateway, routable from every container on the host and from nowhere
// off it. Binding the primary ENI instead would put a credential-injecting
// proxy on the VPC.
//
// The gateway is discovered from the bridge interface rather than assumed:
// dockerd picks the bridge subnet from an address pool and steps around
// collisions with the host's own networks, so 172.17.0.1 is its usual first
// choice, not a guarantee.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/ec2/imds"
	"github.com/aws/aws-sdk-go-v2/service/ecr"

	"github.com/Shavakan/runs-fleet/pkg/mirrorproxy"
)

// resolveRegion returns cfgRegion when the ambient config already resolved one,
// otherwise the region reported by fromIMDS. A systemd unit starts with a
// near-empty environment, so AWS_REGION is usually absent here and the SDK's
// own IMDS lookup stays dormant unless explicitly opted into; without a region
// every ECR call fails and the mirror can only answer 502.
func resolveRegion(ctx context.Context, cfgRegion string, fromIMDS func(context.Context) (string, error)) (string, error) {
	if cfgRegion != "" {
		return cfgRegion, nil
	}
	region, err := fromIMDS(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve region from IMDS: %w", err)
	}
	if region == "" {
		return "", fmt.Errorf("IMDS returned no region")
	}
	return region, nil
}

func imdsRegion(awsCfg aws.Config) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		out, err := imds.NewFromConfig(awsCfg).GetRegion(ctx, &imds.GetRegionInput{})
		if err != nil {
			return "", err
		}
		return out.Region, nil
	}
}

// DefaultListen is the address set that must bind for the proxy to serve at
// all. The deployed value is passed explicitly by the systemd unit in
// packer/provision-runs-fleet.sh, which owns the port; this default only makes
// the binary runnable by hand.
const DefaultListen = "127.0.0.1:8989"

// DefaultBridgeInterface is the docker bridge whose gateway BuildKit reaches
// the mirror on. Binding it is best-effort: losing it costs buildx builds the
// mirror, but failing outright would also cost dockerd the loopback mirror
// that was working — strictly worse than the problem being reported.
const DefaultBridgeInterface = "docker0"

// parseListenAddrs splits the -listen flag into the addresses to bind. It
// dedups (a repeat would make the second bind fail as "address already in
// use") and rejects empty entries rather than quietly binding a smaller set
// than the operator asked for — a mirror missing one of its two namespaces
// serves some clients and silently sheds the rest to Docker Hub.
func parseListenAddrs(flagValue string) ([]string, error) {
	var (
		addrs []string
		seen  = map[string]bool{}
	)
	for _, field := range strings.Split(flagValue, ",") {
		addr := strings.TrimSpace(field)
		if addr == "" {
			return nil, fmt.Errorf("empty address in -listen %q", flagValue)
		}
		if seen[addr] {
			continue
		}
		seen[addr] = true
		addrs = append(addrs, addr)
	}
	if len(addrs) == 0 {
		return nil, errors.New("-listen is empty; nothing to serve the mirror on")
	}
	return addrs, nil
}

// portOf returns the port half of a host:port address. The bridge listener
// takes its port from the required listen set rather than a second flag, so
// the two cannot drift apart.
func portOf(addr string) (string, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", addr, err)
	}
	return port, nil
}

// discoverBridgeAddr returns the host:port to serve the mirror on for the
// docker bridge, given a function yielding an interface's addresses
// (net.InterfaceByName(...).Addrs in production).
func discoverBridgeAddr(addrsOf func(string) ([]net.Addr, error), iface, port string) (string, error) {
	addrs, err := addrsOf(iface)
	if err != nil {
		return "", fmt.Errorf("look up %s: %w", iface, err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return net.JoinHostPort(ip4.String(), port), nil
		}
	}
	return "", fmt.Errorf("%s has no IPv4 address", iface)
}

func interfaceAddrs(name string) ([]net.Addr, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return nil, err
	}
	return iface.Addrs()
}

// listenAll binds every address or none. A half-bound required set must not
// look healthy: the mirror would answer one namespace and refuse the other,
// and BuildKit turns a refused mirror into an unauthenticated Docker Hub pull
// instead of an error.
func listenAll(addrs []string) ([]net.Listener, error) {
	listeners := make([]net.Listener, 0, len(addrs))
	for _, addr := range addrs {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			for _, opened := range listeners {
				_ = opened.Close()
			}
			return nil, fmt.Errorf("listen on %s: %w", addr, err)
		}
		listeners = append(listeners, l)
	}
	return listeners, nil
}

// bindBridge binds the docker bridge gateway best-effort, returning the
// listener and the address it bound. Every failure path returns ("", nil):
// losing the bridge costs buildx builds the mirror, but exiting would also
// cost dockerd the loopback mirror it was already using. The empty address is
// load-bearing — reconcileBuildkitdConfig removes the config on it, so a
// failure here can never leave BuildKit pointed at an address nothing serves.
func bindBridge(
	iface, portSource string,
	addrsOf func(string) ([]net.Addr, error),
	listen func(network, address string) (net.Listener, error),
	logger *slog.Logger,
) (net.Listener, string) {
	if iface == "" {
		return nil, ""
	}
	port, err := portOf(portSource)
	if err != nil {
		logger.Error("cannot derive the bridge port from -listen; BuildKit will bypass the mirror and pull from Docker Hub",
			"listen", portSource, "error", err)
		return nil, ""
	}
	bridgeAddr, err := discoverBridgeAddr(addrsOf, iface, port)
	if err != nil {
		logger.Error("no docker bridge address to bind; BuildKit will bypass the mirror and pull from Docker Hub",
			"interface", iface, "error", err)
		return nil, ""
	}
	l, err := listen("tcp", bridgeAddr)
	if err != nil {
		logger.Error("could not bind the docker bridge address; BuildKit will bypass the mirror and pull from Docker Hub",
			"addr", bridgeAddr, "error", err)
		return nil, ""
	}
	return l, bridgeAddr
}

// reconcileBuildkitdConfig points BuildKit at mirrorAddr, or removes the
// config when mirrorAddr is empty. It is called from the address actually
// bound and before serving starts, so the readiness probe on the listening
// port also implies the config is in place for buildx-setup.service and the
// buildx shim. A config naming an unbound address looks like a working mirror
// and silently sheds builds to Docker Hub; absence makes the shim skip
// cleanly instead.
func reconcileBuildkitdConfig(path, mirrorAddr string, rules map[string]string, logger *slog.Logger) {
	if path == "" {
		return
	}
	if mirrorAddr == "" {
		if err := removeBuildkitdConfig(path); err != nil {
			logger.Error("could not remove the stale BuildKit mirror config", "path", path, "error", err)
		}
		return
	}
	if err := writeBuildkitdConfig(path, buildkitdConfig(mirrorAddr, rules)); err != nil {
		logger.Error("could not write the BuildKit mirror config; BuildKit will bypass the mirror and pull from Docker Hub",
			"path", path, "error", err)
		return
	}
	logger.Info("wrote BuildKit mirror config", "path", path, "mirror", mirrorAddr)
}

type bridgeParams struct {
	iface      string
	portSource string
	configPath string
	rules      map[string]string
	addrsOf    func(string) ([]net.Addr, error)
	listen     func(network, address string) (net.Listener, error)
	logger     *slog.Logger
}

// addBridge appends the best-effort bridge listener to the required set and
// reconciles the BuildKit config against whatever actually bound. The two
// steps belong together: reconcileBuildkitdConfig must see the address
// bindBridge really bound, so a bridge failure removes the config instead of
// leaving BuildKit aimed at a port nothing serves.
func addBridge(p bridgeParams, listeners []net.Listener, addrs []string) ([]net.Listener, []string) {
	bridgeListener, boundBridge := bindBridge(p.iface, p.portSource, p.addrsOf, p.listen, p.logger)
	if bridgeListener != nil {
		listeners = append(listeners, bridgeListener)
		addrs = append(addrs, boundBridge)
	}
	reconcileBuildkitdConfig(p.configPath, boundBridge, p.rules, p.logger)
	return listeners, addrs
}

func main() {
	listen := flag.String("listen", DefaultListen,
		"comma-separated host-local addresses that must bind to serve the mirror")
	bridge := flag.String("bridge-interface", DefaultBridgeInterface,
		"docker bridge whose gateway is additionally bound, best-effort; empty disables")
	buildkitdConfigPath := flag.String("buildkitd-config", DefaultBuildkitdConfig,
		"path to write the BuildKit mirror configuration to; empty disables")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	endpoint := os.Getenv("ECR_PULL_THROUGH_ENDPOINT")
	if endpoint == "" {
		logger.Error("ECR_PULL_THROUGH_ENDPOINT is not set; nothing to mirror onto")
		os.Exit(1)
	}

	awsCfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		logger.Error("failed to load AWS config", "error", err)
		os.Exit(1)
	}

	region, err := resolveRegion(context.Background(), awsCfg.Region, imdsRegion(awsCfg))
	if err != nil {
		logger.Error("no AWS region available; every ECR call would fail and the mirror could only serve 502", "error", err)
		os.Exit(1)
	}
	awsCfg.Region = region

	ecrClient := ecr.NewFromConfig(awsCfg)
	handler, err := mirrorproxy.New(endpoint, mirrorproxy.NewECRTokenSource(ecrClient))
	if err != nil {
		logger.Error("invalid mirror configuration", "endpoint", endpoint, "error", err)
		os.Exit(1)
	}
	rules, err := mirrorproxy.DiscoverRules(context.Background(), ecrClient)
	if err != nil {
		logger.Error("pull-through rule discovery failed; refusing to serve a mirror that would 502 every pull", "error", err)
		os.Exit(1)
	}
	handler.AddRules(rules)
	logger.Info("mirror routing discovered", "rules", rules)

	addrs, err := parseListenAddrs(*listen)
	if err != nil {
		logger.Error("invalid -listen", "listen", *listen, "error", err)
		os.Exit(1)
	}
	listeners, err := listenAll(addrs)
	if err != nil {
		logger.Error("could not bind every required mirror address", "listen", addrs, "error", err)
		os.Exit(1)
	}

	required := len(listeners)

	listeners, addrs = addBridge(bridgeParams{
		iface:      *bridge,
		portSource: addrs[0],
		configPath: *buildkitdConfigPath,
		rules:      rules,
		addrsOf:    interfaceAddrs,
		listen:     net.Listen,
		logger:     logger,
	}, listeners, addrs)

	server := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	logger.Info("mirror proxy serving", "listen", addrs, "endpoint", endpoint)

	// One Serve per listener. A required listener failing takes the process
	// down so systemd restarts it; the best-effort bridge listener failing
	// must not, or a bridge problem would also take away the loopback mirror
	// dockerd is using.
	failed := make(chan error, len(listeners))
	for i, l := range listeners {
		go func(l net.Listener, required bool) {
			err := fmt.Errorf("serve %s: %w", l.Addr(), server.Serve(l))
			if !required {
				logger.Error("the docker bridge listener stopped; BuildKit will bypass the mirror and pull from Docker Hub", "error", err)
				return
			}
			failed <- err
		}(l, i < required)
	}
	logger.Error("mirror proxy exited", "error", <-failed)
	os.Exit(1)
}
