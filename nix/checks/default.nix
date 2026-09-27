# nix/checks/default.nix
#
# Aggregates the `nix flake check` targets for nats-server.
#
# Default set (run by `nix flake check`) — the gating tier: mirrors CI (which is
# green on this repo) plus Nix hygiene, so `nix flake check` staying green is
# meaningful.
#   go-vet, golangci-lint (gating), nix-fmt, deadnix, statix
#
# Formatting is gated by golangci-lint's `goimports` formatter (see .golangci.yml),
# which is build-tag aware and matches upstream CI. A standalone `gofmt -l` is
# deliberately NOT run: this repo's Go 1.26 gofmt disagrees on multiline
# indentation with the version the tree was formatted with, and would flag
# build-tag-skipped files (e.g. server/disk_avail_solaris.go) that CI never sees.
#
# Advisory tools are NOT here — everything in this attrset is run by
# `nix flake check`, and these surface findings that are expected on a mature
# codebase and need triage before they could gate:
#   gosec                — run: nix run .#gosec
#   lint-comprehensive   — run: nix run .#lint-comprehensive  (golangci-lint Tier 2)
#   govulncheck          — run: nix run .#govulncheck          (needs network)
# All three are also captured, side by side, by `nix run .#quality-report`.
#
{
  pkgs,
  src,
  vendoredSource,
}:

{
  go-vet = import ./go-vet.nix { inherit pkgs vendoredSource; };
  golangci-lint = import ./golangci-lint.nix { inherit pkgs vendoredSource; };

  # Nix static analysis — keeps the flake tree itself clean.
  nix-fmt = import ./nix-fmt.nix { inherit pkgs src; };
  deadnix = import ./deadnix.nix { inherit pkgs src; };
  statix = import ./statix.nix { inherit pkgs src; };
}
