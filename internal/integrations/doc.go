// Package integrations is the parent of Bosun's first-party integrations
// (calendar, email, notes, websearch, mcp), one subpackage each.
//
// Integrations run in-process, so isolation is enforced at compile time
// instead of by a process boundary: integrations_test.go fails the build's
// test run if any file under this tree imports os/exec, internal/store,
// internal/auth, or another integration's package. The single exception is
// os/exec in internal/integrations/mcp (that directory only), because stdio
// MCP servers are subprocesses by definition. Integrations reach
// credentials only through integration.CredentialSource and are invoked only
// by the dispatcher.
package integrations
