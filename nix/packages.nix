# nix/packages.nix
#
# Package/tool lists for nats-server.
#
# Three categories:
#   - nativeBuildInputs: build-time tools
#   - buildInputs: link/runtime deps (nats-server is pure Go, so empty)
#   - devTools: extras for the developer shell only
#
{ pkgs }:

let
  versions = import ./versions.nix { inherit pkgs; };
in
rec {
  # Build-time only (used inside derivations).
  nativeBuildInputs = [
    versions.go
    pkgs.git
    pkgs.cacert
  ];

  # Link/runtime deps. nats-server is pure Go (CGO_ENABLED=0) so this stays empty.
  buildInputs = [ ];

  # Developer shell only. Goal: every contributor command works out of the box.
  devTools = with pkgs; [
    # Go ecosystem
    versions.go
    gopls
    gotools
    delve
    go-tools # staticcheck etc.
    versions.golangci-lint
    versions.gosec
    versions.govulncheck

    # HTTP / data plumbing
    curl
    jq

    # Container plumbing — inspect dockerTools-built images without docker.
    skopeo

    # Nix tooling
    versions.nixfmt
    versions.deadnix
    versions.statix
  ];

  # Combined list (everything for the dev shell).
  allDevPackages = nativeBuildInputs ++ buildInputs ++ devTools;
}
