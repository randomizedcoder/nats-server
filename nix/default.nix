# nix/default.nix
#
# Per-system aggregator for the nats-server flake. flake.nix imports this once
# per system and re-exports `packages`, `devShells`, `checks`, and `apps`.
#
{
  pkgs,
  lib,
  src,
}:

let
  versions = import ./versions.nix { inherit pkgs; };

  # Vendored module tree, shared by every Go check + the quality report.
  goModules = import ./lib/goModules.nix {
    inherit pkgs src;
    vendorHash = versions.goVendorHash;
  };
  inherit (goModules) vendoredSource;

  # Binary builder (repo-root main package, server.* ldflags, variants).
  mkGoBinary = import ./lib/mkGoBinary.nix { inherit pkgs lib; };
  mkOciImage = import ./lib/mkOciImage.nix { inherit pkgs lib; };

  # Server version: the compiled-in default from server/const.go. Kept in sync by
  # hand with that file (VERSION = "2.16.0-dev"); a release stamps the real tag.
  serverVersion = "2.16.0-dev";

  nats-server = mkGoBinary {
    inherit src;
    variant = "default";
    version = serverVersion;
  };
  nats-server-debug = mkGoBinary {
    inherit src;
    variant = "debug";
    version = serverVersion;
  };
  nats-server-stripped = mkGoBinary {
    inherit src;
    variant = "stripped";
    version = serverVersion;
  };

  oci-nats-server = mkOciImage {
    name = "nats-server";
    binaries = nats-server;
  };

  # Static-analysis checks (go vet, gofmt, golangci-lint gating, gosec, nix lints).
  checks = import ./checks {
    inherit
      pkgs
      src
      vendoredSource
      ;
  };

  # golangci-lint tiers as runnable apps (shared with the dev shell PATH). The
  # comprehensive tier (`nix run .#lint-comprehensive`) is the advisory Tier 2 —
  # it prints findings and exits nonzero on them, so it lives as an app, not a
  # green-gating check.
  lintTiers = import ./lint-tiers.nix { inherit pkgs; };

  # Advisory tools as apps (run from a checkout, print findings, exit nonzero on
  # them). Kept out of the gating `checks` set — both expect findings on a mature
  # codebase — and captured side by side by `nix run .#quality-report`.
  #
  # gosec: same rule set as the quality report. G104/G115/G404 are excluded as
  # deliberate patterns (unhandled-error policy, size-math conversions, non-crypto
  # RNG); tighten as findings are triaged.
  gosec = pkgs.writeShellApplication {
    name = "gosec";
    runtimeInputs = [
      versions.go
      versions.gosec
    ];
    text = ''
      if [ ! -f flake.nix ]; then
        echo "gosec: must be run from the nats-server repo root" >&2
        exit 2
      fi
      export GOTOOLCHAIN=local
      exec gosec -exclude=G104,G115,G404 -exclude-dir=vendor ./...
    '';
  };

  # govulncheck: needs network for the vuln DB, which a hermetic sandbox blocks,
  # so it can only ever be an app.
  govulncheck = pkgs.writeShellApplication {
    name = "govulncheck";
    runtimeInputs = [
      versions.go
      versions.govulncheck
    ];
    text = ''
      if [ ! -f flake.nix ]; then
        echo "govulncheck: must be run from the nats-server repo root" >&2
        exit 2
      fi
      export GOTOOLCHAIN=local
      exec govulncheck ./...
    '';
  };

  qualityReport = import ./quality-report {
    inherit
      pkgs
      src
      vendoredSource
      ;
  };

  quality-report-app = pkgs.writeShellApplication {
    name = "quality-report";
    runtimeInputs = [ pkgs.coreutils ];
    text = ''
      cat ${qualityReport}/quality-report.md
    '';
  };

  update-quality-report-app = pkgs.writeShellApplication {
    name = "update-quality-report";
    runtimeInputs = [ pkgs.coreutils ];
    text = ''
      if [ ! -f flake.nix ]; then
        echo "update-quality-report: must be run from the nats-server repo root" >&2
        exit 2
      fi
      cp ${qualityReport}/quality-report.md nix/quality-report.md
      echo "wrote nix/quality-report.md"
    '';
  };

  # Turn a package with a bin/<name> into a flake app.
  mkApp = pkg: name: {
    type = "app";
    program = "${pkg}/bin/${name}";
  };
in
{
  packages = {
    default = nats-server;
    inherit
      nats-server
      nats-server-debug
      nats-server-stripped
      oci-nats-server
      ;
    quality-report = qualityReport;
  };

  devShells.default = import ./devshell.nix { inherit pkgs; };

  inherit checks;

  apps = {
    lint = mkApp lintTiers.lint "lint";
    lint-comprehensive = mkApp lintTiers.lint-comprehensive "lint-comprehensive";
    lint-fix = mkApp lintTiers.lint-fix "lint-fix";
    lint-new = mkApp lintTiers.lint-new "lint-new";
    gosec = mkApp gosec "gosec";
    govulncheck = mkApp govulncheck "govulncheck";
    quality-report = mkApp quality-report-app "quality-report";
    update-quality-report = mkApp update-quality-report-app "update-quality-report";
  };
}
