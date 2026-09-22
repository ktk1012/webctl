{
  lib,
  buildGoModule,
  installShellFiles,
  stdenv,
  version ? "0-dev",
}:

buildGoModule {
  pname = "webctl";
  inherit version;

  src = lib.cleanSource ../.;

  vendorHash = "sha256-XKJ+z+Jio8ZVeKjNuWFBp6/DQgLO4q834K/+VtWlw0s=";

  # webctl-bench drives claude, codex, and pi against the live web; it is a
  # development harness, not something to ship in the package.
  excludedPackages = [ "cmd/webctl-bench" ];

  ldflags = [
    "-s"
    "-w"
    "-X=github.com/dorkitude/webctl/cmd/webctl/cli.version=${version}"
  ];

  # The key store and config dir are resolved from $HOME, which the build
  # sandbox does not set.
  preCheck = ''
    export HOME=$TMPDIR
  '';

  nativeBuildInputs = [ installShellFiles ];

  postInstall = lib.optionalString (stdenv.buildPlatform.canExecute stdenv.hostPlatform) ''
    installShellCompletion --cmd webctl \
      --bash <($out/bin/webctl completion bash) \
      --zsh <($out/bin/webctl completion zsh) \
      --fish <($out/bin/webctl completion fish)
  '';

  meta = {
    description = "Smart web search CLI for agents, backed by Jev";
    homepage = "https://github.com/dorkitude/webctl";
    license = lib.licenses.mit;
    mainProgram = "webctl";
  };
}
