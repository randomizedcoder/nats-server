# nix/checks/golangci-lint.nix
#
# The gating linter tier: golangci-lint with the repo's authoritative
# .golangci.yml (schema v2), the same config CI runs. Runs offline against the
# vendored source tree.
#
{
  pkgs,
  vendoredSource,
}:

let
  versions = import ../versions.nix { inherit pkgs; };
in
pkgs.runCommand "nats-server-golangci-lint"
  {
    nativeBuildInputs = [
      versions.go
      versions.golangci-lint
    ];
    inherit vendoredSource;
  }
  ''
    cp -r $vendoredSource ./src && chmod -R +w ./src
    cd ./src
    export HOME=$(mktemp -d)
    export CGO_ENABLED=0
    export GOTOOLCHAIN=local
    # golangci-lint honours modules-download-mode from the config (readonly);
    # force vendor mode so it never reaches the network in the sandbox.
    export GOFLAGS=-mod=vendor
    export GOLANGCI_LINT_CACHE=$(mktemp -d)
    golangci-lint run --config .golangci.yml --modules-download-mode=vendor ./... > $out 2>&1 \
      || (cat $out && exit 1)
  ''
