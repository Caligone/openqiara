# brutella/hap v0.0.35, patched for openqiara

Upstream: https://github.com/brutella/hap (Apache 2.0, see LICENSE), v0.0.35.
Used through a `replace` in openqiara's go.mod.

Changes, each marked `openqiara` in the code:
- `session.go`, `server.go`: the session keeps the Pair-Verify shared
  secret, exposed by `Server.SharedSecret(addr)`: HomeKit Secure Video's
  Data Stream derives its keys from it.
- `characteristics.go`: a timed-write characteristic written without a
  prepare no longer panics (nil timed write); it gets the error status
  HAP specifies (-70410).
