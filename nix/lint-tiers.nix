# nix/lint-tiers.nix
#
# golangci-lint tier helpers, as writeShellApplication packages.
#
# Defined once here and consumed from both the dev shell (nix/devshell.nix, so
# the commands are on PATH) and the flake `apps` (nix/default.nix), so the two
# can't drift.
#
# No --timeout on any invocation — the CLI flag silently OVERRIDES a config's own
# `run.timeout`. The comprehensive config sets its own; the gating config uses
# golangci-lint's default.
#
{ pkgs }:

let
  versions = import ./versions.nix { inherit pkgs; };

  # Every tier passes a relative --config path and `./...`, so all are
  # cwd-sensitive. As PATH binaries they can be invoked from anywhere, which
  # makes the repo-root guard load-bearing rather than decorative.
  rootGuard = name: ''
    if [ ! -f flake.nix ]; then
      echo "${name}: must be run from the nats-server repo root" >&2
      exit 2
    fi
  '';

  # `exec` so the tier's exit status is the wrapper's exit status with no
  # intervening shell — a pre-commit hook or CI step reads golangci-lint's code
  # directly (1 = findings, >1 = the tool itself failed).
  mkTier =
    {
      name,
      config,
      extraArgs ? [ ],
      extraInputs ? [ ],
    }:
    pkgs.writeShellApplication {
      inherit name;
      runtimeInputs = [ versions.golangci-lint ] ++ extraInputs;
      text = ''
        ${rootGuard name}
        exec golangci-lint run --config ${config} ${pkgs.lib.concatStringsSep " " extraArgs} ./...
      '';
    };
in
rec {
  # Gating tier — the config CI runs (nix/checks/golangci-lint.nix).
  lint = mkTier {
    name = "lint";
    config = ".golangci.yml";
  };

  # Comprehensive tier — the superset config.
  lint-comprehensive = mkTier {
    name = "lint-comprehensive";
    config = ".golangci-comprehensive.yml";
  };

  # Gating config with --fix. Writes to the working tree, so it deliberately
  # uses the gating (not comprehensive) config: auto-fixes land only for findings
  # CI would have blocked on anyway.
  lint-fix = mkTier {
    name = "lint-fix";
    config = ".golangci.yml";
    extraArgs = [ "--fix" ];
  };

  # Gating config restricted to the diff since HEAD~1. golangci-lint shells out
  # to git to resolve --new-from-rev, hence git in runtimeInputs.
  lint-new = mkTier {
    name = "lint-new";
    config = ".golangci.yml";
    extraArgs = [ "--new-from-rev=HEAD~1" ];
    extraInputs = [ pkgs.git ];
  };

  # Convenience list for consumers that want all four (the dev shell's
  # `packages`), so a new tier added above shows up in `nix develop` without a
  # second edit.
  all = [
    lint
    lint-comprehensive
    lint-fix
    lint-new
  ];
}
