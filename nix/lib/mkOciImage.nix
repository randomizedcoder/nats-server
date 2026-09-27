# nix/lib/mkOciImage.nix
#
# Wraps `pkgs.dockerTools.streamLayeredImage` with a standard layout for
# nats-server OCI images.
#
# Conventions:
#   - The binary lands under /bin/, entrypoint defaults to /bin/nats-server.
#   - The CA trust bundle and tzdata are included so TLS and timezone-aware
#     features work on an otherwise-scratch image (mirrors docker/Dockerfile.nightly,
#     which installs tzdata + ca-certificates on alpine).
#   - Load pattern: `./result | docker load` (streamLayeredImage emits a script).
#
{ pkgs, lib }:

{
  name,
  tag ? "latest",
  binaries, # derivation containing /bin/nats-server
  exposedPorts ? [
    4222 # client connections
    8222 # HTTP monitoring
    6222 # cluster routes
  ],
  entrypoint ? "/bin/nats-server",
  # Optional default arguments (image config `Cmd`), appended after entrypoint.
  cmd ? [ ],
}:

let
  contents = [
    binaries
    # CA trust bundle. The image is otherwise scratch, so without this Go's
    # crypto/x509 has no roots and TLS to external endpoints fails with
    # "x509: certificate signed by unknown authority". caCertificates installs
    # the bundle at Go's default Linux path /etc/ssl/certs/ca-certificates.crt.
    pkgs.dockerTools.caCertificates
    # tzdata so time-zone-aware config (e.g. scheduled tasks, logs) resolves
    # zones instead of falling back to UTC on a scratch base.
    pkgs.tzdata
  ];

  exposedPortsAttr = lib.listToAttrs (
    map (p: {
      name = "${toString p}/tcp";
      value = { };
    }) exposedPorts
  );
in
pkgs.dockerTools.streamLayeredImage {
  inherit name tag contents;

  config = {
    Entrypoint = [ entrypoint ];
    ExposedPorts = exposedPortsAttr;
    Env = [ "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt" ];
  }
  // lib.optionalAttrs (cmd != [ ]) { Cmd = cmd; };
}
