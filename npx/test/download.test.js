'use strict';

// End-to-end cover for the download path, against a local server serving
// artefacts shaped exactly like a goreleaser release.
//
// The parts worth proving are the ones that only fail in a user's terminal:
// a tampered archive must be refused, and the second run must not go near
// the network.

const assert = require('node:assert/strict');
const crypto = require('node:crypto');
const { execFileSync } = require('node:child_process');
const fs = require('node:fs');
const fsp = require('node:fs/promises');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const { after, test } = require('node:test');

const { archiveName } = require('../lib/artifact');
const { binaryPath, ensureBinary } = require('../lib/download');

const VERSION = '9.9.9';

function sha256(buf) {
  return crypto.createHash('sha256').update(buf).digest('hex');
}

/** Build a tar.gz holding a single executable named "nexus". */
async function fakeRelease() {
  const dir = await fsp.mkdtemp(path.join(os.tmpdir(), 'nexus-release-'));
  await fsp.writeFile(path.join(dir, 'nexus'), '#!/bin/sh\necho fake-nexus "$@"\n', { mode: 0o755 });

  const name = archiveName(VERSION);
  execFileSync('tar', ['-czf', name, 'nexus'], { cwd: dir });

  const archive = await fsp.readFile(path.join(dir, name));
  const checksums = `${sha256(archive)}  ${name}\n`;
  return { dir, name, archive, checksums };
}

async function serve(files) {
  const server = http.createServer((req, res) => {
    const body = files[req.url];
    if (!body) {
      res.writeHead(404).end('not found');
      return;
    }
    res.writeHead(200).end(body);
  });
  await new Promise((resolve) => server.listen(0, '127.0.0.1', resolve));
  return { server, base: `http://127.0.0.1:${server.address().port}` };
}

const cleanups = [];
after(async () => {
  for (const fn of cleanups) await fn();
});

test('downloads, verifies and caches the binary', async () => {
  const release = await fakeRelease();
  const requests = [];
  const { server, base } = await serve({
    [`/${release.name}`]: release.archive,
    '/checksums.txt': release.checksums,
  });
  server.on('request', (req) => requests.push(req.url));
  cleanups.push(() => new Promise((r) => server.close(r)));

  const state = await fsp.mkdtemp(path.join(os.tmpdir(), 'nexus-state-'));
  cleanups.push(() => fsp.rm(state, { recursive: true, force: true }));
  cleanups.push(() => fsp.rm(release.dir, { recursive: true, force: true }));

  process.env.NEXUS_RELEASE_BASE_URL = base;
  const got = await ensureBinary(VERSION, state, () => {});

  assert.equal(got, binaryPath(VERSION, state));
  assert.ok(fs.statSync(got).mode & 0o111, 'downloaded binary is not executable');
  assert.match(execFileSync(got, ['hello'], { encoding: 'utf8' }), /fake-nexus hello/);

  // Second call is a cache hit: no further requests reach the server, which
  // is what makes the second `npx` start immediately.
  const before = requests.length;
  await ensureBinary(VERSION, state, () => {});
  assert.equal(requests.length, before, 'cached run went back to the network');
});

test('refuses an archive that does not match the published checksum', async () => {
  const release = await fakeRelease();
  const { server, base } = await serve({
    [`/${release.name}`]: Buffer.concat([release.archive, Buffer.from('tampered')]),
    '/checksums.txt': release.checksums,
  });
  cleanups.push(() => new Promise((r) => server.close(r)));

  const state = await fsp.mkdtemp(path.join(os.tmpdir(), 'nexus-state-'));
  cleanups.push(() => fsp.rm(state, { recursive: true, force: true }));
  cleanups.push(() => fsp.rm(release.dir, { recursive: true, force: true }));

  process.env.NEXUS_RELEASE_BASE_URL = base;
  await assert.rejects(() => ensureBinary(VERSION, state, () => {}), /checksum mismatch/);
  assert.equal(fs.existsSync(binaryPath(VERSION, state)), false, 'a rejected binary was cached');
});

test('a missing release reports the URL rather than a bare 404', async () => {
  const { server, base } = await serve({});
  cleanups.push(() => new Promise((r) => server.close(r)));

  const state = await fsp.mkdtemp(path.join(os.tmpdir(), 'nexus-state-'));
  cleanups.push(() => fsp.rm(state, { recursive: true, force: true }));

  process.env.NEXUS_RELEASE_BASE_URL = base;
  await assert.rejects(() => ensureBinary(VERSION, state, () => {}), /returned 404/);
});
