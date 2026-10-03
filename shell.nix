# nixpkgs is pinned to one revision rather than taken from the channel, so every
# tool here -- Go, just, DuckDB and the rest -- is the same version for everyone
# who enters the shell, and changes only when these two lines do. An unpinned
# toolchain is a build that differs by machine, which is what a hermetic build
# exists to rule out.
let
  nixpkgs = builtins.fetchTarball {
    url = "https://github.com/NixOS/nixpkgs/archive/e554fab72f81915600f3f449b786fd9af40439a5.tar.gz";
    sha256 = "sha256-ZKhUe/2IJUq1JhKxKMu8rbkgSGmPP2ZCqlIPn40aGCM=";
  };
in
{
  pkgs ? import nixpkgs { },
}:

let
  # The MCP conformance suite, which every subject -- the fixture gateway
  # included -- is held to before charpy's results about it mean anything.
  #
  # Deliberately unpinned, unlike everything else here: this is the packaging
  # flake's head and its `main`, upstream main as of that flake's lock. It
  # checks the fixture outside `just check`, and the fixture should meet the
  # suite as it now stands, not as it stood when this line was written. The
  # flake also pins every release by name, for whenever a result has to stay
  # fixed. getFlake needs the flakes feature, which the packaging needs anyway.
  conformance =
    (builtins.getFlake "github:serverplumber/mcp-conformance")
    .packages.${pkgs.stdenv.hostPlatform.system}.main;
in
pkgs.mkShell {
  name = "charpy-dev";
  packages = with pkgs; [
    go
    gopls
    gotools
    go-tools

    # The task runner. Every build, check and demo is a recipe.
    just
    # just lint and just fmt list the committable files with git, so a pure
    # shell -- the one CI would enter -- needs it as much as it needs Go.
    git

    # Cross-SDK differential reference peers: the TypeScript and Python
    # SDKs are to run as subprocess peers over stdio.
    nodejs
    python3

    # Transcript queries. JSONL is the database; this is how you read it.
    duckdb

    # TOML LSP; validates case manifests against the generated JSON Schema
    # via their #:schema directive. See docs/design/policy-format.md.
    taplo

    curl # just vendor-schemas, just vendor-specs

    conformance # just fixture-conformance
  ];

  shellHook = ''
    export GOPATH="''${GOPATH:-$HOME/go}"
    export PATH="$GOPATH/bin:$PATH"
    # Use the Go this shell pins, and fail rather than fetch another: with the
    # default, a go.mod asking for a newer Go makes the go command download one
    # quietly, and the pin stops meaning anything.
    export GOTOOLCHAIN=local
  '';
}
