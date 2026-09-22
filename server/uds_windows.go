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
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// nativeUnixAddr returns the kernel-level address for net.Listen/net.Dial
// ("unix", ...) from a URL-form UDS address. The configuration value is a URL
// (e.g. "unix:///C:/nats/route.sock"), so paths arrive with forward slashes
// and a leading "/" before the drive letter, matching the file:// URI
// convention. The Go net package on Windows expects native paths (e.g.
// `C:\nats\route.sock`), so for a path whose first character is "/" followed
// by a drive letter and ":" the leading "/" is stripped and separators are
// converted. Drive-less paths keep their leading separator. Abstract
// addresses ("@name") are not filesystem paths and pass through unchanged.
func nativeUnixAddr(addr string) string {
	if strings.HasPrefix(addr, "@") {
		return addr
	}
	if len(addr) >= 3 && addr[0] == '/' && isDriveLetter(addr[1]) && addr[2] == ':' {
		addr = addr[1:]
	}
	return filepath.FromSlash(addr)
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// urlUnixAddr is the inverse of nativeUnixAddr: it returns the URL-form
// address for a kernel-level address, such as the Name of a *net.UnixAddr.
// Separators become "/" and a drive-letter path gains the leading "/" of
// the file:// URI convention, so `C:\nats\a.sock` becomes "/C:/nats/a.sock".
// Abstract addresses pass through unchanged.
func urlUnixAddr(native string) string {
	if strings.HasPrefix(native, "@") {
		return native
	}
	addr := filepath.ToSlash(native)
	if len(addr) >= 2 && isDriveLetter(addr[0]) && addr[1] == ':' {
		addr = "/" + addr
	}
	return addr
}

// isConnRefused reports whether err is a connection-refused error from a
// dial. Winsock reports WSAECONNREFUSED; syscall.ECONNREFUSED on Windows is
// an invented errno that the network stack never returns.
func isConnRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED)
}
