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

//go:build windows

package server

import (
	"errors"
	"net"
	"os"
	"syscall"
	"testing"

	"golang.org/x/sys/windows"
)

func TestNativeUnixAddrWindows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		in          string
		expected    string
	}{
		// positive
		{description: "uppercase drive", in: "/C:/ProgramData/nats/a.sock", expected: `C:\ProgramData\nats\a.sock`},
		{description: "lowercase drive", in: "/c:/x/a.sock", expected: `c:\x\a.sock`},
		{description: "non-C drive", in: "/D:/a.sock", expected: `D:\a.sock`},
		{description: "abstract passes through", in: "@nats-a", expected: "@nats-a"},
		{description: "abstract with slash untouched", in: "@nats/a", expected: "@nats/a"},
		// negative-ish: not a drive letter
		{description: "letter without colon is not a drive", in: "/abc/a.sock", expected: `\abc\a.sock`},
		{description: "digit before colon is not a drive", in: "/1:/a.sock", expected: `\1:\a.sock`},
		// boundary
		{description: "shortest drive path", in: "/C:/a", expected: `C:\a`},
		{description: "drive with nothing after colon", in: "/C:", expected: `C:`},
		{description: "empty string", in: "", expected: ""},
		// corner
		{description: "drive-less absolute keeps leading separator", in: "/run/a.sock", expected: `\run\a.sock`},
		{description: "UNC-looking path", in: "//server/share/a.sock", expected: `\\server\share\a.sock`},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := nativeUnixAddr(tc.in); got != tc.expected {
				t.Fatalf("nativeUnixAddr(%q) = %q, expected %q", tc.in, got, tc.expected)
			}
		})
	}
}

func TestURLUnixAddrWindows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		in          string
		expected    string
	}{
		// positive
		{description: "drive path gains leading slash and forward slashes", in: `C:\ProgramData\nats\a.sock`, expected: "/C:/ProgramData/nats/a.sock"},
		{description: "lowercase drive", in: `c:\x\a.sock`, expected: "/c:/x/a.sock"},
		{description: "abstract passes through", in: "@nats-a", expected: "@nats-a"},
		// negative-ish: no drive letter
		{description: "drive-less path only converts separators", in: `\run\a.sock`, expected: "/run/a.sock"},
		{description: "digit before colon is not a drive", in: `1:\a.sock`, expected: "1:/a.sock"},
		// boundary
		{description: "bare drive", in: "C:", expected: "/C:"},
		{description: "empty string", in: "", expected: ""},
		// corner: round trip with nativeUnixAddr is the identity.
		{description: "round trip drive path", in: nativeUnixAddr("/C:/nats/a.sock"), expected: "/C:/nats/a.sock"},
		{description: "round trip drive-less path", in: nativeUnixAddr("/run/a.sock"), expected: "/run/a.sock"},
		{description: "UNC path", in: `\\server\share\a.sock`, expected: "//server/share/a.sock"},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := urlUnixAddr(tc.in); got != tc.expected {
				t.Fatalf("urlUnixAddr(%q) = %q, expected %q", tc.in, got, tc.expected)
			}
		})
	}
}

func TestIsConnRefusedWindows(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		description string
		err         error
		expected    bool
	}{
		// positive
		{description: "bare WSAECONNREFUSED", err: windows.WSAECONNREFUSED, expected: true},
		{description: "wrapped like net.Dial returns", err: &net.OpError{Op: "dial", Net: "unix", Err: os.NewSyscallError("connectex", windows.WSAECONNREFUSED)}, expected: true},
		// negative
		{description: "nil", err: nil, expected: false},
		{description: "invented posix ECONNREFUSED is not what winsock returns", err: syscall.ECONNREFUSED, expected: false},
		{description: "ENOENT", err: windows.ERROR_FILE_NOT_FOUND, expected: false},
		{description: "plain error", err: errors.New("connection refused"), expected: false},
	} {
		t.Run(tc.description, func(t *testing.T) {
			if got := isConnRefused(tc.err); got != tc.expected {
				t.Fatalf("isConnRefused(%v) = %v, expected %v", tc.err, got, tc.expected)
			}
		})
	}
}
