#!/usr/bin/env node
'use strict';

// `npx -y @ffxnexus/nexus` — download the gateway for this machine and run it
// with its own database, so the console at :8081 works without the user
// installing or configuring anything.
//
// Arguments are passed through, so `npx @ffxnexus/nexus migrate` and
// `npx @ffxnexus/nexus serve` reach the same subcommands the binary has.

const os = require('node:os');
const path = require('node:path');
const { spawn } = require('node:child_process');

const { ensureBinary } = require('../lib/download');
const { version } = require('../package.json');

function stateDir() {
  return process.env.NEXUS_LOCAL_STATE_DIR || path.join(os.homedir(), '.nexus');
}

/**
 * Add `serve --local` unless the caller asked for something else.
 *
 * Bare `npx @ffxnexus/nexus` has to be the working quickstart, and without
 * --local the console answers 503 to every write. A caller who names a
 * subcommand or passes flags is doing something deliberate and gets exactly
 * what they typed.
 */
function commandArgs(argv) {
  if (argv.length > 0) return argv;
  return ['serve', '--local'];
}

async function main() {
  const argv = process.argv.slice(2);
  const dir = stateDir();

  const binary = await ensureBinary(version, dir);
  const child = spawn(binary, commandArgs(argv), {
    stdio: 'inherit',
    env: { ...process.env, NEXUS_LOCAL_STATE_DIR: dir },
  });

  // Forward the signals a terminal sends so the gateway shuts its database
  // down cleanly. Without this, Ctrl-C kills node and leaves an orphaned
  // postgres holding the data directory.
  const signals = ['SIGINT', 'SIGTERM', 'SIGHUP'];
  const forward = {};
  for (const signal of signals) {
    forward[signal] = () => child.kill(signal);
    process.on(signal, forward[signal]);
  }

  child.on('error', (err) => {
    console.error(`nexus: could not start ${binary}: ${err.message}`);
    process.exit(1);
  });
  child.on('exit', (code, signal) => {
    // Reproduce the child's fate rather than always exiting 0: a wrapper that
    // swallows a non-zero exit hides every startup failure from a script.
    if (signal) {
      // Drop the forwarders first, or re-raising the signal on ourselves
      // lands back in the handler above and kills a child that is gone.
      for (const s of signals) process.off(s, forward[s]);
      process.kill(process.pid, signal);
      return;
    }
    process.exit(code ?? 0);
  });
}

main().catch((err) => {
  console.error(`nexus: ${err.message}`);
  process.exit(1);
});
