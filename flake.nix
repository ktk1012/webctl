{
  description = "Smart web search CLI for agents, backed by Jev";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  };

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "aarch64-darwin"
        "x86_64-darwin"
        "aarch64-linux"
        "x86_64-linux"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # The release tag plus the revision being built, so `webctl version`
      # identifies a fork build rather than claiming to be the tag.
      version = "0.1.6-${self.shortRev or self.dirtyShortRev or "dirty"}";
    in
    {
      overlays.default = _final: prev: {
        webctl = prev.callPackage ./nix/package.nix { inherit version; };
      };

      packages = forAllSystems (pkgs: rec {
        webctl = pkgs.callPackage ./nix/package.nix { inherit version; };
        default = webctl;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            pkgs.gotools
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixfmt-tree);
    };
}
