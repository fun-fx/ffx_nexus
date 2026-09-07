'use strict';

// Names of the artefacts .goreleaser.yaml publishes.
//
// This module is the only place that knows the layout of a release. It is
// separate from the download logic so scripts/test_release_naming.sh can ask
// it what it expects and compare that against what goreleaser actually built
// — the mismatch is otherwise undetectable until a user's terminal shows a
// 404 from a URL nobody printed.

const REPO = 'fun-fx/ffx_nexus';

// goreleaser writes GOOS/GOARCH; node reports its own spelling for both.
const PLATFORMS = { darwin: 'darwin', linux: 'linux' };
const ARCHITECTURES = { x64: 'amd64', arm64: 'arm64' };

/**
 * Translate a node platform/arch pair into the goreleaser one.
 * @throws when the combination has no published binary.
 */
function target(platform = process.platform, arch = process.arch) {
  const os = PLATFORMS[platform];
  const goarch = ARCHITECTURES[arch];
  if (!os || !goarch) {
    throw new Error(
      `no Nexus binary is published for ${platform}/${arch}. ` +
        'Run it with Docker instead: ' +
        'docker run -p 8080:8080 -p 8081:8081 -v "$PWD/data:/app/data" ghcr.io/fun-fx/ffx_nexus'
    );
  }
  return { os, arch: goarch };
}

/** Archive file name for a released version, e.g. nexus_0.7.0_darwin_arm64.tar.gz */
function archiveName(version, platform, arch) {
  const t = target(platform, arch);
  return `nexus_${version}_${t.os}_${t.arch}.tar.gz`;
}

/**
 * Where the release archives are served from.
 *
 * NEXUS_RELEASE_BASE_URL points this at an internal mirror, which is the only
 * way `npx` works on a host that cannot reach github.com. It also lets the
 * test suite verify the download path against a local server instead of
 * asserting on URL strings and hoping.
 */
function releaseBaseURL(version, env = process.env) {
  const override = (env.NEXUS_RELEASE_BASE_URL || '').replace(/\/+$/, '');
  if (override) return override;
  return `https://github.com/${REPO}/releases/download/v${version}`;
}

function archiveURL(version, platform, arch) {
  return `${releaseBaseURL(version)}/${archiveName(version, platform, arch)}`;
}

function checksumsURL(version) {
  return `${releaseBaseURL(version)}/checksums.txt`;
}

module.exports = {
  REPO,
  archiveName,
  archiveURL,
  checksumsURL,
  releaseBaseURL,
  target,
};
