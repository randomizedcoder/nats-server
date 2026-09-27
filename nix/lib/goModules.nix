# nix/lib/goModules.nix
#
# Produces a derivation containing the nats-server Go module dependencies as a
# vendor/ tree. Reused by every Nix check that needs Go deps in the sandbox.
#
# The `vendorHash` MUST be updated after the first build. On a fresh checkout:
#   nix build .#nats-server 2>&1 | grep 'got:.*sha256-' | head -1
# then paste the value into versions.nix's `goVendorHash` slot.
#
{
  pkgs,
  src,
  vendorHash,
}:

let
  versions = import ../versions.nix { inherit pkgs; };

  # buildGoModule exposes `goModules` — a derivation containing the populated
  # vendor/ tree. No `subPackages` restriction: we want the FULL module graph so
  # lint checks can type-check every package in the repo.
  parent = (pkgs.buildGoModule.override { inherit (versions) go; }) {
    pname = "nats-server";
    version = "vendored";
    inherit src vendorHash;
    env.CGO_ENABLED = "0";
    env.GOTOOLCHAIN = "local";
    doCheck = false;
  };
in
{
  inherit (parent) goModules;
  # Convenience: a writable source tree with vendor/ already populated.
  vendoredSource = pkgs.runCommand "nats-server-vendored-source" { } ''
    cp -r ${src}/. $out
    chmod -R +w $out
    cp -r ${parent.goModules} $out/vendor
  '';
}
