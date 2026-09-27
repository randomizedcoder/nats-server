# nix/devshell.nix
#
# Developer environment. `nix develop` lands here.
#
# Goals:
#   - Every contributor tool already on PATH (Go, golangci-lint, gosec,
#     govulncheck, skopeo, the Nix linters).
#   - Helper commands (lint, lint-comprehensive, lint-fix, lint-new) discoverable
#     via `nats-help` in the shell.
#
# The lint helpers are writeShellApplication packages (nix/lint-tiers.nix), not
# shellHook functions, so their bash is shellcheck'd at build time and they
# resolve after `exec bash`.
#
{ pkgs }:

let
  packages = import ./packages.nix { inherit pkgs; };

  # The golangci-lint tiers, shared verbatim with nix/default.nix's `apps` so the
  # shell and the flake cannot drift.
  lintTiers = import ./lint-tiers.nix { inherit pkgs; };

  natsHelp = pkgs.writeShellApplication {
    name = "nats-help";
    runtimeInputs = [ pkgs.coreutils ]; # cat
    text = ''
      cat <<'EOF'

      nats-server dev shell
      =====================
      Build:
        nix build .#nats-server                 Build the server binary
        nix build .#nats-server-debug           Build with symbols + DWARF (delve)
        nix build .#oci-nats-server             Build the OCI image
                                                (load: ./result | docker load)

      Static analysis (fix issues, do not ignore):
        lint                                    golangci-lint, gating config (CI)
        lint-comprehensive                      golangci-lint, comprehensive config
        lint-fix                                Apply auto-fixable findings
        lint-new                                Lint only the diff since HEAD~1
                                                (all take relative config paths, so
                                                 run from the repo root; exit 2 else)
        nix flake check                         go vet + golangci-lint +
                                                nix-fmt + deadnix + statix (gating)
        nix run .#gosec                         gosec security scan (advisory)
        nix run .#lint-comprehensive            Tier 2 lint (advisory)
        nix run .#govulncheck                   Vulnerability scan (needs network)

      Quality report (every tool, aggregated):
        nix run .#quality-report                Print the aggregated report
        nix run .#update-quality-report         Refresh nix/quality-report.md
        nix build .#quality-report              Build the report artifact (result/)

      Tests:
        go test ./...                           Unit tests (see scripts/ for shards)

      Nix:
        nixfmt --check **/*.nix                 Verify nix formatting
        deadnix flake.nix nix                   Dead Nix bindings
        statix check                            Nix antipatterns

      EOF
    '';
  };
in
pkgs.mkShell {
  name = "nats-server-dev";

  packages = packages.allDevPackages ++ lintTiers.all ++ [ natsHelp ];

  shellHook = ''
    export CGO_ENABLED=0
    export GOTOOLCHAIN=local
    # Resolves from PATH (the natsHelp package), not as a shell function.
    nats-help
  '';
}
