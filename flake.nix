{
  description = "A Nix-flake-based Go development environment";
  inputs.nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
  outputs =
    { self, ... }@inputs:
    let
      goVersion = 26; # Change this to update the whole stack
      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "aarch64-darwin"
      ];
      forEachSupportedSystem =
        f:
        inputs.nixpkgs.lib.genAttrs supportedSystems (
          system:
          f {
            inherit system;
            pkgs = import inputs.nixpkgs {
              inherit system;
              overlays = [ inputs.self.overlays.default ];
            };
          }
        );
    in
    {
      overlays.default = final: prev: {
        go = final."go_1_${toString goVersion}";
      };

      devShells = forEachSupportedSystem (
        { pkgs, ... }:
        {
          default = pkgs.mkShell {
            CGO_ENABLED = "1";
            packages = with pkgs; [
              go
              gopls
              gotools
              golangci-lint
              bash
              zsh

              nixfmt
              prek
              actionlint
            ];
          };
        }
      );

    };
}
