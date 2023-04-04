{ nixpkgs, utils }:
nixpkgs.lib.genAttrs utils.lib.defaultSystems (system:
  let
    makeStatic = (import ./util.nix).makeStatic;
    pkgs = nixpkgs.legacyPackages.${system};
    isMacOS = nixpkgs.legacyPackages.${system}.hostPlatform.isMacOS;
    combyBuilder = ocamlPkgs: systemDepsPkgs:
      (ocamlPkgs.comby.override {
        sqlite = systemDepsPkgs.sqlite;
        zlib = if isMacOS then systemDepsPkgs.zlib.static else systemDepsPkgs.zlib;
        libev = makeStatic systemDepsPkgs.libev;
        gmp = makeStatic (systemDepsPkgs.gmp.override {
          withStatic = true;
        });
        ocamlPackages = ocamlPkgs.ocamlPackages.overrideScope' (self: super: {
          ocaml_pcre = super.ocaml_pcre.override {
            pcre = makeStatic systemDepsPkgs.pcre.dev;
          };
          ssl = super.ssl.override {
            openssl = systemDepsPkgs.openssl.override {
              static = true;
            };
          };
        });
      });
  in
  if isMacOS then
    {
      comby = combyBuilder pkgs pkgs;
    } else
    {
      comby-musl =
        let
          inherit (pkgs) stdenv writeScriptBin pkg-config;
          pkg-config-script =
            let
              pkg-config-pkg =
                if stdenv.cc.targetPrefix == ""
                then "${pkg-config}/bin/pkg-config"
                else "${stdenv.cc.targetPrefix}pkg-config";
            in
            writeScriptBin "pkg-config" ''
              #!${stdenv.shell}
              ${pkg-config-pkg} $@
            '';
        in
        pkgs.pkgsCross.musl64.comby.overrideAttrs (o: {
          nativeBuildInputs = o.nativeBuildInputs ++ [ pkg-config-script ];
        });

      comby-glibc = (combyBuilder pkgs pkgs.pkgsStatic).overrideAttrs (oldAttrs: {
        postFixup = ''
          patchelf \
            --set-rpath /usr/lib \
            --set-interpreter /lib64/ld-linux-x86-64.so.2 \
            $out/bin/comby
        '';
      });
    }
)
