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
	"net/url"
	"runtime"
	"strings"
	"testing"
)

func TestHasUnixScheme(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		in          string
		expected    bool
	}{
		// positive
		{description: "lowercase scheme", in: "unix:///run/a.sock", expected: true},
		{description: "uppercase scheme", in: "UNIX:///run/a.sock", expected: true},
		{description: "mixed-case scheme", in: "Unix://@a", expected: true},
		{description: "scheme with nothing after it", in: "unix://", expected: true},
		// negative
		{description: "empty string", in: "", expected: false},
		{description: "bare path", in: "/run/a.sock", expected: false},
		{description: "tcp scheme", in: "tcp://127.0.0.1:6222", expected: false},
		{description: "nats-route scheme", in: "nats-route://127.0.0.1:6222", expected: false},
		{description: "one slash", in: "unix:/run/a.sock", expected: false},
		{description: "scheme only, no slashes", in: "unix:", expected: false},
		// boundary
		{description: "prefix minus one byte", in: "unix:/", expected: false},
		{description: "exactly the prefix", in: "unix://", expected: true},
		// corner
		{description: "leading space defeats the prefix", in: " unix:///a", expected: false},
		{description: "scheme embedded later in string", in: "nats://unix://a", expected: false},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := hasUnixScheme(tc.in); got != tc.expected {
				t.Fatalf("hasUnixScheme(%q) = %v, expected %v", tc.in, got, tc.expected)
			}
		})
	}
}

func TestParseUnixAddr(t *testing.T) {
	t.Parallel()
	// A pathname leaves one byte of sun_path for the NUL terminator; an
	// abstract name may use all of it because "@" stands in for the leading
	// NUL. Both limits are what Go's SockaddrUnix enforces.
	maxPath := "/" + strings.Repeat("a", maxUnixSocketPathLen-2)
	maxAbstract := "@" + strings.Repeat("a", maxUnixSocketPathLen-1)

	type row struct {
		description string
		in          string
		expected    string // expected address when expectedErr is empty
		expectedErr string // substring of the expected error, empty for success
	}
	rows := []row{
		// positive
		{description: "pathname socket", in: "unix:///run/nats/a.sock", expected: "/run/nats/a.sock"},
		{description: "shortest absolute path", in: "unix:///a", expected: "/a"},
		{description: "uppercase scheme accepted", in: "UNIX:///run/a.sock", expected: "/run/a.sock"},
		{description: "mixed-case scheme accepted", in: "Unix:///run/a.sock", expected: "/run/a.sock"},
		{description: "abstract socket", in: "unix://@nats-a", expected: "@nats-a"},
		{description: "abstract name containing slash", in: "unix://@nats/a", expected: "@nats/a"},
		{description: "windows drive path stored verbatim", in: "unix:///C:/nats/a.sock", expected: "/C:/nats/a.sock"},
		{description: "windows lowercase drive", in: "unix:///c:/nats/a.sock", expected: "/c:/nats/a.sock"},
		{description: "dots and dashes in path", in: "unix:///run/nats.io/a-b_c.sock", expected: "/run/nats.io/a-b_c.sock"},
		{description: "no file extension required", in: "unix:///run/nats/route", expected: "/run/nats/route"},
		// negative
		{description: "empty string", in: "", expectedErr: `must start with "unix://"`},
		{description: "bare path without scheme", in: "/run/a.sock", expectedErr: `must start with "unix://"`},
		{description: "tcp scheme", in: "tcp://127.0.0.1:6222", expectedErr: `must start with "unix://"`},
		{description: "nats-route scheme", in: "nats-route://127.0.0.1:6222", expectedErr: `must start with "unix://"`},
		{description: "scheme with one slash", in: "unix:/run/a.sock", expectedErr: `must start with "unix://"`},
		{description: "scheme only", in: "unix://", expectedErr: "no socket path"},
		{description: "relative path", in: "unix://run/a.sock", expectedErr: "must be an absolute path"},
		{description: "trailing slash", in: "unix:///run/a.sock/", expectedErr: "not a directory"},
		{description: "root directory", in: "unix:///", expectedErr: "not a directory"},
		{description: "query string", in: "unix:///run/a.sock?x=1", expectedErr: "query"},
		{description: "bare query delimiter", in: "unix:///run/a.sock?", expectedErr: "query"},
		{description: "fragment", in: "unix:///run/a.sock#f", expectedErr: "fragment"},
		{description: "bare fragment delimiter", in: "unix:///run/a.sock#", expectedErr: "fragment"},
		{description: "percent sign rejected", in: "unix:///run/a%20b.sock", expectedErr: `must not contain "%"`},
		{description: "empty abstract name", in: "unix://@", expectedErr: "no abstract socket name"},
		{description: "userinfo not supported", in: "unix://ruser:pw@/run/a.sock", expectedErr: "must be an absolute path"},
		{description: "leading space", in: " unix:///run/a.sock", expectedErr: "leading or trailing spaces"},
		{description: "trailing space", in: "unix:///run/a.sock ", expectedErr: "leading or trailing spaces"},
		{description: "embedded newline", in: "unix:///run/a.sock\nx", expectedErr: "unable to parse"},
		{description: "malformed percent escape", in: "unix:///%ZZ", expectedErr: "unable to parse"},
		{description: "NUL byte in path", in: "unix:///run/a\x00.sock", expectedErr: "unable to parse"},
		// boundary
		{description: "path exactly at limit (sun_path minus the NUL)", in: "unix://" + maxPath, expected: maxPath},
		{description: "path one byte over limit", in: "unix://" + maxPath + "a", expectedErr: "too long"},
		{description: "abstract name at limit, @ counts as the NUL byte", in: "unix://" + maxAbstract, expected: maxAbstract},
		{description: "abstract name one byte over limit", in: "unix://" + maxAbstract + "a", expectedErr: "too long"},
		// corner
		{description: "double slash inside path preserved", in: "unix:///run//a.sock", expected: "/run//a.sock"},
		{description: "dot segments preserved, not cleaned", in: "unix:///run/../a.sock", expected: "/run/../a.sock"},
		{description: "unicode path", in: "unix:///run/ñ.sock", expected: "/run/ñ.sock"},
		{description: "multibyte path measured in bytes not runes", in: "unix:///" + strings.Repeat("ñ", maxUnixSocketPathLen/2), expectedErr: "too long"},
		{description: "four slashes is still absolute", in: "unix:////run/a.sock", expected: "//run/a.sock"},
		{description: "tab inside path parses but is kept verbatim", in: "unix:///run/a\tb.sock", expectedErr: "unable to parse"},
	}
	// The kernel limit applies to the native form. On Windows a drive-letter
	// URL path is one byte longer than the native path ("/C:/..." -> "C:\..."),
	// so a URL form one byte over the pathname limit is still accepted there.
	winDrive := "/C:/" + strings.Repeat("a", maxUnixSocketPathLen-4) // len == pathname limit + 1
	if runtime.GOOS == "windows" {
		rows = append(rows, row{description: "windows drive form one over URL limit fits natively", in: "unix://" + winDrive, expected: winDrive})
		rows = append(rows, row{description: "windows drive form two over URL limit is too long", in: "unix://" + winDrive + "a", expectedErr: "too long"})
	} else {
		rows = append(rows, row{description: "drive form is a plain path on posix and is too long", in: "unix://" + winDrive, expectedErr: "too long"})
	}

	for _, tc := range rows {
		t.Run(tc.description, func(t *testing.T) {
			got, err := parseUnixAddr(tc.in)
			if tc.expectedErr != _EMPTY_ {
				if err == nil {
					t.Fatalf("parseUnixAddr(%q) = %q, expected error containing %q", tc.in, got, tc.expectedErr)
				}
				if !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("parseUnixAddr(%q) error %q, expected it to contain %q", tc.in, err.Error(), tc.expectedErr)
				}
				if got != _EMPTY_ {
					t.Fatalf("parseUnixAddr(%q) returned %q alongside an error, expected empty", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseUnixAddr(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.expected {
				t.Fatalf("parseUnixAddr(%q) = %q, expected %q", tc.in, got, tc.expected)
			}
		})
	}
}

func TestUnixRouteURLRoundTrip(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description    string
		in             string // configuration form
		expectedAddr   string // address recovered from every form
		expectedString string // canonical String()
	}{
		// positive
		{description: "pathname", in: "unix:///run/a.sock", expectedAddr: "/run/a.sock", expectedString: "unix:///run/a.sock"},
		{description: "abstract", in: "unix://@nats-a", expectedAddr: "@nats-a", expectedString: "unix://@nats-a"},
		{description: "abstract with slash", in: "unix://@nats/a", expectedAddr: "@nats/a", expectedString: "unix://@nats/a"},
		{description: "windows drive", in: "unix:///C:/nats/a.sock", expectedAddr: "/C:/nats/a.sock", expectedString: "unix:///C:/nats/a.sock"},
		// boundary
		{description: "shortest pathname", in: "unix:///a", expectedAddr: "/a", expectedString: "unix:///a"},
		{description: "shortest abstract", in: "unix://@a", expectedAddr: "@a", expectedString: "unix://@a"},
		// corner
		{description: "unicode pathname survives String and re-parse", in: "unix:///run/ñ.sock", expectedAddr: "/run/ñ.sock", expectedString: "unix:///run/%C3%B1.sock"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			addr, err := parseUnixAddr(tc.in)
			if err != nil {
				t.Fatalf("parseUnixAddr(%q): %v", tc.in, err)
			}
			if addr != tc.expectedAddr {
				t.Fatalf("parseUnixAddr(%q) = %q, expected %q", tc.in, addr, tc.expectedAddr)
			}
			canonical := unixRouteURL(addr)
			if s := canonical.String(); s != tc.expectedString {
				t.Fatalf("canonical String() = %q, expected %q", s, tc.expectedString)
			}
			if !isUnixRouteURL(canonical) {
				t.Fatalf("isUnixRouteURL(canonical) = false, expected true")
			}
			// (a) from the canonical form
			if got, ok := unixAddrFromRouteURL(canonical); !ok || got != tc.expectedAddr {
				t.Fatalf("unixAddrFromRouteURL(canonical) = %q, %v; expected %q, true", got, ok, tc.expectedAddr)
			}
			// (b) from url.Parse of the configuration string
			parsed, err := url.Parse(tc.in)
			if err != nil {
				t.Fatalf("url.Parse(%q): %v", tc.in, err)
			}
			if got, ok := unixAddrFromRouteURL(parsed); !ok || got != tc.expectedAddr {
				t.Fatalf("unixAddrFromRouteURL(url.Parse(in)) = %q, %v; expected %q, true", got, ok, tc.expectedAddr)
			}
			// (c) from url.Parse of the canonical String(), which is what a
			// peer sees in INFO.IP.
			reparsed, err := url.Parse(canonical.String())
			if err != nil {
				t.Fatalf("url.Parse(canonical.String()): %v", err)
			}
			if got, ok := unixAddrFromRouteURL(reparsed); !ok || got != tc.expectedAddr {
				t.Fatalf("unixAddrFromRouteURL(url.Parse(canonical.String())) = %q, %v; expected %q, true", got, ok, tc.expectedAddr)
			}
			// (d) with route credentials injected the way processImplicitRoute does.
			withCreds := *canonical
			withCreds.User = url.UserPassword("ruser", "top_secret")
			if got, ok := unixAddrFromRouteURL(&withCreds); !ok || got != tc.expectedAddr {
				t.Fatalf("unixAddrFromRouteURL(with credentials) = %q, %v; expected %q, true", got, ok, tc.expectedAddr)
			}
		})
	}
}

func TestUnixAddrFromRouteURLRejects(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		in          *url.URL
		expectedOK  bool
	}{
		// negative
		{description: "nil URL", in: nil, expectedOK: false},
		{description: "nats-route URL", in: mustParseURL(t, "nats-route://127.0.0.1:6222"), expectedOK: false},
		{description: "nats URL", in: mustParseURL(t, "nats://127.0.0.1:6222"), expectedOK: false},
		{description: "unix scheme with a host and no @ artifact", in: mustParseURL(t, "unix://host/run/a.sock"), expectedOK: false},
		{description: "unix scheme with empty path and no host", in: &url.URL{Scheme: "unix"}, expectedOK: false},
		{description: "unix scheme with only the @ artifact and no host", in: &url.URL{Scheme: "unix", User: url.User(_EMPTY_)}, expectedOK: false},
		// corner
		{description: "empty username with password is not the @ artifact", in: &url.URL{Scheme: "unix", User: url.UserPassword(_EMPTY_, "pw"), Path: "/run/a.sock"}, expectedOK: true},
		{description: "scheme compared case-insensitively", in: &url.URL{Scheme: "UNIX", Path: "/run/a.sock"}, expectedOK: true},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if _, ok := unixAddrFromRouteURL(tc.in); ok != tc.expectedOK {
				t.Fatalf("unixAddrFromRouteURL(%v) ok = %v, expected %v", tc.in, ok, tc.expectedOK)
			}
			if got := isUnixRouteURL(tc.in); got != tc.expectedOK {
				t.Fatalf("isUnixRouteURL(%v) = %v, expected %v", tc.in, got, tc.expectedOK)
			}
		})
	}
}

func TestUnixRouteURLFromString(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		in          string
		expected    string
		expectedErr string
	}{
		// positive
		{description: "config form", in: "unix:///run/a.sock", expected: "/run/a.sock"},
		{description: "canonical unicode form", in: unixRouteURL("/run/ñ.sock").String(), expected: "/run/ñ.sock"},
		{description: "abstract form", in: "unix://@a", expected: "@a"},
		// negative
		{description: "not unix", in: "nats-route://127.0.0.1:6222", expectedErr: "invalid unix route URL"},
		{description: "relative unix path", in: "unix://relative", expectedErr: "invalid unix route URL"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			u, err := unixRouteURLFromString(tc.in)
			if tc.expectedErr != _EMPTY_ {
				if err == nil || !strings.Contains(err.Error(), tc.expectedErr) {
					t.Fatalf("expected error containing %q, got %v", tc.expectedErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got, ok := unixAddrFromRouteURL(u); !ok || got != tc.expected {
				t.Fatalf("unixRouteURLFromString(%q) recovered %q, %v; expected %q, true", tc.in, got, ok, tc.expected)
			}
		})
	}
}

func mustParseURL(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatalf("url.Parse(%q): %v", s, err)
	}
	return u
}

func TestMaxUnixSocketPathLen(t *testing.T) {
	t.Parallel()
	expected := 108
	switch runtime.GOOS {
	case "darwin", "freebsd", "openbsd", "netbsd", "dragonfly":
		expected = 104
	}
	if maxUnixSocketPathLen != expected {
		t.Fatalf("maxUnixSocketPathLen = %d on %s, expected %d", maxUnixSocketPathLen, runtime.GOOS, expected)
	}
}
