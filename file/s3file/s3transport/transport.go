package s3transport

import (
	"crypto/tls"
	"fmt"
	"math/rand"
	"net"
	"net/http"
	"sync"
	"time"
)

// T is an http.RoundTripper specialized for S3. See https://github.com/aws/aws-sdk-go/issues/3739.
type T struct {
	factory func() *http.Transport

	hostRTsMu sync.Mutex
	hostRTs   map[string]http.RoundTripper

	hostIPs *expiringMap
}

var (
	stdDefaultTransport = http.DefaultTransport.(*http.Transport)
	httpTransport       = &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second, // Copied from http.DefaultTransport.
			KeepAlive: 30 * time.Second, // Copied from same.
		}).DialContext,
		ForceAttemptHTTP2:     false,                           // S3 doesn't support HTTP2.
		MaxIdleConns:          200,                             // Keep many peers for future bursts.
		MaxIdleConnsPerHost:   4,                               // But limit connections to each.
		IdleConnTimeout:       expireAfter + 2*expireLoopEvery, // Keep until we forget the peer.
		TLSClientConfig:       &tls.Config{},
		TLSHandshakeTimeout:   stdDefaultTransport.TLSHandshakeTimeout,
		ExpectContinueTimeout: stdDefaultTransport.ExpectContinueTimeout,
	}

	// Default is an http.RoundTripper with recommended settings.
	Default = New(httpTransport.Clone)
	// DefaultClient uses Default (suitable for general use, analogous to "net/http".DefaultClient).
	DefaultClient = &http.Client{Transport: Default}
)

// New constructs *T using factory to create internal transports. Each call to factory()
// must return a separate http.Transport and they must not share TLSClientConfig.
func New(factory func() *http.Transport) *T {
	return &T{
		factory: factory,
		hostRTs: map[string]http.RoundTripper{},
		hostIPs: newExpiringMap(runPeriodicForever(), time.Now),
	}
}

func (t *T) RoundTrip(req *http.Request) (*http.Response, error) {
	host := req.URL.Hostname()

	ips, err := defaultResolver.LookupIP(host)
	if err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("s3transport: lookup ip: %w", err)
	}
	ips = t.hostIPs.AddAndGet(host, ips)

	hostReq := req.Clone(req.Context())
	hostReq.Host = host
	// TODO: Consider other load balancing strategies.
	hostReq.URL.Host = ips[rand.Intn(len(ips))].String()

	return t.hostRoundTripper(host).RoundTrip(hostReq)
}

func (t *T) hostRoundTripper(host string) http.RoundTripper {
	t.hostRTsMu.Lock()
	defer t.hostRTsMu.Unlock()
	if rt, ok := t.hostRTs[host]; ok {
		return rt
	}
	transport := t.factory()
	// We modify request URL to contain an IP, but server certificates list hostnames, so we
	// configure our client to check against original hostname.
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.ServerName = host
	t.hostRTs[host] = transport
	return transport
}
