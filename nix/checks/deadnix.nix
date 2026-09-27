# nix/checks/deadnix.nix
#
# deadnix — flags unused Nix bindings and function arguments across the flake.
#
{
  pkgs,
  src,
}:

let
  versions = import ../versions.nix { inherit pkgs; };
in
pkgs.runCommand "nats-server-deadnix"
  {
    nativeBuildInputs = [ versions.deadnix ];
    inherit src;
  }
  ''
    cd ${src}
    if ! deadnix --fail flake.nix nix > $out 2>&1; then
      cat $out
      exit 1
    fi
    echo "deadnix: no dead code" >> $out
  ''
