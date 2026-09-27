# nix/checks/go-vet.nix
#
# `go vet ./...` against the vendored source tree.
#
{
  pkgs,
  vendoredSource,
}:

let
  versions = import ../versions.nix { inherit pkgs; };
in
pkgs.runCommand "nats-server-go-vet"
  {
    nativeBuildInputs = [ versions.go ];
    inherit vendoredSource;
  }
  ''
    cp -r $vendoredSource ./src && chmod -R +w ./src
    cd ./src
    export HOME=$(mktemp -d)
    export CGO_ENABLED=0
    export GOTOOLCHAIN=local
    export GOFLAGS=-mod=vendor
    go vet ./... > $out 2>&1 || (cat $out && exit 1)
  ''
