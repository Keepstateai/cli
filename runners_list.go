// runners_list.go: the Cruise check runners, the service's ctl/runners.json
// vendored byte for byte (TestRunnersVendoredListMatchesTheServer holds it
// equal; regenerate with KS_RUNNERS_WRITE=1 and KS_SERVER_CHECKOUT set).
package main

const runnersJSON = `{
 "schemaVersion": 1,
 "about": "The Cruise check runners: the one list the judge (judge/checks.py), ctl (ctl/runners.go) and the client (a vendored copy, held equal by the client-drift job) read. A path's role in an ecosystem is the first of test, config, deps, test_support that matches; patterns match the file name (search semantics, so anchor them). The first three ecosystems are legacy: their fixture rule is computed among themselves only, so a tree's pinned inputs for them read as before the list existed. offered says whether ctl and the client offer the runner to a person: a runner is offered only after the fleet carries its toolchain and its fleet gate is green; the judge runs any runner listed. requiresDeps names a dependency file the check cannot run without (the work tree is read-only to the check, so the runner cannot write it). argv placeholders: {python} the judge's interpreter (recorded as python3), {report} the report file; paths append after argv, or defaultPaths when none are given.",
 "testDirs": ["tests", "test", "__tests__"],
 "fixtureDirs": ["testdata", "test_data", "fixtures", "__fixtures__", "__snapshots__"],
 "ecosystems": [
  {
   "ecosystem": "python",
   "offered": true,
   "runner": "pytest",
   "language": "Python",
   "legacy": true,
   "test": "^(test_.*|.*_test)\\.py$",
   "configAnywhere": "^conftest\\.py$",
   "configRoot": "^(conftest\\.py|pytest\\.ini|pyproject\\.toml|setup\\.cfg|tox\\.ini|setup\\.py)$",
   "depsRoot": "^(requirements.*\\.txt|constraints.*\\.txt|Pipfile|Pipfile\\.lock|poetry\\.lock|uv\\.lock|pdm\\.lock)$",
   "supportInTestDir": "\\.py$",
   "argv": ["{python}", "-m", "pytest", "-q", "-p", "no:cacheprovider", "--junitxml={report}"],
   "defaultPaths": ["."],
   "paths": "append",
   "report": "pytest-junit",
   "version": ["{python}", "-m", "pytest", "--version"],
   "command": "python3 -m pytest -q"
  },
  {
   "ecosystem": "go",
   "offered": true,
   "runner": "go",
   "language": "Go",
   "legacy": true,
   "test": "^.*_test\\.go$",
   "configAnywhere": "^(go\\.mod|go\\.work)$",
   "depsAnywhere": "^(go\\.sum|go\\.work\\.sum)$",
   "argv": ["go", "test", "-json", "-count=1"],
   "defaultPaths": ["./..."],
   "paths": "append",
   "report": "go-json",
   "version": ["go", "version"],
   "command": "go test ./..."
  },
  {
   "ecosystem": "node",
   "offered": true,
   "runner": "node",
   "language": "Node",
   "legacy": true,
   "test": "^(.*[.\\-_]test|.*\\.spec|test-.*|test)\\.(c|m)?[jt]sx?$",
   "testInDirs": {"name": "\\.(c|m)?[jt]sx?$", "dirs": ["__tests__", "test"]},
   "configAnywhere": "^package\\.json$",
   "configRoot": "^(package\\.json|\\.mocharc\\..*|jest\\.config\\..*|vitest\\.config\\..*|babel\\.config\\..*|\\.babelrc|tsconfig\\.json|\\.nvmrc|\\.node-version)$",
   "depsAnywhere": "^(package-lock\\.json|npm-shrinkwrap\\.json|yarn\\.lock|pnpm-lock\\.yaml)$",
   "argv": ["node", "--test", "--test-reporter=tap"],
   "defaultPaths": [],
   "paths": "append",
   "report": "tap",
   "version": ["node", "--version"],
   "command": "node --test"
  },
  {
   "ecosystem": "rust",
   "offered": true,
   "runner": "cargo",
   "language": "Rust",
   "legacy": false,
   "testInDirs": {"name": "\\.rs$", "dirs": ["tests"]},
   "configAnywhere": "^(Cargo\\.toml|rust-toolchain|rust-toolchain\\.toml)$",
   "depsAnywhere": "^Cargo\\.lock$",
   "requiresDeps": "Cargo.lock",
   "argv": ["cargo", "test", "--offline", "--workspace", "--no-fail-fast", "--test", "*"],
   "defaultPaths": [],
   "paths": "none",
   "report": "libtest",
   "stderr": "merge",
   "env": ["CARGO_HOME", "RUSTUP_HOME", "CARGO_TARGET_DIR"],
   "version": ["cargo", "--version"],
   "command": "cargo test --test '*'"
  },
  {
   "ecosystem": "ruby",
   "offered": true,
   "runner": "minitest",
   "language": "Ruby",
   "legacy": false,
   "test": "^(test_.*|.*_test)\\.rb$",
   "depsRoot": "^(Gemfile|Gemfile\\.lock|gems\\.rb|gems\\.locked)$",
   "supportInTestDir": "\\.rb$",
   "argv": ["ruby", "-W0", "-Itest", "-Ilib", "-e", "skip=%w[.git .hg .svn node_modules __pycache__ .pytest_cache .mypy_cache .ruff_cache .venv venv .tox .nox .keepstate .claude dist build]; files=ARGV.empty? ? Dir.glob('**/{test_*,*_test}.rb', File::FNM_DOTMATCH).reject{|f| (f.split('/')[0..-2] & skip).any?}.sort : ARGV.dup; ARGV.replace(['-v']); require 'minitest/autorun'; files.each{|f| require File.expand_path(f)}"],
   "defaultPaths": [],
   "paths": "append",
   "report": "minitest",
   "version": ["ruby", "-e", "require 'minitest'; puts \"ruby #{RUBY_VERSION} minitest #{Minitest::VERSION}\""],
   "command": "ruby (minitest, every test_*.rb and *_test.rb)"
  }
 ]
}
`
