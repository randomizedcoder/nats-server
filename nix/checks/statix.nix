# nix/checks/statix.nix
#
# statix — flags Nix antipatterns. Scope/ignores live in the repo-root statix.toml.
#
{
  pkgs,
  src,
}:

let
  versions = import ../versions.nix { inherit pkgs; };
in
pkgs.runCommand "nats-server-statix"
  {
    nativeBuildInputs = [ versions.statix ];
    inherit src;
  }
  ''
    cp -r ${src} ./src && chmod -R +w ./src
    cd ./src
    if ! statix check -o errfmt > $out 2>&1; then
      cat $out
      exit 1
    fi
    echo "statix: no findings" >> $out
  ''
