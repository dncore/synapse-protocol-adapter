#!/usr/bin/env node
// Stamps the release version into every npm manifest under npm/:
// each platform package gets version=<v>, and the root package's
// optionalDependencies are pinned to the same exact version.
// Usage: node scripts/npm-set-version.js 1.2.3
'use strict';
const fs = require('fs');
const path = require('path');

const version = process.argv[2];
if (!version || !/^\d+\.\d+\.\d+(-[\w.]+)?$/.test(version)) {
  console.error('usage: node scripts/npm-set-version.js <semver>');
  process.exit(1);
}

const npmDir = path.join(__dirname, '..', 'npm');
const rootPath = path.join(npmDir, 'package.json');
const root = JSON.parse(fs.readFileSync(rootPath, 'utf8'));
root.version = version;
for (const dep of Object.keys(root.optionalDependencies || {})) {
  root.optionalDependencies[dep] = version;
}
fs.writeFileSync(rootPath, JSON.stringify(root, null, 2) + '\n');

for (const entry of fs.readdirSync(npmDir, { withFileTypes: true })) {
  if (!entry.isDirectory()) continue;
  const p = path.join(npmDir, entry.name, 'package.json');
  if (!fs.existsSync(p)) continue;
  const pkg = JSON.parse(fs.readFileSync(p, 'utf8'));
  pkg.version = version;
  fs.writeFileSync(p, JSON.stringify(pkg, null, 2) + '\n');
  console.log(`stamped ${pkg.name}@${version}`);
}
console.log(`stamped ${root.name}@${version}`);
