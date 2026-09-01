#!/usr/bin/env node
const { spawn } = require("child_process");
const path = require("path");
const bin = path.join(__dirname, "ks-bin");
spawn(bin, process.argv.slice(2), { stdio: "inherit" }).on("exit", (c) => process.exit(c ?? 1));
