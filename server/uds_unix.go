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
	"syscall"
)

// nativeUnixAddr returns the kernel-level address for net.Listen/net.Dial
// ("unix", ...) from a URL-form UDS address. On POSIX platforms the URL form
// and the filesystem path are the same string.
func nativeUnixAddr(addr string) string {
	return addr
}

// isConnRefused reports whether err is a connection-refused error from a
// dial. It is used to tell a stale socket file apart from a live listener.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
