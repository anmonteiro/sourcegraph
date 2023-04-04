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
        (pkgs.pkgsCross.musl64.comby.override {
          sqlite = pkgs.sqlite-oc;
          zlib = pkgs.zlib-oc;
          gmp = pkgs.gmp-oc;
          libev = pkgs.libev-oc;
        }).overrideAttrs (o: {
          buildInputs = o.buildInputs ++ [ pkgs.pkgsCross.musl64.ocamlPackages.camlzip ];
          doCheck = false;
          postPatch = ''
            rm -rf lib/app/vendored/camlzip
            substituteInPlace "lib/app/configuration/dune" "lib/app/pipeline/dune" \
              --replace "comby.camlzip" "camlzip"
            substituteInPlace "lib/app/configuration/command_input.ml" \
              --replace "Camlzip.Zip." "Zip."

            substituteInPlace \
              "lib/app/configuration/command_configuration.ml" \
              "lib/app/pipeline/fold.ml" --replace \
                  "open Camlzip" "open Zip"

            cat >> src/dune <<EOF
            (env (release (flags  :standard -ccopt -static)))
            EOF
          '';
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
