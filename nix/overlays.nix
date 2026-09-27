# nix/overlays.nix
#
# Overlay so downstream consumers can `inputs.nats-server.overlays.default` and
# pick up the server binary and OCI image from their own pkgs set.
#
# Usage in a consumer flake:
#   inputs.nats-server.url = "github:nats-io/nats-server";
#   outputs = { nixpkgs, nats-server, ... }: let
#     pkgs = import nixpkgs { overlays = [ nats-server.overlays.default ]; system = "x86_64-linux"; };
#   in { packages.default = pkgs.nats-server; };
#
{ self }:

# `_prev` rather than `prev`: every attribute below is a fresh definition taken
# from `self`, so the underlying package set is never consulted. The argument
# still has to be named to keep the overlay's `final: prev:` shape; the leading
# underscore tells deadnix that is deliberate.
final: _prev: {
  nats-server = self.packages.${final.system}.nats-server or null;
  nats-server-oci = self.packages.${final.system}.oci-nats-server or null;
}
