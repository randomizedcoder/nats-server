#
# flake.nix — nats-server
#
# Thin orchestrator. Every concern lives under ./nix/ and is wired up here.
# See ./nix/default.nix for the per-system aggregator.
#
# Quick references:
#   nix develop                          # dev shell (Go 1.26, linters, skopeo, ...)
#   nix build .#nats-server              # the server binary (default variant)
#   nix build .#nats-server-debug        # keep symbols + DWARF (delve/pprof)
#   nix build .#oci-nats-server          # OCI image (load via `./result | docker load`)
#   nix flake check                      # go vet + golangci-lint + nix lints (gating)
#   nix run    .#gosec                   # gosec security scan (advisory)
#   nix run    .#lint-comprehensive      # golangci-lint Tier 2 (advisory)
#   nix run    .#govulncheck             # vuln scan (needs network — not a sandboxed check)
#   nix run    .#quality-report          # every tool, aggregated into one markdown report
#   nix run    .#lint                    # golangci-lint (gating config), from a checkout
#
{
  description = "nats-server — high-performance messaging system (NATS)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" ] (
      system:
      let
        pkgs = import nixpkgs { inherit system; };
        inherit (nixpkgs) lib;

        aggregator = import ./nix {
          inherit pkgs lib;
          src = ./.;
        };
      in
      {
        inherit (aggregator)
          packages
          devShells
          checks
          apps
          ;
      }
    )
    // {
      overlays.default = import ./nix/overlays.nix { inherit self; };
    };
}
