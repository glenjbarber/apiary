// Package jail manages FreeBSD jails by shelling out to jail(8)/jls(8),
// scoped under a configured name prefix so Apiary never lists or touches
// jails it didn't create.
//
// All external command execution goes through the injectable
// CommandRunner (runner.go), so the package's orchestration logic is
// unit-testable off a FreeBSD host; the integration tests in
// integration_test.go are the only tests that need root.
package jail
