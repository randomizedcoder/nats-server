// Copyright 2026 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// Cluster routes over Unix domain sockets (UDS).
//
// A UDS address is written as a URL, following the convention used by Docker,
// containerd, gRPC and systemd:
//
//   - "unix:///absolute/path.sock" is a pathname socket. Note the three
//     slashes. On Windows the path is written in file:// style with forward
//     slashes and a leading "/" before the drive letter, e.g.
//     "unix:///C:/nats/route.sock"; nativeUnixAddr converts it before the
//     syscall.
//   - "unix://@name" is a Linux abstract socket. The "@" is translated to a
//     leading NUL byte by the Go net package. The parser accepts the form on
//     every platform so that a configuration file can be validated anywhere;
//     platforms without abstract sockets fail at net.Listen/net.Dial time.
//
// Inside the server a UDS address is carried as the string after "unix://"
// (the "URL form"), e.g. "/run/nats/a.sock", "@nats-a" or "/C:/nats/a.sock".
// Route URLs (Options.Routes and INFO.IP) use the canonical *url.URL built by
// unixRouteURL so that the existing reflect.DeepEqual based URL comparison
// keeps working.

// unixSchemePrefix is the scheme prefix, compared case-insensitively.
const unixSchemePrefix = "unix://"

// udsStats holds counters for route connections over Unix domain sockets.
// They are surfaced through /varz (see RouteUnixSocketStats).
type udsStats struct {
	accepted     atomic.Uint64 // route connections accepted on the UDS listener
	dialed       atomic.Uint64 // successful outbound UDS route dials
	dialErrors   atomic.Uint64 // failed outbound UDS route dials, per attempt
	staleRemoved atomic.Uint64 // stale socket files removed at listener start
}

// hasUnixScheme reports whether s starts with "unix://", ignoring case.
func hasUnixScheme(s string) bool {
	return len(s) >= len(unixSchemePrefix) &&
		strings.EqualFold(s[:len(unixSchemePrefix)], unixSchemePrefix)
}

// parseUnixAddr validates a "unix://" address from the configuration or the
// command line and returns the URL-form address after the scheme.
//
// The value is set by an operator, so the checks aim to catch typos early
// with a clear message rather than to be lenient. The text after "unix://" is
// used verbatim as the socket address, which is why a "?" query or "#"
// fragment is rejected outright, and why "%" is rejected: the canonical
// url.URL form would re-encode it on the wire and a gossiped address would no
// longer match the configured one.
func parseUnixAddr(raw string) (string, error) {
	// Extra spaces are usually a copy-paste slip.
	if strings.TrimSpace(raw) != raw {
		return _EMPTY_, fmt.Errorf("unix socket address %q must not have leading or trailing spaces", raw)
	}
	if !hasUnixScheme(raw) {
		return _EMPTY_, fmt.Errorf("unix socket address %q must start with %q", raw, unixSchemePrefix)
	}
	// url.Parse is used only as a gate: it rejects control characters such as
	// NUL or newline and malformed escapes. The address itself is taken from
	// the raw string because url.Parse drops a leading "@" into User/Host.
	if _, err := url.Parse(raw); err != nil {
		return _EMPTY_, fmt.Errorf("unable to parse unix socket address %q: %w", raw, err)
	}
	if strings.Contains(raw, "?") {
		return _EMPTY_, fmt.Errorf("unix socket address %q must not contain a \"?\" query", raw)
	}
	if strings.Contains(raw, "#") {
		return _EMPTY_, fmt.Errorf("unix socket address %q must not contain a \"#\" fragment", raw)
	}
	if strings.Contains(raw, "%") {
		return _EMPTY_, fmt.Errorf("unix socket address %q must not contain \"%%\"", raw)
	}
	addr := raw[len(unixSchemePrefix):]
	if addr == _EMPTY_ {
		return _EMPTY_, fmt.Errorf("unix socket address %q has no socket path after %q", raw, unixSchemePrefix)
	}
	// The address must fit sun_path. Measure the native form net.Dial
	// receives: on Windows a drive-letter URL path carries an extra leading
	// "/" that nativeUnixAddr strips before dialing. A pathname must leave
	// one byte for the terminating NUL, which is what Go's SockaddrUnix
	// enforces on every OS. For an abstract socket the "@" occupies the
	// byte the kernel uses for the leading NUL, so the name may fill
	// sun_path completely.
	limit := maxUnixSocketPathLen
	if !strings.HasPrefix(addr, "@") {
		limit--
	}
	if n := len(nativeUnixAddr(addr)); n > limit {
		return _EMPTY_, fmt.Errorf("unix socket address %q is too long: %d bytes, max is %d",
			raw, n, limit)
	}
	if strings.HasPrefix(addr, "@") {
		if addr == "@" {
			return _EMPTY_, fmt.Errorf("unix socket address %q has \"@\" but no abstract socket name", raw)
		}
		return addr, nil
	}
	if !strings.HasPrefix(addr, "/") {
		return _EMPTY_, fmt.Errorf("unix socket address %q must be an absolute path like \"unix:///run/nats/route.sock\" (note the three slashes)", raw)
	}
	if strings.HasSuffix(addr, "/") {
		return _EMPTY_, fmt.Errorf("unix socket address %q ends with \"/\"; it must point to a socket file, not a directory", raw)
	}
	return addr, nil
}

// unixRouteURL returns the canonical route URL for a URL-form UDS address
// produced by parseUnixAddr. The address is stored in Path even for abstract
// sockets so that two URLs for the same socket are reflect.DeepEqual.
// String() yields "unix:///run/a.sock" or "unix://@name".
func unixRouteURL(addr string) *url.URL {
	return &url.URL{Scheme: "unix", Path: addr}
}

// unixRouteURLFromString parses a unix route URL that may have arrived over
// route INFO. It accepts the strict configuration form and the canonical
// url.URL.String form emitted by unixRouteURL, where non-ASCII path bytes are
// percent-encoded on the wire.
func unixRouteURLFromString(raw string) (*url.URL, error) {
	if addr, err := parseUnixAddr(raw); err == nil {
		return unixRouteURL(addr), nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("unable to parse unix socket address %q: %w", raw, err)
	}
	addr, ok := unixAddrFromRouteURL(u)
	if !ok {
		return nil, fmt.Errorf("invalid unix route URL %q", raw)
	}
	if _, err := parseUnixAddr(unixSchemePrefix + addr); err != nil {
		return nil, err
	}
	return unixRouteURL(addr), nil
}

// unixAddrFromRouteURL returns the URL-form UDS address of a route URL, or
// ok=false when u is not a UDS route URL.
//
// It accepts the canonical form from unixRouteURL as well as the result of
// url.Parse on the canonical String(), which differs for abstract sockets:
// url.Parse("unix://@name") yields a non-nil, empty User and Host "name".
// A User with a non-empty username or a password is not that artifact but
// route credentials injected by processImplicitRoute, and the address is then
// in Path as usual.
func unixAddrFromRouteURL(u *url.URL) (string, bool) {
	if u == nil || !strings.EqualFold(u.Scheme, "unix") {
		return _EMPTY_, false
	}
	if u.User != nil && u.User.Username() == _EMPTY_ {
		if _, hasPassword := u.User.Password(); !hasPassword {
			// "unix://@name" or "unix://@name/rest" parsed by url.Parse.
			if u.Host == _EMPTY_ {
				return _EMPTY_, false
			}
			return "@" + u.Host + u.Path, true
		}
	}
	// A host component without the "@" artifact is not a form we produce.
	if u.Host != _EMPTY_ || u.Path == _EMPTY_ {
		return _EMPTY_, false
	}
	return u.Path, true
}

// isUnixRouteURL reports whether u is a UDS route URL.
func isUnixRouteURL(u *url.URL) bool {
	_, ok := unixAddrFromRouteURL(u)
	return ok
}

// errRouteTLSUnixNoName is returned when a TLS route is solicited over a
// unix socket and there is no name to verify the peer certificate against.
var errRouteTLSUnixNoName = errors.New("TLS route over unix socket requires cluster.tls.insecure or a configured TCP route hostname")

// unixStaleProbeTimeout bounds the connect used to tell a stale socket file
// (nothing listening, ECONNREFUSED) from one that is in use.
const unixStaleProbeTimeout = 250 * time.Millisecond

// Route transport names reported by /varz, /routez and STATSZ.
const (
	routeTransportTCP  = "tcp"
	routeTransportUnix = "unix"
)

// routeTransport names the transport of a route connection from its
// remote address: "unix" for unix domain sockets, "tcp" otherwise.
func routeTransport(nc net.Conn) string {
	if nc != nil {
		if _, ok := nc.RemoteAddr().(*net.UnixAddr); ok {
			return routeTransportUnix
		}
	}
	return routeTransportTCP
}

// updateUnixRoutesToSelf records the unix socket addresses this server should
// never dial as routes to itself. Server lock is held on entry.
func (s *Server) updateUnixRoutesToSelf(c *ClusterOpts) {
	if s.unixRoutesToSelf == nil {
		s.unixRoutesToSelf = make(map[string]struct{})
	}
	clear(s.unixRoutesToSelf)
	if c.UnixSocket == _EMPTY_ {
		return
	}
	s.unixRoutesToSelf[c.UnixSocket] = struct{}{}
	if hasUnixScheme(c.Advertise) {
		if addr, err := parseUnixAddr(c.Advertise); err == nil {
			s.unixRoutesToSelf[addr] = struct{}{}
		}
	}
}

// listenRouteUnix binds the route listener to the unix domain socket at the
// URL-form address addr produced by parseUnixAddr. For a pathname socket a
// stale file left behind by a crashed server is removed first; anything that
// is not a socket, or a socket another process is serving, is an error and
// is never removed. The listener unlinks its own socket file on Close.
func (s *Server) listenRouteUnix(addr string) (net.Listener, error) {
	native := nativeUnixAddr(addr)
	if !strings.HasPrefix(addr, "@") {
		if err := s.removeStaleUnixSocket(native); err != nil {
			return nil, err
		}
	}
	return natsListen("unix", native)
}

// removeStaleUnixSocket removes the socket file at path when it exists and
// nothing is accepting connections on it. It returns nil when there is
// nothing at path, and an error, without touching the file, in every other
// case that is not a stale socket.
func (s *Server) removeStaleUnixSocket(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("unable to stat %q: %w", path, err)
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%q exists and is not a socket", path)
	}
	conn, err := net.DialTimeout("unix", path, unixStaleProbeTimeout)
	if err == nil {
		conn.Close()
		return fmt.Errorf("socket %q is in use by another process", path)
	}
	if !isConnRefused(err) {
		return fmt.Errorf("unable to probe socket %q: %w", path, err)
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("unable to remove stale socket %q: %w", path, err)
	}
	s.Warnf("Removed stale unix socket %q", path)
	s.udsStats.staleRemoved.Add(1)
	return nil
}
