// Package guards holds repo-wide source audits that run as ordinary tests, so
// a convention this codebase depends on fails the build instead of waiting for
// a reviewer to notice it. The audits parse the repository's own source; they
// have no runtime code and nothing imports them.
package guards
