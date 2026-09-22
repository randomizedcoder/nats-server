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

//go:build !windows

package server

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
)

func TestNativeUnixAddr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		in          string
		expected    string
	}{
		// positive: on POSIX the URL form is the filesystem path.
		{description: "pathname unchanged", in: "/run/nats/a.sock", expected: "/run/nats/a.sock"},
		{description: "abstract unchanged", in: "@nats-a", expected: "@nats-a"},
		{description: "abstract with slash unchanged", in: "@nats/a", expected: "@nats/a"},
		// corner: a windows-looking drive path is just a path here.
		{description: "drive-letter form is a plain path", in: "/C:/nats/a.sock", expected: "/C:/nats/a.sock"},
		{description: "backslashes are ordinary characters", in: `/run/a\b.sock`, expected: `/run/a\b.sock`},
		// boundary
		{description: "empty string", in: "", expected: ""},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := nativeUnixAddr(tc.in); got != tc.expected {
				t.Fatalf("nativeUnixAddr(%q) = %q, expected %q", tc.in, got, tc.expected)
			}
		})
	}
}

func TestURLUnixAddr(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		in          string
		expected    string
	}{
		// positive: on POSIX the native form is the URL form.
		{description: "pathname unchanged", in: "/run/nats/a.sock", expected: "/run/nats/a.sock"},
		{description: "abstract unchanged", in: "@nats-a", expected: "@nats-a"},
		// boundary
		{description: "empty string", in: "", expected: ""},
		// corner: round trip with nativeUnixAddr is the identity.
		{description: "round trip", in: nativeUnixAddr("/run/nats/a.sock"), expected: "/run/nats/a.sock"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := urlUnixAddr(tc.in); got != tc.expected {
				t.Fatalf("urlUnixAddr(%q) = %q, expected %q", tc.in, got, tc.expected)
			}
		})
	}
}

func TestIsConnRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		err         error
		expected    bool
	}{
		// positive
		{description: "bare ECONNREFUSED", err: syscall.ECONNREFUSED, expected: true},
		{description: "wrapped in OpError and SyscallError like net.Dial returns", err: &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}, expected: true},
		{description: "wrapped with fmt %w", err: errors.Join(errors.New("dial"), syscall.ECONNREFUSED), expected: true},
		// negative
		{description: "nil", err: nil, expected: false},
		{description: "ENOENT", err: syscall.ENOENT, expected: false},
		{description: "EADDRINUSE", err: syscall.EADDRINUSE, expected: false},
		{description: "ENOTSOCK", err: syscall.ENOTSOCK, expected: false},
		{description: "plain error", err: errors.New("connection refused"), expected: false},
		// corner
		{description: "timeout error", err: &net.OpError{Op: "dial", Net: "unix", Err: os.ErrDeadlineExceeded}, expected: false},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := isConnRefused(tc.err); got != tc.expected {
				t.Fatalf("isConnRefused(%v) = %v, expected %v", tc.err, got, tc.expected)
			}
		})
	}
}
