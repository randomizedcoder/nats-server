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

//go:build wasm

package server

// maxUnixSocketPathLen is a compile-time fallback for wasm targets, whose
// syscall package has no RawSockaddrUnix. Unix domain sockets are not
// available there; the value only keeps address validation total.
const maxUnixSocketPathLen = 108
