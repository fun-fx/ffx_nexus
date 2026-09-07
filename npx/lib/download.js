'use strict';

// Fetch, verify and cache the nexus binary for this machine.

const crypto = require('node:crypto');
const fs = require('node:fs');
const fsp = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');
const { Readable } = require('node:stream');
const { pipeline } = require('node:stream/promises');

const { archiveName, archiveURL, checksumsURL, target } = require('./artifact');

/** Where a downloaded binary lives, keyed by version so upgrades do not collide. */
function binaryPath(version, stateDir) {
  return path.join(stateDir, 'bin', version, 'nexus');
}

async function exists(p) {
  try {
    await fsp.access(p, fs.constants.X_OK);
    return true;
  } catch {
    return false;
  }
}

async function get(url) {
  const res = await fetch(url, { redirect: 'follow', headers: { 'user-agent': 'nexus-npx' } });
  if (!res.ok) {
    throw new Error(`GET ${url} returned ${res.status} ${res.statusText}`);
  }
  return res;
}

/**
 * Read the expected sha256 for one file out of goreleaser's checksums.txt.
 *
 * Verifying matters more here than in most installers: this runs as
 * `npx -y`, so nobody reads the URL before it executes, and the binary it
 * fetches then starts a server and holds the user's provider API keys.
 */
async function expectedChecksum(version, fileName) {
  const res = await get(checksumsURL(version));
  const text = await res.text();
  for (const line of text.split('\n')) {
    const [sum, name] = line.trim().split(/\s+/);
    if (name === fileName) return sum;
  }
  throw new Error(`checksums.txt for v${version} does not list ${fileName}`);
}

async function sha256(file) {
  const hash = crypto.createHash('sha256');
  await pipeline(fs.createReadStream(file), hash);
  return hash.digest('hex');
}

/**
 * Unpack a single-binary tarball.
 *
 * tar is a hard dependency rather than a bundled JS implementation: this
 * package has no node_modules, which keeps `npx` from resolving a dependency
 * tree before it can start, and every platform we publish for ships tar.
 */
function extract(archive, into) {
  return new Promise((resolve, reject) => {
    const child = spawn('tar', ['-xzf', archive, '-C', into, 'nexus'], { stdio: 'inherit' });
    child.on('error', (err) =>
      reject(new Error(`could not run tar to unpack ${archive}: ${err.message}`))
    );
    child.on('exit', (code) =>
      code === 0 ? resolve() : reject(new Error(`tar exited with ${code} unpacking ${archive}`))
    );
  });
}

/**
 * Return a path to the nexus binary for `version`, downloading it if this is
 * the first run. Subsequent runs are a cache hit and start immediately.
 */
async function ensureBinary(version, stateDir, log = console.error) {
  const dest = binaryPath(version, stateDir);
  if (await exists(dest)) return dest;

  target(); // fail with the platform message before any network call

  const name = archiveName(version);
  const url = archiveURL(version);
  log(`nexus: downloading ${name} (first run only)`);

  const tmp = await fsp.mkdtemp(path.join(os.tmpdir(), 'nexus-npx-'));
  try {
    const archive = path.join(tmp, name);
    const res = await get(url);
    await pipeline(Readable.fromWeb(res.body), fs.createWriteStream(archive));

    const want = await expectedChecksum(version, name);
    const got = await sha256(archive);
    if (want !== got) {
      throw new Error(
        `checksum mismatch for ${name}: expected ${want}, got ${got}. ` +
          'Refusing to run a binary that does not match the published release.'
      );
    }

    await extract(archive, tmp);
    await fsp.mkdir(path.dirname(dest), { recursive: true });
    // Rename within the same filesystem would be cheaper, but tmpdir and the
    // state dir are often on different mounts.
    await fsp.copyFile(path.join(tmp, 'nexus'), dest);
    await fsp.chmod(dest, 0o755);
  } finally {
    await fsp.rm(tmp, { recursive: true, force: true });
  }

  return dest;
}

module.exports = { binaryPath, ensureBinary, expectedChecksum };
