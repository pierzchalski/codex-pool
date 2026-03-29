{ pkgs, ... }:

{
  languages.go = {
    enable = true;
    # gopls (language server) is enabled by default
    enableHardeningWorkaround = true; # required for Delve debugger
  };

  packages = [ pkgs.delve ];
}
