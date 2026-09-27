# nix/lib/mkGoBinary.nix
#
# Builds the nats-server binary from the repo-root main package.
#
# Parameters:
#   name         — binary name (default "nats-server")
#   src          — source tree (the repo root)
#   variant      — one of "debug", "default", "stripped" (see versions.nix
#                  buildVariants). Drives ldflags (-s -w), the postFixup strip
#                  pass, and the derivation `pname` suffix.
#   commit,
#   version      — injected into the server package via -ldflags -X, matching
#                  goreleaser's stamps (github.com/nats-io/nats-server/v2/server.*)
#   extraLdflags — additional -ldflags entries appended after the variant's
#   doCheck      — run `go test ./...` during build (default false; tests run as
#                  separate targets, not part of the binary build)
#
{
  pkgs,
  lib,
}:

let
  versions = import ../versions.nix { inherit pkgs; };

  # Build with the pinned Go toolchain, not pkgs' default `go`.
  buildGoModule = pkgs.buildGoModule.override { inherit (versions) go; };

  # The module is github.com/nats-io/nats-server/v2; the version vars live in the
  # v2/server package (server/const.go: gitCommit, serverVersion). These are the
  # exact ldflag targets goreleaser uses.
  serverPkg = "github.com/nats-io/nats-server/v2/server";
in
{
  name ? "nats-server",
  src,
  variant ? "default",
  vendorHash ? versions.goVendorHash,
  commit ? "nix",
  version ? "nix",
  extraLdflags ? [ ],
  doCheck ? false,
}:

let
  variantCfg =
    versions.buildVariants.${variant}
      or (throw "mkGoBinary: unknown variant '${variant}'; expected one of ${toString (builtins.attrNames versions.buildVariants)}");
in
buildGoModule {
  pname = "${name}${variantCfg.tagSuffix}";
  inherit
    version
    src
    vendorHash
    doCheck
    ;

  # The main package is the repo root (`go build .`), not cmd/*.
  subPackages = [ "." ];

  env = {
    CGO_ENABLED = if versions.cgoEnabled then "1" else "0";
    GOTOOLCHAIN = "local";
  };

  tags = versions.buildTags;

  ldflags =
    variantCfg.extraLdflags
    ++ [
      "-X ${serverPkg}.gitCommit=${commit}"
      "-X ${serverPkg}.serverVersion=${version}"
    ]
    ++ extraLdflags;

  # -trimpath matches goreleaser and docker/Dockerfile.nightly.
  preBuild = ''
    export GOFLAGS="-trimpath ''${GOFLAGS:-}"
  '';

  # Filippo's trick: `strip` after -s -w shaves a bit more off. Only the
  # "stripped" variant. Other variants disable Nix's automatic strip so the
  # debug variant keeps its symbols.
  dontStrip = !variantCfg.doStrip;
  postFixup = lib.optionalString variantCfg.doStrip ''
    for bin in $out/bin/*; do
      ${pkgs.binutils-unwrapped}/bin/strip --strip-all "$bin"
    done
  '';

  meta = with lib; {
    description = "nats-server (${variant}) — the NATS messaging system server";
    homepage = "https://github.com/nats-io/nats-server";
    license = licenses.asl20;
    platforms = platforms.unix;
    mainProgram = "nats-server";
  };
}
