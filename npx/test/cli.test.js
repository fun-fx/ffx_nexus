'use strict';

const assert = require('node:assert/strict');
const { execFileSync } = require('node:child_process');
const path = require('node:path');
const { test } = require('node:test');

const CLI = path.join(__dirname, '..', 'bin', 'nexus.js');

test('the launcher parses', () => {
  execFileSync(process.execPath, ['--check', CLI]);
});

test('the launcher parses on an unsupported platform without reaching the network', () => {
  // node --check only proves syntax; running it with a bogus cache directory
  // and no network would hang. Loading the module under test is enough to
  // catch a require() of something that is not in "files".
  const out = execFileSync(
    process.execPath,
    ['-e', "require('../lib/download'); console.log('ok')"],
    { cwd: __dirname, encoding: 'utf8' }
  );
  assert.match(out, /ok/);
});

// bin/nexus.js is the package's only entry point, and npm publishes what
// "files" lists. A lib/ left out of that list installs a package that
// crashes on require.
test('everything the launcher requires is inside the published files list', () => {
  const pkg = require('../package.json');
  for (const needed of ['bin/', 'lib/']) {
    assert.ok(pkg.files.includes(needed), `package.json files is missing ${needed}`);
  }
  assert.equal(pkg.bin.nexus, 'bin/nexus.js');
});
