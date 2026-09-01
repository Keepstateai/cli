// Downloads the ks release binary for this platform and VERIFIES its SHA256
// against the release's SHA256SUMS before trusting it. A mismatch aborts the
// install with nothing kept — the same refusal as the shell installer.
const { createHash } = require("crypto");
const fs = require("fs");
const path = require("path");
const https = require("https");

const REPO = process.env.KS_INSTALL_REPO || "esrygrtc/cli";
const BASE = process.env.KS_INSTALL_BASE || `https://github.com/${REPO}/releases/latest/download`;
const os = { darwin: "darwin", linux: "linux" }[process.platform];
const arch = { arm64: "arm64", x64: "amd64" }[process.arch];
if (!os || !arch) { console.error(`unsupported platform: ${process.platform}/${process.arch}`); process.exit(1); }
const asset = `ks-${os}-${arch}`;

function fetch(url, redirects = 5) {
  return new Promise((resolve, reject) => {
    https.get(url, (res) => {
      if ([301, 302, 307, 308].includes(res.statusCode) && redirects > 0)
        return resolve(fetch(res.headers.location, redirects - 1));
      if (res.statusCode !== 200) return reject(new Error(`GET ${url}: ${res.statusCode}`));
      const chunks = [];
      res.on("data", (c) => chunks.push(c));
      res.on("end", () => resolve(Buffer.concat(chunks)));
      res.on("error", reject);
    }).on("error", reject);
  });
}

(async () => {
  console.log(`downloading ${asset} ...`);
  const [bin, sums] = await Promise.all([fetch(`${BASE}/${asset}`), fetch(`${BASE}/SHA256SUMS`)]);
  const line = sums.toString().split("\n").map(l => l.trim().split(/\s+/)).find(f => f[1] === asset || f[1] === `*${asset}`);
  if (!line) { console.error(`REFUSED: no SHA256SUMS entry for ${asset}`); process.exit(1); }
  const got = createHash("sha256").update(bin).digest("hex");
  if (got !== line[0]) {
    console.error(`REFUSED: checksum mismatch for ${asset}\n  expected: ${line[0]}\n  got:      ${got}\nNothing was installed.`);
    process.exit(1);
  }
  const dest = path.join(__dirname, "ks-bin");
  fs.writeFileSync(dest, bin, { mode: 0o755 });
  console.log(`checksum OK; installed. Next step: ks login`);
})().catch((e) => { console.error(e.message); process.exit(1); });
