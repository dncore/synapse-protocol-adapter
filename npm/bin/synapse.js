#!/usr/bin/env node
// Launcher for the prebuilt synapse binary (esbuild-style distribution):
// resolve the platform package installed as an optionalDependency and exec
// the native binary, forwarding signals and the exit code.
'use strict';
const { spawn } = require('child_process');
const path = require('path');

const platformPackage = (() => {
  const os = process.platform === 'win32' ? 'windows' : process.platform;
  return `@dncore/synapse-${os}-${process.arch}`;
})();

function resolveBinary() {
  if (process.env.SYNAPSE_BINARY) return process.env.SYNAPSE_BINARY;
  try {
    const pkg = require.resolve(`${platformPackage}/package.json`);
    return path.join(path.dirname(pkg), 'bin', 'synapse');
  } catch {
    console.error(
      `synapse: platform package ${platformPackage} is not installed.\n` +
        `         npm may have skipped optional dependencies (e.g. --no-optional or --omit=optional).\n` +
        `         Reinstall without those flags, or point SYNAPSE_BINARY at a binary.`
    );
    process.exit(1);
  }
}

const child = spawn(resolveBinary(), process.argv.slice(2), { stdio: 'inherit' });
for (const sig of ['SIGINT', 'SIGTERM']) {
  process.on(sig, () => child.kill(sig)); // forward Ctrl-C / stop; the daemon drains gracefully
}
child.on('error', (err) => {
  console.error(`synapse: failed to start binary: ${err.message}`);
  process.exit(1);
});
child.on('exit', (code, signal) => {
  process.exit(signal ? 128 + 15 : (code ?? 0));
});
