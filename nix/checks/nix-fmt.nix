# nix/checks/nix-fmt.nix
#
# Verifies every *.nix file is nixfmt-clean.
#
{
  pkgs,
  src,
}:

let
  versions = import ../versions.nix { inherit pkgs; };
in
pkgs.runCommand "nats-server-nix-fmt"
  {
    nativeBuildInputs = [ versions.nixfmt ];
    inherit src;
  }
  ''
    cd ${src}
    if ! find . -name '*.nix' -not -path './vendor/*' -print0 \
        | xargs -0 nixfmt --check > $out 2>&1; then
      cat $out
      exit 1
    fi
    echo "all *.nix files nixfmt-clean" >> $out
  ''
