{ pkgs, ... }:

{
  languages.go = {
    enable = true;
    version = "1.24";
    # gopls (language server) is enabled by default
    enableHardeningWorkaround = true; # required for Delve debugger
  };

  packages = [ pkgs.delve ];
}
