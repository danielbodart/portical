{
  lib,
  stdenvNoCC,
  bun,
  makeWrapper,
  iproute2,
  version,
  src,
}:

# The same `bun build --compile` as run.ts's build. Portical has no runtime
# dependencies, so there is nothing to fetch and it builds in the sandbox from
# the source alone.
stdenvNoCC.mkDerivation {
  pname = "portical";
  inherit version src;

  nativeBuildInputs = [
    bun
    makeWrapper
  ];

  buildPhase = ''
    runHook preBuild
    export HOME=$TMPDIR
    bun build --compile --minify --sourcemap \
      --define 'PORTICAL_VERSION="${version}"' \
      src/main.ts --outfile portical
    runHook postBuild
  '';

  doCheck = true;
  checkPhase = ''
    runHook preCheck
    bun test
    runHook postCheck
  '';

  installPhase = ''
    runHook preInstall
    install -Dm755 portical $out/libexec/portical
    # ss, for the systemd source's `published`.
    makeWrapper $out/libexec/portical $out/bin/portical \
      --prefix PATH : ${lib.makeBinPath [ iproute2 ]}
    runHook postInstall
  '';

  # A compiled Bun binary is Bun with the bundle appended to it. Stripping it,
  # or patchelf rewriting it, loses the bundle and leaves plain Bun behind.
  dontStrip = true;
  dontPatchELF = true;

  meta = {
    description = "UPnP port forwarding for Docker containers and systemd units";
    homepage = "https://github.com/danielbodart/portical";
    license = lib.licenses.asl20;
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
    ];
    mainProgram = "portical";
  };
}
