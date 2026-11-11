# Release assets from .github/workflows/release-binaries.yaml, for the version in
# .github/rigel-version. `hash` is the SRI hash of each asset. lib.fakeHash means
# that release is not cut yet: the fetch fails closed, and nothing builds from source.
# Refresh after a release: download the asset, `nix hash file --sri`, paste it here.
{ lib }:
{
  assets = {
    aarch64-darwin = {
      name = "darwin-arm64";
      hash = lib.fakeHash;
    };
    x86_64-linux = {
      name = "linux-amd64";
      hash = lib.fakeHash;
    };
    aarch64-linux = {
      name = "linux-arm64";
      hash = lib.fakeHash;
    };
  };
}
