# nix/versions.nix
#
# Pinned tool versions for the nats-server Nix flake.
#
# Single source of truth — every other module reads from here.
# Changing a version here propagates to dev shell, build derivations, and checks.
#
{ pkgs }:

{
  # Go toolchain. go.mod declares `go 1.26.0` with `toolchain go1.26.8`; nixpkgs
  # nixos-unstable ships go_1_26, which satisfies the `go 1.26.0` directive. All
  # derivations set GOTOOLCHAIN=local so a hermetic build never tries to fetch
  # the exact 1.26.8 patch. If a security fix requires an exact patch ahead of
  # nixpkgs, pin it here with `pkgs.go_1_26.overrideAttrs` (version + fetchurl).
  go = pkgs.go_1_26;

  # Static analysis. golangci-lint is the gating linter (schema-v2 .golangci.yml,
  # matching CI); gosec adds the security-focused pass. deadnix/statix/nixfmt
  # lint the Nix tree itself so the flake stays clean. govulncheck is surfaced as
  # an app, not a check — it needs network for the vulnerability database.
  inherit (pkgs)
    golangci-lint
    gosec
    govulncheck
    deadnix
    statix
    ;
  nixfmt = pkgs.nixfmt-rfc-style or pkgs.nixfmt;

  # Per-variant build configuration. mkGoBinary picks one by name.
  #
  # Reference: https://words.filippo.io/shrink-your-go-binaries-with-this-one-weird-trick/
  #
  #   debug    — plain `go build` output. Keeps the symbol table and DWARF debug
  #              info. Largest; works directly with delve / `go tool pprof`.
  #   default  — `-ldflags "-s -w"`. Drops the symbol table (-s) and DWARF (-w).
  #              ~25% smaller. Production default; matches goreleaser's `-w`.
  #   stripped — default + binutils `strip`. Smallest.
  buildVariants = {
    debug = {
      extraLdflags = [ ];
      doStrip = false;
      tagSuffix = "-debug";
    };
    default = {
      extraLdflags = [
        "-s"
        "-w"
      ];
      doStrip = false;
      tagSuffix = "";
    };
    stripped = {
      extraLdflags = [
        "-s"
        "-w"
      ];
      doStrip = true;
      tagSuffix = "-stripped";
    };
  };

  # No build tags for the production binary — plain `go build .`, exactly as CI
  # and docker/Dockerfile.nightly do. (The repo's build tags are test-only shards.)
  buildTags = [ ];
  cgoEnabled = false;

  # Go vendor hash. Update by running `nix build .#nats-server` and pasting the
  # `got:` value from the hash-mismatch error. Used by every Nix check that needs
  # deps in the sandbox (see nix/lib/goModules.nix).
  goVendorHash = "sha256-C2CLryuDRyCTtaZ/oIInsGDWcUqWuSsohPKccykC32k=";
}
