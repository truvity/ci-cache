# The server as a Nix package, so that a devbox or a runner image can pin this
# binary by hash rather than by a tag somebody can move.
#
# The tiers are one program: the same binary is the cluster's cache over a
# bucket, a runner agent's cache over the network, and a developer's cache over
# a bucket. This flake is how the second and third get it.
#
# This is a SOURCE build, and it is not the flake a release publishes. The
# shared release workflow generates one per goreleaser archive id
# (`nix-flakes: ["ci-cache"]`) whose sources are the released tarballs and
# their checksums: that one is the fast pin, fetching a built binary. This one
# is for a checkout, a fork, or a commit that has no release.
{
  description = "ci-cache — one cache for every build tool";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixpkgs-unstable";

  outputs =
    { self, nixpkgs }:
    let
      # No flake-utils. Another input is another thing to pin, update and
      # explain, for a four-line genAttrs.
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
      version = "0.1.0";
    in
    {
      packages = forAllSystems (pkgs: rec {
        # The toolchain is pinned to the same major devbox.json pins. nixpkgs'
        # default Go trails the go.mod directive often enough that leaving it
        # unpinned means the flake breaks on somebody else's nixpkgs bump,
        # with a message about GOTOOLCHAIN=local that names nothing here.
        ci-cache = (pkgs.buildGoModule.override { go = pkgs.go_1_27; }) {
          pname = "ci-cache";
          inherit version;
          src = ./.;

          # TODO(vendorHash): this is `lib.fakeHash` and WILL NOT BUILD.
          #
          # The value is the hash of the vendored module tree, so it can only
          # be taken from a `go mod download` that succeeds -- and go.mod does
          # not yet require everything the tree imports, so no download does.
          # Fill it in with the first go.mod that builds:
          #
          #     nix build .#ci-cache 2>&1 | grep -A1 'specified:'
          #
          # and paste the `got:` line here. It changes with every go.mod
          # change, which is exactly what makes it a pin.
          vendorHash = pkgs.lib.fakeHash;

          # The same as .goreleaser.yaml stamps, so that a binary from here and
          # a binary from a release answer `ci-cache version` the same way.
          ldflags = [
            "-s"
            "-w"
            "-X"
            "main.version=${version}"
          ];

          # Static, as everywhere else in this repository: it is what makes the
          # image small and the binary copyable.
          env.CGO_ENABLED = "0";

          # `just test` is where the suite runs, as its own CI job. A package
          # build that also ran it would make a pin fail for a reason that has
          # nothing to do with the binary it is pinning.
          doCheck = false;

          meta = {
            description = "One cache for every build tool: a disk tier over a bucket";
            homepage = "https://github.com/truvity/ci-cache";
            license = pkgs.lib.licenses.mit;
            mainProgram = "ci-cache";
          };
        };
        default = ci-cache;
      });
    };
}
