'use strict';

const assert = require('node:assert/strict');
const { test } = require('node:test');

const { archiveName, archiveURL, checksumsURL, target } = require('../lib/artifact');

test('archive names match the goreleaser name_template', () => {
  assert.equal(archiveName('0.7.0', 'darwin', 'arm64'), 'nexus_0.7.0_darwin_arm64.tar.gz');
  assert.equal(archiveName('0.7.0', 'darwin', 'x64'), 'nexus_0.7.0_darwin_amd64.tar.gz');
  assert.equal(archiveName('0.7.0', 'linux', 'x64'), 'nexus_0.7.0_linux_amd64.tar.gz');
  assert.equal(archiveName('0.7.0', 'linux', 'arm64'), 'nexus_0.7.0_linux_arm64.tar.gz');
});

test('the download URL points at the tag for that version', () => {
  assert.equal(
    archiveURL('0.7.0', 'linux', 'x64'),
    'https://github.com/fun-fx/ffx_nexus/releases/download/v0.7.0/nexus_0.7.0_linux_amd64.tar.gz'
  );
  assert.equal(
    checksumsURL('0.7.0'),
    'https://github.com/fun-fx/ffx_nexus/releases/download/v0.7.0/checksums.txt'
  );
});

// An unsupported platform must say what to do instead. Failing with
// "undefined.tar.gz returned 404" would send the user to the issue tracker
// for something Docker solves in one command.
test('an unpublished platform names the Docker fallback', () => {
  assert.throws(() => target('win32', 'x64'), /docker run/);
  assert.throws(() => target('linux', 'ppc64'), /docker run/);
});

test('node architecture spellings are translated to GOARCH', () => {
  assert.deepEqual(target('darwin', 'arm64'), { os: 'darwin', arch: 'arm64' });
  assert.deepEqual(target('linux', 'x64'), { os: 'linux', arch: 'amd64' });
});
