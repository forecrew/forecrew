<!-- forecrew:verification -->
## Verification of `abc1234def56`: failed

Failed: 2 of 3 required results passed, 1 of 4 criteria verified, 1 flaky.

### Blocking the merge

- unit in mac-go (os=macos) failed (4 failed tests)
- AC-11 has no test
- new test auth::TestLogin passed on the base branch

### Results

| Environment | Cell | Suite | Required | Status | Passed | Failed | Skipped | Errored | Attempts | Job |
|---|---|---|---|---|---|---|---|---|---|---|
| linux-go | `go=1.27,os=linux` | unit | yes | passed | 120 | 0 | 2 | 0 | 2 | JOB-41 |
| mac-go | `os=macos` | unit | yes | failed | 118 | 3 | 0 | 1 | 1 | JOB-42 |
| mac-go | `os=macos` | e2e | no | missing | 0 | 0 | 0 | 0 | 0 | - |
| host | - | docs | yes | passed | 4 | 0 | 0 | 0 | 1 | - |

### Acceptance criteria

| Criterion | Status | Tests |
|---|---|---|
| AC-1 | passed | `auth::TestLogin` passed<br>`auth::TestLogout` passed |
| AC-2 | failed | `auth::TestExpiry` failed |
| AC-10 | untested | untestable: a human reads the page |
| AC-11 | untested | none |

### New tests on the base branch

| Test | Failed on base |
|---|---|
| `auth::TestExpiry` | yes |
| `auth::TestLogin` | no |

### Flaky tests

- `auth::TestRefresh` failed, then passed in linux-go, mac-go

### Environments

| Environment | Tool | Version |
|---|---|---|
| linux-go | go | `go version go1.27 linux/amd64` |
| linux-go | node | `v26.1.0` |
| mac-go | - | - |
