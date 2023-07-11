package caddy_docker_upstreams

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp/reverseproxy"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"go.uber.org/zap"
)

const (
	LabelEnable       = "com.caddyserver.http.enable"
	LabelMatchHost    = "com.caddyserver.http.matchers.host"
	LabelMatchPath    = "com.caddyserver.http.matchers.path"
	LabelUpstreamPort = "com.caddyserver.http.upstream.port"
)

func init() {
	caddy.RegisterModule(&Upstreams{})
}

// Upstreams provides upstreams from the docker host.
type Upstreams struct {
	logger *zap.Logger

	mu         sync.RWMutex
	containers []types.Container
}

func (u *Upstreams) CaddyModule() caddy.ModuleInfo {
	return caddy.ModuleInfo{
		ID:  "http.reverse_proxy.upstreams.docker",
		New: func() caddy.Module { return new(Upstreams) },
	}
}

func (u *Upstreams) keepUpdated(ctx context.Context, cli *client.Client) {
	for {
		messages, errs := cli.Events(ctx, types.EventsOptions{
			Filters: filters.NewArgs(filters.Arg("type", events.ContainerEventType)),
		})

	selectLoop:
		for {
			select {
			case <-messages:
				containers, err := cli.ContainerList(ctx, types.ContainerListOptions{
					Filters: filters.NewArgs(filters.Arg("label", LabelEnable)),
				})
				if err != nil {
					u.logger.Error("unable to get the list of containers", zap.Error(err))
					continue
				}

				u.mu.Lock()
				u.containers = containers
				u.mu.Unlock()
			case err := <-errs:
				if errors.Is(err, context.Canceled) {
					return
				}

				u.logger.Warn("unable to monitor container events; will retry", zap.Error(err))
				break selectLoop
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (u *Upstreams) Provision(ctx caddy.Context) error {
	u.logger = ctx.Logger()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}

	ping, err := cli.Ping(ctx)
	if err != nil {
		return err
	}

	u.logger.Info("docker engine is connected", zap.String("api_version", ping.APIVersion))

	options := types.ContainerListOptions{
		Filters: filters.NewArgs(filters.Arg("label", LabelEnable)),
	}
	containers, err := cli.ContainerList(ctx, options)
	if err != nil {
		return err
	}

	u.containers = containers

	go u.keepUpdated(ctx, cli)

	return nil
}

var matchers = map[string]func(string) caddyhttp.RequestMatcher{
	// TODO: more matchers
	LabelMatchHost: func(value string) caddyhttp.RequestMatcher {
		return caddyhttp.MatchHost([]string{value})
	},
	LabelMatchPath: func(value string) caddyhttp.RequestMatcher {
		return caddyhttp.MatchPath([]string{value})
	},
}

func match(r *http.Request, container types.Container) bool {
	if enable, ok := container.Labels[LabelEnable]; !ok || enable != "true" {
		return false
	}

	for key, matcher := range matchers {
		value, ok := container.Labels[key]
		if !ok {
			continue
		}

		m := matcher(value)
		if !m.Match(r) {
			return false
		}
	}

	return true
}

var (
	addresses   = make(map[string]*reverseproxy.Upstream)
	addressesMu sync.RWMutex
)

func toUpstream(container types.Container) (*reverseproxy.Upstream, error) {
	addressesMu.RLock()
	cached, ok := addresses[container.ID]
	addressesMu.RUnlock()
	if ok {
		return cached, nil
	}

	port, ok := container.Labels[LabelUpstreamPort]
	if !ok {
		return nil, errors.New("unable to get port from container labels")
	}

	// Use the first networks of container.
	for _, network := range container.NetworkSettings.Networks {
		address := net.JoinHostPort(network.IPAddress, port)
		upstream := &reverseproxy.Upstream{Dial: address}

		addressesMu.Lock()
		addresses[container.ID] = upstream
		addressesMu.Unlock()

		return upstream, nil
	}

	return nil, errors.New("unable to get ip address from container networks")
}

func (u *Upstreams) GetUpstreams(r *http.Request) ([]*reverseproxy.Upstream, error) {
	upstreams := make([]*reverseproxy.Upstream, 0, 1)

	u.mu.RLock()
	defer u.mu.RUnlock()

	for _, container := range u.containers {
		ok := match(r, container)
		if !ok {
			continue
		}

		upstream, err := toUpstream(container)
		if err != nil {
			u.logger.Warn("unable to get upstream from container", zap.Error(err))
			continue
		}
		upstreams = append(upstreams, upstream)
	}

	return upstreams, nil
}

// Interface guards
var (
	_ caddy.Provisioner           = (*Upstreams)(nil)
	_ reverseproxy.UpstreamSource = (*Upstreams)(nil)
)
